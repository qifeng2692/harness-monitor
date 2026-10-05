package explain

import (
	"strings"
	"testing"
)

func mk(kind, title, meta string) Event {
	return Event{Kind: kind, Title: title, Meta: meta}
}

func TestExplainFlow(t *testing.T) {
	events := []Event{
		mk("turn.start", "user", ""),
		mk("llm.reasoning", "reasoning", ""),
		mk("tool.call", "terminal", `{"tool_name":"terminal"}`),
		mk("tool.result", "tool result", `{"tool_name":"terminal"}`),
		mk("llm.reasoning", "reasoning", ""),
		mk("llm.response", "assistant", ""),
	}
	check := func(i int, wantSub, field, got string) {
		if !strings.Contains(got, wantSub) {
			t.Errorf("step %d %s: want contains %q, got %q", i, field, wantSub, got)
		}
	}

	ex := ExplainOne(&events[0], nil, &events[1])
	check(1, "用户", "summary", ex.Summary)
	check(1, "是", "visible", ex.Visible)
	check(1, "模型", "to", ex.To)

	ex = ExplainOne(&events[1], &events[0], &events[2])
	check(2, "打草稿", "summary", ex.Summary)
	check(2, "否", "visible", ex.Visible)

	ex = ExplainOne(&events[2], &events[1], &events[3])
	check(3, "terminal", "summary", ex.Summary)
	check(3, "否", "visible", ex.Visible)
	check(3, "工具", "to", ex.To)

	ex = ExplainOne(&events[3], &events[2], &events[4])
	check(4, "工具", "summary", ex.Summary)
	check(4, "模型", "to", ex.To)

	ex = ExplainOne(&events[5], &events[4], nil)
	check(6, "最终回答", "to", ex.To)
	check(6, "是", "visible", ex.Visible)

	ex = ExplainOne(&events[0], nil, &events[1])
	check(1, "模型思考", "nextHint", ex.NextHint)

	mid := []Event{
		mk("llm.response", "assistant", ""),
		mk("tool.call", "read_file", `{"tool_name":"read_file"}`),
	}
	ex = ExplainOne(&mid[0], nil, &mid[1])
	check(0, "read_file", "to", ex.To)
	check(0, "过渡", "visible", ex.Visible)
}
