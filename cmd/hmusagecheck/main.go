// cmd/hmusagecheck：把一个会话的用量/成本合计打印出来（开发验证用，与 explainpreview 同型）。
//
// 用途：核对 internal/usage 的合计是否与源文件一致——尤其 codex 的「会话累计快照」
// 与其余四家的「逐条相加」两种口径。不带 harness 时列出各家用量记录最多的会话。
//
// 运行：go run ./cmd/hmusagecheck [harness] [session-id]
package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
	"github.com/qifeng2692/harness-monitor/internal/usage"
)

func main() {
	s, err := store.Open()
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer s.Close()

	harness, sid := "", ""
	if len(os.Args) > 1 {
		harness = os.Args[1]
	}
	if len(os.Args) > 2 {
		sid = os.Args[2]
	}

	if harness == "" {
		listTop(s)
		return
	}
	if sid == "" {
		listSessions(s, harness)
		return
	}
	printOne(s, harness, sid)
}

func listTop(s *store.Store) {
	hs, err := s.Harnesses()
	if err != nil {
		fmt.Println("harnesses:", err)
		os.Exit(1)
	}
	fmt.Println("harness      事件数     会话数")
	for _, h := range hs {
		fmt.Printf("%-10s %10d %8d\n", h.Harness, h.EventCount, h.SessionCount)
	}
	fmt.Println("\n用法: hmusagecheck <harness> [session-id]")
}

func listSessions(s *store.Store, harness string) {
	sessions, err := s.ListSessions(harness)
	if err != nil {
		fmt.Println("sessions:", err)
		os.Exit(1)
	}
	type row struct {
		sid   string
		recs  int64
		total int64
	}
	rows := make([]row, 0, len(sessions))
	for _, sess := range sessions {
		evs, err := s.ListEvents(harness, sess.SessionID)
		if err != nil {
			continue
		}
		u := usage.Summarize(harness, evs)
		if u.Records == 0 && u.Skipped == 0 {
			continue
		}
		rows = append(rows, row{sess.SessionID, u.Records, u.TotalTokens})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].total > rows[j].total })
	if len(rows) > 15 {
		rows = rows[:15]
	}
	fmt.Printf("%s 有用量记录的会话（按合计 token 降序，前 %d 个）：\n", harness, len(rows))
	for _, r := range rows {
		fmt.Printf("  %-40s 记录 %6d  合计 %12d tokens\n", r.sid, r.recs, r.total)
	}
}

func printOne(s *store.Store, harness, sid string) {
	evs, err := s.ListEvents(harness, sid)
	if err != nil {
		fmt.Println("events:", err)
		os.Exit(1)
	}
	u := usage.Summarize(harness, evs)

	fmt.Printf("harness=%s session=%s\n", harness, sid)
	fmt.Printf("  用量记录    : %d 条（另有 %d 条无法解析被跳过）\n", u.Records, u.Skipped)
	fmt.Printf("  口径        : %s\n", harnessNote(harness))
	fmt.Printf("  输入 token  : %d\n", u.InputTokens)
	fmt.Printf("  输出 token  : %d\n", u.OutputTokens)
	if u.CacheRead != 0 {
		fmt.Printf("  缓存读      : %d\n", u.CacheRead)
	}
	if u.CacheWrite != 0 {
		fmt.Printf("  缓存写      : %d\n", u.CacheWrite)
	}
	if u.Reasoning != 0 {
		fmt.Printf("  推理 token  : %d\n", u.Reasoning)
	}
	fmt.Printf("  合计 token  : %d\n", u.TotalTokens)
	if u.CostKnown {
		fmt.Printf("  费用        : $%.8f\n", u.CostUSD)
	} else {
		fmt.Printf("  费用        : 未知（该 harness 的记录里没有费用字段）\n")
	}
	if u.FailedCalls != 0 {
		fmt.Printf("  失败调用    : %d（非 200 状态）\n", u.FailedCalls)
	}
	if len(u.Models) > 0 {
		fmt.Println("  分模型：")
		for _, m := range u.Models {
			cost := ""
			if m.CostUSD != 0 {
				cost = fmt.Sprintf("  $%.6f", m.CostUSD)
			}
			fmt.Printf("    %-32s %8d 条  %12d tokens%s\n", m.Model, m.Records, m.TotalTokens, cost)
		}
	}
	// 原始用量事件条数（用于人工核对是否漏采）。
	var raw int
	for _, e := range evs {
		if e.Kind == model.KindLLMUsage {
			raw++
		}
	}
	fmt.Printf("  （会话内 llm.usage 事件 %d 条，其中可解析 %d 条）\n", raw, u.Records)
}

// harnessNote 说明该 harness 的用量记录形态（人工核对时知道自己在看什么）。
func harnessNote(harness string) string {
	switch harness {
	case "codex":
		return "逐次调用增量（取 last_token_usage；total_token_usage 是按 rollout 文件重置的累计计数器，不可相加）"
	case "hermes":
		return "「会话 × 模型」汇总行相加"
	case "claude":
		return "每会话一条 cost-state 汇总（含分模型明细）"
	case "pi":
		return "逐次模型调用的用量相加"
	case "magpie":
		return "逐次模型调用的用量相加（含 status，可看限流/失败）"
	default:
		return "逐条相加"
	}
}
