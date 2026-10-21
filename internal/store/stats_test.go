package store

import (
	"testing"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// TestStatsToolStatsFailureJudgement 覆盖统计取数层最要紧的一条：失败判定只看
// 各家自己上报的结构化字段（meta.is_error），不猜正文。
//
// 真实库形态（2026-10-21 实测）：
//   - hermes：成功也写 meta.is_error=false，失败写 true → judged = 成功+失败；
//   - pi    ：只在失败时写 meta.is_error=true → judged = failed；
//   - codex/claude：工具结果不带任何失败信号 → 不出现在失败判定里。
func TestStatsToolStatsFailureJudgement(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		// hermes：一次成功、一次失败、一次「判不了」（正文非 JSON，没写 is_error）。
		{Harness: "hermes", SessionID: "h1", Kind: model.KindToolCall, Title: "terminal",
			Meta: `{"tool_name":"terminal"}`, SourceKey: "t:h:0"},
		{Harness: "hermes", SessionID: "h1", Kind: model.KindToolResult, Title: "terminal",
			Meta: `{"tool_name":"terminal","is_error":false,"exit_code":0}`, SourceKey: "t:h:1"},
		{Harness: "hermes", SessionID: "h1", Kind: model.KindToolCall, Title: "terminal",
			Meta: `{"tool_name":"terminal"}`, SourceKey: "t:h:2"},
		{Harness: "hermes", SessionID: "h1", Kind: model.KindToolResult, Title: "terminal",
			Meta: `{"tool_name":"terminal","is_error":true,"exit_code":1}`, SourceKey: "t:h:3"},
		{Harness: "hermes", SessionID: "h1", Kind: model.KindToolResult, Title: "terminal",
			Meta: `{"tool_name":"terminal"}`, SourceKey: "t:h:4"},
		// pi：一次失败（只有失败才写 is_error）。
		{Harness: "pi", SessionID: "p1", Kind: model.KindToolCall, Title: "bash",
			Meta: `{"tool_name":"bash"}`, SourceKey: "t:p:0"},
		{Harness: "pi", SessionID: "p1", Kind: model.KindToolResult, Title: "tool result",
			Meta: `{"tool_name":"bash","is_error":true}`, SourceKey: "t:p:1"},
		// codex：工具结果不带失败信号。
		{Harness: "codex", SessionID: "c1", Kind: model.KindToolCall, Title: "exec_command",
			Meta: `{}`, SourceKey: "t:c:0"},
		{Harness: "codex", SessionID: "c1", Kind: model.KindToolResult, Title: "tool result",
			Meta: `{"call_id":"x"}`, SourceKey: "t:c:1"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	stats, err := s.StatsToolStats()
	if err != nil {
		t.Fatalf("StatsToolStats: %v", err)
	}
	byKey := map[string]ToolStat{}
	for _, st := range stats {
		byKey[st.Harness+"/"+st.Tool] = st
	}

	hermes := byKey["hermes/terminal"]
	if hermes.Calls != 2 {
		t.Errorf("hermes terminal 调用数 = %d，期望 2", hermes.Calls)
	}
	if hermes.Failed != 1 {
		t.Errorf("hermes terminal 失败数 = %d，期望 1", hermes.Failed)
	}
	// 成功(false) + 失败(true) 都带 is_error 键 → judged=2；第三条没写键 → 不计入。
	if hermes.Judged != 2 {
		t.Errorf("hermes terminal 判定数 = %d，期望 2（只有带 is_error 键的才算判得了）", hermes.Judged)
	}

	pi := byKey["pi/bash"]
	if pi.Failed != 1 || pi.Judged != 1 {
		t.Errorf("pi bash 失败/判定 = %d/%d，期望 1/1（pi 只在失败时写 is_error）", pi.Failed, pi.Judged)
	}

	codex := byKey["codex/exec_command"]
	if codex.Calls != 1 {
		t.Errorf("codex exec_command 调用数 = %d，期望 1", codex.Calls)
	}
	if codex.Failed != 0 || codex.Judged != 0 {
		t.Errorf("codex 不该有失败判定，得到 %d/%d", codex.Failed, codex.Judged)
	}
}

// TestStatsHarnessStats 覆盖规模统计：会话数按 DISTINCT 算，事件按 kind 分桶。
func TestStatsHarnessStats(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		{Harness: "hermes", SessionID: "s1", Kind: model.KindTurnStart, Title: "输入", SourceKey: "x:0"},
		{Harness: "hermes", SessionID: "s1", Kind: model.KindToolCall, Title: "terminal", SourceKey: "x:1"},
		{Harness: "hermes", SessionID: "s2", Kind: model.KindTurnStart, Title: "输入", SourceKey: "x:2"},
		{Harness: "hermes", SessionID: "s2", Kind: model.KindError, Title: "模型调用失败", SourceKey: "x:3"},
		{Harness: "pi", SessionID: "p1", Kind: model.KindTurnStart, Title: "输入", SourceKey: "x:4"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	stats, err := s.StatsHarnessStats()
	if err != nil {
		t.Fatalf("StatsHarnessStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("应有 2 行，得到 %d", len(stats))
	}
	hermes := stats[0]
	if hermes.Harness != "hermes" {
		t.Fatalf("第一行应是 hermes，得到 %s", hermes.Harness)
	}
	if hermes.Sessions != 2 {
		t.Errorf("hermes 会话数 = %d，期望 2", hermes.Sessions)
	}
	if hermes.Events != 4 {
		t.Errorf("hermes 事件数 = %d，期望 4", hermes.Events)
	}
	if hermes.ToolCalls != 1 {
		t.Errorf("hermes 工具调用 = %d，期望 1", hermes.ToolCalls)
	}
	if hermes.ErrorEvents != 1 {
		t.Errorf("hermes 错误事件 = %d，期望 1", hermes.ErrorEvents)
	}
}

// TestStatsErrorStats 覆盖失败原因聚合（按 harness/标题归并，次数降序）。
func TestStatsErrorStats(t *testing.T) {
	s := newTestStore(t)

	events := []model.Event{
		{Harness: "pi", SessionID: "p1", Kind: model.KindError, Title: "模型调用失败：429", SourceKey: "e:0"},
		{Harness: "pi", SessionID: "p1", Kind: model.KindError, Title: "模型调用失败：429", SourceKey: "e:1"},
		{Harness: "pi", SessionID: "p1", Kind: model.KindError, Title: "模型调用失败：401", SourceKey: "e:2"},
		{Harness: "claude", SessionID: "c1", Kind: model.KindError, Title: "请求失败（HTTP 502）", SourceKey: "e:3"},
	}
	if _, _, err := s.InsertEvents(events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	fails, err := s.StatsErrorStats()
	if err != nil {
		t.Fatalf("StatsErrorStats: %v", err)
	}
	if len(fails) != 3 {
		t.Fatalf("应有 3 组，得到 %d", len(fails))
	}
	if fails[0].Harness != "pi" || fails[0].Title != "模型调用失败：429" || fails[0].Count != 2 {
		t.Errorf("第一组应是 pi/429 ×2，得到 %+v", fails[0])
	}
}
