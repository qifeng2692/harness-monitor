// hermes 采集器：只读 Hermes state.db（SQLite）→ 内容级事件。
//
// 数据源格式见 docs/DATA-FORMATS.md §1。库路径 %LOCALAPPDATA%\hermes\state.db，
// 以 file:...?mode=ro 只读打开（Hermes 运行中、WAL 模式，并发读安全；绝不写入）。
//
// 事件映射（一一对应 docs/DESIGN.md 的 kind 规范）：
//
//	sessions                                    → session.start（每行 1 个）
//	messages.role='user'                        → turn.start
//	messages.role='assistant'.content           → llm.response
//	messages.role='assistant'.reasoning_content → llm.reasoning
//	messages.role='assistant'.tool_calls[i]     → tool.call（每个 call 一个）
//	messages.role='tool'                        → tool.result
//	session_model_usage                         → llm.usage（按 session_id+model 聚合）
//
// 幂等键（UNIQUE source_key）以 "hermes:"+messages.id 为基，因一条消息行可产出
// 多个事件（正文 + 推理 + 多个工具调用），故按事件类型追加后缀以保证唯一：
//
//	turn.start / tool.result → hermes:<msgid>
//	llm.response             → hermes:<msgid>:response
//	llm.reasoning            → hermes:<msgid>:reasoning
//	tool.call                → hermes:<msgid>:call:<call_id>
//	session.start            → hermes:session:<session_id>
//	llm.usage                → hermes:usage:<session_id>:<model>
package collect

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"

	_ "modernc.org/sqlite"
)

const (
	hermesHarness   = "hermes"
	hermesBatchSize = 5000 // 分批入库，避免一次性持有全部事件内容
)

type hermesCollector struct{}

func init() { Register(hermesCollector{}) }

func (hermesCollector) Name() string { return hermesHarness }

// HermesStateDBPath 返回 state.db 路径：HERMES_STATE_DB 优先（测试/覆盖用），
// 否则 %LOCALAPPDATA%\hermes\state.db。
func HermesStateDBPath() (string, error) {
	if p := os.Getenv("HERMES_STATE_DB"); p != "" {
		return p, nil
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return filepath.Join(la, "hermes", "state.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("解析用户目录: %w", err)
	}
	return filepath.Join(home, "AppData", "Local", "hermes", "state.db"), nil
}

// Collect 只读扫描 state.db 并幂等写入事件。
func (hermesCollector) Collect(sink Sink) (Stats, error) {
	return hermesCollect(sink, 0)
}

// CollectIncremental 实现增量续读：只处理 messages.id 大于水位线的新消息。
//
// sessions / session_model_usage 仍每轮重扫（数量小且幂等 UPSERT），保证新会话的
// session.start 与用量变化都能跟上；messages 是内容大头，按 id 水位线增量。
func (hermesCollector) CollectIncremental(sink TailSink) (int, bool, error) {
	states, err := sink.IngestStates(hermesHarness)
	if err != nil {
		return 0, false, err
	}
	var since int64
	if p, ok := states[hermesWatermarkKey]; ok {
		since = p.Offset
	}
	stats, err := hermesCollect(sink, since)
	return stats.Inserted, true, err
}

// hermesWatermarkKey 是 ingest_state 中保存 messages 已消费最大 id 的键（非文件）。
const hermesWatermarkKey = "messages"

// hermesCollect 是 Collect 与 CollectIncremental 的共用实现。
// since==0 时全量处理 messages；否则只处理 id > since 的消息。
func hermesCollect(sink Sink, since int64) (Stats, error) {
	stats := Stats{Harness: hermesHarness}

	path, err := HermesStateDBPath()
	if err != nil {
		return stats, err
	}
	if _, err := os.Stat(path); err != nil {
		return stats, fmt.Errorf("state.db 不可用 %s: %w", path, err)
	}
	db, err := openHermesDB(path)
	if err != nil {
		return stats, fmt.Errorf("只读打开 %s: %w", path, err)
	}
	defer db.Close()

	eb := &eventBuffer{sink: sink, size: hermesBatchSize}

	n, err := hermesSessions(db, eb)
	stats.Scanned += n
	if err != nil {
		return stats, err
	}
	m, bad, lastID, err := hermesMessages(db, eb, since)
	stats.Scanned += m
	stats.BadLines += bad
	if err != nil {
		return stats, err
	}
	u, err := hermesUsage(db, eb)
	stats.Scanned += u
	if err != nil {
		return stats, err
	}
	if err := eb.flush(); err != nil {
		return stats, err
	}
	stats.Inserted, stats.Skipped = eb.inserted, eb.skipped
	stats.Increment = since > 0

	// 增量模式下推进 messages 水位线（取本轮处理到的最大 id）。
	if ts, ok := sink.(TailSink); ok && lastID > since {
		if err := ts.SaveIngestStates(hermesHarness, map[string]model.IngestPos{
			hermesWatermarkKey: {Offset: lastID},
		}); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// ---- 打开（只读 + SQLITE_BUSY 重试） -------------------------------------

const (
	hermesBusyRetries = 3
	hermesBusyBackoff = 500 * time.Millisecond
)

// openHermesDB 以 file:<path>?mode=ro 只读打开 state.db；
// 遇 SQLITE_BUSY / database is locked 重试 3 次，间隔 500ms。
func openHermesDB(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro"
	var db *sql.DB
	err := withBusyRetry(func() error {
		conn, err := sql.Open("sqlite", dsn)
		if err != nil {
			return err
		}
		conn.SetMaxOpenConns(1)
		if err := conn.Ping(); err != nil {
			conn.Close()
			return err
		}
		db = conn
		return nil
	})
	if err != nil {
		return nil, err
	}
	return db, nil
}

// withBusyRetry 执行 fn，仅在遇 SQLITE_BUSY 类错误时重试（最多 hermesBusyRetries 次）。
func withBusyRetry(fn func() error) error {
	var err error
	for i := 0; i < hermesBusyRetries; i++ {
		if i > 0 {
			time.Sleep(hermesBusyBackoff)
		}
		err = fn()
		if err == nil || !isBusyError(err) {
			return err
		}
	}
	return err
}

func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToUpper(err.Error())
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "DATABASE IS LOCKED")
}

// ---- 分批写入 -------------------------------------------------------------

// eventBuffer 累积事件并在达到 size 时批量幂等写入，限制单次内存占用。
type eventBuffer struct {
	sink     Sink
	buf      []model.Event
	size     int
	inserted int
	skipped  int
}

func (b *eventBuffer) add(ev model.Event) error {
	b.buf = append(b.buf, ev)
	if len(b.buf) >= b.size {
		return b.flush()
	}
	return nil
}

func (b *eventBuffer) flush() error {
	if len(b.buf) == 0 {
		return nil
	}
	ins, skip, err := b.sink.InsertEvents(b.buf)
	b.inserted += ins
	b.skipped += skip
	b.buf = b.buf[:0]
	return err
}

// ---- sessions -------------------------------------------------------------

func hermesSessions(db *sql.DB, eb *eventBuffer) (scanned int, err error) {
	rows, err := db.Query(`
		SELECT id, source, model, cwd, git_branch,
		       message_count, tool_call_count, title, started_at
		  FROM sessions
		 ORDER BY started_at, id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id                                       string
			source, modelName, cwd, gitBranch, title sql.NullString
			msgCount, toolCount                      int64
			startedAt                                sql.NullFloat64
		)
		if err := rows.Scan(&id, &source, &modelName, &cwd, &gitBranch,
			&msgCount, &toolCount, &title, &startedAt); err != nil {
			return scanned, err
		}
		scanned++

		t := title.String
		if t == "" {
			t = modelName.String // 无标题时退回模型名
		}
		meta, err := json.Marshal(map[string]any{
			"source":          source.String,
			"model":           modelName.String,
			"cwd":             cwd.String,
			"git_branch":      gitBranch.String,
			"message_count":   msgCount,
			"tool_call_count": toolCount,
		})
		if err != nil {
			return scanned, err
		}
		if err := eb.add(model.Event{
			Harness:   hermesHarness,
			SessionID: id,
			Time:      unixFloat(startedAt),
			Kind:      model.KindSessionStart,
			Title:     t,
			Meta:      string(meta),
			SourceKey: hermesHarness + ":session:" + id,
		}); err != nil {
			return scanned, err
		}
	}
	return scanned, rows.Err()
}

// ---- messages -------------------------------------------------------------

func hermesMessages(db *sql.DB, eb *eventBuffer, since int64) (scanned, bad int, lastID int64, err error) {
	query := `
		SELECT id, session_id, role, content, tool_calls, tool_name,
		       tool_call_id, timestamp, reasoning_content
		  FROM messages`
	args := []any{}
	if since > 0 {
		query += ` WHERE id > ?`
		args = append(args, since)
	}
	query += ` ORDER BY id`
	rows, err := db.Query(query, args...)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id                                       int64
			sessionID, role                          string
			content, toolCalls, toolName, toolCallID sql.NullString
			reasoning                                sql.NullString
			ts                                       sql.NullFloat64
		)
		if err := rows.Scan(&id, &sessionID, &role, &content, &toolCalls,
			&toolName, &toolCallID, &ts, &reasoning); err != nil {
			return scanned, bad, lastID, err
		}
		scanned++
		if id > lastID {
			lastID = id
		}

		when := unixFloat(ts)
		base := hermesHarness + ":" + strconv.FormatInt(id, 10)

		switch role {
		case "user":
			if err := eb.add(model.Event{
				Harness:   hermesHarness,
				SessionID: sessionID,
				Time:      when,
				Kind:      model.KindTurnStart,
				Title:     "user",
				Content:   content.String,
				SourceKey: base,
			}); err != nil {
				return scanned, bad, lastID, err
			}

		case "assistant":
			if content.String != "" {
				if err := eb.add(model.Event{
					Harness:   hermesHarness,
					SessionID: sessionID,
					Time:      when,
					Kind:      model.KindLLMResponse,
					Title:     "assistant",
					Content:   content.String,
					SourceKey: base + ":response",
				}); err != nil {
					return scanned, bad, lastID, err
				}
			}
			if reasoning.String != "" {
				if err := eb.add(model.Event{
					Harness:   hermesHarness,
					SessionID: sessionID,
					Time:      when,
					Kind:      model.KindLLMReasoning,
					Title:     "reasoning",
					Content:   reasoning.String,
					SourceKey: base + ":reasoning",
				}); err != nil {
					return scanned, bad, lastID, err
				}
			}
			if toolCalls.String != "" {
				calls, ok := parseHermesToolCalls(toolCalls.String)
				if !ok {
					bad++
					break
				}
				for _, c := range calls {
					meta, err := json.Marshal(map[string]any{
						"tool_name": c.name,
						"call_id":   c.id,
					})
					if err != nil {
						return scanned, bad, lastID, err
					}
					if err := eb.add(model.Event{
						Harness:   hermesHarness,
						SessionID: sessionID,
						Time:      when,
						Kind:      model.KindToolCall,
						Title:     c.name,
						Content:   c.arguments,
						Meta:      string(meta),
						ParentID:  c.id,
						SourceKey: base + ":call:" + c.id,
					}); err != nil {
						return scanned, bad, lastID, err
					}
				}
			}

		case "tool":
			meta, err := json.Marshal(hermesToolResultMeta(toolName.String, toolCallID.String, content.String))
			if err != nil {
				return scanned, bad, lastID, err
			}
			if err := eb.add(model.Event{
				Harness:   hermesHarness,
				SessionID: sessionID,
				Time:      when,
				Kind:      model.KindToolResult,
				Title:     toolName.String,
				Content:   content.String,
				Meta:      string(meta),
				ParentID:  toolCallID.String,
				SourceKey: base,
			}); err != nil {
				return scanned, bad, lastID, err
			}

		default:
			// 其他 role（如 session_meta）无内容，跳过且不计坏行。
		}
	}
	return scanned, bad, lastID, rows.Err()
}

// hermesParsedCall 是从 messages.tool_calls 中解析出的单个调用。
type hermesParsedCall struct {
	id        string
	name      string
	arguments string // 参数原文（内层 JSON 文本）
}

type hermesToolCall struct {
	ID     string `json:"id"`
	CallID string `json:"call_id"`
	Func   struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// parseHermesToolCalls 解析 tool_calls JSON 数组；解析失败返回 ok=false（计入坏行）。
func parseHermesToolCalls(s string) (calls []hermesParsedCall, ok bool) {
	var raw []hermesToolCall
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, false
	}
	calls = make([]hermesParsedCall, 0, len(raw))
	for i, c := range raw {
		id := c.ID
		if id == "" {
			id = c.CallID
		}
		if id == "" { // 兜底，保证 source_key 唯一
			id = "idx" + strconv.Itoa(i)
		}
		calls = append(calls, hermesParsedCall{
			id:        id,
			name:      c.Func.Name,
			arguments: rawArgsToString(c.Func.Arguments),
		})
	}
	return calls, true
}

// rawArgsToString 把 function.arguments 还原为参数原文：通常是 JSON 编码的字符串
// （内层即参数 JSON 文本），若本身不是字符串则原样返回。
func rawArgsToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return string(raw)
}

// hermesToolResultMeta 构造 hermes 工具结果事件的 meta，并尽力从结果正文里读出
// 「这次调用是不是失败了」。
//
// 为什么要读：hermes 的 tool 行 content 是工具返回的 JSON（如
// {"output":"...","exit_code":1,"error":null}），失败信号全在里面，而 meta 此前只有
// 工具名与 call_id —— 一次失败的命令在时间线上与成功的长得一模一样。
//
// 实测本机 23021 条 tool 行：exit_code 非 0 的 862 条、带非空 error 字段的 496 条、
// 非 JSON 正文 1200 条。只认结构化的两个信号（exit_code 非 0 / error 非空），
// 不做文本猜测——猜错会把正常输出标成失败。
func hermesToolResultMeta(toolName, callID, content string) map[string]any {
	meta := map[string]any{
		"tool_name":    toolName,
		"tool_call_id": callID,
	}
	if code, ok := hermesExitCode(content); ok {
		meta["exit_code"] = code
		meta["is_error"] = code != 0
	} else if errText, ok := hermesErrorField(content); ok {
		meta["is_error"] = true
		meta["error"] = errText
	}
	return meta
}

// hermesExitCode 从工具结果的 JSON 正文里取 exit_code；没有该字段或正文不是 JSON 时
// 返回 ok=false（不是失败，只是没有可判定的信号）。
func hermesExitCode(content string) (int, bool) {
	obj, ok := hermesResultObject(content)
	if !ok {
		return 0, false
	}
	v, present := obj["exit_code"]
	if !present {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case string:
		code, err := strconv.Atoi(n)
		if err != nil {
			return 0, false
		}
		return code, true
	}
	return 0, false
}

// hermesErrorField 从工具结果的 JSON 正文里取非空的 error 字段文本。
func hermesErrorField(content string) (string, bool) {
	obj, ok := hermesResultObject(content)
	if !ok {
		return "", false
	}
	v, present := obj["error"]
	if !present || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return "", false
		}
		return t, true
	case bool:
		if !t {
			return "", false
		}
		return "true", true
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "", false
		}
		s := string(b)
		if s == "" || s == "{}" || s == "[]" {
			return "", false
		}
		return s, true
	}
}

// hermesResultObject 把工具结果正文解析成 JSON 对象；不是对象返回 ok=false。
func hermesResultObject(content string) (map[string]any, bool) {
	s := strings.TrimSpace(content)
	if s == "" || s[0] != '{' {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil || len(obj) == 0 {
		return nil, false
	}
	return obj, true
}

// ---- session_model_usage --------------------------------------------------

func hermesUsage(db *sql.DB, eb *eventBuffer) (scanned int, err error) {
	rows, err := db.Query(`
		SELECT session_id, model,
		       COALESCE(SUM(api_call_count), 0),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(reasoning_tokens), 0),
		       COALESCE(SUM(estimated_cost_usd), 0),
		       COALESCE(SUM(actual_cost_usd), 0),
		       MIN(first_seen)
		  FROM session_model_usage
		 GROUP BY session_id, model
		 ORDER BY session_id, model`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			sessionID, modelName                    string
			apiCalls, in, out, cacheRead, reasoning int64
			estCost, actCost                        float64
			firstSeen                               sql.NullFloat64
		)
		if err := rows.Scan(&sessionID, &modelName, &apiCalls, &in, &out,
			&cacheRead, &reasoning, &estCost, &actCost, &firstSeen); err != nil {
			return scanned, err
		}
		scanned++

		meta, err := json.Marshal(map[string]any{
			"api_call_count":     apiCalls,
			"input_tokens":       in,
			"output_tokens":      out,
			"cache_read_tokens":  cacheRead,
			"reasoning_tokens":   reasoning,
			"estimated_cost_usd": estCost,
			"actual_cost_usd":    actCost,
		})
		if err != nil {
			return scanned, err
		}
		if err := eb.add(model.Event{
			Harness:   hermesHarness,
			SessionID: sessionID,
			Time:      unixFloat(firstSeen),
			Kind:      model.KindLLMUsage,
			Title:     modelName,
			Meta:      string(meta),
			SourceKey: hermesHarness + ":usage:" + sessionID + ":" + modelName,
		}); err != nil {
			return scanned, err
		}
	}
	return scanned, rows.Err()
}

// ---- 时间工具 -------------------------------------------------------------

// unixFloat 把 Unix 秒浮点（SQLite REAL）转为 UTC time.Time；NULL 返回零时间。
func unixFloat(v sql.NullFloat64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return timeFromUnixFloat(v.Float64)
}

func timeFromUnixFloat(f float64) time.Time {
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	if nsec < 0 {
		nsec = 0
	}
	return time.Unix(sec, nsec).UTC()
}
