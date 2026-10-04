// Package tui 提供 bubbletea 最小外壳。
//
// M1-1 只做首屏：显示 "harness-monitor" 标题 + 当前 store 的 harness 汇总，q 退出。
// 完整三级导航（harness → 会话 → 时间线）是 M1-7。
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/qf/harness-monitor/internal/store"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205")).
			MarginBottom(1)
	dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

// model 是 bubbletea 的最小模型。
type model struct {
	store   *store.Store
	summary []store.HarnessSummary
	err     error
}

// Run 启动 TUI，阻塞直到用户退出。
func Run(s *store.Store) error {
	m := model{store: s}
	if m.store != nil {
		if sum, err := m.store.Harnesses(); err != nil {
			m.err = err
		} else {
			m.summary = sum
		}
	}
	_, err := tea.NewProgram(m).Run()
	return err
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("harness-monitor"))
	b.WriteString("\n")

	if m.store != nil {
		b.WriteString(dimStyle.Render("db: "+m.store.Path) + "\n\n")
	}
	if m.err != nil {
		b.WriteString("读取汇总失败: " + m.err.Error() + "\n\n")
	}
	if len(m.summary) == 0 {
		b.WriteString("（空库）运行 `hm collect --harness <name>` 采集数据。\n\n")
	} else {
		b.WriteString(fmt.Sprintf("%-12s %8s %8s\n", "HARNESS", "会话数", "事件数"))
		for _, h := range m.summary {
			b.WriteString(fmt.Sprintf("%-12s %8d %8d\n", h.Harness, h.SessionCount, h.EventCount))
		}
		b.WriteString("\n")
	}
	b.WriteString(dimStyle.Render("q 退出 · 三级导航（M1-7）待实现"))
	return b.String()
}
