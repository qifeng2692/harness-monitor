// stats.go 是统计视图的取数层：把「规模 / 用量 / 工具 / 失败」四类原始数据
// 从库里按形状取出来，交给 internal/stats 汇总成报告。
//
// 为什么单独一层：各 harness 的失败信号所在位置完全不同（hermes 的 exit_code 由
// 采集器写进 meta.is_error、pi 的 isError 同理、codex/claude 的工具结果不带任何
// 失败信号），把「哪家的信号在哪」集中在这一个文件里，才不会出现同一个数字在
// 两处不一样。
//
// 取数原则：**结构化信号优先，判定口径写死在 SQL 里**——失败与否只看各家自己
// 上报的字段，不做正文猜测（猜错会把正常输出说成失败）。
package store

import "time"

// HarnessStats 是某 harness 的规模统计（一行一家）。
type HarnessStats struct {
	Harness     string
	Sessions    int64
	Events      int64
	ToolCalls   int64
	ToolResults int64
	ErrorEvents int64
}

// ToolStat 是某 harness 某个工具的调用与失败统计。
//
// Calls 是 tool.call 事件的计数（各家 tool.call 的 title 就是工具名：
// hermes 的 terminal/patch、pi 的 bash、codex 的 exec_command、claude 的 Bash）。
//
// Failed / Judged 是工具结果的失败判定：
//
//	hermes：采集器已把 exit_code 非 0 / error 非空写进 meta.is_error；
//	pi    ：采集器已把 toolResult.isError=true 写进 meta.is_error；
//	codex、claude：工具结果不带任何失败信号字段，因此不参与判定。
//
// Judged 是「meta 里带 is_error 键」的行数——只有这些行「判得了」。
// pi 只在失败时才写该键，所以 pi 的 Judged 等于 Failed（成功与判不了不可分）；
// hermes 成功时也写 is_error=false，故 hermes 的 Judged 覆盖成功+失败两类。
type ToolStat struct {
	Harness string
	Tool    string
	Calls   int64
	Failed  int64
	Judged  int64
}

// ErrorStat 是失败原因的聚合（error 事件按 harness/标题归并）。
type ErrorStat struct {
	Harness string
	Title   string
	Count   int64
	Time    time.Time // 该组最近一次出现的时间
}

// UsageRow 是一条用量事件（只取元数据，不取正文——正文可达数百字节 × 三万条）。
type UsageRow struct {
	Harness   string
	SessionID string
	Title     string
	Meta      string
	Time      time.Time
}

// StatsHarnessStats 返回各 harness 的规模统计。
func (s *Store) StatsHarnessStats() ([]HarnessStats, error) {
	rows, err := s.db.Query(`
		SELECT harness,
		       COUNT(DISTINCT session_id) AS sessions,
		       COUNT(*)                  AS events,
		       SUM(CASE WHEN kind = 'tool.call'   THEN 1 ELSE 0 END) AS tool_calls,
		       SUM(CASE WHEN kind = 'tool.result' THEN 1 ELSE 0 END) AS tool_results,
		       SUM(CASE WHEN kind = 'error'       THEN 1 ELSE 0 END) AS error_events
		  FROM events
		 GROUP BY harness
		 ORDER BY harness`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HarnessStats{}
	for rows.Next() {
		var h HarnessStats
		if err := rows.Scan(&h.Harness, &h.Sessions, &h.Events,
			&h.ToolCalls, &h.ToolResults, &h.ErrorEvents); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// StatsUsageRows 返回全部用量事件（按 harness/会话/时间升序）。
func (s *Store) StatsUsageRows() ([]UsageRow, error) {
	rows, err := s.db.Query(`
		SELECT harness, session_id, time, title, meta
		  FROM events
		 WHERE kind = 'llm.usage'
		 ORDER BY harness, session_id, seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UsageRow{}
	for rows.Next() {
		var (
			r  UsageRow
			ts string
		)
		if err := rows.Scan(&r.Harness, &r.SessionID, &ts, &r.Title, &r.Meta); err != nil {
			return nil, err
		}
		r.Time = parseTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// StatsToolStats 返回各 harness 各工具的调用数与失败判定结果。
//
// 失败判定口径（各家失败信号的位置不同，全部取自各家自己上报的结构化字段，
// 详见 ToolStat 的注释）：
//
//	hermes：采集器把 exit_code 非 0 / error 非空写进 meta.is_error；
//	pi    ：采集器把 toolResult.isError=true 写进 meta.is_error；
//	其余  ：工具结果不带退出码或错误标记（codex/claude），因此只统计调用数，
//	        不出现在失败判定里——「判不了」不计入「没失败」。
func (s *Store) StatsToolStats() ([]ToolStat, error) {
	// 调用数：tool.call 的 title 就是工具名。
	callRows, err := s.db.Query(`
		SELECT harness, title, COUNT(*)
		  FROM events
		 WHERE kind = 'tool.call'
		 GROUP BY harness, title`)
	if err != nil {
		return nil, err
	}
	defer callRows.Close()

	out := []ToolStat{}
	idx := map[string]int{}
	for callRows.Next() {
		var t ToolStat
		if err := callRows.Scan(&t.Harness, &t.Tool, &t.Calls); err != nil {
			return nil, err
		}
		idx[t.Harness+"\x00"+t.Tool] = len(out)
		out = append(out, t)
	}
	if err := callRows.Err(); err != nil {
		return nil, err
	}

	// 失败判定：只看 meta 里带 is_error 键的行（结构化信号，不猜正文）。
	// 工具名优先取 meta.tool_name（hermes/pi 都写），取不到才回落到 title。
	resRows, err := s.db.Query(`
		SELECT harness,
		       COALESCE(json_extract(meta, '$.tool_name'), title) AS tool,
		       SUM(CASE WHEN json_extract(meta, '$.is_error') = 1 THEN 1 ELSE 0 END) AS failed,
		       COUNT(*) AS judged
		  FROM events
		 WHERE kind = 'tool.result'
		   AND json_valid(meta)
		   AND json_extract(meta, '$.is_error') IS NOT NULL
		 GROUP BY harness, tool`)
	if err != nil {
		return nil, err
	}
	defer resRows.Close()

	for resRows.Next() {
		var (
			h, tool string
			failed  int64
			judged  int64
		)
		if err := resRows.Scan(&h, &tool, &failed, &judged); err != nil {
			return nil, err
		}
		key := h + "\x00" + tool
		if i, ok := idx[key]; ok {
			out[i].Failed = failed
			out[i].Judged = judged
			continue
		}
		// 有失败记录但没有对应调用事件（调用早于采集起点）：照样列出，不丢。
		out = append(out, ToolStat{Harness: h, Tool: tool, Failed: failed, Judged: judged})
	}
	return out, resRows.Err()
}

// StatsErrorStats 返回失败原因的聚合（error 事件按 harness/标题归并，按次数降序）。
func (s *Store) StatsErrorStats() ([]ErrorStat, error) {
	rows, err := s.db.Query(`
		SELECT harness, title, COUNT(*), MAX(time)
		  FROM events
		 WHERE kind = 'error'
		 GROUP BY harness, title
		 ORDER BY COUNT(*) DESC, harness, title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ErrorStat{}
	for rows.Next() {
		var (
			e  ErrorStat
			ts string
		)
		if err := rows.Scan(&e.Harness, &e.Title, &e.Count, &ts); err != nil {
			return nil, err
		}
		e.Time = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
