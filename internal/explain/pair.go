// 工具调用配对（call ↔ result）。
//
// 为什么需要：一次工具调用的「请求」和「返回」在时间线上经常不相邻——实测本机库
// （2026-10-08，hm.db）里 codex 有 1198 次、hermes 709 次、pi 189 次的工具结果落在
// 调用之后 5 步以上（claude 最多 5 步）。只看相邻步骤读不出「这个结果是哪次调用的」，
// 一次工具往返会被拆成两段无关联的记录。
//
// 配对键来自采集器写入的稳定 id（实测四家 100% 可配对）：
//
//	harness   调用侧的键                      结果侧的键
//	hermes    meta.call_id（同时也在 parent_id） parent_id
//	codex     meta.call_id                     parent_id
//	claude    meta.call_id                     parent_id（meta.tool_use_id 同值）
//	pi        meta.call_id                     parent_id
//
// 即：**调用看 meta.call_id，结果看 parent_id**。本包用「调用键集合」与「结果键」做
// 会话内匹配，纯函数实现（不依赖存储/前端），可单测、可被 GUI 与预览工具共用。
package explain

import (
	"encoding/json"
	"fmt"
)

// Link 是一次成功配对的工具往返：调用步骤与结果步骤（都用会话内 seq 标识）。
type Link struct {
	CallSeq   int64
	ResultSeq int64
	Tool      string
	Round     int // 第几轮工具往返（会话内从 1 起，按调用出现顺序）
}

// Pairing 是一个会话内全部工具调用的配对结果。
type Pairing struct {
	byCall   map[int64]*Link // 调用 seq → 往返
	byResult map[int64]*Link // 结果 seq → 往返
	key      map[int64]string
}

// PairEvents 对一批事件（同一会话，按 seq 升序）做工具调用配对。
//
// 匹配规则（会话内，按 seq 顺序）：
//  1. 调用键 = meta.call_id → meta.item_id → parent_id（取第一个非空）；
//  2. 结果键 = parent_id → meta.call_id → meta.tool_call_id → meta.tool_use_id；
//  3. 键相同的调用与结果配对；同一调用键多次出现时按「先到先配」逐个消耗。
//
// 调用无结果（运行中/被杀）、结果无调用（记录缺失）都不算错，只是不配对——
// 二者都会被记录下来，供说明文字如实告知用户。
func PairEvents(events []Event) *Pairing {
	p := &Pairing{
		byCall:   map[int64]*Link{},
		byResult: map[int64]*Link{},
		key:      map[int64]string{},
	}
	pending := map[string][]int64{} // 调用键 → 尚未配对的结果，按出现顺序
	round := 0

	for i := range events {
		e := &events[i]
		switch e.Kind {
		case "tool.call":
			k := callKey(e)
			if k == "" {
				break
			}
			round++
			link := &Link{CallSeq: e.Seq, Tool: toolNameOf(e), Round: round}
			p.byCall[e.Seq] = link
			p.key[e.Seq] = k
			pending[k] = append(pending[k], e.Seq)

		case "tool.result":
			k := resultKey(e)
			if k == "" {
				break
			}
			q := pending[k]
			if len(q) == 0 {
				break // 结果没有对应调用（记录缺失）：留给 OrphanResults 报告
			}
			callSeq := q[0]
			pending[k] = q[1:]
			link := p.byCall[callSeq]
			if link == nil {
				break
			}
			link.ResultSeq = e.Seq
			p.byResult[e.Seq] = link
		}
	}
	return p
}

// LinkOfCall 返回某调用步骤的往返（未配对也返回非 nil，ResultSeq==0）。
func (p *Pairing) LinkOfCall(seq int64) *Link { return p.byCall[seq] }

// LinkOfResult 返回某结果步骤对应的往返；找不到对应调用时返回 nil。
func (p *Pairing) LinkOfResult(seq int64) *Link { return p.byResult[seq] }

// OrphanResults 返回没有对应调用步骤的结果 seq（原始记录里调用侧缺失）。
func (p *Pairing) OrphanResults(events []Event) []int64 {
	out := []int64{}
	for i := range events {
		if events[i].Kind != "tool.result" {
			continue
		}
		if p.byResult[events[i].Seq] == nil {
			out = append(out, events[i].Seq)
		}
	}
	return out
}

// UnreturnedCalls 返回还没有结果的调用 seq（会话在运行中，或工具被中断）。
func (p *Pairing) UnreturnedCalls(events []Event) []int64 {
	out := []int64{}
	for i := range events {
		if events[i].Kind != "tool.call" {
			continue
		}
		if l := p.byCall[events[i].Seq]; l != nil && l.ResultSeq == 0 {
			out = append(out, events[i].Seq)
		}
	}
	return out
}

// callKey 取调用侧的配对键（meta.call_id → meta.item_id → parent_id）。
func callKey(e *Event) string {
	m := metaMap(e.Meta)
	for _, k := range []string{"call_id", "item_id"} {
		if v := str(m[k]); v != "" {
			return v
		}
	}
	return e.ParentID
}

// resultKey 取结果侧的配对键（parent_id → meta 里的几个别名）。
func resultKey(e *Event) string {
	if e.ParentID != "" {
		return e.ParentID
	}
	m := metaMap(e.Meta)
	for _, k := range []string{"call_id", "tool_call_id", "tool_use_id", "item_id"} {
		if v := str(m[k]); v != "" {
			return v
		}
	}
	return ""
}

// metaMap 解析 meta JSON；失败返回空表（不 panic、不污染配对）。
func metaMap(meta string) map[string]any {
	if meta == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(meta), &m) != nil {
		return nil
	}
	return m
}

// str 把 meta 里的值转成字符串（JSON 解出来可能是 string 或 number）。
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}
