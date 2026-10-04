// Package model 定义 harness-monitor 的统一事件模型。
//
// 设计要点（见 docs/DESIGN.md）：
//   - Harness + SessionID 是一等维度：查询、列表、导航全部先按它们分组；
//   - Content 存内容原文，不做截断；
//   - token/耗时是 Meta 属性，不是事件的替代。
package model

import "time"

// 事件 kind 规范（所有采集器统一，见 docs/DESIGN.md）。
const (
	KindSessionStart = "session.start"
	KindSessionMeta  = "session.meta"
	KindTurnStart    = "turn.start"
	KindLLMResponse  = "llm.response"
	KindLLMReasoning = "llm.reasoning"
	KindToolCall     = "tool.call"
	KindToolResult   = "tool.result"
	KindLLMUsage     = "llm.usage"
	KindTurnEnd      = "turn.end"
	KindSessionEnd   = "session.end"
	KindCompaction   = "compaction"
	KindError        = "error"
)

// Event 是一条内容级事件。
type Event struct {
	ID        int64     // 自增主键（插入前为 0）
	Harness   string    // "hermes" | "codex" | "claude" | "pi" | "magpie"
	SessionID string    // 该 harness 内的会话 ID
	Seq       int64     // 会话内序号（升序）；0 表示由 store 自动分配
	Time      time.Time // 统一为 UTC；原始值保留在 Meta
	Kind      string    // 见上方 kind 常量
	Title     string    // 一行摘要（列表展示用）
	Content   string    // 内容原文（推理/参数/结果/输入；不截断）
	Meta      string    // JSON：token/耗时/状态等属性
	ParentID  string    // 关联父键（tool_call_id 等）
	SourceKey string    // 幂等去重键（UNIQUE）
}

// Session 是一个会话的惰性聚合结果（由 events 表按 harness+session_id 聚合）。
type Session struct {
	SessionID  string
	EventCount int64
	FirstTime  time.Time
	LastTime   time.Time
	Title      string
}

// IngestPos 是某数据源已被消费到的位置，用于增量续读（live 跟随）。
//
// 对文件型数据源：Offset 是已消费的字节数（且恰好停在某个换行之后），
// Lines 是已消费的物理行数（换行符计数，含空行），用于复现与全量采集一致的
// source_key（行号）。Size/MTime 仅用于诊断（文件被截断/轮转时判定重启）。
//
// 对非文件型数据源（如 Hermes 的 SQLite state.db）只使用 Offset 作为水位线
// （例如 messages 表已消费的最大 id）。
type IngestPos struct {
	Offset int64
	Lines  int64
	Size   int64
	MTime  time.Time
}
