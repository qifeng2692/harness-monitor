// app.go 是 Wails 绑定层：暴露给前端 JS 的 API。
//
// 设计：数据层（internal/collect + internal/store）完全复用 M1 已验证的采集器；
// 本层只做「查询结果 → 前端友好结构」的转换与动作编排。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qf/harness-monitor/internal/collect"
	"github.com/qf/harness-monitor/internal/model"
	"github.com/qf/harness-monitor/internal/store"
)

// App 是 Wails 应用实例（绑定给前端）。
type App struct {
	ctx   context.Context
	store *store.Store
	mu    sync.Mutex // 串行化采集（避免并发写库）
}

// version 由构建期注入：go build -ldflags "-X main.version=..."。
var version = "dev"

// NewApp 构造 App（不打开库——库在 startup 打开，避免 main 里做 IO）。
func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	s, err := store.Open()
	if err != nil {
		// 打开失败不阻塞启动：API 层会返回错误给前端展示。
		fmt.Println("打开数据库失败:", err)
		return
	}
	a.store = s
}

func (a *App) shutdown(ctx context.Context) {
	if a.store != nil {
		_ = a.store.Close()
	}
}

// ---- 数据结构（前端友好） ---------------------------------------------------

// HarnessInfo 是左侧导航的 harness 汇总。
type HarnessInfo struct {
	Name         string `json:"name"`
	SessionCount int64  `json:"sessionCount"`
	EventCount   int64  `json:"eventCount"`
	LastTime     string `json:"lastTime"`
}

// SessionInfo 是会话列表项。
type SessionInfo struct {
	SessionID  string `json:"sessionId"`
	EventCount int64  `json:"eventCount"`
	FirstTime  string `json:"firstTime"`
	LastTime   string `json:"lastTime"`
	Title      string `json:"title"`
}

// EventInfo 是时间线步骤（内容全文）。
type EventInfo struct {
	ID        int64  `json:"id"`
	Seq       int64  `json:"seq"`
	Time      string `json:"time"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Meta      string `json:"meta"`
	ParentID  string `json:"parentId"`
}

// SearchHitInfo 是搜索命中。
type SearchHitInfo struct {
	Harness   string `json:"harness"`
	SessionID string `json:"sessionId"`
	Seq       int64  `json:"seq"`
	Time      string `json:"time"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Content   string `json:"content"`
}

// CollectResult 是一次采集的结果汇总。
type CollectResult struct {
	Total   int               `json:"total"`
	Details map[string]string `json:"details"` // harness -> "+N" 或 "失败(...)"
}

// ---- API：查询 -------------------------------------------------------------

// ListHarnesses 返回全部 harness 的汇总（含前端未采集过的 harness 占位）。
func (a *App) ListHarnesses() ([]HarnessInfo, error) {
	if a.store == nil {
		return nil, fmt.Errorf("数据库未打开")
	}
	sum, err := a.store.Harnesses()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []HarnessInfo{}
	for _, h := range sum {
		out = append(out, HarnessInfo{
			Name:         h.Harness,
			SessionCount: h.SessionCount,
			EventCount:   h.EventCount,
			LastTime:     fmtTime(h.LastTime),
		})
		seen[h.Harness] = true
	}
	// 已注册但库里还没有数据的 harness 也列出来（灰色显示）。
	for _, name := range collect.Names() {
		if !seen[name] {
			out = append(out, HarnessInfo{Name: name, SessionCount: 0, EventCount: 0, LastTime: ""})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListSessions 返回某 harness 的会话列表。
func (a *App) ListSessions(harness string) ([]SessionInfo, error) {
	if a.store == nil {
		return nil, fmt.Errorf("数据库未打开")
	}
	sessions, err := a.store.ListSessions(harness)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, SessionInfo{
			SessionID:  s.SessionID,
			EventCount: s.EventCount,
			FirstTime:  fmtTime(s.FirstTime),
			LastTime:   fmtTime(s.LastTime),
			Title:      s.Title,
		})
	}
	return out, nil
}

// GetEvents 返回某会话的全部事件（按 seq 升序；含内容全文）。
func (a *App) GetEvents(harness, sessionID string) ([]EventInfo, error) {
	if a.store == nil {
		return nil, fmt.Errorf("数据库未打开")
	}
	events, err := a.store.ListEvents(harness, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]EventInfo, 0, len(events))
	for _, e := range events {
		out = append(out, EventInfo{
			ID:       e.ID,
			Seq:      e.Seq,
			Time:     fmtTime(e.Time),
			Kind:     e.Kind,
			Title:    e.Title,
			Content:  e.Content,
			Meta:     e.Meta,
			ParentID: e.ParentID,
		})
	}
	return out, nil
}

// Search 全库（或限定 harness）搜索事件内容。
func (a *App) Search(harness, query string) ([]SearchHitInfo, error) {
	if a.store == nil {
		return nil, fmt.Errorf("数据库未打开")
	}
	hits, err := a.store.SearchEvents(harness, query, 500)
	if err != nil {
		return nil, err
	}
	out := make([]SearchHitInfo, 0, len(hits))
	for _, h := range hits {
		out = append(out, SearchHitInfo{
			Harness:   h.Harness,
			SessionID: h.SessionID,
			Seq:       h.Seq,
			Time:      fmtTime(h.Time),
			Kind:      h.Kind,
			Title:     h.Title,
			Content:   h.Content,
		})
	}
	return out, nil
}

// ---- API：动作 -------------------------------------------------------------

// Collect 增量采集（harness 为空 = 全部）。
func (a *App) Collect(harness string) (CollectResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	res := CollectResult{Details: map[string]string{}}
	if a.store == nil {
		return res, fmt.Errorf("数据库未打开")
	}
	names := collect.Names()
	if harness != "" {
		if collect.Get(harness) == nil {
			return res, fmt.Errorf("未注册的 harness: %s", harness)
		}
		names = []string{harness}
	}
	for _, name := range names {
		n, err := collect.CollectIncremental(name, a.store)
		if err != nil {
			res.Details[name] = "失败: " + err.Error()
			continue
		}
		res.Details[name] = fmt.Sprintf("+%d", n)
		res.Total += n
	}
	return res, nil
}

// OpenInExplorer 在资源管理器中打开数据目录（关于页/工具栏用）。
func (a *App) OpenInExplorer() error {
	if a.store == nil {
		return fmt.Errorf("数据库未打开")
	}
	dir := store.DataDir()
	return openExplorer(dir)
}

// Version 返回版本号。
func (a *App) Version() string { return version }

// GetEventMeta 把事件的 meta JSON 解析为 map（前端表格展示用；解析失败原样返回）。
func (a *App) GetEventMeta(meta string) map[string]any {
	out := map[string]any{}
	if meta == "" {
		return out
	}
	if err := json.Unmarshal([]byte(meta), &out); err != nil {
		out["raw"] = meta
	}
	return out
}

// ---- 工具 ------------------------------------------------------------------

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// trimContent 仅用于可能超长的摘要展示（列表页）；详情页始终给全文。
func trimContent(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

var _ = model.Event{} // 保留引用（模型常量在采集器中已用）
