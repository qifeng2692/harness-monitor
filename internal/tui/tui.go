// Package tui 提供 bubbletea 三级导航界面：
//
//	harness 层 → 会话层 → 步骤时间线层（enter 再展开单步全文详情）
//
// 数据全部来自 internal/store；渲染用 lipgloss；窗口 resize 自适应。
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/qf/harness-monitor/internal/model"
	"github.com/qf/harness-monitor/internal/store"
)

// Level 是三级导航所在层级（外加一个单步全文详情层）。
type Level int

const (
	LevelHarness  Level = iota // 首屏：harness 列表
	LevelSession               // 会话列表
	LevelTimeline              // 会话时间线（步骤列表）
	LevelDetail                // 单步全文（viewport 滚动）
)

// String 便于测试断言与调试。
func (l Level) String() string {
	switch l {
	case LevelSession:
		return "session"
	case LevelTimeline:
		return "timeline"
	case LevelDetail:
		return "detail"
	default:
		return "harness"
	}
}

const (
	defaultWidth  = 100
	defaultHeight = 30
	summaryWidth  = 80 // 时间线列表里 content 首行摘要的截断宽度（rune）
)

// canonicalHarnesses 是首屏固定展示的五个 harness（无数据时也列出，便于发现漏采）。
var canonicalHarnesses = []string{"hermes", "codex", "claude", "pi", "magpie"}

// kindColor 是各事件 kind 的展示颜色（口径见 docs/DESIGN.md）。
var kindColor = map[string]string{
	model.KindTurnStart:    "51",  // 青
	model.KindLLMResponse:  "42",  // 绿
	model.KindLLMReasoning: "140", // 灰紫
	model.KindToolCall:     "220", // 黄
	model.KindToolResult:   "39",  // 蓝
	model.KindLLMUsage:     "245", // 灰
	model.KindError:        "196", // 红
	model.KindSessionStart: "213",
	model.KindSessionMeta:  "177",
	model.KindTurnEnd:      "245",
	model.KindSessionEnd:   "245",
	model.KindCompaction:   "180",
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	selectedStyle = lipgloss.NewStyle().Bold(true)
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func kindStyle(kind string) lipgloss.Style {
	c, ok := kindColor[kind]
	if !ok {
		c = "252"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c))
}

// Model 是三级导航的 bubbletea 模型。
type Model struct {
	store *store.Store

	level  Level
	width  int
	height int

	harnesses []store.HarnessSummary
	hIdx      int

	allSessions  []model.Session
	sessions     []model.Session
	sessIdx      int
	sessOff      int
	filtering    bool
	filterInput  string
	filterAnchor string // 进入过滤前的选中会话（取消过滤时恢复）

	harness string
	session model.Session
	events  []model.Event
	evIdx   int
	evOff   int

	detailOff int

	showHelp bool
	status   string
	err      error
}

// NewModel 构造 TUI 模型并加载首屏数据（测试直接使用）。
func NewModel(s *store.Store) *Model {
	m := &Model{store: s, width: defaultWidth, height: defaultHeight}
	m.loadHarnesses()
	return m
}

// Run 启动 TUI，阻塞直到用户退出。
func Run(s *store.Store) error {
	_, err := tea.NewProgram(NewModel(s), tea.WithAltScreen()).Run()
	return err
}

func (m *Model) Init() tea.Cmd { return nil }

// ---------- 数据加载 ----------

func (m *Model) loadHarnesses() {
	if m.store == nil {
		return
	}
	sums, err := m.store.Harnesses()
	if err != nil {
		m.err = err
		return
	}
	byName := make(map[string]store.HarnessSummary, len(sums))
	for _, s := range sums {
		byName[s.Harness] = s
	}
	out := make([]store.HarnessSummary, 0, len(canonicalHarnesses)+len(sums))
	for _, name := range canonicalHarnesses {
		s := byName[name]
		s.Harness = name
		out = append(out, s)
		delete(byName, name)
	}
	// 库中出现的非规范 harness 也列出，保证数据可见。
	extra := make([]string, 0, len(byName))
	for name := range byName {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	for _, name := range extra {
		out = append(out, byName[name])
	}
	m.harnesses = out
	if m.hIdx >= len(out) {
		m.hIdx = 0
	}
}

func (m *Model) loadSessions() {
	if m.store == nil || m.harness == "" {
		return
	}
	sessions, err := m.store.ListSessions(m.harness)
	if err != nil {
		m.err = err
		return
	}
	m.allSessions = sessions
	m.applyFilter()
}

// applyFilter 按 session_id / 标题做前缀匹配（大小写不敏感）。
//
// 选中项按“当前高亮 → 过滤前锚点 → 第一条”的优先级保留，避免过滤/取消过滤后
// 光标跳回列表头部（否则用户选中的会话会丢失）。
func (m *Model) applyFilter() {
	var cur string
	if m.sessIdx >= 0 && m.sessIdx < len(m.sessions) {
		cur = m.sessions[m.sessIdx].SessionID
	}
	q := strings.ToLower(strings.TrimSpace(m.filterInput))
	if q == "" {
		m.sessions = m.allSessions
	} else {
		out := make([]model.Session, 0, len(m.allSessions))
		for _, s := range m.allSessions {
			if strings.HasPrefix(strings.ToLower(s.SessionID), q) ||
				strings.HasPrefix(strings.ToLower(s.Title), q) {
				out = append(out, s)
			}
		}
		m.sessions = out
	}
	for _, want := range []string{cur, m.filterAnchor} {
		if want == "" {
			continue
		}
		for i, s := range m.sessions {
			if s.SessionID == want {
				m.sessIdx = i
				m.ensureVisible(&m.sessOff, m.sessIdx, len(m.sessions))
				return
			}
		}
	}
	m.sessIdx, m.sessOff = 0, 0
}

func (m *Model) loadEvents() {
	if m.store == nil || m.harness == "" || m.session.SessionID == "" {
		return
	}
	events, err := m.store.ListEvents(m.harness, m.session.SessionID)
	if err != nil {
		m.err = err
		return
	}
	m.events = events
	if m.evIdx >= len(events) {
		m.evIdx = len(events) - 1
	}
	if m.evIdx < 0 {
		m.evIdx = 0
	}
	m.ensureVisible(&m.evOff, m.evIdx, len(events))
}

func (m *Model) refresh() {
	switch m.level {
	case LevelSession:
		m.loadSessions()
		m.status = "已刷新会话列表"
	case LevelTimeline, LevelDetail:
		m.loadEvents()
		m.status = "已刷新时间线"
	default:
		m.loadHarnesses()
		m.status = "已刷新 harness 列表"
	}
}

// ---------- 导航 ----------

func (m *Model) enterHarness() {
	if m.hIdx < 0 || m.hIdx >= len(m.harnesses) {
		return
	}
	h := m.harnesses[m.hIdx]
	if h.SessionCount == 0 {
		m.status = h.Harness + "：无会话（先运行 hm collect --harness " + h.Harness + "）"
		return
	}
	m.harness = h.Harness
	m.level = LevelSession
	m.sessIdx, m.sessOff = 0, 0
	m.filtering, m.filterInput = false, ""
	m.loadSessions()
}

func (m *Model) enterSession() {
	if m.sessIdx < 0 || m.sessIdx >= len(m.sessions) {
		return
	}
	m.session = m.sessions[m.sessIdx]
	m.level = LevelTimeline
	m.evIdx, m.evOff = 0, 0
	m.loadEvents()
}

func (m *Model) enterDetail() {
	if len(m.events) == 0 {
		return
	}
	m.level = LevelDetail
	m.detailOff = 0
}

func (m *Model) currentEvent() *model.Event {
	if m.evIdx < 0 || m.evIdx >= len(m.events) {
		return nil
	}
	return &m.events[m.evIdx]
}

func (m *Model) moveHarness(d int) {
	if len(m.harnesses) == 0 {
		return
	}
	m.hIdx += d
	if m.hIdx < 0 {
		m.hIdx = 0
	}
	if m.hIdx >= len(m.harnesses) {
		m.hIdx = len(m.harnesses) - 1
	}
}

func (m *Model) moveSession(d int) {
	if len(m.sessions) == 0 {
		return
	}
	m.sessIdx += d
	if m.sessIdx < 0 {
		m.sessIdx = 0
	}
	if m.sessIdx >= len(m.sessions) {
		m.sessIdx = len(m.sessions) - 1
	}
	m.ensureVisible(&m.sessOff, m.sessIdx, len(m.sessions))
}

func (m *Model) moveEvent(d int) {
	if len(m.events) == 0 {
		return
	}
	m.evIdx += d
	if m.evIdx < 0 {
		m.evIdx = 0
	}
	if m.evIdx >= len(m.events) {
		m.evIdx = len(m.events) - 1
	}
	m.ensureVisible(&m.evOff, m.evIdx, len(m.events))
}

func (m *Model) scrollDetail(d int) {
	off := m.detailOff + d
	if off < 0 {
		off = 0
	}
	if max := m.detailMax(); off > max {
		off = max
	}
	m.detailOff = off
}

// ensureVisible 把 idx 滚入以 off 为起点的 pageSize 窗口。
func (m *Model) ensureVisible(off *int, idx, n int) {
	h := m.pageSize()
	if idx < *off {
		*off = idx
	}
	if idx >= *off+h {
		*off = idx - h + 1
	}
	max := n - h
	if max < 0 {
		max = 0
	}
	if *off > max {
		*off = max
	}
	if *off < 0 {
		*off = 0
	}
}

// ---------- 按键 ----------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		return m.handleFilterKey(msg)
	}
	key := msg.String()
	if m.showHelp {
		if key == "?" || key == "esc" {
			m.showHelp = false
		}
		return m, nil
	}
	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "?":
		m.showHelp = true
		return m, nil
	case "r":
		m.refresh()
		return m, nil
	}

	switch m.level {
	case LevelHarness:
		switch key {
		case "up", "k":
			m.moveHarness(-1)
		case "down", "j":
			m.moveHarness(1)
		case "enter":
			m.enterHarness()
		case "q":
			return m, tea.Quit
		}
	case LevelSession:
		switch key {
		case "up", "k":
			m.moveSession(-1)
		case "down", "j":
			m.moveSession(1)
		case "enter":
			m.enterSession()
		case "esc":
			m.level = LevelHarness
		case "/":
			m.filtering = true
			m.filterInput = ""
			m.filterAnchor = ""
			if m.sessIdx >= 0 && m.sessIdx < len(m.sessions) {
				m.filterAnchor = m.sessions[m.sessIdx].SessionID
			}
		case "q":
			return m, tea.Quit
		}
	case LevelTimeline:
		switch key {
		case "up", "k":
			m.moveEvent(-1)
		case "down", "j":
			m.moveEvent(1)
		case "enter":
			m.enterDetail()
		case "esc":
			m.level = LevelSession
		case "q":
			return m, tea.Quit
		}
	case LevelDetail:
		switch key {
		case "up", "k":
			m.scrollDetail(-1)
		case "down", "j":
			m.scrollDetail(1)
		case "pgup":
			m.scrollDetail(-m.pageSize())
		case "pgdown":
			m.scrollDetail(m.pageSize())
		case "g", "home":
			m.detailOff = 0
		case "G", "end":
			m.detailOff = m.detailMax()
		case "enter", "esc":
			m.level = LevelTimeline
		case "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *Model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.filtering = false
		m.filterInput = ""
		m.applyFilter()
	case tea.KeyEnter:
		m.filtering = false
	case tea.KeyBackspace:
		r := []rune(m.filterInput)
		if len(r) > 0 {
			m.filterInput = string(r[:len(r)-1])
		}
		m.applyFilter()
	case tea.KeyRunes:
		m.filterInput += string(msg.Runes)
		m.applyFilter()
	case tea.KeySpace:
		m.filterInput += " "
		m.applyFilter()
	}
	return m, nil
}

// ---------- 布局 ----------

func (m *Model) listHeight() int {
	h := m.height - 2 // 标题行 + 底部提示行
	if h < 1 {
		h = 1
	}
	return h
}

// pageSize 是列表区可显示的条目数（扣除列头/过滤行）。
func (m *Model) pageSize() int {
	h := m.listHeight()
	switch m.level {
	case LevelHarness:
		h-- // 列头
	case LevelSession:
		h-- // 列头
		if m.filtering {
			h-- // 过滤输入行
		}
	}
	if h < 1 {
		h = 1
	}
	return h
}

func (m *Model) detailMax() int {
	n := len(m.detailLines()) - m.pageSize()
	if n < 0 {
		n = 0
	}
	return n
}

// detailLines 返回详情页的可滚动行（信息头 + content 全文折行）。
func (m *Model) detailLines() []string {
	e := m.currentEvent()
	if e == nil {
		return nil
	}
	w := m.width - 4
	if w < 20 {
		w = 20
	}
	out := []string{
		dimStyle.Render(fmt.Sprintf("时间 %s   kind %s   title %s", fmtTime(e.Time), e.Kind, e.Title)),
	}
	if e.ParentID != "" {
		out = append(out, dimStyle.Render("parent "+e.ParentID))
	}
	if e.Meta != "" {
		out = append(out, dimStyle.Render("meta "+e.Meta))
	}
	out = append(out, dimStyle.Render("── content ──"))
	// 用 ansi.Hardwrap 折行：按显示宽度计（CJK 双宽），并保留 ANSI 转义。
	wrapped := ansi.Hardwrap(e.Content, w, true)
	return append(out, strings.Split(wrapped, "\n")...)
}

// ---------- 渲染 ----------

func (m *Model) View() string {
	if m.width <= 0 {
		m.width = defaultWidth
	}
	if m.height <= 0 {
		m.height = defaultHeight
	}
	if m.showHelp {
		return m.helpView()
	}
	switch m.level {
	case LevelSession:
		return m.sessionView()
	case LevelTimeline:
		return m.timelineView()
	case LevelDetail:
		return m.detailView()
	default:
		return m.harnessView()
	}
}

// frame 组装固定高度的画面：标题行 + 列表区 + 底部提示行。
func (m *Model) frame(crumb string, rows []string, hint string) string {
	h := m.listHeight()
	if len(rows) > h {
		rows = rows[:h]
	}
	for len(rows) < h {
		rows = append(rows, "")
	}
	// 逐行按窗口宽度截断，避免终端折行破坏布局（ansi.Truncate 保留着色序列）。
	limit := m.width - 1
	if limit < 10 {
		limit = 10
	}
	for i, r := range rows {
		rows[i] = ansi.Truncate(r, limit, "…")
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("harness-monitor"))
	if crumb != "" {
		b.WriteString(dimStyle.Render("  ·  " + crumb))
	}
	b.WriteString("\n")
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n")
	b.WriteString(ansi.Truncate(m.footer(hint), limit, "…"))
	return b.String()
}

func (m *Model) footer(hint string) string {
	var parts []string
	if m.status != "" {
		parts = append(parts, dimStyle.Render(m.status))
	}
	parts = append(parts, dimStyle.Render(hint))
	s := strings.Join(parts, dimStyle.Render("  |  "))
	if m.err != nil {
		s = errStyle.Render("错误: "+m.err.Error()) + "  " + s
	}
	return s
}

func (m *Model) harnessView() string {
	rows := []string{
		dimStyle.Render(fmt.Sprintf("  %-12s %8s %8s  %s", "HARNESS", "会话数", "事件数", "最近活动")),
	}
	for i, h := range m.harnesses {
		last := "-"
		if !h.LastTime.IsZero() {
			last = h.LastTime.Local().Format("2006-01-02 15:04:05")
		}
		line := fmt.Sprintf("%-12s %8d %8d  %s", h.Harness, h.SessionCount, h.EventCount, last)
		if i == m.hIdx {
			rows = append(rows, cursorStyle.Render("▶ ")+selectedStyle.Render(line))
		} else {
			rows = append(rows, "  "+line)
		}
	}
	return m.frame("harness", rows, "↑↓ 选择 · enter 进入 · r 刷新 · ? 帮助 · q 退出")
}

func (m *Model) sessionView() string {
	rows := []string{}
	if m.filtering {
		rows = append(rows, cursorStyle.Render("/")+m.filterInput+"█"+
			dimStyle.Render("  前缀匹配 session_id/标题 · enter 确定 · esc 取消"))
	}
	rows = append(rows, dimStyle.Render(fmt.Sprintf("  %-38s %6s  %-19s %-19s %s",
		"SESSION_ID", "事件数", "首次", "末次", "标题")))
	end := m.sessOff + m.pageSize()
	if end > len(m.sessions) {
		end = len(m.sessions)
	}
	for i := m.sessOff; i < end; i++ {
		s := m.sessions[i]
		line := fmt.Sprintf("%-38s %6d  %-19s %-19s %s",
			truncRunes(s.SessionID, 38), s.EventCount,
			fmtTime(s.FirstTime), fmtTime(s.LastTime), truncRunes(s.Title, 40))
		if i == m.sessIdx {
			rows = append(rows, cursorStyle.Render("▶ ")+selectedStyle.Render(line))
		} else {
			rows = append(rows, "  "+line)
		}
	}
	if len(m.sessions) == 0 {
		rows = append(rows, dimStyle.Render("  （无会话）"))
	}
	crumb := fmt.Sprintf("harness: %s  ·  %d 个会话", m.harness, len(m.sessions))
	return m.frame(crumb, rows, "↑↓ 选择 · enter 进入 · / 过滤 · esc 返回 · r 刷新 · ? 帮助 · q 退出")
}

func (m *Model) timelineView() string {
	rows := []string{}
	end := m.evOff + m.pageSize()
	if end > len(m.events) {
		end = len(m.events)
	}
	for i := m.evOff; i < end; i++ {
		e := m.events[i]
		line := fmt.Sprintf("%s  %s  %s  %s",
			e.Time.Local().Format("15:04:05"),
			kindStyle(e.Kind).Render(fmt.Sprintf("%-13s", e.Kind)),
			truncRunes(e.Title, 18),
			truncRunes(oneLine(e.Content), summaryWidth))
		if i == m.evIdx {
			rows = append(rows, cursorStyle.Render("▶ ")+line)
		} else {
			rows = append(rows, "  "+line)
		}
	}
	if len(m.events) == 0 {
		rows = append(rows, dimStyle.Render("  （无事件）"))
	}
	crumb := fmt.Sprintf("%s  ·  %s  ·  %d 步（seq 升序）",
		m.harness, truncRunes(m.session.SessionID, 24), len(m.events))
	return m.frame(crumb, rows, "↑↓ 滚动 · enter 展开全文 · esc 返回 · r 刷新 · ? 帮助 · q 退出")
}

func (m *Model) detailView() string {
	e := m.currentEvent()
	if e == nil {
		return m.frame("详情", []string{dimStyle.Render("  （无事件）")}, "esc 返回")
	}
	lines := m.detailLines()
	end := m.detailOff + m.pageSize()
	if end > len(lines) {
		end = len(lines)
	}
	rows := lines[m.detailOff:end]
	crumb := fmt.Sprintf("%s  ·  %s  ·  第 %d/%d 步  ·  %s",
		m.harness, truncRunes(m.session.SessionID, 20), m.evIdx+1, len(m.events), e.Kind)
	hint := fmt.Sprintf("↑↓ 滚动 · PgUp/PgDn 翻页 · G 到底 · enter/esc 收起全文   [行 %d-%d/%d]",
		m.detailOff+1, end, len(lines))
	return m.frame(crumb, rows, hint)
}

func (m *Model) helpView() string {
	lines := []string{
		titleStyle.Render("harness-monitor 帮助"),
		"",
		selectedStyle.Render("harness 层（首屏）"),
		"  ↑/k ↓/j   选择 harness",
		"  enter     进入该 harness 的会话列表",
		"",
		selectedStyle.Render("会话层"),
		"  ↑/k ↓/j   选择会话",
		"  enter     进入会话时间线",
		"  /         过滤（session_id / 标题前缀匹配）",
		"  esc       返回 harness 层",
		"",
		selectedStyle.Render("时间线层"),
		"  ↑/k ↓/j   滚动步骤",
		"  enter     展开该步 content 全文（viewport）",
		"  esc       返回会话层",
		"",
		selectedStyle.Render("详情层"),
		"  ↑/k ↓/j   滚动 · PgUp/PgDn 翻页 · g/G 顶/底",
		"  enter/esc 收起全文",
		"",
		selectedStyle.Render("全局"),
		"  r 刷新（重新查库） · ? 帮助 · q 退出",
		"",
		dimStyle.Render("按 ? 或 esc 关闭帮助"),
	}
	return strings.Join(lines, "\n")
}

// ---------- 工具 ----------

// truncRunes 按 rune 截断，超出时补省略号。
func truncRunes(s string, n int) string {
	r := []rune(oneLine(s))
	if len(r) <= n {
		return string(r)
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// oneLine 把多行内容压成一行。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
