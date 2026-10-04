// magpie 采集器：~/.config/magpie/usage.jsonl → llm.usage 事件。
//
// 数据源格式见 docs/DATA-FORMATS.md §5。每行一条调用记录，逐行解析、坏行跳过计数；
// 每行生成 1 个 llm.usage 事件，content 存该行原始 JSON 全文（一字不改）。
//
// 会话划分：session_id = "magpie/" + agent（agent 是来源标识，如 hermes/OpenAI/pi）。
// 幂等键：source_key = "magpie:" + route_id（route_id 缺失时用行号 + t 兜底）。
package collect

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qf/harness-monitor/internal/model"
)

const magpieHarness = "magpie"

// magpieMetaKeys 是写入事件 meta 的字段集合；仅保留源行中实际存在的键
// （cache_read/reasoning/ttft_ms/route_id 等在部分行缺失）。
var magpieMetaKeys = []string{
	"in", "out", "cache_read", "reasoning", "ms", "ttft_ms",
	"status", "model", "provider", "route_id",
}

type magpieCollector struct{}

func init() { Register(magpieCollector{}) }

func (magpieCollector) Name() string { return magpieHarness }

// MagpieUsagePath 返回 usage.jsonl 路径：环境变量 MAGPIE_USAGE_PATH 优先（测试/覆盖用），
// 否则 ~/.config/magpie/usage.jsonl。
func MagpieUsagePath() (string, error) {
	if p := os.Getenv("MAGPIE_USAGE_PATH"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("解析用户目录: %w", err)
	}
	return filepath.Join(home, ".config", "magpie", "usage.jsonl"), nil
}

// Collect 读取整个 usage.jsonl 并幂等写入事件。
//
// 只读源文件；每次全量扫描（文件仅 800+ 行且持续追加，全量扫描保证不漏），
// 重复执行靠 source_key UNIQUE 去重，不产生重复事件。
func (magpieCollector) Collect(sink Sink) (Stats, error) {
	stats := Stats{Harness: magpieHarness}

	path, err := MagpieUsagePath()
	if err != nil {
		return stats, err
	}
	f, err := os.Open(path)
	if err != nil {
		return stats, fmt.Errorf("打开 %s: %w", path, err)
	}
	defer f.Close()

	var events []model.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // 单行上限 1 MB
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimRight(sc.Text(), "\r") // 仅去行尾 CR/LF，正文一字不改
		if strings.TrimSpace(line) == "" {
			continue // 空行忽略，不计入扫描/坏行
		}
		stats.Scanned++
		ev, ok := magpieEvent(line, lineNo)
		if !ok {
			stats.BadLines++
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return stats, fmt.Errorf("读取 %s: %w", path, err)
	}

	inserted, skipped, err := sink.InsertEvents(events)
	stats.Inserted, stats.Skipped = inserted, skipped
	return stats, err
}

// magpieEvent 把一行 usage JSON 映射为一个 llm.usage 事件；解析失败返回 false。
func magpieEvent(line string, lineNo int) (model.Event, bool) {
	// raw 用于按需保留 meta 字段与 route_id（保留原始数值形态）。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return model.Event{}, false
	}

	var rec struct {
		T      string `json:"t"`
		Agent  string `json:"agent"`
		Model  string `json:"model"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return model.Event{}, false
	}

	agent := rec.Agent
	if agent == "" {
		agent = "unknown"
	}

	meta := make(map[string]json.RawMessage, len(magpieMetaKeys))
	for _, k := range magpieMetaKeys {
		if v, ok := raw[k]; ok {
			meta[k] = v
		}
	}
	metaJSON, err := json.Marshal(meta) // map key 排序，输出稳定
	if err != nil {
		return model.Event{}, false
	}

	return model.Event{
		Harness:   magpieHarness,
		SessionID: magpieHarness + "/" + agent,
		Time:      parseMagpieTime(rec.T),
		Kind:      model.KindLLMUsage,
		Title:     rec.Model + " " + strconv.Itoa(rec.Status),
		Content:   line,
		Meta:      string(metaJSON),
		SourceKey: magpieSourceKey(raw, lineNo, rec.T),
	}, true
}

// magpieSourceKey 生成幂等键：优先 route_id，缺失/为 null 时用 "行号:t" 兜底。
func magpieSourceKey(raw map[string]json.RawMessage, lineNo int, t string) string {
	if v, ok := raw["route_id"]; ok {
		s := strings.TrimSpace(string(v))
		if s != "" && s != "null" {
			if unq, err := strconv.Unquote(s); err == nil { // 兼容字符串形态
				s = unq
			}
			return magpieHarness + ":" + s
		}
	}
	return fmt.Sprintf("%s:%d:%s", magpieHarness, lineNo, t)
}

// parseMagpieTime 解析 ISO8601（带 +08:00 偏移、可含 7 位小数秒）。
func parseMagpieTime(s string) time.Time {
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
