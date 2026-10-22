// loops.go 是工具调用环的取数层：把「调用」与「结果」两类原始行从库里取出来，
// 交给 internal/loops 归组成重复调用组。
//
// 为什么不在 SQL 里做 JOIN：本机库实测 500MB / 66 万条工具调用，
// `tool.call LEFT JOIN tool.result ON parent_id = json_extract(meta,'$.call_id')`
// 跑 180 秒没跑完（无索引的 JSON 提取 + 字符串 JOIN）。改成「两次平表查询 + Go 里
// 归组」后各只要 ~1 秒。
//
// 口径纪律（与 internal/loops、internal/stats 一致）：
//   - 调用侧键取 meta.call_id（实测四家 100% 都有：hermes/codex/claude/pi）；
//   - 结果侧键取 parent_id（实测四家 100% 都有）；
//   - 失败判定只看 meta.is_error：hermes 成功也写 false（判得了），pi 只在失败时
//     写 true（判得了 = 失败数），codex/claude 不带该字段（判不了，不算成功）。
package store

import (
	"database/sql"
)

// LoopCall 是一条工具调用（取数层形状，供 internal/loops 用）。
type LoopCall struct {
	Harness   string
	SessionID string
	Tool      string
	Args      string
	Key       string // meta.call_id
}

// LoopResult 是一条工具结果（取数层形状，供 internal/loops 用）。
type LoopResult struct {
	Harness   string
	SessionID string
	Key       string // parent_id
	Failed    bool
	Judged    bool
}

// StatsLoopCalls 返回全部工具调用（harness 为空表示不限）。
//
// 只取需要的列，不取正文全文之外的额外字段；调用参数原文（content）要取——
// 「同一组参数」是判重的前提。
func (s *Store) StatsLoopCalls(harness string) ([]LoopCall, error) {
	sqlText := `
		SELECT harness, session_id, title, content,
		       COALESCE(json_extract(meta, '$.call_id'), '') AS call_id
		  FROM events
		 WHERE kind = 'tool.call'`
	args := []any{}
	if harness != "" {
		sqlText += ` AND harness = ?`
		args = append(args, harness)
	}
	sqlText += ` ORDER BY harness, session_id, seq`

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LoopCall{}
	for rows.Next() {
		var c LoopCall
		if err := rows.Scan(&c.Harness, &c.SessionID, &c.Tool, &c.Args, &c.Key); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StatsLoopResults 返回全部工具结果（harness 为空表示不限）。
//
// Failed/Judged 的口径：
//   - Judged：meta 带 is_error 键（该结果「判得了」失败与否）；
//   - Failed：meta.is_error = 1；
//   - meta 为空/非法 JSON → 判不了（Judged=false, Failed=false）。
func (s *Store) StatsLoopResults(harness string) ([]LoopResult, error) {
	sqlText := `
		SELECT harness, session_id, parent_id,
		       CASE WHEN json_valid(meta)
		            THEN json_extract(meta, '$.is_error') END AS is_error
		  FROM events
		 WHERE kind = 'tool.result'
		   AND parent_id <> ''`
	args := []any{}
	if harness != "" {
		sqlText += ` AND harness = ?`
		args = append(args, harness)
	}
	sqlText += ` ORDER BY harness, session_id, seq`

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LoopResult{}
	for rows.Next() {
		var (
			r     LoopResult
			isErr sql.NullInt64
		)
		if err := rows.Scan(&r.Harness, &r.SessionID, &r.Key, &isErr); err != nil {
			return nil, err
		}
		if isErr.Valid {
			r.Judged = true
			r.Failed = isErr.Int64 != 0
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
