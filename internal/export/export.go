// Package export 把一个会话导出成自包含的 Markdown 文档。
//
// 目标（对标 cc-sessions-viewer 的「export to html/markdown」能力）：
// 会话此前只能开 GUI 逐步翻——没有任何「带出去」的方式。导出文件包含
// 每一步的完整内容 + 自然语言说明书（复用 internal/explain，工具往返
// 配对随之带上），可以直接阅读、归档或发给别人看。
//
// 纯函数实现（不依赖存储/GUI），可单测。
package export

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/explain"
	"github.com/qifeng2692/harness-monitor/internal/model"
)

// Session 把一个会话导出为 Markdown 文本。
//
// title/first/last 为会话级元信息（可零值，导出时如实标注）；
// events 为该会话全部事件（按 seq 升序——store.ListEvents 已保证，
// 这里再兜底排一次，保证调用方乱序传入也不出错）。
func Session(harness, title string, first, last time.Time, events []model.Event) string {
	// 兜底排序：导出文档的步骤顺序必须与 seq 一致。
	evs := make([]model.Event, len(events))
	copy(evs, events)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })

	// 说明书与事件一一对应（含工具往返配对）。
	items := make([]explain.Event, len(evs))
	for i, e := range evs {
		items[i] = explain.Event{Seq: e.Seq, Kind: e.Kind, Title: e.Title, Meta: e.Meta, ParentID: e.ParentID}
	}
	explains := explain.ExplainAll(items)

	errCount := 0
	for _, e := range evs {
		if e.Kind == model.KindError {
			errCount++
		}
	}

	var b strings.Builder
	writeHeader(&b, harness, title, first, last, len(evs), errCount)
	for i := range evs {
		writeStep(&b, i+1, &evs[i], &explains[i])
	}
	return b.String()
}

func writeHeader(b *strings.Builder, harness, title string, first, last time.Time, steps, errCount int) {
	b.WriteString(fmt.Sprintf("# 会话导出：%s\n\n", orDash(title)))
	b.WriteString(fmt.Sprintf("- Harness：%s\n", orDash(harness)))
	b.WriteString(fmt.Sprintf("- 时间：%s → %s\n", fmtTime(first), fmtTime(last)))
	b.WriteString(fmt.Sprintf("- 步骤数：%d\n", steps))
	if errCount > 0 {
		b.WriteString(fmt.Sprintf("- 失败/异常：%d 步（文中以「⚠ 失败」标出）\n", errCount))
	}
	b.WriteString("\n本文由 harness-monitor 导出：每一步含完整内容与自然语言说明书（这一步是什么 / 输入来自 / 输出去向 / 为什么流转）。\n\n---\n\n")
}

func writeStep(b *strings.Builder, n int, e *model.Event, x *explain.StepExplain) {
	b.WriteString(fmt.Sprintf("## %d. [%s] %s\n\n", n, e.Kind, orDash(e.Title)))
	b.WriteString(fmt.Sprintf("- 时间：%s\n", fmtTime(e.Time)))
	if x.Round > 0 {
		b.WriteString(fmt.Sprintf("- 工具往返：第 %d 轮（%s）\n", x.Round, x.ToolKey))
	}
	b.WriteString(fmt.Sprintf("- 说明：%s\n", orDash(x.Summary)))
	b.WriteString(fmt.Sprintf("- 输入来自：%s\n", orDash(x.From)))
	b.WriteString(fmt.Sprintf("- 输出去向：%s\n", orDash(x.To)))
	b.WriteString(fmt.Sprintf("- 给用户看吗：%s\n", orDash(x.Visible)))
	b.WriteString(fmt.Sprintf("- 为什么流转：%s\n", orDash(x.Why)))
	if x.NextHint != "" {
		b.WriteString(fmt.Sprintf("- 下一步：%s\n", x.NextHint))
	}
	if x.WaitNote != "" {
		b.WriteString(fmt.Sprintf("- 往返备注：%s\n", x.WaitNote))
	}
	if e.Kind == model.KindError {
		b.WriteString("- **⚠ 失败**：这一步是错误/失败信号。\n")
	}
	if strings.TrimSpace(e.Content) != "" {
		b.WriteString("\n")
		b.WriteString(fence(e.Content))
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// fence 把内容包进代码围栏；内容本身含 ``` 时换用更长的围栏。
func fence(content string) string {
	f := "```"
	for strings.Contains(content, f) {
		f += "`"
	}
	return f + "\n" + strings.TrimRight(content, "\n") + "\n" + f
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return strings.TrimSpace(s)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
