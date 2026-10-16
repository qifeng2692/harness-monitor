// Package store 是 harness-monitor 的 SQLite 事件库（data/hm.db）。
//
// 采用纯 Go 的 modernc.org/sqlite（免 CGO）。表结构见 docs/DESIGN.md。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"

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
CREATE TABLE IF NOT EXISTS ingest_state (
	source  TEXT    NOT NULL,
	file    TEXT    NOT NULL,
	offset  INTEGER NOT NULL DEFAULT 0,
	lines   INTEGER NOT NULL DEFAULT 0,
	size    INTEGER NOT NULL DEFAULT 0,
	mtime   TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (source, file)
);
CREATE TABLE IF NOT EXISTS runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	prompt     TEXT NOT NULL,
	harnesses  TEXT NOT NULL,
	created_at TEXT NOT NULL,
	status     TEXT NOT NULL DEFAULT 'running'
);
CREATE TABLE IF NOT EXISTS run_harness (
	run_id      INTEGER NOT NULL,
	harness     TEXT NOT NULL,
	status      TEXT NOT NULL DEFAULT 'pending',
	started_at  TEXT NOT NULL DEFAULT '',
	ended_at    TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	tokens_in   INTEGER NOT NULL DEFAULT 0,
	tokens_out  INTEGER NOT NULL DEFAULT 0,
	cost_usd    REAL    NOT NULL DEFAULT 0,
	final_text  TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	session_id  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (run_id, harness)
);
CREATE TABLE IF NOT EXISTS run_events (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id   INTEGER NOT NULL,
	harness  TEXT NOT NULL,
	seq      INTEGER NOT NULL,
	time     TEXT NOT NULL,
	kind     TEXT NOT NULL,
	title    TEXT NOT NULL DEFAULT '',
	content  TEXT NOT NULL DEFAULT '',
	meta     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_run_events_rhs ON run_events(run_id, harness, seq);
-- 会话置顶：用户标记，与采集数据（events）完全隔离——清库重采不影响置顶。
CREATE TABLE IF NOT EXISTS session_pins (
	harness    TEXT NOT NULL,
	session_id TEXT NOT NULL,
	pinned_at  TEXT NOT NULL,
	PRIMARY KEY (harness, session_id)
);
`

// Store 包装一个 SQLite 连接。
type Store struct {
	db   *sql.DB
	Path string
}

// DataDir 返回数据目录，解析顺序：
//  1. HM_DATA_DIR 环境变量（若设置）；
//  2. exe 所在目录旁的 data/（若其中已有 hm.db——双击 exe 场景的锚点）；
//  3. 从 exe 所在目录向上逐级查找含 data/hm.db 的目录（GUI 在 build/bin/ 深层目录时命中仓库根）；
//  4. ./data（开发场景：cwd 即仓库根）。
func DataDir() string {
	if d := os.Getenv("HM_DATA_DIR"); d != "" {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), "data")
		if _, err := os.Stat(filepath.Join(dir, "hm.db")); err == nil {
			return dir
		}
		// 向上查找（最多 4 级）：gui/build/bin/ → gui/build → gui → 仓库根
		up := filepath.Dir(exe)
		for i := 0; i < 4; i++ {
			up = filepath.Dir(up)
			if up == filepath.Dir(up) { // 到达盘符根
				break
			}
			dir := filepath.Join(up, "data")
			if _, err := os.Stat(filepath.Join(dir, "hm.db")); err == nil {
				return dir
			}
		}
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
	// busy_timeout：多进程并发访问时等待锁而不是立刻报错（双击可能多开实例）。
	// journal_mode(WAL)：读写并发不互斥。
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
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

// ListSessions 返回某 harness 下的会话聚合列表：置顶会话排最前（按置顶时间倒序），
// 其余按最近活动倒序。
func (s *Store) ListSessions(harness string) ([]model.Session, error) {
	rows, err := s.db.Query(`
		SELECT e.session_id,
		       COUNT(*)          AS event_count,
		       MIN(e.time)       AS first_time,
		       MAX(e.time)       AS last_time,
		       COALESCE((SELECT title FROM events t
		                  WHERE t.harness = e.harness AND t.session_id = e.session_id
		                    AND t.title <> ''
		                  ORDER BY t.seq ASC LIMIT 1), '') AS title,
		       COALESCE(p.session_id <> '', 0) AS pinned,
		       COALESCE(p.pinned_at, '')       AS pinned_at
		  FROM events e
		  LEFT JOIN session_pins p
		    ON p.harness = e.harness AND p.session_id = e.session_id
		 WHERE e.harness = ?
		 GROUP BY e.session_id
		 ORDER BY pinned DESC, pinned_at DESC, MAX(e.time) DESC`, harness)
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
			pinned      int
			pinnedAt    sql.NullString
		)
		if err := rows.Scan(&sess.SessionID, &sess.EventCount, &first, &last, &title, &pinned, &pinnedAt); err != nil {
			return nil, err
		}
		sess.FirstTime = parseTime(first)
		sess.LastTime = parseTime(last)
		sess.Title = title.String
		sess.Pinned = pinned != 0
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// SetSessionPinned 置顶 / 取消置顶一个会话。重复置顶/取消均幂等。
func (s *Store) SetSessionPinned(harness, sessionID string, pinned bool) error {
	if pinned {
		_, err := s.db.Exec(`
			INSERT INTO session_pins (harness, session_id, pinned_at)
			VALUES (?, ?, ?)
			ON CONFLICT(harness, session_id) DO UPDATE SET pinned_at = excluded.pinned_at`,
			harness, sessionID, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}
	_, err := s.db.Exec(`DELETE FROM session_pins WHERE harness = ? AND session_id = ?`, harness, sessionID)
	return err
}

// SessionPinned 返回会话当前是否被置顶。
func (s *Store) SessionPinned(harness, sessionID string) (bool, error) {
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM session_pins WHERE harness = ? AND session_id = ?`, harness, sessionID,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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

// Harnesses 返回库中出现过的 harness 列表及其汇总（TUI 首屏用）。
func (s *Store) Harnesses() ([]HarnessSummary, error) {
	rows, err := s.db.Query(`
		SELECT harness, COUNT(*), COUNT(DISTINCT session_id), MAX(time)
		  FROM events GROUP BY harness ORDER BY harness`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HarnessSummary{}
	for rows.Next() {
		var (
			h    HarnessSummary
			last string
		)
		if err := rows.Scan(&h.Harness, &h.EventCount, &h.SessionCount, &last); err != nil {
			return nil, err
		}
		h.LastTime = parseTime(last)
		out = append(out, h)
	}
	return out, rows.Err()
}

// HarnessSummary 是 TUI 首屏的 harness 汇总。
type HarnessSummary struct {
	Harness      string
	EventCount   int64
	SessionCount int64
	LastTime     time.Time // 最近活动时间（会话内 MAX(time)）
}

// ---- 增量采集水位（ingest_state） -----------------------------------------

// HasSession 报告某 harness 下是否已有该会话的事件。
// 增量模式用它判断“新文件是否已入库”：已入库则从文件末尾起跟（跳过历史），
// 未入库则从头补历史，避免漏采。
func (s *Store) HasSession(harness, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM events WHERE harness = ? AND session_id = ? LIMIT 1`,
		harness, sessionID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// IngestStates 返回某 source（= harness 名）下所有文件的已消费位置。
func (s *Store) IngestStates(source string) (map[string]model.IngestPos, error) {
	rows, err := s.db.Query(
		`SELECT file, offset, lines, size, mtime FROM ingest_state WHERE source = ?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]model.IngestPos{}
	for rows.Next() {
		var (
			file  string
			pos   model.IngestPos
			mtime string
		)
		if err := rows.Scan(&file, &pos.Offset, &pos.Lines, &pos.Size, &mtime); err != nil {
			return nil, err
		}
		pos.MTime = parseTime(mtime)
		out[file] = pos
	}
	return out, rows.Err()
}

// SaveIngestStates 幂等写入（UPSERT）一批文件的已消费位置。
func (s *Store) SaveIngestStates(source string, states map[string]model.IngestPos) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO ingest_state (source, file, offset, lines, size, mtime)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, file) DO UPDATE SET
			offset = excluded.offset, lines = excluded.lines,
			size = excluded.size, mtime = excluded.mtime`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for file, pos := range states {
		if _, err := stmt.Exec(source, file, pos.Offset, pos.Lines, pos.Size, formatTime(pos.MTime)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- 全文搜索 -------------------------------------------------------------

// SearchHit 是一条搜索命中（含会话定位信息）。
type SearchHit struct {
	Harness   string
	SessionID string
	Seq       int64
	Time      time.Time
	Kind      string
	Title     string
	Content   string // 命中事件的内容原文（列表展示时再截断）
}

// SearchEvents 对 events 表的 content/title 做子串匹配（LIKE，大小写不敏感）。
//
// harness 为空表示全库搜索，否则限定该 harness。limit<=0 时用默认上限。
func (s *Store) SearchEvents(harness, query string, limit int) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	// 转义 LIKE 通配符，让用户输入按字面匹配（大小写不敏感：SQLite LIKE 对 ASCII 本就如此）。
	pat := "%" + escapeLike(query) + "%"

	sqlText := `SELECT harness, session_id, seq, time, kind, title, content
	              FROM events
	             WHERE (content LIKE ? ESCAPE '\' OR title LIKE ? ESCAPE '\')`
	args := []any{pat, pat}
	if harness != "" {
		sqlText += ` AND harness = ?`
		args = append(args, harness)
	}
	sqlText += ` ORDER BY harness, session_id, seq LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hits := []SearchHit{}
	for rows.Next() {
		var (
			h  SearchHit
			ts string
		)
		if err := rows.Scan(&h.Harness, &h.SessionID, &h.Seq, &ts, &h.Kind, &h.Title, &h.Content); err != nil {
			return nil, err
		}
		h.Time = parseTime(ts)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// defaultSearchLimit 是搜索命中上限（避免超大结果集灌爆 TUI）。
const defaultSearchLimit = 500

// escapeLike 转义 LIKE 模式中的 \ % _，配合 ESCAPE '\' 使用。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
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
