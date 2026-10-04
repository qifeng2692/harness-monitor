package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/qf/harness-monitor/internal/model"
)

// ---------- replay 回放 ----------

// space → n → n → p 后高亮事件应正确变化（从第 1 步起，逐事件前进/后退）。
func TestReplaySpaceNNP(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 120, 24)
	m = key(t, m, "down")  // codex
	m = key(t, m, "enter") // 会话层
	m = key(t, m, "enter") // 时间线（sess-long，5 步）

	if m.level != LevelTimeline || len(m.events) != 5 {
		t.Fatalf("前置状态错误: level=%v n=%d", m.level, len(m.events))
	}
	m = key(t, m, "space")
	if !m.replay {
		t.Fatal("space 未进入回放模式")
	}
	if m.replayIdx != 0 {
		t.Fatalf("回放应从第 1 个事件开始，idx=%d", m.replayIdx)
	}
	if got := m.replayEvent(); got == nil || got.Seq != 1 {
		t.Fatalf("回放第 1 步应为 seq=1，得到 %+v", got)
	}
	// 回放视图应展开当前事件全文（含 kind）
	v := m.View()
	if !strings.Contains(v, "回放") || !strings.Contains(v, "session.start") {
		t.Fatalf("回放视图未显示当前步全文:\n%s", v)
	}

	m = key(t, m, "n")
	if m.replayIdx != 1 || m.replayEvent().Seq != 2 {
		t.Fatalf("n 后应到第 2 步(seq=2)，idx=%d ev=%+v", m.replayIdx, m.replayEvent())
	}
	m = key(t, m, "n")
	if m.replayIdx != 2 || m.replayEvent().Seq != 3 {
		t.Fatalf("n 后应到第 3 步(seq=3)，idx=%d ev=%+v", m.replayIdx, m.replayEvent())
	}
	m = key(t, m, "p")
	if m.replayIdx != 1 || m.replayEvent().Seq != 2 {
		t.Fatalf("p 后应回到第 2 步(seq=2)，idx=%d ev=%+v", m.replayIdx, m.replayEvent())
	}

	// 回放当前步全文展开：第 2 步(seq=2)是用户输入
	if !strings.Contains(stripANSI(m.View()), "请把这一版 UI 重做一遍") {
		t.Fatalf("回放未展开当前步全文:\n%s", m.View())
	}

	// esc 退出回放，回到普通浏览（停在当前步）
	m = key(t, m, "esc")
	if m.replay {
		t.Fatal("esc 未退出回放")
	}
	if m.evIdx != 1 {
		t.Fatalf("退出回放应停在当前步 idx=1，得到 %d", m.evIdx)
	}
}

// 回放到边界应停住（不越界、不 panic）。
func TestReplayBounds(t *testing.T) {
	m := resize(t, NewModel(newFixtureStore(t)), 120, 24)
	m = key(t, m, "down")
	m = key(t, m, "enter")
	m = key(t, m, "enter")
	m = key(t, m, "space")
	for i := 0; i < 50; i++ {
		m = key(t, m, "p")
	}
	if m.replayIdx != 0 {
		t.Fatalf("p 越界: idx=%d", m.replayIdx)
	}
	for i := 0; i < 50; i++ {
		m = key(t, m, "n")
	}
	if m.replayIdx != len(m.events)-1 {
		t.Fatalf("n 越界: idx=%d max=%d", m.replayIdx, len(m.events)-1)
	}
}

// ---------- 搜索 ----------

// / 搜索已知关键词 → 命中列表非空 → enter 定位到对应会话的该事件。
func TestSearchHitAndLocate(t *testing.T) {
	s := newFixtureStore(t)
	m := resize(t, NewModel(s), 120, 24)

	m = key(t, m, "/")
	if !m.searching {
		t.Fatal("/ 未进入搜索输入")
	}
	if !m.searchAll {
		t.Fatal("默认应为全局搜索")
	}
	// 输入 "重做"（仅出现在 codex/sess-long 的 seq=2 用户输入里）
	for _, c := range "重做" {
		m = key(t, m, string(c))
	}
	if len(m.searchHits) == 0 {
		t.Fatalf("搜索“重做”应命中，得到 0 条（输入=%q）", m.searchInput)
	}
	// 断言命中定位信息
	h := m.searchHits[0]
	if h.Harness != "codex" || h.SessionID != "sess-long" || h.Seq != 2 {
		t.Fatalf("命中定位错误: %+v", h)
	}
	if !strings.Contains(m.View(), "命中") {
		t.Errorf("搜索视图未显示命中列表:\n%s", m.View())
	}
	t.Logf("【搜索视图渲染样例】\n%s", m.View())

	m = key(t, m, "enter")
	if m.searching {
		t.Fatal("enter 后应退出搜索输入")
	}
	if m.level != LevelTimeline {
		t.Fatalf("enter 后应进入时间线，得到 %v", m.level)
	}
	if m.harness != "codex" || m.session.SessionID != "sess-long" {
		t.Fatalf("未定位到命中会话: harness=%s session=%s", m.harness, m.session.SessionID)
	}
	if cur := m.currentEvent(); cur == nil || cur.Seq != 2 || !strings.Contains(cur.Content, "重做") {
		t.Fatalf("未定位到命中事件: %+v", cur)
	}
	t.Logf("【搜索命中定位后的时间线】\n%s", m.View())

	// esc 返回搜索列表
	m = key(t, m, "esc")
	if !m.searching {
		t.Fatal("esc 未返回搜索列表")
	}
}

// 搜索范围限定：tab 在全局 / 当前 harness 间切换。
func TestSearchScopeToggle(t *testing.T) {
	s := newFixtureStore(t)
	m := resize(t, NewModel(s), 120, 24)
	m = key(t, m, "/")
	m = key(t, m, "s") // "sess" 在两个 harness 都可能命中
	if !m.searchAll {
		t.Fatal("初始应为全局")
	}
	m = key(t, m, "tab")
	if m.searchAll {
		t.Fatal("tab 后应为限定当前 harness")
	}
	if !strings.Contains(m.View(), "当前 harness") {
		t.Errorf("限定范围未显示:\n%s", m.View())
	}
}

// 搜索转义：LIKE 通配符按字面匹配（% 不应匹配任意串）。
func TestSearchLiteralWildcards(t *testing.T) {
	s := newFixtureStore(t)
	m := resize(t, NewModel(s), 120, 24)
	m = key(t, m, "/")
	m = key(t, m, "%")
	if len(m.searchHits) != 0 {
		t.Fatalf("'%%' 应按字面匹配（无字面 %% 内容），却命中 %d 条", len(m.searchHits))
	}
}

// ---------- live 跟随 ----------

// liveTick 消息应触发一次增量刷新；断言 refreshLive 被调度且新增事件后自动滚到底部。
func TestLiveTickRefreshesAndScrolls(t *testing.T) {
	s := newFixtureStore(t)
	// live 刷新会真跑 codex 增量采集：把数据源指向空目录，避免读本机真实 rollout。
	t.Setenv("CODEX_SESSIONS_DIR", t.TempDir())
	m := resize(t, NewModel(s), 120, 24)
	m = key(t, m, "down") // codex
	m = key(t, m, "enter")
	m = key(t, m, "enter") // 时间线 sess-long

	// 打开 live（会立即刷新一次并返回 tick 命令）
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m = next.(*Model)
	if !m.live {
		t.Fatal("l 未开启 live")
	}
	if cmd == nil {
		t.Fatal("开启 live 应返回 tick 命令")
	}
	if m.refreshCount == 0 {
		t.Fatal("开启 live 后应已刷新一次")
	}
	baseCount := m.refreshCount

	// 向当前会话插入一条新事件（模拟采集器增量入库）
	ev := model.Event{
		Harness: "codex", SessionID: "sess-long", Seq: 99,
		Time: time.Now().UTC(), Kind: model.KindLLMResponse,
		Title: "assistant", Content: "LIVE-新增内容-8f2a",
		SourceKey: "codex:test:live:99",
	}
	if _, _, err := s.InsertEvents([]model.Event{ev}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	// 手动派发一次 liveTickMsg（等价于 2s 定时器触发）
	next, _ = m.Update(liveTickMsg{})
	m = next.(*Model)
	if m.refreshCount != baseCount+1 {
		t.Fatalf("liveTick 未触发刷新: refreshCount %d → %d", baseCount, m.refreshCount)
	}
	if m.lastRefresh.IsZero() {
		t.Fatal("live 刷新未记录时间")
	}
	if len(m.events) != 6 {
		t.Fatalf("增量后事件数应为 6，得到 %d", len(m.events))
	}
	if m.evIdx != len(m.events)-1 {
		t.Fatalf("有新增事件时应自动滚到底部: evIdx=%d n=%d", m.evIdx, len(m.events))
	}
	if cur := m.currentEvent(); cur == nil || cur.Seq != 99 {
		t.Fatalf("当前应停在新增事件: %+v", cur)
	}
	// 标题栏显示 LIVE 指示
	if !strings.Contains(m.View(), "LIVE") {
		t.Errorf("标题栏缺少 LIVE 指示:\n%s", m.View())
	}
	t.Logf("【live 跟随渲染样例】\n%s", m.View())

	// 再次按 l 关闭
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m = next.(*Model)
	if m.live {
		t.Fatal("再次按 l 未关闭 live")
	}
	if cmd != nil {
		t.Fatal("关闭 live 不应再返回 tick 命令")
	}
}
