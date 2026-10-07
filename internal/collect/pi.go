// pi 采集器：~/.pi/agent/sessions/**/*.jsonl（v3 会话 JSONL）→ 内容级事件。
//
// 数据源格式见 docs/DATA-FORMATS.md §4。实测 205 个文件 / 292 MB，单文件最大
// 95 MB，单行最长 2.98 MB——必须流式按行解析（bufio.Scanner + 32 MB 上限），
// 禁止一次性 ReadAll；事件按条数 + 字节双上限分批入库，控制内存（RSS < 500 MB）。
//
// 事件映射（一一对应 docs/DESIGN.md 的 kind 规范）：
//
//	首行 {type:session,version,id,timestamp,cwd}    → session.start（session_id=id；meta: cwd/version）
//	{type:model_change}                             → session.meta（title="model change: "+modelId；meta: provider/modelId）
//	message role=user（content[{type:text,text}]）  → turn.start（拼接 text 全文）
//	message role=assistant（content 数组）：
//	  {type:thinking,thinking}                      → llm.reasoning
//	  {type:text,text}                              → llm.response
//	  {type:toolCall,id,name,arguments}             → tool.call（content=arguments 对象序列化；meta: tool_name/call_id）
//	message role=toolResult（{toolCallId,toolName,content[{type:text,text}]}）→ tool.result
//	  （content=拼接 text 全文；meta: tool_name/call_id；parent_id=toolCallId）
//	{type:compaction,summary}                       → compaction（title="context compaction"；content=summary）
//	其余顶层 type（thinking_level_change/custom_message/custom/session_info/context_edit） → 不映射，计数
//
// 幂等键：source_key = "pi:" + 文件相对路径 + ":" + 行号；一行若产出多个事件
// （assistant content 含多个块）则在末尾追加 ":<块序号>" 以保唯一。
//
// 会话划分：session_id = 首行 session.id（同一会话被拆分/复制到多个文件时汇入同一条
// 时间线）。首行无 session 记录的文件（实测 9 个 observation-pack/ledger.jsonl，非 pi
// 会话，其行均无 type 字段）不产出事件，自然不进入会话列表；兜底用文件名 uuid。
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
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

const piHarness = "pi"

const (
	// piScannerMaxToken 是单行上限；实测最长 2.98 MB，留 10x 余量。
	piScannerMaxToken = 32 << 20
	// 分批入库双上限：任一到阈值即 flush，限制单次内存占用。
	piBatchMaxEvents = 2000
	piBatchMaxBytes  = 32 << 20
)

type piCollector struct{}

func init() { Register(piCollector{}) }

func (piCollector) Name() string { return piHarness }

// PiSessionsDir 返回 sessions 根目录：PI_SESSIONS_DIR 优先（测试/覆盖用），
// 否则 ~/.pi/agent/sessions。
func PiSessionsDir() (string, error) {
	if d := os.Getenv("PI_SESSIONS_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("解析用户目录: %w", err)
	}
	return filepath.Join(home, ".pi", "agent", "sessions"), nil
}

// Collect 流式扫描全部会话 JSONL 并幂等写入事件。
func (piCollector) Collect(sink Sink) (Stats, error) {
	return piCollect(sink, nil)
}

// CollectIncremental 实现增量续读：从上次保存的 offset 起读各文件新增行。
//
// 首次运行（无 ingest_state）时，对已入库的历史文件从末尾起跟、对新文件从头补。
func (piCollector) CollectIncremental(sink TailSink) (int, bool, error) {
	states, err := sink.IngestStates(piHarness)
	if err != nil {
		return 0, false, err
	}
	stats, err := piCollect(sink, states)
	return stats.Inserted, true, err
}

// piCollect 是 Collect 与 CollectIncremental 的共用实现。
// states 为 nil 时全量扫描；非 nil 时按各文件已存 offset 增量续读。
func piCollect(sink Sink, states map[string]model.IngestPos) (Stats, error) {
	stats := Stats{Harness: piHarness}

	root, err := PiSessionsDir()
	if err != nil {
		return stats, err
	}
	if fi, err := os.Stat(root); err != nil {
		return stats, fmt.Errorf("sessions 目录不可用 %s: %w", root, err)
	} else if !fi.IsDir() {
		return stats, fmt.Errorf("sessions 路径不是目录: %s", root)
	}
	files, err := piFiles(root)
	if err != nil {
		return stats, err
	}

	r := &piRun{sink: sink, root: root, unmapped: map[string]int{}}
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

// piFiles 递归收集 root 下的 *.jsonl，按路径升序（顺序稳定 → seq 稳定）。
func piFiles(root string) ([]string, error) {
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

type piRun struct {
	sink                                 Sink
	root                                 string
	batch                                []model.Event
	bytes                                int
	inserted, skipped, bad, lines, files int
	skippedKnown                         int            // 已识别但无内容/按规范不出事件
	unmapped                             map[string]int // 未映射顶层 type / 块类型 / role → 次数
	heapPeak                             uint64         // 采集期间 HeapAlloc 峰值
	err                                  error
	nextPos                              map[string]model.IngestPos // 增量模式下各文件的新位置
}

func (r *piRun) collectFile(p string, states map[string]model.IngestPos) error {
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
	start, _ := resolveStart(rel, states, fi.Size())
	pos := start

	// session_id 来自文件首行 session.id，实测等于文件名 uuid（piFileSessionID）。
	// 续读（offset>0）时首行已消费，直接用文件名派生，避免每轮重开文件读首行。
	fallback := piFileSessionID(rel)
	sid := fallback

	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", rel, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), piScannerMaxToken)
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

		var rec piRecord
		if json.Unmarshal(line, &rec) != nil {
			r.bad++
			continue
		}
		if rec.Type == "session" {
			if rec.ID != "" {
				sid = rec.ID
			}
			cur := sid
			if cur == "" {
				cur = fallback
			}
			r.emitSessionStart(rec, rel, lineNo, cur)
			continue
		}
		cur := sid
		if cur == "" {
			cur = fallback
		}
		r.mapRecord(rec, rel, lineNo, cur)
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
func (r *piRun) saveStates(sink Sink, states map[string]model.IngestPos) error {
	if states == nil || len(r.nextPos) == 0 {
		return nil
	}
	ts, ok := sink.(TailSink)
	if !ok {
		return nil
	}
	return ts.SaveIngestStates(piHarness, r.nextPos)
}

// ---- 记录解析结构 ---------------------------------------------------------

type piRecord struct {
	Type      string     `json:"type"`
	Version   int        `json:"version"`
	ID        string     `json:"id"`
	ParentID  string     `json:"parentId"`
	Timestamp string     `json:"timestamp"`
	Cwd       string     `json:"cwd"`
	Provider  string     `json:"provider"`
	ModelID   string     `json:"modelId"`
	Summary   string     `json:"summary"`
	Message   *piMessage `json:"message"`
}

type piMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // 字符串（system）或块数组
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
}

type piBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // 已解析对象的原文（对象，非字符串）
	Data      string          `json:"data"`      // image：base64
	MimeType  string          `json:"mimeType"`  // image
}

// ---- 映射 -----------------------------------------------------------------

// emitSessionStart 处理 type=="session" 的首行 → session.start。
func (r *piRun) emitSessionStart(rec piRecord, rel string, lineNo int, sid string) {
	meta := map[string]any{"cwd": rec.Cwd}
	if rec.Version != 0 {
		meta["version"] = rec.Version
	}
	r.emit(model.Event{
		Harness:   piHarness,
		SessionID: sid,
		Time:      parsePiTime(rec.Timestamp),
		Kind:      model.KindSessionStart,
		Title:     piCwdTitle(rec.Cwd),
		Meta:      piMeta(meta),
		SourceKey: piSourceKey(rel, lineNo),
	})
}

// mapRecord 把一行映射为 0..N 个事件并赋幂等键后入库。
func (r *piRun) mapRecord(rec piRecord, rel string, lineNo int, sid string) {
	when := parsePiTime(rec.Timestamp)

	var events []model.Event
	switch rec.Type {
	case "model_change":
		events = []model.Event{{
			Harness: piHarness, SessionID: sid, Time: when,
			Kind: model.KindSessionMeta, Title: "model change: " + rec.ModelID,
			Meta: piMeta(map[string]any{"provider": rec.Provider, "modelId": rec.ModelID}),
		}}
	case "message":
		events = r.messageEvents(rec, sid, when)
	case "compaction":
		events = []model.Event{{
			Harness: piHarness, SessionID: sid, Time: when,
			Kind: model.KindCompaction, Title: "context compaction", Content: rec.Summary,
		}}
	default:
		r.unmapped[rec.Type]++
		return
	}

	if len(events) == 0 {
		r.skippedKnown++ // 该行可识别但无内容（如空 text / system 角色）
		return
	}
	base := piSourceKey(rel, lineNo)
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

// messageEvents 处理 type=="message" 的行（按 message.role 分派）。
func (r *piRun) messageEvents(rec piRecord, sid string, when time.Time) []model.Event {
	if rec.Message == nil {
		return nil
	}
	switch rec.Message.Role {
	case "user":
		text := piContentText(rec.Message.Content)
		if text == "" {
			return nil
		}
		return []model.Event{{
			Harness: piHarness, SessionID: sid, Time: when,
			Kind: model.KindTurnStart, Title: "user", Content: text,
		}}
	case "assistant":
		return r.assistantEvents(rec, sid, when)
	case "toolResult":
		return []model.Event{{
			Harness: piHarness, SessionID: sid, Time: when,
			Kind: model.KindToolResult, Title: "tool result",
			Content: piContentText(rec.Message.Content),
			Meta: piMeta(map[string]any{
				"tool_name": rec.Message.ToolName,
				"call_id":   rec.Message.ToolCallID,
			}),
			ParentID: rec.Message.ToolCallID,
		}}
	default:
		r.unmapped["message/"+rec.Message.Role]++
		return nil
	}
}

// assistantEvents 处理 role=="assistant" 的行（content 数组）：
//
//	{thinking}  → llm.reasoning
//	{text}      → llm.response
//	{toolCall}  → tool.call（content=arguments 对象序列化）
func (r *piRun) assistantEvents(rec piRecord, sid string, when time.Time) []model.Event {
	trimmed := bytes.TrimSpace(rec.Message.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	// 防御：assistant content 实测恒为数组，仍兼容字符串形态。
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) != nil || s == "" {
			return nil
		}
		return []model.Event{{
			Harness: piHarness, SessionID: sid, Time: when,
			Kind: model.KindLLMResponse, Title: "assistant", Content: s,
		}}
	}
	if trimmed[0] != '[' {
		return nil
	}

	var blocks []piBlock
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
				Harness: piHarness, SessionID: sid, Time: when,
				Kind: model.KindLLMReasoning, Title: "reasoning", Content: b.Thinking,
			})
		case "text":
			if b.Text == "" {
				continue
			}
			out = append(out, model.Event{
				Harness: piHarness, SessionID: sid, Time: when,
				Kind: model.KindLLMResponse, Title: "assistant", Content: b.Text,
			})
		case "toolCall":
			out = append(out, model.Event{
				Harness: piHarness, SessionID: sid, Time: when,
				Kind: model.KindToolCall, Title: b.Name, Content: piArgsJSON(b.Arguments),
				Meta: piMeta(map[string]any{"tool_name": b.Name, "call_id": b.ID}),
			})
		default:
			r.unmapped["assistant/"+b.Type]++
		}
	}
	return out
}

// ---- 入库（双上限分批） ----------------------------------------------------

func (r *piRun) emit(ev model.Event) {
	r.batch = append(r.batch, ev)
	r.bytes += len(ev.Content) + len(ev.Meta) + len(ev.Title) + len(ev.SourceKey) + 64
	if len(r.batch) >= piBatchMaxEvents || r.bytes >= piBatchMaxBytes {
		if err := r.flush(); err != nil && r.err == nil {
			r.err = err
		}
	}
}

func (r *piRun) flush() error {
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
func (r *piRun) sampleMem() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > r.heapPeak {
		r.heapPeak = m.HeapAlloc
	}
}

// report 把详细统计打印到 stderr（files/lines/events/bad/未映射类型/内存峰值）。
// 默认静默（双击 exe 场景控制台干净）；HM_DEBUG=1 时输出。
func (r *piRun) report() {
	if os.Getenv("HM_DEBUG") == "" {
		return
	}
	out := os.Stderr
	fmt.Fprintf(out, "pi: files=%d lines=%d events=%d inserted=%d skipped=%d bad=%d known_skip=%d\n",
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
		fmt.Fprintf(out, "pi: unmapped=%d\n", total)
		for _, k := range keys {
			label := k
			if label == "" {
				label = "<no-type>"
			}
			fmt.Fprintf(out, "pi:   %s x%d\n", label, r.unmapped[k])
		}
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Fprintf(out, "pi: mem heap_peak=%d heap_sys=%d sys=%d total_alloc=%d\n",
		r.heapPeak, m.HeapSys, m.Sys, m.TotalAlloc)
}

// ---- 工具 -----------------------------------------------------------------

func piSourceKey(rel string, lineNo int) string {
	return fmt.Sprintf("%s:%s:%d", piHarness, rel, lineNo)
}

// piFileSessionID 从文件名解析兜底会话 ID：<ISO>_<uuid>.jsonl → "<uuid>"。
// 仅在文件缺 session 记录时使用（实测不触发，纯防御）。
func piFileSessionID(rel string) string {
	base := strings.TrimSuffix(rel, path.Ext(rel))
	if i := strings.LastIndex(base, "_"); i >= 0 {
		return base[i+1:]
	}
	return path.Base(base)
}

// piContentText 拼接内容块为全文：text 块按序拼接；image 块以
// [image: <mime>, <n> bytes] 占位（base64 不内联，避免撑爆事件库）；
// 字符串形态直接返回。
func piContentText(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
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
		var blocks []piBlock
		if json.Unmarshal(t, &blocks) == nil {
			var sb strings.Builder
			for _, b := range blocks {
				switch b.Type {
				case "text":
					sb.WriteString(b.Text)
				case "image":
					fmt.Fprintf(&sb, "[image: %s, %d bytes]", piMime(b.MimeType), len(b.Data))
				}
			}
			return sb.String()
		}
	}
	return string(t)
}

// piArgsJSON 返回 toolCall.arguments 对象的序列化文本。
//
// 采用源文件该值的原始字节（json.RawMessage）而非 json.Marshal(解析对象)：
//   - Go 默认 JSON 会做 HTML 转义（> < & → \u003e \u003c \u0026）；
//   - json.Marshal 对 map 会按键名排序，丢失原顺序。
//
// RawMessage 即源对象原文，最忠实，且与 claude/codex/hermes 的 arguments 原文口径一致。
// 实测源为紧凑 JSON（无空格），故等价于对象的一次紧凑序列化。
func piArgsJSON(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	return string(t)
}

// piMime 归一化图片 mime（缺失时用 "image"）。
func piMime(m string) string {
	if m == "" {
		return "image"
	}
	return m
}

// piCwdTitle 取 cwd 最后一段作为标题（同时兼容 / 与 \ 分隔）。
func piCwdTitle(cwd string) string {
	s := strings.ReplaceAll(cwd, "\\", "/")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "pi session"
	}
	return s
}

// piMeta 把 meta map 序列化为 JSON，跳过空字符串与零值。
func piMeta(m map[string]any) string {
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

// parsePiTime 解析 ISO8601（UTC，Z 结尾，含毫秒）。
func parsePiTime(s string) time.Time {
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
