// Package filechange 回答一个问题：**这个会话到底改动了哪些文件**。
//
// 为什么需要：harness-monitor 已经能回答「发生了什么」（时间线）、「花了多少」
// （用量条）、「卡在哪」（会话状态）、「有没有打转」（loops），但答不了「它到底
// 动了哪些文件」——而这是同类产品（agentacct 的 Work Receipt 里叫 files changed）
// 最先摆出来的一条事实，也是人复查一次 agent 工作时最先要看的。
//
// 口径纪律（与 internal/stats / internal/loops 一致）：
//   - **只认结构化字段**：只有那些「参数里明确写了文件路径」的写文件工具才算数
//     （claude 的 Edit/Write → file_path，hermes 的 patch/write_file → path，
//     pi 的 edit/write → path）。这些字段实测 100% 命中（见文件末尾的实测记录）。
//   - **不猜 shell 命令**：codex 用 exec_command、claude 用 Bash、hermes 用 terminal
//     改文件时，路径埋在命令行文本里，解析它就是在猜（`Set-Content a.ps1` 与
//     `rg a.ps1` 长得一样）。这些改动**不计入**，并在输出里明说——缺失胜过猜错。
//   - **路径归一化后归组**：同一文件被写成 `G:\a\b.go` 和 `G:/a/b.go` 是同一个文件。
//     归一化只统一分隔符与 `./`、重复斜杠，不做大小写折叠（Linux 上大小写有别）。
package filechange

import (
	"encoding/json"
	"path"
	"sort"
	"strings"
	"time"
)

// Op 是一次改动的类型。
type Op string

const (
	// OpWrite 是整文件写入（覆盖式）。
	OpWrite Op = "write"
	// OpEdit 是局部编辑（改一段、打补丁）。
	OpEdit Op = "edit"
)

// Call 是一次工具调用（来自 tool.call 事件）。
type Call struct {
	Tool string    // 工具名（tool.call 的 title）
	Args string    // 参数原文（tool.call 的 content，JSON）
	Seq  int64     // 会话内序号（定位用）
	Time time.Time // 调用时间
}

// Change 是一个被改动的文件：同一路径的多次改动合并成一条。
type Change struct {
	Path      string    // 归一化后的路径（分隔符统一为 /）
	Count     int       // 改动次数
	Ops       []Op      // 出现过的操作类型（去重，按首次出现顺序）
	Tools     []string  // 用过的工具名（去重，按首次出现顺序）
	FirstSeq  int64     // 第一次改动的步骤号
	LastSeq   int64     // 最后一次改动的步骤号
	FirstTime time.Time // 第一次改动的时间
	LastTime  time.Time // 最后一次改动的时间
}

// Summary 是一个会话的文件改动汇总。
type Summary struct {
	Changes []Change // 按改动次数降序，同数按路径升序
	Calls   int      // 认出来的写文件调用总数（= 各 Change.Count 之和）
	NoPath  int      // 认得出是写文件、但参数里没有可用路径的调用数（不计入 Changes）
	Other   int      // 其它工具调用数（读、搜、执行……不算改动）
}

// rule 描述「某家的某个工具是写文件工具，路径在哪个字段」。
type rule struct {
	op Op
	// keys 按顺序尝试，取第一个非空字段。patch 改名时只有 new_path，
	// 所以「旧路径缺失就退回新路径」是必要的兜底。
	keys []string
}

// rules 是「有结构化文件字段」的工具表，按 harness 分家。
//
// 每一行都对着本机真实库核过（2026-10-24，data/hm.db 的 tool.call 参数 JSON）：
//   - claude  Edit   keys=[file_path new_string old_string replace_all]  118/118 带 file_path
//   - claude  Write  keys=[content file_path]                            41/41  带 file_path
//   - hermes  patch  keys=[new_string old_string path]                   299/300 带 path
//   - hermes  write_file keys=[content path]                             300/300 带 path
//   - pi      edit   keys=[edits path]                                   299/300 带 path
//   - pi      write  keys=[content path]                                 298/300 带 path
//
// 少数没带路径的（各家都有 1-2 条）计入 Summary.NoPath，不当成「改了某个文件」。
var rules = map[string]map[string]rule{
	"claude": {
		"Write":        {op: OpWrite, keys: []string{"file_path"}},
		"Edit":         {op: OpEdit, keys: []string{"file_path"}},
		"MultiEdit":    {op: OpEdit, keys: []string{"file_path"}},
		"NotebookEdit": {op: OpEdit, keys: []string{"notebook_path", "file_path"}},
	},
	"hermes": {
		"write_file": {op: OpWrite, keys: []string{"path"}},
		"patch":      {op: OpEdit, keys: []string{"path", "new_path"}},
	},
	"pi": {
		"write": {op: OpWrite, keys: []string{"path"}},
		"edit":  {op: OpEdit, keys: []string{"path"}},
	},
}

// Supports 报告某家 harness 是否有结构化的写文件工具。
//
// 返回 false 不代表「它没改文件」，只代表「它的改动没有结构化的文件字段，
// 本包拒绝猜」。codex 就是这种：它用 exec_command 跑 shell 命令改文件。
func Supports(harness string) bool {
	_, ok := rules[harness]
	return ok
}

// Supported 返回有结构化写文件工具的 harness 名单（按字母序）。
func Supported() []string {
	out := make([]string, 0, len(rules))
	for h := range rules {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// Classify 判定一次工具调用是不是「改文件」，是则给出路径（路径可能为空）。
//
// 返回值：
//   - isWrite=false：不是写文件类工具（读、搜、执行……）；
//   - isWrite=true 且 filePath=""：是写文件类工具，但参数里没有可用路径——
//     调用方应计入 NoPath，而不是编一个路径出来。
func Classify(harness, tool, args string) (filePath string, op Op, isWrite bool) {
	byTool, ok := rules[harness]
	if !ok {
		return "", "", false
	}
	r, ok := byTool[tool]
	if !ok {
		return "", "", false
	}
	return pickPath(args, r.keys), r.op, true
}

// pickPath 从参数 JSON 里按 keys 顺序取第一个非空字符串。
//
// 参数不是合法 JSON 时返回空串（调用方计入 NoPath）——不去正则抠路径，
// 抠出来的是猜测，不是事实。
func pickPath(args string, keys []string) string {
	if strings.TrimSpace(args) == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := obj[k].(string); ok {
			if p := Normalize(v); p != "" {
				return p
			}
		}
	}
	return ""
}

// Normalize 把路径写成可比较的形态：分隔符统一为 /，清掉 ./ 与重复斜杠。
//
// 用 path（而不是 filepath）做清理，是为了让同一份记录在 Windows 与 Linux
// 上得到同样的结果——本包只做「同一文件的不同写法归到一组」这一件事。
// 不做大小写折叠：Linux 上 `A.go` 与 `a.go` 是两个文件。
func Normalize(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, `\`, "/")
	return path.Clean(p)
}

// Summarize 把一个会话的全部工具调用汇总成文件改动清单。
//
// 排序：改动次数降序；次数相同按路径升序（保证输出稳定，便于 diff 与断言）。
func Summarize(harness string, calls []Call) Summary {
	sum := Summary{Changes: []Change{}}
	index := map[string]int{} // 归一化路径 → Changes 下标

	for _, c := range calls {
		filePath, op, isWrite := Classify(harness, c.Tool, c.Args)
		if !isWrite {
			sum.Other++
			continue
		}
		if filePath == "" {
			sum.NoPath++
			continue
		}
		sum.Calls++
		if i, ok := index[filePath]; ok {
			ch := &sum.Changes[i]
			ch.Count++
			ch.Ops = appendOp(ch.Ops, op)
			ch.Tools = appendTool(ch.Tools, c.Tool)
			ch.LastSeq = c.Seq
			ch.LastTime = c.Time
			continue
		}
		index[filePath] = len(sum.Changes)
		sum.Changes = append(sum.Changes, Change{
			Path:      filePath,
			Count:     1,
			Ops:       []Op{op},
			Tools:     []string{c.Tool},
			FirstSeq:  c.Seq,
			LastSeq:   c.Seq,
			FirstTime: c.Time,
			LastTime:  c.Time,
		})
	}

	sort.SliceStable(sum.Changes, func(i, j int) bool {
		if sum.Changes[i].Count != sum.Changes[j].Count {
			return sum.Changes[i].Count > sum.Changes[j].Count
		}
		return sum.Changes[i].Path < sum.Changes[j].Path
	})
	return sum
}

func appendOp(ops []Op, op Op) []Op {
	for _, o := range ops {
		if o == op {
			return ops
		}
	}
	return append(ops, op)
}

func appendTool(tools []string, tool string) []string {
	for _, t := range tools {
		if t == tool {
			return tools
		}
	}
	return append(tools, tool)
}
