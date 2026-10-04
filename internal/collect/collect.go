// Package collect 定义采集器接口与注册表。
//
// M1-1 只提供骨架：registry 为空，`hm collect` 对未注册的 harness 打印 "not implemented"。
// 各真实采集器（magpie/hermes/codex/claude/pi）在 M1-2..M1-6 中通过 init() 注册。
package collect

import (
	"fmt"
	"sort"

	"github.com/qf/harness-monitor/internal/model"
)

// Sink 是采集器的写入目标（由 store.Store 满足）。
type Sink interface {
	// InsertEvents 幂等写入事件，返回实际插入数与重复跳过数。
	InsertEvents(events []model.Event) (inserted, skipped int, err error)
}

// TailSink 是 Sink 的扩展：支持读取/保存每个文件的已消费位置（增量续读）。
// live 跟随要求 Sink 满足它；普通全量采集只用 Sink 即可。
type TailSink interface {
	Sink
	// IngestStates 返回 source（= harness 名）下各文件的已消费位置。
	IngestStates(source string) (map[string]model.IngestPos, error)
	// SaveIngestStates 幂等写入一批文件的已消费位置。
	SaveIngestStates(source string, states map[string]model.IngestPos) error
}

// Stats 是一次采集的统计结果。
type Stats struct {
	Harness   string
	Scanned   int  // 扫描的记录/行数
	Inserted  int  // 入库（新增）数
	Skipped   int  // 幂等跳过数
	BadLines  int  // 解析失败、被跳过的行数
	Increment bool // 是否为增量续读（从已存 offset 起读，而非全量重扫）
}

// Collector 是所有采集器的统一接口。
type Collector interface {
	// Name 返回 harness 名（如 "magpie"）。
	Name() string
	// Collect 采集一次，幂等可重复执行。
	Collect(store Sink) (Stats, error)
}

// incrementalCollector 由支持增量续读的采集器实现。
// CollectIncremental 从上次保存的 offset 继续读，返回本次新增事件数与是否真正增量。
type incrementalCollector interface {
	CollectIncremental(store TailSink) (inserted int, incremental bool, err error)
}

// CollectIncremental 在 name 采集器上执行一次增量采集（用于 live 跟随）。
//
// 采集器未实现增量时回退为全量 Collect（幂等，不产生重复事件，只是较慢）；
// sink 不满足 TailSink 时同样回退全量。返回本次新增事件数。
func CollectIncremental(name string, sink Sink) (int, error) {
	c := Get(name)
	if c == nil {
		return 0, fmt.Errorf("未注册的 harness: %s", name)
	}
	ts, ok := sink.(TailSink)
	if !ok {
		stats, err := c.Collect(sink)
		return stats.Inserted, err
	}
	if ic, ok := c.(incrementalCollector); ok {
		inserted, _, err := ic.CollectIncremental(ts)
		return inserted, err
	}
	stats, err := c.Collect(sink)
	return stats.Inserted, err
}

// ---- 增量续读的通用工具 ----------------------------------------------------

// fileStartPos 为增量模式下尚未记录位置的文件计算起始位置。
//
// 语义：文件若已完全入库（store 中已有该文件的会话/事件），从文件末尾起读
// （跳过历史，只跟后续新增行）；文件若是新的（尚未入库），从 0 起读（补历史，
// 保证不丢数据）。判断由采集器提供（如按 session_id / source_key 查库）。
func fileStartPos(size int64, alreadyIngested bool) model.IngestPos {
	if alreadyIngested {
		return model.IngestPos{Offset: size, Size: size}
	}
	return model.IngestPos{Offset: 0, Size: size}
}

var registry = map[string]Collector{}

// Register 注册一个采集器（在各采集器包的 init() 中调用）。
func Register(c Collector) {
	registry[c.Name()] = c
}

// Get 按名取采集器，未注册返回 nil。
func Get(name string) Collector {
	return registry[name]
}

// Names 返回已注册的 harness 名（升序）。
func Names() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// String 便于日志/CLI 展示。
func (s Stats) String() string {
	return fmt.Sprintf("harness=%s scanned=%d inserted=%d skipped=%d bad=%d",
		s.Harness, s.Scanned, s.Inserted, s.Skipped, s.BadLines)
}
