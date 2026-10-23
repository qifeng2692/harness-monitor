package sessionstate

import (
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/store"
)

var base = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

func TestClassifyWaitingWhenAskUnanswered(t *testing.T) {
	f := store.SessionFact{
		Harness:   "hermes",
		SessionID: "s1",
		LastKind:  "tool.call",
		LastTitle: "clarify",
		LastTime:  base.Add(-2 * time.Minute),
		Asks: []store.SessionAsk{{
			Tool:     "clarify",
			Seq:      12,
			Time:     base.Add(-2 * time.Minute),
			Content:  `{"questions":[{"question":"接下来怎么做？"}]}`,
			Answered: false,
		}},
	}
	got := Classify(f, base, DefaultActiveWindow)
	if got.State != StateWaiting {
		t.Fatalf("状态 = %q，期望 %q", got.State, StateWaiting)
	}
	if got.PendingAsk == nil {
		t.Fatal("应带出待回答的问题")
	}
	if got.Reason == "" {
		t.Error("应给出人话原因")
	}
	if got.PendingAsk.Question != "接下来怎么做？" {
		t.Errorf("问题文本 = %q", got.PendingAsk.Question)
	}
}

func TestClassifyNotWaitingWhenAskAnswered(t *testing.T) {
	f := store.SessionFact{
		Harness: "pi", SessionID: "s2",
		LastKind: "llm.usage", LastTime: base.Add(-time.Minute),
		Asks: []store.SessionAsk{{Tool: "ask_user_question", Seq: 5, Answered: true}},
	}
	got := Classify(f, base, DefaultActiveWindow)
	if got.State == StateWaiting {
		t.Fatal("已回答的提问不该算等待")
	}
}

func TestClassifyNotWaitingWhenUserSpokeAfter(t *testing.T) {
	// 用户在提问之后又说过话 → agent 已经往下走了，不算卡住。
	f := store.SessionFact{
		Harness: "pi", SessionID: "s3",
		LastKind: "llm.response", LastTime: base.Add(-time.Minute),
		LastUserTurnSeq: 9,
		Asks:            []store.SessionAsk{{Tool: "ask_user_question", Seq: 5, Answered: false}},
	}
	got := Classify(f, base, DefaultActiveWindow)
	if got.State == StateWaiting {
		t.Fatal("用户之后又说过话，不该算等待")
	}
}

func TestClassifyFailedBeatsWaiting(t *testing.T) {
	f := store.SessionFact{
		Harness: "pi", SessionID: "s4",
		LastKind: "error", LastTitle: "模型调用失败：429", LastTime: base.Add(-time.Minute),
		ErrorCount:      1,
		LastUserTurnSeq: 1,
		Asks:            []store.SessionAsk{{Tool: "ask_user_question", Seq: 5, Answered: false}},
	}
	got := Classify(f, base, DefaultActiveWindow)
	if got.State != StateFailed {
		t.Fatalf("状态 = %q，期望 %q（最后一步是错误）", got.State, StateFailed)
	}
}

func TestClassifyDoneBeatsWaiting(t *testing.T) {
	f := store.SessionFact{
		Harness: "codex", SessionID: "s5",
		LastKind: "turn.end", LastTitle: "turn complete", LastTime: base.Add(-time.Minute),
		LastUserTurnSeq: 1,
		Asks:            []store.SessionAsk{{Tool: "request_user_input", Seq: 5, Answered: false}},
	}
	got := Classify(f, base, DefaultActiveWindow)
	if got.State != StateDone {
		t.Fatalf("状态 = %q，期望 %q（有终态标记）", got.State, StateDone)
	}
}

func TestClassifyActiveVsIdle(t *testing.T) {
	f := store.SessionFact{Harness: "hermes", SessionID: "s6", LastKind: "llm.usage", LastTime: base.Add(-5 * time.Minute)}
	if got := Classify(f, base, DefaultActiveWindow); got.State != StateActive {
		t.Errorf("5 分钟前有动作 → 状态 = %q，期望 %q", got.State, StateActive)
	}
	f2 := store.SessionFact{Harness: "hermes", SessionID: "s7", LastKind: "llm.usage", LastTime: base.Add(-3 * time.Hour)}
	if got := Classify(f2, base, DefaultActiveWindow); got.State != StateIdle {
		t.Errorf("3 小时前有动作 → 状态 = %q，期望 %q", got.State, StateIdle)
	}
}

func TestStateLabelsAreChinese(t *testing.T) {
	want := map[State]string{
		StateWaiting: "等待你",
		StateFailed:  "失败",
		StateDone:    "已完成",
		StateActive:  "进行中",
		StateIdle:    "空闲",
	}
	for st, w := range want {
		if got := st.Label(); got != w {
			t.Errorf("%s.Label() = %q，期望 %q", st, got, w)
		}
	}
}

func TestExtractQuestionFromHarnessShapes(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"hermes clarify", `{"questions":[{"question":"先只读分析行吗？","choices":[]}]}`, "先只读分析行吗？"},
		{"pi ask_user_question", `{"questions":[{"header":"交付方式","options":[],"question":"用哪种交付方式？"}]}`, "用哪种交付方式？"},
		{"codex request_user_input", `{"questions":[{"id":"x","header":"端","question":"先做哪个端？"}]}`, "先做哪个端？"},
		{"claude AskUserQuestion", `{"questions":[{"question":"你说的 Hermes 指哪一个？"}]}`, "你说的 Hermes 指哪一个？"},
		{"codex request_permissions", `{"permissions":{"network":{"enabled":true}},"reason":"装依赖要访问 npm"}`, "装依赖要访问 npm"},
		{"非法 JSON", `{不是 JSON`, ""},
		{"没有问句", `{"foo":1}`, ""},
	}
	for _, c := range cases {
		if got := ExtractQuestion(c.content); got != c.want {
			t.Errorf("%s：ExtractQuestion = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestExtractQuestionNotesMultiple(t *testing.T) {
	content := `{"questions":[{"question":"第一个？"},{"question":"第二个？"}]}`
	got := ExtractQuestion(content)
	if got != "第一个？" {
		t.Errorf("应取第一问，得到 %q", got)
	}
}

func TestSummarizeCountsStates(t *testing.T) {
	ss := []Session{
		{State: StateWaiting},
		{State: StateWaiting},
		{State: StateFailed},
		{State: StateIdle},
	}
	sum := Summarize(ss)
	if sum.Total != 4 {
		t.Errorf("合计 = %d，期望 4", sum.Total)
	}
	if sum.Counts[StateWaiting] != 2 {
		t.Errorf("等待你 = %d，期望 2", sum.Counts[StateWaiting])
	}
	if sum.Counts[StateIdle] != 1 {
		t.Errorf("空闲 = %d，期望 1", sum.Counts[StateIdle])
	}
}
