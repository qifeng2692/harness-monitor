package store

import (
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// TestSessionPinnedLifecycle 覆盖：默认未置顶 → 置顶生效且幂等 → 排序置前 →
// 取消后恢复原排序 → 取消幂等。置顶表与 events 采集数据互不影响。
func TestSessionPinnedLifecycle(t *testing.T) {
	s := newTestStore(t)

	// 三个会话：old（最早）、mid、new（最近活动）——按最近活动倒序应为 new, mid, old。
	now := time.Now().UTC()
	mk := func(sessionID string, offset time.Duration) {
		events := []model.Event{
			{Harness: "hermes", SessionID: sessionID, Kind: model.KindTurnStart,
				Title: "输入", Content: sessionID + " 的输入", Time: now.Add(-offset),
				SourceKey: "test:" + sessionID + ":0"},
			{Harness: "hermes", SessionID: sessionID, Kind: model.KindSessionEnd,
				Title: "", Content: "", Time: now.Add(-offset + time.Minute),
				SourceKey: "test:" + sessionID + ":1"},
		}
		if _, _, err := s.InsertEvents(events); err != nil {
			t.Fatalf("InsertEvents(%s): %v", sessionID, err)
		}
	}
	mk("old", 3*time.Hour)
	mk("mid", 2*time.Hour)
	mk("new", 1*time.Hour)

	// 初始：全部未置顶。
	sessions, err := s.ListSessions("hermes")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("应有 3 个会话，得到 %d", len(sessions))
	}
	for _, sess := range sessions {
		if sess.Pinned {
			t.Errorf("初始 %s 不应置顶", sess.SessionID)
		}
	}
	if sessions[0].SessionID != "new" {
		t.Errorf("未置顶时应按最近活动倒序，首行应为 new，得到 %s", sessions[0].SessionID)
	}

	// 置顶最旧的 old：应排到最前。
	if err := s.SetSessionPinned("hermes", "old", true); err != nil {
		t.Fatalf("SetSessionPinned(true): %v", err)
	}
	pinned, err := s.SessionPinned("hermes", "old")
	if err != nil {
		t.Fatalf("SessionPinned: %v", err)
	}
	if !pinned {
		t.Fatal("置顶后 SessionPinned 应为 true")
	}
	sessions, err = s.ListSessions("hermes")
	if err != nil {
		t.Fatalf("ListSessions(置顶后): %v", err)
	}
	if sessions[0].SessionID != "old" || !sessions[0].Pinned {
		t.Errorf("置顶的 old 应排最前且 Pinned=true，得到 %s pinned=%v", sessions[0].SessionID, sessions[0].Pinned)
	}
	if sessions[1].SessionID != "new" || sessions[2].SessionID != "mid" {
		t.Errorf("未置顶的仍按最近活动倒序，得到 %s,%s", sessions[1].SessionID, sessions[2].SessionID)
	}

	// 重复置顶幂等（仍是一个 pin 行，且不报错）。
	if err := s.SetSessionPinned("hermes", "old", true); err != nil {
		t.Fatalf("重复 SetSessionPinned(true): %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM session_pins`).Scan(&n); err != nil {
		t.Fatalf("COUNT(session_pins): %v", err)
	}
	if n != 1 {
		t.Fatalf("重复置顶后应仍只有 1 行 pin，得到 %d", n)
	}

	// 取消置顶：恢复按最近活动倒序；再取消一次仍幂等。
	if err := s.SetSessionPinned("hermes", "old", false); err != nil {
		t.Fatalf("SetSessionPinned(false): %v", err)
	}
	if err := s.SetSessionPinned("hermes", "old", false); err != nil {
		t.Fatalf("重复 SetSessionPinned(false): %v", err)
	}
	pinned, _ = s.SessionPinned("hermes", "old")
	if pinned {
		t.Fatal("取消后 SessionPinned 应为 false")
	}
	sessions, err = s.ListSessions("hermes")
	if err != nil {
		t.Fatalf("ListSessions(取消后): %v", err)
	}
	if sessions[0].SessionID != "new" {
		t.Errorf("取消置顶后首行应恢复为 new，得到 %s", sessions[0].SessionID)
	}

	// pin 按 (harness, session_id) 隔离：hermes 的 pin 不影响 codex 的同名会话。
	if err := s.SetSessionPinned("hermes", "mid", true); err != nil {
		t.Fatalf("SetSessionPinned(codex 前置): %v", err)
	}
	cx := []model.Event{
		{Harness: "codex", SessionID: "mid", Kind: model.KindTurnStart, Title: "输入", Content: "x", Time: now},
	}
	if _, _, err := s.InsertEvents(cx); err != nil {
		t.Fatalf("InsertEvents(codex/mid): %v", err)
	}
	codexSessions, err := s.ListSessions("codex")
	if err != nil {
		t.Fatalf("ListSessions(codex): %v", err)
	}
	if len(codexSessions) != 1 || codexSessions[0].Pinned {
		t.Errorf("codex 的 mid 不应被 hermes 的置顶影响，得到 %+v", codexSessions)
	}
}

// TestPinSurvivesReseed 覆盖置顶表与采集数据隔离的关键承诺：events 被清空重采后
// （同 harness+session_id 重新入库），置顶仍然保留。
func TestPinSurvivesReseed(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UTC()
	events := []model.Event{
		{Harness: "pi", SessionID: "sess-1", Kind: model.KindTurnStart, Title: "输入", Content: "c", Time: now,
			SourceKey: "test:reseed:0"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	if err := s.SetSessionPinned("pi", "sess-1", true); err != nil {
		t.Fatalf("SetSessionPinned: %v", err)
	}

	// 模拟清库重采：删光 events（不动 session_pins），再原样插回。
	if _, err := s.DB().Exec(`DELETE FROM events`); err != nil {
		t.Fatalf("DELETE events: %v", err)
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("重插 events: %v", err)
	}

	sessions, err := s.ListSessions("pi")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || !sessions[0].Pinned {
		t.Fatalf("清库重采后置顶应保留，得到 %+v", sessions)
	}
}
