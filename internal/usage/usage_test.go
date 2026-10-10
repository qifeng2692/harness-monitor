package usage

import (
	"testing"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// usageEvent 造一条 llm.usage 事件。
func usageEvent(title, meta string) model.Event {
	return model.Event{Kind: model.KindLLMUsage, Title: title, Meta: meta}
}

// TestCodexUsesLastIncrementNotCumulativeTotal 是本包最要紧的一条。
// codex 的 token_count 事件同时带两个块：total_token_usage 是**该 rollout 文件的**
// 累计计数器，last_token_usage 是**本次调用**的增量。逐条相加 total 会把用量放大成百上千倍
// （实测单会话 3061 条记录、末值 9.1 亿）；正确做法是取 last 相加。
func TestCodexUsesLastIncrementNotCumulativeTotal(t *testing.T) {
	// 三次调用：total 是累计值（105 → 320 → 755），last 是各自的增量。
	events := []model.Event{
		usageEvent("token_count", `{"total_token_usage":{"input_tokens":100,"cached_input_tokens":10,"output_tokens":5,"reasoning_output_tokens":1,"total_tokens":105},"last_token_usage":{"input_tokens":100,"cached_input_tokens":10,"output_tokens":5,"reasoning_output_tokens":1,"total_tokens":105},"model_context_window":258400}`),
		usageEvent("token_count", `{"total_token_usage":{"input_tokens":300,"cached_input_tokens":40,"output_tokens":20,"reasoning_output_tokens":3,"total_tokens":320},"last_token_usage":{"input_tokens":200,"cached_input_tokens":30,"output_tokens":15,"reasoning_output_tokens":2,"total_tokens":215},"model_context_window":258400}`),
		usageEvent("token_count", `{"total_token_usage":{"input_tokens":700,"cached_input_tokens":90,"output_tokens":55,"reasoning_output_tokens":9,"total_tokens":755},"last_token_usage":{"input_tokens":400,"cached_input_tokens":50,"output_tokens":35,"reasoning_output_tokens":6,"total_tokens":435},"model_context_window":258400}`),
	}
	s := Summarize("codex", events)

	if s.TotalTokens != 755 {
		t.Errorf("TotalTokens = %d，期望 755（三次增量之和）；若为 1180 说明错误地累加了 total 块", s.TotalTokens)
	}
	if s.InputTokens != 700 || s.OutputTokens != 55 {
		t.Errorf("输入/输出 = %d/%d，期望 700/55", s.InputTokens, s.OutputTokens)
	}
	if s.CacheRead != 90 || s.Reasoning != 9 {
		t.Errorf("缓存读/推理 = %d/%d，期望 90/9", s.CacheRead, s.Reasoning)
	}
	if s.Records != 3 {
		t.Errorf("Records = %d，期望 3", s.Records)
	}
}

// TestCodexResumedSessionAcrossFiles 会话被恢复时，新 rollout 文件的累计计数器会继承
// 父文件的值（实测同一会话横跨 19 个文件）。取 last 相加才能如实反映各文件的实际用量；
// 若用 total 的末值或最大值，都会把继承来的上游基数算重或算漏。
func TestCodexResumedSessionAcrossFiles(t *testing.T) {
	events := []model.Event{
		// 第一个文件：末次累计 960。
		usageEvent("token_count", `{"total_token_usage":{"input_tokens":900,"output_tokens":60,"total_tokens":960},"last_token_usage":{"input_tokens":900,"output_tokens":60,"total_tokens":960}}`),
		// 第二个文件（会话恢复）：累计计数器继承自父文件，末次 1088 = 960 + 本次 128。
		usageEvent("token_count", `{"total_token_usage":{"input_tokens":1020,"output_tokens":68,"total_tokens":1088},"last_token_usage":{"input_tokens":120,"output_tokens":8,"total_tokens":128}}`),
	}
	s := Summarize("codex", events)
	// 两次调用实际用量 = 960 + 128 = 1088；若取 total 末值也是 1088，但若有三段以上
	// 就会失真——这里同时断言逐条相加的语义。
	if s.TotalTokens != 1088 {
		t.Errorf("TotalTokens = %d，期望 1088（960 + 128）", s.TotalTokens)
	}
	if s.InputTokens != 1020 || s.OutputTokens != 68 {
		t.Errorf("输入/输出 = %d/%d，期望 1020/68", s.InputTokens, s.OutputTokens)
	}
}

// TestHermesSessionModelRollupSummed hermes 的每条记录是「会话 × 模型」汇总行，
// 多模型时要相加，并且分模型明细要分开列出。
func TestHermesSessionModelRollupSummed(t *testing.T) {
	events := []model.Event{
		usageEvent("glm-5.3-flash", `{"api_call_count":3,"input_tokens":1000,"output_tokens":200,"cache_read_tokens":50,"reasoning_tokens":30,"estimated_cost_usd":0.01,"actual_cost_usd":0}`),
		usageEvent("claude-opus-4.6", `{"api_call_count":2,"input_tokens":400,"output_tokens":100,"cache_read_tokens":20,"reasoning_tokens":10,"estimated_cost_usd":0.02,"actual_cost_usd":0}`),
	}
	s := Summarize("hermes", events)

	if s.InputTokens != 1400 || s.OutputTokens != 300 {
		t.Errorf("输入/输出 = %d/%d，期望 1400/300", s.InputTokens, s.OutputTokens)
	}
	if s.TotalTokens != 1700 {
		t.Errorf("TotalTokens = %d，期望 1700（无 total 字段时用输入+输出补齐）", s.TotalTokens)
	}
	if !s.CostKnown || s.CostUSD != 0.03 {
		t.Errorf("CostKnown/CostUSD = %v/%.4f，期望 true/0.0300", s.CostKnown, s.CostUSD)
	}
	if len(s.Models) != 2 {
		t.Fatalf("Models 数 = %d，期望 2（%+v）", len(s.Models), s.Models)
	}
	// 按 total 降序：glm 1200 > claude 500
	if s.Models[0].Model != "glm-5.3-flash" || s.Models[0].TotalTokens != 1200 {
		t.Errorf("首个模型 = %+v，期望 glm-5.3-flash/1200", s.Models[0])
	}
}

// TestHermesZeroCostIsUnknownNotFree 供应商未回填费用时，hermes 的 estimated/actual
// 都是 0——这是「没报」不是「免费」。必须 CostKnown=false，前端才能显示「费用未知」。
func TestHermesZeroCostIsUnknownNotFree(t *testing.T) {
	events := []model.Event{
		usageEvent("glm-5.3-flash", `{"api_call_count":73,"input_tokens":38179,"output_tokens":20368,"cache_read_tokens":4175936,"reasoning_tokens":2312,"estimated_cost_usd":0,"actual_cost_usd":0}`),
	}
	s := Summarize("hermes", events)

	if s.CostKnown {
		t.Errorf("全零费用应视为「未知」而非「免费」：CostKnown 应为 false，得到 true（CostUSD=%v）", s.CostUSD)
	}
	if s.CostUSD != 0 {
		t.Errorf("CostUSD = %v，期望 0（但仍应标记 CostKnown=false）", s.CostUSD)
	}
	if s.TotalTokens != 58547 {
		t.Errorf("TotalTokens = %d，期望 58547（38179+20368）", s.TotalTokens)
	}
}

// TestMagpiePerCallSummedAndFailuresCounted magpie 是逐次调用记录，相加；
// 非 200 的调用（限流 429 / 超时 499）要单独计数——用量为 0 的失败行也不能丢。
func TestMagpiePerCallSummedAndFailuresCounted(t *testing.T) {
	events := []model.Event{
		usageEvent("glm-5.3-flash 200", `{"in":1000,"out":100,"cache_read":20,"reasoning":10,"ms":900,"status":200,"model":"glm-5.3-flash","provider":"workbuddy"}`),
		usageEvent("glm-5.3-flash 429", `{"in":0,"out":0,"ms":1200,"status":429,"model":"glm-5.3-flash","provider":"zai"}`),
		usageEvent("deepseek-flash 499", `{"in":0,"out":0,"ms":30000,"status":499,"model":"deepseek-flash","provider":"deepseek"}`),
	}
	s := Summarize("magpie", events)

	if s.InputTokens != 1000 || s.OutputTokens != 100 {
		t.Errorf("输入/输出 = %d/%d，期望 1000/100", s.InputTokens, s.OutputTokens)
	}
	if s.FailedCalls != 2 {
		t.Errorf("FailedCalls = %d，期望 2（429 + 499）", s.FailedCalls)
	}
	if s.Records != 3 {
		t.Errorf("Records = %d，期望 3（失败行也是记录）", s.Records)
	}
	if s.CostKnown {
		t.Errorf("magpie 的 meta 无费用字段，CostKnown 应为 false（$0 是「未知」不是「免费」）")
	}
}

// TestClaudeCostStateSummed claude 每会话一条 cost-state 汇总（含分模型明细）。
func TestClaudeCostStateSummed(t *testing.T) {
	events := []model.Event{
		usageEvent("zcode/GLM-5.3[1m]", `{"source":"cost-state","input_tokens":2862,"output_tokens":16,"thinking_tokens":7,"cache_read_tokens":21184,"cache_creation_tokens":0,"estimated_cost_usd":0.025302,"total_duration_ms":5290}`),
	}
	s := Summarize("claude", events)

	if s.TotalTokens != 2878 {
		t.Errorf("TotalTokens = %d，期望 2878（2862+16）", s.TotalTokens)
	}
	if s.Reasoning != 7 {
		t.Errorf("Reasoning = %d，期望 7（claude 的 thinking_tokens 计入推理）", s.Reasoning)
	}
	if !s.CostKnown || s.CostUSD != 0.025302 {
		t.Errorf("CostKnown/CostUSD = %v/%.6f，期望 true/0.025302", s.CostKnown, s.CostUSD)
	}
}

// TestPiPerCallSummed pi 逐次调用带 totalTokens 与 cost。
func TestPiPerCallSummed(t *testing.T) {
	events := []model.Event{
		usageEvent("deepseek-flash", `{"source":"message.usage","input_tokens":46299,"output_tokens":49,"cache_read":256,"cache_write":0,"reasoning":0,"total_tokens":46604,"provider":"cc-switch-deep-seek","model":"deepseek-flash","cost_usd":0.003}`),
		usageEvent("deepseek-flash", `{"source":"message.usage","input_tokens":100,"output_tokens":10,"total_tokens":110,"cost_usd":0.001}`),
	}
	s := Summarize("pi", events)

	if s.TotalTokens != 46714 {
		t.Errorf("TotalTokens = %d，期望 46714（逐次相加）", s.TotalTokens)
	}
	if s.CostUSD != 0.004 {
		t.Errorf("CostUSD = %.4f，期望 0.0040", s.CostUSD)
	}
	if len(s.Models) != 1 || s.Models[0].Records != 2 {
		t.Errorf("分模型应合并为 1 个（deepseek-flash, 2 条），得到 %+v", s.Models)
	}
}

// TestBadMetaReportedNotCounted meta 为空/坏的行要如实报数，不能静默丢弃，
// 也不能把 0 混进合计（实测 codex 库里有 23 条 meta=null 的行）。
func TestBadMetaReportedNotCounted(t *testing.T) {
	events := []model.Event{
		usageEvent("token_count", `null`),
		usageEvent("token_count", ``),
		usageEvent("glm-5.3-flash", `{"in":500,"out":50,"status":200,"model":"glm-5.3-flash"}`),
	}
	s := Summarize("magpie", events)

	if s.Skipped != 2 {
		t.Errorf("Skipped = %d，期望 2（null + 空 meta）", s.Skipped)
	}
	if s.Records != 1 || s.InputTokens != 500 {
		t.Errorf("Records/InputTokens = %d/%d，期望 1/500", s.Records, s.InputTokens)
	}
}

// TestNonUsageEventsIgnored 传入整个会话的事件流时，非 llm.usage 事件不影响合计。
func TestNonUsageEventsIgnored(t *testing.T) {
	events := []model.Event{
		{Kind: model.KindTurnStart, Title: "user", Content: "你好"},
		usageEvent("glm-5.3-flash", `{"in":10,"out":2,"status":200,"model":"glm-5.3-flash"}`),
		{Kind: model.KindToolCall, Title: "terminal", Content: `{"cmd":"ls"}`},
	}
	s := Summarize("magpie", events)
	if s.Records != 1 || s.TotalTokens != 12 {
		t.Errorf("Records/TotalTokens = %d/%d，期望 1/12", s.Records, s.TotalTokens)
	}
}

// TestEmptySession 没有用量记录时返回零值摘要（前端据此不显示汇总条）。
func TestEmptySession(t *testing.T) {
	s := Summarize("hermes", []model.Event{{Kind: model.KindTurnStart, Title: "user"}})
	if s.Records != 0 || s.TotalTokens != 0 || len(s.Models) != 0 {
		t.Errorf("空会话应返回零值摘要，得到 %+v", s)
	}
}
