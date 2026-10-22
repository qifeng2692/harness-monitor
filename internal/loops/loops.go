// Package loops 检测「同一个调用在会话里反复出现」的工具调用环。
//
// 为什么需要：harness-monitor 此前只能回答「失败了多少次」（见 internal/stats），
// 答不了「同一个动作是不是卡在原地反复做」。这是 agent 最常见的一类空转：
// 同一条命令、同一个参数，失败一次、再一次、又一次——每次都记了日志，但没人
// 把它单独拎出来说「这里在打转」。
//
// 同类工具的叫法：MINDLAS 叫它 tool loop，rungraph 叫它 retry storm。
//
// 口径纪律（与 internal/stats 一致）：
//   - 「重复」是**结构事实**：同一会话内，同一个工具 + 同一组参数被调用了 N 次。
//     这不依赖任何失败信号，因此对五家 harness 一视同仁（codex/claude 的工具结果
//     完全不带失败信号，靠失败判定是查不出它们的）。
//   - 「失败」只认各家自己上报的结构化字段（meta.is_error）。判不了就说判不了，
//     不把「没失败」当成「成功」。
//   - 「次数」按**调用标识（call_id）去重**：本机库实测有 5139 个 call_id 被采了两遍
//     （同一次调用进了两行），按行数算会把「重复采集」误报成「重复调用」。
package loops

import (
	"sort"
	"strings"
)

// DefaultThreshold 是默认的重复次数下限。
//
// 实测本机库（2026-10-22，hm.db）：阈值 3 命中 367 组 / 126 个会话；阈值 5 命中
// 96 组。3 是既能挑出真打转、又不至于把正常轮询全捞进来的下限。
const DefaultThreshold = 3

// Call 是一次工具调用（来自 tool.call 事件）。
type Call struct {
	Harness   string
	SessionID string
	Tool      string // 工具名（各家的 tool.call 的 title 就是工具名）
	Args      string // 参数原文（各家的 tool.call 的 content）
	Key       string // 调用标识（meta.call_id），用于去重与配对
}

// Result 是一次工具结果（来自 tool.result 事件）。
type Result struct {
	Harness   string
	SessionID string
	Key       string // 结果侧的配对键（parent_id = 对应的 call_id）
	Failed    bool   // 结果判为失败
	Judged    bool   // 结果带失败信号（判得了）；false = 判不了
}

// Group 是一组「同一工具 + 同一组参数」的重复调用。
type Group struct {
	Harness   string
	SessionID string
	Tool      string
	Calls     int    // 重复次数（按 Key 去重后的调用数）
	Failed    int    // 其中判为失败的次数
	Judged    int    // 其中判得了失败信号的次数（Calls-Judged = 判不了）
	Sample    string // 参数样例（已截断，供人眼判断是不是真打转）
}

// Detect 找出「同一会话内、同一个工具、同一组参数被调用了至少 threshold 次」的组。
//
// threshold <= 0 时按 DefaultThreshold（3）处理。
//
// 失败计数只统计能判定的结果（Result.Judged）；判不了的计入 Judged 的缺口，
// 不当作成功。
//
// 同一组内按 Key 去重：库里同一次调用可能被采成多行（实测 5139 个 call_id 重复），
// 去重后才是「调用了几次」这个事实。
func Detect(calls []Call, results []Result, threshold int) []Group {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}

	// 结果索引：键 → 该次调用的判定。同一个键有多条结果时取「并集」——
	// 只要有一条判失败就算失败（宁可报出失败，也不把失败藏起来）。
	type verdict struct {
		failed bool
		judged bool
	}
	idx := map[string]verdict{}
	for _, r := range results {
		if r.Key == "" {
			continue
		}
		k := resultKey(r.Harness, r.SessionID, r.Key)
		v := idx[k]
		if r.Failed {
			v.failed = true
		}
		if r.Judged {
			v.judged = true
		}
		idx[k] = v
	}

	// 按（harness, 会话, 工具, 参数）归组。
	type groupKey struct {
		harness   string
		sessionID string
		tool      string
		args      string
	}
	groups := map[groupKey]*Group{}
	seen := map[groupKey]map[string]bool{}

	for _, c := range calls {
		gk := groupKey{c.Harness, c.SessionID, c.Tool, c.Args}
		g, ok := groups[gk]
		if !ok {
			g = &Group{
				Harness:   c.Harness,
				SessionID: c.SessionID,
				Tool:      c.Tool,
			}
			groups[gk] = g
			seen[gk] = map[string]bool{}
		}
		// 同一个 Key 只算一次调用。
		if c.Key != "" {
			if seen[gk][c.Key] {
				continue
			}
			seen[gk][c.Key] = true
		}
		g.Calls++
	}

	out := []Group{}
	for gk, g := range groups {
		if g.Calls < threshold {
			continue
		}
		// 统计这一组里各次调用的失败判定。
		for key := range seen[gk] {
			v, ok := idx[resultKey(gk.harness, gk.sessionID, key)]
			if !ok {
				continue // 结果缺失（会话还在跑 / 记录不全）→ 判不了
			}
			if v.judged {
				g.Judged++
			}
			if v.failed {
				g.Failed++
			}
		}
		g.Sample = Sample(gk.args, 160)
		out = append(out, *g)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		if out[i].Failed != out[j].Failed {
			return out[i].Failed > out[j].Failed
		}
		if out[i].Harness != out[j].Harness {
			return out[i].Harness < out[j].Harness
		}
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].Tool < out[j].Tool
	})
	return out
}

// resultKey 组成结果索引的键：跨 harness、跨会话的 call_id 可能撞车。
func resultKey(harness, sessionID, key string) string {
	return harness + "\x00" + sessionID + "\x00" + key
}

// Sample 截取参数样例：按 rune 截断（中文按字断，不切出乱码），超出加省略号。
func Sample(args string, n int) string {
	r := []rune(strings.TrimSpace(args))
	if len(r) <= n {
		return string(r)
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
