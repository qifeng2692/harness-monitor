package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/sessionstate"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

// runStatus 实现子命令 status：一眼看出每个会话「现在卡在哪」。
//
// 为什么需要：hm 的其它子命令都在讲「发生了什么」（ls / show / search / export），
// 唯独没有一条命令能回答「现在有哪个会话在等我回答、哪个刚失败」——用户只能
// 逐个点开会话翻时间线。同类工具（tmux-agent-sidebar 的状态列、MioIsland 的
// 待批准提醒）都把这一条当核心界面。
//
// 口径纪律：
//   - 「等你回答」只认结构化事实（存在等人回答类调用 + 没拿到回答 + 用户之后没再说话）；
//   - 「失败」只认 error 事件；「已完成」只认 turn.end / session.end。
//     判不出就说判不出，不猜正文。
func runStatus(args []string) error {
	harness := ""
	showAll := false
	positional := []string{}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--harness" && i+1 < len(args):
			harness = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--harness="):
			harness = strings.TrimPrefix(args[i], "--harness=")
		case args[i] == "--all":
			showAll = true
		case args[i] == "-h" || args[i] == "--help":
			printUsage()
			return nil
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 0 {
		printUsage()
		return fmt.Errorf("status 不接受位置参数（给了 %d 个）", len(positional))
	}

	s, err := store.Open()
	if err != nil {
		return err
	}
	defer s.Close()

	facts, err := s.SessionFacts(harness)
	if err != nil {
		return err
	}
	if len(facts) == 0 {
		scope := "全部 harness"
		if harness != "" {
			scope = harness
		}
		fmt.Printf("无会话（范围：%s）\n", scope)
		return nil
	}

	now := time.Now()
	sessions := make([]sessionstate.Session, 0, len(facts))
	for _, f := range facts {
		sessions = append(sessions, sessionstate.Classify(f, now, sessionstate.DefaultActiveWindow))
	}
	sum := sessionstate.Summarize(sessions)

	// 排序：先按「要你出手」的程度（等待 > 失败 > 进行中 > 已完成 > 空闲），
	// 同级按最近活动倒序——最该看的排最前。
	rank := map[sessionstate.State]int{
		sessionstate.StateWaiting: 0,
		sessionstate.StateFailed:  1,
		sessionstate.StateActive:  2,
		sessionstate.StateDone:    3,
		sessionstate.StateIdle:    4,
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		if rank[sessions[i].State] != rank[sessions[j].State] {
			return rank[sessions[i].State] < rank[sessions[j].State]
		}
		return sessions[i].LastAt.After(sessions[j].LastAt)
	})

	fmt.Printf("== 会话状态（%d 个会话）==\n", sum.Total)
	fmt.Printf("  等待你 %d · 失败 %d · 进行中 %d · 已完成 %d · 空闲 %d\n\n",
		sum.Counts[sessionstate.StateWaiting],
		sum.Counts[sessionstate.StateFailed],
		sum.Counts[sessionstate.StateActive],
		sum.Counts[sessionstate.StateDone],
		sum.Counts[sessionstate.StateIdle])

	// 默认只列「需要你出手」的会话（等待 + 失败）——这就是这个命令的用途。
	// --all 才列全部。
	shown := 0
	for _, sess := range sessions {
		if !showAll && sess.State != sessionstate.StateWaiting && sess.State != sessionstate.StateFailed {
			continue
		}
		shown++
		fmt.Printf("%-8s %-38s %-6s %s\n", sess.Harness, truncate(sess.SessionID, 38),
			sess.State.Label(), sess.Reason)
		// 问题正文单独一行（Reason 里已经概括过，这里不重复）。
		if sess.PendingAsk != nil && sess.PendingAsk.Question != "" {
			fmt.Printf("         └ %s\n", truncate(sess.PendingAsk.Question, 100))
		}
	}
	if shown == 0 {
		if showAll {
			fmt.Println("（无会话）")
		} else {
			fmt.Println("没有等你出手的会话——没有会话在等回答，也没有会话以失败告终。")
			fmt.Println("（要看全部会话状态，加 --all）")
		}
		return nil
	}
	if !showAll {
		fmt.Printf("\n（只列了要你出手的会话；看全部加 --all，限定某一家加 --harness <name>）\n")
	}
	return nil
}
