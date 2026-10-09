package collect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

// collectFileInto 把一个 JSONL 文件跑一遍采集器（全量），返回该库里的全部事件。
func collectFileInto(t *testing.T, harness string, dir, name, content string) []model.Event {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("写入样本: %v", err)
	}
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := CollectIncremental(harness, s); err != nil {
		t.Fatalf("采集 %s: %v", harness, err)
	}
	sessions, err := s.ListSessions(harness)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var evs []model.Event
	for _, sess := range sessions {
		got, err := s.ListEvents(harness, sess.SessionID)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		evs = append(evs, got...)
	}
	return evs
}

// usageEvents 过滤出 llm.usage 事件。
func usageEvents(evs []model.Event) []model.Event {
	var out []model.Event
	for _, e := range evs {
		if e.Kind == model.KindLLMUsage {
			out = append(out, e)
		}
	}
	return out
}

// TestClaudeCostStateUsage 验证 claude 的 cost-state 行被采成一条 llm.usage：
// 分模型 token 汇总、费用、模型名进 title，空账行被跳过，且重复采集幂等。
func TestClaudeCostStateUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", dir)

	line := `{"type":"cost-state","sessionId":"s-1","totalCostUSD":0.025302,` +
		`"totalDuration":5290,"startTime":1791175434200,"modelUsage":{` +
		`"zcode/GLM-5.3[1m]":{"inputTokens":2862,"outputTokens":16,"thinkingTokens":7,` +
		`"cacheReadInputTokens":21184,"cacheCreationInputTokens":0,"webSearchRequests":0,"costUSD":0.025302}}}`
	// 第二行是实测存在的空账收尾（无模型调用）——不应产出事件。
	empty := `{"type":"cost-state","sessionId":"s-2","totalCostUSD":0,"modelUsage":{}}`
	evs := collectFileInto(t, "claude", dir, "s-1.jsonl", line+"\n"+empty+"\n")

	us := usageEvents(evs)
	if len(us) != 1 {
		t.Fatalf("应采到 1 条 llm.usage，得到 %d（事件总数 %d）", len(us), len(evs))
	}
	u := us[0]
	if u.Title != "zcode/GLM-5.3[1m]" {
		t.Errorf("title 应为模型名，得到 %q", u.Title)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(u.Meta), &m); err != nil {
		t.Fatalf("meta 非 JSON: %v (%s)", err, u.Meta)
	}
	if got := m["input_tokens"]; got != float64(2862) {
		t.Errorf("input_tokens = %v，期望 2862", got)
	}
	if got := m["cache_read_tokens"]; got != float64(21184) {
		t.Errorf("cache_read_tokens = %v，期望 21184", got)
	}
	if got := m["estimated_cost_usd"]; got != 0.025302 {
		t.Errorf("estimated_cost_usd = %v，期望 0.025302", got)
	}
	if m["source"] != "cost-state" {
		t.Errorf("source = %v，期望 cost-state", m["source"])
	}

	// 幂等：再采一次不产生新事件。
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "hm2.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	defer s.Close()
	first, err := CollectIncremental("claude", s)
	if err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	second, err := CollectIncremental("claude", s)
	if err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	if first == 0 || second != 0 {
		t.Errorf("增量幂等失败：first=%d second=%d", first, second)
	}
}

// TestPiMessageUsage 验证 pi 的 assistant.message.usage 被采成 llm.usage，
// 且与同消息产出的 llm.response 各自独立、source_key 不冲突（都会入库）。
func TestPiMessageUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_SESSIONS_DIR", dir)

	line := `{"type":"message","id":"m1","timestamp":"2026-09-14T02:40:52.623Z",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"PONG"}],` +
		`"api":"openai-completions","provider":"cc-switch-deep-seek","model":"deepseek-flash",` +
		`"usage":{"input":46299,"output":49,"cacheRead":256,"cacheWrite":0,"reasoning":0,` +
		`"totalTokens":46604,"cost":{"input":0.001,"output":0.002,"cacheRead":0,"cacheWrite":0,"total":0.003}}}}`
	evs := collectFileInto(t, "pi", dir, "2026-09-14T02-40-40-193Z_01a09dc9.jsonl", line+"\n")

	us := usageEvents(evs)
	if len(us) != 1 {
		t.Fatalf("应采到 1 条 llm.usage，得到 %d（事件总数 %d）", len(us), len(evs))
	}
	u := us[0]
	if u.Title != "deepseek-flash" {
		t.Errorf("title 应为模型名，得到 %q", u.Title)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(u.Meta), &m); err != nil {
		t.Fatalf("meta 非 JSON: %v (%s)", err, u.Meta)
	}
	if got := m["input_tokens"]; got != float64(46299) {
		t.Errorf("input_tokens = %v，期望 46299", got)
	}
	if got := m["total_tokens"]; got != float64(46604) {
		t.Errorf("total_tokens = %v，期望 46604", got)
	}
	if got := m["cost_usd"]; got != 0.003 {
		t.Errorf("cost_usd = %v，期望 0.003", got)
	}

	// llm.response 必须仍在（usage 事件不能顶掉正文事件）。
	var hasResponse bool
	for _, e := range evs {
		if e.Kind == model.KindLLMResponse && e.Content == "PONG" {
			hasResponse = true
		}
	}
	if !hasResponse {
		t.Errorf("llm.response 丢失：事件 kind 分布 = %v", kindsOf(evs))
	}
}

// TestPiUsageAllZeroSkipped 全零用量不产生噪声条目。
func TestPiUsageAllZeroSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_SESSIONS_DIR", dir)

	line := `{"type":"message","id":"m1","timestamp":"2026-09-14T02:40:52.623Z",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"reasoning":0,"totalTokens":0}}}`
	evs := collectFileInto(t, "pi", dir, "2026-09-14T02-40-40-193Z_01a09dc9.jsonl", line+"\n")
	if us := usageEvents(evs); len(us) != 0 {
		t.Errorf("全零用量应被跳过，得到 %d 条：%v", len(us), kindsOf(us))
	}
}

func kindsOf(evs []model.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}
