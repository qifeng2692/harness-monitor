package main

import (
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

// newTestApp 开一个临时库的 App（绑定层测试用）。
func newTestApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	s, err := store.OpenPath(t.TempDir() + "/hm.db")
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return &App{store: s}, s
}

// TestListSessionsIncludesState 覆盖：会话列表要带上状态与「在等什么」，
// 前端才能在列表上直接看出哪个会话卡住了。
func TestListSessionsIncludesState(t *testing.T) {
	app, s := newTestApp(t)

	events := []model.Event{
		{Harness: "hermes", SessionID: "waiting", Kind: model.KindTurnStart, Title: "user",
			Content: "干活", Time: time.Now().UTC(), SourceKey: "t:0"},
		{Harness: "hermes", SessionID: "waiting", Kind: model.KindToolCall, Title: "clarify",
			Content: `{"questions":[{"question":"先只读行吗？"}]}`,
			Meta:    `{"call_id":"c1","tool_name":"clarify"}`, Time: time.Now().UTC(), SourceKey: "t:1"},
		{Harness: "hermes", SessionID: "done", Kind: model.KindTurnStart, Title: "user",
			Content: "收尾", Time: time.Now().UTC(), SourceKey: "t:2"},
		{Harness: "hermes", SessionID: "done", Kind: model.KindTurnEnd, Title: "turn complete",
			Time: time.Now().UTC(), SourceKey: "t:3"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	sessions, err := app.ListSessions("hermes")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("应有 2 个会话，得到 %d", len(sessions))
	}

	byID := map[string]SessionInfo{}
	for _, s := range sessions {
		byID[s.SessionID] = s
	}

	w := byID["waiting"]
	if w.State != "waiting" {
		t.Errorf("waiting 会话的状态 = %q，期望 waiting", w.State)
	}
	if w.StateLabel != "等待你" {
		t.Errorf("状态标签 = %q，期望 等待你", w.StateLabel)
	}
	if w.PendingQuestion != "先只读行吗？" {
		t.Errorf("待回答的问题 = %q，期望 先只读行吗？", w.PendingQuestion)
	}
	if w.StateReason == "" {
		t.Error("应给出状态说明")
	}

	d := byID["done"]
	if d.State != "done" {
		t.Errorf("done 会话的状态 = %q，期望 done", d.State)
	}
	if d.PendingQuestion != "" {
		t.Errorf("非等待会话不该带待回答问题，得到 %q", d.PendingQuestion)
	}
}

// TestSessionStatesCounts 覆盖：状态汇总要按状态给出数量（GUI 顶部的概览用）。
func TestSessionStatesCounts(t *testing.T) {
	app, s := newTestApp(t)

	events := []model.Event{
		{Harness: "pi", SessionID: "e1", Kind: model.KindError, Title: "模型调用失败：429",
			Time: time.Now().UTC(), SourceKey: "t:0"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	sum, err := app.SessionStateSummary("")
	if err != nil {
		t.Fatalf("SessionStateSummary: %v", err)
	}
	if sum.Total != 1 {
		t.Errorf("会话总数 = %d，期望 1", sum.Total)
	}
	if sum.Counts["failed"] != 1 {
		t.Errorf("失败数 = %d，期望 1", sum.Counts["failed"])
	}
}
