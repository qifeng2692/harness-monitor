// Package usage 把一个会话的 llm.usage 事件合计成「用量 / 成本摘要」。
//
// 为什么需要它：库里已有上万条 llm.usage 事件（各家 harness 的 token 与费用记录），
// 但它们只是时间线上孤立的步骤——「这个会话烧了多少 token、花了多少钱」读不出来。
// 同类工具（abtop）的看家能力正是用量一览。
//
// 本包存在的核心原因：**各家记录口径不同**，直接相加会算错。实测：
//
//	harness  记录的含义                      合计方式
//	hermes   「会话 × 模型」的汇总行           相加（每条即该模型在整个会话的合计）
//	claude   每会话一条 cost-state 汇总        相加（通常只有一条）
//	pi       每次模型调用的用量                相加
//	magpie   每次模型调用的用量                相加（另带 status，可看限流/失败）
//	codex    每次模型调用的用量                相加，但**必须取 last_token_usage 子块**
//
// codex 是唯一有坑的一家：它的 token_count 事件同时带两个块——
//   - total_token_usage：该 rollout 文件的**累计计数器**（会话被恢复时继承父文件的值，
//     实测同一会话横跨 19 个文件、计数器按文件重置并携带 277 万的上游基数）；
//   - last_token_usage：**本次调用**的用量增量，与 pi/magpie 的逐次记录语义一致。
//
// 因此取 last 逐条相加，得到的是「模型实际处理过的 token」（含恢复会话时重发的上下文，
// 也就是真正计费的部分），且与其余四家口径统一；用 total 相加或取快照都会算错。
//
// 纯函数（不依赖存储/前端），可单测，被 GUI 与命令行共用。
package usage

import (
	"encoding/json"
	"sort"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// ModelUsage 是分模型的用量小计。
type ModelUsage struct {
	Model        string  `json:"model"`
	Records      int64   `json:"records"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CacheRead    int64   `json:"cacheReadTokens"`
	CacheWrite   int64   `json:"cacheWriteTokens"`
	Reasoning    int64   `json:"reasoningTokens"`
	TotalTokens  int64   `json:"totalTokens"`
	CostUSD      float64 `json:"costUsd"`
}

// Summary 是一个会话的用量/成本合计（前端直接渲染）。
type Summary struct {
	Records      int64        `json:"records"`      // 参与合计的用量记录条数
	Skipped      int64        `json:"skipped"`      // 无法解析（meta 为空/坏）而被跳过的条数
	InputTokens  int64        `json:"inputTokens"`  // 输入 token（含/不含缓存读取各家口径不一）
	OutputTokens int64        `json:"outputTokens"` // 输出 token
	CacheRead    int64        `json:"cacheReadTokens"`
	CacheWrite   int64        `json:"cacheWriteTokens"`
	Reasoning    int64        `json:"reasoningTokens"`
	TotalTokens  int64        `json:"totalTokens"`
	CostUSD      float64      `json:"costUsd"`
	CostKnown    bool         `json:"costKnown"`   // 至少一条记录带了费用字段（否则 $0 是「未知」而非「免费」）
	FailedCalls  int64        `json:"failedCalls"` // 非 200 状态的调用数（限流 429 / 超时 499 等）
	Models       []ModelUsage `json:"models"`      // 分模型小计（按 total 降序；无法分辨模型时为空）
}

// Summarize 合计某 harness 某会话的事件流（口径见包注释）。
//
// 传入整个会话的事件也无妨：非 llm.usage 事件被忽略。
func Summarize(harness string, events []model.Event) Summary {
	recs := make([]record, 0, len(events))
	for _, e := range events {
		if e.Kind != model.KindLLMUsage {
			continue
		}
		r, ok := parseRecord(e)
		if !ok {
			// meta 为空/坏：不计入合计，但要如实报数（实测 codex 有 23 条这样的行）。
			recs = append(recs, record{bad: true})
			continue
		}
		recs = append(recs, r)
	}
	return aggregate(harness, recs)
}

// record 是一条用量记录解析后的统一形态（已归一为「本次调用的用量」）。
type record struct {
	bad        bool
	model      string
	in         int64
	out        int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
	total      int64 // 记录里自报的合计（0 = 未提供，由输入输出相加补齐）
	cost       float64
	hasCost    bool
	status     int
	hasStatus  bool
}

// parseRecord 把一条 llm.usage 事件的 meta 解析为统一形态。
// 各 harness 的 meta 字段名不同（见包注释），此处集中适配。
func parseRecord(e model.Event) (record, bool) {
	if e.Meta == "" || e.Meta == "null" {
		return record{}, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(e.Meta), &m); err != nil || len(m) == 0 {
		return record{}, false
	}
	r := record{model: e.Title}

	// codex：token_count 事件同时带两个块——total_token_usage 是**该 rollout 文件的**
	// 累计计数器（会话被恢复时继承父文件的值，按文件重置），last_token_usage 是
	// **本次调用**的用量增量。取 last（与 pi/magpie 的逐次记录语义一致）才能跨文件相加，
	// 且不受计数器继承/重置影响。last 缺失时退回 total。
	if t, ok := m["total_token_usage"].(map[string]any); ok {
		src := t
		if l, ok := m["last_token_usage"].(map[string]any); ok && len(l) > 0 {
			src = l
		}
		r.in = intOf(src["input_tokens"])
		r.out = intOf(src["output_tokens"])
		r.cacheRead = intOf(src["cached_input_tokens"])
		r.cacheWrite = intOf(src["cache_write_input_tokens"])
		r.reasoning = intOf(src["reasoning_output_tokens"])
		r.total = intOf(src["total_tokens"])
		return r, true
	}

	// 通用 token 字段：hermes / claude / pi 用 input_tokens/output_tokens，
	// magpie 用 in/out（两套字段名同时兼容）。
	r.in = firstInt(m, "input_tokens", "in")
	r.out = firstInt(m, "output_tokens", "out")
	r.cacheRead = firstInt(m, "cache_read_tokens", "cache_read", "cached_input_tokens")
	r.cacheWrite = firstInt(m, "cache_creation_tokens", "cache_write", "cache_write_input_tokens")
	r.reasoning = firstInt(m, "reasoning_tokens", "reasoning", "thinking_tokens")
	r.total = firstInt(m, "total_tokens", "totalTokens")

	// 费用：hermes 给 actual/estimated（实测多为 0——供应商未回填，不是「免费」），
	// pi 给 cost_usd，claude 给 estimated_cost_usd。**只有非零值才算「费用已知」**：
	// 全零时 CostKnown=false，前端显示「费用未知」，避免把「没报」误读成「不花钱」。
	if v, ok := m["actual_cost_usd"]; ok {
		if f := floatOf(v); f != 0 {
			r.cost, r.hasCost = f, true
		}
	}
	if !r.hasCost {
		if v, ok := m["estimated_cost_usd"]; ok {
			if f := floatOf(v); f != 0 {
				r.cost, r.hasCost = f, true
			}
		}
	}
	if v, ok := m["cost_usd"]; ok {
		if f := floatOf(v); f != 0 {
			r.cost, r.hasCost = f, true
		}
	}

	if v, ok := m["status"]; ok {
		r.status, r.hasStatus = int(intOf(v)), true
	}
	// 模型名：meta 里更可靠（title 在 codex 是 "token_count"、claude 可能是「会话用量」）。
	if s := strOf(m["model"]); s != "" {
		r.model = s
	}
	// 记录里没有任何 token 也没有费用 → 视为无内容，跳过（如 magpie 的 0 token 失败行仍有 status，
	// 需保留以计入失败次数）。
	if r.in == 0 && r.out == 0 && r.cacheRead == 0 && r.cacheWrite == 0 &&
		r.reasoning == 0 && r.total == 0 && !r.hasCost && !r.hasStatus {
		return record{}, false
	}
	return r, true
}

// aggregate 把已归一为「本次调用用量」的记录相加成会话合计。
//
// 五家的记录都已归一（codex 取 last_token_usage、其余取各自字段），因此只需一条规则：
// 逐条相加。分模型明细同一套累加，按 total 降序输出。
func aggregate(harness string, recs []record) Summary {
	var s Summary
	byModel := map[string]*ModelUsage{}

	for _, r := range recs {
		if r.bad {
			s.Skipped++
			continue
		}
		s.Records++
		if r.hasStatus && r.status != 200 {
			s.FailedCalls++
		}
		s.InputTokens += r.in
		s.OutputTokens += r.out
		s.CacheRead += r.cacheRead
		s.CacheWrite += r.cacheWrite
		s.Reasoning += r.reasoning
		s.TotalTokens += r.total
		if r.hasCost {
			s.CostUSD += r.cost
			s.CostKnown = true
		}
		if name := modelName(r.model); name != "" {
			mu := modelBucket(byModel, name)
			mu.Records++
			mu.InputTokens += r.in
			mu.OutputTokens += r.out
			mu.CacheRead += r.cacheRead
			mu.CacheWrite += r.cacheWrite
			mu.Reasoning += r.reasoning
			mu.TotalTokens += r.total
			if r.hasCost {
				mu.CostUSD += r.cost
			}
		}
	}
	// 记录里没自报合计的（magpie 的部分行、hermes 无 total 字段）：用输入+输出补齐，
	// 避免合计显示为 0。
	if s.TotalTokens == 0 {
		s.TotalTokens = s.InputTokens + s.OutputTokens
	}

	for _, mu := range byModel {
		if mu.TotalTokens == 0 {
			mu.TotalTokens = mu.InputTokens + mu.OutputTokens
		}
		s.Models = append(s.Models, *mu)
	}
	sort.Slice(s.Models, func(i, j int) bool {
		if s.Models[i].TotalTokens != s.Models[j].TotalTokens {
			return s.Models[i].TotalTokens > s.Models[j].TotalTokens
		}
		return s.Models[i].Model < s.Models[j].Model
	})
	return s
}

// modelName 过滤掉不是模型名的 title（codex 的 "token_count"、claude 单模型时的「会话用量」）。
func modelName(title string) string {
	switch title {
	case "", "token_count", "会话用量", "usage":
		return ""
	}
	return title
}

func modelBucket(m map[string]*ModelUsage, name string) *ModelUsage {
	if mu, ok := m[name]; ok {
		return mu
	}
	mu := &ModelUsage{Model: name}
	m[name] = mu
	return mu
}

// ---- meta 取值工具（JSON 数值可能是 int 或 float） -------------------------

func intOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func floatOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// firstInt 返回第一个存在且非零的数值字段（兼容各家不同的字段名）。
func firstInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n := intOf(v); n != 0 {
				return n
			}
		}
	}
	return 0
}
