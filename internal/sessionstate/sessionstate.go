// Package sessionstate 判定每个会话「此刻处于什么状态」。
//
// 为什么需要它：harness-monitor 此前只回答「这个会话里发生了什么」，答不了
// 「这个会话现在卡在哪、要不要我出手」。用户看到一长串会话列表，只能自己点进去
// 逐个翻时间线，才能找出那个正等着他回答问题的会话。
//
// 同类工具都把「会话当前状态」当核心信息：
//   - tmux-agent-sidebar：侧边栏每个 pane 一列状态，含「needs permission」「等待原因」；
//   - MioIsland：从刘海里直接看到并批准待处理的请求。
//
// 本包是纯函数：输入是 store 取出的事实（store.SessionFact），输出是状态判定，
// 不碰数据库也不碰前端，因此可单测、可被 GUI 与命令行共用。
//
// 判定纪律（与 internal/stats、internal/loops 一致）：
//   - 「等人回答」只认**结构化事实**：存在一个「等人回答类」工具调用，且这次调用
//     没有拿到回答，且用户在此之后没有再说过话。判不出就说判不出，不猜正文；
//   - 「失败」只认 error 事件（各家自己上报的结构化字段）；
//   - 「已完成」只认 turn.end / session.end 这类终态标记。
package sessionstate

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/store"
)

// State 是一个会话的状态。
type State string

const (
	// StateWaiting：会话正在等用户回答（提问了但没得到回答）。
	StateWaiting State = "waiting"
	// StateFailed：会话最后一步是错误。
	StateFailed State = "failed"
	// StateDone：会话有终态标记（turn.end / session.end）。
	StateDone State = "done"
	// StateActive：最近还有动作，agent 正在跑。
	StateActive State = "active"
	// StateIdle：很久没动作了。
	StateIdle State = "idle"
)

// Label 返回状态的中文标签（面向用户展示，界面里不出现英文状态名）。
func (s State) Label() string {
	switch s {
	case StateWaiting:
		return "等待你"
	case StateFailed:
		return "失败"
	case StateDone:
		return "已完成"
	case StateActive:
		return "进行中"
	case StateIdle:
		return "空闲"
	}
	return "未知"
}

// DefaultActiveWindow 是「进行中」与「空闲」的分界：最近一次动作在这么久之内就算进行中。
//
// 实测本机库（2026-10-23）：90 分钟内有过动作的会话里，绝大多数还在几十分钟内
// 追加过新事件——agent 干活时是连续追加的，不会中途停 90 分钟再继续。
const DefaultActiveWindow = 90 * time.Minute

// PendingAsk 是一个正在等回答的提问。
type PendingAsk struct {
	Tool     string    // 工具名（clarify / ask_user_question / request_permissions …）
	Seq      int64     // 提问那一步的 seq
	Time     time.Time // 提问时间
	Question string    // 问的是什么（从参数里提取，取不到就空）
}

// Session 是一个会话的状态判定结果。
type Session struct {
	Harness   string
	SessionID string

	State  State
	Reason string    // 人话说明（界面直接显示）
	Since  time.Time // 进入当前状态的时刻（= 最后一次动作的时间）
	LastAt time.Time // 最后一次动作的时间

	// PendingAsk 仅在 State == StateWaiting 时非空——把「在等什么」一起带出来，
	// 用户不用点进时间线就知道该回什么。
	PendingAsk *PendingAsk
}

// Summary 是全体会话的状态汇总。
type Summary struct {
	Total  int
	Counts map[State]int
}

// Summarize 统计各状态的数量。
func Summarize(sessions []Session) Summary {
	s := Summary{Total: len(sessions), Counts: map[State]int{}}
	for _, sess := range sessions {
		s.Counts[sess.State]++
	}
	return s
}

// Classify 判定一个会话的状态。
//
// now 是「当前时间」（由调用方传入，便于测试与口径统一）；
// activeWindow 是「进行中」与「空闲」的分界，<=0 时用 DefaultActiveWindow。
//
// 判定顺序（顺序本身是口径的一部分）：
//  1. 失败：最后一步是 error —— 这是最需要立刻看见的状态，压过其它；
//  2. 已完成：最后一步是 turn.end / session.end —— 明确收尾；
//  3. 等待你：存在没被回答的提问，且用户在这之后没再说过话；
//  4. 进行中 / 空闲：按最后一次动作距今多久分。
func Classify(f store.SessionFact, now time.Time, activeWindow time.Duration) Session {
	if activeWindow <= 0 {
		activeWindow = DefaultActiveWindow
	}

	out := Session{
		Harness:   f.Harness,
		SessionID: f.SessionID,
		Since:     f.LastTime,
		LastAt:    f.LastTime,
	}

	// 1) 失败。
	if f.LastKind == "error" {
		out.State = StateFailed
		out.Reason = failReason(f.LastTitle)
		return out
	}

	// 2) 已完成。
	if f.LastKind == "turn.end" || f.LastKind == "session.end" {
		out.State = StateDone
		out.Reason = doneReason(f.LastKind)
		return out
	}

	// 3) 等待用户回答。
	if ask, ok := pendingAsk(f); ok {
		out.State = StateWaiting
		out.PendingAsk = &ask
		out.Reason = waitingReason(ask)
		return out
	}

	// 4) 进行中 / 空闲。
	age := now.Sub(f.LastTime)
	if age <= activeWindow {
		out.State = StateActive
		out.Reason = "最近 " + humanDuration(age) + "还有动作"
		return out
	}
	out.State = StateIdle
	out.Reason = "已经 " + humanDuration(age) + "没有动作"
	return out
}

// pendingAsk 找出这个会话里「正在等用户回答」的那次提问。
//
// 三个条件同时成立才算在等：
//   - 存在一个「等人回答类」的调用；
//   - 这次调用没有拿到回答（结果缺失，或结果里没有回答内容）；
//   - 用户在这之后没有再说过话（否则说明 agent 已经往下走了，不算卡住）。
//
// 多个提问都没回答时，取**最后一个**——那是当前真正卡住的那一问。
func pendingAsk(f store.SessionFact) (PendingAsk, bool) {
	var last *PendingAsk
	for i := range f.Asks {
		a := f.Asks[i]
		if a.Answered {
			continue
		}
		// 用户之后又说过话 → agent 已经往下走了，不算卡在这次提问上。
		if f.LastUserTurnSeq > a.Seq {
			continue
		}
		p := PendingAsk{
			Tool:     a.Tool,
			Seq:      a.Seq,
			Time:     a.Time,
			Question: ExtractQuestion(a.Content),
		}
		last = &p
	}
	if last == nil {
		return PendingAsk{}, false
	}
	return *last, true
}

// ExtractQuestion 从「等人回答类」调用的参数原文里提取问的是什么。
//
// 各家形态（实测本机库 2026-10-23）：
//
//	hermes clarify      {"questions":[{"question":"...","choices":[...]}]}
//	pi ask_user_question {"questions":[{"header":"...","options":[...],"question":"..."}]}
//	claude AskUserQuestion {"questions":[{"question":"..."}]}
//	codex request_user_input {"questions":[{"id":"...","header":"...","question":"..."}]}
//	codex request_permissions {"permissions":{...},"reason":"..."}   ← 用 reason 当问题
//
// 取不到就返回空字符串——不编造问题。
func ExtractQuestion(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}

	// codex request_permissions 没有问句，用 reason 说明「为什么需要授权」。
	var perm struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(content), &perm) == nil && perm.Reason != "" {
		return perm.Reason
	}

	var payload struct {
		Questions []struct {
			Question string `json:"question"`
			Header   string `json:"header"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(content), &payload) != nil {
		return ""
	}
	for _, q := range payload.Questions {
		if q.Question != "" {
			return q.Question
		}
		if q.Header != "" {
			return q.Header
		}
	}
	return ""
}

// waitingReason 生成「为什么在等你」的人话说明。
//
// 只讲「在等什么类型的事」，不把问题正文抄一遍——问题正文由 PendingAsk.Question
// 单独带出，重复一遍会让界面显得啰嗦。
func waitingReason(ask PendingAsk) string {
	switch ask.Tool {
	case "request_permissions":
		return "等你批准权限"
	case "request_user_input", "ask_user_question", "AskUserQuestion", "clarify":
		return "等你回答"
	}
	return "等你的回应"
}

// failReason 生成失败说明。
func failReason(title string) string {
	if title == "" {
		return "最后一步是错误"
	}
	return "最后一步是错误：" + title
}

// doneReason 生成完成说明。
func doneReason(kind string) string {
	if kind == "session.end" {
		return "会话已正常结束"
	}
	return "这一轮已完成"
}

// humanDuration 把时长说成人话（中文）。
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "不到 1 分钟"
	case d < time.Hour:
		return fmtInt(int(d.Minutes())) + " 分钟"
	case d < 24*time.Hour:
		return fmtInt(int(d.Hours())) + " 小时"
	default:
		return fmtInt(int(d.Hours()/24)) + " 天"
	}
}

// fmtInt 输出整数（不引 strconv，减少依赖面）。
func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
