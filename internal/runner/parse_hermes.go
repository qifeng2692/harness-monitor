// hermes（stream-json）输出解析器。映射表见 docs/DESIGN-runner.md 第 4 节。
//
// 源事件 → 统一 kind：
//
//	{"type":"system","subtype":"init",...}         → session.start（meta: model/session_id）
//	{"type":"tool_use","name","input"}             → tool.call（content=input 美化 JSON）
//	{"type":"tool_result","name","output",...}     → tool.result（content=output）
//	{"type":"text","text"}                         → 文本增量（"P"/"ONG" 分片）→ 累积，
//	                                                 遇非 text 事件或进程结束时冲刷为 llm.response
//	{"type":"result",...}                          → llm.usage + final_text=text；标记完成
//
// 协议不含 reasoning（hermes stream-json 源码注释明确）——v1 如实记录，不臆造。
// 非 JSON 行（如尾部的 "session_id: ..."）一律忽略。
package runner

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/qf/harness-monitor/internal/model"
)

// hermesRec 是 hermes stream-json 的一行。
type hermesRec struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	Model      string          `json:"model"`
	SessionID  string          `json:"session_id"`
	Timestamp  int64           `json:"timestamp"` // epoch 毫秒
	Text       string          `json:"text"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input"`
	Output     string          `json:"output"`
	DurationMs int64           `json:"duration_ms"`
	IsError    *bool           `json:"is_error"`
	Error      string          `json:"error"`
	ExitCode   *int            `json:"exit_code"`
	Tokens     *hermesTokens   `json:"tokens"`
}

type hermesTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Total      int64 `json:"total"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// ParseHermesLine 解析 hermes stream-json 的一行，返回 0..N 个统一事件。
func ParseHermesLine(line []byte, st *ParseState) []model.Event {
	if len(bytes.TrimSpace(line)) == 0 || bytes.TrimSpace(line)[0] != '{' {
		return nil // 非 JSON 行（尾部 session_id 摘要等）忽略
	}
	var rec hermesRec
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}
	ts := msTime(rec.Timestamp)

	switch rec.Type {
	case "system":
		if rec.Subtype != "init" {
			return nil
		}
		if rec.SessionID != "" {
			st.SessionID = rec.SessionID
		}
		return []model.Event{{
			SessionID: rec.SessionID,
			Time:      ts,
			Kind:      model.KindSessionStart,
			Title:     firstNonEmpty(rec.Model, "hermes session"),
			Meta: metaJSON(map[string]any{
				"model":      rec.Model,
				"session_id": rec.SessionID,
			}),
		}}

	case "text":
		st.pending.WriteString(rec.Text)
		return nil

	case "tool_use":
		evs := st.flushText(ts)
		return append(evs, model.Event{
			SessionID: st.SessionID,
			Time:      ts,
			Kind:      model.KindToolCall,
			Title:     firstNonEmpty(rec.Name, "tool"),
			Content:   indentJSON(rec.Input),
			Meta:      metaJSON(map[string]any{"tool_name": rec.Name}),
		})

	case "tool_result":
		evs := st.flushText(ts)
		return append(evs, model.Event{
			SessionID: st.SessionID,
			Time:      ts,
			Kind:      model.KindToolResult,
			Title:     firstNonEmpty(rec.Name, "tool result"),
			Content:   rec.Output,
			Meta: metaJSON(map[string]any{
				"tool_name":   rec.Name,
				"duration_ms": rec.DurationMs,
				"is_error":    rec.IsError,
			}),
		})

	case "result":
		if rec.SessionID != "" {
			st.SessionID = rec.SessionID
		}
		evs := st.flushText(ts)
		switch {
		case rec.Text != "":
			st.FinalText = rec.Text
		case len(evs) > 0:
			st.FinalText = evs[0].Content
		}
		if rec.Tokens != nil {
			st.TokensIn = rec.Tokens.Input
			st.TokensOut = rec.Tokens.Output
		}
		evs = append(evs, model.Event{
			SessionID: st.SessionID,
			Time:      ts,
			Kind:      model.KindLLMUsage,
			Title:     "usage",
			Content:   usageContent(st.TokensIn, st.TokensOut, 0),
			Meta: metaJSON(map[string]any{
				"tokens_in":   st.TokensIn,
				"tokens_out":  st.TokensOut,
				"duration_ms": rec.DurationMs,
				"exit_code":   rec.ExitCode,
			}),
		})
		st.Done = true
		// 网关瞬时故障（502）时 hermes 在 result 里带 error 字段——如实记为失败。
		if msg := strings.TrimSpace(rec.Error); msg != "" {
			st.ErrText = msg
			st.Fatal = true
		} else if rec.ExitCode != nil && *rec.ExitCode != 0 {
			st.ErrText = "hermes 退出码 " + itoa(*rec.ExitCode)
			st.Fatal = true
		}
		return evs
	}
	return nil
}

// FlushHermes 在进程结束时冲刷累积的文本分片（超时/中途退出时保留已产出的回答）。
func FlushHermes(st *ParseState) []model.Event {
	return st.flushText(time.Time{})
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
