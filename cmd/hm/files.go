package main

import (
	"fmt"
	"strings"

	"github.com/qifeng2692/harness-monitor/internal/filechange"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

// runFiles 实现子命令 files：一个会话到底改动了哪些文件。
//
// 为什么需要：hm 已有 ls / show / search / export / stats / status / loops，
// 分别回答「有哪些会话」「发生过什么」「怎么找到它」「花了多少」「卡在哪」「有没有
// 打转」，唯独答不了「它动了哪些文件」。同类产品（agentacct 的 Work Receipt）
// 把 files changed 摆在最前面——人复查一次 agent 的工作，先看它改了哪几个文件。
//
// 口径纪律：
//   - 只认参数里明确写了路径的写文件工具（判定表在 internal/filechange，只一份）；
//   - 通过 shell 命令改的文件（codex exec_command、claude/hermes/pi 的 bash）
//     记录里没有结构化字段，**不计入、也不去解析命令行**——缺失胜过猜错，输出里明说。
func runFiles(args []string) error {
	positional := []string{}
	for _, a := range args {
		switch a {
		case "-h", "--help":
			printUsage()
			return nil
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 2 {
		printUsage()
		return fmt.Errorf("files 需要两个参数：<harness> <session-id>（给了 %d 个）", len(positional))
	}
	harness, sessionID := positional[0], positional[1]

	s, err := store.Open()
	if err != nil {
		return err
	}
	defer s.Close()

	if !filechange.Supports(harness) {
		fmt.Printf("%s 没有结构化的写文件工具，给不出文件清单。\n", harness)
		fmt.Println("它改文件的方式是跑 shell 命令（命令行文本里才有路径），解析命令行等于猜，本命令不做。")
		fmt.Printf("能给出文件清单的是：%s。\n", strings.Join(filechange.Supported(), " / "))
		return nil
	}

	rows, err := s.SessionToolCalls(harness, sessionID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Printf("无工具调用记录：harness=%s session=%s\n", harness, sessionID)
		return nil
	}

	calls := make([]filechange.Call, 0, len(rows))
	for _, r := range rows {
		calls = append(calls, filechange.Call{Tool: r.Tool, Args: r.Args, Seq: r.Seq, Time: r.Time})
	}
	sum := filechange.Summarize(harness, calls)

	if len(sum.Changes) == 0 {
		fmt.Printf("%s %s — 没有改动文件\n", harness, sessionID)
		fmt.Printf("  %d 次工具调用都不是写文件类（读、搜、执行……）", len(rows))
		if sum.NoPath > 0 {
			fmt.Printf("，另有 %d 次写文件调用参数里没带路径", sum.NoPath)
		}
		fmt.Println("。")
		return nil
	}

	fmt.Printf("%s %s — 改动了 %d 个文件，共 %d 次改动\n\n", harness, sessionID, len(sum.Changes), sum.Calls)
	fmt.Printf("%5s  %-10s  %-16s  %8s  %s\n", "次数", "操作", "工具", "首次步骤", "文件")
	for _, ch := range sum.Changes {
		ops := make([]string, 0, len(ch.Ops))
		for _, op := range ch.Ops {
			ops = append(ops, string(op))
		}
		fmt.Printf("%5d  %-10s  %-16s  %8d  %s\n",
			ch.Count, strings.Join(ops, "+"), truncate(strings.Join(ch.Tools, ","), 16), ch.FirstSeq, ch.Path)
	}

	fmt.Println()
	if sum.NoPath > 0 {
		fmt.Printf("另有 %d 次写文件调用参数里没有文件路径，未计入。\n", sum.NoPath)
	}
	fmt.Println("只统计参数里明确写了文件路径的写文件工具；通过 shell 命令（terminal / bash / exec_command）改的文件没有结构化字段，不在其中。")
	return nil
}
