// compare_test.go：对比实验台 API 层的单元测试。
//
// 用注入的 fake Runner（不 spawn 真进程）验证：StartCompare 立即返回 runID、
// ListRuns/GetRun 的摘要与详情、GetRunEvents/ExplainRunEvents、StopRun 终态、
// 入参校验。真实四家进程的端到端验收在 cmd/hmc4check（无 GUI 驱动同一套 API）。
package compare

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/qf/harness-monitor/internal/model"
	"github.com/qf/harness-monitor/internal/runner"
	"github.com/qf/harness-monitor/internal/store"
)

// fakeRunner 是注入用的 Runner：发两条事件、返回 PONG 结果。
type fakeRunner struct {
	onEvent  func(string, model.Event)
	onStatus func(string, string, string)
	hold     bool
}

func (f *fakeRunner) factory(_ int64, _ string,
	onEvent func(string, model.Event), onStatus func(string, string, string)) runner.Runner {
	f.onEvent, f.onStatus = onEvent, onStatus
	return f
}

func (f *fakeRunner) Run(ctx context.Context, harness, prompt string) (runner.Result, error) {
	if f.onStatus != nil {
		f.onStatus(harness, store.RunHarnessRunning, "")
	}
	for _, ev := range []model.Event{
		{Kind: model.KindSessionStart, Title: "init"},
		{Kind: model.KindLLMResponse, Title: "assistant", Content: "PONG"},
	} {
		if f.hold {
			select {
			case <-ctx.Done():
				return runner.Result{Harness: harness, Status: store.RunHarnessStopped}, nil
			case <-time.After(20 * time.Millisecond):
			}
		}
		if f.onEvent != nil {
			f.onEvent(harness, ev)
		}
	}
	if f.hold {
		<-ctx.Done()
		return runner.Result{Harness: harness, Status: store.RunHarnessStopped}, nil
	}
	return runner.Result{Harness: harness, Status: store.RunHarnessDone, FinalText: "PONG",
		SessionID: "sess-" + harness, TokensIn: 10, TokensOut: 5, DurationMs: 42}, nil
}

func newTestService(t *testing.T, f *fakeRunner) (*Service, *store.Store) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st)
	if f != nil {
		s.orch.NewRunner = f.factory
	}
	t.Cleanup(s.Close)
	return s, st
}

// waitRunDone 轮询直到批次 status=done。
func waitRunDone(t *testing.T, s *Service, runID int64, timeout time.Duration) RunInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ri, err := s.GetRun(runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if ri.Status == store.RunStatusDone {
			return ri
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("批次 %d 未在 %s 内 done", runID, timeout)
	return RunInfo{}
}

// TestStartCompareReturnsImmediatelyAndUpdates 验证 StartCompare 立即返回 runID、
// 事件增量落库、OnUpdate 被推送、终态与结果正确。
func TestStartCompareReturnsImmediatelyAndUpdates(t *testing.T) {
	s, _ := newTestService(t, &fakeRunner{})

	updates := 0
	s.OnUpdate = func(int64) { updates++ }

	start := time.Now()
	runID, err := s.StartCompare("Reply with exactly: PONG", runner.All)
	if err != nil {
		t.Fatalf("StartCompare: %v", err)
	}
	if runID <= 0 {
		t.Fatalf("runID 非法: %d", runID)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("StartCompare 应立即返回，耗时 %s", elapsed)
	}

	ri := waitRunDone(t, s, runID, 5*time.Second)
	if len(ri.Runners) != len(runner.All) {
		t.Fatalf("应有 %d 家 harness，得到 %d", len(runner.All), len(ri.Runners))
	}
	if ri.Active {
		t.Error("批次已 done，Active 应为 false")
	}
	for _, h := range ri.Runners {
		if h.Status != store.RunHarnessDone {
			t.Errorf("%s 状态应为 done，得到 %q", h.Harness, h.Status)
		}
		if h.FinalText != "PONG" {
			t.Errorf("%s final_text 应为 PONG，得到 %q", h.Harness, h.FinalText)
		}
		if h.SessionID != "sess-"+h.Harness {
			t.Errorf("%s session_id 不对: %q", h.Harness, h.SessionID)
		}
		if h.DurationMs != 42 || h.TokensIn != 10 || h.TokensOut != 5 {
			t.Errorf("%s 结果字段不对: %+v", h.Harness, h)
		}
		evs, err := s.GetRunEvents(runID, h.Harness)
		if err != nil {
			t.Fatalf("GetRunEvents(%s): %v", h.Harness, err)
		}
		if len(evs) != 2 || evs[0].Kind != model.KindSessionStart || evs[1].Content != "PONG" {
			t.Errorf("%s 事件流不对: %+v", h.Harness, evs)
		}
	}
	if updates == 0 {
		t.Error("OnUpdate 从未被推送")
	}

	// 倒序列表：新批次在最前。
	runs, err := s.ListRuns()
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != runID {
		t.Errorf("ListRuns 应为 [%d]，得到 %+v", runID, runs)
	}
}

// TestListRunsDescending 验证 ListRuns 倒序。
func TestListRunsDescending(t *testing.T) {
	s, _ := newTestService(t, &fakeRunner{})
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := s.StartCompare("p", []string{"hermes"})
		if err != nil {
			t.Fatalf("StartCompare %d: %v", i, err)
		}
		ids = append(ids, id)
		waitRunDone(t, s, id, 5*time.Second)
	}
	runs, err := s.ListRuns()
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 || runs[0].ID != ids[2] || runs[2].ID != ids[0] {
		t.Errorf("ListRuns 应倒序，得到 %+v", runs)
	}
}

// TestStopRun 验证 StopRun 中断在跑批次并记 stopped。
func TestStopRun(t *testing.T) {
	s, _ := newTestService(t, &fakeRunner{hold: true})
	runID, err := s.StartCompare("count slowly", []string{"hermes", "codex"})
	if err != nil {
		t.Fatalf("StartCompare: %v", err)
	}
	// 等编排器登记为活跃。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !s.orch.IsActive(runID) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.StopRun(runID); err != nil {
		t.Fatalf("StopRun: %v", err)
	}
	ri := waitRunDone(t, s, runID, 5*time.Second)
	for _, h := range ri.Runners {
		if h.Status != store.RunHarnessStopped {
			t.Errorf("%s 停止后应为 stopped，得到 %q", h.Harness, h.Status)
		}
	}
}

// TestExplainRunEvents 验证复用 internal/explain 生成逐步说明。
func TestExplainRunEvents(t *testing.T) {
	s, _ := newTestService(t, &fakeRunner{})
	runID, err := s.StartCompare("p", []string{"hermes"})
	if err != nil {
		t.Fatalf("StartCompare: %v", err)
	}
	waitRunDone(t, s, runID, 5*time.Second)

	steps, err := s.ExplainRunEvents(runID, "hermes")
	if err != nil {
		t.Fatalf("ExplainRunEvents: %v", err)
	}
	evs, _ := s.GetRunEvents(runID, "hermes")
	if len(steps) != len(evs) {
		t.Fatalf("说明数 %d 应与事件数 %d 一一对应", len(steps), len(evs))
	}
	for i, st := range steps {
		if st.Summary == "" || st.From == "" || st.To == "" || st.Visible == "" || st.Why == "" {
			t.Errorf("第 %d 步说明字段不完整: %+v", i, st)
		}
	}
}

// TestValidation 验证入参校验（prompt / harnesses / 未知 harness）。
func TestValidation(t *testing.T) {
	s, _ := newTestService(t, &fakeRunner{})
	cases := []struct {
		name      string
		prompt    string
		harnesses []string
	}{
		{"空 prompt", "   ", []string{"hermes"}},
		{"空 harnesses", "p", nil},
		{"未知 harness", "p", []string{"nope"}},
		{"混入未知 harness", "p", []string{"hermes", "magpie"}},
	}
	for _, tc := range cases {
		if _, err := s.StartCompare(tc.prompt, tc.harnesses); err == nil {
			t.Errorf("%s: 应返回错误", tc.name)
		}
	}
	// 重复名去重（不报错）。
	id, err := s.StartCompare("p", []string{"hermes", "hermes"})
	if err != nil {
		t.Fatalf("重复 harness 应被去重而非报错: %v", err)
	}
	ri := waitRunDone(t, s, id, 5*time.Second)
	if len(ri.Runners) != 1 || len(ri.Harnesses) != 1 {
		t.Errorf("重复 harness 应去重为 1 家，得到 %+v", ri)
	}
}

// TestGetRunMissing 验证不存在的批次返回错误。
func TestGetRunMissing(t *testing.T) {
	s, _ := newTestService(t, nil)
	if _, err := s.GetRun(99999); err == nil {
		t.Error("GetRun 不存在的批次应返回错误")
	}
	if _, err := s.GetRunEvents(99999, "hermes"); err != nil {
		// 不存在的批次事件流为空即可（不强制报错）。
		t.Logf("GetRunEvents(不存在): %v", err)
	}
	if _, err := s.GetRunEvents(1, "magpie"); err == nil {
		t.Error("GetRunEvents 未知 harness 应返回错误")
	}
}
