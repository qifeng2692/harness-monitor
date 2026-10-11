package collect

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// eventsOfKind 过滤出指定 kind 的事件。
func eventsOfKind(evs []model.Event, kind string) []model.Event {
	var out []model.Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// metaOf 把事件的 meta 解析成 map（断言用）。
func metaOf(t *testing.T, e model.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if e.Meta == "" {
		return m
	}
	if err := json.Unmarshal([]byte(e.Meta), &m); err != nil {
		t.Fatalf("meta 非 JSON: %v (%s)", err, e.Meta)
	}
	return m
}

// TestCodexTurnAbortedIsError 验证 codex 的 turn_aborted 事件此前被当作「未映射类型」
// 丢弃（时间线上看不出这一轮没跑完），现在采成 error 事件并带上中断原因。
func TestCodexTurnAbortedIsError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_SESSIONS_DIR", dir)

	lines := `{"timestamp":"2026-02-04T08:59:00.000Z","type":"session_meta","payload":{"session_id":"s-1","cwd":"G:\\proj"}}` + "\n" +
		`{"timestamp":"2026-02-04T08:59:35.865Z","ordinal":13,"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"rollout-6","reason":"interrupted"}}` + "\n"
	evs := collectFileInto(t, "codex", dir, "rollout-2026-02-04T16-58-44-019c.jsonl", lines)

	es := eventsOfKind(evs, model.KindError)
	if len(es) != 1 {
		t.Fatalf("应采到 1 条 error，得到 %d（全部 kind=%v）", len(es), kindsOf(evs))
	}
	m := metaOf(t, es[0])
	if m["type"] != "turn_aborted" {
		t.Errorf("meta.type = %v，期望 turn_aborted", m["type"])
	}
	if m["reason"] != "interrupted" {
		t.Errorf("meta.reason = %v，期望 interrupted", m["reason"])
	}
	if es[0].Content != "interrupted" {
		t.Errorf("content 应为中断原因，得到 %q", es[0].Content)
	}
}

// TestClaudeAPIErrorIsError 验证 claude 的两处失败信号都被采成 error：
// type=system/subtype=api_error（重试记录）与 type=assistant/isApiErrorMessage=true
// （网关把报错写成「模型回复」的合成行——此前会被当成正常回复采进 llm.response）。
func TestClaudeAPIErrorIsError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", dir)

	sysLine := `{"type":"system","subtype":"api_error","level":"error",` +
		`"error":{"message":"502 status code (no body)","status":502},` +
		`"retryAttempt":1,"maxRetries":10,"source":"request_retry","timestamp":"2026-10-05T04:41:47.644Z"}`
	// 合成行：message.model 是 <synthetic>，报错文本在 content[0].text。
	apiMsg := `{"type":"assistant","timestamp":"2026-10-05T04:36:01.704Z","error":"server_error",` +
		`"isApiErrorMessage":true,"apiErrorStatus":502,"message":{"model":"<synthetic>","role":"assistant",` +
		`"content":[{"type":"text","text":"API Error: 502 status code (no body)."}]}}`
	evs := collectFileInto(t, "claude", dir, "s-1.jsonl", sysLine+"\n"+apiMsg+"\n")

	es := eventsOfKind(evs, model.KindError)
	if len(es) != 2 {
		t.Fatalf("应采到 2 条 error，得到 %d（全部 kind=%v）", len(es), kindsOf(evs))
	}
	// 两条都带 HTTP 状态码。
	for i, e := range es {
		if m := metaOf(t, e); m["status"] != float64(502) {
			t.Errorf("第 %d 条 meta.status = %v，期望 502", i+1, m["status"])
		}
	}
	// 重试记录保留重试次数。
	if m := metaOf(t, es[0]); m["retry_attempt"] != float64(1) || m["max_retries"] != float64(10) {
		t.Errorf("重试字段丢失：%v", m)
	}
	// 报错原文必须进 content（可读）。
	if es[0].Content != "502 status code (no body)" {
		t.Errorf("content = %q，期望报错原文", es[0].Content)
	}
	// 合成行不能污染「模型回复」。
	if rs := eventsOfKind(evs, model.KindLLMResponse); len(rs) != 0 {
		t.Errorf("api_error 合成行不应产出 llm.response，得到 %d 条", len(rs))
	}
}

// TestClaudeNonErrorSystemRowNotError 验证普通 system 行（如 hook 输出）不会被误判为错误。
func TestClaudeNonErrorSystemRowNotError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", dir)

	line := `{"type":"system","subtype":"hook_response","timestamp":"2026-10-05T04:41:47.644Z","content":"ok"}`
	evs := collectFileInto(t, "claude", dir, "s-1.jsonl", line+"\n")
	if es := eventsOfKind(evs, model.KindError); len(es) != 0 {
		t.Errorf("非 api_error 的 system 行不该产出 error，得到 %d 条", len(es))
	}
}

// TestPiStopReasonErrorIsError 验证 pi 的 stopReason=error（实测 196 条，content 为空数组）
// 此前被静默跳过——时间线上这一轮凭空消失，现在采成 error 并带上 errorMessage。
func TestPiStopReasonErrorIsError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_SESSIONS_DIR", dir)

	line := `{"type":"message","id":"m1","timestamp":"2026-09-15T03:50:54.201Z","message":{"role":"assistant",` +
		`"content":[],"provider":"cc-switch-deep-seek","model":"deepseek-flash",` +
		`"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0},` +
		`"stopReason":"error","errorMessage":"terminated"}}`
	evs := collectFileInto(t, "pi", dir, "2026-09-15T03-42-12-464Z_01a0a328.jsonl", line+"\n")

	es := eventsOfKind(evs, model.KindError)
	if len(es) != 1 {
		t.Fatalf("应采到 1 条 error，得到 %d（全部 kind=%v）", len(es), kindsOf(evs))
	}
	if es[0].Content != "terminated" {
		t.Errorf("content = %q，期望 terminated", es[0].Content)
	}
	if m := metaOf(t, es[0]); m["type"] != "stop_reason_error" || m["model"] != "deepseek-flash" {
		t.Errorf("meta 不完整：%v", m)
	}
}

// TestPiNormalStopReasonNotError 验证正常结束的 assistant 消息不会产出 error。
func TestPiNormalStopReasonNotError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_SESSIONS_DIR", dir)

	line := `{"type":"message","id":"m1","timestamp":"2026-09-15T03:50:54.201Z","message":{"role":"assistant",` +
		`"content":[{"type":"text","text":"PONG"}],"stopReason":"stop"}}`
	evs := collectFileInto(t, "pi", dir, "2026-09-15T03-42-12-464Z_01a0a328.jsonl", line+"\n")
	if es := eventsOfKind(evs, model.KindError); len(es) != 0 {
		t.Errorf("stopReason=stop 不该产出 error，得到 %d 条", len(es))
	}
	if rs := eventsOfKind(evs, model.KindLLMResponse); len(rs) != 1 {
		t.Errorf("正常回复应保留，得到 %d 条 llm.response", len(rs))
	}
}

// TestPiToolResultIsErrorFlagged 验证 pi 的 toolResult.isError=true 被标进 meta
// （实测 988 条，多为命令退出码非 0）——否则失败的调用与成功的长得一样。
func TestPiToolResultIsErrorFlagged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_SESSIONS_DIR", dir)

	failed := `{"type":"message","id":"m1","timestamp":"2026-09-14T02:49:05.728Z","message":{"role":"toolResult",` +
		`"toolCallId":"call_1","toolName":"bash","content":[{"type":"text","text":"Command exited with code 1"}],` +
		`"isError":true}}`
	ok := `{"type":"message","id":"m2","timestamp":"2026-09-14T02:49:06.728Z","message":{"role":"toolResult",` +
		`"toolCallId":"call_2","toolName":"bash","content":[{"type":"text","text":"fine"}],"isError":false}}`
	evs := collectFileInto(t, "pi", dir, "2026-09-14T02-48-23-416Z_01a09dd0.jsonl", failed+"\n"+ok+"\n")

	rs := eventsOfKind(evs, model.KindToolResult)
	if len(rs) != 2 {
		t.Fatalf("应采到 2 条 tool.result，得到 %d", len(rs))
	}
	byCall := map[string]model.Event{}
	for _, e := range rs {
		byCall[e.ParentID] = e
	}
	if m := metaOf(t, byCall["call_1"]); m["is_error"] != true {
		t.Errorf("失败的工具结果应标 is_error=true，得到 %v", m)
	}
	if m := metaOf(t, byCall["call_2"]); m["is_error"] != nil {
		t.Errorf("成功的工具结果不该带 is_error，得到 %v", m)
	}
}

// TestPiErrorTitleStaysShort 验证 pi 的报错标题不会把整段 JSON/HTML 报错体塞进标题
// （实测 errorMessage 常是整段响应体，原样当标题会把列表撑爆），全文仍完整在 content。
func TestPiErrorTitleStaysShort(t *testing.T) {
	cases := []struct{ msg, wantTitle, wantContent string }{
		{"terminated", "模型调用失败：terminated", "terminated"},
		{`429: {"code":"1310","message":"您已达到每周/每月使用上限"}`, "模型调用失败：429", `429: {"code":"1310","message":"您已达到每周/每月使用上限"}`},
		{"502 <html>\n<head><title>502 Bad Gateway</title></head>", "模型调用失败：502", "502 <html>\n<head><title>502 Bad Gateway</title></head>"},
		{"", "模型调用失败", ""},
	}
	for _, c := range cases {
		rec := piRecord{Message: &piMessage{StopReason: "error", ErrorMessage: c.msg, Model: "m"}}
		e := (&piRun{}).assistantErrorEvent(rec, "s-1", time.Time{})
		if e == nil {
			t.Fatalf("msg=%q 应产出 error 事件", c.msg)
		}
		if e.Title != c.wantTitle {
			t.Errorf("msg=%q title = %q，期望 %q", c.msg, e.Title, c.wantTitle)
		}
		if e.Content != c.wantContent {
			t.Errorf("msg=%q content 应保留全文，得到 %q", c.msg, e.Content)
		}
		if len([]rune(e.Title)) > 20 {
			t.Errorf("标题过长（%d 字）：%q", len([]rune(e.Title)), e.Title)
		}
	}
}

// TestHermesToolResultFailureSignals 验证 hermes 工具结果的失败信号被读出：
// exit_code 非 0、非空 error 字段；正常结果与不可解析正文都不标记。
func TestHermesToolResultFailureSignals(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
		wantKey string
	}{
		{"非零退出码", `{"output":"boom","exit_code":1,"error":null}`, true, "exit_code"},
		{"退出码 0", `{"output":"ok","exit_code":0,"error":null}`, false, ""},
		{"非空 error 字段", `{"output":"","error":"permission denied"}`, true, "error"},
		{"空 error 字段", `{"output":"ok","error":""}`, false, ""},
		{"非 JSON 正文", `plain text output`, false, ""},
		{"无信号 JSON", `{"task":{"id":"t_1"}}`, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := hermesToolResultMeta("terminal", "call_1", c.content)
			got, _ := m["is_error"].(bool)
			if got != c.wantErr {
				t.Fatalf("is_error = %v，期望 %v（meta=%v）", m["is_error"], c.wantErr, m)
			}
			if c.wantKey != "" {
				if _, ok := m[c.wantKey]; !ok {
					t.Errorf("meta 缺 %s：%v", c.wantKey, m)
				}
			}
			if m["tool_name"] != "terminal" || m["tool_call_id"] != "call_1" {
				t.Errorf("原有字段丢失：%v", m)
			}
		})
	}
}
