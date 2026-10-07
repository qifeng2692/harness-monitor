// Command rundbcheck 验证对比实验台存储层对「已有库」的兼容性：
// 对真实 data/hm.db 执行 store.Open()（内含 CREATE TABLE IF NOT EXISTS 三张新表），
// 确认不报错、既有数据未受影响、新表可读写。
//
// 用法：go run ./cmd/rundbcheck   （HM_DATA_DIR 可覆盖数据目录）
package main

import (
	"fmt"
	"os"

	"github.com/qifeng2692/harness-monitor/internal/store"
)

func main() {
	fmt.Println("数据目录:", store.DataDir())

	s, err := store.Open()
	if err != nil {
		fmt.Println("FAIL: store.Open 报错:", err)
		os.Exit(1)
	}
	defer s.Close()
	fmt.Println("OK: store.Open 成功（已有库执行 IF NOT EXISTS 建表无冲突）")
	fmt.Println("库路径:", s.Path)

	// 既有主监控数据未受影响。
	hss, err := s.Harnesses()
	if err != nil {
		fmt.Println("FAIL: Harnesses 查询失败:", err)
		os.Exit(1)
	}
	fmt.Printf("既有 harness 汇总（events 表）：%d 行\n", len(hss))

	// 新三表可读写：写一个探针批次再读回，最后清理。
	runID, err := s.CreateRun("[rundbcheck] 探针", []string{"hermes"})
	if err != nil {
		fmt.Println("FAIL: CreateRun 失败:", err)
		os.Exit(1)
	}
	if err := s.SetRunHarnessStatus(runID, "hermes", store.RunHarnessDone, ""); err != nil {
		fmt.Println("FAIL: SetRunHarnessStatus 失败:", err)
		os.Exit(1)
	}
	d, err := s.GetRun(runID)
	if err != nil {
		fmt.Println("FAIL: GetRun 失败:", err)
		os.Exit(1)
	}
	fmt.Printf("OK: 新三表可读写 — run#%d prompt=%q harness 行数=%d\n",
		runID, d.Summary.Prompt, len(d.HarnessRuns))

	// 清理探针（保持真实库干净）。
	if _, err := s.DB().Exec(`DELETE FROM run_harness WHERE run_id = ?`, runID); err != nil {
		fmt.Println("FAIL: 清理 run_harness:", err)
		os.Exit(1)
	}
	if _, err := s.DB().Exec(`DELETE FROM runs WHERE id = ?`, runID); err != nil {
		fmt.Println("FAIL: 清理 runs:", err)
		os.Exit(1)
	}
	fmt.Println("OK: 探针已清理")
	fmt.Println("ALL OK")
}
