// 验证工具：把真实数据源采到临时库，统计 error 事件与失败标记。
// 只读源文件；库写在 scratch 目录，绝不碰仓库 data/。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/qifeng2692/harness-monitor/internal/collect"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

func main() {
	dir := os.Args[1]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	dbPath := filepath.Join(dir, "hm-verify.db")
	_ = os.Remove(dbPath)

	s, err := store.OpenPath(dbPath)
	if err != nil {
		panic(err)
	}
	defer s.Close()

	for _, name := range collect.Names() {
		n, err := collect.CollectIncremental(name, s)
		if err != nil {
			fmt.Printf("%-8s 采集失败: %v\n", name, err)
			continue
		}
		fmt.Printf("%-8s 新增事件 %d\n", name, n)
	}

	fmt.Println()
	fmt.Println("== error 事件（按 harness / title）==")
	rows, err := s.DB().Query(`SELECT harness, title, COUNT(*) FROM events WHERE kind='error'
		GROUP BY harness, title ORDER BY harness, 3 DESC`)
	if err != nil {
		panic(err)
	}
	total := 0
	for rows.Next() {
		var h, t string
		var c int
		_ = rows.Scan(&h, &t, &c)
		fmt.Printf("%-8s %-28s %d\n", h, t, c)
		total += c
	}
	rows.Close()
	fmt.Println("error 事件合计:", total)

	// 每 kind 的数量分布，用来证明「未映射类型被丢弃」而不是「数据本来就没有」。
	kindRows, err := s.DB().Query(`SELECT kind, COUNT(*) FROM events GROUP BY kind ORDER BY 2 DESC`)
	if err != nil {
		panic(err)
	}
	fmt.Println()
	fmt.Println("== 事件 kind 分布 ==")
	for kindRows.Next() {
		var k string
		var c int
		_ = kindRows.Scan(&k, &c)
		fmt.Printf("%-14s %d\n", k, c)
	}
	kindRows.Close()

	fmt.Println()
	fmt.Println("== 失败标记（meta.is_error=true）按 harness / kind ==")
	// 用 LIKE 而不是 json_extract：空 meta（53712 条）上 json_extract 会直接报
	// malformed JSON 让整个查询失败——诊断工具不能比被诊断的数据更脆。
	rows, err = s.DB().Query(`SELECT harness, kind, COUNT(*) FROM events
		WHERE meta LIKE '%"is_error":true%' GROUP BY harness, kind ORDER BY 3 DESC`)
	if err != nil {
		panic(err)
	}
	marked := 0
	for rows.Next() {
		var h, k string
		var c int
		_ = rows.Scan(&h, &k, &c)
		fmt.Printf("%-8s %-14s %d\n", h, k, c)
		marked += c
	}
	rows.Close()
	fmt.Println("失败标记合计:", marked)

	fmt.Println()
	fmt.Println("== 抽样：error 事件的说明文字（前 6 条）==")
	rows, err = s.DB().Query(`SELECT harness, session_id, seq, title, substr(content,1,90), meta
		FROM events WHERE kind='error' ORDER BY harness, session_id, seq LIMIT 6`)
	if err != nil {
		panic(err)
	}
	type ev struct {
		harness, sid, title, content, meta string
		seq                                int64
	}
	var evs []ev
	for rows.Next() {
		var e ev
		_ = rows.Scan(&e.harness, &e.sid, &e.seq, &e.title, &e.content, &e.meta)
		evs = append(evs, e)
	}
	rows.Close()
	for _, e := range evs {
		fmt.Printf("[%s] %s seq=%d title=%q content=%q\n      meta=%s\n", e.harness, e.sid, e.seq, e.title, e.content, e.meta)
	}

	// 说明文字：用 explain 包跑一遍，确认不是笼统文案。
	fmt.Println()
	fmt.Println("== 抽样：失败工具结果的 meta（前 3 条）==")
	rows, err = s.DB().Query(`SELECT harness, session_id, seq, meta FROM events
		WHERE kind='tool.result' AND json_extract(meta,'$.is_error')=1 LIMIT 3`)
	if err != nil {
		panic(err)
	}
	var lines []string
	for rows.Next() {
		var h, sid, m string
		var seq int64
		_ = rows.Scan(&h, &sid, &seq, &m)
		lines = append(lines, fmt.Sprintf("[%s] %s seq=%d %s", h, sid, seq, m))
	}
	rows.Close()
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
}
