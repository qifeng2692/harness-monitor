// codex 采集器：~/.codex/sessions/**/*.jsonl（rollout JSONL）→ 内容级事件。
//
// 数据源格式见 docs/DATA-FORMATS.md §2。实测 141 个文件 / 616.5 MB，单文件最大
// 172 MB，单行最长 8.2 MB——必须流式按行解析（bufio.Scanner + 32 MB 上限），
// 禁止一次性 ReadAll；事件按条数 + 字节双上限分批入库，控制内存。
//
// 事件映射（一一对应 docs/DESIGN.md 的 kind 规范）：
//
//	session_meta                                    → session.start
//	event_msg/item_completed item.type=UserMessage  → turn.start（权威来源）
//	response_item/message role=user                 → turn.start（仅当本文件无 UserMessage 时降级）
//	response_item/message role=assistant            → llm.response
//	response_item/reasoning                         → llm.reasoning
//	response_item/function_call                     → tool.call
//	response_item/function_call_output              → tool.result
//	event_msg/task_started                          → 不单独出事件（turn 由 UserMessage 起）
//	event_msg/task_complete                         → turn.end
//	event_msg/token_count                           → llm.usage
//
// 幂等键：source_key = "codex:" + 文件相对路径 + ":" + 行号（稳定、幂等）。
// 坏行与未映射类型都跳过并计数，采集结束打印统计。
//
// 会话划分：session_id = session_meta.payload.session_id（同一 thread 被多次
// 追加会产生多个 rollout 文件，它们共享同一 session_id，汇入同一条时间线）。
package collect

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qf/harness-monitor/internal/model"
)

const codexHarness = "codex"

const (
	// codexScannerMaxToken 是单行上限；实测最长 8.2 MB，留 4x 余量。
	codexScannerMaxToken = 32 << 20
	// 分批入库双上限：任一到阈值即 flush，限制单次内存占用。
	codexBatchMaxEvents = 2000
	codexBatchMaxBytes  = 32 << 20
)

// codexInjectedPrefixes 是系统注入内容的前缀（不是用户真实输入，跳过）。
// DATA-FORMATS §2.1 点名 AGENTS.md 全文与 <INSTRUCTIONS>；这里补全实测出现的同类注入。
var codexInjectedPrefixes = []string{
	"<INSTRUCTIONS>",
	"<app-context>",
	"<environment_context>",
	"<turn_aborted>",
	"<in-app-browser-context",
	"# AGENTS.md instructions",
	"The following is the Codex agent history",
}

type codexCollector struct{}

func init() { Register(codexCollector{}) }

func (codexCollector) Name() string { return codexHarness }

// CodexSessionsDir 返回 rollout 根目录：CODEX_SESSIONS_DIR 优先（测试/覆盖用），
// 否则 ~/.codex/sessions。
func CodexSessionsDir() (string, error) {
	if d := os.Getenv("CODEX_SESSIONS_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("解析用户目录: %w", err)
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// Collect 流式扫描全部 rollout JSONL 并幂等写入事件。
func (codexCollector) Collect(sink Sink) (Stats, error) {
	return codexCollect(sink, nil)
}

// CollectIncremental 实现增量续读：从上次保存的 offset 起读各文件新增行。
//
// 首次运行（无 ingest_state）时，对已入库的历史文件从末尾起跟、对新文件从头补，
// 保证既不重复扫历史（172 MB 文件秒级完成）也不漏采。之后每轮只读新增字节。
func (codexCollector) CollectIncremental(sink TailSink) (int, bool, error) {
	states, err := sink.IngestStates(codexHarness)
	if err != nil {
		return 0, false, err
	}
	stats, err := codexCollect(sink, states)
	return stats.Inserted, true, err
}

// codexCollect 是 Collect 与 CollectIncremental 的共用实现。
// states 为 nil 时全量扫描；非 nil 时按各文件已存 offset 增量续读。
func codexCollect(sink Sink, states map[string]model.IngestPos) (Stats, error) {
	stats := Stats{Harness: codexHarness}

	root, err := CodexSessionsDir()
	if err != nil {
		return stats, err
	}
	if fi, err := os.Stat(root); err != nil {
		return stats, fmt.Errorf("rollout 目录不可用 %s: %w", root, err)
	} else if !fi.IsDir() {
		return stats, fmt.Errorf("rollout 路径不是目录: %s", root)
	}
	files, err := codexFiles(root)
	if err != nil {
		return stats, err
	}

	r := &codexRun{sink: sink, root: root, unmapped: map[string]int{}}
	for _, path := range files {
		if err := r.collectFile(path, states); err != nil {
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
	r.report(stats)
	return stats, nil
}

// codexFiles 递归收集 root 下的 *.jsonl，按路径升序（= 时间先后，保证 seq 顺序稳定）。
func codexFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".jsonl") {
			files = append(files, path)
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

type codexRun struct {
	sink                                 Sink
	root                                 string
	batch                                []model.Event
	bytes                                int
	inserted, skipped, bad, lines, files int
	unmapped                             map[string]int // 未映射类型 → 次数
	skippedKnown                         int            // 已识别但按规范不出事件/无内容
	heapPeak                             uint64         // 采集期间 HeapAlloc 峰值
	err                                  error
	nextPos                              map[string]model.IngestPos // 增量模式下各文件的新位置
}

func (r *codexRun) collectFile(path string, states map[string]model.IngestPos) error {
	if r.err != nil {
		return r.err
	}
	rel, err := filepath.Rel(r.root, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	rel = filepath.ToSlash(rel)
	r.files++

	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	start, incremental := resolveStart(rel, states, fi.Size())
	pos := start

	// 预扫：本文件是否有权威的 UserMessage（决定是否降级 response_item/message user）。
	// 增量模式下只对从头读的文件做预扫；从中间续读时该降级决策不适用（历史已入库）。
	hasUser := false
	if !incremental || start.Offset == 0 {
		hasUser, err = codexFileHasUserMessage(path)
		if err != nil {
			return err
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", rel, err)
	}
	defer f.Close()

	fallbackSID := codexFileUUID(path)
	sessionID := ""

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), codexScannerMaxToken)
	sc.Split(splitCompleteLines)
	lineNo := int(pos.Lines)
	if pos.Offset > 0 {
		if _, err := f.Seek(pos.Offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek %s: %w", rel, err)
		}
	}
	// consumed 是“已完整消费的字节数”（每条完整行含其换行符）；末尾半行不计入。
	consumed := start.Offset
	for sc.Scan() {
		consumed += int64(len(sc.Bytes())) + 1
		lineNo++
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue // 空行不计入扫描
		}
		r.lines++

		var rec codexRecord
		if json.Unmarshal(line, &rec) != nil {
			r.bad++
			continue
		}

		if rec.Type == "session_meta" {
			sid := r.emitSessionMeta(rec, rel, lineNo)
			if sid != "" {
				sessionID = sid
			}
			continue
		}

		sid := sessionID
		if sid == "" {
			sid = fallbackSID
		}
		r.mapRecord(rec, rel, lineNo, sid, hasUser)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取 %s: %w", rel, err)
	}
	// 记录本文件新位置：offset = 已完整消费的字节数。
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
func (r *codexRun) saveStates(sink Sink, states map[string]model.IngestPos) error {
	if states == nil || len(r.nextPos) == 0 {
		return nil
	}
	ts, ok := sink.(TailSink)
	if !ok {
		return nil
	}
	return ts.SaveIngestStates(codexHarness, r.nextPos)
}

// ---- 记录解析结构 ---------------------------------------------------------

type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content []codexBlock    `json:"content"`
	Item    *codexItem      `json:"item"`
	Name    string          `json:"name"`
	CallID  string          `json:"call_id"`
	ArgRaw  json.RawMessage `json:"arguments"`
	Output  json.RawMessage `json:"output"`
	Info    json.RawMessage `json:"info"`

	DurationMs         *float64 `json:"duration_ms"`
	TimeToFirstTokenMs *float64 `json:"time_to_first_token_ms"`

	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	Originator    string `json:"originator"`
	CliVersion    string `json:"cli_version"`
	ModelProvider string `json:"model_provider"`
	TurnID        string `json:"turn_id"`
}

type codexItem struct {
	Type    string       `json:"type"`
	Content []codexBlock `json:"content"`
}

type codexBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

func codexDecode(raw json.RawMessage, v any) bool {
	if len(raw) == 0 {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

// ---- 映射 -----------------------------------------------------------------

// emitSessionMeta 处理 session_meta → session.start，返回其中的 session_id。
func (r *codexRun) emitSessionMeta(rec codexRecord, rel string, lineNo int) string {
	var p codexPayload
	if !codexDecode(rec.Payload, &p) {
		r.bad++
		return ""
	}
	r.emit(model.Event{
		Harness:   codexHarness,
		SessionID: p.SessionID,
		Time:      parseCodexTime(rec.Timestamp),
		Kind:      model.KindSessionStart,
		Title:     codexCwdTitle(p.Cwd),
		Meta: codexMeta(map[string]any{
			"cwd":            p.Cwd,
			"originator":     p.Originator,
			"cli_version":    p.CliVersion,
			"model_provider": p.ModelProvider,
		}),
		SourceKey: codexSourceKey(rel, lineNo),
	})
	return p.SessionID
}

func (r *codexRun) mapRecord(rec codexRecord, rel string, lineNo int, sid string, hasUser bool) {
	switch rec.Type {
	case "response_item":
		r.mapResponseItem(rec, rel, lineNo, sid, hasUser)
	case "event_msg":
		r.mapEventMsg(rec, rel, lineNo, sid)
	default:
		r.unmapped[rec.Type]++
	}
}

func (r *codexRun) mapResponseItem(rec codexRecord, rel string, lineNo int, sid string, hasUser bool) {
	var p codexPayload
	if !codexDecode(rec.Payload, &p) {
		r.bad++
		return
	}
	base := codexSourceKey(rel, lineNo)
	when := parseCodexTime(rec.Timestamp)

	switch p.Type {
	case "message":
		switch p.Role {
		case "user":
			if hasUser {
				r.skippedKnown++ // 有权威 UserMessage，不降级
				return
			}
			text := joinBlocks(p.Content, "input_text")
			if text == "" || codexIsInjected(text) {
				r.skippedKnown++
				return
			}
			r.emit(model.Event{
				Harness: codexHarness, SessionID: sid, Time: when,
				Kind: model.KindTurnStart, Title: "user", Content: text,
				Meta:      codexMeta(map[string]any{"role": "user", "source": "response_item"}),
				SourceKey: base,
			})
		case "assistant":
			text := joinBlocks(p.Content, "output_text")
			if text == "" || codexIsInjected(text) {
				r.skippedKnown++
				return
			}
			r.emit(model.Event{
				Harness: codexHarness, SessionID: sid, Time: when,
				Kind: model.KindLLMResponse, Title: "assistant", Content: text,
				SourceKey: base,
			})
		default:
			r.skippedKnown++ // developer 等系统角色
		}

	case "reasoning":
		if len(p.Content) == 0 || p.Content[0].Text == "" {
			r.skippedKnown++ // 推理被加密/清空（实测 1140 例）
			return
		}
		r.emit(model.Event{
			Harness: codexHarness, SessionID: sid, Time: when,
			Kind: model.KindLLMReasoning, Title: "reasoning", Content: p.Content[0].Text,
			SourceKey: base,
		})

	case "function_call":
		r.emit(model.Event{
			Harness: codexHarness, SessionID: sid, Time: when,
			Kind: model.KindToolCall, Title: p.Name, Content: codexArgsToString(p.ArgRaw),
			Meta:      codexMeta(map[string]any{"name": p.Name, "call_id": p.CallID}),
			SourceKey: base,
		})

	case "function_call_output":
		content, images := codexOutputContent(p.Output)
		meta := map[string]any{"call_id": p.CallID}
		if images > 0 {
			meta["images"] = images // 图片块未内联（见 codexOutputContent 说明）
		}
		r.emit(model.Event{
			Harness: codexHarness, SessionID: sid, Time: when,
			Kind: model.KindToolResult, Title: "tool result", Content: content,
			Meta:      codexMeta(meta),
			ParentID:  p.CallID,
			SourceKey: base,
		})

	default:
		r.unmapped["response_item/"+p.Type]++
	}
}

func (r *codexRun) mapEventMsg(rec codexRecord, rel string, lineNo int, sid string) {
	var p codexPayload
	if !codexDecode(rec.Payload, &p) {
		r.bad++
		return
	}
	base := codexSourceKey(rel, lineNo)
	when := parseCodexTime(rec.Timestamp)

	switch p.Type {
	case "item_completed":
		if p.Item == nil {
			r.unmapped["event_msg/item_completed/<nil>"]++
			return
		}
		if p.Item.Type != "UserMessage" {
			// 其余 item（CommandExecution/AgentMessage/Reasoning/...）与 response_item 重复，
			// 按规范不单独出事件。
			r.unmapped["event_msg/item_completed/"+p.Item.Type]++
			return
		}
		text := ""
		if len(p.Item.Content) > 0 {
			text = p.Item.Content[0].Text
		}
		r.emit(model.Event{
			Harness: codexHarness, SessionID: sid, Time: when,
			Kind: model.KindTurnStart, Title: "user", Content: text,
			Meta:      codexMeta(map[string]any{"turn_id": p.TurnID}),
			SourceKey: base,
		})

	case "task_started":
		r.skippedKnown++ // turn 由 UserMessage 起，不单独出事件

	case "task_complete":
		r.emit(model.Event{
			Harness: codexHarness, SessionID: sid, Time: when,
			Kind: model.KindTurnEnd, Title: "turn complete",
			Meta: codexMeta(map[string]any{
				"turn_id":                p.TurnID,
				"duration_ms":            p.DurationMs,
				"time_to_first_token_ms": p.TimeToFirstTokenMs,
			}),
			SourceKey: base,
		})

	case "token_count":
		content := ""
		if len(rec.Payload) > 0 {
			content = string(rec.Payload)
		}
		meta := ""
		if len(p.Info) > 0 {
			meta = string(p.Info) // {"total_token_usage":{...},"last_token_usage":{...},...}
		}
		r.emit(model.Event{
			Harness: codexHarness, SessionID: sid, Time: when,
			Kind: model.KindLLMUsage, Title: "token_count", Content: content,
			Meta: meta, SourceKey: base,
		})

	default:
		r.unmapped["event_msg/"+p.Type]++
	}
}

// ---- 入库（双上限分批） ----------------------------------------------------

func (r *codexRun) emit(ev model.Event) {
	r.batch = append(r.batch, ev)
	r.bytes += len(ev.Content) + len(ev.Meta) + len(ev.Title) + len(ev.SourceKey) + 64
	if len(r.batch) >= codexBatchMaxEvents || r.bytes >= codexBatchMaxBytes {
		if err := r.flush(); err != nil && r.err == nil {
			r.err = err
		}
	}
}

func (r *codexRun) flush() error {
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
	r.sampleMem()
	return err
}

// sampleMem 记录采集期间的 HeapAlloc 峰值（验收用）。
func (r *codexRun) sampleMem() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > r.heapPeak {
		r.heapPeak = m.HeapAlloc
	}
}

// report 把详细统计打印到 stderr（files/lines/events/bad/未映射类型/内存峰值）。
// 默认静默（双击 exe 场景控制台干净）；HM_DEBUG=1 时输出。
func (r *codexRun) report(stats Stats) {
	if os.Getenv("HM_DEBUG") == "" {
		return
	}
	out := os.Stderr
	fmt.Fprintf(out, "codex: files=%d lines=%d events=%d inserted=%d skipped=%d bad=%d known_skip=%d\n",
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
		fmt.Fprintf(out, "codex: unmapped=%d\n", total)
		for _, k := range keys {
			fmt.Fprintf(out, "codex:   %s x%d\n", k, r.unmapped[k])
		}
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Fprintf(out, "codex: mem heap_peak=%d heap_sys=%d sys=%d total_alloc=%d\n",
		r.heapPeak, m.HeapSys, m.Sys, m.TotalAlloc)
}

// ---- 工具 -----------------------------------------------------------------

func codexSourceKey(rel string, lineNo int) string {
	return fmt.Sprintf("%s:%s:%d", codexHarness, rel, lineNo)
}

// codexFileHasUserMessage 预扫文件，判断是否含 event_msg/item_completed/UserMessage。
// 命中即提前返回；用 bytes.Contains 先粗筛，避免对每行做完整解析。
func codexFileHasUserMessage(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), codexScannerMaxToken)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte("UserMessage")) {
			continue
		}
		var rec codexRecord
		if json.Unmarshal(line, &rec) != nil || rec.Type != "event_msg" {
			continue
		}
		var p codexPayload
		if !codexDecode(rec.Payload, &p) {
			continue
		}
		if p.Type == "item_completed" && p.Item != nil && p.Item.Type == "UserMessage" {
			return true, nil
		}
	}
	return false, sc.Err()
}

// joinBlocks 拼接指定类型的内容块（want 为空表示全部）。
func joinBlocks(blocks []codexBlock, want string) string {
	var sb strings.Builder
	for _, b := range blocks {
		if want == "" || b.Type == want {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// codexIsInjected 判断内容是否为系统注入（AGENTS.md / <INSTRUCTIONS> 等），
// 忽略前导空白后按前缀匹配。
func codexIsInjected(text string) bool {
	t := strings.TrimLeft(text, " 	\r\n")
	for _, p := range codexInjectedPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// codexArgsToString 还原 function_call 的 arguments 原文：通常是 JSON 编码的字符串
// （内层即参数 JSON 文本），若本身不是字符串则原样返回。
func codexArgsToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

// codexOutputContent 把 function_call_output.output 还原为内容原文，并返回其中的图片块数。
//
// output 实测有两种形态：
//   - 字符串：原样返回（最常见的命令输出）；
//   - 块数组：{type: input_text, text} 与 {type: input_image, image_url} 混合。
//     文本块按序拼接为原文；图片块是 base64 data URL（实测 388 块 / 188 MB，单块最大
//     3.35 MB），内联会撑爆事件库且 TUI 无法展示，故以 [image: <mime>, <n> bytes]
//     占位符代替（非文本内容不截断，只是不内联二进制），块数记入 meta.images。
func codexOutputContent(raw json.RawMessage) (string, int) {
	if len(raw) == 0 {
		return "", 0
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, 0
		}
		return string(raw), 0
	}
	if raw[0] == '[' {
		var blocks []codexBlock
		if json.Unmarshal(raw, &blocks) == nil {
			var sb strings.Builder
			images := 0
			for _, b := range blocks {
				if b.Type == "input_image" {
					images++
					mime, n := codexImageInfo(b.ImageURL)
					fmt.Fprintf(&sb, "[image: %s, %d bytes]", mime, n)
					continue
				}
				sb.WriteString(b.Text)
			}
			return sb.String(), images
		}
		return string(raw), 0
	}
	return string(raw), 0
}

// codexImageInfo 从 data URL 解析 mime 与 base64 负载字节数。
func codexImageInfo(u string) (string, int) {
	mime := "image"
	data := u
	if strings.HasPrefix(u, "data:") {
		rest := u[len("data:"):]
		if i := strings.Index(rest, ";"); i >= 0 {
			mime = rest[:i]
			rest = rest[i+1:]
		}
		if i := strings.Index(rest, ","); i >= 0 {
			data = rest[i+1:]
		} else {
			data = rest
		}
	}
	return mime, len(data)
}

// codexCwdTitle 取 cwd 最后一段作为标题（同时兼容 / 与 \ 分隔）。
func codexCwdTitle(cwd string) string {
	s := strings.ReplaceAll(cwd, "\\", "/")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// codexFileUUID 从 rollout-<ISO>-<uuid>.jsonl 文件名解析 uuid（最后 5 个连字符组），
// 仅作 session_meta 缺失时的兜底。
func codexFileUUID(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.TrimPrefix(base, "rollout-")
	parts := strings.Split(base, "-")
	if len(parts) >= 5 {
		return strings.Join(parts[len(parts)-5:], "-")
	}
	return base
}

func codexMeta(m map[string]any) string {
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
		}
		clean[k] = v
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseCodexTime 解析 ISO8601（UTC，Z 结尾，含毫秒）。
func parseCodexTime(s string) time.Time {
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
