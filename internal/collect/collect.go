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

// Stats 是一次采集的统计结果。
type Stats struct {
	Harness  string
	Scanned  int // 扫描的记录/行数
	Inserted int // 入库（新增）数
	Skipped  int // 幂等跳过数
	BadLines int // 解析失败、被跳过的行数
}

// Collector 是所有采集器的统一接口。
type Collector interface {
	// Name 返回 harness 名（如 "magpie"）。
	Name() string
	// Collect 采集一次，幂等可重复执行。
	Collect(store Sink) (Stats, error)
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
