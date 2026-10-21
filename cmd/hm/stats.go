package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/qifeng2692/harness-monitor/internal/stats"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

// runStats 实现子命令 stats：跨会话统计报告——规模、用量、工具、失败四个维度。
//
// 为什么需要：此前 hm 的子命令全是「单会话视角」（ls / show / export / search），
// 想回答「我这段时间用了多少 token、哪个工具最常失败」只能自己导出会话再手算。
// 同类工具（sniffly、token-dashboard）的首页都是这种跨会话概览。
//
// 口径纪律：失败只认各家自己上报的结构化字段；「判不了」与「没失败」分开报。
func runStats(args []string) error {
	harness := ""
	topN := 10
	positional := []string{}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--harness" && i+1 < len(args):
			harness = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--harness="):
			harness = strings.TrimPrefix(args[i], "--harness=")
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
		return fmt.Errorf("stats 不接受位置参数（给了 %d 个）", len(positional))
	}

	s, err := store.Open()
	if err != nil {
		return err
	}
	defer s.Close()

	rep, err := buildReport(s, harness)
	if err != nil {
		return err
	}
	if len(rep.Harnesses) == 0 {
		scope := "全部 harness"
		if harness != "" {
			scope = harness
		}
		fmt.Printf("无统计数据（范围：%s）\n", scope)
		return nil
	}

	printStatsReport(rep, topN)
	return nil
}

// buildReport 取数并汇总。harness 非空时只统计该家。
func buildReport(s *store.Store, harness string) (stats.Report, error) {
	in := stats.Input{}

	hs, err := s.StatsHarnessStats()
	if err != nil {
		return stats.Report{}, err
	}
	for _, h := range hs {
		if harness != "" && h.Harness != harness {
			continue
		}
		in.Harnesses = append(in.Harnesses, h)
	}

	rows, err := s.StatsUsageRows()
	if err != nil {
		return stats.Report{}, err
	}
	for _, r := range rows {
		if harness != "" && r.Harness != harness {
			continue
		}
		in.UsageRows = append(in.UsageRows, r)
	}

	tools, err := s.StatsToolStats()
	if err != nil {
		return stats.Report{}, err
	}
	for _, t := range tools {
		if harness != "" && t.Harness != harness {
			continue
		}
		in.Tools = append(in.Tools, t)
	}

	fails, err := s.StatsErrorStats()
	if err != nil {
		return stats.Report{}, err
	}
	for _, f := range fails {
		if harness != "" && f.Harness != harness {
			continue
		}
		in.Failures = append(in.Failures, f)
	}

	return stats.Build(in), nil
}

// printStatsReport 打印报告。布局原则：先给总账，再给各家明细，最后给热点与失败。
func printStatsReport(rep stats.Report, topN int) {
	t := rep.Totals

	fmt.Println("== 总览 ==")
	fmt.Printf("  harness      %d 家\n", t.Harnesses)
	fmt.Printf("  会话         %d\n", t.Sessions)
	fmt.Printf("  事件         %d\n", t.Events)
	fmt.Printf("  工具调用     %d\n", t.ToolCalls)
	// 拆分用「新输入 + 缓存读 + 输出」而不是「输入 + 缓存读 + 输出」：
	// codex 的输入字段本身含缓存（实测 97.4% 都是缓存命中），直接相加会把缓存
	// 算两遍、总数与三项之和对不上。用新输入这一项，三项之和恰好等于处理 token。
	fmt.Printf("  处理 token   %d（新输入 %d · 缓存读 %d · 输出 %d）\n",
		t.ProcessedTokens, t.FreshInput, t.CacheRead, t.OutputTokens)
	if t.CostKnown {
		fmt.Printf("  费用         $%.6f\n", t.CostUSD)
	} else {
		fmt.Println("  费用         未知（记录里没有费用字段）")
	}
	if t.FailedCalls > 0 {
		fmt.Printf("  失败调用     %d（用量记录里的非 200 状态）\n", t.FailedCalls)
	}
	if t.JudgedTools > 0 {
		fmt.Printf("  工具失败     %d / 判定 %d 条", t.FailedTools, t.JudgedTools)
		if t.Unjudged > 0 {
			fmt.Printf("（另有 %d 条工具结果不带失败信号，判不了）", t.Unjudged)
		}
		fmt.Println()
	} else if t.ToolResults > 0 {
		fmt.Printf("  工具失败     判不了（%d 条工具结果都不带失败信号字段）\n", t.ToolResults)
	}

	fmt.Println()
	fmt.Println("== 各家分布 ==")
	fmt.Printf("%-10s %8s %10s %14s %10s %10s %8s\n",
		"HARNESS", "会话", "事件", "处理token", "新输入", "工具失败", "错误事件")
	for _, h := range rep.Harnesses {
		failed := "-"
		if h.JudgedTools > 0 {
			failed = fmt.Sprintf("%d/%d", h.FailedTools, h.JudgedTools)
		}
		fmt.Printf("%-10s %8d %10d %14d %10d %10s %8d\n",
			h.Name, h.Sessions, h.Events, h.ProcessedTokens,
			h.FreshInput, failed, h.ErrorEvents)
	}

	if len(rep.Tools) > 0 {
		fmt.Println()
		fmt.Printf("== 最常用的工具（前 %d）==\n", topN)
		fmt.Printf("%-10s %-24s %8s %10s %10s\n", "HARNESS", "工具", "调用", "失败", "判定")
		for i, tool := range rep.Tools {
			if i >= topN {
				break
			}
			fmt.Printf("%-10s %-24s %8d %10d %10d\n",
				tool.Harness, truncate(tool.Name, 24), tool.Calls, tool.Failed, tool.Judged)
		}
	}

	if len(rep.Failures) > 0 {
		fmt.Println()
		fmt.Printf("== 失败原因（前 %d）==\n", topN)
		fmt.Printf("%-10s %-44s %8s %20s\n", "HARNESS", "原因", "次数", "最近")
		for i, f := range rep.Failures {
			if i >= topN {
				break
			}
			fmt.Printf("%-10s %-44s %8d %20s\n",
				f.Harness, truncate(f.Title, 44), f.Count, fmtTime(f.Last))
		}
	}
}

// atoi 解析正整数（复用 strconv，错误信息对用户可读）。
func atoi(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("不是数字: %q", s)
	}
	return n, nil
}
