// cmd/explainpreview/main.go：预览真实会话的自然语言步骤说明（开发验证用）。
//
// 运行：go run ./cmd/explainpreview <harness> <session-id> [start] [count]
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/qifeng2692/harness-monitor/internal/explain"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: explainpreview <harness> <session-id> [start] [count]")
		os.Exit(1)
	}
	harness, sid := os.Args[1], os.Args[2]
	start, count := 0, 8
	if len(os.Args) > 3 {
		start, _ = strconv.Atoi(os.Args[3])
	}
	if len(os.Args) > 4 {
		count, _ = strconv.Atoi(os.Args[4])
	}

	s, err := store.Open()
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer s.Close()

	events, err := s.ListEvents(harness, sid)
	if err != nil {
		fmt.Println("list:", err)
		os.Exit(1)
	}

	// 转成 explain 包的输入格式
	items := make([]explain.Event, 0, len(events))
	for _, e := range events {
		items = append(items, explain.Event{
			Seq: e.Seq, Kind: e.Kind, Title: e.Title, Meta: e.Meta,
		})
	}

	explains := explain.ExplainAll(items)
	end := start + count
	if end > len(items) {
		end = len(items)
	}
	for i := start; i < end; i++ {
		e := events[i]
		x := explains[i]
		fmt.Printf("━━━ 步骤 %d/%d  [%s] %s ━━━\n", i+1, len(items), e.Kind, e.Title)
		fmt.Printf("  这一步      : %s\n", x.Summary)
		fmt.Printf("  输入来自    : %s\n", x.From)
		fmt.Printf("  输出去向    : %s\n", x.To)
		fmt.Printf("  给用户看吗  : %s\n", x.Visible)
		fmt.Printf("  为什么流转  : %s\n", x.Why)
		fmt.Printf("  衔接        : %s\n\n", x.NextHint)
	}
}
