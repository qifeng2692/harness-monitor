package store

import (
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// TestSessionToolCalls 覆盖「会话文件改动」的取数层：只取该会话的 tool.call，
// 按 seq 升序，且带工具名与参数原文（判定文件路径的前提）。
func TestSessionToolCalls(t *testing.T) {
	s := newTestStore(t)

	t0 := time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)
	events := []model.Event{
		{Harness: "hermes", SessionID: "s1", Seq: 2, Time: t0.Add(time.Minute), Kind: model.KindToolCall, Title: "write_file",
			Content: `{"content":"x","path":"G:/a/b.go"}`, Meta: `{"call_id":"c2"}`, SourceKey: "f:0"},
		{Harness: "hermes", SessionID: "s1", Seq: 1, Time: t0, Kind: model.KindToolCall, Title: "read_file",
			Content: `{"path":"G:/a/c.go"}`, Meta: `{"call_id":"c1"}`, SourceKey: "f:1"},
		// 另一个会话的调用不该被取进来
		{Harness: "hermes", SessionID: "s2", Seq: 1, Kind: model.KindToolCall, Title: "patch",
			Content: `{"path":"G:/a/d.go"}`, Meta: `{"call_id":"c3"}`, SourceKey: "f:2"},
		// 非工具调用事件不该被取进来
		{Harness: "hermes", SessionID: "s1", Seq: 3, Kind: model.KindLLMResponse, Title: "回答",
			SourceKey: "f:3"},
		// 同一会话的另一家 harness 也不该被取进来
		{Harness: "claude", SessionID: "s1", Seq: 1, Kind: model.KindToolCall, Title: "Write",
			Content: `{"file_path":"G:/a/e.go"}`, Meta: `{"call_id":"c4"}`, SourceKey: "f:4"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	rows, err := s.SessionToolCalls("hermes", "s1")
	if err != nil {
		t.Fatalf("SessionToolCalls: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有 2 条调用（只算 hermes/s1 的 tool.call），得到 %d", len(rows))
	}
	if rows[0].Tool != "read_file" || rows[1].Tool != "write_file" {
		t.Fatalf("应按 seq 升序返回 read_file → write_file，实际 %q → %q", rows[0].Tool, rows[1].Tool)
	}
	if rows[1].Args != `{"content":"x","path":"G:/a/b.go"}` {
		t.Errorf("参数原文应原样返回，实际 %q", rows[1].Args)
	}
	if rows[1].Seq != 2 {
		t.Errorf("Seq = %d，期望 2", rows[1].Seq)
	}
	if !rows[1].Time.Equal(t0.Add(time.Minute)) {
		t.Errorf("Time = %v，期望 %v（时间要原样读回）", rows[1].Time, t0.Add(time.Minute))
	}

	// 没有工具调用的会话：空结果而不是报错。
	empty, err := s.SessionToolCalls("hermes", "nope")
	if err != nil {
		t.Fatalf("SessionToolCalls(空会话): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空会话应返回 0 条，得到 %d", len(empty))
	}
}
