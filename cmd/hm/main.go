// Command hm 是 harness-monitor 的 CLI 入口。
//
// 子命令：collect / ls / show / export / search / version / tui。
//
// 注意：本文件刻意不使用 cobra 等 CLI 框架——实测（2026-10-04）带 cobra.Execute()
// 的二进制在 explorer 双击启动环境下会被火绒（Huorong）静默终止（进程 2-3 秒后消失、
// 无错误无崩溃记录；同一二进制从终端启动正常）。手写解析器在双击场景下稳定。
// 详见 docs/DESIGN.md「双击启动与安全软件」。
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/collect"
	"github.com/qifeng2692/harness-monitor/internal/export"
	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
	"github.com/qifeng2692/harness-monitor/internal/tui"
)

// version 由构建期注入：go build -ldflags "-X main.version=..."。
var version = "dev"

// bootLogFile 是启动日志：记录每次启动的关键阶段（写 exe 同目录的 hm-boot.log）。
// 双击场景出错时控制台窗口可能一闪而过——这份日志让事后排查有据可查。
var bootLogFile *os.File

func bootLog(format string, args ...any) {
	if bootLogFile == nil {
		return
	}
	fmt.Fprintf(bootLogFile, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05.000"), fmt.Sprintf(format, args...))
	_ = bootLogFile.Sync()
}

func initBootLog() {
	write := func(path string) bool {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return false
		}
		if st, err := f.Stat(); err == nil && st.Size() > 2<<20 { // 超 2MB 截断重来
			_ = f.Truncate(0)
			_, _ = f.Seek(0, 0)
		}
		bootLogFile = f
		return true
	}
	if exe, err := os.Executable(); err == nil {
		if write(filepath.Join(filepath.Dir(exe), "hm-boot.log")) {
			return
		}
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		dir := filepath.Join(la, "harness-monitor")
		if os.MkdirAll(dir, 0o755) == nil {
			write(filepath.Join(dir, "hm-boot.log"))
		}
	}
}

// pauseOnError 在“双击启动”（无参数、stdin 是控制台）且出错时暂停，让窗口留在屏幕上显示错误，
// 而不是一闪而过。管道 / CI 调用不暂停。
func pauseOnError(err error) error {
	if len(os.Args) != 1 {
		return err
	}
	if fi, statErr := os.Stdin.Stat(); statErr != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return err
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "出错:", err)
	fmt.Fprintln(os.Stderr, "按 Enter 键退出…")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	return err
}

func main() {
	initBootLog()
	if bootLogFile != nil {
		defer bootLogFile.Close()
	}
	// panic 捕获：把崩溃堆栈写进启动日志（双击场景控制台会秒关，这是唯一能看到现场的地方）。
	defer func() {
		if r := recover(); r != nil {
			bootLog("PANIC: %v\n%s", r, debug.Stack())
			if bootLogFile != nil {
				bootLogFile.Close()
			}
			os.Exit(3)
		}
	}()
	wd, _ := os.Getwd()
	exe, _ := os.Executable()
	bootLog("boot: exe=%s cwd=%s args=%q", exe, wd, os.Args[1:])

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hm:", err)
		bootLog("exit: error=%v", err)
		pauseOnError(err)
		os.Exit(1)
	}
	bootLog("exit: ok")
}

// run 是手写的子命令分发（替代 cobra，见文件头注释）。
func run(args []string) error {
	if len(args) == 0 {
		return runRoot()
	}
	switch args[0] {
	case "collect":
		return runCollect(args[1:])
	case "ls":
		return runLs(args[1:])
	case "show":
		return runShow(args[1:])
	case "search":
		return runSearch(args[1:])
	case "export":
		return runExport(args[1:])
	case "tui":
		return runTui()
	case "version":
		fmt.Printf("hm %s\n", version)
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", args[0])
		printUsage()
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

func printUsage() {
	fmt.Print(`hm — harness-monitor：采集并查看各 AI harness（Hermes / Codex / Claude Code / pi / magpie）的步骤级事件

用法:
  hm                         双击场景：增量采集全部 harness → 打开 TUI
  hm collect [flags]         采集事件到本地库（幂等、增量）
      --harness <name>       只采集指定 harness（默认全部）
      --incremental          增量续读（只读上次 offset 之后的新增行）
  hm ls [flags]              列出会话
      --harness <name>       只列出指定 harness 的会话
  hm show <harness> <sid>    打印某会话的时间线
      --full                 打印内容全文（默认截断到 120 字符）
  hm search <关键词>         全文检索会话内容（不开 GUI，直接搜本地库）
      --harness <name>       只搜指定 harness
      --limit N              命中上限（默认 500）
      --context N            命中处上下文行数（默认 2）
  hm export <harness> <sid>  把某会话导出为 Markdown 文件（含每步内容与说明书）
      -o <path>              输出文件路径（默认 exports/<harness>-<sid前8位>.md）
      --no-redact            关闭脱敏（默认把个人路径/邮箱/密钥替换为 [已脱敏:…]）
  hm tui                     启动 TUI（三级导航）
  hm version                 打印版本
`)
}

// runRoot：无参数时——增量采集全部 harness → 进入 TUI（双击 exe 即用）。
func runRoot() error {
	bootLog("stage: store.Open")
	s, err := store.Open()
	if err != nil {
		bootLog("stage: store.Open FAILED: %v", err)
		return err
	}
	defer s.Close()
	bootLog("stage: store.Open ok (db=%s)", s.Path)

	fmt.Println("[hm] 增量采集中...")
	total := 0
	parts := []string{}
	for _, name := range collect.Names() {
		n, err := collect.CollectIncremental(name, s)
		if err != nil {
			bootLog("stage: collect %s FAILED: %v", name, err)
			parts = append(parts, fmt.Sprintf("%s 失败(%v)", name, err))
			continue
		}
		total += n
		parts = append(parts, fmt.Sprintf("%s +%d", name, n))
	}
	fmt.Printf("[hm] 采集: %s = %d 条新事件\n", strings.Join(parts, " · "), total)
	bootLog("stage: collect done (%d new)", total)

	bootLog("stage: tui.Run")
	err = tui.Run(s)
	bootLog("stage: tui.Run returned: %v", err)
	return err
}

// ---- 子命令实现 -----------------------------------------------------------

func runCollect(args []string) error {
	harness, incremental := "", false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--harness" && i+1 < len(args):
			harness = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--harness="):
			harness = strings.TrimPrefix(args[i], "--harness=")
		case args[i] == "--incremental":
			incremental = true
		case args[i] == "-h" || args[i] == "--help":
			printUsage()
			return nil
		}
	}

	s, err := store.Open()
	if err != nil {
		return err
	}
	defer s.Close()

	names := collect.Names()
	if harness != "" {
		if collect.Get(harness) == nil {
			fmt.Printf("not implemented: harness %q 采集器尚未实现（已注册：%s）\n", harness, orNone(names))
			return nil
		}
		names = []string{harness}
	}
	if len(names) == 0 {
		fmt.Println("没有已注册的采集器。")
		return nil
	}
	for _, name := range names {
		if incremental {
			n, err := collect.CollectIncremental(name, s)
			if err != nil {
				fmt.Printf("%s: 增量采集失败: %v\n", name, err)
				continue
			}
			fmt.Printf("harness=%s incremental inserted=%d\n", name, n)
			continue
		}
		c := collect.Get(name)
		stats, err := c.Collect(s)
		if err != nil {
			fmt.Printf("%s: 采集失败: %v\n", name, err)
			continue
		}
		fmt.Printf("%s\n", stats)
	}
	return nil
}

func runLs(args []string) error {
	harness := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--harness" && i+1 < len(args):
			harness = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--harness="):
			harness = strings.TrimPrefix(args[i], "--harness=")
		case args[i] == "-h" || args[i] == "--help":
			printUsage()
			return nil
		}
	}

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
			fmt.Println("无会话（空库）")
			return nil
		}
	}
	for _, h := range harnesses {
		sessions, err := s.ListSessions(h)
		if err != nil {
			return err
		}
		fmt.Printf("== %s ==\n", h)
		if len(sessions) == 0 {
			fmt.Println("无会话")
			continue
		}
		fmt.Printf("%-40s %8s  %-25s %-25s %s\n",
			"SESSION_ID", "事件数", "首次", "末次", "标题")
		for _, sess := range sessions {
			fmt.Printf("%-40s %8d  %-25s %-25s %s\n",
				truncate(sess.SessionID, 40), sess.EventCount,
				fmtTime(sess.FirstTime), fmtTime(sess.LastTime),
				truncate(sess.Title, 40))
		}
	}
	return nil
}

func runShow(args []string) error {
	full := false
	positional := []string{}
	for _, a := range args {
		switch a {
		case "--full":
			full = true
		case "-h", "--help":
			printUsage()
			return nil
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 2 {
		printUsage()
		return fmt.Errorf("show 需要两个参数：<harness> <session-id>（给了 %d 个）", len(positional))
	}
	harness, sessionID := positional[0], positional[1]

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
		fmt.Printf("无事件：harness=%s session=%s\n", harness, sessionID)
		return nil
	}
	for _, e := range events {
		content := e.Content
		if !full {
			content = oneLine(truncate(content, 120))
		}
		fmt.Printf("%s  %-14s  %s\n", fmtTime(e.Time), e.Kind, e.Title)
		if content != "" {
			if full {
				fmt.Printf("%s\n", content)
			} else {
				fmt.Printf("    %s\n", content)
			}
		}
	}
	return nil
}

// defaultSearchContext 是 search 命中窗口的默认大小（命中点前后各显示的字符数）。
const defaultSearchContext = 40

// runSearch 实现子命令 search：不开 GUI 直接检索本地库的会话内容。
// 查询复用 store.SearchEvents（与 GUI/TUI 搜索同一条路径），输出按 harness → 会话
// 分组，每条命中给出命中点上下文窗口——完整内容用 hm show <harness> <sid> 深入。
func runSearch(args []string) error {
	harness := ""
	limit, ctx := 0, defaultSearchContext
	positional := []string{}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--harness" && i+1 < len(args):
			harness = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--harness="):
			harness = strings.TrimPrefix(args[i], "--harness=")
		case args[i] == "--limit" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				return fmt.Errorf("--limit 需要正整数（给了 %q）", args[i+1])
			}
			limit = n
			i++
		case strings.HasPrefix(args[i], "--limit="):
			v := strings.TrimPrefix(args[i], "--limit=")
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("--limit 需要正整数（给了 %q）", v)
			}
			limit = n
		case args[i] == "--context" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return fmt.Errorf("--context 需要非负整数（给了 %q）", args[i+1])
			}
			ctx = n
			i++
		case strings.HasPrefix(args[i], "--context="):
			v := strings.TrimPrefix(args[i], "--context=")
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return fmt.Errorf("--context 需要非负整数（给了 %q）", v)
			}
			ctx = n
		case args[i] == "-h" || args[i] == "--help":
			printUsage()
			return nil
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 1 {
		printUsage()
		return fmt.Errorf("search 需要一个参数：<关键词>（给了 %d 个）", len(positional))
	}
	query := positional[0]

	s, err := store.Open()
	if err != nil {
		return err
	}
	defer s.Close()

	hits, err := s.SearchEvents(harness, query, limit)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		scope := "全部 harness"
		if harness != "" {
			scope = harness
		}
		fmt.Printf("无命中：%q（范围：%s）\n", query, scope)
		return nil
	}

	fmt.Printf("搜索 %q：%d 条命中\n\n", query, len(hits))
	cur := "" // 当前分组的 harness/session，命中已按其排序，换组时打表头
	for _, h := range hits {
		key := h.Harness + "\x00" + h.SessionID
		if key != cur {
			cur = key
			fmt.Printf("== %s / %s ==\n", h.Harness, h.SessionID)
		}
		fmt.Printf("%s  %-14s  %s\n", fmtTime(h.Time), h.Kind, oneLine(truncate(h.Title, 80)))
		if ctx <= 0 {
			fmt.Printf("    %s\n\n", oneLine(truncate(h.Content, 160)))
			continue
		}
		fmt.Printf("    %s\n\n", matchWindow(h.Content, query, ctx))
	}
	if limit > 0 && len(hits) == limit {
		fmt.Printf("（已达 --limit %d，可能还有更多命中——调大 limit 或用 --harness 缩小范围）\n", limit)
	}
	return nil
}

// matchWindow 从内容里找第一处命中行，返回以命中点为中心的窗口（前后各 around 字符）。
// 单条事件内容可达数 MB（工具结果全文），直接打印会灌爆终端——窗口让上下文一眼可见。
// 命中可能落在 title 或大小写形态不同（SQL LIKE 的匹配面比这里宽），找不到时退回内容开头截断。
func matchWindow(content, query string, around int) string {
	if around <= 0 {
		return oneLine(truncate(content, 160))
	}
	nq := strings.ToLower(query)
	for _, line := range strings.Split(content, "\n") {
		pos := strings.Index(strings.ToLower(line), nq)
		if pos < 0 {
			continue
		}
		r := []rune(line)
		ri := len([]rune(line[:pos])) // 命中点的 rune 下标
		rq := len([]rune(query))
		start, end := ri-around, ri+rq+around
		if start < 0 {
			start = 0
		}
		if end > len(r) {
			end = len(r)
		}
		var b strings.Builder
		if start > 0 {
			b.WriteString("…")
		}
		b.WriteString(strings.TrimSpace(string(r[start:end])))
		if end < len(r) {
			b.WriteString("…")
		}
		return b.String()
	}
	return oneLine(truncate(content, 2*around+60))
}

func runExport(args []string) error {
	outPath := ""
	noRedact := false
	positional := []string{}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-o" && i+1 < len(args):
			outPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-o="):
			outPath = strings.TrimPrefix(args[i], "-o=")
		case args[i] == "--no-redact":
			noRedact = true
		case args[i] == "-h" || args[i] == "--help":
			printUsage()
			return nil
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 2 {
		printUsage()
		return fmt.Errorf("export 需要两个参数：<harness> <session-id>（给了 %d 个）", len(positional))
	}
	harness, sessionID := positional[0], positional[1]

	s, err := store.Open()
	if err != nil {
		return err
	}
	defer s.Close()

	sessions, err := s.ListSessions(harness)
	if err != nil {
		return err
	}
	var sess *model.Session
	for i := range sessions {
		if sessions[i].SessionID == sessionID {
			sess = &sessions[i]
			break
		}
	}
	if sess == nil {
		return fmt.Errorf("找不到会话：harness=%s session=%s（先 hm ls 确认）", harness, sessionID)
	}
	events, err := s.ListEvents(harness, sessionID)
	if err != nil {
		return err
	}

	doc := export.SessionOpts(harness, sess.Title, sess.FirstTime, sess.LastTime, events, export.Opts{Redact: !noRedact})

	if outPath == "" {
		id := sessionID
		if len(id) > 8 {
			id = id[:8]
		}
		outPath = filepath.Join("exports", sanitize(sanitize(harness)+"-"+id)+".md")
	}
	if dir := filepath.Dir(outPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(outPath, []byte(doc), 0o644); err != nil {
		return err
	}
	fmt.Printf("已导出 %d 步 → %s\n", len(events), outPath)
	return nil
}

// sanitize 把字符串里不适合做文件名的字符换成 -。
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '-'
		}
		return r
	}, s)
}

func runTui() error {
	s, err := store.Open()
	if err != nil {
		return err
	}
	defer s.Close()
	return tui.Run(s)
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
