package tui

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/qf/harness-monitor/internal/model"
	"github.com/qf/harness-monitor/internal/store"
)

// TestRealDBRender 用真实事件库渲染一遍三级导航（默认跳过）。
//
//	HM_REAL_DB=data/hm.db go test ./internal/tui/ -run TestRealDBRender -v
//
// 它不写库，只读：打开 → 首屏 → 进入 codex → 进入最近会话 → 展开一条 tool.call，
// 断言展开内容与库中 content 完全一致（全文，不截断）。
func TestRealDBRender(t *testing.T) {
	path := os.Getenv("HM_REAL_DB")
	if path == "" {
		t.Skip("未设置 HM_REAL_DB，跳过真实库渲染检查")
	}
	s, err := store.OpenPath(path)
	if err != nil {
		t.Fatalf("打开真实库: %v", err)
	}
	defer s.Close()

	m := NewModel(s)
	m = resize(t, m, 120, 30)
	if len(m.harnesses) < 5 {
		t.Fatalf("harness 列表=%d，期望 ≥5", len(m.harnesses))
	}
	t.Logf("【真实库 · harness 层】\n%s", m.View())

	// 进入 codex
	found := false
	for i, h := range m.harnesses {
		if h.Harness == "codex" {
			m.hIdx = i
			found = true
			break
		}
	}
	if !found {
		t.Fatal("真实库中无 codex")
	}
	m = key(t, m, "enter")
	if m.level != LevelSession || len(m.sessions) == 0 {
		t.Fatalf("codex 会话层为空: level=%v n=%d", m.level, len(m.sessions))
	}
	t.Logf("【真实库 · 会话层（codex，前 3 行）】\n%s", firstLines(m.View(), 5))

	// 进入事件数最多的会话（保证有 tool.call 步骤可展开）
	best := 0
	for i, s := range m.sessions {
		if s.EventCount > m.sessions[best].EventCount {
			best = i
		}
	}
	m.sessIdx = best
	m = key(t, m, "enter")
	if m.level != LevelTimeline || len(m.events) == 0 {
		t.Fatalf("时间线为空: level=%v n=%d", m.level, len(m.events))
	}
	t.Logf("【真实库 · 时间线层（%s，前 8 行）】\n%s",
		m.session.SessionID, firstLines(m.View(), 10))

	// 定位事件数最多的一条 tool.call 并展开（内容最长，能验证不截断）
	idx, bestLen := -1, 0
	for i, e := range m.events {
		if e.Kind == model.KindToolCall && len(e.Content) > bestLen {
			idx, bestLen = i, len(e.Content)
		}
	}
	if idx < 0 {
		t.Skip("该会话无 tool.call 事件")
	}
	m.evIdx = idx
	m.ensureVisible(&m.evOff, m.evIdx, len(m.events))
	m = key(t, m, "enter")
	if m.level != LevelDetail {
		t.Fatalf("期望 detail，得到 %v", m.level)
	}
	want := m.events[idx].Content
	// 只比对 content 段（detailLines 前面还有时间/kind/meta 头）
	lines := m.detailLines()
	start := 0
	for i, l := range lines {
		if strings.Contains(stripANSI(l), "── content ──") {
			start = i + 1
			break
		}
	}
	got := stripANSI(strings.Join(lines[start:], "\n"))
	// 折行切断了换行，去掉空白后必须与原文一致
	if compact(got) != compact(want) {
		t.Fatalf("展开全文与库中 content 不一致：库 %d 字符，展开 %d 字符\n库: %.200s\n展开: %.200s",
			len([]rune(want)), len([]rune(got)), want, got)
	}
	// 滚到底部，确认 viewport 能走到全文末尾
	m = key(t, m, "G")
	bottom := stripANSI(m.View())
	last := compact(want)
	if tail := last[len(last)-min(24, len(last)):]; !strings.Contains(compact(bottom), tail) {
		t.Fatalf("滚到底部看不到全文末尾：want 尾部 %.40q", tail)
	}
	t.Logf("【真实库 · 展开 tool.call 全文 %d 字符 / %d 行，前 6 行】\n%s",
		len([]rune(want)), len(lines)-start,
		firstLines(strings.Join(m.detailLines(), "\n"), 6))
	t.Logf("【真实库 · 滚到底部（viewport 尾部）】\n%s", firstLines(bottom, 6))

	// esc 返回两层 → q 退出
	m = key(t, m, "esc")
	if m.level != LevelTimeline {
		t.Fatalf("esc 后期望 timeline，得到 %v", m.level)
	}
	m = key(t, m, "esc")
	if m.level != LevelSession {
		t.Fatalf("esc 后期望 session，得到 %v", m.level)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q 未退出")
	}
}

// compact 去掉所有空白，用于比对被折行/着色处理过的全文。
func compact(s string) string {
	return strings.Join(strings.Fields(s), "")
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
