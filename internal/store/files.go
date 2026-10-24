// files.go 是「会话改动了哪些文件」的取数层：把某会话的全部工具调用取出来，
// 交给 internal/filechange 判定与归组。
//
// 为什么取全部工具调用、而不是在 SQL 里按工具名过滤：写文件工具名每家不同
// （claude 的 Edit/Write、hermes 的 patch/write_file、pi 的 edit/write），
// 把这张表复制到 SQL 里会变成两处口径。这里只负责取数，判定表只有一份。
//
// 性能：走 idx_events_hss(harness, session_id, seq)，单会话毫秒级（实测本机
// 500MB 库上最大的 hermes 会话约 1.1 万条工具调用，查询 < 50ms）。
package store

import "time"

// ToolCallRow 是一条工具调用（取数层形状，供 internal/filechange 用）。
type ToolCallRow struct {
	Seq  int64
	Time time.Time
	Tool string // tool.call 的 title 就是工具名
	Args string // tool.call 的 content 是参数原文（JSON）
}

// SessionToolCalls 返回某会话的全部工具调用（按 seq 升序）。
//
// 只取判定需要的四列，不取 meta / parent_id：文件改动判定只看工具名与参数。
func (s *Store) SessionToolCalls(harness, sessionID string) ([]ToolCallRow, error) {
	rows, err := s.db.Query(`
		SELECT seq, time, title, content
		  FROM events
		 WHERE harness = ? AND session_id = ? AND kind = 'tool.call'
		 ORDER BY seq ASC`, harness, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ToolCallRow{}
	for rows.Next() {
		var (
			r  ToolCallRow
			ts string
		)
		if err := rows.Scan(&r.Seq, &ts, &r.Tool, &r.Args); err != nil {
			return nil, err
		}
		r.Time = parseTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}
