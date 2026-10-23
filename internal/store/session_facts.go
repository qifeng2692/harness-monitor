package store

import (
	"encoding/json"
	"strings"
	"time"
)

// SessionFact 是判一个会话「此刻处于什么状态」所需的原始事实（取数层形状）。
//
// 为什么需要：harness-monitor 此前只记录「发生了什么」，不记录「现在卡在哪」——
// 会话列表只有 id/标题/事件数/时间，用户看不出哪个会话正等着他回答、哪个刚失败。
// 同类工具（tmux-agent-sidebar 的状态列、MioIsland 的待批准提醒）都把「会话当前
// 状态」当核心信息。本包只负责把判定需要的事实取出来，判定规则在 internal/sessionstate。
//
// 取数原则（与 internal/stats、internal/loops 一致）：**只看结构化信号，不猜正文**。
// 「问了用户」= 存在一个「等人回答类」的工具调用；「用户答了」= 该调用有配对的结果
// 且结果里带回答内容；「用户没答」= 结果缺失或结果里没有回答内容。
type SessionFact struct {
	Harness   string
	SessionID string

	// LastKind/LastTitle/LastTime 是会话的最后一步（按 seq 取最大值）。
	LastKind  string
	LastTitle string
	LastTime  time.Time

	// LastUserTurnSeq 是最后一个「用户输入」步骤的 seq——用来判断提问之后用户
	// 是否又说过话（说过就说明 agent 已经往下走了，不算卡在提问上）。
	LastUserTurnSeq int64

	// ErrorCount 是该会话里 error 事件的条数。
	ErrorCount int64

	// Asks 是该会话里所有「等人回答类」的工具调用（按 seq 升序）。
	Asks []SessionAsk
}

// SessionAsk 是一次「问用户 / 要授权」的调用。
type SessionAsk struct {
	Harness   string
	SessionID string
	Tool      string // 工具名（各家的 tool.call 的 title 就是工具名）
	Seq       int64
	Time      time.Time

	// Content 是调用参数原文（各家把问题写在 content 里）。
	Content string

	// Key 是调用标识（meta.call_id，回落 parent_id）——用来配对结果。
	Key string

	// Answered 为真表示这次提问拿到了回答。
	Answered bool

	// Answer 是回答内容原文（各家形态不同：hermes 是 JSON 的 responses[].user_response，
	// pi 是 "User has answered your questions: ..."，codex 是 {"answers":{...}}）。
	Answer string
}

// SessionFacts 返回所有会话的状态判定事实。
//
// harness 为空表示不限。取数走「平表查询 + Go 里归组」而不是 SQL JOIN——
// 本机库实测 500MB / 23 万事件，跨表 JOIN + JSON 提取会跑到分钟级
// （见 internal/store/loops.go 的说明）。
func (s *Store) SessionFacts(harness string) ([]SessionFact, error) {
	facts, order, err := s.sessionBasics(harness)
	if err != nil {
		return nil, err
	}
	if err := s.fillUserTurns(facts, harness); err != nil {
		return nil, err
	}
	if err := s.fillErrorCounts(facts, harness); err != nil {
		return nil, err
	}
	if err := s.fillAsks(facts, harness); err != nil {
		return nil, err
	}

	out := make([]SessionFact, 0, len(order))
	for _, k := range order {
		out = append(out, *facts[k])
	}
	return out, nil
}

// sessionBasics 取每个会话的最后一步，并按出现顺序记下会话清单。
//
// 取「最后一步」用 group-by + join 而不是窗口函数：实测本机库（500MB / 23 万事件）
// 窗口函数 ROW_NUMBER() 要 8.3 秒，group-by MAX(seq) + join 只要 121 毫秒——
// 差 70 倍，而结果完全一样。
//
// 子查询里的 harness 过滤也要带上：GUI 每次切 harness 都要重算该家的状态，
// 过滤后 42ms，不过滤要 636ms。
func (s *Store) sessionBasics(harness string) (map[string]*SessionFact, []string, error) {
	sqlText := `
		SELECT e.harness, e.session_id, e.kind, e.title, e.time
		  FROM events e
		  JOIN (SELECT harness, session_id, MAX(seq) AS ms
		          FROM events`
	args := []any{}
	if harness != "" {
		sqlText += ` WHERE harness = ?`
		args = append(args, harness)
	}
	sqlText += `
		         GROUP BY harness, session_id) m
		    ON e.harness = m.harness AND e.session_id = m.session_id AND e.seq = m.ms`
	if harness != "" {
		sqlText += ` WHERE e.harness = ?`
		args = append(args, harness)
	}

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	facts := map[string]*SessionFact{}
	order := []string{}
	for rows.Next() {
		var (
			h, sid, kind, title, ts string
		)
		if err := rows.Scan(&h, &sid, &kind, &title, &ts); err != nil {
			return nil, nil, err
		}
		key := h + "\x00" + sid
		f, ok := facts[key]
		if !ok {
			f = &SessionFact{Harness: h, SessionID: sid}
			facts[key] = f
			order = append(order, key)
		}
		f.LastKind = kind
		f.LastTitle = title
		f.LastTime = parseTime(ts)
	}
	return facts, order, rows.Err()
}

// fillUserTurns 填每个会话最后一个「用户输入」的 seq。
func (s *Store) fillUserTurns(facts map[string]*SessionFact, harness string) error {
	sqlText := `
		SELECT harness, session_id, MAX(seq)
		  FROM events
		 WHERE kind = 'turn.start'`
	args := []any{}
	if harness != "" {
		sqlText += ` AND harness = ?`
		args = append(args, harness)
	}
	sqlText += ` GROUP BY harness, session_id`

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h, sid string
		var seq int64
		if err := rows.Scan(&h, &sid, &seq); err != nil {
			return err
		}
		if f, ok := facts[h+"\x00"+sid]; ok {
			f.LastUserTurnSeq = seq
		}
	}
	return rows.Err()
}

// fillErrorCounts 填每个会话的错误条数。
func (s *Store) fillErrorCounts(facts map[string]*SessionFact, harness string) error {
	sqlText := `
		SELECT harness, session_id, COUNT(*)
		  FROM events
		 WHERE kind = 'error'`
	args := []any{}
	if harness != "" {
		sqlText += ` AND harness = ?`
		args = append(args, harness)
	}
	sqlText += ` GROUP BY harness, session_id`

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h, sid string
		var n int64
		if err := rows.Scan(&h, &sid, &n); err != nil {
			return err
		}
		if f, ok := facts[h+"\x00"+sid]; ok {
			f.ErrorCount = n
		}
	}
	return rows.Err()
}

// fillAsks 取「等人回答类」的工具调用，并给它们配对结果、标出有没有拿到回答。
func (s *Store) fillAsks(facts map[string]*SessionFact, harness string) error {
	calls, err := s.sessionAskCalls(harness)
	if err != nil {
		return err
	}
	if len(calls) == 0 {
		return nil
	}

	// 只按这批调用的键去捞结果——不扫全库 6 万条 tool.result。
	keys := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.Key != "" {
			keys = append(keys, c.Key)
		}
	}
	results, err := s.sessionAskResults(keys)
	if err != nil {
		return err
	}

	// 结果按调用键索引：同一键多条结果时取「有回答的那条」——宁可报出已回答，
	// 也不把「其实答过了」误报成「卡住」。
	resByKey := map[string]SessionAsk{}
	for _, r := range results {
		key := r.Harness + "\x00" + r.SessionID + "\x00" + r.Key
		if prev, ok := resByKey[key]; ok && prev.Answered {
			continue
		}
		resByKey[key] = r
	}

	for _, c := range calls {
		f, ok := facts[c.Harness+"\x00"+c.SessionID]
		if !ok {
			continue
		}
		if r, ok := resByKey[c.Harness+"\x00"+c.SessionID+"\x00"+c.Key]; ok && r.Answered {
			c.Answered = true
			c.Answer = r.Answer
		}
		f.Asks = append(f.Asks, c)
	}
	return nil
}

// sessionAskCalls 返回「等人回答类」的工具调用。
//
// 这类工具各家叫法不同，实测本机库里出现过：hermes 的 clarify、pi 的
// ask_user_question、claude 的 AskUserQuestion、codex 的 request_user_input 与
// request_permissions。它们共同的特征是「调用方在等一个人回话」——正是竞品
// （tmux-agent-sidebar 的「needs permission」、MioIsland 的待批准）盯的那一类。
func (s *Store) sessionAskCalls(harness string) ([]SessionAsk, error) {
	// title 直接写进 SQL（而不是取回后在 Go 里筛）：有 (kind, title) 索引时
	// 只要 4ms，取回 7 万行再筛要 468ms。工具名清单与 isAskTool 同源。
	ph, args := askToolSQL()
	sqlText := `
		SELECT harness, session_id, seq, title, time, content,
		       COALESCE(CASE WHEN json_valid(meta) THEN json_extract(meta, '$.call_id') END, '') AS call_id,
		       parent_id
		  FROM events
		 WHERE kind = 'tool.call'
		   AND title IN (` + ph + `)`
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

	out := []SessionAsk{}
	for rows.Next() {
		var (
			a        SessionAsk
			ts       string
			parentID string
		)
		if err := rows.Scan(&a.Harness, &a.SessionID, &a.Seq, &a.Tool, &ts, &a.Content, &a.Key, &parentID); err != nil {
			return nil, err
		}
		a.Time = parseTime(ts)
		if a.Key == "" {
			a.Key = parentID
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// sessionAskResults 按调用键取回结果，并解析其中有没有回答。
func (s *Store) sessionAskResults(keys []string) ([]SessionAsk, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	// 键可能有上千个，分批查（SQLite 变量上限 999）。
	out := []SessionAsk{}
	for start := 0; start < len(keys); start += 500 {
		end := start + 500
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[start:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch))
		for _, k := range batch {
			args = append(args, k)
		}
		rows, err := s.db.Query(`
			SELECT harness, session_id, seq, title, time, content, parent_id
			  FROM events
			 WHERE kind = 'tool.result'
			   AND parent_id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				a        SessionAsk
				ts       string
				parentID string
			)
			if err := rows.Scan(&a.Harness, &a.SessionID, &a.Seq, &a.Tool, &ts, &a.Content, &parentID); err != nil {
				rows.Close()
				return nil, err
			}
			a.Time = parseTime(ts)
			a.Key = parentID
			answer, ok := extractAnswer(a.Content)
			if !ok {
				continue
			}
			a.Answer = answer
			a.Answered = true
			out = append(out, a)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// askToolNames 是「等人回答类」的工具名清单。
//
// 名单来自实测本机库（2026-10-23）里出现过的工具名，不是猜测：
// hermes=clarify、pi=ask_user_question、claude=AskUserQuestion、
// codex=request_user_input / request_permissions。
//
// 清单同时用在 SQL 的 title IN (...) 里（走 (kind, title) 索引，4ms）
// 与 Go 侧的兜底判定里——两处必须同源，否则会出现「SQL 捞到了但 Go 丢掉」
// 或反过来的口径不一致。
var askToolNames = []string{
	"clarify",
	"ask_user_question",
	"AskUserQuestion",
	"request_user_input",
	"request_permissions",
}

// askToolSQL 返回带占位符的 title IN (...) 片段与对应参数。
func askToolSQL() (string, []any) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(askToolNames)), ",")
	args := make([]any, 0, len(askToolNames))
	for _, n := range askToolNames {
		args = append(args, n)
	}
	return ph, args
}

// isAskTool 判断工具名是否属于「等人回答类」。
func isAskTool(tool string) bool {
	for _, n := range askToolNames {
		if n == tool {
			return true
		}
	}
	return false
}

// extractAnswer 从工具结果原文里判断「用户有没有回答」，并取出回答内容。
//
// 各家形态（实测）：
//   - hermes clarify：{"responses":[{"question":..,"user_response":".."}]}
//     user_response 为空字符串 = 问了但用户没选；
//   - pi ask_user_question："User has answered your questions: \"问\"=\"答\""
//     或 "User declined to answer questions"（拒绝 = 回答了，只是内容是拒绝）；
//   - codex request_user_input：{"answers":{"id":{"answers":["选项"]}}}
//   - codex request_permissions：{"permissions":{...},"scope":"turn"}
//     —— 授权本身就是「回答」（拒绝了就是 permissions 里全 null）。
//
// 判不了的（解析失败、形态不认识）返回 ok=false —— 不把「看不懂」当成「没回答」。
func extractAnswer(content string) (string, bool) {
	if content == "" {
		return "", false
	}
	// hermes 的 clarify：JSON 里 responses[].user_response。
	if s, ok := extractHermesAnswer(content); ok {
		return s, true
	}
	// pi 的 ask_user_question / claude 的 AskUserQuestion：纯文本。
	if s, ok := extractPlainAnswer(content); ok {
		return s, true
	}
	// codex 的 request_user_input / request_permissions：JSON。
	if s, ok := extractCodexAnswer(content); ok {
		return s, true
	}
	return "", false
}

// extractHermesAnswer 处理 hermes clarify 的结果形态。
//
// 口径：user_response 为空字符串表示「问了但用户没选」——这**不是**回答，
// 返回 ok=false（判为没回答），但也不算「解析失败」。
func extractHermesAnswer(content string) (string, bool) {
	var payload struct {
		Responses []struct {
			Question     string `json:"question"`
			UserResponse string `json:"user_response"`
		} `json:"responses"`
	}
	if json.Unmarshal([]byte(content), &payload) != nil || len(payload.Responses) == 0 {
		return "", false
	}
	parts := []string{}
	for _, r := range payload.Responses {
		if r.UserResponse == "" {
			continue
		}
		parts = append(parts, r.UserResponse)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "；"), true
}

// extractPlainAnswer 处理纯文本形态（pi 的 ask_user_question、claude 的 AskUserQuestion）。
func extractPlainAnswer(content string) (string, bool) {
	if !strings.HasPrefix(content, "User has answered your questions") &&
		!strings.HasPrefix(content, "User declined to answer questions") {
		return "", false
	}
	return oneLine(content), true
}

// extractCodexAnswer 处理 codex request_user_input / request_permissions 的结果形态。
//
// request_user_input 的实际形态（实测）：
//
//	{"answers":{"<问题 id>":{"answers":["选项一","选项二"]}}}
//
// 注意外层是 {"answers":{...}}，内层每个 id 下又有一个 "answers" 数组——
// 两层同名，解析时别把外层当成内层。
func extractCodexAnswer(content string) (string, bool) {
	var payload struct {
		Answers map[string]struct {
			Answers []string `json:"answers"`
		} `json:"answers"`
	}
	if json.Unmarshal([]byte(content), &payload) == nil && len(payload.Answers) > 0 {
		parts := []string{}
		for _, v := range payload.Answers {
			parts = append(parts, v.Answers...)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "；"), true
		}
		return "", false
	}
	// request_permissions：授权结果本身就是回答（permissions 全 null = 被拒绝）。
	var perm struct {
		Permissions map[string]any `json:"permissions"`
		Scope       string         `json:"scope"`
	}
	if json.Unmarshal([]byte(content), &perm) == nil && perm.Permissions != nil {
		granted := []string{}
		for name, v := range perm.Permissions {
			if m, ok := v.(map[string]any); ok {
				if en, ok := m["enabled"].(bool); ok && en {
					granted = append(granted, name)
				}
				continue
			}
			if v != nil {
				granted = append(granted, name)
			}
		}
		if len(granted) == 0 {
			return "授权被拒绝", true
		}
		return "已授权：" + strings.Join(granted, "、"), true
	}
	return "", false
}

// oneLine 把多行压成一行（回答文本常有换行）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
