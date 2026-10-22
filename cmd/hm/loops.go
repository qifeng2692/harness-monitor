package main

import (
	"fmt"
	"strings"

	"github.com/qifeng2692/harness-monitor/internal/loops"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

// runLoops 实现子命令 loops：找出「同一个调用在会话里反复出现」的工具调用环。
//
// 为什么需要：hm stats 只能回答「失败了多少次」，答不了「同一个动作是不是卡在
// 原地反复做」。这是 agent 最常见的一类空转——同一条命令、同一个参数，失败一次、
// 再一次、又一次，每次都记了日志，但没人把它单独拎出来说「这里在打转」。
// 同类工具（MINDLAS 的 tool loop、rungraph 的 retry storm）都把这条当核心信号。
//
// 口径纪律：
//   - 「重复」是结构事实，不依赖失败信号 → 对五家 harness 一视同仁；
//   - 「失败」只认各家自己上报的结构化字段（meta.is_error）；判不了就说判不了。
func runLoops(args []string) error {
	harness := ""
	threshold := loops.DefaultThreshold
	topN := 20
	positional := []string{}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--harness" && i+1 < len(args):
			harness = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--harness="):
			harness = strings.TrimPrefix(args[i], "--harness=")
		case args[i] == "--threshold" && i+1 < len(args):
			n, err := atoi(args[i+1])
			if err != nil || n <= 0 {
				return fmt.Errorf("--threshold 需要正整数（给了 %q）", args[i+1])
			}
			threshold = n
			i++
		case strings.HasPrefix(args[i], "--threshold="):
			n, err := atoi(strings.TrimPrefix(args[i], "--threshold="))
			if err != nil || n <= 0 {
				return fmt.Errorf("--threshold 需要正整数（给了 %q）", strings.TrimPrefix(args[i], "--threshold="))
			}
			threshold = n
		case args[i] == "--top" && i+1 < len(args):
			n, err := atoi(args[i+1])
			if err != nil || n <= 0 {
				return fmt.Errorf("--top 需要正整数（给了 %q）", args[i+1])
			}
			topN = n
			i++
		case strings.HasPrefix(args[i], "--top="):
			n, err := atoi(strings.TrimPrefix(args[i], "--top="))
			if err != nil || n <= 0 {
				return fmt.Errorf("--top 需要正整数（给了 %q）", strings.TrimPrefix(args[i], "--top="))
			}
			topN = n
		case args[i] == "-h" || args[i] == "--help":
			printUsage()
			return nil
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 0 {
		printUsage()
		return fmt.Errorf("loops 不接受位置参数（给了 %d 个）", len(positional))
	}

	s, err := store.Open()
	if err != nil {
		return err
	}
	defer s.Close()

	calls, err := s.StatsLoopCalls(harness)
	if err != nil {
		return err
	}
	results, err := s.StatsLoopResults(harness)
	if err != nil {
		return err
	}

	lcalls := make([]loops.Call, 0, len(calls))
	for _, c := range calls {
		lcalls = append(lcalls, loops.Call{
			Harness:   c.Harness,
			SessionID: c.SessionID,
			Tool:      c.Tool,
			Args:      c.Args,
			Key:       c.Key,
		})
	}
	lresults := make([]loops.Result, 0, len(results))
	for _, r := range results {
		lresults = append(lresults, loops.Result{
			Harness:   r.Harness,
			SessionID: r.SessionID,
			Key:       r.Key,
			Failed:    r.Failed,
			Judged:    r.Judged,
		})
	}

	groups := loops.Detect(lcalls, lresults, threshold)
	if len(groups) == 0 {
		scope := "全部 harness"
		if harness != "" {
			scope = harness
		}
		fmt.Printf("未发现重复调用（阈值：同一会话内同一工具同一参数调用 %d 次以上；范围：%s）\n", threshold, scope)
		return nil
	}

	printLoopReport(groups, topN, threshold)
	return nil
}

// printLoopReport 打印重复调用报告。
//
// 布局：先给总量（多少组、涉及多少会话、多少次调用），再列最严重的若干组。
// 每组给出「调用次数 / 失败次数（判得了的） / 参数样例」，让人一眼看出是不是真打转。
func printLoopReport(groups []loops.Group, topN, threshold int) {
	sessions := map[string]bool{}
	totalCalls := 0
	totalFailed := 0
	totalJudged := 0
	for _, g := range groups {
		sessions[g.Harness+"\x00"+g.SessionID] = true
		totalCalls += g.Calls
		totalFailed += g.Failed
		totalJudged += g.Judged
	}

	fmt.Printf("== 重复调用环（阈值 %d）==\n", threshold)
	fmt.Printf("  分组         %d 组\n", len(groups))
	fmt.Printf("  涉及会话     %d 个\n", len(sessions))
	fmt.Printf("  重复调用     %d 次\n", totalCalls)
	if totalJudged > 0 {
		fmt.Printf("  其中失败     %d 次（判得了的 %d 次调用里）\n", totalFailed, totalJudged)
	} else {
		fmt.Println("  其中失败     判不了（这些 harness 的工具结果不带失败信号字段）")
	}

	fmt.Println()
	fmt.Printf("%-10s %-24s %-18s %6s %6s %8s  %s\n",
		"HARNESS", "会话", "工具", "调用", "失败", "判定", "参数")
	for i, g := range groups {
		if i >= topN {
			break
		}
		failed := "-"
		if g.Judged > 0 {
			failed = fmt.Sprintf("%d", g.Failed)
		}
		judged := "-"
		if g.Judged > 0 {
			judged = fmt.Sprintf("%d", g.Judged)
		}
		fmt.Printf("%-10s %-24s %-18s %6d %6s %8s  %s\n",
			g.Harness, truncate(g.SessionID, 24), truncate(g.Tool, 18),
			g.Calls, failed, judged, truncate(g.Sample, 80))
	}
	if len(groups) > topN {
		fmt.Printf("\n（只列前 %d 组，共 %d 组；用 --top 调整）\n", topN, len(groups))
	}
}
