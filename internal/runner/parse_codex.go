// codex（codex exec --json）输出解析器。映射表见 docs/DESIGN-runner.md 第 4 节。
//
// 源事件 → 统一 kind：
//
//	thread.started {thread_id}                                  → session.start
//	item.started   item.type=command_execution {command}        → tool.call
//	item.completed item.type=command_execution {aggregated_output,exit_code} → tool.result
//	item.completed item.type=agent_message {text}               → llm.response
//	item.*         item.type=reasoning {text}                   → llm.reasoning
//	item.*         item.type=error {message}                    → error（记入 state.ErrText）
//	turn.completed {usage:{input_tokens,...}}                   → llm.usage + Done
//	turn.failed    {error}                                      → error + 标记失败
//
// 非 JSON 行（"Reading additional input from stdin..."）忽略。
package runner

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// codexRec 是 codex --json 的一行。
type codexRec struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id"`
	Item     *codexItem `json:"item"`
	Usage    *codexUse  `json:"usage"`
	Error    any        `json:"error"`
	Message  string     `json:"message"`
}

type codexItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Text             string `json:"text"`
	Message          string `json:"message"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Status           string `json:"status"`
	Reasoning        string `json:"reasoning"`
}

type codexUse struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// ParseCodexLine 解析 codex --json 的一行，返回 0..N 个统一事件。
func ParseCodexLine(line []byte, st *ParseState) []model.Event {
	if len(bytes.TrimSpace(line)) == 0 || bytes.TrimSpace(line)[0] != '{' {
		return nil
	}
	var rec codexRec
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}

	switch rec.Type {
	case "thread.started":
		st.SessionID = rec.ThreadID
		return []model.Event{{
			SessionID: rec.ThreadID,
			Kind:      model.KindSessionStart,
			Title:     "codex session",
			Meta:      metaJSON(map[string]any{"thread_id": rec.ThreadID}),
		}}

	case "turn.started":
		return []model.Event{{Kind: model.KindTurnStart, Title: "turn"}}

	case "item.started", "item.completed", "item.updated":
		if rec.Item == nil {
			return nil
		}
		return codexItemEvents(rec.Type, rec.Item, st)

	case "turn.completed":
		if rec.Usage != nil {
			st.TokensIn = rec.Usage.InputTokens
			st.TokensOut = rec.Usage.OutputTokens
		}
		st.Done = true
		return []model.Event{{
			SessionID: st.SessionID,
			Kind:      model.KindLLMUsage,
			Title:     "usage",
			Content:   usageContent(st.TokensIn, st.TokensOut, 0),
			Meta: metaJSON(map[string]any{
				"tokens_in":  st.TokensIn,
				"tokens_out": st.TokensOut,
			}),
		}}

	case "turn.failed":
		msg := firstNonEmpty(codexErrText(rec.Error), rec.Message, "turn.failed")
		st.ErrText = msg
		st.Fatal = true
		return []model.Event{{
			SessionID: st.SessionID,
			Kind:      model.KindError,
			Title:     "turn failed",
			Content:   msg,
		}}
	}
	return nil
}

// codexItemEvents 把 item.* 映射为事件（按 item.type 分派）。
func codexItemEvents(phase string, it *codexItem, st *ParseState) []model.Event {
	switch it.Type {
	case "command_execution":
		switch phase {
		case "item.started":
			return []model.Event{{
				SessionID: st.SessionID,
				Kind:      model.KindToolCall,
				Title:     "command",
				Content:   it.Command,
				ParentID:  it.ID,
				Meta:      metaJSON(map[string]any{"item_id": it.ID}),
			}}
		case "item.completed":
			return []model.Event{{
				SessionID: st.SessionID,
				Kind:      model.KindToolResult,
				Title:     "command result",
				Content:   it.AggregatedOutput,
				ParentID:  it.ID,
				Meta: metaJSON(map[string]any{
					"item_id":   it.ID,
					"exit_code": it.ExitCode,
				}),
			}}
		}
		return nil

	case "agent_message":
		if phase != "item.completed" {
			return nil
		}
		st.FinalText = it.Text
		return []model.Event{{
			SessionID: st.SessionID,
			Kind:      model.KindLLMResponse,
			Title:     "assistant",
			Content:   it.Text,
		}}

	case "reasoning":
		text := firstNonEmpty(it.Text, it.Reasoning)
		if phase != "item.completed" || text == "" {
			return nil
		}
		return []model.Event{{
			SessionID: st.SessionID,
			Kind:      model.KindLLMReasoning,
			Title:     "reasoning",
			Content:   text,
		}}

	case "error":
		if phase != "item.completed" {
			return nil
		}
		// 注意：codex 对「忽略未知配置项」这类告警也发 item.type=error。
		// 那是噪声不是失败——只记 ErrText 供展示，不置 Fatal（否则正常运行被误标失败）。
		st.ErrText = it.Message
		return []model.Event{{
			SessionID: st.SessionID,
			Kind:      model.KindError,
			Title:     "error",
			Content:   it.Message,
			Meta:      metaJSON(map[string]any{"item_id": it.ID}),
		}}
	}
	return nil
}

// codexErrText 把 turn.failed 的 error 字段（可能是对象或字符串）转为文本。
func codexErrText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any:
		if m, ok := t["message"].(string); ok {
			return m
		}
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return strings.Trim(string(b), `"`)
	}
}
