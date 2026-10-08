package explain

import (
	"strings"
	"testing"
)

// 四家 harness 的真实 meta/parent_id 形状（2026-10-08 从 hm.db 实测，见 pair.go 顶部注释）。
const (
	// codex：调用侧 meta.call_id，parent_id 为空；结果侧 parent_id。
	codexCall = `{"call_id":"call_5d57cac7554b4acbbdca0ab9","name":"exec_command"}`
	codexRes  = `{"call_id":"call_5d57cac7554b4acbbdca0ab9"}`
	// hermes：调用侧 meta.call_id 且 parent_id 同值；结果侧 parent_id + meta.tool_call_id。
	hermesCall = `{"call_id":"call_255fd44d614743179f61adb4","tool_name":"kanban_show"}`
	hermesRes  = `{"tool_call_id":"call_255fd44d614743179f61adb4","tool_name":"kanban_show"}`
	// pi：调用侧 meta.call_id，parent_id 为空；结果侧 parent_id + meta.call_id。
	piCall = `{"call_id":"tool:1789958682428:z6uef3clnvn","tool_name":"block"}`
	piRes  = `{"call_id":"tool:1789958682428:z6uef3clnvn","tool_name":"block"}`
)

// 相隔多步的调用 ↔ 结果必须能配对（codex 实测有 1198 次这种情形）。
func TestPairAcrossGap(t *testing.T) {
	events := []Event{
		{Seq: 1, Kind: "turn.start", Title: "user"},
		{Seq: 2, Kind: "llm.reasoning", Title: "reasoning"},
		{Seq: 3, Kind: "tool.call", Title: "exec_command", Meta: codexCall},
		{Seq: 4, Kind: "llm.reasoning", Title: "reasoning"},
		{Seq: 5, Kind: "llm.response", Title: "assistant"},
		{Seq: 6, Kind: "llm.usage", Title: "usage"},
		{Seq: 7, Kind: "turn.end", Title: "turn end"},
		{Seq: 8, Kind: "turn.start", Title: "user"},
		{Seq: 9, Kind: "tool.result", Title: "tool result", Meta: codexRes, ParentID: "call_5d57cac7554b4acbbdca0ab9"},
	}
	ex := ExplainAll(events)

	call := ex[2]
	if call.Round != 1 || call.ResultSeq != 9 {
		t.Fatalf("call: want round=1 resultSeq=9, got round=%d resultSeq=%d", call.Round, call.ResultSeq)
	}
	if !strings.Contains(call.WaitNote, "第 9 步") || !strings.Contains(call.WaitNote, "隔了 5 步") {
		t.Errorf("call WaitNote 未说明间隔: %q", call.WaitNote)
	}
	if call.ToolKey != "call_5d57cac7554b4acbbdca0ab9" {
		t.Errorf("call ToolKey: %q", call.ToolKey)
	}

	res := ex[8]
	if res.Round != 1 || res.CallSeq != 3 {
		t.Fatalf("result: want round=1 callSeq=3, got round=%d callSeq=%d", res.Round, res.CallSeq)
	}
	if !strings.Contains(res.WaitNote, "第 3 步") {
		t.Errorf("result WaitNote 未指认调用步骤: %q", res.WaitNote)
	}
	if res.Summary == "" || !strings.Contains(res.Summary, "exec_command") {
		t.Errorf("result summary 丢了工具名: %q", res.Summary)
	}

	// 相邻的调用/返回不该有多余的间隔说明。
	adj := ExplainAll([]Event{
		{Seq: 1, Kind: "tool.call", Title: "kanban_show", Meta: hermesCall, ParentID: "call_255fd44d614743179f61adb4"},
		{Seq: 2, Kind: "tool.result", Title: "kanban_show", Meta: hermesRes, ParentID: "call_255fd44d614743179f61adb4"},
	})
	if adj[0].WaitNote != "" || adj[1].WaitNote != "" {
		t.Errorf("相邻往返不该有间隔说明: call=%q result=%q", adj[0].WaitNote, adj[1].WaitNote)
	}
	if adj[0].ResultSeq != 2 || adj[1].CallSeq != 1 {
		t.Errorf("相邻往返配对失败: call.resultSeq=%d result.callSeq=%d", adj[0].ResultSeq, adj[1].CallSeq)
	}
}

// 三家的键形状不同，配对规则必须都能吃下（hermes 调用侧 parent_id 与 meta.call_id 同值）。
func TestPairKeyShapes(t *testing.T) {
	cases := []struct {
		name   string
		call   Event
		result Event
	}{
		{"codex", Event{Seq: 1, Kind: "tool.call", Title: "exec_command", Meta: codexCall},
			Event{Seq: 2, Kind: "tool.result", Title: "tool result", Meta: codexRes, ParentID: "call_5d57cac7554b4acbbdca0ab9"}},
		{"hermes", Event{Seq: 1, Kind: "tool.call", Title: "kanban_show", Meta: hermesCall, ParentID: "call_255fd44d614743179f61adb4"},
			Event{Seq: 2, Kind: "tool.result", Title: "kanban_show", Meta: hermesRes, ParentID: "call_255fd44d614743179f61adb4"}},
		{"pi", Event{Seq: 1, Kind: "tool.call", Title: "block", Meta: piCall},
			Event{Seq: 2, Kind: "tool.result", Title: "tool result", Meta: piRes, ParentID: "tool:1789958682428:z6uef3clnvn"}},
		// claude：结果侧只有 parent_id（meta 里是 tool_use_id）。
		{"claude", Event{Seq: 1, Kind: "tool.call", Title: "Bash", Meta: `{"call_id":"call_2dfae66855414f27bb162c1b","tool_name":"Bash"}`},
			Event{Seq: 2, Kind: "tool.result", Title: "tool result", Meta: `{"tool_use_id":"call_2dfae66855414f27bb162c1b"}`, ParentID: "call_2dfae66855414f27bb162c1b"}},
		// 结果侧 meta 只有 call_id、parent_id 为空（兜底路径）。
		{"result-meta-only", Event{Seq: 1, Kind: "tool.call", Title: "block", Meta: piCall},
			Event{Seq: 2, Kind: "tool.result", Title: "tool result", Meta: piRes}},
	}
	for _, c := range cases {
		p := PairEvents([]Event{c.call, c.result})
		if l := p.LinkOfCall(1); l == nil || l.ResultSeq != 2 {
			t.Errorf("%s: 未配对 (link=%+v)", c.name, l)
		}
		if l := p.LinkOfResult(2); l == nil || l.CallSeq != 1 {
			t.Errorf("%s: 结果侧未回指调用 (link=%+v)", c.name, l)
		}
	}
}

// 未返回的调用、缺调用侧的结果都要如实说明，而不是静默错配。
func TestPairIncomplete(t *testing.T) {
	events := []Event{
		{Seq: 1, Kind: "tool.call", Title: "exec_command", Meta: codexCall},                                  // 没有结果
		{Seq: 2, Kind: "tool.result", Title: "tool result", Meta: codexRes, ParentID: "call_不存在的id"},        // 没有调用
		{Seq: 3, Kind: "tool.result", Title: "tool result", Meta: `{}`, ParentID: ""},                        // 连键都没有
	}
	p := PairEvents(events)
	ex := ExplainAll(events)

	if !strings.Contains(ex[0].WaitNote, "没有看到返回结果") {
		t.Errorf("未返回的调用未说明: %q", ex[0].WaitNote)
	}
	if got := p.UnreturnedCalls(events); len(got) != 1 || got[0] != 1 {
		t.Errorf("UnreturnedCalls: %v", got)
	}
	if got := p.OrphanResults(events); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("OrphanResults: %v", got)
	}
	if !strings.Contains(ex[1].WaitNote, "找不到与之配对的调用") {
		t.Errorf("孤儿结果未说明: %q", ex[1].WaitNote)
	}
	if ex[1].CallSeq != 0 {
		t.Errorf("孤儿结果不该有 callSeq: %d", ex[1].CallSeq)
	}
}

// 同一种工具调用多次、结果乱序回来时，靠配对键区分，不能串台。
func TestPairNoCrossTalk(t *testing.T) {
	events := []Event{
		{Seq: 1, Kind: "tool.call", Title: "read_file", Meta: `{"call_id":"A","tool_name":"read_file"}`},
		{Seq: 2, Kind: "tool.call", Title: "read_file", Meta: `{"call_id":"B","tool_name":"read_file"}`},
		{Seq: 3, Kind: "tool.result", Title: "tool result", Meta: `{"call_id":"B"}`, ParentID: "B"},
		{Seq: 4, Kind: "tool.result", Title: "tool result", Meta: `{"call_id":"A"}`, ParentID: "A"},
	}
	p := PairEvents(events)
	if l := p.LinkOfCall(1); l == nil || l.ResultSeq != 4 {
		t.Errorf("调用 A 应配 seq=4: %+v", l)
	}
	if l := p.LinkOfCall(2); l == nil || l.ResultSeq != 3 {
		t.Errorf("调用 B 应配 seq=3: %+v", l)
	}
	ex := ExplainAll(events)
	if ex[0].Round != 1 || ex[1].Round != 2 {
		t.Errorf("轮次编号: %d / %d", ex[0].Round, ex[1].Round)
	}
	if ex[0].ToolKey != "A" || ex[1].ToolKey != "B" {
		t.Errorf("配对键透传: %q / %q", ex[0].ToolKey, ex[1].ToolKey)
	}
}

// 非工具步骤不受配对影响（回归保护：ExplainAll 的旧行为不能变）。
func TestNonToolStepsUnaffected(t *testing.T) {
	events := []Event{
		{Seq: 1, Kind: "turn.start", Title: "user"},
		{Seq: 2, Kind: "llm.response", Title: "assistant"},
	}
	ex := ExplainAll(events)
	if ex[0].Round != 0 || ex[0].ToolKey != "" || ex[0].WaitNote != "" {
		t.Errorf("turn.start 被污染: %+v", ex[0])
	}
	if !strings.Contains(ex[1].To, "最终回答") {
		t.Errorf("llm.response 说明变了: %q", ex[1].To)
	}
}
