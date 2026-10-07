package collect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qifeng2692/harness-monitor/internal/store"
)

// TestCollectIncrementalTail 验证增量续读：首轮补历史，之后只跟新增行（offset 续读），
// 且全量重扫仍幂等。用 magpie 采集器（纯追加文件、格式简单）。
func TestCollectIncrementalTail(t *testing.T) {
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	defer s.Close()

	path := filepath.Join(t.TempDir(), "usage.jsonl")
	t.Setenv("MAGPIE_USAGE_PATH", path)

	line := func(route, ts string) string {
		return `{"t":"` + ts + `","agent":"hermes","model":"m1","status":200,"route_id":"` + route + `"}` + "\n"
	}
	if err := os.WriteFile(path, []byte(line("r1", "2026-10-04T10:00:00Z")+line("r2", "2026-10-04T10:00:01Z")), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 第一轮：从头读，补 2 条。
	n, err := CollectIncremental("magpie", s)
	if err != nil {
		t.Fatalf("第一轮增量: %v", err)
	}
	if n != 2 {
		t.Fatalf("第一轮应插入 2 条，得到 %d", n)
	}

	// 第二轮：无新增 → 0。
	n, err = CollectIncremental("magpie", s)
	if err != nil {
		t.Fatalf("第二轮增量: %v", err)
	}
	if n != 0 {
		t.Fatalf("第二轮无新增应为 0，得到 %d", n)
	}

	// 追加一条新行 → 第三轮应只插入 1 条（offset 续读）。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(line("r3", "2026-10-04T10:00:02Z")); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()
	n, err = CollectIncremental("magpie", s)
	if err != nil {
		t.Fatalf("第三轮增量: %v", err)
	}
	if n != 1 {
		t.Fatalf("第三轮应只插入 1 条新增，得到 %d", n)
	}

	// 水位线已保存。
	states, err := s.IngestStates("magpie")
	if err != nil {
		t.Fatalf("IngestStates: %v", err)
	}
	if pos, ok := states["usage.jsonl"]; !ok || pos.Offset == 0 {
		t.Fatalf("ingest_state 未记录 offset: %+v", states)
	}

	// 全量重扫仍幂等（不产生重复）。
	c := Get("magpie")
	stats, err := c.Collect(s)
	if err != nil {
		t.Fatalf("全量重扫: %v", err)
	}
	if stats.Inserted != 0 {
		t.Fatalf("全量重扫不应新增，得到 inserted=%d", stats.Inserted)
	}
}

// TestCollectIncrementalIncompleteLine 验证末尾半行（写入中）不被消费，
// 待其补全换行后下一轮才被读入（保证 offset 停在换行之后）。
func TestCollectIncrementalIncompleteLine(t *testing.T) {
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	defer s.Close()

	path := filepath.Join(t.TempDir(), "usage.jsonl")
	t.Setenv("MAGPIE_USAGE_PATH", path)

	// 一条完整行 + 一条无换行结尾的半行。
	half := `{"t":"2026-10-04T10:00:05Z","agent":"hermes","model":"m1","status":200,"route_id":"r5"}`
	if err := os.WriteFile(path, []byte(
		`{"t":"2026-10-04T10:00:00Z","agent":"hermes","model":"m1","status":200,"route_id":"r4"}`+"\n"+half), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	n, err := CollectIncremental("magpie", s)
	if err != nil {
		t.Fatalf("增量: %v", err)
	}
	if n != 1 {
		t.Fatalf("半行不应被消费，应只插入 1 条，得到 %d", n)
	}

	// 补上换行后，半行成为完整行 → 下一轮插入 1 条。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()
	n, err = CollectIncremental("magpie", s)
	if err != nil {
		t.Fatalf("第二轮增量: %v", err)
	}
	if n != 1 {
		t.Fatalf("补全换行后应插入 1 条，得到 %d", n)
	}
}
