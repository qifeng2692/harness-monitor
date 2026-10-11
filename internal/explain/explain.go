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
	Seq      int64
	Kind     string
	Title    string
	Meta     string
	ParentID string // 关联父键（工具调用的 call_id 等）——配对靠它（见 pair.go）
}

// StepExplain 是单步的自然语言说明（前端直接渲染）。
type StepExplain struct {
	Summary  string `json:"summary"`  // 一句话：这一步在做什么
	From     string `json:"from"`     // 输入来自哪里
	To       string `json:"to"`       // 输出去向（含是否显示给用户）
	Visible  string `json:"visible"`  // 是否给用户端显示（是/否 + 原因）
	Why      string `json:"why"`      // 为什么流转到下一步
	NextHint string `json:"nextHint"` // 下一步预告（衔接）

	// 工具调用配对（见 pair.go）。非工具步骤保持零值。
	Round     int    `json:"round"`     // 第几轮工具往返（会话内从 1 起；0 = 不适用）
	ToolKey   string `json:"toolKey"`   // 配对键（同一工具的多次调用靠它区分）
	CallSeq   int64  `json:"callSeq"`   // 调用步骤 seq（tool.result 上填；0 = 未配对）
	ResultSeq int64  `json:"resultSeq"` // 结果步骤 seq（tool.call 上填；0 = 尚无结果）
	WaitNote  string `json:"waitNote"`  // 调用与结果之间的间隔说明（不相邻时才有）
}

// ExplainAll 为整个事件序列生成说明（与输入一一对应）。
//
// 会先做一次工具调用配对（PairEvents），让相隔很远的「调用 ↔ 返回」在说明里互相指认。
func ExplainAll(events []Event) []StepExplain {
	p := PairEvents(events)
	out := make([]StepExplain, 0, len(events))
	for i := range events {
		var prev, next *Event
		if i > 0 {
			prev = &events[i-1]
		}
		if i < len(events)-1 {
			next = &events[i+1]
		}
		out = append(out, ExplainOnePaired(&events[i], prev, next, p))
	}
	return out
}

// ExplainOne 生成一步的说明（不做工具配对）。prev/next 可为 nil。
func ExplainOne(e, prev, next *Event) StepExplain {
	return ExplainOnePaired(e, prev, next, nil)
}

// ExplainOnePaired 在 ExplainOne 基础上叠加工具调用配对信息。p 可为 nil（退化为 ExplainOne）。
func ExplainOnePaired(e, prev, next *Event, p *Pairing) StepExplain {
	// 结果侧常常没有工具名（实测：codex 的 tool.result 只有 call_id、claude 只有 tool_use_id，
	// title 是通用的 "tool result"），单看这一步只能说出「未知工具」。配对后可以从调用侧
	// 把工具名补回来，让说明书说的是真工具名。
	override := ""
	if p != nil {
		if l := p.LinkOfResult(e.Seq); l != nil && l.Tool != "" {
			override = l.Tool
		}
	}
	ex := explainOne(e, prev, next, override)
	if p == nil {
		return ex
	}
	// 调用侧：结果可能很晚才回来（甚至还没回来）——如实告诉用户隔了多少步。
	if l := p.LinkOfCall(e.Seq); l != nil {
		ex.Round, ex.ToolKey, ex.ResultSeq = l.Round, p.key[e.Seq], l.ResultSeq
		if l.ResultSeq == 0 {
			ex.WaitNote = "这次调用到会话结束（或目前为止）都没有看到返回结果。"
		} else if l.ResultSeq != e.Seq+1 {
			ex.WaitNote = fmt.Sprintf("结果不是紧接着出现的：它在第 %d 步之后才回来（中间隔了 %d 步）。",
				l.ResultSeq, l.ResultSeq-e.Seq-1)
		}
	}
	// 结果侧：指认它属于哪一轮调用。
	if l := p.LinkOfResult(e.Seq); l != nil {
		ex.Round, ex.ToolKey, ex.CallSeq = l.Round, p.key[l.CallSeq], l.CallSeq
		if l.CallSeq != e.Seq-1 {
			ex.WaitNote = fmt.Sprintf("这是第 %d 步那次调用（第 %d 轮工具往返）的返回，不是紧邻上一步的结果。",
				l.CallSeq, l.Round)
		}
	} else if e.Kind == "tool.result" {
		ex.WaitNote = "在本次会话记录里找不到与之配对的调用步骤（原始记录里调用侧缺失）。"
	}
	return ex
}

// explainOne 是不含配对信息的说明生成（ExplainOne / ExplainOnePaired 共用）。
// toolOverride 非空时用它当工具名（配对层从调用侧补来的真名）。
func explainOne(e, prev, next *Event, toolOverride string) StepExplain {
	ex := StepExplain{}
	tool := toolNameOf(e)
	if toolOverride != "" {
		tool = toolOverride
	}
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
		// 这次调用失败时如实说明——失败的结果和成功的在正文里长得差不多，
		// 只有 meta 里的结构化标记（exit_code 非 0 / is_error）能区分。
		if note := toolFailureNote(e); note != "" {
			ex.Summary = fmt.Sprintf("工具「%s」执行失败，返回的是错误信息。", tool)
			ex.Why = note + " " + ex.Why
		}

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
		// 失败步骤：说清楚「失败的是什么、为什么、算不算致命」。
		// title/meta 由各采集器给出（codex 回合被中断 / claude 请求失败 HTTP 502 /
		// pi 模型调用失败 terminated），这里把结构化字段翻译成自然语言。
		ex.Summary = errorSummary(e)
		ex.From = "来自失败的那次操作" + backRef(prev)
		ex.To = "记入会话档案（模型可能据此重试或放弃这一轮）"
		ex.Visible = "视情况——网关/供应商侧的报错通常不直接展示给用户，用户只看到「没有回复」或重试提示。"
		ex.Why = errorWhy(e)

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
		if next.Kind == "error" && e.Kind == "error" {
			// 重试过程会连续出现多条失败记录——说「下一步是「错误」」等于没说。
			ex.NextHint = "下一步还是一条失败记录（重试过程中会连续出现多条）。"
		} else {
			ex.NextHint = fmt.Sprintf("下一步是「%s」", kindLabel(next.Kind))
			if next.Kind == "tool.call" {
				ex.NextHint += fmt.Sprintf("（调用 %s）", toolNameOf(next))
			}
			ex.NextHint += "。"
		}
	} else {
		ex.NextHint = "这是该会话的最后一步。"
	}
	return ex
}

// errorSummary 把 error 事件的标题与结构化字段拼成一句人话。
//
// 各采集器的 title 已带上下文（「回合被中断」/「请求失败（HTTP 502）」/「模型调用失败：429」），
// 这里在其后补上「这意味着什么」，避免只说「出错了」而没说清失败在哪一层，
// 也不重复 title 已经说过的话。
func errorSummary(e *Event) string {
	what := e.Title
	if what == "" {
		what = "操作失败"
	}
	switch errorType(e) {
	case "turn_aborted":
		return what + "——用户打断、客户端退出或连接被掐断，这一轮没有跑完。"
	case "api_error":
		return what + "——请求没能送达或对方返回了错误，系统可能已自动重试。"
	case "stop_reason_error":
		return what + "——模型这次没有给出结果，调用在中途失败，这一轮的产出是空的。"
	default:
		return "这一步出错了：" + what + "。"
	}
}

// errorWhy 说明这次失败对后续流程意味着什么（不夸大也不轻描淡写）。
func errorWhy(e *Event) string {
	switch errorType(e) {
	case "turn_aborted":
		return "回合被中断后，这一轮不会再有后续步骤（没有模型回复，也没有工具调用）；用户的下一条消息会开启新的一轮。"
	case "api_error":
		return "接口失败后系统通常会自动重试（见 meta 里的重试次数与上限）：重试成功则流程继续，重试用尽则这一轮到此为止。"
	case "stop_reason_error":
		return "模型调用失败意味着这一步拿不到任何内容，流程要么重试要么直接中断——看下一条事件是继续还是终止。"
	default:
		return "错误可能让流程中断，也可能被自动恢复（看下一条事件是继续还是终止）。"
	}
}

// errorType 取错误的结构化类型（各采集器写在 meta.type）。
func errorType(e *Event) string {
	m := metaMap(e.Meta)
	return str(m["type"])
}

// toolFailureNote 判断工具结果是否标记为失败，返回一句可拼进说明的提示；非失败返回空。
//
// 依据是采集器写入 meta 的结构化信号（hermes 的 exit_code 非 0 / 非空 error、
// pi 的 is_error），不做正文文本猜测——猜错会把正常输出说成失败。
func toolFailureNote(e *Event) string {
	m := metaMap(e.Meta)
	if m == nil {
		return ""
	}
	if isErr, ok := m["is_error"].(bool); ok {
		if !isErr {
			return ""
		}
		if code, ok := m["exit_code"]; ok {
			return fmt.Sprintf("注意：这次调用失败了（退出码 %s），返回的是错误输出而不是有效结果。", str(code))
		}
		return "注意：这次调用失败了，返回的是错误输出而不是有效结果。"
	}
	return ""
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
