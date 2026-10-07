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

	"github.com/qifeng2692/harness-monitor/internal/collect"
	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
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
	liveStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	hitStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
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

	// live 跟随：每 2 秒增量采集 + 重新查询当前会话事件。
	live         bool
	lastRefresh  time.Time
	refreshCount int
	liveErr      string

	// replay 回放：从第 1 步起逐事件前进/后退，当前事件全文展开。
	replay    bool
	replayIdx int
	replayOff int

	// 搜索：query 输入 → store 命中列表 → enter 定位。
	searching   bool
	searchInput string
	searchAll   bool // true=全局，false=当前 harness
	searchHits  []store.SearchHit
	searchIdx   int
	searchOff   int
	searchErr   string
	prevLevel   Level
	prevSession model.Session
	prevEvents  []model.Event
	prevEvIdx   int
	prevEvOff   int

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

// ---------- live 跟随 ----------

// liveInterval 是 live 模式下的增量采集周期。
const liveInterval = 2 * time.Second

// liveTickMsg 是 live 定时器触发（每 liveInterval 一次）。
type liveTickMsg struct{}

// liveTickCmd 返回一个在 liveInterval 后发出 liveTickMsg 的命令。
func liveTickCmd() tea.Cmd {
	return tea.Tick(liveInterval, func(time.Time) tea.Msg { return liveTickMsg{} })
}

// toggleLive 打开/关闭 live 跟随；打开时立即刷新一次并启动定时器。
func (m *Model) toggleLive() tea.Cmd {
	m.live = !m.live
	if !m.live {
		m.status = "live 已关闭"
		return nil
	}
	m.refreshLive()
	return liveTickCmd()
}

// refreshLive 做一次增量采集（直接调用进程内 registry，按当前 harness 跑），
// 然后重新查询当前会话事件；若 seq 有新增则自动滚到底部。
func (m *Model) refreshLive() {
	if m.store == nil || m.harness == "" {
		m.status = "live：无当前 harness"
		m.lastRefresh = time.Now()
		return
	}
	before := len(m.events)
	if _, err := collect.CollectIncremental(m.harness, m.store); err != nil {
		m.liveErr = err.Error()
	} else {
		m.liveErr = ""
	}
	m.loadEvents()
	m.refreshCount++
	m.lastRefresh = time.Now()
	if m.replay {
		// 回放模式下不打断当前步，只更新可用事件数。
		if m.replayIdx >= len(m.events) {
			m.replayIdx = len(m.events) - 1
		}
		return
	}
	if len(m.events) > before {
		m.evIdx = len(m.events) - 1 // 有新事件：滚到底部
		m.ensureVisible(&m.evOff, m.evIdx, len(m.events))
		m.status = fmt.Sprintf("LIVE 新增 %d 步（%d→%d）", len(m.events)-before, before, len(m.events))
	} else {
		m.status = "LIVE 无新增"
	}
}

// liveStatus 是标题栏的 LIVE 指示文本。
func (m *Model) liveStatus() string {
	if !m.live {
		return ""
	}
	s := "● LIVE"
	if !m.lastRefresh.IsZero() {
		s += " " + m.lastRefresh.Local().Format("15:04:05")
	}
	if m.liveErr != "" {
		s += " 错误:" + m.liveErr
	}
	return s
}

// ---------- replay 回放 ----------

// enterReplay 从第 1 个事件进入回放模式（space）。
func (m *Model) enterReplay() {
	if len(m.events) == 0 {
		m.status = "无可回放事件"
		return
	}
	m.replay = true
	m.replayIdx = 0
	m.replayOff = 0
	m.status = fmt.Sprintf("回放 %d/%d（n/→ 下一步 · p/← 上一步 · esc 退出）", 1, len(m.events))
}

// exitReplay 退出回放，回到普通浏览（停在当前步）。
func (m *Model) exitReplay() {
	m.replay = false
	m.evIdx = m.replayIdx
	if m.evIdx >= len(m.events) {
		m.evIdx = len(m.events) - 1
	}
	if m.evIdx < 0 {
		m.evIdx = 0
	}
	m.ensureVisible(&m.evOff, m.evIdx, len(m.events))
	m.status = "已退出回放"
}

// replayMove 在回放中前进/后退 d 步（到边界即停）。
func (m *Model) replayMove(d int) {
	if !m.replay || len(m.events) == 0 {
		return
	}
	i := m.replayIdx + d
	if i < 0 {
		i = 0
	}
	if i >= len(m.events) {
		i = len(m.events) - 1
	}
	m.replayIdx = i
	m.replayOff = 0 // 每步从顶部显示全文
	m.status = fmt.Sprintf("回放 %d/%d", i+1, len(m.events))
}

// replayEvent 返回回放当前步的事件。
func (m *Model) replayEvent() *model.Event {
	if !m.replay || m.replayIdx < 0 || m.replayIdx >= len(m.events) {
		return nil
	}
	return &m.events[m.replayIdx]
}

// ---------- 搜索 ----------

// enterSearch 进入搜索输入（/ 任意层，或 f 在 harness 层）。
// global=true 表示全局搜索；false 表示限定当前 harness。
func (m *Model) enterSearch(global bool) {
	m.searching = true
	m.searchInput = ""
	m.searchHits = nil
	m.searchIdx, m.searchOff = 0, 0
	m.searchErr = ""
	m.searchAll = global
	m.status = ""
}

// scopeHarness 返回“限定当前 harness”搜索时应使用的 harness：
// 已进入某 harness 时用它；仍在 harness 首屏时用当前高亮项。
func (m *Model) scopeHarness() string {
	if m.harness != "" {
		return m.harness
	}
	if m.level == LevelHarness && m.hIdx >= 0 && m.hIdx < len(m.harnesses) {
		return m.harnesses[m.hIdx].Harness
	}
	return ""
}

// applySearch 执行子串搜索（content/title LIKE）。
func (m *Model) applySearch() {
	m.searchHits = nil
	m.searchErr = ""
	q := strings.TrimSpace(m.searchInput)
	if q == "" {
		return
	}
	harness := ""
	if !m.searchAll {
		harness = m.scopeHarness()
	}
	hits, err := m.store.SearchEvents(harness, q, 0)
	if err != nil {
		m.searchErr = err.Error()
		return
	}
	m.searchHits = hits
	if m.searchIdx >= len(hits) {
		m.searchIdx = 0
	}
	m.ensureVisibleH(&m.searchOff, m.searchIdx, len(hits), m.searchPageSize())
}

// searchScopeLabel 返回搜索范围的展示文本。
func (m *Model) searchScopeLabel() string {
	if m.searchAll {
		return "全局"
	}
	if h := m.scopeHarness(); h != "" {
		return "当前 harness: " + h
	}
	return "当前(无 harness)"
}

// openSearchHit 跳转到命中的会话并定位到该事件。
func (m *Model) openSearchHit() {
	if m.searchIdx < 0 || m.searchIdx >= len(m.searchHits) {
		return
	}
	h := m.searchHits[m.searchIdx]

	// 记住当前位置，便于 esc 返回搜索列表。
	m.prevLevel = m.level
	m.prevSession = m.session
	m.prevEvents = m.events
	m.prevEvIdx = m.evIdx
	m.prevEvOff = m.evOff

	m.harness = h.Harness
	m.session = model.Session{SessionID: h.SessionID}
	m.loadEvents()
	m.level = LevelTimeline
	// 定位到命中事件（按 session_id + seq 匹配）。
	m.evIdx = 0
	for i := range m.events {
		if m.events[i].Seq == h.Seq {
			m.evIdx = i
			break
		}
	}
	m.ensureVisible(&m.evOff, m.evIdx, len(m.events))
	m.searching = false
	m.status = fmt.Sprintf("搜索命中 → %s/%s 第 %d 步", h.Harness, truncRunes(h.SessionID, 16), h.Seq)
}

// backFromSearchHit 从命中的会话层返回搜索列表。
func (m *Model) backFromSearchHit() {
	m.searching = true
	m.level = m.prevLevel
	m.session = m.prevSession
	m.events = m.prevEvents
	m.evIdx = m.prevEvIdx
	m.evOff = m.prevEvOff
}

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
	m.ensureVisibleH(off, idx, n, m.pageSize())
}

// ensureVisibleH 把 idx 滚入以 off 为起点、高度 h 的窗口。
func (m *Model) ensureVisibleH(off *int, idx, n, h int) {
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

// searchPageSize 是搜索命中列表的可显示行数（扣除输入行与列头）。
func (m *Model) searchPageSize() int {
	h := m.listHeight() - 2
	if h < 1 {
		h = 1
	}
	return h
}

// ---------- 按键 ----------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case liveTickMsg:
		if !m.live {
			return m, nil
		}
		m.refreshLive()
		return m, liveTickCmd()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		return m.handleFilterKey(msg)
	}
	if m.searching {
		return m.handleSearchKey(msg)
	}
	key := msg.String()
	if m.showHelp {
		if key == "?" || key == "esc" {
			m.showHelp = false
		}
		return m, nil
	}
	// 回放模式：space/n/→/p/←/esc 独占按键。
	if m.replay && m.level == LevelTimeline {
		switch key {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", " ":
			m.exitReplay()
			return m, nil
		case "n", "right":
			m.replayMove(1)
			return m, nil
		case "p", "left":
			m.replayMove(-1)
			return m, nil
		case "up", "k":
			m.scrollReplay(-1)
			return m, nil
		case "down", "j":
			m.scrollReplay(1)
			return m, nil
		case "q":
			return m, tea.Quit
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
	case "/":
		m.enterSearch(true)
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
		case "f":
			m.enterSearch(false) // harness 层按 f：限定当前 harness 搜索
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
		case "F":
			// 会话层前缀过滤（M1-8 起 '/' 让位给全局搜索，过滤改用 F）。
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
		case " ":
			m.enterReplay() // 注：tea.KeySpace 的 String() 是 " "
		case "l":
			return m, m.toggleLive()
		case "esc":
			if len(m.searchHits) > 0 {
				m.backFromSearchHit() // 从搜索命中返回搜索列表
			} else {
				m.level = LevelSession
			}
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

// scrollReplay 在回放模式下滚动当前步的全文（不改步号）。
func (m *Model) scrollReplay(d int) {
	off := m.replayOff + d
	if off < 0 {
		off = 0
	}
	if max := m.replayMax(); off > max {
		off = max
	}
	m.replayOff = off
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

// handleSearchKey 处理搜索态的按键：输入即搜、↑↓ 选命中、enter 跳转、esc 取消。
func (m *Model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.searching = false
		m.searchHits = nil
		m.searchErr = ""
		m.status = "已取消搜索"
	case tea.KeyEnter:
		if len(m.searchHits) == 0 {
			return m, nil
		}
		m.openSearchHit()
	case tea.KeyUp:
		m.moveSearchHit(-1)
	case tea.KeyDown:
		m.moveSearchHit(1)
	case tea.KeyTab:
		m.searchAll = !m.searchAll
		m.applySearch()
	case tea.KeyBackspace:
		r := []rune(m.searchInput)
		if len(r) > 0 {
			m.searchInput = string(r[:len(r)-1])
		}
		m.applySearch()
	case tea.KeyRunes:
		m.searchInput += string(msg.Runes)
		m.applySearch()
	case tea.KeySpace:
		m.searchInput += " "
		m.applySearch()
	}
	return m, nil
}

func (m *Model) moveSearchHit(d int) {
	if len(m.searchHits) == 0 {
		return
	}
	m.searchIdx += d
	if m.searchIdx < 0 {
		m.searchIdx = 0
	}
	if m.searchIdx >= len(m.searchHits) {
		m.searchIdx = len(m.searchHits) - 1
	}
	m.ensureVisibleH(&m.searchOff, m.searchIdx, len(m.searchHits), m.searchPageSize())
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
	return eventLines(m.currentEvent(), m.width)
}

// eventLines 把一条事件渲染为可滚动行（信息头 + content 全文折行）。
func eventLines(e *model.Event, width int) []string {
	if e == nil {
		return nil
	}
	w := width - 4
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

// replayMax 是回放当前步全文的最大滚动偏移。
func (m *Model) replayMax() int {
	n := len(eventLines(m.replayEvent(), m.width)) - m.pageSize()
	if n < 0 {
		n = 0
	}
	return n
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
	if m.searching {
		return m.searchView()
	}
	switch m.level {
	case LevelSession:
		return m.sessionView()
	case LevelTimeline:
		if m.replay {
			return m.replayView()
		}
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
	if live := m.liveStatus(); live != "" {
		b.WriteString(dimStyle.Render("  ·  ") + liveStyle.Render(live))
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
	return m.frame(crumb, rows, "↑↓ 滚动 · enter 展开全文 · space 回放 · l live · esc 返回 · r 刷新 · ? 帮助 · q 退出")
}

// replayView 渲染回放模式：当前步自动展开全文（viewport 滚动），并高亮当前步。
func (m *Model) replayView() string {
	e := m.replayEvent()
	if e == nil {
		return m.frame("回放", []string{dimStyle.Render("  （无事件）")}, "esc 退出回放")
	}
	lines := eventLines(e, m.width)
	end := m.replayOff + m.pageSize()
	if end > len(lines) {
		end = len(lines)
	}
	rows := lines[m.replayOff:end]
	// 在内容之后附一步骤上下文（前 2 步 / 后 2 步），便于回放时定位。
	if ctx := m.replayContext(e); len(ctx) > 0 {
		rows = append(rows, dimStyle.Render("── 上下文 ──"))
		rows = append(rows, ctx...)
	}
	crumb := fmt.Sprintf("回放  ·  %s  ·  %s  ·  第 %d/%d 步  ·  %s",
		m.harness, truncRunes(m.session.SessionID, 18), m.replayIdx+1, len(m.events), e.Kind)
	hint := fmt.Sprintf("n/→ 下一步 · p/← 上一步 · ↑↓ 滚动全文 · esc 退出回放   [行 %d-%d/%d]",
		m.replayOff+1, end, len(lines))
	return m.frame(crumb, rows, hint)
}

// replayContext 返回当前步前后各 2 步的单行摘要（用于回放上下文）。
func (m *Model) replayContext(e *model.Event) []string {
	var out []string
	for i := m.replayIdx - 2; i <= m.replayIdx+2; i++ {
		if i < 0 || i >= len(m.events) || i == m.replayIdx {
			continue
		}
		ev := m.events[i]
		line := fmt.Sprintf("%s  %s  %s",
			ev.Time.Local().Format("15:04:05"),
			kindStyle(ev.Kind).Render(fmt.Sprintf("%-13s", ev.Kind)),
			truncRunes(oneLine(ev.Content), summaryWidth))
		marker := "  "
		if i < m.replayIdx {
			marker = "↑ "
		} else {
			marker = "↓ "
		}
		out = append(out, dimStyle.Render(marker+line))
	}
	return out
}

// searchView 渲染搜索态：查询输入行 + 命中列表（harness/会话/时间/命中摘要）。
func (m *Model) searchView() string {
	rows := []string{}
	scope := m.searchScopeLabel()
	rows = append(rows, cursorStyle.Render("/")+m.searchInput+"█"+
		dimStyle.Render(fmt.Sprintf("  [%s] · enter 跳转 · ↑↓ 选择 · tab 切换范围 · esc 取消", scope)))
	if m.searchErr != "" {
		rows = append(rows, errStyle.Render("  搜索错误: "+m.searchErr))
	} else if len(m.searchHits) == 0 {
		if strings.TrimSpace(m.searchInput) == "" {
			rows = append(rows, dimStyle.Render("  输入关键词搜索 events.content / title（子串匹配）"))
		} else {
			rows = append(rows, dimStyle.Render("  无命中"))
		}
	} else {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("  %-9s %-30s %-19s %s",
			"HARNESS", "会话", "时间", "命中摘要")))
		end := m.searchOff + m.searchPageSize()
		if end > len(m.searchHits) {
			end = len(m.searchHits)
		}
		for i := m.searchOff; i < end; i++ {
			h := m.searchHits[i]
			line := fmt.Sprintf("%-9s %-30s %-19s %s",
				h.Harness, truncRunes(h.SessionID, 30),
				h.Time.Local().Format("2006-01-02 15:04:05"),
				highlight(truncRunes(oneLine(h.Content), summaryWidth), m.searchInput))
			if i == m.searchIdx {
				rows = append(rows, cursorStyle.Render("▶ ")+line)
			} else {
				rows = append(rows, "  "+line)
			}
		}
	}
	crumb := fmt.Sprintf("搜索：%q  ·  %d 命中（%s）", m.searchInput, len(m.searchHits), scope)
	return m.frame(crumb, rows, "enter 跳转定位 · ↑↓ 选择 · tab 切换全局/当前 harness · esc 取消")
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
		"  f         搜索（限定当前 harness）",
		"",
		selectedStyle.Render("会话层"),
		"  ↑/k ↓/j   选择会话",
		"  enter     进入会话时间线",
		"  F         前缀过滤（session_id / 标题）",
		"  esc       返回 harness 层",
		"",
		selectedStyle.Render("时间线层"),
		"  ↑/k ↓/j   滚动步骤",
		"  enter     展开该步 content 全文（viewport）",
		"  space     进入回放：n/→ 下一步 · p/← 上一步 · esc 退出",
		"  l         切换 live 跟随（每 2s 增量采集，自动滚到底部）",
		"  esc       返回会话层",
		"",
		selectedStyle.Render("详情层"),
		"  ↑/k ↓/j   滚动 · PgUp/PgDn 翻页 · g/G 顶/底",
		"  enter/esc 收起全文",
		"",
		selectedStyle.Render("搜索（任意层）"),
		"  /         搜索 events 的 content / title（子串，大小写不敏感）",
		"  输入即搜 · ↑↓ 选择 · tab 切换 全局/当前 harness · enter 跳转定位 · esc 取消",
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

// highlight 在文本中把 query 子串（大小写不敏感）着色，便于定位命中位置。
// 仅做 ASCII 大小写折叠匹配（与 SQLite LIKE 口径一致），无匹配则原样返回。
func highlight(s, query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return s
	}
	lowerS := strings.ToLower(s)
	lowerQ := strings.ToLower(query)
	var b strings.Builder
	for {
		i := strings.Index(lowerS, lowerQ)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(hitStyle.Render(s[i : i+len(query)]))
		s = s[i+len(query):]
		lowerS = lowerS[i+len(query):]
	}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
