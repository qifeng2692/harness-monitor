package stats

import (
	"testing"

	"github.com/qifeng2692/harness-monitor/internal/store"
	"github.com/qifeng2692/harness-monitor/internal/usage"
)

// codexUsage 造一条 codex 的用量记录。
//
// 注意 codex 的口径：**input_tokens 本身含 cached_input_tokens**（实测合计 =
// 输入 + 输出，且输入的 97.4% 都是缓存命中）。所以要表达「200 个新输入 + 800 个
// 缓存命中 + 100 个输出」这份工作量，codex 的 input 要写 1000（=200+800）。
func codexUsage(freshInput, cached, output int64) store.UsageRow {
	input := freshInput + cached
	return store.UsageRow{
		Harness: "codex",
		Title:   "token_count",
		Meta: `{"total_token_usage":{"input_tokens":` + itoa(input) + `,"cached_input_tokens":` + itoa(cached) +
			`,"output_tokens":` + itoa(output) + `,"total_tokens":` + itoa(input+output) + `},` +
			`"last_token_usage":{"input_tokens":` + itoa(input) + `,"cached_input_tokens":` + itoa(cached) +
			`,"output_tokens":` + itoa(output) + `,"total_tokens":` + itoa(input+output) + `}}`,
	}
}

// piUsage 造一条 pi 的用量记录（合计=输入+缓存+输出，缓存单列且不含在输入里）。
func piUsage(freshInput, cacheRead, output int64) store.UsageRow {
	return store.UsageRow{
		Harness: "pi",
		Title:   "glm",
		Meta: `{"input_tokens":` + itoa(freshInput) + `,"cache_read":` + itoa(cacheRead) +
			`,"output_tokens":` + itoa(output) + `,"total_tokens":` + itoa(freshInput+cacheRead+output) + `}`,
	}
}

// hermesUsage 造一条 hermes 的用量记录（无 total 字段，由输入+输出补齐；缓存单列）。
func hermesUsage(freshInput, cacheRead, output int64) store.UsageRow {
	return store.UsageRow{
		Harness: "hermes",
		Title:   "glm",
		Meta: `{"input_tokens":` + itoa(freshInput) + `,"cache_read_tokens":` + itoa(cacheRead) +
			`,"output_tokens":` + itoa(output) + `,"reasoning_tokens":0}`,
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}

// TestProcessedTokensUnifiesCacheAcrossHarnesses 是本包最要紧的一条。
//
// 各家「合计 token」的口径不同：codex 的 input_tokens 本身含 cached_input_tokens、
// pi 的 total_tokens = 输入+缓存+输出，而 hermes 的合计只到「输入+输出」、缓存读
// 是单列的另一项。直接相加得到的跨家总数不是同一把尺子（缓存读会重复算或漏算）。
//
// 统一口径：「处理 token」= 输入 + 缓存读 + 输出。三家表达的是**同一份工作量**
// （200 新输入 + 800 缓存命中 + 100 输出），处理 token 必须相等。
func TestProcessedTokensUnifiesCacheAcrossHarnesses(t *testing.T) {
	in := Input{
		Harnesses: []store.HarnessStats{
			{Harness: "codex", Sessions: 1, Events: 3},
			{Harness: "pi", Sessions: 1, Events: 3},
			{Harness: "hermes", Sessions: 1, Events: 3},
		},
		UsageRows: []store.UsageRow{
			codexUsage(200, 800, 100),
			piUsage(200, 800, 100),
			hermesUsage(200, 800, 100),
		},
	}
	rep := Build(in)

	want := int64(1100) // 200 新输入 + 800 缓存 + 100 输出
	for _, h := range rep.Harnesses {
		if h.ProcessedTokens != want {
			t.Errorf("%s 的处理 token = %d，期望 %d（输入+缓存+输出，统一口径）",
				h.Name, h.ProcessedTokens, want)
		}
	}
	if rep.Totals.ProcessedTokens != want*3 {
		t.Errorf("合计处理 token = %d，期望 %d", rep.Totals.ProcessedTokens, want*3)
	}
}

// TestFreshInputExcludesCacheHit 验证「新输入」口径：codex 的输入字段含缓存，
// 要扣掉；其余家的缓存读本就是单列的，输入不用扣。
func TestFreshInputExcludesCacheHit(t *testing.T) {
	in := Input{
		Harnesses: []store.HarnessStats{
			{Harness: "codex", Sessions: 1},
			{Harness: "pi", Sessions: 1},
			{Harness: "hermes", Sessions: 1},
		},
		UsageRows: []store.UsageRow{
			codexUsage(200, 800, 100),
			piUsage(200, 800, 100),
			hermesUsage(200, 800, 100),
		},
	}
	rep := Build(in)

	want := map[string]int64{
		"codex":  200, // codex 的输入 1000 含 800 缓存 → 新输入 200
		"pi":     200,
		"hermes": 200,
	}
	for _, h := range rep.Harnesses {
		if h.FreshInput != want[h.Name] {
			t.Errorf("%s 的新输入 = %d，期望 %d", h.Name, h.FreshInput, want[h.Name])
		}
	}
}

// TestBreakdownSumsToTotal 保证报告里展示的三项之和恒等于「处理 token」总数。
//
// 这条不变量是报告可信的前提：总数与拆分数对不上，读者就不知道该信哪个。
// 之前用各家自报的合计做总数时，codex 会翻车（它的输入字段已含缓存，
// 「输入+缓存+输出」比总数大了一个缓存量）。
func TestBreakdownSumsToTotal(t *testing.T) {
	in := Input{
		Harnesses: []store.HarnessStats{
			{Harness: "codex", Sessions: 1},
			{Harness: "pi", Sessions: 1},
			{Harness: "hermes", Sessions: 1},
			{Harness: "magpie", Sessions: 1},
		},
		UsageRows: []store.UsageRow{
			codexUsage(200, 800, 100),
			piUsage(200, 800, 100),
			hermesUsage(200, 800, 100),
			{Harness: "magpie", Title: "glm", Meta: `{"in":200,"out":100,"status":200}`},
		},
	}
	rep := Build(in)

	tot := rep.Totals
	if got := tot.FreshInput + tot.CacheRead + tot.OutputTokens; got != tot.ProcessedTokens {
		t.Errorf("三项之和 %d ≠ 处理 token %d（报告里的拆分数对不上总数）", got, tot.ProcessedTokens)
	}
	// 逐家也要成立。
	for _, h := range rep.Harnesses {
		if got := h.FreshInput + h.Usage.CacheRead + h.Usage.OutputTokens; got != h.ProcessedTokens {
			t.Errorf("%s 三项之和 %d ≠ 处理 token %d", h.Name, got, h.ProcessedTokens)
		}
	}
}

// TestToolFailureJudgementSeparatesUnjudged 验证「判不了」与「没失败」分开报：
// 带 is_error 键的行才算判得了；不带信号的失败不计入失败数。
func TestToolFailureJudgementSeparatesUnjudged(t *testing.T) {
	in := Input{
		Harnesses: []store.HarnessStats{
			{Harness: "hermes", Sessions: 1, ToolResults: 10},
			{Harness: "codex", Sessions: 1, ToolResults: 5},
		},
		Tools: []store.ToolStat{
			{Harness: "hermes", Tool: "terminal", Calls: 8, Failed: 3, Judged: 6},
			{Harness: "codex", Tool: "exec_command", Calls: 5},
		},
	}
	rep := Build(in)

	byName := map[string]Harness{}
	for _, h := range rep.Harnesses {
		byName[h.Name] = h
	}

	hermes := byName["hermes"]
	if hermes.FailedTools != 3 {
		t.Errorf("hermes 失败数 = %d，期望 3", hermes.FailedTools)
	}
	if hermes.JudgedTools != 6 {
		t.Errorf("hermes 判定数 = %d，期望 6", hermes.JudgedTools)
	}
	// 10 条工具结果里只有 6 条判得了 → 4 条判不了（不是「成功」）。
	if hermes.Unjudged != 4 {
		t.Errorf("hermes 判不了 = %d，期望 4", hermes.Unjudged)
	}

	codex := byName["codex"]
	if codex.FailedTools != 0 || codex.JudgedTools != 0 {
		t.Errorf("codex 不该有失败判定，得到 %d/%d", codex.FailedTools, codex.JudgedTools)
	}
	if codex.Unjudged != 5 {
		t.Errorf("codex 判不了 = %d，期望 5（工具结果不带失败信号）", codex.Unjudged)
	}
}

// TestToolsSortedByCalls 验证工具按调用数降序（热点在前）。
func TestToolsSortedByCalls(t *testing.T) {
	in := Input{
		Harnesses: []store.HarnessStats{{Harness: "hermes", Sessions: 1}},
		Tools: []store.ToolStat{
			{Harness: "hermes", Tool: "read_file", Calls: 10},
			{Harness: "hermes", Tool: "terminal", Calls: 100},
			{Harness: "hermes", Tool: "patch", Calls: 50},
		},
	}
	rep := Build(in)
	if len(rep.Tools) != 3 {
		t.Fatalf("应有 3 个工具，得到 %d", len(rep.Tools))
	}
	want := []string{"terminal", "patch", "read_file"}
	for i, w := range want {
		if rep.Tools[i].Name != w {
			t.Errorf("第 %d 个工具 = %s，期望 %s", i, rep.Tools[i].Name, w)
		}
	}
}

// TestFailuresSortedByCount 验证失败原因按次数降序。
func TestFailuresSortedByCount(t *testing.T) {
	in := Input{
		Harnesses: []store.HarnessStats{{Harness: "pi", Sessions: 1}},
		Failures: []store.ErrorStat{
			{Harness: "pi", Title: "偶发", Count: 3},
			{Harness: "pi", Title: "高频", Count: 50},
		},
	}
	rep := Build(in)
	if len(rep.Failures) != 2 || rep.Failures[0].Title != "高频" {
		t.Errorf("失败原因应按次数降序，得到 %+v", rep.Failures)
	}
}

// TestEmptyInput 不 panic，报告为空。
func TestEmptyInput(t *testing.T) {
	rep := Build(Input{})
	if len(rep.Harnesses) != 0 || len(rep.Tools) != 0 || len(rep.Failures) != 0 {
		t.Errorf("空输入应得到空报告，得到 %+v", rep)
	}
	if rep.Totals.ProcessedTokens != 0 {
		t.Errorf("空输入合计应为 0，得到 %d", rep.Totals.ProcessedTokens)
	}
}

// TestUsageCacheFlags 验证各家口径标记正确（CacheInTotal / InputIncludesCache）。
func TestUsageCacheFlags(t *testing.T) {
	cases := []struct {
		harness string
		cacheIn bool
		inputIn bool
	}{
		{"codex", true, true},
		{"pi", true, false},
		{"hermes", false, false},
		{"claude", false, false},
	}
	for _, c := range cases {
		u := usage.Summarize(c.harness, nil)
		if u.CacheInTotal != c.cacheIn {
			t.Errorf("%s 的 CacheInTotal = %v，期望 %v", c.harness, u.CacheInTotal, c.cacheIn)
		}
		if u.InputIncludesCache != c.inputIn {
			t.Errorf("%s 的 InputIncludesCache = %v，期望 %v", c.harness, u.InputIncludesCache, c.inputIn)
		}
	}
}
