package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/qf/harness-monitor/internal/store"
)

// TestRealDBSearchReplayLive 用真实事件库（HM_REAL_DB）端到端跑 M1-8 三件套：
//
//	HM_REAL_DB=data/hm.db go test ./internal/tui/ -run TestRealDBSearchReplayLive -v
//
// 只读库；搜索对真实关键词断言命中并 enter 定位；replay 断言高亮事件随 n/p 变化；
// live 断言增量刷新被调度。渲染样例打到 t.Logf 供人工核对。
func TestRealDBSearchReplayLive(t *testing.T) {
	path := os.Getenv("HM_REAL_DB")
	if path == "" {
		t.Skip("未设置 HM_REAL_DB，跳过真实库 M1-8 检查")
	}
	s, err := store.OpenPath(path)
	if err != nil {
		t.Fatalf("打开真实库: %v", err)
	}
	defer s.Close()

	m := resize(t, NewModel(s), 140, 34)

	// ---------- 搜索：对真实关键词 "kanban" ----------
	m = key(t, m, "/")
	for _, c := range "kanban" {
		m = key(t, m, string(c))
	}
	if len(m.searchHits) == 0 {
		t.Fatalf("搜索 kanban 无命中（真实库应含大量 kanban）")
	}
	t.Logf("【真实库·搜索 %q 命中 %d 条，渲染样例】\n%s", m.searchInput, len(m.searchHits), firstLines(m.View(), 12))

	// 找一个命中内容确实含关键词的条目并 enter 定位
	wantIdx := -1
	for i, h := range m.searchHits {
		if strings.Contains(strings.ToLower(h.Content), "kanban") || strings.Contains(strings.ToLower(h.Title), "kanban") {
			wantIdx = i
			break
		}
	}
	if wantIdx < 0 {
		t.Fatal("命中列表中没有 content/title 含 kanban 的条目")
	}
	m.searchIdx = wantIdx
	want := m.searchHits[wantIdx]
	m = key(t, m, "enter")

	if m.level != LevelTimeline {
		t.Fatalf("enter 后应进入时间线，得到 %v", m.level)
	}
	if m.harness != want.Harness || m.session.SessionID != want.SessionID {
		t.Fatalf("定位会话错误: 期望 %s/%s，得到 %s/%s",
			want.Harness, want.SessionID, m.harness, m.session.SessionID)
	}
	cur := m.currentEvent()
	if cur == nil || cur.Seq != want.Seq {
		t.Fatalf("未定位到命中事件 seq=%d: %+v", want.Seq, cur)
	}
	if !strings.Contains(strings.ToLower(cur.Content+cur.Title), "kanban") {
		t.Fatalf("定位到的事件内容不含关键词: %.120q", cur.Content)
	}
	t.Logf("【真实库·搜索定位 → %s/%s 第 %d 步 · %s】\n%s",
		m.harness, truncRunes(m.session.SessionID, 24), cur.Seq, cur.Kind, firstLines(m.View(), 8))

	// ---------- replay：在命中会话上 space → n → n → p（先于离开命中时间线）----------
	if len(m.events) < 3 {
		t.Fatalf("命中会话事件数 %d < 3，无法回放", len(m.events))
	}
	m = key(t, m, " ")
	if !m.replay {
		t.Fatal("space 未进入回放")
	}
	if m.replayIdx != 0 {
		t.Fatalf("回放应从第 1 步开始，idx=%d", m.replayIdx)
	}
	seq0 := m.replayEvent().Seq
	m = key(t, m, "n")
	seq1 := m.replayEvent().Seq
	m = key(t, m, "n")
	seq2 := m.replayEvent().Seq
	m = key(t, m, "p")
	seqBack := m.replayEvent().Seq
	if !(seq0 < seq1 && seq1 < seq2) {
		t.Fatalf("n 未按序前进: %d → %d → %d", seq0, seq1, seq2)
	}
	if seqBack != seq1 {
		t.Fatalf("p 未回到上一步: 期望 seq=%d，得到 %d", seq1, seqBack)
	}
	// 回放当前步全文展开：内容原文出现在渲染里
	ev := m.replayEvent()
	probe := compact(ev.Content)
	if len(probe) > 0 {
		tail := probe
		if len(tail) > 40 {
			tail = tail[:40]
		}
		if !strings.Contains(compact(m.View()), tail) {
			t.Fatalf("回放未展开当前步全文（找不到内容前缀 %.40q）:\n%s", tail, m.View())
		}
	}
	t.Logf("【真实库·回放 第 %d/%d 步 · seq=%d · %s】\n%s",
		m.replayIdx+1, len(m.events), ev.Seq, ev.Kind, firstLines(m.View(), 10))
	m = key(t, m, "esc")
	if m.replay {
		t.Fatal("esc 未退出回放")
	}

	// ---------- live：仍在时间线层，打开 live 断言增量刷新被调度 ----------
	if m.level != LevelTimeline {
		t.Fatalf("退出回放后应在时间线层，得到 %v", m.level)
	}
	m = key(t, m, "l")
	if !m.live {
		t.Fatal("l 未开启 live")
	}
	if m.refreshCount == 0 || m.lastRefresh.IsZero() {
		t.Fatal("live 开启后未记录刷新")
	}
	before := m.refreshCount
	next, _ := m.Update(liveTickMsg{})
	m = next.(*Model)
	if m.refreshCount != before+1 {
		t.Fatalf("liveTick 未触发刷新: %d → %d", before, m.refreshCount)
	}
	if !strings.Contains(m.View(), "LIVE") {
		t.Errorf("标题栏缺少 LIVE 指示:\n%s", m.View())
	}
	t.Logf("【真实库·live 跟随（harness=%s，已刷新 %d 次）】\n%s",
		m.harness, m.refreshCount, firstLines(m.View(), 8))

	// esc 返回搜索列表，再 esc 退出搜索
	m = key(t, m, "esc")
	if !m.searching {
		t.Fatal("esc 未返回搜索列表")
	}
	m = key(t, m, "esc")
	if m.searching {
		t.Fatal("第二次 esc 未退出搜索")
	}
}
