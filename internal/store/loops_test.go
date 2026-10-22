package store

import (
	"testing"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// TestStatsLoopCallsReadsCallID 覆盖取数层：调用侧的键取 meta.call_id，
// 参数原文取 content（判重的前提）。
func TestStatsLoopCallsReadsCallID(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		{Harness: "hermes", SessionID: "s1", Kind: model.KindToolCall, Title: "terminal",
			Content: `{"command":"ls"}`, Meta: `{"call_id":"c1","tool_name":"terminal"}`, SourceKey: "t:0"},
		{Harness: "hermes", SessionID: "s1", Kind: model.KindToolCall, Title: "terminal",
			Content: `{"command":"pwd"}`, Meta: `{"call_id":"c2","tool_name":"terminal"}`, SourceKey: "t:1"},
		// codex 的工具名在 meta.name 而不是 meta.tool_name——但 title 就是工具名，取 title 即可。
		{Harness: "codex", SessionID: "c1", Kind: model.KindToolCall, Title: "exec_command",
			Content: `{"cmd":"dir"}`, Meta: `{"call_id":"c3","name":"exec_command"}`, SourceKey: "t:2"},
		// 非工具调用事件不该被取进来。
		{Harness: "hermes", SessionID: "s1", Kind: model.KindLLMResponse, Title: "回答",
			SourceKey: "t:3"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	calls, err := s.StatsLoopCalls("")
	if err != nil {
		t.Fatalf("StatsLoopCalls: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("应有 3 条调用，得到 %d", len(calls))
	}
	byKey := map[string]LoopCall{}
	for _, c := range calls {
		byKey[c.Key] = c
	}
	if got := byKey["c1"]; got.Args != `{"command":"ls"}` {
		t.Errorf("c1 的参数 = %q，期望原样取 content", got.Args)
	}
	if got := byKey["c1"]; got.Tool != "terminal" {
		t.Errorf("c1 的工具名 = %q，期望 terminal", got.Tool)
	}
	if got := byKey["c3"]; got.Tool != "exec_command" {
		t.Errorf("codex 那条的工具名 = %q，期望 exec_command", got.Tool)
	}
	if got := byKey["c2"]; got.Args != `{"command":"pwd"}` {
		t.Errorf("c2 的参数 = %q，期望原样取 content", got.Args)
	}

	// 按 harness 过滤。
	only, err := s.StatsLoopCalls("codex")
	if err != nil {
		t.Fatalf("StatsLoopCalls(codex): %v", err)
	}
	if len(only) != 1 || only[0].Harness != "codex" {
		t.Errorf("按 harness 过滤应只剩 1 条 codex，得到 %+v", only)
	}
}

// TestStatsLoopResultsJudgement 覆盖失败判定的取数口径：
//   - hermes：成功也写 is_error=false → 判得了（Judged=true, Failed=false）；
//   - pi    ：只在失败时写 is_error=true → 判得了（Judged=true, Failed=true）；
//   - codex：不带 is_error 键 → 判不了（Judged=false, Failed=false）；
//   - meta 是非法 JSON → 判不了（不能把解析失败当成成功）。
func TestStatsLoopResultsJudgement(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		{Harness: "hermes", SessionID: "s1", Kind: model.KindToolResult, Title: "terminal",
			Meta: `{"tool_name":"terminal","is_error":false,"exit_code":0}`, ParentID: "c1", SourceKey: "r:0"},
		{Harness: "hermes", SessionID: "s1", Kind: model.KindToolResult, Title: "terminal",
			Meta: `{"tool_name":"terminal","is_error":true,"exit_code":1}`, ParentID: "c2", SourceKey: "r:1"},
		{Harness: "pi", SessionID: "p1", Kind: model.KindToolResult, Title: "tool result",
			Meta: `{"call_id":"c3","is_error":true}`, ParentID: "c3", SourceKey: "r:2"},
		{Harness: "codex", SessionID: "c1", Kind: model.KindToolResult, Title: "tool result",
			Meta: `{"call_id":"c4"}`, ParentID: "c4", SourceKey: "r:3"},
		{Harness: "codex", SessionID: "c1", Kind: model.KindToolResult, Title: "tool result",
			Meta: `{"call_id":"c5","is_error":false}`, ParentID: "c5", SourceKey: "r:4"},
		// 非法 JSON：不能当成成功，也不能当成失败——判不了。
		{Harness: "codex", SessionID: "c1", Kind: model.KindToolResult, Title: "tool result",
			Meta: `{不是 JSON`, ParentID: "c6", SourceKey: "r:5"},
		// 没有 parent_id 的结果挂不上任何调用，不该被取出来。
		{Harness: "codex", SessionID: "c1", Kind: model.KindToolResult, Title: "tool result",
			Meta: `{"call_id":"c7","is_error":true}`, SourceKey: "r:6"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	results, err := s.StatsLoopResults("")
	if err != nil {
		t.Fatalf("StatsLoopResults: %v", err)
	}
	if len(results) != 6 {
		t.Fatalf("应有 6 条结果（无 parent_id 的那条被排除），得到 %d", len(results))
	}

	byKey := map[string]LoopResult{}
	for _, r := range results {
		byKey[r.Key] = r
	}
	if got := byKey["c1"]; !got.Judged || got.Failed {
		t.Errorf("hermes 成功结果应判得了且不算失败，得到 %+v", got)
	}
	if got := byKey["c2"]; !got.Judged || !got.Failed {
		t.Errorf("hermes 失败结果应判得了且算失败，得到 %+v", got)
	}
	if got := byKey["c3"]; !got.Judged || !got.Failed {
		t.Errorf("pi 失败结果应判得了且算失败，得到 %+v", got)
	}
	if got := byKey["c4"]; got.Judged || got.Failed {
		t.Errorf("codex 不带 is_error 的结果应判不了，得到 %+v", got)
	}
	// codex 显式写了 is_error=false → 判得了，但不是失败。
	if got := byKey["c5"]; !got.Judged || got.Failed {
		t.Errorf("codex is_error=false 应判得了且不算失败，得到 %+v", got)
	}
	if got := byKey["c6"]; got.Judged || got.Failed {
		t.Errorf("非法 JSON 的结果应判不了，得到 %+v", got)
	}
	if _, ok := byKey["c7"]; ok {
		t.Error("没有 parent_id 的结果不该被取出来")
	}
}
