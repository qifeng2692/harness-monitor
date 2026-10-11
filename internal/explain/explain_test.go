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

// TestExplainErrorSteps 验证 error 步骤说的是「哪一层失败、意味着什么」，
// 而不是一句笼统的「这一步出错了」——各采集器的 meta.type 决定措辞。
func TestExplainErrorSteps(t *testing.T) {
	cases := []struct {
		name    string
		meta    string
		wantSub []string
	}{
		{"回合被中断", `{"type":"turn_aborted","reason":"interrupted"}`, []string{"中断", "回合"}},
		{"接口失败", `{"type":"api_error","status":502}`, []string{"接口", "重试"}},
		{"模型调用失败", `{"type":"stop_reason_error"}`, []string{"模型", "结果"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := Event{Kind: "error", Title: "失败", Meta: c.meta}
			ex := ExplainOne(&e, nil, nil)
			joined := ex.Summary + ex.Why
			for _, sub := range c.wantSub {
				if !strings.Contains(joined, sub) {
					t.Errorf("说明里应出现 %q：summary=%q why=%q", sub, ex.Summary, ex.Why)
				}
			}
			if strings.Contains(ex.Summary, "这一步出错了。") {
				t.Errorf("已知类型不该退化成笼统文案：%q", ex.Summary)
			}
		})
	}
}

// TestExplainFailedToolResult 验证标记为失败的工具结果在说明里如实说明失败，
// 未标记的仍说「执行完毕」。
func TestExplainFailedToolResult(t *testing.T) {
	failed := Event{Kind: "tool.result", Title: "terminal", Meta: `{"tool_name":"terminal","is_error":true,"exit_code":1}`}
	ex := ExplainOne(&failed, nil, nil)
	if !strings.Contains(ex.Summary, "失败") {
		t.Errorf("失败结果应说明失败，得到 %q", ex.Summary)
	}
	if !strings.Contains(ex.Why, "退出码 1") {
		t.Errorf("应带出退出码，得到 %q", ex.Why)
	}

	ok := Event{Kind: "tool.result", Title: "terminal", Meta: `{"tool_name":"terminal","is_error":false,"exit_code":0}`}
	ex = ExplainOne(&ok, nil, nil)
	if !strings.Contains(ex.Summary, "执行完毕") {
		t.Errorf("正常结果不该说失败，得到 %q", ex.Summary)
	}
}
