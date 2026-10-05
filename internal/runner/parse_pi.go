// pi（pi -p --mode json）输出解析器。映射表见 docs/DESIGN-runner.md 第 4 节。
//
// 源事件 → 统一 kind：
//
//	session {id,cwd}                                     → session.start
//	tool_execution_start {toolCallId,toolName,args}      → tool.call
//	tool_execution_end {…,result:{structuredContent:{output},content},isError} → tool.result
//	turn_end {message:{content:[thinking|text|toolCall],usage,model}} → thinking→llm.reasoning；
//	                                          text→llm.response；usage→llm.usage；
//	                                          toolCall 跳过（已由 tool_execution 覆盖，避免重复）
//	agent_settled                                        → session.end
//	message_start/message_update/message_end             → 忽略（流式增量太细；turn_end 有全量）
//
// final_text = 最后一个 turn_end 的 text 块（无 text 块时保留上一个非空值）。
// 非 JSON 行（启动时的 "Warning: Extension package ..."）忽略。
package runner

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/qf/harness-monitor/internal/model"
)

// piRec 是 pi --mode json 的一行。
type piRec struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Cwd       string          `json:"cwd"`
	Model     string          `json:"model"`
	ToolName  string          `json:"toolName"`
	ToolID    string          `json:"toolCallId"`
	Args      json.RawMessage `json:"args"`
	IsError   *bool           `json:"isError"`
	Result    *piResult       `json:"result"`
	Message   *piMessage      `json:"message"`
	SessionID string          `json:"sessionId"`
}

type piResult struct {
	Content           json.RawMessage `json:"content"`
	StructuredContent *piStructured   `json:"structuredContent"`
}

type piStructured struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	ExitCode  *int   `json:"exit_code"`
}

type piMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"` // 块数组
	Usage   *piUsage        `json:"usage"`
}

type piUsage struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Cost       *piCost `json:"cost"`
}

type piCost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Total  float64 `json:"total"`
}

type piContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Args     json.RawMessage `json:"args"`
}

// ParsePiLine 解析 pi --mode json 的一行，返回 0..N 个统一事件。
func ParsePiLine(line []byte, st *ParseState) []model.Event {
	if len(bytes.TrimSpace(line)) == 0 || bytes.TrimSpace(line)[0] != '{' {
		return nil
	}
	var rec piRec
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}

	switch rec.Type {
	case "session":
		if rec.ID != "" {
			st.SessionID = rec.ID
		}
		return []model.Event{{
			SessionID: rec.ID,
			Kind:      model.KindSessionStart,
			Title:     piCwdTitle(rec.Cwd),
			Meta:      metaJSON(map[string]any{"cwd": rec.Cwd, "session_id": rec.ID}),
		}}

	case "tool_execution_start":
		return []model.Event{{
			SessionID: st.SessionID,
			Kind:      model.KindToolCall,
			Title:     firstNonEmpty(rec.ToolName, "tool"),
			Content:   indentJSON(rec.Args),
			ParentID:  rec.ToolID,
			Meta:      metaJSON(map[string]any{"tool_name": rec.ToolName, "call_id": rec.ToolID}),
		}}

	case "tool_execution_end":
		return []model.Event{{
			SessionID: st.SessionID,
			Kind:      model.KindToolResult,
			Title:     firstNonEmpty(rec.ToolName, "tool result"),
			Content:   piToolResultText(rec.Result),
			ParentID:  rec.ToolID,
			Meta: metaJSON(map[string]any{
				"tool_name": rec.ToolName,
				"call_id":   rec.ToolID,
				"is_error":  rec.IsError,
			}),
		}}

	case "turn_end":
		if rec.Message == nil {
			return nil
		}
		return piTurnEnd(rec.Message, st)

	case "agent_settled":
		// pi 的协议层完成事件（agent 已停止、不会再产出）→ 标记 Done，
		// 否则一次成功运行会被「未收到完成事件」的兜底判成 failed。
		st.Done = true
		return []model.Event{{
			SessionID: st.SessionID,
			Kind:      model.KindSessionEnd,
			Title:     "agent settled",
		}}
	}
	// agent_start / turn_start / message_start / message_update / message_end / agent_end：
	// 不产出事件（流式增量太细，全量在 turn_end）。
	return nil
}

// piTurnEnd 处理 turn_end：内容块 + usage，并把最后一个 text 块记为 final_text。
func piTurnEnd(msg *piMessage, st *ParseState) []model.Event {
	var out []model.Event

	trimmed := bytes.TrimSpace(msg.Content)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var blocks []piContentBlock
		if json.Unmarshal(trimmed, &blocks) == nil {
			for _, b := range blocks {
				switch b.Type {
				case "thinking":
					if b.Thinking == "" {
						continue
					}
					out = append(out, model.Event{
						SessionID: st.SessionID,
						Kind:      model.KindLLMReasoning,
						Title:     "reasoning",
						Content:   b.Thinking,
					})
				case "text":
					if b.Text == "" {
						continue
					}
					st.FinalText = b.Text
					out = append(out, model.Event{
						SessionID: st.SessionID,
						Kind:      model.KindLLMResponse,
						Title:     "assistant",
						Content:   b.Text,
					})
				case "toolCall":
					// 已由 tool_execution_start/end 覆盖，跳过避免重复。
				}
			}
		}
	}

	if msg.Usage != nil {
		st.TokensIn = msg.Usage.Input
		st.TokensOut = msg.Usage.Output
		if msg.Usage.Cost != nil {
			st.CostUSD = msg.Usage.Cost.Total
		}
		out = append(out, model.Event{
			SessionID: st.SessionID,
			Kind:      model.KindLLMUsage,
			Title:     "usage",
			Content:   usageContent(st.TokensIn, st.TokensOut, st.CostUSD),
			Meta: metaJSON(map[string]any{
				"tokens_in":  st.TokensIn,
				"tokens_out": st.TokensOut,
				"cost_usd":   st.CostUSD,
				"model":      msg.Model,
			}),
		})
	}
	return out
}

// piToolResultText 取工具结果文本：优先 structuredContent.output（本机 bash 工具
// 的 stdout 原文），退回 content 块数组拼接。
func piToolResultText(res *piResult) string {
	if res == nil {
		return ""
	}
	if res.StructuredContent != nil && res.StructuredContent.Output != "" {
		return res.StructuredContent.Output
	}
	return piContentText(res.Content)
}

// piContentText 拼接 content 块数组里的 text 块（字符串形态直接返回）。
func piContentText(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(t, &s) == nil {
			return s
		}
		return string(t)
	}
	if t[0] == '[' {
		var blocks []piContentBlock
		if json.Unmarshal(t, &blocks) == nil {
			var sb strings.Builder
			for _, b := range blocks {
				if b.Type == "text" {
					sb.WriteString(b.Text)
				}
			}
			return sb.String()
		}
	}
	return string(t)
}

// piCwdTitle 取 cwd 最后一段作为会话标题（兼容 / 与 \ 分隔）。
func piCwdTitle(cwd string) string {
	s := strings.ReplaceAll(cwd, "\\", "/")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "pi session"
	}
	return s
}
