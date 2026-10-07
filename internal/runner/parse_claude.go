// claude（claude -p --output-format stream-json --verbose）输出解析器。
// 映射表见 docs/DESIGN-runner.md 第 4 节。
//
// 源事件 → 统一 kind：
//
//	system/init {session_id,model,tools}                → session.start
//	assistant message.content[]: thinking               → llm.reasoning
//	                              text                   → llm.response
//	                              tool_use {name,input}  → tool.call
//	user message.content[]: tool_result {content,is_error} → tool.result
//	result {result,duration_ms,total_cost_usd,usage}    → llm.usage + final_text=result + Done
//
// 非 JSON 行（"[claude-code:unrecognized_model] {...}"）忽略；system/thinking_tokens
// 是流式估算噪声，忽略（与 pi 的 message_update 同理——全量在 assistant/result 里）。
package runner

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// claudeRec 是 claude stream-json 的一行。
type claudeRec struct {
	Type      string       `json:"type"`
	Subtype   string       `json:"subtype"`
	SessionID string       `json:"session_id"`
	Model     string       `json:"model"`
	Message   *claudeMsg   `json:"message"`
	Result    string       `json:"result"`
	IsError   *bool        `json:"is_error"`
	Duration  int64        `json:"duration_ms"`
	CostUSD   float64      `json:"total_cost_usd"`
	Usage     *claudeUsage `json:"usage"`
	Error     string       `json:"error"`
	Timestamp string       `json:"timestamp"`
	UUID      string       `json:"uuid"`
	Tools     []string     `json:"tools"`
}

type claudeMsg struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"` // 块数组
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result：字符串或块数组
	IsError   *bool           `json:"is_error"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// ParseClaudeLine 解析 claude stream-json 的一行，返回 0..N 个统一事件。
func ParseClaudeLine(line []byte, st *ParseState) []model.Event {
	if len(bytes.TrimSpace(line)) == 0 || bytes.TrimSpace(line)[0] != '{' {
		return nil
	}
	var rec claudeRec
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}
	ts := rfcTime(rec.Timestamp)
	if rec.SessionID != "" {
		st.SessionID = rec.SessionID
	}

	switch rec.Type {
	case "system":
		if rec.Subtype != "init" {
			return nil // thinking_tokens 等流式噪声
		}
		return []model.Event{{
			SessionID: rec.SessionID,
			Time:      ts,
			Kind:      model.KindSessionStart,
			Title:     firstNonEmpty(rec.Model, "claude session"),
			Meta: metaJSON(map[string]any{
				"model":      rec.Model,
				"session_id": rec.SessionID,
				"tools":      len(rec.Tools),
			}),
		}}

	case "assistant":
		if rec.Message == nil {
			return nil
		}
		return claudeBlocks(rec.Message.Content, st, ts, model.KindLLMResponse)

	case "user":
		if rec.Message == nil {
			return nil
		}
		return claudeBlocks(rec.Message.Content, st, ts, "")

	case "result":
		evs := []model.Event{}
		if rec.Usage != nil {
			st.TokensIn = rec.Usage.InputTokens
			st.TokensOut = rec.Usage.OutputTokens
		}
		st.CostUSD = rec.CostUSD
		st.FinalText = rec.Result
		st.Done = true
		evs = append(evs, model.Event{
			SessionID: st.SessionID,
			Time:      ts,
			Kind:      model.KindLLMUsage,
			Title:     "usage",
			Content:   usageContent(st.TokensIn, st.TokensOut, st.CostUSD),
			Meta: metaJSON(map[string]any{
				"tokens_in":   st.TokensIn,
				"tokens_out":  st.TokensOut,
				"cost_usd":    st.CostUSD,
				"duration_ms": rec.Duration,
				"subtype":     rec.Subtype,
			}),
		})
		if rec.IsError != nil && *rec.IsError {
			st.ErrText = firstNonEmpty(rec.Error, rec.Result, "claude 报告错误")
			st.Fatal = true
			evs = append(evs, model.Event{
				SessionID: st.SessionID,
				Time:      ts,
				Kind:      model.KindError,
				Title:     "error",
				Content:   st.ErrText,
			})
		}
		return evs
	}
	return nil
}

// claudeBlocks 解析 message.content 块数组。
//
// fallbackKind 用于「text 块」的默认映射（assistant 消息 → llm.response）；
// user 消息的 tool_result 块有专属映射，不受 fallbackKind 影响。
func claudeBlocks(raw json.RawMessage, st *ParseState, ts time.Time, fallbackKind string) []model.Event {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || trimmed[0] != '[' {
		return nil
	}
	var blocks []claudeBlock
	if json.Unmarshal(trimmed, &blocks) != nil {
		return nil
	}

	var out []model.Event
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			if b.Thinking == "" {
				continue
			}
			out = append(out, model.Event{
				SessionID: st.SessionID, Time: ts,
				Kind: model.KindLLMReasoning, Title: "reasoning", Content: b.Thinking,
			})

		case "text":
			if b.Text == "" || fallbackKind == "" {
				continue
			}
			out = append(out, model.Event{
				SessionID: st.SessionID, Time: ts,
				Kind: fallbackKind, Title: "assistant", Content: b.Text,
			})

		case "tool_use":
			out = append(out, model.Event{
				SessionID: st.SessionID, Time: ts,
				Kind: model.KindToolCall, Title: firstNonEmpty(b.Name, "tool"),
				Content:  indentJSON(b.Input),
				ParentID: b.ID,
				Meta:     metaJSON(map[string]any{"tool_name": b.Name, "call_id": b.ID}),
			})

		case "tool_result":
			content := claudeToolResultText(b.Content)
			out = append(out, model.Event{
				SessionID: st.SessionID, Time: ts,
				Kind: model.KindToolResult, Title: "tool result",
				Content:  content,
				ParentID: b.ToolUseID,
				Meta:     metaJSON(map[string]any{"call_id": b.ToolUseID, "is_error": b.IsError}),
			})
		}
	}
	return out
}

// claudeToolResultText 取 tool_result 的 content（字符串或 [{type:text,text}] 块数组）。
func claudeToolResultText(raw json.RawMessage) string {
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
		var blocks []claudeBlock
		if json.Unmarshal(t, &blocks) == nil {
			var sb bytes.Buffer
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
