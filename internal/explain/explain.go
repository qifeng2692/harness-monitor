// Package explain 把监控事件翻译成自然语言「步骤说明书」。
//
// 目标（用户需求）：每一步讲清楚四件事——
//  1. 这一步是什么（动作）；
//  2. 输入来自哪里（上一个谁）；
//  3. 输出给了谁——**是否显示给用户端**；没有显示给用户的话，去了哪、为什么；
//  4. 为什么会流转到下一步。
//
// 纯函数实现（不依赖前端/存储），可单测、可被 GUI 与预览工具共用。
package explain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event 是解释器输入的最小事件（从存储/模型层转换而来）。
type Event struct {
	Seq   int64
	Kind  string
	Title string
	Meta  string
}

// StepExplain 是单步的自然语言说明（前端直接渲染）。
type StepExplain struct {
	Summary  string `json:"summary"`  // 一句话：这一步在做什么
	From     string `json:"from"`     // 输入来自哪里
	To       string `json:"to"`       // 输出去向（含是否显示给用户）
	Visible  string `json:"visible"`  // 是否给用户端显示（是/否 + 原因）
	Why      string `json:"why"`      // 为什么流转到下一步
	NextHint string `json:"nextHint"` // 下一步预告（衔接）
}

// ExplainAll 为整个事件序列生成说明（与输入一一对应）。
func ExplainAll(events []Event) []StepExplain {
	out := make([]StepExplain, 0, len(events))
	for i := range events {
		var prev, next *Event
		if i > 0 {
			prev = &events[i-1]
		}
		if i < len(events)-1 {
			next = &events[i+1]
		}
		out = append(out, ExplainOne(&events[i], prev, next))
	}
	return out
}

// ExplainOne 生成一步的说明。prev/next 可为 nil。
func ExplainOne(e, prev, next *Event) StepExplain {
	ex := StepExplain{}
	tool := toolNameOf(e)
	prevTool := toolNameOf(prev)

	switch e.Kind {
	case "session.start":
		ex.Summary = "一个新的对话会话开始了，系统为它建立了独立的记录档案。"
		ex.From = "无（这是整个会话的起点）"
		ex.To = "记录到会话档案，不在对话窗口显示"
		ex.Visible = "否——这是系统内部的档案建立动作，对话双方看不到。"
		ex.Why = "有了这个档案，后续每一步才有归属（知道它们属于哪个对话）。"

	case "turn.start":
		ex.Summary = "用户发来了一条消息（真实的人写的内容）。"
		ex.From = "来自用户的键盘输入（或粘贴的内容）"
		ex.To = "发送给 AI 模型（连同之前的对话历史）"
		ex.Visible = "是——这正是用户自己发出的内容，用户当然知道。"
		ex.Why = "模型收到这句话后，开始思考怎么回应（下一步通常是「模型思考」）。"

	case "llm.reasoning":
		ex.Summary = "模型在「打草稿」——它先在心里盘算怎么处理刚才的输入，还没给出正式回复。"
		ex.From = "来自上一条用户输入" + backRef(prev)
		ex.To = "只发给模型自己（作为下一步决策的依据）"
		ex.Visible = "否——这是模型的内部思考过程，默认不显示给用户端（就像人写草稿，草稿不给人看）。"
		ex.Why = "模型想清楚之后，要么直接回复用户，要么决定调用工具去查/去做（下一步见分晓）。"

	case "llm.response":
		ex.Summary = "模型给出了正式回复（自然语言文本）。"
		ex.From = "来自模型的思考结果" + backRef(prev)
		if next != nil && next.Kind == "tool.call" {
			ex.To = fmt.Sprintf("这条文本先显示给用户看，同时模型准备调用工具「%s」继续干活", toolNameOf(next))
			ex.Visible = "是——用户会看到这段文字（通常是模型在说「我来查一下…」之类的过渡语）。"
			ex.Why = fmt.Sprintf("模型决定先说明意图，再调用「%s」工具获取信息或执行操作。", toolNameOf(next))
		} else {
			ex.To = "显示给用户（这是本轮的最终回答）"
			ex.Visible = "是——这就是用户在界面上看到的回答。"
			ex.Why = "模型认为任务已完成或本轮话已说完，等待用户的下一条消息。"
		}

	case "tool.call":
		ex.Summary = fmt.Sprintf("模型请求调用工具「%s」——执行一个具体动作（跑命令/读文件/搜索等）。", tool)
		ex.From = "来自模型的决策" + backRef(prev)
		ex.To = fmt.Sprintf("发送给工具执行器，由它实际执行「%s」", tool)
		ex.Visible = "否——这是模型与系统之间的内部指令，用户端一般不显示（界面上可能有个小提示，但内容本身不展示给用户）。"
		ex.Why = fmt.Sprintf("工具执行需要时间，结果回来后（下一步「工具返回」）模型才能继续。%s", toolPurpose(tool))

	case "tool.result":
		ex.Summary = fmt.Sprintf("工具「%s」执行完毕，把结果送回来了。", tool)
		ex.From = "来自上一步的工具调用" + backRef(prev)
		ex.To = "发送给模型（作为它继续思考的素材）"
		ex.Visible = "否——工具原始结果默认不直接显示给用户（用户只看模型的总结，除非展开工具详情）。"
		ex.Why = "模型拿到结果后要继续判断：结果够不够？够就直接回复用户，不够就再调工具（所以下一步常见「模型思考」或直接「模型回复」）。"

	case "llm.usage":
		ex.Summary = "系统记录了一次模型调用的用量（token 数/耗时/状态）。"
		ex.From = "来自刚才那次模型调用的统计信息"
		ex.To = "记录到账本（用于成本核算与性能监控）"
		ex.Visible = "否——这是后台记账，与对话内容无关，用户端不显示。"
		ex.Why = "只是留痕记账，不影响对话流程；下一个真正影响流程的步骤会继续。"
		if prevTool != "" {
			ex.From = fmt.Sprintf("来自刚才围绕工具「%s」的那次模型调用", prevTool)
		}

	case "turn.end":
		ex.Summary = "这一轮对话结束了（用户问 → 模型答的完整循环走完）。"
		ex.From = "来自本轮最后一步的完成"
		ex.To = "回到等待状态，等用户发下一条消息"
		ex.Visible = "否——这是流程控制点，用户看到的是前面那条「模型回复」。"
		ex.Why = "轮次结束不等于会话结束；用户随时可以继续发消息，开始下一轮。"

	case "session.end":
		ex.Summary = "整个会话结束了。"
		ex.From = "来自会话的收尾"
		ex.To = "档案封存"
		ex.Visible = "否——系统内部动作。"
		ex.Why = "没有下一步了，这个会话到此为止。"

	case "compaction":
		ex.Summary = "对话历史太长，系统做了一次「上下文压缩」——把早期内容浓缩成摘要，腾出空间。"
		ex.From = "来自过长的对话历史"
		ex.To = "压缩后的摘要，替换掉原始历史，供模型后续使用"
		ex.Visible = "否——用户端通常不感知（有的界面会提示「上下文已压缩」）。"
		ex.Why = "压缩后模型可以继续对话而不超出容量限制。"

	case "error":
		ex.Summary = "这一步出错了。"
		ex.From = "来自失败的那次操作"
		ex.To = "错误信息上报（可能触发重试或中止）"
		ex.Visible = "视情况——严重错误会显示给用户，轻微错误可能自动重试后不显示。"
		ex.Why = "错误可能让流程中断，也可能被自动恢复（看下一条事件是继续还是终止）。"

	case "session.meta":
		ex.Summary = "会话的配置信息发生了变化（比如切换了模型）。"
		ex.From = "来自用户的设置操作或系统调整"
		ex.To = "更新会话档案"
		ex.Visible = "部分——用户如果自己切了模型会知道，系统自动调整则不显示。"
		ex.Why = "后续的模型调用将使用新配置。"

	default:
		ex.Summary = fmt.Sprintf("步骤类型：%s", e.Kind)
		ex.From = "—"
		ex.To = "—"
		ex.Visible = "—"
		ex.Why = "—"
	}

	// 下一步预告（衔接）
	if next != nil {
		ex.NextHint = fmt.Sprintf("下一步是「%s」", kindLabel(next.Kind))
		if next.Kind == "tool.call" {
			ex.NextHint += fmt.Sprintf("（调用 %s）", toolNameOf(next))
		}
		ex.NextHint += "。"
	} else {
		ex.NextHint = "这是该会话的最后一步。"
	}
	return ex
}

// kindLabel 是步骤类型的中文名。
func kindLabel(kind string) string {
	switch kind {
	case "session.start":
		return "会话开始"
	case "session.meta":
		return "会话信息变更"
	case "turn.start":
		return "用户输入"
	case "llm.response":
		return "模型回复"
	case "llm.reasoning":
		return "模型思考"
	case "tool.call":
		return "调用工具"
	case "tool.result":
		return "工具返回"
	case "llm.usage":
		return "用量记录"
	case "turn.end":
		return "轮次结束"
	case "session.end":
		return "会话结束"
	case "compaction":
		return "上下文压缩"
	case "error":
		return "错误"
	default:
		return kind
	}
}

// toolNameOf 从事件中提取工具名（meta.tool_name 或 title）。
func toolNameOf(e *Event) string {
	if e == nil {
		return ""
	}
	if e.Kind != "tool.call" && e.Kind != "tool.result" {
		return ""
	}
	var m map[string]any
	if e.Meta != "" && json.Unmarshal([]byte(e.Meta), &m) == nil {
		if tn, ok := m["tool_name"].(string); ok && tn != "" {
			return tn
		}
	}
	if e.Title != "" && e.Title != "tool result" {
		return e.Title
	}
	return "未知工具"
}

// backRef 生成"来自上一步"的引用文字。
func backRef(prev *Event) string {
	if prev == nil {
		return ""
	}
	lbl := kindLabel(prev.Kind)
	if tn := toolNameOf(prev); tn != "" {
		return fmt.Sprintf("（上一步是「%s」，工具 %s）", lbl, tn)
	}
	return fmt.Sprintf("（上一步是「%s」）", lbl)
}

// toolPurpose 给常见工具一句话用途说明。
func toolPurpose(tool string) string {
	switch {
	case tool == "terminal" || tool == "bash" || tool == "exec_command" || tool == "Bash":
		return "这类工具用来在本机执行命令（看目录、跑脚本、查状态）。"
	case tool == "read_file" || tool == "Read":
		return "这类工具用来读取文件内容。"
	case tool == "write_file" || tool == "Write" || tool == "Edit":
		return "这类工具用来写文件或改代码。"
	case strings.HasPrefix(tool, "web_") || tool == "WebSearch" || tool == "WebFetch":
		return "这类工具用来上网搜索或抓取网页。"
	case strings.HasPrefix(tool, "kanban"):
		return "这类工具用来读写任务看板。"
	case tool == "execute_code":
		return "这类工具用来跑一段代码做计算或数据处理。"
	default:
		return ""
	}
}
