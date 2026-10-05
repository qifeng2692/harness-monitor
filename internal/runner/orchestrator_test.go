// 编排器单元测试：用 fake Runner 精确控制事件/状态时序，验证增量落库、
// 状态机、结果写库、Stop 中断、通知回调。真实 spawn 的端到端验证在
// orchestrator_integration_test.go（-tags=integration）。
package runner

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qf/harness-monitor/internal/model"
	"github.com/qf/harness-monitor/internal/store"
)

// scriptedRunner 是注入用的 Runner 工厂：每家按 harness 名给一份事件脚本。
type scriptedRunner struct {
	mu       sync.Mutex
	onEvent  func(string, model.Event)
	onStatus func(string, string, string)
	perEvent time.Duration
	hold     map[string]bool
	fail     map[string]bool
}

func (s *scriptedRunner) factory(_ int64, _ string,
	onEvent func(string, model.Event), onStatus func(string, string, string)) Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onEvent, s.onStatus = onEvent, onStatus
	return s
}

func (s *scriptedRunner) Run(ctx context.Context, harness, prompt string) (Result, error) {
	s.mu.Lock()
	onEvent, per, hold, fail := s.onEvent, s.perEvent, s.hold[harness], s.fail[harness]
	s.mu.Unlock()

	res := Result{Harness: harness, Status: store.RunHarnessDone, FinalText: "PONG",
		SessionID: "sess-" + harness, TokensIn: 10, TokensOut: 5, DurationMs: 7}

	script := []model.Event{
		{Kind: model.KindSessionStart, Title: "init"},
		{Kind: model.KindLLMResponse, Title: "assistant", Content: "PONG"},
	}
	for i, ev := range script {
		if per > 0 {
			select {
			case <-time.After(per):
			case <-ctx.Done():
				return stoppedResult(harness), nil
			}
		}
		if onEvent != nil {
			onEvent(harness, ev)
		}
		_ = i
	}
	if hold {
		<-ctx.Done()
		return stoppedResult(harness), nil
	}
	if fail {
		res.Status = store.RunHarnessFailed
		res.Error = "fake failure"
	}
	return res, nil
}

func stoppedResult(harness string) Result {
	return Result{Harness: harness, Status: store.RunHarnessStopped, Error: "已停止"}
}

func newOrchStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// waitRunHarnessTerminal 轮询直到某家进入终态（或超时）。
func waitRunHarnessTerminal(t *testing.T, s *store.Store, runID int64, harness string, timeout time.Duration) store.RunHarness {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d, err := s.GetRun(runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		for _, h := range d.HarnessRuns {
			if h.Harness == harness {
				switch h.Status {
				case store.RunHarnessDone, store.RunHarnessFailed,
					store.RunHarnessTimeout, store.RunHarnessStopped:
					return h
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s 未在 %s 内进入终态", harness, timeout)
	return store.RunHarness{}
}

// TestOrchestratorIncrementalWrites 验证事件在运行「过程中」就落库（不是结束后一次性写），
// 且 OnUpdate 被推送。用慢速 fake（每条事件间隔 120ms）在跑的中途查库。
func TestOrchestratorIncrementalWrites(t *testing.T) {
	st := newOrchStore(t)
	runID, err := st.CreateRun("Reply with exactly: PONG", []string{"hermes"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	var mu sync.Mutex
	updates := map[int64]int{}
	o := New(st)
	defer o.Close()
	o.OnUpdate = func(id int64) { mu.Lock(); updates[id]++; mu.Unlock() }
	o.NewRunner = (&scriptedRunner{perEvent: 120 * time.Millisecond, hold: map[string]bool{"hermes": true}}).factory

	if err := o.Start(runID, "Reply with exactly: PONG", []string{"hermes"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 中途轮询：进程还在跑（hold），但事件应已增量可见。
	var sawMidRun int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evs, err := st.ListRunEvents(runID, "hermes")
		if err != nil {
			t.Fatalf("ListRunEvents: %v", err)
		}
		if len(evs) > 0 && o.IsActive(runID) {
			sawMidRun = len(evs)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sawMidRun == 0 {
		t.Fatal("运行过程中未观察到增量落库（GUI 实时显示依赖此行为）")
	}

	// hold 住 → Stop 结束。
	if err := o.Stop(runID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h := waitRunHarnessTerminal(t, st, runID, "hermes", 3*time.Second)
	if h.Status != store.RunHarnessStopped {
		t.Errorf("Stop 后状态应为 stopped，得到 %q", h.Status)
	}

	// 收尾：等批次被标记 done（全部 harness 结束后 FinishRun）。
	waitRunDone(t, st, runID, 3*time.Second)
	mu.Lock()
	got := updates[runID]
	mu.Unlock()
	if got == 0 {
		t.Error("OnUpdate 未被推送")
	}
}

// TestOrchestratorAllTerminalAndResults 验证四家并发跑完后：全部终态、结果写库、批次 done。
func TestOrchestratorAllTerminalAndResults(t *testing.T) {
	st := newOrchStore(t)
	runID, err := st.CreateRun("Reply with exactly: PONG", All)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	o := New(st)
	defer o.Close()
	o.NewRunner = (&scriptedRunner{}).factory

	if err := o.Start(runID, "Reply with exactly: PONG", All); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, st, runID, 5*time.Second)

	d, err := st.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if d.Summary.Status != store.RunStatusDone {
		t.Errorf("批次 status 应为 done，得到 %q", d.Summary.Status)
	}
	if len(d.HarnessRuns) != len(All) {
		t.Fatalf("应有 %d 行 harness，得到 %d", len(All), len(d.HarnessRuns))
	}
	for _, h := range d.HarnessRuns {
		if h.Status != store.RunHarnessDone {
			t.Errorf("%s 状态应为 done，得到 %q（err=%s）", h.Harness, h.Status, h.Error)
		}
		if h.FinalText != "PONG" {
			t.Errorf("%s final_text 应为 PONG，得到 %q", h.Harness, h.FinalText)
		}
		if h.SessionID != "sess-"+h.Harness {
			t.Errorf("%s session_id 应为 sess-%s，得到 %q", h.Harness, h.Harness, h.SessionID)
		}
		if h.TokensIn != 10 || h.TokensOut != 5 {
			t.Errorf("%s tokens 应为 10/5，得到 %d/%d", h.Harness, h.TokensIn, h.TokensOut)
		}
		if h.StartedAt.IsZero() || h.EndedAt.IsZero() {
			t.Errorf("%s started_at/ended_at 应已写入", h.Harness)
		}
		evs, err := st.ListRunEvents(runID, h.Harness)
		if err != nil {
			t.Fatalf("ListRunEvents(%s): %v", h.Harness, err)
		}
		if len(evs) < 2 {
			t.Errorf("%s 事件数应 >=2，得到 %d", h.Harness, len(evs))
		}
		if evs[0].Seq != 1 {
			t.Errorf("%s 事件 seq 应从 1 起，得到 %d", h.Harness, evs[0].Seq)
		}
	}
}

// TestOrchestratorStopKillsLongRun 验证 Stop 能中断一个长运行（模拟长任务）：
// 批次状态 stopped，且不会等到自然结束。
func TestOrchestratorStopKillsLongRun(t *testing.T) {
	st := newOrchStore(t)
	runID, err := st.CreateRun("count slowly from 1 to 1000", []string{"hermes", "codex"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	o := New(st)
	defer o.Close()
	// 两家都 hold（模拟长任务），收到取消才返回。
	o.NewRunner = (&scriptedRunner{
		perEvent: 30 * time.Millisecond,
		hold:     map[string]bool{"hermes": true, "codex": true},
	}).factory

	if err := o.Start(runID, "count slowly from 1 to 1000", []string{"hermes", "codex"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 等两家都进入 running（确保进程真的起了）。
	waitRunHarnessRunning(t, st, runID, 2*time.Second)

	start := time.Now()
	if err := o.Stop(runID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, h := range []string{"hermes", "codex"} {
		got := waitRunHarnessTerminal(t, st, runID, h, 3*time.Second)
		if got.Status != store.RunHarnessStopped {
			t.Errorf("%s 停止后状态应为 stopped，得到 %q", h, got.Status)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Stop 未及时中断长任务，耗时 %s", elapsed)
	}
	waitRunDone(t, st, runID, 3*time.Second)
}

// TestOrchestratorFailStatus 验证失败结果如实落库（status=failed + error）。
func TestOrchestratorFailStatus(t *testing.T) {
	st := newOrchStore(t)
	runID, _ := st.CreateRun("p", []string{"hermes"})
	o := New(st)
	defer o.Close()
	o.NewRunner = (&scriptedRunner{fail: map[string]bool{"hermes": true}}).factory

	if err := o.Start(runID, "p", []string{"hermes"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := waitRunHarnessTerminal(t, st, runID, "hermes", 3*time.Second)
	if h.Status != store.RunHarnessFailed {
		t.Errorf("状态应为 failed，得到 %q", h.Status)
	}
	if h.Error != "fake failure" {
		t.Errorf("error 应为 fake failure，得到 %q", h.Error)
	}
}

// TestOrchestratorValidation 验证 Start 入参校验。
func TestOrchestratorValidation(t *testing.T) {
	st := newOrchStore(t)
	o := New(st)
	defer o.Close()
	o.NewRunner = (&scriptedRunner{}).factory

	cases := []struct {
		name      string
		prompt    string
		harnesses []string
	}{
		{"空 prompt", "  ", []string{"hermes"}},
		{"空 harnesses", "p", nil},
		{"未知 harness", "p", []string{"nope"}},
	}
	for _, tc := range cases {
		if err := o.Start(1, tc.prompt, tc.harnesses); err == nil {
			t.Errorf("%s: Start 应返回错误", tc.name)
		}
	}
}

// TestTruncateErr 验证错误文本按 500 字符截断（rune 安全）。
func TestTruncateErr(t *testing.T) {
	long := ""
	for i := 0; i < 600; i++ {
		long += "中"
	}
	got := truncateErr(long)
	if n := len([]rune(got)); n > maxErrorLen+1 { // +1 为省略号
		t.Errorf("截断后长度 %d 超过上限 %d", n, maxErrorLen)
	}
	if truncateErr("  hello  ") != "hello" {
		t.Errorf("truncateErr 应去除首尾空白")
	}
}

// waitRunDone 轮询直到批次 status=done。
func waitRunDone(t *testing.T, s *store.Store, runID int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d, err := s.GetRun(runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if d.Summary.Status == store.RunStatusDone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("批次 %d 未在 %s 内标记 done", runID, timeout)
}

// waitRunHarnessRunning 轮询直到某家进入 running（确认进程已起）。
func waitRunHarnessRunning(t *testing.T, s *store.Store, runID int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d, err := s.GetRun(runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		all := len(d.HarnessRuns) > 0
		for _, h := range d.HarnessRuns {
			if h.Status != store.RunHarnessRunning {
				all = false
			}
		}
		if all {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("批次 %d 的 harness 未在 %s 内全部 running", runID, timeout)
}
