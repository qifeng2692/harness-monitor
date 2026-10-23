package store

import (
	"testing"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// TestSessionFactsPicksUpAsks 覆盖取数层：只有「等人回答类」的工具调用会被取出来，
// 且能按调用键配上结果、标出有没有拿到回答。
func TestSessionFactsPicksUpAsks(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		{Harness: "hermes", SessionID: "s1", Kind: model.KindTurnStart, Title: "user",
			Content: "干活", SourceKey: "t:0"},
		// hermes 的 clarify：结果里 user_response 非空 = 答过了。
		{Harness: "hermes", SessionID: "s1", Kind: model.KindToolCall, Title: "clarify",
			Content: `{"questions":[{"question":"先只读行吗？"}]}`,
			Meta:    `{"call_id":"c1","tool_name":"clarify"}`, SourceKey: "t:1"},
		{Harness: "hermes", SessionID: "s1", Kind: model.KindToolResult, Title: "clarify",
			Content: `{"responses":[{"question":"先只读行吗？","user_response":"好"}]}`,
			Meta:    `{"tool_call_id":"c1","tool_name":"clarify"}`, ParentID: "c1", SourceKey: "t:2"},
		// 另一个会话：clarify 的 user_response 为空 = 问了但用户没选。
		{Harness: "hermes", SessionID: "s2", Kind: model.KindToolCall, Title: "clarify",
			Content: `{"questions":[{"question":"用哪种交付？"}]}`,
			Meta:    `{"call_id":"c2","tool_name":"clarify"}`, SourceKey: "t:3"},
		{Harness: "hermes", SessionID: "s2", Kind: model.KindToolResult, Title: "clarify",
			Content: `{"responses":[{"question":"用哪种交付？","user_response":""}]}`,
			Meta:    `{"tool_call_id":"c2","tool_name":"clarify"}`, ParentID: "c2", SourceKey: "t:4"},
		// 非「等人回答类」的工具不该被取出来。
		{Harness: "hermes", SessionID: "s1", Kind: model.KindToolCall, Title: "terminal",
			Content: `{"command":"ls"}`, Meta: `{"call_id":"c3"}`, SourceKey: "t:5"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	facts, err := s.SessionFacts("")
	if err != nil {
		t.Fatalf("SessionFacts: %v", err)
	}
	bySess := map[string]SessionFact{}
	for _, f := range facts {
		bySess[f.SessionID] = f
	}

	s1, ok := bySess["s1"]
	if !ok {
		t.Fatal("应有会话 s1")
	}
	if len(s1.Asks) != 1 {
		t.Fatalf("s1 应只有 1 次提问，得到 %d", len(s1.Asks))
	}
	if !s1.Asks[0].Answered {
		t.Error("s1 的 clarify 拿到了回答（user_response 非空），应标为已回答")
	}
	if s1.Asks[0].Answer != "好" {
		t.Errorf("回答内容 = %q，期望 好", s1.Asks[0].Answer)
	}
	if s1.LastUserTurnSeq == 0 {
		t.Error("s1 应记下最后一个用户输入的 seq")
	}

	s2, ok := bySess["s2"]
	if !ok {
		t.Fatal("应有会话 s2")
	}
	if len(s2.Asks) != 1 {
		t.Fatalf("s2 应只有 1 次提问，得到 %d", len(s2.Asks))
	}
	if s2.Asks[0].Answered {
		t.Error("s2 的 clarify 是空回答（user_response=\"\"），不该标为已回答")
	}
}

// TestSessionFactsAnswerShapes 覆盖各家回答形态的解析：
// pi 的纯文本、codex 的 answers JSON、codex 的授权结果。
func TestSessionFactsAnswerShapes(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		{Harness: "pi", SessionID: "p1", Kind: model.KindToolCall, Title: "ask_user_question",
			Content: `{"questions":[{"question":"用哪个？"}]}`,
			Meta:    `{"call_id":"k1","tool_name":"ask_user_question"}`, SourceKey: "t:0"},
		{Harness: "pi", SessionID: "p1", Kind: model.KindToolResult, Title: "tool result",
			Content: `User has answered your questions: "用哪个？"="方案 A". You can now continue.`,
			Meta:    `{"call_id":"k1"}`, ParentID: "k1", SourceKey: "t:1"},
		{Harness: "pi", SessionID: "p2", Kind: model.KindToolCall, Title: "ask_user_question",
			Content: `{"questions":[{"question":"继续？"}]}`,
			Meta:    `{"call_id":"k2","tool_name":"ask_user_question"}`, SourceKey: "t:2"},
		{Harness: "pi", SessionID: "p2", Kind: model.KindToolResult, Title: "tool result",
			Content: `User declined to answer questions`,
			Meta:    `{"call_id":"k2"}`, ParentID: "k2", SourceKey: "t:3"},
		{Harness: "codex", SessionID: "x1", Kind: model.KindToolCall, Title: "request_user_input",
			Content: `{"questions":[{"id":"a","question":"先做哪个端？"}]}`,
			Meta:    `{"call_id":"k3","name":"request_user_input"}`, SourceKey: "t:4"},
		{Harness: "codex", SessionID: "x1", Kind: model.KindToolResult, Title: "tool result",
			Content: `{"answers":{"a":{"answers":["Web 先行"]}}}`,
			Meta:    `{"call_id":"k3"}`, ParentID: "k3", SourceKey: "t:5"},
		{Harness: "codex", SessionID: "x2", Kind: model.KindToolCall, Title: "request_permissions",
			Content: `{"permissions":{"network":{"enabled":true}},"reason":"要装依赖"}`,
			Meta:    `{"call_id":"k4","name":"request_permissions"}`, SourceKey: "t:6"},
		{Harness: "codex", SessionID: "x2", Kind: model.KindToolResult, Title: "tool result",
			Content: `{"permissions":{"network":{"enabled":true},"file_system":null},"scope":"turn"}`,
			Meta:    `{"call_id":"k4"}`, ParentID: "k4", SourceKey: "t:7"},
		// 拒绝授权：permissions 全 null —— 也是「回答了」（回答内容是拒绝）。
		{Harness: "codex", SessionID: "x3", Kind: model.KindToolCall, Title: "request_permissions",
			Content: `{"permissions":{"network":{"enabled":false}},"reason":"不联网"}`,
			Meta:    `{"call_id":"k5","name":"request_permissions"}`, SourceKey: "t:8"},
		{Harness: "codex", SessionID: "x3", Kind: model.KindToolResult, Title: "tool result",
			Content: `{"permissions":{"network":null,"file_system":null},"scope":"turn"}`,
			Meta:    `{"call_id":"k5"}`, ParentID: "k5", SourceKey: "t:9"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	facts, err := s.SessionFacts("")
	if err != nil {
		t.Fatalf("SessionFacts: %v", err)
	}
	bySess := map[string]SessionFact{}
	for _, f := range facts {
		bySess[f.SessionID] = f
	}

	check := func(sid string, wantAnswered bool, wantContains string) {
		t.Helper()
		f, ok := bySess[sid]
		if !ok {
			t.Fatalf("应有会话 %s", sid)
		}
		if len(f.Asks) != 1 {
			t.Fatalf("%s 应只有 1 次提问，得到 %d", sid, len(f.Asks))
		}
		if f.Asks[0].Answered != wantAnswered {
			t.Errorf("%s 的已回答标记 = %v，期望 %v（回答=%q）",
				sid, f.Asks[0].Answered, wantAnswered, f.Asks[0].Answer)
		}
		if wantContains != "" && !contains(f.Asks[0].Answer, wantContains) {
			t.Errorf("%s 的回答 = %q，应含 %q", sid, f.Asks[0].Answer, wantContains)
		}
	}

	check("p1", true, "方案 A")
	check("p2", true, "declined")
	check("x1", true, "Web 先行")
	check("x2", true, "network")
	check("x3", true, "拒绝")
}

// TestSessionFactsIgnoresUnknownResultShape 覆盖「判不了」的情况：
// 结果形态不认识时不能当成「没回答」，也不能当成「回答了」——
// 这里期望它既不标已回答、也不报错。
func TestSessionFactsIgnoresUnknownResultShape(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		{Harness: "pi", SessionID: "p9", Kind: model.KindToolCall, Title: "ask_user_question",
			Content: `{"questions":[{"question":"继续？"}]}`,
			Meta:    `{"call_id":"z1","tool_name":"ask_user_question"}`, SourceKey: "t:0"},
		{Harness: "pi", SessionID: "p9", Kind: model.KindToolResult, Title: "tool result",
			Content: `某个不认识的结果形态`,
			Meta:    `{"call_id":"z1"}`, ParentID: "z1", SourceKey: "t:1"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	facts, err := s.SessionFacts("")
	if err != nil {
		t.Fatalf("SessionFacts: %v", err)
	}
	found := false
	for _, f := range facts {
		if f.SessionID != "p9" {
			continue
		}
		found = true
		if len(f.Asks) != 1 {
			t.Fatalf("p9 应有 1 次提问，得到 %d", len(f.Asks))
		}
		if f.Asks[0].Answered {
			t.Error("不认识的结果形态不该被当成已回答")
		}
	}
	if !found {
		t.Fatal("应有会话 p9")
	}
}

// TestSessionFactsLastStepAndErrors 覆盖：最后一步的 kind/title、错误条数、
// 最后一个用户输入的 seq 都要如实取到。
func TestSessionFactsLastStepAndErrors(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		{Harness: "pi", SessionID: "e1", Kind: model.KindTurnStart, Title: "user", SourceKey: "t:0"},
		{Harness: "pi", SessionID: "e1", Kind: model.KindError, Title: "模型调用失败：429", SourceKey: "t:1"},
		{Harness: "pi", SessionID: "e1", Kind: model.KindTurnStart, Title: "user", SourceKey: "t:2"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	facts, err := s.SessionFacts("")
	if err != nil {
		t.Fatalf("SessionFacts: %v", err)
	}
	for _, f := range facts {
		if f.SessionID != "e1" {
			continue
		}
		if f.LastKind != model.KindTurnStart {
			t.Errorf("最后一步 kind = %q，期望 %q", f.LastKind, model.KindTurnStart)
		}
		if f.ErrorCount != 1 {
			t.Errorf("错误条数 = %d，期望 1", f.ErrorCount)
		}
		if f.LastUserTurnSeq == 0 {
			t.Error("应取到最后一个用户输入的 seq")
		}
	}
}

// contains 是 strings.Contains 的本地副本（测试里少引一个包）。
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
