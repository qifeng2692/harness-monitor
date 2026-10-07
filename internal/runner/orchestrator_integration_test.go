//go:build integration

// 编排器真实集成测试：实际 spawn 四家 harness，经编排器完整跑一遍，
// 验证「运行中增量写库」「全终态」「Stop 中断长运行」。
//
// 这不是 fixture/fake 测试——真的启动 hermes / codex / claude / pi 子进程，
// 走网关调用模型。网关偶发 502 属已知现象，故「至少 3 家 done」。
//
// 运行：go test ./internal/runner/ -tags=integration -run TestOrchestratorIntegration -v -timeout 25m
package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

// newIntegrationStore 开一个临时库并返回 store。
func newIntegrationStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestOrchestratorIntegrationFourHarnesses 四家并发真实跑 PONG，
// 断言：增量写库（跑到中途事件数增长）、全终态、至少 3 家 done、批次 done。
func TestOrchestratorIntegrationFourHarnesses(t *testing.T) {
	if os.Getenv("HM_SKIP_INTEGRATION") != "" {
		t.Skip("HM_SKIP_INTEGRATION 已设置")
	}
	st := newIntegrationStore(t)
	runID, err := st.CreateRun(integrationPrompt, All)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	o := New(st)
	defer o.Close()
	o.RunRoot = filepath.Join(t.TempDir(), "runs")

	var mu sync.Mutex
	updateCount := 0
	o.OnUpdate = func(id int64) {
		if id == runID {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
	}

	if err := o.Start(runID, integrationPrompt, All); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 轮询：在批次仍 running 时观察 run_events 增量增长（GUI 实时列显示依赖此行为）。
	// 记录「增量」证据：首次看到事件后，事件数应继续增长；且此期间批次仍在跑。
	seen := map[string]int{}
	var incrementalProof []string
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		for _, h := range All {
			evs, err := st.ListRunEvents(runID, h)
			if err != nil {
				t.Fatalf("ListRunEvents(%s): %v", h, err)
			}
			if prev, ok := seen[h]; ok && len(evs) > prev && o.IsActive(runID) {
				incrementalProof = append(incrementalProof,
					h+" 在运行中从 "+strconv.Itoa(prev)+" 增至 "+strconv.Itoa(len(evs)))
			}
			seen[h] = len(evs)
		}
		d, err := st.GetRun(runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if d.Summary.Status == store.RunStatusDone {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	d, err := st.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun(结束): %v", err)
	}
	if d.Summary.Status != store.RunStatusDone {
		t.Errorf("批次 status 应为 done，得到 %q", d.Summary.Status)
	}
	if len(incrementalProof) == 0 {
		t.Error("未观察到运行中的增量写库证据（事件数应在批次仍 running 时增长）")
	}

	pass := 0
	for _, h := range d.HarnessRuns {
		evs, _ := st.ListRunEvents(runID, h.Harness)
		t.Logf("──────────────────────────────────────────────────────────────")
		t.Logf("%-7s status=%-7s duration=%dms tokens in/out=%d/%d cost=%.6f session=%s events=%d",
			h.Harness, h.Status, h.DurationMs, h.TokensIn, h.TokensOut, h.CostUSD, h.SessionID, len(evs))
		t.Logf("        final_text=%q", h.FinalText)
		if h.Error != "" {
			t.Logf("        error=%s", h.Error)
		}
		kinds := map[string]int{}
		for _, ev := range evs {
			kinds[ev.Kind]++
		}
		t.Logf("        kinds=%v", kinds)

		switch h.Status {
		case store.RunHarnessDone, store.RunHarnessFailed, store.RunHarnessTimeout, store.RunHarnessStopped:
		default:
			t.Errorf("%s 未到终态，status=%q", h.Harness, h.Status)
		}
		if strings.TrimSpace(h.FinalText) == "PONG" {
			pass++
			if h.Status != store.RunHarnessDone {
				t.Errorf("%s 拿到 PONG 但 status=%s（完成事件未被识别）", h.Harness, h.Status)
			}
		}
		if len(evs) == 0 {
			t.Errorf("%s 无任何事件落库", h.Harness)
		}
		if len(filterKind(evs, model.KindSessionStart)) == 0 {
			t.Errorf("%s 未产出 session.start（status=%s err=%s）", h.Harness, h.Status, h.Error)
		}
	}
	t.Logf("──────────────────────────────────────────────────────────────")
	t.Logf("增量写库证据：%v", incrementalProof)
	t.Logf("OnUpdate 推送次数 = %d", updateCount)
	t.Logf("PONG 家数 = %d / %d", pass, len(All))

	if pass < 3 {
		t.Errorf("只有 %d 家拿到 PONG，期望至少 3 家（网关 502 时如实失败，请重试）", pass)
	}
	if updateCount == 0 {
		t.Error("OnUpdate 从未被推送")
	}
}

// TestOrchestratorIntegrationStopLongRun 用长任务（慢慢数到 1000）验证 Stop
// 真能中断在跑的真实子进程，且状态如实记为 stopped。
func TestOrchestratorIntegrationStopLongRun(t *testing.T) {
	if os.Getenv("HM_SKIP_INTEGRATION") != "" {
		t.Skip("HM_SKIP_INTEGRATION 已设置")
	}
	st := newIntegrationStore(t)
	const prompt = "count slowly from 1 to 1000, one number per line, do not stop early"
	runID, err := st.CreateRun(prompt, All)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	o := New(st)
	defer o.Close()
	o.RunRoot = filepath.Join(t.TempDir(), "runs")
	if err := o.Start(runID, prompt, All); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 等至少一家真正产出事件（说明进程起来了、在干活），再 Stop。
	ready := time.Now().Add(3 * time.Minute)
	anyProgress := false
	for time.Now().Before(ready) {
		for _, h := range All {
			evs, _ := st.ListRunEvents(runID, h)
			if len(evs) > 0 {
				anyProgress = true
				break
			}
		}
		if anyProgress {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !anyProgress {
		t.Skip("3 分钟内无任何 harness 产出事件（网关/环境问题），跳过 Stop 中断验证")
	}

	start := time.Now()
	if err := o.Stop(runID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// 所有 harness 应在合理时间内到达 stopped 终态。
	stopped := 0
	dl := time.Now().Add(30 * time.Second)
	for time.Now().Before(dl) {
		d, err := st.GetRun(runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		stopped = 0
		done := true
		for _, h := range d.HarnessRuns {
			switch h.Status {
			case store.RunHarnessStopped:
				stopped++
			case store.RunHarnessDone, store.RunHarnessFailed, store.RunHarnessTimeout:
				// 已自然结束，可接受。
			default:
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	d, err := st.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun(停止后): %v", err)
	}
	t.Logf("Stop 后各家状态（耗时 %s）：", time.Since(start))
	for _, h := range d.HarnessRuns {
		t.Logf("  %-7s status=%-8s duration=%dms final_text=%q error=%q",
			h.Harness, h.Status, h.DurationMs, truncate(h.FinalText, 60), h.Error)
	}
	if stopped == 0 {
		t.Errorf("Stop 后无任何 harness 记为 stopped（实际状态见上）")
	}
	if elapsed := time.Since(start); elapsed > 40*time.Second {
		t.Errorf("Stop 后收尾耗时 %s 过长", elapsed)
	}
	waitRunDone(t, st, runID, 20*time.Second)
}
