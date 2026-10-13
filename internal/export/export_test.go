package export

import (
	"strings"
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

func mk(seq int64, kind, title, content string) model.Event {
	return model.Event{
		Harness: "test", SessionID: "s1", Seq: seq,
		Time: time.Date(2026, 10, 12, 9, 30, 0, int(seq), time.UTC),
		Kind: kind, Title: title, Content: content,
	}
}

// mkPair 造一对可配对的工具往返（调用带 call_id、结果带 parent_id）。
func mkPair(callSeq, resultSeq int64, tool, result string) (model.Event, model.Event) {
	call := mk(callSeq, model.KindToolCall, tool, "params...")
	call.ParentID = "call_1"
	call.Meta = `{"call_id":"call_1"}`
	res := mk(resultSeq, model.KindToolResult, "tool result", result)
	res.ParentID = "call_1"
	return call, res
}

func TestSessionBasicStructure(t *testing.T) {
	events := []model.Event{
		mk(1, model.KindSessionStart, "会话开始", ""),
		mk(2, model.KindLLMResponse, "回答", "你好，这是回答。"),
	}
	out := Session("hermes", "测试会话", events[0].Time, events[1].Time, events)

	for _, want := range []string{
		"# 会话导出：测试会话",
		"- Harness：hermes",
		"- 步骤数：2",
		"## 1. [session.start] 会话开始",
		"## 2. [llm.response] 回答",
		"你好，这是回答。",
		"- 说明：",
		"- 输入来自：",
		"- 输出去向：",
		"- 给用户看吗：",
		"- 为什么流转：",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("导出缺少 %q\n---\n%s", want, out)
		}
	}
}

func TestSessionErrorMarked(t *testing.T) {
	events := []model.Event{
		mk(1, model.KindToolCall, "run command", "bash -c \"exit 1\""),
		mk(2, model.KindToolResult, "tool result", "exit code 1"),
		mk(3, model.KindError, "命令失败", "exit_code=1"),
	}
	out := Session("codex", "失败会话", time.Time{}, time.Time{}, events)
	if !strings.Contains(out, "⚠ 失败") {
		t.Errorf("错误步骤应带 ⚠ 失败 标记\n---\n%s", out)
	}
	if !strings.Contains(out, "失败/异常：1 步") {
		t.Errorf("头部应汇总失败步数\n---\n%s", out)
	}
}

func TestSessionToolPairingSurfaces(t *testing.T) {
	// 调用与结果相隔很远：配对信息（第 N 轮、间隔说明）应出现在导出里。
	call, res := mkPair(1, 4, "read file", "file content")
	events := []model.Event{
		call,
		mk(2, model.KindLLMResponse, "思考", "..."),
		mk(3, model.KindLLMReasoning, "推理", "..."),
		res,
	}
	out := Session("pi", "配对会话", time.Time{}, time.Time{}, events)
	if !strings.Contains(out, "工具往返：第 1 轮") {
		t.Errorf("应有工具往返标注\n---\n%s", out)
	}
	if !strings.Contains(out, "往返备注：") {
		t.Errorf("相隔很远的往返应有间隔说明\n---\n%s", out)
	}
}

func TestSessionEmptyAndFenceEscaping(t *testing.T) {
	// 空会话不崩，标题如实标 —。
	out := Session("magpie", "", time.Time{}, time.Time{}, nil)
	if !strings.Contains(out, "# 会话导出：—") {
		t.Errorf("空标题应显示 —\n---\n%s", out)
	}

	// 内容里含 ``` 时围栏要加长，避免把文档截断。
	events := []model.Event{mk(1, model.KindLLMResponse, "回答", "看代码：\n```\nx=1\n```")}
	out = Session("claude", "围栏", time.Time{}, time.Time{}, events)
	if strings.Count(out, "````") < 2 {
		t.Errorf("含 ``` 的内容应使用更长围栏\n---\n%s", out)
	}
}

func TestSessionOutOfOrderInput(t *testing.T) {
	// 调用方乱序传入时按 seq 兜底排序。
	events := []model.Event{
		mk(2, model.KindLLMResponse, "后", "b"),
		mk(1, model.KindSessionStart, "前", "a"),
	}
	out := Session("hermes", "乱序", time.Time{}, time.Time{}, events)
	i1 := strings.Index(out, "## 1. ")
	i2 := strings.Index(out, "## 2. ")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("步骤编号应与 seq 顺序一致\n---\n%s", out)
	}
	if !strings.Contains(out[i1:i2], "前") {
		t.Errorf("第 1 步应是 seq=1 的事件\n---\n%s", out)
	}
}
