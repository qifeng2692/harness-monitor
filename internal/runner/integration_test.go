//go:build integration

// 真实集成测试：实际 spawn 四家 harness 跑同一 prompt，断言至少 3 家拿到 PONG。
//
// 这不是 fixture 测试——它真的启动 hermes / codex / claude / pi 子进程，
// 走网关调用模型。网关偶发 502 属已知现象，故阈值放宽到 3/4，且失败家如实
// 打印其 status/error，绝不伪造。
//
// 运行：go test ./internal/runner/ -tags=integration -run TestIntegration -v -timeout 20m
package runner

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/store"
)

const integrationPrompt = "Reply with exactly: PONG"

// TestIntegrationFourHarnesses 并发跑四家，验证真实端到端链路。
func TestIntegrationFourHarnesses(t *testing.T) {
	if os.Getenv("HM_SKIP_INTEGRATION") != "" {
		t.Skip("HM_SKIP_INTEGRATION 已设置")
	}

	runDir := t.TempDir()
	timeout := 5 * time.Minute
	if v := os.Getenv("HM_INTEGRATION_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	type outcome struct {
		harness string
		res     Result
		err     error
	}
	results := make([]outcome, len(All))

	var wg sync.WaitGroup
	for i, harness := range All {
		wg.Add(1)
		go func(i int, harness string) {
			defer wg.Done()
			sp := &Spawn{RunDir: runDir, Timeout: timeout}
			res, err := sp.Run(context.Background(), harness, integrationPrompt)
			results[i] = outcome{harness: harness, res: res, err: err}
		}(i, harness)
	}
	wg.Wait()

	// 如实打印每一家的真实结果（验收要求的"贴真实运行输出"）。
	pass := 0
	for _, o := range results {
		t.Logf("──────────────────────────────────────────────────────────────")
		if o.err != nil {
			t.Logf("%-7s LAUNCH-ERROR: %v", o.harness, o.err)
			continue
		}
		r := o.res
		t.Logf("%-7s status=%-7s exit=%d duration=%dms session=%s",
			r.Harness, r.Status, r.ExitCode, r.DurationMs, r.SessionID)
		t.Logf("        tokens in=%d out=%d cost=%.6f events=%d",
			r.TokensIn, r.TokensOut, r.CostUSD, len(r.Events))
		t.Logf("        final_text=%q", r.FinalText)
		if r.Error != "" {
			t.Logf("        error=%s", r.Error)
		}
		kinds := map[string]int{}
		for _, ev := range r.Events {
			kinds[ev.Kind]++
		}
		t.Logf("        kinds=%v", kinds)
		if strings.TrimSpace(r.FinalText) == "PONG" {
			pass++
		}
	}
	t.Logf("──────────────────────────────────────────────────────────────")
	t.Logf("PONG 家数 = %d / %d", pass, len(All))

	// 基本不变量：每家都必须产出 session.start（说明进程真的起来了并说协议）。
	for _, o := range results {
		if o.err != nil {
			t.Errorf("%s 启动失败: %v", o.harness, o.err)
			continue
		}
		if len(filterKind(o.res.Events, "session.start")) == 0 {
			t.Errorf("%s 未产出 session.start（status=%s err=%s）",
				o.harness, o.res.Status, o.res.Error)
		}
	}

	// 验收判据：至少 3 家拿到 PONG（网关故障容忍）。
	if pass < 3 {
		t.Errorf("只有 %d 家拿到 PONG，期望至少 3 家（网关 502 时如实失败，请重试一次）", pass)
	}

	// 状态与结果必须自洽：拿到 PONG 的必须报 done（不是 failed/timeout）。
	// 这条断言来自首轮真实运行的教训——pi 曾因漏标完成事件被误判 failed。
	for _, o := range results {
		if o.err != nil {
			continue
		}
		if strings.TrimSpace(o.res.FinalText) == "PONG" && o.res.Status != store.RunHarnessDone {
			t.Errorf("%s 拿到了 PONG 但 status=%s（error=%s）——完成事件未被识别",
				o.harness, o.res.Status, o.res.Error)
		}
	}
}

// TestIntegrationRunDirIsolation 验证每家 harness 在 <runDir>/<harness>/ 下运行
// （工作目录隔离，见 docs/DESIGN-runner.md 第 8 节）。
func TestIntegrationRunDirIsolation(t *testing.T) {
	if os.Getenv("HM_SKIP_INTEGRATION") != "" {
		t.Skip("HM_SKIP_INTEGRATION 已设置")
	}
	runDir := t.TempDir()
	sp := &Spawn{RunDir: runDir, Timeout: 2 * time.Minute}
	res, err := sp.Run(context.Background(), HarnessCodex, integrationPrompt)
	if err != nil {
		t.Fatalf("codex 启动失败: %v", err)
	}
	_ = res
	if fi, err := os.Stat(runDir + "/codex"); err != nil || !fi.IsDir() {
		t.Errorf("隔离目录 %s/codex 未创建: %v", runDir, err)
	}
}

// TestIntegrationStatusMapping 验证真实运行的终态被正确映射到 store 常量。
func TestIntegrationStatusMapping(t *testing.T) {
	if os.Getenv("HM_SKIP_INTEGRATION") != "" {
		t.Skip("HM_SKIP_INTEGRATION 已设置")
	}
	runDir := t.TempDir()
	sp := &Spawn{RunDir: runDir, Timeout: 5 * time.Minute}
	res, err := sp.Run(context.Background(), HarnessPi, integrationPrompt)
	if err != nil {
		t.Fatalf("pi 启动失败: %v", err)
	}
	switch res.Status {
	case store.RunHarnessDone, store.RunHarnessFailed, store.RunHarnessTimeout, store.RunHarnessStopped:
	default:
		t.Errorf("pi 终态 %q 不是 store 的合法状态", res.Status)
	}
	if res.Status == store.RunHarnessDone && strings.TrimSpace(res.FinalText) != "PONG" {
		t.Errorf("pi 状态 done 但 final_text = %q", res.FinalText)
	}
}
