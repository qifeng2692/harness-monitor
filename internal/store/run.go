// 对比实验台（Compare Lab）存储层：runs / run_harness / run_events 三张表的读写。
//
// 规格见 docs/DESIGN-runner.md 第 3 节。这些表与主监控库（events / ingest_state）
// 同库不同域：不参与 harness→会话→时间线导航，仅服务于「一次 prompt 多家 harness 并跑对比」。
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// defaultRunListLimit 是 ListRuns 在 limit<=0 时的默认返回条数。
const defaultRunListLimit = 50

// 批次级状态（runs.status）。
const (
	RunStatusRunning = "running"
	RunStatusDone    = "done"
)

// harness 级状态（run_harness.status）。
const (
	RunHarnessPending = "pending"
	RunHarnessRunning = "running"
	RunHarnessDone    = "done"
	RunHarnessFailed  = "failed"
	RunHarnessTimeout = "timeout"
	RunHarnessStopped = "stopped"
)

// terminalRunHarnessStatus 报告某 harness 状态是否为终态（不再变化）。
func terminalRunHarnessStatus(status string) bool {
	switch status {
	case RunHarnessDone, RunHarnessFailed, RunHarnessTimeout, RunHarnessStopped:
		return true
	}
	return false
}

// RunSummary 是一个对比批次的摘要（ListRuns / GetRun 的批次部分）。
type RunSummary struct {
	ID        int64
	Prompt    string
	Harnesses []string // 本批次参与的 harness（保持创建时的顺序）
	CreatedAt time.Time
	Status    string // running | done
}

// RunHarness 是批次内某一家 harness 的运行记录（run_harness 一行）。
type RunHarness struct {
	RunID      int64
	Harness    string
	Status     string // pending|running|done|failed|timeout|stopped
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMs int64
	TokensIn   int64
	TokensOut  int64
	CostUSD    float64
	FinalText  string
	Error      string
	SessionID  string // 运行产生的会话 id（可跳主监控视图）
}

// RunDetail 是批次详情：批次摘要（runs 行）+ 各 harness 运行行（按 harness 名升序）。
type RunDetail struct {
	Summary     RunSummary
	HarnessRuns []RunHarness
}

// CreateRun 新建一个对比批次：写 runs 行（status='running'）并为每个 harness
// 预建 run_harness 行（status='pending'），返回批次 id。
//
// harnesses 以 JSON 数组存于 runs.harnesses，保持传入顺序。
func (s *Store) CreateRun(prompt string, harnesses []string) (int64, error) {
	if len(harnesses) == 0 {
		return 0, fmt.Errorf("CreateRun: harnesses 不能为空")
	}
	hs, err := json.Marshal(harnesses)
	if err != nil {
		return 0, fmt.Errorf("CreateRun: 序列化 harnesses: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO runs (prompt, harnesses, created_at, status) VALUES (?, ?, ?, ?)`,
		prompt, string(hs), formatTime(time.Now()), RunStatusRunning)
	if err != nil {
		return 0, err
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO run_harness (run_id, harness, status) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, h := range harnesses {
		if _, err := stmt.Exec(runID, h, RunHarnessPending); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return runID, nil
}

// FinishRun 把批次标记为已完成（status='done'）。重复调用幂等。
func (s *Store) FinishRun(runID int64) error {
	_, err := s.db.Exec(`UPDATE runs SET status = ? WHERE id = ?`, RunStatusDone, runID)
	return err
}

// ListRuns 返回最近 limit 个批次的摘要，按 id 倒序（最新在前）。limit<=0 用默认值。
func (s *Store) ListRuns(limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = defaultRunListLimit
	}
	rows, err := s.db.Query(
		`SELECT id, prompt, harnesses, created_at, status FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RunSummary{}
	for rows.Next() {
		var (
			r       RunSummary
			hs      string
			created string
		)
		if err := rows.Scan(&r.ID, &r.Prompt, &hs, &created, &r.Status); err != nil {
			return nil, err
		}
		r.Harnesses = decodeHarnesses(hs)
		r.CreatedAt = parseTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRun 返回批次详情：runs 行 + 各 harness 行（按 harness 名升序）。
func (s *Store) GetRun(runID int64) (RunDetail, error) {
	var (
		d       RunDetail
		hs      string
		created string
	)
	err := s.db.QueryRow(
		`SELECT id, prompt, harnesses, created_at, status FROM runs WHERE id = ?`, runID).
		Scan(&d.Summary.ID, &d.Summary.Prompt, &hs, &created, &d.Summary.Status)
	if err == sql.ErrNoRows {
		return d, fmt.Errorf("run %d 不存在", runID)
	}
	if err != nil {
		return d, err
	}
	d.Summary.Harnesses = decodeHarnesses(hs)
	d.Summary.CreatedAt = parseTime(created)

	rows, err := s.db.Query(`
		SELECT run_id, harness, status, started_at, ended_at, duration_ms,
		       tokens_in, tokens_out, cost_usd, final_text, error, session_id
		  FROM run_harness WHERE run_id = ? ORDER BY harness ASC`, runID)
	if err != nil {
		return d, err
	}
	defer rows.Close()

	d.HarnessRuns = []RunHarness{}
	for rows.Next() {
		var (
			h              RunHarness
			started, ended string
		)
		if err := rows.Scan(&h.RunID, &h.Harness, &h.Status, &started, &ended,
			&h.DurationMs, &h.TokensIn, &h.TokensOut, &h.CostUSD,
			&h.FinalText, &h.Error, &h.SessionID); err != nil {
			return d, err
		}
		h.StartedAt = parseTime(started)
		h.EndedAt = parseTime(ended)
		d.HarnessRuns = append(d.HarnessRuns, h)
	}
	return d, rows.Err()
}

// SetRunHarnessStatus 设置某 harness 在批次中的状态与错误文本（UPSERT，幂等）。
//
// 时间戳约定：首次进入 running 记 started_at；进入终态（done/failed/timeout/stopped）
// 记 ended_at。重复调用不覆盖已写入的时间（保留首次值）。
func (s *Store) SetRunHarnessStatus(runID int64, harness, status, errMsg string) error {
	now := formatTime(time.Now())
	_, err := s.db.Exec(`
		INSERT INTO run_harness (run_id, harness, status, error, started_at, ended_at)
		VALUES (?, ?, ?, ?,
		        CASE WHEN ? = 'running' THEN ? ELSE '' END,
		        CASE WHEN ? IN ('done','failed','timeout','stopped') THEN ? ELSE '' END)
		ON CONFLICT(run_id, harness) DO UPDATE SET
			status     = excluded.status,
			error      = excluded.error,
			started_at = CASE WHEN run_harness.started_at = '' AND excluded.started_at <> ''
			                  THEN excluded.started_at ELSE run_harness.started_at END,
			ended_at   = CASE WHEN run_harness.ended_at = '' AND excluded.ended_at <> ''
			                  THEN excluded.ended_at ELSE run_harness.ended_at END`,
		runID, harness, status, errMsg, status, now, status, now)
	return err
}

// SetRunHarnessResult 写入某 harness 的运行结果（最终回答 / 会话 id / token / 费用 / 耗时）。
//
// 只写结果列，不改状态——状态由 SetRunHarnessStatus 单独驱动，避免「有结果即 done」
// 掩盖退出码非 0 的失败。UPSERT，重复调用幂等。
func (s *Store) SetRunHarnessResult(runID int64, harness, finalText, sessionID string,
	tokensIn, tokensOut int64, costUSD float64, durationMs int64) error {
	_, err := s.db.Exec(`
		INSERT INTO run_harness (run_id, harness, final_text, session_id, tokens_in, tokens_out, cost_usd, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, harness) DO UPDATE SET
			final_text  = excluded.final_text,
			session_id  = excluded.session_id,
			tokens_in   = excluded.tokens_in,
			tokens_out  = excluded.tokens_out,
			cost_usd    = excluded.cost_usd,
			duration_ms = excluded.duration_ms`,
		runID, harness, finalText, sessionID, tokensIn, tokensOut, costUSD, durationMs)
	return err
}

// InsertRunEvents 把一批事件写入某批次某 harness 的事件流。
//
// seq 复用 InsertEvents 的 nextSeq 模式：Event.Seq<=0 时按 (run_id, harness) 内当前
// 最大 seq 递增分配；显式 seq 原样使用（并把游标推进到该值之后）。Time 为零值时取当前时间。
func (s *Store) InsertRunEvents(runID int64, harness string, events []model.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO run_events
		(run_id, harness, seq, time, kind, title, content, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	var next int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(seq), 0) FROM run_events WHERE run_id = ? AND harness = ?`,
		runID, harness).Scan(&next); err != nil {
		return err
	}

	for _, e := range events {
		seq := e.Seq
		if seq <= 0 {
			next++
			seq = next
		} else if seq > next {
			next = seq
		}
		t := e.Time
		if t.IsZero() {
			t = time.Now()
		}
		if _, err := stmt.Exec(runID, harness, seq, formatTime(t),
			e.Kind, e.Title, e.Content, e.Meta); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListRunEvents 返回某批次某 harness 的全部事件，按 seq 升序。
func (s *Store) ListRunEvents(runID int64, harness string) ([]model.Event, error) {
	rows, err := s.db.Query(`
		SELECT id, seq, time, kind, title, content, meta
		  FROM run_events WHERE run_id = ? AND harness = ? ORDER BY seq ASC`,
		runID, harness)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []model.Event{}
	for rows.Next() {
		var (
			e  model.Event
			ts string
		)
		if err := rows.Scan(&e.ID, &e.Seq, &ts, &e.Kind, &e.Title, &e.Content, &e.Meta); err != nil {
			return nil, err
		}
		e.Harness = harness
		e.Time = parseTime(ts)
		events = append(events, e)
	}
	return events, rows.Err()
}

// decodeHarnesses 解析 runs.harnesses 的 JSON 数组；损坏时退化为单元素/空切片而非报错。
func decodeHarnesses(s string) []string {
	var hs []string
	if err := json.Unmarshal([]byte(s), &hs); err != nil {
		return []string{}
	}
	if hs == nil {
		return []string{}
	}
	return hs
}
