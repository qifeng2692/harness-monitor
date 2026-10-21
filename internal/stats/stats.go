// Package stats 把库里取出的原始行汇总成一份「跨会话统计报告」。
//
// 为什么需要它：harness-monitor 此前只有**单会话**视角（GUI 的三级导航、hm show、
// hm export 都是看一个会话）。同类工具（sniffly、token-dashboard）的首页都是
// **跨会话概览**——「我这段时间用了多少 token、哪些工具最常用、哪里老是失败」
// 一眼看完。想回答这三个问题，此前只能自己导出会话再手算。
//
// 本包是纯函数：输入是 store 取出的行，输出是一份报告结构，不碰数据库也不碰
// 前端，因此可单测、可被 GUI 与命令行共用。
//
// 口径纪律（与 internal/usage 一致）：
//   - 用量按各 harness 自己的语义合计（codex 取 last_token_usage 增量，其余逐条相加）；
//   - 失败只认各家自己上报的结构化字段（meta.is_error / error 事件），不猜正文；
//   - 「判不了」与「没失败」分开报，不把前者算成后者。
package stats

import (
	"sort"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
	"github.com/qifeng2692/harness-monitor/internal/usage"
)

// Harness 是一家 harness 的统计汇总。
type Harness struct {
	Name        string
	Sessions    int64
	Events      int64
	ToolCalls   int64
	ToolResults int64
	ErrorEvents int64

	// Usage 是该 harness 全部用量事件的合计（口径见 internal/usage）。
	Usage usage.Summary

	// ProcessedTokens 是「模型处理过的 token 总量」，**跨 harness 可比**。
	//
	// 为什么要另算：各家的「合计 token」口径不同——codex 的 input_tokens 本身
	// 含 cached_input_tokens、pi 的 total_tokens = 输入+缓存+输出，而 hermes/claude
	// 的合计只到「输入+输出」、缓存读是单列的另一项。直接相加得到的总数在
	// 「有的家含缓存、有的不含」，不是同一把尺子。这里统一到「输入+缓存+输出」。
	ProcessedTokens int64

	// FreshInput 是「不含缓存的新输入 token」——真实的新增上下文，跨 harness 可比。
	// codex 的输入字段含缓存，要扣掉；其余家的缓存读本就是单列的，不用扣。
	FreshInput int64

	// FailedTools 是工具结果里判定为失败的条数；JudgedTools 是「判得了」的条数。
	// Unjudged = ToolResults - JudgedTools，是无法判定的条数（工具结果不带失败信号）。
	FailedTools int64
	JudgedTools int64
	Unjudged   int64
}

// Tool 是某个 harness 某个工具的调用/失败统计。
type Tool struct {
	Harness string
	Name    string
	Calls   int64
	Failed  int64
	Judged  int64
}

// Failure 是一条失败原因的聚合。
type Failure struct {
	Harness string
	Title   string
	Count   int64
	Last    time.Time
}

// Totals 是全库合计。
type Totals struct {
	Harnesses   int
	Sessions    int64
	Events      int64
	ToolCalls   int64
	ToolResults int64
	ErrorEvents int64

	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	TotalTokens  int64

	// ProcessedTokens 是「模型处理过的 token 总量」（输入+缓存+输出），跨 harness 可比。
	// 各家自报的「合计」口径不一（有的含缓存、有的不含），这一项是统一尺子。
	ProcessedTokens int64
	// FreshInput 是「不含缓存的新输入 token」——真实新增上下文，跨 harness 可比。
	FreshInput int64

	CostUSD      float64
	CostKnown    bool

	FailedCalls  int64 // 用量事件里的非 200 状态（限流/超时）
	FailedTools  int64
	JudgedTools  int64
	Unjudged     int64
}

// Report 是一份完整的统计报告。
type Report struct {
	Harnesses []Harness
	Tools     []Tool // 按调用数降序
	Failures  []Failure
	Totals    Totals
}

// processedTokens 算出「模型处理过的 token 总量」= 新输入 + 缓存读 + 输出。
//
// 为什么要自己算而不是直接用各家自报的合计：各家口径不同（usage.CacheInTotal
// 说明详情）——codex/pi 的合计已含缓存读，hermes/claude/magpie 的合计不含。
// 统一成「新输入 + 缓存读 + 输出」之后，跨 harness 相加才是同一把尺子，
// 而且这一项**恒等于报告里展示的三项之和**（不会出现总数与拆分数对不上）。
//
// 推理 token 不单列：pi 的 output_tokens 已含推理部分（实测 pi 的
// total_tokens = 输入+缓存+输出），再单列会重复计。
func processedTokens(u usage.Summary) int64 {
	return freshInput(u) + u.CacheRead + u.OutputTokens
}

// freshInput 算出「不含缓存的新输入 token」。
//
// codex 的输入字段本身含缓存（实测 97.4% 的输入都是缓存命中），要扣掉；
// 其余家的缓存读是单列的另一项，输入本来就是新的。
func freshInput(u usage.Summary) int64 {
	in := u.InputTokens
	if u.InputIncludesCache {
		in -= u.CacheRead
	}
	if in < 0 {
		return 0
	}
	return in
}

// Input 是 Build 的输入（全部来自 store 的取数层）。
type Input struct {
	Harnesses []store.HarnessStats
	UsageRows []store.UsageRow
	Tools     []store.ToolStat
	Failures  []store.ErrorStat
}

// Build 汇总成报告。
func Build(in Input) Report {
	rep := Report{
		Harnesses: []Harness{},
		Tools:     []Tool{},
		Failures:  []Failure{},
	}

	// 用量：按 harness 归拢成事件，再交给 internal/usage 按各家语义合计。
	usageByHarness := map[string][]model.Event{}
	for _, r := range in.UsageRows {
		usageByHarness[r.Harness] = append(usageByHarness[r.Harness], model.Event{
			Kind:  model.KindLLMUsage,
			Title: r.Title,
			Meta:  r.Meta,
		})
	}

	// 工具失败判定：按 harness 归拢。
	failByHarness := map[string]int64{}
	judgedByHarness := map[string]int64{}
	for _, t := range in.Tools {
		failByHarness[t.Harness] += t.Failed
		judgedByHarness[t.Harness] += t.Judged
	}

	for _, h := range in.Harnesses {
		hs := Harness{
			Name:        h.Harness,
			Sessions:    h.Sessions,
			Events:      h.Events,
			ToolCalls:   h.ToolCalls,
			ToolResults: h.ToolResults,
			ErrorEvents: h.ErrorEvents,
			Usage:       usage.Summarize(h.Harness, usageByHarness[h.Harness]),
			FailedTools: failByHarness[h.Harness],
			JudgedTools: judgedByHarness[h.Harness],
		}
		hs.ProcessedTokens = processedTokens(hs.Usage)
		hs.FreshInput = freshInput(hs.Usage)
		if hs.ToolResults > hs.JudgedTools {
			hs.Unjudged = hs.ToolResults - hs.JudgedTools
		}
		rep.Harnesses = append(rep.Harnesses, hs)

		rep.Totals.Harnesses++
		rep.Totals.Sessions += h.Sessions
		rep.Totals.Events += h.Events
		rep.Totals.ToolCalls += h.ToolCalls
		rep.Totals.ToolResults += h.ToolResults
		rep.Totals.ErrorEvents += h.ErrorEvents
		rep.Totals.InputTokens += hs.Usage.InputTokens
		rep.Totals.OutputTokens += hs.Usage.OutputTokens
		rep.Totals.CacheRead += hs.Usage.CacheRead
		rep.Totals.TotalTokens += hs.Usage.TotalTokens
		rep.Totals.ProcessedTokens += hs.ProcessedTokens
		rep.Totals.FreshInput += hs.FreshInput
		rep.Totals.CostUSD += hs.Usage.CostUSD
		rep.Totals.FailedCalls += hs.Usage.FailedCalls
		rep.Totals.FailedTools += hs.FailedTools
		rep.Totals.JudgedTools += hs.JudgedTools
		rep.Totals.Unjudged += hs.Unjudged
		if hs.Usage.CostKnown {
			rep.Totals.CostKnown = true
		}
	}

	for _, t := range in.Tools {
		rep.Tools = append(rep.Tools, Tool{
			Harness: t.Harness,
			Name:    t.Tool,
			Calls:   t.Calls,
			Failed:  t.Failed,
			Judged:  t.Judged,
		})
	}
	sort.SliceStable(rep.Tools, func(i, j int) bool {
		if rep.Tools[i].Calls != rep.Tools[j].Calls {
			return rep.Tools[i].Calls > rep.Tools[j].Calls
		}
		if rep.Tools[i].Harness != rep.Tools[j].Harness {
			return rep.Tools[i].Harness < rep.Tools[j].Harness
		}
		return rep.Tools[i].Name < rep.Tools[j].Name
	})

	for _, f := range in.Failures {
		rep.Failures = append(rep.Failures, Failure{
			Harness: f.Harness,
			Title:   f.Title,
			Count:   f.Count,
			Last:    f.Time,
		})
	}
	sort.SliceStable(rep.Failures, func(i, j int) bool {
		if rep.Failures[i].Count != rep.Failures[j].Count {
			return rep.Failures[i].Count > rep.Failures[j].Count
		}
		return rep.Failures[i].Harness < rep.Failures[j].Harness
	})

	return rep
}
