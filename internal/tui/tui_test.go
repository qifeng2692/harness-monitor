package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/qf/harness-monitor/internal/model"
	"github.com/qf/harness-monitor/internal/store"
)

// 已知长内容里的标记：位于 80 字符摘要窗口之外，只有展开全文才能看到。
const (
	paramMarker  = "PARAM-FULL-MARKER-3f9c"
	resultMarker = "RESULT-FULL-MARKER-77ab"
)

func longContent(prefix string, n int) string {
	return strings.Repeat(prefix, n)
}

// newFixtureStore 建一个临时库，塞入两个 harness 的固定事件。
func newFixtureStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	base := time.Date(2026, 9, 16, 7, 25, 26, 0, time.UTC)
	ev := func(harness, session string, seq int, kind, title, content string) model.Event {
		return model.Event{
			Harness:   harness,
			SessionID: session,
			Seq:       int64(seq),
			Time:      base.Add(time.Duration(seq) * time.Second),
			Kind:      kind,
			Title:     title,
			Content:   content,
			SourceKey: harness + ":" + session + ":" + title + ":" + kind,
		}
	}

	codexEvents := []model.Event{
		ev("codex", "sess-long", 1, model.KindSessionStart, "codeX", ""),
		ev("codex", "sess-long", 2, model.KindTurnStart, "user", "请把这一版 UI 重做一遍"),
		ev("codex", "sess-long", 3, model.KindToolCall, "exec_command",
			longContent("参数", 1000)+paramMarker),
		ev("codex", "sess-long", 4, model.KindToolResult, "tool result",
			longContent("结果", 1000)+resultMarker),
		ev("codex", "sess-long", 5, model.KindLLMResponse, "assistant", "已重做完成"),
	}
	// 第二个 codex 会话，用于验证选择/过滤。
	codexB := []model.Event{
		ev("codex", "sess-other", 1, model.KindSessionStart, "codeX", ""),
		ev("codex", "sess-other", 2, model.KindTurnStart, "user", "另一个会话"),
	}
	hermesEvents := []model.Event{
		ev("hermes", "h-1", 1, model.KindSessionStart, "hermes", ""),
		ev("hermes", "h-1", 2, model.KindLLMUsage, "usage", `{"tokens":123}`),
	}

	// 让 sess-long 成为最近活动（会话层按最近活动倒序，enter 后选中它）。
	for _, batch := range [][]model.Event{codexB, hermesEvents, codexEvents} {
		if _, _, err := s.InsertEvents(batch); err != nil {
			t.Fatalf("InsertEvents: %v", err)
		}
	}
	return s
}

func key(t *testing.T, m *Model, k string) *Model {
	t.Helper()
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		msg = tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		msg = tea.KeyMsg{Type: tea.KeyRight}
	case "space":
		msg = tea.KeyMsg{Type: tea.KeySpace}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	next, _ := m.Update(msg)
	nm, ok := next.(*Model)
	if !ok {
		t.Fatalf("Update 返回非 *Model: %T", next)
	}
	return nm
}

func resize(t *testing.T, m *Model, w, h int) *Model {
	t.Helper()
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(*Model)
}

// ---------- 视图渲染 ----------

func TestHarnessViewShowsAllFiveHarnesses(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 100, 24)
	v := m.View()

	if !strings.Contains(v, "harness-monitor") {
		t.Fatalf("缺少标题:\n%s", v)
	}
	// 五个 harness 全部出现（无数据的也列出）
	for _, name := range []string{"hermes", "codex", "claude", "pi", "magpie"} {
		if !strings.Contains(v, name) {
			t.Errorf("harness 列表缺少 %q:\n%s", name, v)
		}
	}
	// codex 行含会话数与事件数
	if !strings.Contains(v, "5") {
		t.Errorf("codex 事件数未显示:\n%s", v)
	}
	// 最近活动时间
	if !strings.Contains(v, "2026-09-16") {
		t.Errorf("最近活动时间未显示:\n%s", v)
	}
	t.Logf("【harness 层渲染样例】\n%s", v)
}

func TestSessionViewColumns(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 110, 20)
	m = key(t, m, "down") // hermes -> codex
	m = key(t, m, "enter")

	if m.level != LevelSession {
		t.Fatalf("enter 后 level=%v，期望 session", m.level)
	}
	if m.harness != "codex" {
		t.Fatalf("harness=%q，期望 codex", m.harness)
	}
	if len(m.sessions) != 2 {
		t.Fatalf("codex 会话数=%d，期望 2", len(m.sessions))
	}
	v := m.View()
	for _, want := range []string{"SESSION_ID", "sess-long", "sess-other", "事件数", "首次", "末次"} {
		if !strings.Contains(v, want) {
			t.Errorf("会话层缺少 %q:\n%s", want, v)
		}
	}
	// 标题来自 session.start 事件
	if !strings.Contains(v, "codeX") {
		t.Errorf("会话标题（来自 session.start）未显示:\n%s", v)
	}
	t.Logf("【会话层渲染样例】\n%s", v)
}

func TestTimelineViewKindAndSummary(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 120, 20)
	m = key(t, m, "down")
	m = key(t, m, "enter") // -> session
	m = key(t, m, "enter") // -> timeline（sess-long，最近活动）
	if m.level != LevelTimeline {
		t.Fatalf("level=%v，期望 timeline", m.level)
	}
	if m.session.SessionID != "sess-long" {
		t.Fatalf("session=%q，期望 sess-long", m.session.SessionID)
	}
	if len(m.events) != 5 {
		t.Fatalf("事件数=%d，期望 5", len(m.events))
	}
	// seq 升序
	for i := 1; i < len(m.events); i++ {
		if m.events[i].Seq <= m.events[i-1].Seq {
			t.Fatalf("seq 非升序: %v", m.events)
		}
	}
	v := m.View()
	for _, want := range []string{"turn.start", "tool.call", "tool.result", "llm.response", "exec_command"} {
		if !strings.Contains(v, want) {
			t.Errorf("时间线缺少 %q:\n%s", want, v)
		}
	}
	// 摘要截断到 ~80 字符：标记不应出现在列表行
	if strings.Contains(v, paramMarker) {
		t.Errorf("时间线摘要未截断（出现了全文标记）:\n%s", v)
	}
	t.Logf("【时间线层渲染样例】\n%s", v)
}

// ---------- 导航 ----------

func TestNavigationEnterEscBackToHarness(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 100, 24)

	m = key(t, m, "down")  // -> codex
	m = key(t, m, "enter") // -> 会话层
	m = key(t, m, "enter") // -> 时间线
	if m.level != LevelTimeline {
		t.Fatalf("期望 timeline，得到 %v", m.level)
	}
	m = key(t, m, "esc")
	if m.level != LevelSession {
		t.Fatalf("esc 后期望 session，得到 %v", m.level)
	}
	m = key(t, m, "esc")
	if m.level != LevelHarness {
		t.Fatalf("esc 后期望 harness，得到 %v", m.level)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q 未返回退出命令")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("q 返回的不是 QuitMsg: %T", cmd())
	}
}

func TestHarnessWithoutDataDoesNotEnter(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 100, 24)
	// claude / pi / magpie 无数据；从 codex 再 down 两次到 claude
	m = key(t, m, "down") // codex
	m = key(t, m, "down") // claude
	if m.harnesses[m.hIdx].Harness != "claude" {
		t.Fatalf("期望停在 claude，得到 %s", m.harnesses[m.hIdx].Harness)
	}
	m = key(t, m, "enter")
	if m.level != LevelHarness {
		t.Fatalf("空 harness enter 不应进入，level=%v", m.level)
	}
	if !strings.Contains(m.status, "无会话") {
		t.Fatalf("未给出无会话提示: %q", m.status)
	}
}

func TestSessionFilterPrefixMatch(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 100, 24)
	m = key(t, m, "down")
	m = key(t, m, "enter")
	if len(m.sessions) != 2 {
		t.Fatalf("过滤前会话数=%d，期望 2", len(m.sessions))
	}
	m = key(t, m, "F")
	if !m.filtering {
		t.Fatal("F 未进入过滤输入")
	}
	for _, c := range "sess-ot" {
		m = key(t, m, string(c))
	}
	if len(m.sessions) != 1 || m.sessions[0].SessionID != "sess-other" {
		t.Fatalf("前缀过滤结果错误: %+v", m.sessions)
	}
	if !strings.Contains(m.View(), "sess-other") {
		t.Errorf("过滤后视图未显示目标会话:\n%s", m.View())
	}
	m = key(t, m, "esc") // 取消过滤
	if len(m.sessions) != 2 {
		t.Fatalf("取消过滤后会话数=%d，期望 2", len(m.sessions))
	}
}

// 过滤前后选中项不应被重置到列表头部。
func TestFilterKeepsSelection(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 100, 24)
	m = key(t, m, "down")
	m = key(t, m, "enter")
	m = key(t, m, "down") // 选中 sess-other
	if m.sessions[m.sessIdx].SessionID != "sess-other" {
		t.Fatalf("未选中 sess-other: %+v", m.sessions[m.sessIdx])
	}
	m = key(t, m, "F")
	m = key(t, m, "s")
	if m.sessions[m.sessIdx].SessionID != "sess-other" {
		t.Fatalf("过滤后选中项被重置: %+v", m.sessions[m.sessIdx])
	}
	m = key(t, m, "esc") // 取消过滤
	if m.sessions[m.sessIdx].SessionID != "sess-other" {
		t.Fatalf("取消过滤后未恢复选中项: %+v", m.sessions[m.sessIdx])
	}
}

// ---------- 步骤内容展开（核心验收） ----------

func TestExpandToolCallAndToolResultFullContent(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 100, 20)
	m = key(t, m, "down")
	m = key(t, m, "enter")
	m = key(t, m, "enter")

	// 定位 tool.call（seq=3，索引 2）
	m = key(t, m, "down")
	m = key(t, m, "down")
	cur := m.currentEvent()
	if cur == nil || cur.Kind != model.KindToolCall {
		t.Fatalf("未定位到 tool.call: %+v", cur)
	}

	m = key(t, m, "enter")
	if m.level != LevelDetail {
		t.Fatalf("enter 后期望 detail，得到 %v", m.level)
	}
	lines := m.detailLines()
	// 折行会切断标记本身，比对时去掉换行
	joined := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(joined, paramMarker) {
		t.Fatalf("展开的 tool.call 全文缺少末尾标记（内容被截断）:\n%s", joined)
	}
	// 全文按宽度折行：应有多行，且总长覆盖原 content
	if len(lines) < 20 {
		t.Fatalf("详情行数=%d，长内容未折行展开", len(lines))
	}
	// viewport：滚到底部应能看到全文末尾标记
	m = key(t, m, "G")
	if !strings.Contains(stripANSI(m.View()), paramMarker) {
		t.Errorf("详情视图滚到底部仍看不到全文标记（viewport 未生效）:\n%s", m.View())
	}
	t.Logf("【tool.call 展开样例（前 12 行）】\n%s",
		strings.Join(lines[:min(12, len(lines))], "\n"))

	// 返回时间线，展开 tool.result
	m = key(t, m, "esc")
	if m.level != LevelTimeline {
		t.Fatalf("esc 后期望 timeline，得到 %v", m.level)
	}
	m = key(t, m, "down")
	cur = m.currentEvent()
	if cur == nil || cur.Kind != model.KindToolResult {
		t.Fatalf("未定位到 tool.result: %+v", cur)
	}
	m = key(t, m, "enter")
	resLines := m.detailLines()
	if !strings.Contains(stripANSI(strings.Join(resLines, "\n")), resultMarker) {
		t.Fatalf("展开的 tool.result 全文缺少末尾标记（内容被截断）:\n%s",
			strings.Join(resLines, "\n"))
	}
	m = key(t, m, "G")
	if !strings.Contains(stripANSI(m.View()), resultMarker) {
		t.Errorf("tool.result 详情滚到底部看不到全文标记:\n%s", m.View())
	}
	t.Logf("【tool.result 展开样例（前 12 行）】\n%s",
		strings.Join(resLines[:min(12, len(resLines))], "\n"))
}

// stripANSI 去掉 ANSI 转义序列与换行，用于比对被折行/着色切断的标记。
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // ESC
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && !(s[i] >= '@' && s[i] <= '~') {
					i++
				}
				if i < len(s) {
					i++ // 吞掉终止字节
				}
			}
			continue
		}
		switch s[i] {
		case '\r', '\n':
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestDetailScroll(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 100, 20)
	m = key(t, m, "down")
	m = key(t, m, "enter")
	m = key(t, m, "enter")
	m = key(t, m, "down")
	m = key(t, m, "down")
	m = key(t, m, "enter") // detail

	if m.detailOff != 0 {
		t.Fatalf("进入详情应停在顶部，off=%d", m.detailOff)
	}
	m = key(t, m, "G")
	if m.detailOff == 0 {
		t.Fatal("G 未跳到末尾")
	}
	bottom := m.detailOff
	m = key(t, m, "g")
	if m.detailOff != 0 {
		t.Fatalf("g 未回到顶部，off=%d", m.detailOff)
	}
	// 越界不 panic、不越界
	for i := 0; i < 100; i++ {
		m = key(t, m, "down")
	}
	if m.detailOff > bottom {
		t.Fatalf("下滚越界: off=%d > max=%d", m.detailOff, bottom)
	}
}

func TestGlobalHelpAndRefresh(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 100, 24)
	m = key(t, m, "?")
	if !m.showHelp || !strings.Contains(m.View(), "帮助") {
		t.Fatal("? 未打开帮助")
	}
	m = key(t, m, "esc")
	if m.showHelp {
		t.Fatal("esc 未关闭帮助")
	}
	m = key(t, m, "r")
	if !strings.Contains(m.status, "刷新") {
		t.Fatalf("r 未触发刷新: %q", m.status)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- teatest 端到端：harness → codex → 会话 → 时间线 → 展开 → esc×2 → q ----------

func TestTeatestFullWalkthrough(t *testing.T) {
	s := newFixtureStore(t)
	m := NewModel(s)

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(110, 28))

	// 首屏：五个 harness
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(string(b), "magpie") && strings.Contains(string(b), "codex")
	}, teatest.WithDuration(5*time.Second))

	// harness 层：hermes -> codex
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(string(b), "sess-long") && strings.Contains(string(b), "个会话")
	}, teatest.WithDuration(5*time.Second))

	// 会话层 -> 时间线
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(string(b), "tool.call") && strings.Contains(string(b), "exec_command")
	}, teatest.WithDuration(5*time.Second))

	// 时间线：下移到 tool.call（索引 2）后展开全文
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	// 进入详情层（第 3/5 步 · tool.call）
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(stripANSI(string(b)), "第 3/5 步") &&
			strings.Contains(stripANSI(string(b)), "tool.call")
	}, teatest.WithDuration(5*time.Second))
	// 滚到底部，应看到长参数全文的末尾标记
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(stripANSI(string(b)), paramMarker)
	}, teatest.WithDuration(5*time.Second))

	// 收起 -> esc -> esc -> q
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(string(b), "HARNESS")
	}, teatest.WithDuration(5*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))

	final := tm.FinalModel(t).(*Model)
	if final.level != LevelHarness {
		t.Fatalf("退出时 level=%v，期望 harness", final.level)
	}
}
