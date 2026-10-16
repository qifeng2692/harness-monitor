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

	"github.com/qifeng2692/harness-monitor/internal/collect"
	"github.com/qifeng2692/harness-monitor/internal/compare"
	"github.com/qifeng2692/harness-monitor/internal/explain"
	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
	"github.com/qifeng2692/harness-monitor/internal/usage"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App 是 Wails 应用实例（绑定给前端）。
type App struct {
	ctx   context.Context
	store *store.Store
	mu    sync.Mutex // 串行化采集（避免并发写库）

	// svc 是对比实验台的 API 服务（持有 runner.Orchestrator）。
	// startup 时创建；shutdown 时 Close（停在跑批次 + 关 writer）。
	svc *compare.Service
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

	// 对比实验台服务：持有编排器（并发 spawn + 增量写库 + 停止）。
	a.svc = compare.New(s)
	// 每次落库 → 推 run:update 给前端（前端收到后拉增量）。
	a.svc.OnUpdate = func(runID int64) {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "run:update", runID)
		}
	}
}

func (a *App) shutdown(ctx context.Context) {
	if a.svc != nil {
		a.svc.Close() // 停全部在跑批次 + 关 writer goroutine
	}
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
	Pinned     bool   `json:"pinned"`
}

// EventInfo 是时间线步骤（内容全文）。
type EventInfo struct {
	ID       int64  `json:"id"`
	Seq      int64  `json:"seq"`
	Time     string `json:"time"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Meta     string `json:"meta"`
	ParentID string `json:"parentId"`
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
			Pinned:     s.Pinned,
		})
	}
	return out, nil
}

// SetSessionPinned 置顶 / 取消置顶会话（GUI 会话列表的 📌 按钮）。
func (a *App) SetSessionPinned(harness, sessionID string, pinned bool) error {
	if a.store == nil {
		return fmt.Errorf("数据库未打开")
	}
	if !pinned {
		return a.store.SetSessionPinned(harness, sessionID, false)
	}
	// 只允许置顶真实存在的会话（防误传空 ID 制造幽灵置顶）。
	ok, err := a.store.HasSession(harness, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("会话不存在: %s/%s", harness, sessionID)
	}
	return a.store.SetSessionPinned(harness, sessionID, true)
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

// ExplainSession 为会话的每一步生成自然语言说明（与 GetEvents 返回的顺序一一对应）。
// 前端在步骤详情里展示：这一步是什么 / 输入来自哪 / 输出给谁（是否显示给用户）/ 为什么流转。
func (a *App) ExplainSession(harness, sessionID string) ([]explain.StepExplain, error) {
	if a.store == nil {
		return nil, fmt.Errorf("数据库未打开")
	}
	events, err := a.store.ListEvents(harness, sessionID)
	if err != nil {
		return nil, err
	}
	items := make([]explain.Event, 0, len(events))
	for _, e := range events {
		items = append(items, explain.Event{
			Seq: e.Seq, Kind: e.Kind, Title: e.Title, Meta: e.Meta, ParentID: e.ParentID,
		})
	}
	return explain.ExplainAll(items), nil
}

// SessionUsage 合计某会话的用量与成本（token / 费用 / 失败调用 / 分模型明细）。
//
// 各家 harness 的记录口径不同（codex 是会话累计快照、hermes 是「会话×模型」汇总、
// claude 是每会话一条 cost-state、pi/magpie 是逐次调用），合计规则集中在
// internal/usage——本方法只做透传，避免前端各自拍脑袋相加算错。
func (a *App) SessionUsage(harness, sessionID string) (usage.Summary, error) {
	if a.store == nil {
		return usage.Summary{}, fmt.Errorf("数据库未打开")
	}
	events, err := a.store.ListEvents(harness, sessionID)
	if err != nil {
		return usage.Summary{}, err
	}
	return usage.Summarize(harness, events), nil
}

// ---- API：对比实验台（Compare Lab） ----------------------------------------
//
// 规格见 docs/DESIGN-runner.md 第 6 节。真实逻辑在 internal/compare（与 cmd/hmc4check
// 共用同一实现），这里只做绑定透传。实时推送：编排器每次落库 → runtime.EventsEmit(
// a.ctx, "run:update", runID)；前端 window.runtime.EventsOn("run:update", ...) 收到后拉增量。

// StartCompare 启动一次对比运行：创建 run 行 → 并发 spawn 四家 → 立即返回 runID。
//
// 过程经 "run:update" 事件推送（前端收到后调 GetRun/GetRunEvents 拉增量）。
func (a *App) StartCompare(prompt string, harnesses []string) (int64, error) {
	if a.svc == nil {
		return 0, fmt.Errorf("对比实验台未就绪（数据库未打开）")
	}
	return a.svc.StartCompare(prompt, harnesses)
}

// StopRun 停止一个运行中的批次：kill 所有子进程，各家终态记 stopped。
func (a *App) StopRun(runID int64) error {
	if a.svc == nil {
		return fmt.Errorf("对比实验台未就绪（数据库未打开）")
	}
	return a.svc.StopRun(runID)
}

// ListRuns 返回最近批次（倒序，最新在前），每项含各 harness 状态摘要。
func (a *App) ListRuns() ([]compare.RunInfo, error) {
	if a.svc == nil {
		return nil, fmt.Errorf("对比实验台未就绪（数据库未打开）")
	}
	return a.svc.ListRuns()
}

// GetRun 返回批次详情：每 harness 一行（状态/耗时/tokens/cost/final_text/session_id）。
func (a *App) GetRun(runID int64) (compare.RunDetail, error) {
	if a.svc == nil {
		return compare.RunDetail{}, fmt.Errorf("对比实验台未就绪（数据库未打开）")
	}
	return a.svc.GetRun(runID)
}

// GetRunEvents 返回某批次某家 harness 的事件流（与主监控 GetEvents 同结构，
// 前端复用同一套渲染逻辑）。
func (a *App) GetRunEvents(runID int64, harness string) ([]EventInfo, error) {
	if a.svc == nil {
		return nil, fmt.Errorf("对比实验台未就绪（数据库未打开）")
	}
	events, err := a.svc.GetRunEvents(runID, harness)
	if err != nil {
		return nil, err
	}
	return toEventInfos(events), nil
}

// ExplainRunEvents 为某批次某家 harness 的每一步生成自然语言说明（复用 internal/explain，
// 返回顺序与 GetRunEvents 一一对应）。
func (a *App) ExplainRunEvents(runID int64, harness string) ([]explain.StepExplain, error) {
	if a.svc == nil {
		return nil, fmt.Errorf("对比实验台未就绪（数据库未打开）")
	}
	return a.svc.ExplainRunEvents(runID, harness)
}

// ---- 工具 ------------------------------------------------------------------

// toEventInfos 把存储层事件转成前端结构（与 GetEvents 同一形状，前端可复用渲染）。
func toEventInfos(events []model.Event) []EventInfo {
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
	return out
}

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
