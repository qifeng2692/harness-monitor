// Package store 是 harness-monitor 的 SQLite 事件库（data/hm.db）。
//
// 采用纯 Go 的 modernc.org/sqlite（免 CGO）。表结构见 docs/DESIGN.md。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/qf/harness-monitor/internal/model"

	_ "modernc.org/sqlite"
)

// defaultDataDir 是 HM_DATA_DIR 未设置时的数据目录。
const defaultDataDir = "data"

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	harness    TEXT    NOT NULL,
	session_id TEXT    NOT NULL,
	seq        INTEGER NOT NULL,
	time       TEXT    NOT NULL,
	kind       TEXT    NOT NULL,
	title      TEXT    NOT NULL DEFAULT '',
	content    TEXT    NOT NULL DEFAULT '',
	meta       TEXT    NOT NULL DEFAULT '',
	parent_id  TEXT    NOT NULL DEFAULT '',
	source_key TEXT    UNIQUE
);
CREATE INDEX IF NOT EXISTS idx_events_hss ON events(harness, session_id, seq);
`

// Store 包装一个 SQLite 连接。
type Store struct {
	db   *sql.DB
	Path string
}

// DataDir 返回数据目录：HM_DATA_DIR 环境变量优先，否则 ./data。
func DataDir() string {
	if d := os.Getenv("HM_DATA_DIR"); d != "" {
		return d
	}
	return defaultDataDir
}

// Open 打开（必要时创建）数据目录下的 hm.db 并建表。
func Open() (*Store, error) {
	dir := DataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s: %w", dir, err)
	}
	path := filepath.Join(dir, "hm.db")
	return OpenPath(path)
}

// OpenPath 打开指定路径的数据库（测试/覆盖用）。
func OpenPath(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s: %w", path, err)
	}
	// modernc sqlite 单写者：限制连接数避免 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表: %w", err)
	}
	return &Store{db: db, Path: path}, nil
}

// Close 关闭底层连接。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接（TUI/调试用）。
func (s *Store) DB() *sql.DB { return s.db }

// InsertEvents 幂等写入事件（INSERT OR IGNORE）。
//
// 返回实际插入数与被忽略（重复）数。Seq<=0 的事件按会话内当前最大 seq 递增分配，
// 保证同一批按输入顺序得到稳定序号；重复执行不产生新行，seq 也保持不变。
func (s *Store) InsertEvents(events []model.Event) (inserted, skipped int, err error) {
	if len(events) == 0 {
		return 0, 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO events
		(harness, session_id, seq, time, kind, title, content, meta, parent_id, source_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, 0, err
	}
	defer stmt.Close()

	// 每个会话的下一序号，惰性从库中读取当前最大值。
	nextSeq := map[string]int64{}
	for _, e := range events {
		seq := e.Seq
		if seq <= 0 {
			key := e.Harness + "\x00" + e.SessionID
			n, ok := nextSeq[key]
			if !ok {
				if err := tx.QueryRow(
					`SELECT COALESCE(MAX(seq), 0) FROM events WHERE harness = ? AND session_id = ?`,
					e.Harness, e.SessionID).Scan(&n); err != nil {
					return inserted, skipped, err
				}
			}
			n++
			nextSeq[key] = n
			seq = n
		}
		res, err := stmt.Exec(
			e.Harness, e.SessionID, seq, formatTime(e.Time), e.Kind,
			e.Title, e.Content, e.Meta, e.ParentID, e.SourceKey,
		)
		if err != nil {
			return inserted, skipped, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		} else {
			skipped++
		}
	}
	if err := tx.Commit(); err != nil {
		return inserted, skipped, err
	}
	return inserted, skipped, nil
}

// ListSessions 返回某 harness 下的会话聚合列表，按最近活动倒序。
func (s *Store) ListSessions(harness string) ([]model.Session, error) {
	rows, err := s.db.Query(`
		SELECT session_id,
		       COUNT(*)          AS event_count,
		       MIN(time)         AS first_time,
		       MAX(time)         AS last_time,
		       COALESCE((SELECT title FROM events t
		                  WHERE t.harness = e.harness AND t.session_id = e.session_id
		                    AND t.title <> ''
		                  ORDER BY t.seq ASC LIMIT 1), '') AS title
		  FROM events e
		 WHERE harness = ?
		 GROUP BY session_id
		 ORDER BY MAX(time) DESC`, harness)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := []model.Session{}
	for rows.Next() {
		var (
			sess        model.Session
			first, last string
			title       sql.NullString
		)
		if err := rows.Scan(&sess.SessionID, &sess.EventCount, &first, &last, &title); err != nil {
			return nil, err
		}
		sess.FirstTime = parseTime(first)
		sess.LastTime = parseTime(last)
		sess.Title = title.String
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// ListEvents 返回某会话的全部事件，按 seq 升序。
func (s *Store) ListEvents(harness, sessionID string) ([]model.Event, error) {
	rows, err := s.db.Query(`
		SELECT id, harness, session_id, seq, time, kind, title, content, meta, parent_id, source_key
		  FROM events
		 WHERE harness = ? AND session_id = ?
		 ORDER BY seq ASC`, harness, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []model.Event{}
	for rows.Next() {
		var (
			e   model.Event
			ts  string
			src sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.Harness, &e.SessionID, &e.Seq, &ts,
			&e.Kind, &e.Title, &e.Content, &e.Meta, &e.ParentID, &src); err != nil {
			return nil, err
		}
		e.Time = parseTime(ts)
		e.SourceKey = src.String
		events = append(events, e)
	}
	return events, rows.Err()
}

// Harnesses 返回库中出现过的 harness 列表及其事件数（TUI 汇总用）。
func (s *Store) Harnesses() ([]HarnessSummary, error) {
	rows, err := s.db.Query(`
		SELECT harness, COUNT(*), COUNT(DISTINCT session_id)
		  FROM events GROUP BY harness ORDER BY harness`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HarnessSummary{}
	for rows.Next() {
		var h HarnessSummary
		if err := rows.Scan(&h.Harness, &h.EventCount, &h.SessionCount); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HarnessSummary 是 TUI 首屏的 harness 汇总。
type HarnessSummary struct {
	Harness      string
	EventCount   int64
	SessionCount int64
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
