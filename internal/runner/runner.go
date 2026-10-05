// Package runner 实现「对比实验台」的四家 harness 非交互运行器与输出解析器。
//
// 规格见 docs/DESIGN-runner.md 第 2/4/5 节：
//
//   - 每家 harness 一条固定命令行，工作目录隔离在 <runDir>/<harness>/（自动创建）；
//   - codex / pi 的 prompt 走 stdin（它们是 npm 的 .cmd 包装器，Go 对 .cmd 的多行/
//     特殊字符参数会直接报错），hermes / claude 走命令行参数（原生 exe，argv 安全）；
//   - 输出为 JSONL：逐行喂给对应解析器（纯函数），产出统一 model.Event；
//   - 单家超时（默认 10 分钟）杀进程树；退出码 / 超时 / 失败 → run_harness 状态。
//
// 解析器不吞异常：非 JSON 行（codex 的 "Reading additional input from stdin..."、
// pi 的 Warning、claude 的 [claude-code:...] 前缀行）一律忽略，不产出事件。
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qf/harness-monitor/internal/model"
	"github.com/qf/harness-monitor/internal/store"
)

// 四家 harness 的规范名（与主监控库一致）。
const (
	HarnessHermes = "hermes"
	HarnessCodex  = "codex"
	HarnessClaude = "claude"
	HarnessPi     = "pi"
)

// All 返回四家 harness 的规范顺序（UI 列顺序、默认勾选顺序）。
var All = []string{HarnessHermes, HarnessCodex, HarnessClaude, HarnessPi}

// DefaultTimeout 是单家 harness 的默认运行超时。
const DefaultTimeout = 10 * time.Minute

// stderrCaptureLimit 是失败时留存的 stderr 摘要上限（防止子进程刷屏撑爆内存）。
const stderrCaptureLimit = 8 << 10

// validHarness 报告 name 是否为四家之一。
func validHarness(name string) bool {
	for _, h := range All {
		if h == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 启动规格
// ---------------------------------------------------------------------------

// Spec 描述一家 harness 的一次非交互启动方式（可执行文件 + 参数 + 工作目录 + stdin）。
type Spec struct {
	Harness  string
	Path     string   // 可执行文件（exec.LookPath 解析结果或环境变量覆盖）
	Args     []string // 参数；prompt 走 stdin 时不含 prompt
	Dir      string   // 工作目录（隔离目录）
	UseStdin bool     // true 时把 Prompt 写入 stdin
	Prompt   string
}

// Command 按 Spec 构造 *exec.Cmd（不绑定 context；超时由调用方 kill 进程树）。
func (s Spec) Command() *exec.Cmd {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Dir = s.Dir
	if s.UseStdin {
		cmd.Stdin = strings.NewReader(s.Prompt)
	}
	return cmd
}

// binOverrideEnv 返回某家 harness 的可执行文件覆盖环境变量名（如 HM_HARNESS_BIN_CODEX）。
// 用于 GUI 从 explorer 启动、PATH 不含 npm 全局目录的场景。
func binOverrideEnv(harness string) string {
	return "HM_HARNESS_BIN_" + strings.ToUpper(harness)
}

// ResolveBin 解析某家 harness 的可执行文件：环境变量覆盖优先，否则走 PATH。
func ResolveBin(harness string) (string, error) {
	name := harness
	switch harness {
	case HarnessHermes:
		name = "hermes"
	case HarnessCodex:
		name = "codex"
	case HarnessClaude:
		name = "claude"
	case HarnessPi:
		name = "pi"
	default:
		return "", fmt.Errorf("未知 harness: %q", harness)
	}
	if ov := os.Getenv(binOverrideEnv(harness)); ov != "" {
		return ov, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("找不到 %s 可执行文件（PATH 中无 %s，可用 %s 指定）: %w",
			harness, name, binOverrideEnv(harness), err)
	}
	return p, nil
}

// BuildSpec 构造某家 harness 在 dir 下跑 prompt 的启动规格。
//
// 命令行（docs/DESIGN-runner.md 第 2 节实测表）：
//
//	hermes  hermes chat -q <prompt> --oneshot --format stream-json --in <dir>
//	codex   codex exec --skip-git-repo-check --json -C <dir>        + prompt 走 stdin
//	claude  claude -p --output-format stream-json --verbose --permission-mode bypassPermissions <prompt>
//	pi      pi -p --mode json                                       + prompt 走 stdin
func BuildSpec(harness, dir, prompt string) (Spec, error) {
	bin, err := ResolveBin(harness)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{Harness: harness, Path: bin, Dir: dir}
	switch harness {
	case HarnessHermes:
		s.Args = []string{"chat", "-q", prompt, "--oneshot", "--format", "stream-json", "--in", dir}
	case HarnessCodex:
		s.Args = []string{"exec", "--skip-git-repo-check", "--json", "-C", dir}
		s.UseStdin, s.Prompt = true, prompt
	case HarnessClaude:
		s.Args = []string{"-p", "--output-format", "stream-json", "--verbose",
			"--permission-mode", "bypassPermissions", prompt}
	case HarnessPi:
		s.Args = []string{"-p", "--mode", "json"}
		s.UseStdin, s.Prompt = true, prompt
	default:
		return Spec{}, fmt.Errorf("未知 harness: %q", harness)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// 解析状态与解析器
// ---------------------------------------------------------------------------

// ParseState 是一次运行中跨行累积的解析状态（每行解析的唯一可变输入）。
//
// 各字段在运行结束时由 Spawn 汇总成 Result：事件流之外的 final_text / tokens /
// cost / session_id 全部从这里取。
type ParseState struct {
	Harness   string
	SessionID string // 运行产生的会话 id
	FinalText string // 最终回答全文

	TokensIn  int64
	TokensOut int64
	CostUSD   float64

	// Done 表示已收到协议层完成事件（hermes result / codex turn.completed /
	// claude result / pi agent_settled）。
	Done bool
	// ErrText 是最近一条错误/告警文本（网关 502、配置告警等），用于展示。
	ErrText string
	// Fatal 表示收到了协议层的致命失败信号（hermes result.error、codex turn.failed、
	// claude is_error）——此时即使 Done 也判 failed。
	//
	// 与 ErrText 的区别很关键：codex 会为「忽略未知配置项」这类告警发 item.type=error
	// 事件，那是噪声不是失败；若据此判 failed，正常的成功运行会被误标。
	Fatal bool

	// pending 累积文本分片（hermes 把回答切成 "P"/"ONG" 逐段下发），
	// 遇到非 text 事件或进程结束时冲刷为一条 llm.response。
	pending strings.Builder
}

// flushText 把累积的文本分片冲刷为一条 llm.response（无内容时返回 nil）。
func (st *ParseState) flushText(ts time.Time) []model.Event {
	if st.pending.Len() == 0 {
		return nil
	}
	text := st.pending.String()
	st.pending.Reset()
	return []model.Event{{
		Harness:   st.Harness,
		SessionID: st.SessionID,
		Time:      ts,
		Kind:      model.KindLLMResponse,
		Title:     "assistant",
		Content:   text,
	}}
}

// LineParser 是单家 harness 的行解析函数：纯函数，只依赖入参行与 state。
// 返回 0..N 个统一事件；无法识别/非 JSON 的行返回 nil。
type LineParser func(line []byte, st *ParseState) []model.Event

// Flusher 在进程结束时冲刷累积状态（返回剩余事件，通常为 nil）。
type Flusher func(st *ParseState) []model.Event

// ParserFor 返回某家 harness 的行解析器与结束冲刷函数。
func ParserFor(harness string) (LineParser, Flusher, bool) {
	switch harness {
	case HarnessHermes:
		return ParseHermesLine, FlushHermes, true
	case HarnessCodex:
		return ParseCodexLine, nil, true
	case HarnessClaude:
		return ParseClaudeLine, nil, true
	case HarnessPi:
		return ParsePiLine, nil, true
	}
	return nil, nil, false
}

// ---------------------------------------------------------------------------
// 运行结果
// ---------------------------------------------------------------------------

// Result 是一次 harness 运行的结果（对应 run_harness 一行）。
type Result struct {
	Harness    string
	Status     string // store.RunHarness{Done,Failed,Timeout,Stopped}
	FinalText  string
	SessionID  string
	TokensIn   int64
	TokensOut  int64
	CostUSD    float64
	DurationMs int64
	Error      string
	ExitCode   int
	Events     []model.Event
}

// Runner 抽象「一家 harness 的一次非交互运行」，便于测试与上层替换实现。
type Runner interface {
	Run(ctx context.Context, harness, prompt string) (Result, error)
}

// ---------------------------------------------------------------------------
// Spawn：Runner 的子进程实现
// ---------------------------------------------------------------------------

// Spawn 用真实子进程实现 Runner。
type Spawn struct {
	// RunDir 是本次对比批次的根目录；每家 harness 在 RunDir/<harness>/ 下运行。
	RunDir string
	// Timeout 是单家超时；<=0 用 DefaultTimeout。
	Timeout time.Duration
	// OnEvent 每解析出一条事件时回调（可为 nil，用于实时推送）。
	OnEvent func(harness string, ev model.Event)
	// OnStatus 在 harness 状态变化时回调（可为 nil）。
	OnStatus func(harness, status, errMsg string)
}

var _ Runner = (*Spawn)(nil)

func (s *Spawn) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultTimeout
}

// Run 启动一家 harness 跑 prompt，流式回调事件，返回终态 Result。
//
// 返回值语义：error 仅在「启动失败」等本地错误时非 nil；子进程自身的失败/超时
// 通过 Result.Status（failed/timeout）如实表达，不作为 Go error 抛出。
func (s *Spawn) Run(ctx context.Context, harness, prompt string) (Result, error) {
	res := Result{Harness: harness, Status: store.RunHarnessFailed}
	if !validHarness(harness) {
		return res, fmt.Errorf("未知 harness: %q", harness)
	}

	dir := filepath.Join(s.RunDir, harness)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Error = fmt.Sprintf("创建工作目录 %s: %v", dir, err)
		return res, fmt.Errorf("runner: %s", res.Error)
	}
	spec, err := BuildSpec(harness, dir, prompt)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}

	parse, flush, ok := ParserFor(harness)
	if !ok {
		return res, fmt.Errorf("runner: 无 %s 解析器", harness)
	}
	st := &ParseState{Harness: harness}

	timeout := s.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := spec.Command()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Error = err.Error()
		return res, fmt.Errorf("runner: stdout pipe: %w", err)
	}
	var stderr capBuffer
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Error = fmt.Sprintf("启动 %s 失败: %v", spec.Path, err)
		return res, fmt.Errorf("runner: %s", res.Error)
	}
	s.status(harness, store.RunHarnessRunning, "")

	// 超时/取消 → 杀整棵进程树（codex/pi 是 .cmd 包装器，真正干活的是子 node 进程）。
	killed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killTree(cmd)
		case <-killed:
		}
	}()

	var emitted []model.Event
	emit := func(evs []model.Event) {
		for _, ev := range evs {
			ev.Harness = harness
			if ev.SessionID == "" {
				ev.SessionID = st.SessionID
			}
			emitted = append(emitted, ev)
			if s.OnEvent != nil {
				s.OnEvent(harness, ev)
			}
		}
	}

	// 用 bufio.Reader.ReadBytes 而非 Scanner：pi 单行最大 ~118KB（message_start
	// 携带完整 system prompt），Scanner 的默认 64KB 上限会直接报错。
	reader := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if len(bytes.TrimSpace(trimmed)) > 0 {
				emit(parse(trimmed, st))
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				st.ErrText = strings.TrimSpace(strings.Join(
					[]string{st.ErrText, "读取输出: " + rerr.Error()}, "; "))
			}
			break
		}
	}
	if flush != nil {
		emit(flush(st))
	}

	werr := cmd.Wait()
	close(killed)
	res.DurationMs = time.Since(start).Milliseconds()
	res.FinalText = st.FinalText
	res.SessionID = st.SessionID
	res.TokensIn, res.TokensOut, res.CostUSD = st.TokensIn, st.TokensOut, st.CostUSD
	res.Events = emitted

	exitCode := 0
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	res.ExitCode = exitCode

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status = store.RunHarnessTimeout
		res.Error = fmt.Sprintf("超时（%s）", timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		res.Status = store.RunHarnessStopped
		res.Error = "已停止"
	case st.Fatal:
		// 协议层致命失败（网关 502 / turn.failed / is_error）——如实记录。
		res.Status = store.RunHarnessFailed
		res.Error = firstNonEmpty(st.ErrText, "运行失败")
	case !st.Done:
		// 退出码非 0 且未收到完成事件 → failed（docs/DESIGN-runner.md 第 5 节）。
		res.Status = store.RunHarnessFailed
		res.Error = firstNonEmpty(st.ErrText, stderr.summary(),
			fmt.Sprintf("进程退出码 %d，且未收到完成事件", exitCode))
	default:
		res.Status = store.RunHarnessDone
	}
	s.status(harness, res.Status, res.Error)
	return res, nil
}

// status 触发状态回调（OnStatus 为 nil 时无操作）。
func (s *Spawn) status(harness, status, errMsg string) {
	if s.OnStatus != nil {
		s.OnStatus(harness, status, errMsg)
	}
}

// capBuffer 是带上限的 stderr 收集器（超出后丢弃并置截断标记）。
type capBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	room := stderrCaptureLimit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

// summary 返回 stderr 摘要：取最后一行非空内容（通常是错误原因），截断过长文本。
func (c *capBuffer) summary() string {
	s := strings.TrimSpace(c.buf.String())
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 400 {
		last = last[:400] + "…"
	}
	if c.truncated {
		last += " (stderr 已截断)"
	}
	return last
}

// ---------------------------------------------------------------------------
// 共享工具
// ---------------------------------------------------------------------------

// metaJSON 把 meta map 序列化为 JSON，跳过空字符串与零值（保持库内 meta 干净）。
// encoding/json 对 map 按键名排序，故输出确定。
func metaJSON(m map[string]any) string {
	clean := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			if t == "" {
				continue
			}
		case int:
			if t == 0 {
				continue
			}
		case int64:
			if t == 0 {
				continue
			}
		case *int:
			if t == nil {
				continue
			}
			clean[k] = *t
			continue
		case *bool:
			if t == nil {
				continue
			}
			clean[k] = *t
			continue
		case *float64:
			if t == nil {
				continue
			}
			clean[k] = *t
			continue
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return ""
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(b)
}

// indentJSON 把 JSON 原文美化为缩进文本（tool.call 的 content 用）。
// 非法 JSON 原样返回，保证信息不丢。
func indentJSON(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, t, "", "  ") != nil {
		return string(t)
	}
	return buf.String()
}

// msTime 把 epoch 毫秒时间戳转为 UTC 时间（<=0 返回零值，由 store 填当前时间）。
func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// rfcTime 解析 RFC3339 时间串（失败返回零值）。
func rfcTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// usageContent 生成 llm.usage 的可读摘要（列表展示用）。
func usageContent(in, out int64, cost float64) string {
	parts := []string{fmt.Sprintf("in=%s out=%s", strconv.FormatInt(in, 10), strconv.FormatInt(out, 10))}
	if cost > 0 {
		parts = append(parts, "cost=$"+strconv.FormatFloat(cost, 'f', 6, 64))
	}
	return strings.Join(parts, " ")
}
