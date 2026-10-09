// claude 采集器：~/.claude/projects/**/*.jsonl（对话 JSONL）→ 内容级事件。
//
// 数据源格式见 docs/DATA-FORMATS.md §3。实测 12 个文件 / 0.8 MB（其中 1 个子代理转录），
// 每行一个 JSON：{parentUuid, isSidechain, type(user|assistant|...), message{...}, timestamp, ...}。
// 仍按流式逐行解析（bufio.Scanner），坏行跳过并计数，不中断。
//
// 事件映射（一一对应 docs/DESIGN.md 的 kind 规范）：
//
//	每文件一条 session.start（session_id 由文件路径派生；见 claudeSessionID）
//	user.message.content 为字符串                      → turn.start
//	user.message.content[] {type:text}                 → turn.start
//	user.message.content[] {type:tool_result}          → tool.result（parent_id=tool_use_id）
//	assistant.message.content[] {type:thinking}        → llm.reasoning
//	assistant.message.content[] {type:text}            → llm.response
//	assistant.message.content[] {type:tool_use}        → tool.call（content=参数 JSON）
//	{type:cost-state}（每文件末尾一条会话汇总）        → llm.usage（title=模型名；meta: token/费用/分模型明细）
//	其余顶层 type（mode/queue-operation/attachment/system/...） → 不映射，计数
//
// 会话划分：session_id = 文件名 uuid（去掉 .jsonl）；子代理转录
// （<uuid>/subagents/agent-*.jsonl）用 "<父uuid>/subagents/<agent名>"。
// 实测 12 个文件 → 12 个不同 session_id（含 1 个子代理）。
//
// 幂等键：source_key = "claude:" + 文件相对路径 + ":" + 行号；一行若产出多个事件
// （content 数组含多个块）则在末尾追加 ":<块序号>" 以保唯一。session.start 是每文件
// 一条的会话级事件，不属任何一行，键用 "claude:<相对路径>:session.start"。
package collect

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

const claudeHarness = "claude"

const (
	// claudeScannerMaxToken 是单行上限；实测文件很小，留足余量。
	claudeScannerMaxToken = 8 << 20
	// 分批入库双上限：任一到阈值即 flush，限制单次内存占用。
	claudeBatchMaxEvents = 2000
	claudeBatchMaxBytes  = 16 << 20
)

type claudeCollector struct{}

func init() { Register(claudeCollector{}) }

func (claudeCollector) Name() string { return claudeHarness }

// ClaudeProjectsDir 返回 projects 根目录：CLAUDE_PROJECTS_DIR 优先（测试/覆盖用），
// 否则 ~/.claude/projects。
func ClaudeProjectsDir() (string, error) {
	if d := os.Getenv("CLAUDE_PROJECTS_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("解析用户目录: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// Collect 流式扫描全部对话 JSONL 并幂等写入事件。
func (claudeCollector) Collect(sink Sink) (Stats, error) {
	return claudeCollect(sink, nil)
}

// CollectIncremental 实现增量续读：从上次保存的 offset 起读各文件新增行。
//
// 首次运行（无 ingest_state）时，对已入库的历史文件从末尾起跟、对新文件从头补，
// 保证既不重复扫历史也不漏采。
func (claudeCollector) CollectIncremental(sink TailSink) (int, bool, error) {
	states, err := sink.IngestStates(claudeHarness)
	if err != nil {
		return 0, false, err
	}
	stats, err := claudeCollect(sink, states)
	return stats.Inserted, true, err
}

// claudeCollect 是 Collect 与 CollectIncremental 的共用实现。
// states 为 nil 时全量扫描；非 nil 时按各文件已存 offset 增量续读。
func claudeCollect(sink Sink, states map[string]model.IngestPos) (Stats, error) {
	stats := Stats{Harness: claudeHarness}

	root, err := ClaudeProjectsDir()
	if err != nil {
		return stats, err
	}
	if fi, err := os.Stat(root); err != nil {
		return stats, fmt.Errorf("projects 目录不可用 %s: %w", root, err)
	} else if !fi.IsDir() {
		return stats, fmt.Errorf("projects 路径不是目录: %s", root)
	}
	files, err := claudeFiles(root)
	if err != nil {
		return stats, err
	}

	r := &claudeRun{sink: sink, root: root, unmapped: map[string]int{}}
	for _, p := range files {
		if err := r.collectFile(p, states); err != nil {
			return stats, err
		}
	}
	if err := r.flush(); err != nil {
		return stats, err
	}
	if err := r.saveStates(sink, states); err != nil {
		return stats, err
	}

	stats.Scanned = r.lines
	stats.Inserted = r.inserted
	stats.Skipped = r.skipped
	stats.BadLines = r.bad
	stats.Increment = states != nil
	r.report()
	return stats, nil
}

// claudeFiles 递归收集 root 下的 *.jsonl，按路径升序（顺序稳定 → seq 稳定）。
func claudeFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".jsonl") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("遍历 %s: %w", root, err)
	}
	sort.Strings(files)
	return files, nil
}

// ---- 单文件采集 -----------------------------------------------------------

type claudeRun struct {
	sink                                 Sink
	root                                 string
	batch                                []model.Event
	bytes                                int
	inserted, skipped, bad, lines, files int
	skippedKnown                         int            // 已识别但无内容/按规范不出事件
	unmapped                             map[string]int // 未映射顶层 type / 块类型 → 次数
	err                                  error
	nextPos                              map[string]model.IngestPos // 增量模式下各文件的新位置
}

// claudeFileInfo 是每文件的会话级元数据（用于 session.start）。
type claudeFileInfo struct {
	cwd         string
	isSidechain *bool
	time        time.Time
}

func (r *claudeRun) collectFile(p string, states map[string]model.IngestPos) error {
	if r.err != nil {
		return r.err
	}
	rel, err := filepath.Rel(r.root, p)
	if err != nil {
		rel = filepath.Base(p)
	}
	rel = filepath.ToSlash(rel)
	r.files++

	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	start, incremental := resolveStart(rel, states, fi.Size())
	pos := start

	sid := claudeSessionID(rel)

	// 只有从头读的文件才发 session.start（会话级事件；历史文件续读时不重复）。
	if !incremental || start.Offset == 0 {
		info, err := claudeFileMeta(p)
		if err != nil {
			return fmt.Errorf("读取 %s: %w", rel, err)
		}
		sidechain := false
		if info.isSidechain != nil {
			sidechain = *info.isSidechain
		}
		r.emit(model.Event{
			Harness:   claudeHarness,
			SessionID: sid,
			Time:      info.time,
			Kind:      model.KindSessionStart,
			Title:     claudeTitle(info.cwd),
			Meta:      claudeMeta(map[string]any{"cwd": info.cwd, "isSidechain": sidechain}),
			SourceKey: claudeHarness + ":" + rel + ":session.start",
		})
	}

	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", rel, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxToken)
	sc.Split(splitCompleteLines)
	lineNo := int(pos.Lines)
	if pos.Offset > 0 {
		if _, err := f.Seek(pos.Offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek %s: %w", rel, err)
		}
	}
	consumed := start.Offset
	for sc.Scan() {
		consumed += int64(len(sc.Bytes())) + 1
		lineNo++
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue // 空行不计入扫描
		}
		r.lines++

		var rec claudeRecord
		if json.Unmarshal(line, &rec) != nil {
			r.bad++
			continue
		}
		r.mapRecord(rec, rel, lineNo, sid)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取 %s: %w", rel, err)
	}
	pos.Offset = consumed
	pos.Lines = int64(lineNo)
	pos.Size = fi.Size()
	pos.MTime = fi.ModTime()
	if r.nextPos == nil {
		r.nextPos = map[string]model.IngestPos{}
	}
	r.nextPos[rel] = pos
	return r.err
}

// saveStates 增量模式下把各文件新位置写回 store（全量模式为 no-op）。
func (r *claudeRun) saveStates(sink Sink, states map[string]model.IngestPos) error {
	if states == nil || len(r.nextPos) == 0 {
		return nil
	}
	ts, ok := sink.(TailSink)
	if !ok {
		return nil
	}
	return ts.SaveIngestStates(claudeHarness, r.nextPos)
}

// ---- 记录解析结构 ---------------------------------------------------------

type claudeRecord struct {
	Type        string         `json:"type"`
	IsSidechain *bool          `json:"isSidechain"`
	Cwd         string         `json:"cwd"`
	SessionID   string         `json:"sessionId"`
	Timestamp   string         `json:"timestamp"`
	Uuid        string         `json:"uuid"`
	ParentUuid  string         `json:"parentUuid"`
	Message     *claudeMessage `json:"message"`

	// cost-state 行（每文件一条，写在会话末尾）：会话级用量与费用汇总。
	// 实测字段见 docs/DATA-FORMATS.md §3。
	TotalCostUSD  *float64                    `json:"totalCostUSD"`
	TotalDuration *float64                    `json:"totalDuration"`
	StartTime     *float64                    `json:"startTime"`
	ModelUsage    map[string]claudeModelUsage `json:"modelUsage"`
}

// claudeModelUsage 是 cost-state.modelUsage 里单个模型的用量（键是「provider/模型名」）。
type claudeModelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	ThinkingTokens           int64   `json:"thinkingTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	WebSearchRequests        int64   `json:"webSearchRequests"`
	CostUSD                  float64 `json:"costUSD"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"` // 字符串 或 块数组
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result 的内容：字符串 或 块数组
}

// ---- 映射 -----------------------------------------------------------------

// mapRecord 把一行映射为 0..N 个事件并赋幂等键后入库。
func (r *claudeRun) mapRecord(rec claudeRecord, rel string, lineNo int, sid string) {
	when := parseClaudeTime(rec.Timestamp)

	var events []model.Event
	switch rec.Type {
	case "user":
		events = r.userEvents(rec, sid, when)
	case "assistant":
		events = r.assistantEvents(rec, sid, when)
	case "cost-state":
		// 会话级用量汇总：库里 claude 此前一条 llm.usage 都没有（实测 0 条），
		// 而文件末尾这行带着完整 token/费用账。不采它 = 这个 harness 的成本不可见。
		events = r.costStateEvents(rec, sid, when)
	default:
		r.unmapped[rec.Type]++
		return
	}

	if len(events) == 0 {
		r.skippedKnown++ // 该行可识别但无内容（如空 thinking）
		return
	}
	base := claudeSourceKey(rel, lineNo)
	for i := range events {
		if len(events) == 1 {
			events[i].SourceKey = base
		} else {
			// 一行多事件：追加块序号保证 source_key 唯一（确定性 → 幂等）。
			events[i].SourceKey = fmt.Sprintf("%s:%d", base, i)
		}
		r.emit(events[i])
	}
}

// userEvents 处理 type=="user" 的行。
//
//	content 为字符串          → 单个 turn.start
//	content 为块数组          → {text}→turn.start；{tool_result}→tool.result
func (r *claudeRun) userEvents(rec claudeRecord, sid string, when time.Time) []model.Event {
	if rec.Message == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(rec.Message.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) != nil || s == "" {
			return nil
		}
		return []model.Event{{
			Harness: claudeHarness, SessionID: sid, Time: when,
			Kind: model.KindTurnStart, Title: "user", Content: s,
		}}
	}

	if trimmed[0] != '[' {
		return nil
	}
	var blocks []claudeBlock
	if json.Unmarshal(trimmed, &blocks) != nil {
		return nil
	}
	var out []model.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			out = append(out, model.Event{
				Harness: claudeHarness, SessionID: sid, Time: when,
				Kind: model.KindTurnStart, Title: "user", Content: b.Text,
			})
		case "tool_result":
			out = append(out, model.Event{
				Harness: claudeHarness, SessionID: sid, Time: when,
				Kind: model.KindToolResult, Title: "tool result",
				Content:  claudeToolResultContent(b.Content),
				Meta:     claudeMeta(map[string]any{"tool_use_id": b.ToolUseID}),
				ParentID: b.ToolUseID,
			})
		default:
			r.unmapped["user/"+b.Type]++
		}
	}
	return out
}

// assistantEvents 处理 type=="assistant" 的行（content 数组）：
//
//	{thinking}  → llm.reasoning
//	{text}      → llm.response
//	{tool_use}  → tool.call（content=参数 JSON 原文）
func (r *claudeRun) assistantEvents(rec claudeRecord, sid string, when time.Time) []model.Event {
	if rec.Message == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(rec.Message.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	// 防御：assistant content 理论上恒为数组，仍兼容字符串形态。
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) != nil || s == "" {
			return nil
		}
		return []model.Event{{
			Harness: claudeHarness, SessionID: sid, Time: when,
			Kind: model.KindLLMResponse, Title: "assistant", Content: s,
		}}
	}
	if trimmed[0] != '[' {
		return nil
	}

	var blocks []claudeBlock
	if json.Unmarshal(trimmed, &blocks) != nil {
		return nil
	}
	var out []model.Event
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			if b.Thinking == "" {
				continue
			}
			out = append(out, model.Event{
				Harness: claudeHarness, SessionID: sid, Time: when,
				Kind: model.KindLLMReasoning, Title: "reasoning", Content: b.Thinking,
			})
		case "text":
			if b.Text == "" {
				continue
			}
			out = append(out, model.Event{
				Harness: claudeHarness, SessionID: sid, Time: when,
				Kind: model.KindLLMResponse, Title: "assistant", Content: b.Text,
			})
		case "tool_use":
			out = append(out, model.Event{
				Harness: claudeHarness, SessionID: sid, Time: when,
				Kind: model.KindToolCall, Title: b.Name, Content: claudeInputJSON(b.Input),
				Meta: claudeMeta(map[string]any{"tool_name": b.Name, "call_id": b.ID}),
			})
		default:
			r.unmapped["assistant/"+b.Type]++
		}
	}
	return out
}

// costStateEvents 处理 type=="cost-state" 的行（每文件一条，写在会话末尾）。
//
// 它给出这个会话的**真实账**：总费用 + 分模型的 token 明细（含缓存读写、思考 token）。
// 事件时间优先用行内 timestamp，缺失时退回 startTime（毫秒 epoch）。
// 每行一个 llm.usage 事件，title 用模型名（与 hermes 采集器一致：title=模型名），
// 这样 GUI 时间线上「用量」条目直接显示是哪个模型。
func (r *claudeRun) costStateEvents(rec claudeRecord, sid string, when time.Time) []model.Event {
	if len(rec.ModelUsage) == 0 {
		// 空账：实测存在 totalCostUSD=0 且 modelUsage 为空的收尾行（该会话没有模型调用）。
		// 按「没有模型明细就没有可报的用量」跳过，避免时间线上出现零值噪声条目。
		return nil
	}
	if when.IsZero() && rec.StartTime != nil {
		when = time.UnixMilli(int64(*rec.StartTime)).UTC()
	}

	// 分模型明细按名排序，保证同一文件重复采集得到相同的 meta（幂等）。
	names := make([]string, 0, len(rec.ModelUsage))
	for name := range rec.ModelUsage {
		names = append(names, name)
	}
	sort.Strings(names)

	var (
		in, out, thinking, cacheRead, cacheCreate int64
		models                                    []map[string]any
	)
	for _, name := range names {
		u := rec.ModelUsage[name]
		in += u.InputTokens
		out += u.OutputTokens
		thinking += u.ThinkingTokens
		cacheRead += u.CacheReadInputTokens
		cacheCreate += u.CacheCreationInputTokens
		models = append(models, map[string]any{
			"model":                 name,
			"input_tokens":          u.InputTokens,
			"output_tokens":         u.OutputTokens,
			"thinking_tokens":       u.ThinkingTokens,
			"cache_read_tokens":     u.CacheReadInputTokens,
			"cache_creation_tokens": u.CacheCreationInputTokens,
			"web_search_requests":   u.WebSearchRequests,
			"cost_usd":              u.CostUSD,
		})
	}

	totalCost := 0.0
	if rec.TotalCostUSD != nil {
		totalCost = *rec.TotalCostUSD
	}
	title := "会话用量"
	if len(names) == 1 {
		title = names[0]
	}

	return []model.Event{{
		Harness:   claudeHarness,
		SessionID: sid,
		Time:      when,
		Kind:      model.KindLLMUsage,
		Title:     title,
		Meta: claudeMeta(map[string]any{
			"source":                "cost-state",
			"input_tokens":          in,
			"output_tokens":         out,
			"thinking_tokens":       thinking,
			"cache_read_tokens":     cacheRead,
			"cache_creation_tokens": cacheCreate,
			"estimated_cost_usd":    totalCost,
			"models":                models,
			"total_duration_ms":     rec.TotalDuration,
		}),
	}}
}

// ---- 入库（双上限分批） ----------------------------------------------------

func (r *claudeRun) emit(ev model.Event) {
	r.batch = append(r.batch, ev)
	r.bytes += len(ev.Content) + len(ev.Meta) + len(ev.Title) + len(ev.SourceKey) + 64
	if len(r.batch) >= claudeBatchMaxEvents || r.bytes >= claudeBatchMaxBytes {
		if err := r.flush(); err != nil && r.err == nil {
			r.err = err
		}
	}
}

func (r *claudeRun) flush() error {
	if r.err != nil {
		return r.err
	}
	if len(r.batch) == 0 {
		return nil
	}
	ins, skip, err := r.sink.InsertEvents(r.batch)
	r.inserted += ins
	r.skipped += skip
	r.batch = r.batch[:0]
	r.bytes = 0
	return err
}

// report 把详细统计打印到 stderr（files/lines/events/bad/未映射类型）。
// 默认静默（双击 exe 场景控制台干净）；HM_DEBUG=1 时输出。
func (r *claudeRun) report() {
	if os.Getenv("HM_DEBUG") == "" {
		return
	}
	out := os.Stderr
	fmt.Fprintf(out, "claude: files=%d lines=%d events=%d inserted=%d skipped=%d bad=%d known_skip=%d\n",
		r.files, r.lines, r.inserted+r.skipped, r.inserted, r.skipped, r.bad, r.skippedKnown)

	if len(r.unmapped) > 0 {
		keys := make([]string, 0, len(r.unmapped))
		total := 0
		for k, v := range r.unmapped {
			keys = append(keys, k)
			total += v
		}
		sort.Slice(keys, func(i, j int) bool {
			if r.unmapped[keys[i]] != r.unmapped[keys[j]] {
				return r.unmapped[keys[i]] > r.unmapped[keys[j]]
			}
			return keys[i] < keys[j]
		})
		fmt.Fprintf(out, "claude: unmapped=%d\n", total)
		for _, k := range keys {
			fmt.Fprintf(out, "claude:   %s x%d\n", k, r.unmapped[k])
		}
	}
}

// ---- 工具 -----------------------------------------------------------------

func claudeSourceKey(rel string, lineNo int) string {
	return fmt.Sprintf("%s:%s:%d", claudeHarness, rel, lineNo)
}

// claudeSessionID 由相对路径派生会话 ID：
//
//	<proj>/<uuid>.jsonl                              → "<uuid>"
//	<proj>/<uuid>/subagents/agent-<id>.jsonl         → "<uuid>/subagents/agent-<id>"
func claudeSessionID(rel string) string {
	base := strings.TrimSuffix(rel, path.Ext(rel))
	dir := path.Dir(base)
	if path.Base(dir) == "subagents" {
		parent := path.Base(path.Dir(dir))
		return parent + "/subagents/" + path.Base(base)
	}
	return path.Base(base)
}

// claudeFileMeta 预扫文件，取会话级元数据：首个非空 cwd、首个 isSidechain、
// 首个非空 timestamp。见到 cwd 即可停止（实测 cwd 与 isSidechain 同现于首个消息行）。
func claudeFileMeta(p string) (claudeFileInfo, error) {
	var info claudeFileInfo
	f, err := os.Open(p)
	if err != nil {
		return info, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxToken)
	for sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec claudeRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if info.time.IsZero() && rec.Timestamp != "" {
			info.time = parseClaudeTime(rec.Timestamp)
		}
		if info.isSidechain == nil && rec.IsSidechain != nil {
			info.isSidechain = rec.IsSidechain
		}
		if info.cwd == "" && rec.Cwd != "" {
			info.cwd = rec.Cwd
			break
		}
	}
	return info, sc.Err()
}

// claudeInputJSON 返回 tool_use.input 的参数 JSON 原文（一字不改）。
//
// 注意：不能用 json.Marshal(input) —— Go 默认会做 HTML 转义，把 > < & 写成
// \u003e \u003c \u0026（实测 42 条 tool_use 中 4 条命中，如 "2>/dev/null"），
// 那不是原文。RawMessage 本身即源文件中该值的原始字节，直接透传最忠实，
// 也与其他采集器（codex/hermes 的 arguments 原文）一致。
func claudeInputJSON(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	return string(t)
}

// claudeToolResultContent 还原 tool_result.content：字符串原样返回；
// 块数组时取各 text 块按序拼接。
func claudeToolResultContent(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return ""
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(t, &s) == nil {
			return s
		}
		return string(t)
	}
	if t[0] == '[' {
		var blocks []claudeBlock
		if json.Unmarshal(t, &blocks) == nil {
			var sb strings.Builder
			for _, b := range blocks {
				if b.Type == "text" {
					sb.WriteString(b.Text)
				}
			}
			return sb.String()
		}
	}
	return string(t)
}

// claudeTitle 取 cwd 最后一段作为标题（同时兼容 / 与 \ 分隔）；为空时用默认名。
func claudeTitle(cwd string) string {
	s := strings.ReplaceAll(cwd, "\\", "/")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "claude session"
	}
	return s
}

// claudeMeta 把 meta map 序列化为 JSON，跳过空字符串与 nil 指针。
func claudeMeta(m map[string]any) string {
	clean := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			if t == "" {
				continue
			}
		case *float64:
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
		}
		clean[k] = v
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseClaudeTime 解析 ISO8601（UTC，Z 结尾，含毫秒）。
func parseClaudeTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
