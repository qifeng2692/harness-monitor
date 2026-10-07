// 运行编排器：一次对比批次的完整生命周期管理（并发 spawn / 增量写库 / 停止）。
//
// 规格见 docs/DESIGN-runner.md 第 5/6 节：
//
//   - Start 为每家 harness 起一个 goroutine 跑 Runner（默认 Spawn）：spawn → 逐行解析
//     → 事件经单一 channel 交给唯一 writer goroutine 串行落库（SQLite 单写者），
//     每写完一批通知 OnUpdate(runID) 让 GUI 拉增量；
//   - 状态机 pending → running → done|failed|timeout|stopped；
//   - Stop 取消批次 context（→ Spawn killTree 杀子进程树）→ 状态记 stopped；
//   - 每家 harness 结束后写 SetRunHarnessResult；全部结束 → FinishRun。
//
// 事件必须「增量」落库：不能等进程结束才写——GUI 的实时列显示依赖这个。因此事件
// 走 Runner 的 OnEvent 回调逐条入 channel，而不是等 Result.Events 一次性写入。
package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
	"github.com/qifeng2692/harness-monitor/internal/store"
)

// maxErrorLen 是 run_harness.error 的字符上限（规格：截断 500 字符）。
const maxErrorLen = 500

// writeBatchMax 是 writer 一次最多连续应用的写操作数（合并突发写入，减少推送次数）。
const writeBatchMax = 64

// RunnerFactory 构造某批次的 Runner，并把增量回调接好。
//
// onEvent / onStatus 由编排器提供：Runner 每解析出一条事件就调 onEvent，
// 状态变化就调 onStatus。测试可注入 fake Runner，用同一对回调模拟增量上报。
type RunnerFactory func(runID int64, runDir string,
	onEvent func(harness string, ev model.Event),
	onStatus func(harness, status, errMsg string)) Runner

// writeOp 是一次落库操作（事件批 / 结果 / 状态 / 批次收尾），经 channel 串行化。
type writeOp struct {
	runID   int64
	harness string
	events  []model.Event // 增量事件批
	result  *Result       // SetRunHarnessResult
	status  string        // SetRunHarnessStatus（"" 表示无）
	errMsg  string
	finish  bool // FinishRun（runs.status='done'）
}

// activeRun 是一个运行中批次的活跃进程表：每家 harness 一个 context.CancelFunc，
// Stop 时逐个取消（Spawn 收到取消即 killTree 杀进程树）。
type activeRun struct {
	runID int64

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	stopped bool
}

// add 登记一家的取消函数；批次已被 Stop 时返回 false（该家直接记 stopped，不起进程）。
func (ar *activeRun) add(harness string, cancel context.CancelFunc) bool {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	if ar.stopped {
		return false
	}
	ar.cancels[harness] = cancel
	return true
}

// remove 注销一家的取消函数（该家已结束）。
func (ar *activeRun) remove(harness string) {
	ar.mu.Lock()
	delete(ar.cancels, harness)
	ar.mu.Unlock()
}

// stop 标记批次停止并取消全部在跑 harness 的 context（幂等）。
func (ar *activeRun) stop() {
	ar.mu.Lock()
	ar.stopped = true
	cs := make([]context.CancelFunc, 0, len(ar.cancels))
	for _, c := range ar.cancels {
		cs = append(cs, c)
	}
	ar.mu.Unlock()
	for _, c := range cs {
		c()
	}
}

// Orchestrator 管理对比批次的完整生命周期。
//
// 零值不可用——用 New 构造（New 会启动唯一的 writer goroutine）。
type Orchestrator struct {
	store *store.Store

	// OnUpdate 在每次成功落库后回调（GUI 用它 runtime.EventsEmit("run:update", runID)）。
	// 可能为 nil。回调在 writer goroutine 上执行，应尽快返回。
	OnUpdate func(runID int64)

	// RunRoot 是批次工作目录根：批次目录 = RunRoot/<runID>，每家再下一层
	// <runID>/<harness>/（docs/DESIGN-runner.md 第 2 节）。空值 = <data>/runs。
	RunRoot string

	// Timeout 是单家 harness 超时；<=0 用 DefaultTimeout。
	Timeout time.Duration

	// NewRunner 可替换 Runner 构造（测试注入 fake）。nil = 真实 Spawn。
	NewRunner RunnerFactory

	ch      chan writeOp
	done    chan struct{} // writer 退出信号（close）
	closeMu sync.RWMutex
	closed  bool

	mu     sync.Mutex
	active map[int64]*activeRun

	errMu sync.Mutex
	errs  []error
}

// New 构造编排器并启动 writer goroutine。用完调 Close 释放。
func New(st *store.Store) *Orchestrator {
	o := &Orchestrator{
		store:  st,
		ch:     make(chan writeOp, 256),
		done:   make(chan struct{}),
		active: map[int64]*activeRun{},
	}
	go o.writer()
	return o
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// Start 并发启动一个批次的各家 harness，立即返回。
//
// 前置条件：runID 对应的 runs 行已存在（调用方先 CreateRun）。过程经 OnUpdate 推送。
// 重复的 harness 名会被去重（保持首次出现顺序）。
func (o *Orchestrator) Start(runID int64, prompt string, harnesses []string) error {
	if runID <= 0 {
		return fmt.Errorf("orchestrator: 非法批次 id %d", runID)
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("orchestrator: prompt 不能为空")
	}
	if len(harnesses) == 0 {
		return fmt.Errorf("orchestrator: harnesses 不能为空")
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(harnesses))
	for _, h := range harnesses {
		if !validHarness(h) {
			return fmt.Errorf("orchestrator: 未知 harness %q", h)
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		uniq = append(uniq, h)
	}

	ar := &activeRun{runID: runID, cancels: map[string]context.CancelFunc{}}
	o.mu.Lock()
	if _, busy := o.active[runID]; busy {
		o.mu.Unlock()
		return fmt.Errorf("orchestrator: 批次 %d 已在运行", runID)
	}
	o.active[runID] = ar
	o.mu.Unlock()

	runDir := filepath.Join(o.runRoot(), strconv.FormatInt(runID, 10))
	factory := o.NewRunner
	if factory == nil {
		factory = o.defaultRunner
	}

	var wg sync.WaitGroup
	for _, h := range uniq {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			o.runOne(ar, factory, runDir, h, prompt)
		}(h)
	}

	go func() {
		wg.Wait()
		// 全部结束 → 批次收尾（runs.status='done'）。
		o.enqueue(writeOp{runID: runID, finish: true})
		o.mu.Lock()
		delete(o.active, runID)
		o.mu.Unlock()
	}()
	return nil
}

// Stop 停止一个运行中的批次：取消所有在跑子进程（Spawn 内部 killTree 杀进程树），
// 终态记 stopped。批次未在运行时返回错误。
func (o *Orchestrator) Stop(runID int64) error {
	o.mu.Lock()
	ar, ok := o.active[runID]
	o.mu.Unlock()
	if !ok {
		return fmt.Errorf("orchestrator: 批次 %d 未在运行", runID)
	}
	ar.stop()
	return nil
}

// IsActive 报告批次当前是否在运行。
func (o *Orchestrator) IsActive(runID int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.active[runID]
	return ok
}

// ActiveRunIDs 返回当前在运行批次的 id（升序）。
func (o *Orchestrator) ActiveRunIDs() []int64 {
	o.mu.Lock()
	ids := make([]int64, 0, len(o.active))
	for id := range o.active {
		ids = append(ids, id)
	}
	o.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// WriteErrors 返回落库过程中累积的错误（诊断用）。
func (o *Orchestrator) WriteErrors() []error {
	o.errMu.Lock()
	defer o.errMu.Unlock()
	out := make([]error, len(o.errs))
	copy(out, o.errs)
	return out
}

// Close 停止全部在跑批次并关闭 writer goroutine（幂等）。
func (o *Orchestrator) Close() {
	o.mu.Lock()
	ars := make([]*activeRun, 0, len(o.active))
	for _, ar := range o.active {
		ars = append(ars, ar)
	}
	o.mu.Unlock()
	for _, ar := range ars {
		ar.stop()
	}

	o.closeMu.Lock()
	if o.closed {
		o.closeMu.Unlock()
		return
	}
	o.closed = true
	close(o.ch)
	o.closeMu.Unlock()
	<-o.done
}

// ---------------------------------------------------------------------------
// 单家 harness 的一次运行
// ---------------------------------------------------------------------------

// runOne 跑一家 harness：登记取消函数 → 置 running → Runner → 结果 + 终态落库。
func (o *Orchestrator) runOne(ar *activeRun, factory RunnerFactory, runDir, harness, prompt string) {
	ctx, cancel := context.WithCancel(context.Background())
	if !ar.add(harness, cancel) {
		// 批次已被 Stop：该家直接记 stopped，不起进程。
		cancel()
		o.enqueue(writeOp{runID: ar.runID, harness: harness,
			status: store.RunHarnessStopped, errMsg: "已停止"})
		return
	}
	defer ar.remove(harness)
	defer cancel()

	onEvent := func(h string, ev model.Event) {
		o.enqueue(writeOp{runID: ar.runID, harness: h, events: []model.Event{ev}})
	}
	onStatus := func(h, status, errMsg string) {
		o.enqueue(writeOp{runID: ar.runID, harness: h, status: status, errMsg: truncateErr(errMsg)})
	}
	r := factory(ar.runID, runDir, onEvent, onStatus)

	// 状态机：pending → running（进程即将启动）。
	o.enqueue(writeOp{runID: ar.runID, harness: harness, status: store.RunHarnessRunning})

	res, err := r.Run(ctx, harness, prompt)
	if err != nil {
		// 本地错误（启动失败等）：Spawn 此时不会调 OnStatus，由编排器兜底落库。
		status := store.RunHarnessFailed
		msg := firstNonEmpty(res.Error, err.Error())
		if errors.Is(ctx.Err(), context.Canceled) {
			status, msg = store.RunHarnessStopped, "已停止"
		}
		o.enqueue(writeOp{runID: ar.runID, harness: harness,
			status: status, errMsg: truncateErr(msg)})
		return
	}

	// 结果先落库、终态后落库：GUI 看到终态时结果必已就绪。
	o.enqueue(writeOp{runID: ar.runID, harness: harness, result: &res})
	o.enqueue(writeOp{runID: ar.runID, harness: harness,
		status: res.Status, errMsg: truncateErr(res.Error)})
}

// defaultRunner 是真实 Runner：Spawn + 增量回调。
func (o *Orchestrator) defaultRunner(_ int64, runDir string,
	onEvent func(string, model.Event), onStatus func(string, string, string)) Runner {
	return &Spawn{
		RunDir:   runDir,
		Timeout:  o.Timeout,
		OnEvent:  onEvent,
		OnStatus: onStatus,
	}
}

// runRoot 返回批次工作目录根。
func (o *Orchestrator) runRoot() string {
	if o.RunRoot != "" {
		return o.RunRoot
	}
	dir := "data"
	if o.store != nil && o.store.Path != "" {
		dir = filepath.Dir(o.store.Path)
	}
	return filepath.Join(dir, "runs")
}

// ---------------------------------------------------------------------------
// 唯一 writer goroutine：所有落库串行化（SQLite 单写者）
// ---------------------------------------------------------------------------

// enqueue 把一次落库操作交给 writer。Close 之后静默丢弃（避免 panic）。
func (o *Orchestrator) enqueue(op writeOp) {
	o.closeMu.RLock()
	defer o.closeMu.RUnlock()
	if o.closed {
		return
	}
	o.ch <- op
}

// writer 是唯一的落库 goroutine：按到达顺序应用操作，合并突发批次后统一推送。
func (o *Orchestrator) writer() {
	defer close(o.done)
	for {
		op, ok := <-o.ch
		if !ok {
			return
		}
		touched := map[int64]bool{}
		o.apply(op, touched)

		applied := 1
		draining, chClosed := true, false
		for draining && applied < writeBatchMax {
			select {
			case op, ok := <-o.ch:
				if !ok {
					chClosed, draining = true, false
					continue
				}
				o.apply(op, touched)
				applied++
			default:
				draining = false
			}
		}
		o.notify(touched)
		if chClosed {
			return
		}
	}
}

// apply 把一次写操作落到库里，并记录受影响的批次。
func (o *Orchestrator) apply(op writeOp, touched map[int64]bool) {
	if len(op.events) > 0 {
		if err := o.store.InsertRunEvents(op.runID, op.harness, op.events); err != nil {
			o.recordErr(fmt.Errorf("写入 run_events(run=%d,%s): %w", op.runID, op.harness, err))
		} else {
			touched[op.runID] = true
		}
	}
	if op.result != nil {
		r := op.result
		if err := o.store.SetRunHarnessResult(op.runID, op.harness,
			r.FinalText, r.SessionID, r.TokensIn, r.TokensOut, r.CostUSD, r.DurationMs); err != nil {
			o.recordErr(fmt.Errorf("写 run_harness 结果(run=%d,%s): %w", op.runID, op.harness, err))
		} else {
			touched[op.runID] = true
		}
	}
	if op.status != "" {
		if err := o.store.SetRunHarnessStatus(op.runID, op.harness, op.status, op.errMsg); err != nil {
			o.recordErr(fmt.Errorf("写 run_harness 状态(run=%d,%s): %w", op.runID, op.harness, err))
		} else {
			touched[op.runID] = true
		}
	}
	if op.finish {
		if err := o.store.FinishRun(op.runID); err != nil {
			o.recordErr(fmt.Errorf("FinishRun(run=%d): %w", op.runID, err))
		} else {
			touched[op.runID] = true
		}
	}
}

// notify 通知每个受影响的批次（GUI 收到后拉增量）。
func (o *Orchestrator) notify(touched map[int64]bool) {
	if o.OnUpdate == nil || len(touched) == 0 {
		return
	}
	for runID := range touched {
		o.OnUpdate(runID)
	}
}

func (o *Orchestrator) recordErr(err error) {
	o.errMu.Lock()
	o.errs = append(o.errs, err)
	o.errMu.Unlock()
}

// truncateErr 把错误文本截断到 maxErrorLen 个字符（按 rune，避免切坏 UTF-8）。
func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxErrorLen {
		return s
	}
	return string(r[:maxErrorLen]) + "…"
}
