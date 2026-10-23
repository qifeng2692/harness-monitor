// session_state.go 是 Wails 绑定层里「会话状态」这一块的实现。
//
// 为什么需要：此前 GUI 的会话列表只有 id/标题/事件数/时间，用户看不出哪个会话
// 正等着他回答、哪个刚失败——只能逐个点进去翻时间线。同类工具
// （tmux-agent-sidebar 的状态列、MioIsland 的待批准提醒）都把「会话当前状态」
// 当核心信息。本文件把 internal/sessionstate 的判定结果转成前端友好的结构。
package main

import (
	"fmt"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/sessionstate"
)

// SessionStateInfo 是一个会话的状态（前端会话列表的一行）。
type SessionStateInfo struct {
	SessionID string `json:"sessionId"`

	// State 是状态代码（waiting / failed / done / active / idle）。
	State string `json:"state"`

	// StateLabel 是中文标签（界面直接显示这个，不显示英文代码）。
	StateLabel string `json:"stateLabel"`

	// StateReason 是一句话说明（为什么是这个状态）。
	StateReason string `json:"stateReason"`

	// PendingQuestion 是正在等用户回答的问题正文（只在 waiting 时有）。
	PendingQuestion string `json:"pendingQuestion"`

	// LastAt 是最后一次动作的时间。
	LastAt string `json:"lastAt"`
}

// SessionStateSummary 是全体会话的状态汇总（GUI 顶部概览用）。
type SessionStateSummary struct {
	Total  int            `json:"total"`
	Counts map[string]int `json:"counts"` // 状态代码 → 数量
	Labels map[string]int `json:"labels"` // 状态中文标签 → 数量
}

// sessionStates 算出一批会话的状态（harness 为空 = 全部）。
func (a *App) sessionStates(harness string) (map[string]SessionStateInfo, error) {
	if a.store == nil {
		return nil, fmt.Errorf("数据库未打开")
	}
	facts, err := a.store.SessionFacts(harness)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := map[string]SessionStateInfo{}
	for _, f := range facts {
		sess := sessionstate.Classify(f, now, sessionstate.DefaultActiveWindow)
		info := SessionStateInfo{
			SessionID:   f.SessionID,
			State:       string(sess.State),
			StateLabel:  sess.State.Label(),
			StateReason: sess.Reason,
			LastAt:      sess.LastAt.Local().Format("2006-01-02 15:04:05"),
		}
		if sess.PendingAsk != nil {
			info.PendingQuestion = sess.PendingAsk.Question
		}
		out[f.Harness+"\x00"+f.SessionID] = info
	}
	return out, nil
}

// SessionStates 返回某 harness 下所有会话的状态（前端单独拉一次，按 sessionId 对齐）。
func (a *App) SessionStates(harness string) ([]SessionStateInfo, error) {
	states, err := a.sessionStates(harness)
	if err != nil {
		return nil, err
	}
	out := make([]SessionStateInfo, 0, len(states))
	for _, v := range states {
		out = append(out, v)
	}
	return out, nil
}

// SessionStateSummary 返回全体会话的状态汇总。
func (a *App) SessionStateSummary(harness string) (SessionStateSummary, error) {
	if a.store == nil {
		return SessionStateSummary{}, fmt.Errorf("数据库未打开")
	}
	facts, err := a.store.SessionFacts(harness)
	if err != nil {
		return SessionStateSummary{}, err
	}
	now := time.Now()
	sessions := make([]sessionstate.Session, 0, len(facts))
	for _, f := range facts {
		sessions = append(sessions, sessionstate.Classify(f, now, sessionstate.DefaultActiveWindow))
	}
	sum := sessionstate.Summarize(sessions)
	out := SessionStateSummary{
		Total:  sum.Total,
		Counts: map[string]int{},
		Labels: map[string]int{},
	}
	for st, n := range sum.Counts {
		out.Counts[string(st)] = n
		out.Labels[st.Label()] = n
	}
	return out, nil
}
