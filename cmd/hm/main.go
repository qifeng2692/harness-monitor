// Command hm 是 harness-monitor 的 CLI 入口。
//
// 子命令：collect / ls / show / version / tui。
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qf/harness-monitor/internal/collect"
	"github.com/qf/harness-monitor/internal/store"
	"github.com/qf/harness-monitor/internal/tui"
)

// version 由构建期注入：go build -ldflags "-X main.version=..."。
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "hm:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "hm",
		Short:         "harness-monitor —— 按 harness / 会话采集并查看各 AI 编码助手的步骤级内容",
		Long:          "harness-monitor 采集本机各 AI harness（Hermes / Codex / Claude Code / pi / magpie）的步骤级事件，\n存入 SQLite 事件库，并以 TUI 按 harness → 会话 → 时间线三级导航查看。",
		SilenceUsage:  true,
		SilenceErrors: true,
		// 无子命令时直接进入 TUI（`go run ./cmd/hm` 即首屏）。
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			defer s.Close()
			return tui.Run(s)
		},
	}
	root.AddCommand(newCollectCmd())
	root.AddCommand(newLsCmd())
	root.AddCommand(newShowCmd())
	root.AddCommand(newTuiCmd())
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "打印版本",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "hm %s\n", version)
			return nil
		},
	}
}

func newCollectCmd() *cobra.Command {
	var harness string
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "采集各 harness 的事件到本地库（幂等、增量）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			s, err := store.Open()
			if err != nil {
				return err
			}
			defer s.Close()

			names := collect.Names()
			if harness != "" {
				if collect.Get(harness) == nil {
					// M1-1 骨架：真实采集器尚未实现。
					fmt.Fprintf(out, "not implemented: harness %q 采集器尚未实现（已注册：%s）\n",
						harness, orNone(names))
					return nil
				}
				names = []string{harness}
			}
			if len(names) == 0 {
				fmt.Fprintln(out, "没有已注册的采集器（M1-2..M1-6 逐个实现）。")
				return nil
			}
			for _, name := range names {
				c := collect.Get(name)
				stats, err := c.Collect(s)
				if err != nil {
					fmt.Fprintf(out, "%s: 采集失败: %v\n", name, err)
					continue
				}
				fmt.Fprintf(out, "%s\n", stats)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "只采集指定 harness（默认全部）")
	return cmd
}

func newLsCmd() *cobra.Command {
	var harness string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "列出会话（按 harness）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			s, err := store.Open()
			if err != nil {
				return err
			}
			defer s.Close()

			harnesses := []string{harness}
			if harness == "" {
				sum, err := s.Harnesses()
				if err != nil {
					return err
				}
				harnesses = harnesses[:0]
				for _, h := range sum {
					harnesses = append(harnesses, h.Harness)
				}
				if len(harnesses) == 0 {
					fmt.Fprintln(out, "无会话（空库）")
					return nil
				}
			}
			for _, h := range harnesses {
				sessions, err := s.ListSessions(h)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "== %s ==\n", h)
				if len(sessions) == 0 {
					fmt.Fprintln(out, "无会话")
					continue
				}
				fmt.Fprintf(out, "%-40s %8s  %-25s %-25s %s\n",
					"SESSION_ID", "事件数", "首次", "末次", "标题")
				for _, sess := range sessions {
					fmt.Fprintf(out, "%-40s %8d  %-25s %-25s %s\n",
						truncate(sess.SessionID, 40), sess.EventCount,
						fmtTime(sess.FirstTime), fmtTime(sess.LastTime),
						truncate(sess.Title, 40))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "只列出指定 harness 的会话（默认全部）")
	return cmd
}

func newShowCmd() *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "show <harness> <session-id>",
		Short: "打印某会话的时间线",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			harness, sessionID := args[0], args[1]
			s, err := store.Open()
			if err != nil {
				return err
			}
			defer s.Close()

			events, err := s.ListEvents(harness, sessionID)
			if err != nil {
				return err
			}
			if len(events) == 0 {
				fmt.Fprintf(out, "无事件：harness=%s session=%s\n", harness, sessionID)
				return nil
			}
			for _, e := range events {
				content := e.Content
				if !full {
					content = oneLine(truncate(content, 120))
				}
				fmt.Fprintf(out, "%s  %-14s  %s\n",
					fmtTime(e.Time), e.Kind, e.Title)
				if content != "" {
					if full {
						fmt.Fprintf(out, "%s\n", content)
					} else {
						fmt.Fprintf(out, "    %s\n", content)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "打印内容全文（默认截断到 120 字符）")
	return cmd
}

func newTuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "启动 TUI（bubbletea）",
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			defer s.Close()
			return tui.Run(s)
		},
	}
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "无"
	}
	return strings.Join(names, ", ")
}

// truncate 按 rune 截断，超出时补省略号。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// oneLine 把多行内容压成一行，便于列表展示。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// fmtTime 把时间格式化为本地时区的紧凑展示形式。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
