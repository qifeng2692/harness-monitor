// Package compare 是对比实验台的 API 层：把 store（runs / run_harness / run_events 三表）
// 与 runner.Orchestrator（并发运行编排）组合成前端可直接调用的服务。
//
// 规格见 docs/DESIGN-runner.md 第 6 节。
//
// 为什么独立成包而不是直接写在 gui/app.go：Wails 绑定层（package main）无法被
// cmd/ 下的程序 import，而验收要求「无 GUI 跑通同一套 API」。因此真正的逻辑在这里，
// gui/app.go 只是把它逐个透传给前端 JS，cmd/hmc4check 直接驱动它——
// 两者跑的是同一段代码，验收结论对 GUI 有效。
package compare

import (
	"fmt"
	"strings"
	"time"

	"github.com/qf/harness-monitor/internal/explain"
	"github.com/qf/harness-monitor/internal/model"
	"github.com/qf/harness-monitor/internal/runner"
	"github.com/qf/harness-monitor/internal/store"
)

// listLimit 是 ListRuns 返回的批次数上限（最近 N 个）。
const listLimit = 50

// RunHarnessInfo 是批次内某一家 harness 的运行行（前端列头 / 列尾用）。
type RunHarnessInfo struct {
	Harness    string  `json:"harness"`
	Status     string  `json:"status"` // pending|running|done|failed|timeout|stopped
	StartedAt  string  `json:"startedAt"`
	EndedAt    string  `json:"endedAt"`
	DurationMs int64   `json:"durationMs"`
	TokensIn   int64   `json:"tokensIn"`
	TokensOut  int64   `json:"tokensOut"`
	CostUSD    float64 `json:"costUsd"`
	FinalText  string  `json:"finalText"`
	Error      string  `json:"error"`
	SessionID  string  `json:"sessionId"`
}

// RunInfo 是一个对比批次（前端历史列表项 + 并列列区的数据源）。
//
// Active 表示编排器此刻仍在跑这个批次（前端据此启用「停止」按钮）。
type RunInfo struct {
	ID        int64            `json:"id"`
	Prompt    string           `json:"prompt"`
	Harnesses []string         `json:"harnesses"` // 创建时的顺序
	CreatedAt string           `json:"createdAt"`
	Status    string           `json:"status"` // running | done
	Active    bool             `json:"active"`
	Runners   []RunHarnessInfo `json:"runners"`
}

// RunDetail 是批次详情（GetRun）。与 RunInfo 同构——列表项已带各 harness 摘要，
// 详情只是同一份数据的单批次视图，前端两处复用同一个渲染函数。
type RunDetail = RunInfo

// Service 是对比实验台的 API 实现。
//
// 零值不可用——用 New 构造（New 会启动编排器的 writer goroutine）；用完调 Close。
type Service struct {
	store *store.Store
	orch  *runner.Orchestrator

	// OnUpdate 在批次有新的落库（事件/状态/结果）时回调，参数是受影响的 runID。
	// GUI 用它 runtime.EventsEmit(ctx, "run:update", runID) 推给前端。可为 nil。
	OnUpdate func(runID int64)
}

// New 构造服务并启动编排器。st 必须非 nil。
func New(st *store.Store) *Service {
	s := &Service{store: st, orch: runner.New(st)}
	s.orch.OnUpdate = func(runID int64) {
		if s.OnUpdate != nil {
			s.OnUpdate(runID)
		}
	}
	return s
}

// Close 停止全部在跑批次并释放编排器（幂等）。
func (s *Service) Close() {
	if s == nil || s.orch == nil {
		return
	}
	s.orch.Close()
}

// ---------------------------------------------------------------------------
// API：动作
// ---------------------------------------------------------------------------

// StartCompare 启动一次对比运行：创建 run 行（含各家 pending 行）→ 并发 spawn →
// 立即返回 runID。过程经 OnUpdate 推送，前端收到后调 GetRun 拉增量。
func (s *Service) StartCompare(prompt string, harnesses []string) (int64, error) {
	if s == nil || s.store == nil {
		return 0, fmt.Errorf("数据库未打开")
	}
	if s.orch == nil {
		return 0, fmt.Errorf("编排器未就绪")
	}
	if strings.TrimSpace(prompt) == "" {
		return 0, fmt.Errorf("prompt 不能为空")
	}
	hs, err := normalizeHarnesses(harnesses)
	if err != nil {
		return 0, err
	}

	runID, err := s.store.CreateRun(prompt, hs)
	if err != nil {
		return 0, fmt.Errorf("创建批次失败: %w", err)
	}
	if err := s.orch.Start(runID, prompt, hs); err != nil {
		// 启动失败不能留下永远 running 的僵尸批次：收尾成 done，前端看得到失败原因。
		_ = s.store.FinishRun(runID)
		return runID, fmt.Errorf("启动失败: %w", err)
	}
	return runID, nil
}

// StopRun 停止一个运行中的批次（kill 所有子进程，各家终态记 stopped）。
func (s *Service) StopRun(runID int64) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("数据库未打开")
	}
	if s.orch == nil {
		return fmt.Errorf("编排器未就绪")
	}
	return s.orch.Stop(runID)
}

// ---------------------------------------------------------------------------
// API：查询
// ---------------------------------------------------------------------------

// ListRuns 返回最近 listLimit 个批次的摘要（倒序，最新在前），每项含各 harness
// 的状态摘要。
func (s *Service) ListRuns() ([]RunInfo, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("数据库未打开")
	}
	sums, err := s.store.ListRuns(listLimit)
	if err != nil {
		return nil, err
	}
	out := make([]RunInfo, 0, len(sums))
	for _, sum := range sums {
		d, err := s.store.GetRun(sum.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, s.toRunInfo(d))
	}
	return out, nil
}

// GetRun 返回批次详情：批次摘要 + 每 harness 运行行（状态/耗时/tokens/cost/
// final_text/session_id）。
func (s *Service) GetRun(runID int64) (RunDetail, error) {
	if s == nil || s.store == nil {
		return RunDetail{}, fmt.Errorf("数据库未打开")
	}
	d, err := s.store.GetRun(runID)
	if err != nil {
		return RunDetail{}, err
	}
	return s.toRunInfo(d), nil
}

// GetRunEvents 返回某批次某家 harness 的事件流（按 seq 升序，内容全文）。
// 结构与主监控 GetEvents 一致（同为 model.Event），前端复用同一套渲染逻辑。
func (s *Service) GetRunEvents(runID int64, harness string) ([]model.Event, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("数据库未打开")
	}
	if !isHarness(harness) {
		return nil, fmt.Errorf("未知 harness: %s", harness)
	}
	return s.store.ListRunEvents(runID, harness)
}

// ExplainRunEvents 为某批次某家 harness 的每一步生成自然语言说明（复用 internal/explain，
// 与主监控的 ExplainSession 同一实现；返回顺序与 GetRunEvents 一一对应）。
func (s *Service) ExplainRunEvents(runID int64, harness string) ([]explain.StepExplain, error) {
	events, err := s.GetRunEvents(runID, harness)
	if err != nil {
		return nil, err
	}
	items := make([]explain.Event, 0, len(events))
	for _, e := range events {
		items = append(items, explain.Event{Seq: e.Seq, Kind: e.Kind, Title: e.Title, Meta: e.Meta})
	}
	return explain.ExplainAll(items), nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// toRunInfo 把存储层的批次详情转成前端结构。
func (s *Service) toRunInfo(d store.RunDetail) RunInfo {
	ri := RunInfo{
		ID:        d.Summary.ID,
		Prompt:    d.Summary.Prompt,
		Harnesses: d.Summary.Harnesses,
		CreatedAt: fmtTime(d.Summary.CreatedAt),
		Status:    d.Summary.Status,
		Active:    s.orch != nil && s.orch.IsActive(d.Summary.ID),
		Runners:   make([]RunHarnessInfo, 0, len(d.HarnessRuns)),
	}
	if ri.Harnesses == nil {
		ri.Harnesses = []string{}
	}
	for _, h := range d.HarnessRuns {
		ri.Runners = append(ri.Runners, RunHarnessInfo{
			Harness:    h.Harness,
			Status:     h.Status,
			StartedAt:  fmtTime(h.StartedAt),
			EndedAt:    fmtTime(h.EndedAt),
			DurationMs: h.DurationMs,
			TokensIn:   h.TokensIn,
			TokensOut:  h.TokensOut,
			CostUSD:    h.CostUSD,
			FinalText:  h.FinalText,
			Error:      h.Error,
			SessionID:  h.SessionID,
		})
	}
	return ri
}

// normalizeHarnesses 校验并规范化 harness 列表：非空、全部是四家之一、按首次出现去重。
func normalizeHarnesses(harnesses []string) ([]string, error) {
	if len(harnesses) == 0 {
		return nil, fmt.Errorf("请至少勾选一家 harness")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(harnesses))
	for _, h := range harnesses {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if !isHarness(h) {
			return nil, fmt.Errorf("未知 harness: %q（可选：%s）", h, strings.Join(runner.All, " / "))
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("请至少勾选一家 harness")
	}
	return out, nil
}

// isHarness 报告 name 是否为四家之一。
func isHarness(name string) bool {
	for _, h := range runner.All {
		if h == name {
			return true
		}
	}
	return false
}

// fmtTime 把时间转成前端展示用字符串（本地时区；零值返回空串）。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
