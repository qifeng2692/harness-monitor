// Command hmc4check 是 HM-C4 的验收程序：不需要 GUI，直接驱动对比实验台 API 层
// （internal/compare —— 与 gui/app.go 绑定的是同一段代码），把真实四家 harness
// 跑一遍并打印每家的终态与结果。
//
// 用法：
//
//	go run ./cmd/hmc4check                      # 默认 PONG 问题、四家
//	go run ./cmd/hmc4check -prompt "..." -harnesses hermes,codex
//	go run ./cmd/hmc4check -explain             # 额外打印某家的逐步自然语言说明
//
// 环境变量 HM_DATA_DIR 可覆盖数据目录（默认用仓库根的 data/，与 GUI 同一个库，
// 因此这里产生的批次在 GUI 的历史列表里同样可见）。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qf/harness-monitor/internal/compare"
	"github.com/qf/harness-monitor/internal/runner"
	"github.com/qf/harness-monitor/internal/store"
)

func main() {
	prompt := flag.String("prompt", "Reply with exactly: PONG", "对比用的 prompt")
	harnessCSV := flag.String("harnesses", strings.Join(runner.All, ","), "参与对比的 harness（逗号分隔）")
	pollMS := flag.Int("poll", 500, "轮询间隔（毫秒）")
	deadline := flag.Duration("timeout", 15*time.Minute, "整批等待上限")
	stopAfter := flag.Duration("stop-after", 0, "启动后多久调 StopRun（>0 时启用；用于验收停止路径）")
	explainHarness := flag.String("explain", "", "额外打印该 harness 的逐步自然语言说明")
	flag.Parse()

	st, err := store.Open()
	if err != nil {
		fatal("打开数据库失败: %v", err)
	}
	defer st.Close()
	fmt.Printf("数据目录: %s\n", store.DataDir())
	fmt.Printf("库路径:   %s\n", st.Path)

	var harnesses []string
	for _, h := range strings.Split(*harnessCSV, ",") {
		if h = strings.TrimSpace(h); h != "" {
			harnesses = append(harnesses, h)
		}
	}

	svc := compare.New(st)
	defer svc.Close()

	updates := 0
	svc.OnUpdate = func(runID int64) { updates++ } // GUI 里这里换成 runtime.EventsEmit

	fmt.Println(strings.Repeat("─", 74))
	fmt.Printf("prompt:    %q\n", *prompt)
	fmt.Printf("harnesses: %v\n", harnesses)

	start := time.Now()
	runID, err := svc.StartCompare(*prompt, harnesses)
	if err != nil {
		fatal("StartCompare 失败: %v", err)
	}
	fmt.Printf("StartCompare 返回 runID=%d，耗时 %s（应立即返回，进程在后台跑）\n",
		runID, time.Since(start).Round(time.Millisecond))
	fmt.Println(strings.Repeat("─", 74))

	// 轮询直到全部终态（批次 status=done）。
	hardDeadline := time.Now().Add(*deadline)
	tick := time.Duration(*pollMS) * time.Millisecond
	stopAt := time.Time{}
	if *stopAfter > 0 {
		stopAt = start.Add(*stopAfter)
		fmt.Printf("已设定 -stop-after=%s：到点调用 StopRun（验收停止路径）\n", *stopAfter)
	}
	lastLine := ""
	for {
		ri, err := svc.GetRun(runID)
		if err != nil {
			fatal("GetRun 失败: %v", err)
		}
		line := progressLine(ri)
		if line != lastLine {
			fmt.Printf("[%6s] %s\n", time.Since(start).Round(time.Second), line)
			lastLine = line
		}
		if ri.Status == store.RunStatusDone {
			break
		}
		if !stopAt.IsZero() && time.Now().After(stopAt) {
			stopAt = time.Time{} // 只停一次
			if err := svc.StopRun(runID); err != nil {
				fmt.Printf("!! StopRun 失败: %v\n", err)
			} else {
				fmt.Printf("[%6s] 已调用 StopRun\n", time.Since(start).Round(time.Second))
			}
		}
		if time.Now().After(hardDeadline) {
			fmt.Println("!! 等待超时，仍未全部终态；尝试 StopRun")
			_ = svc.StopRun(runID)
			break
		}
		time.Sleep(tick)
	}

	// 终态结果。
	ri, err := svc.GetRun(runID)
	if err != nil {
		fatal("GetRun(结束) 失败: %v", err)
	}
	fmt.Println(strings.Repeat("─", 74))
	fmt.Printf("批次 #%d  status=%s  active=%v  created=%s\n", ri.ID, ri.Status, ri.Active, ri.CreatedAt)
	fmt.Printf("prompt=%q  harnesses=%v\n", ri.Prompt, ri.Harnesses)
	fmt.Println(strings.Repeat("─", 74))

	pong := 0
	for _, h := range ri.Runners {
		evs, err := svc.GetRunEvents(runID, h.Harness)
		if err != nil {
			fmt.Printf("  (GetRunEvents(%s) 失败: %v)\n", h.Harness, err)
		}
		fmt.Printf("%-7s status=%-8s duration=%6dms  tokens in/out=%d/%d  cost=%.6f  events=%d\n",
			h.Harness, h.Status, h.DurationMs, h.TokensIn, h.TokensOut, h.CostUSD, len(evs))
		fmt.Printf("        session_id=%s\n", orDash(h.SessionID))
		fmt.Printf("        final_text=%q\n", h.FinalText)
		if h.Error != "" {
			fmt.Printf("        error=%s\n", h.Error)
		}
		if strings.TrimSpace(h.FinalText) == "PONG" && h.Status == store.RunHarnessDone {
			pong++
		}
	}
	fmt.Println(strings.Repeat("─", 74))
	fmt.Printf("OnUpdate 推送次数 = %d\n", updates)
	fmt.Printf("PONG 家数 = %d / %d\n", pong, len(ri.Runners))

	// 历史批次（倒序）——GUI 历史列表的数据来源。
	runs, err := svc.ListRuns()
	if err != nil {
		fatal("ListRuns 失败: %v", err)
	}
	fmt.Printf("ListRuns 返回 %d 个批次（倒序，最新在前）:\n", len(runs))
	for i, r := range runs {
		if i >= 5 {
			fmt.Printf("  ...（共 %d 个）\n", len(runs))
			break
		}
		states := make([]string, 0, len(r.Runners))
		for _, h := range r.Runners {
			states = append(states, h.Harness+"="+h.Status)
		}
		fmt.Printf("  #%-3d %s  status=%-7s  [%s]  %s\n",
			r.ID, r.CreatedAt, r.Status, strings.Join(states, " "), truncate(r.Prompt, 40))
	}

	if *explainHarness != "" {
		steps, err := svc.ExplainRunEvents(runID, *explainHarness)
		if err != nil {
			fmt.Printf("ExplainRunEvents(%s) 失败: %v\n", *explainHarness, err)
		} else {
			fmt.Println(strings.Repeat("─", 74))
			fmt.Printf("ExplainRunEvents(%s) 共 %d 步:\n", *explainHarness, len(steps))
			for i, st := range steps {
				fmt.Printf("  [%d] %s\n      输入来自: %s\n      输出去向: %s\n      是否显示给用户: %s\n",
					i+1, st.Summary, st.From, st.To, st.Visible)
			}
		}
	}

	fmt.Println(strings.Repeat("─", 74))
	switch {
	case *stopAfter > 0:
		fmt.Println("结果：本轮用于验收 StopRun 路径（各家应记 stopped，非失败）。")
	case pong == len(ri.Runners):
		fmt.Println("结果：四家全部 done 且 final_text=PONG。")
	default:
		fmt.Println("结果：并非全部 done（网关 502 / 超时会如实失败，属预期）。")
	}
	if pong == 0 && *stopAfter == 0 {
		os.Exit(1)
	}
}

// progressLine 生成一行动态进度（各家当前状态）。
func progressLine(ri compare.RunInfo) string {
	parts := make([]string, 0, len(ri.Runners))
	for _, h := range ri.Runners {
		parts = append(parts, fmt.Sprintf("%s=%s", h.Harness, h.Status))
	}
	return fmt.Sprintf("batch=%-7s %s", ri.Status, strings.Join(parts, " "))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(strings.ReplaceAll(strings.TrimSpace(s), "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func fatal(format string, args ...any) {
	fmt.Printf("FAIL: "+format+"\n", args...)
	os.Exit(1)
}
