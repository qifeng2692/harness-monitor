package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/qf/harness-monitor/internal/model"
)

// newTestStore 开一个全新临时库（含对比实验台三张表）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenPath(filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestRunLifecycle 覆盖：CreateRun → 插事件 → 读回一致 → FinishRun/ListRuns →
// SetRunHarnessResult/GetRun 正确 → 重复调用幂等。
func TestRunLifecycle(t *testing.T) {
	s := newTestStore(t)

	runID, err := s.CreateRun("Reply with exactly: PONG", []string{"hermes", "codex"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if runID <= 0 {
		t.Fatalf("CreateRun 返回非法 id: %d", runID)
	}

	// 建表后 GetRun 应看到 pending 的两家 harness（按名升序）。
	d, err := s.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun(新建): %v", err)
	}
	if d.Summary.Prompt != "Reply with exactly: PONG" {
		t.Errorf("prompt 不一致: %q", d.Summary.Prompt)
	}
	if d.Summary.Status != RunStatusRunning {
		t.Errorf("新批次 status 应为 running，得到 %q", d.Summary.Status)
	}
	if len(d.Summary.Harnesses) != 2 || d.Summary.Harnesses[0] != "hermes" || d.Summary.Harnesses[1] != "codex" {
		t.Errorf("harnesses 顺序/内容不一致: %v", d.Summary.Harnesses)
	}
	if len(d.HarnessRuns) != 2 {
		t.Fatalf("应有 2 行 harness，得到 %d", len(d.HarnessRuns))
	}
	if d.HarnessRuns[0].Harness != "codex" || d.HarnessRuns[1].Harness != "hermes" {
		t.Errorf("harness 行应按名升序，得到 %s,%s", d.HarnessRuns[0].Harness, d.HarnessRuns[1].Harness)
	}
	for _, h := range d.HarnessRuns {
		if h.Status != RunHarnessPending {
			t.Errorf("%s 初始 status 应为 pending，得到 %q", h.Harness, h.Status)
		}
	}

	// 插事件：seq<=0 自动递增；显式 seq 原样使用。
	now := time.Now().UTC()
	events := []model.Event{
		{Kind: model.KindSessionStart, Title: "session start", Content: "init", Time: now},
		{Kind: model.KindToolCall, Title: "call", Content: "LABTEST tool", Time: now},
		{Kind: model.KindLLMResponse, Title: "resp", Content: "PONG", Time: now},
	}
	if err := s.InsertRunEvents(runID, "hermes", events); err != nil {
		t.Fatalf("InsertRunEvents: %v", err)
	}
	// 再插一批，验证 seq 在已有基础上继续递增（不重置）。
	if err := s.InsertRunEvents(runID, "hermes", []model.Event{
		{Kind: model.KindLLMUsage, Title: "usage", Content: "tokens", Time: now},
	}); err != nil {
		t.Fatalf("InsertRunEvents(第二批): %v", err)
	}

	got, err := s.ListRunEvents(runID, "hermes")
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("应有 4 条事件，得到 %d", len(got))
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Errorf("事件 %d seq 应为 %d，得到 %d", i, i+1, e.Seq)
		}
		if e.Harness != "hermes" {
			t.Errorf("事件 %d harness 应为 hermes，得到 %q", i, e.Harness)
		}
	}
	if got[1].Kind != model.KindToolCall || got[1].Content != "LABTEST tool" {
		t.Errorf("tool.call 事件读回不一致: %+v", got[1])
	}
	if got[2].Content != "PONG" {
		t.Errorf("llm.response 内容应为 PONG，得到 %q", got[2].Content)
	}

	// 另一家 harness 的事件流独立（各自从 seq=1 起）。
	if err := s.InsertRunEvents(runID, "codex", []model.Event{
		{Kind: model.KindLLMResponse, Content: "PONG", Time: now},
	}); err != nil {
		t.Fatalf("InsertRunEvents(codex): %v", err)
	}
	codexEvents, err := s.ListRunEvents(runID, "codex")
	if err != nil {
		t.Fatalf("ListRunEvents(codex): %v", err)
	}
	if len(codexEvents) != 1 || codexEvents[0].Seq != 1 {
		t.Errorf("codex 事件流应独立从 seq=1 起: %+v", codexEvents)
	}

	// 结果 + 状态。
	if err := s.SetRunHarnessResult(runID, "hermes", "PONG", "sess-123", 10, 5, 0.0012, 1234); err != nil {
		t.Fatalf("SetRunHarnessResult: %v", err)
	}
	if err := s.SetRunHarnessStatus(runID, "hermes", RunHarnessRunning, ""); err != nil {
		t.Fatalf("SetRunHarnessStatus(running): %v", err)
	}
	if err := s.SetRunHarnessStatus(runID, "hermes", RunHarnessDone, ""); err != nil {
		t.Fatalf("SetRunHarnessStatus(done): %v", err)
	}
	if err := s.SetRunHarnessStatus(runID, "codex", RunHarnessFailed, "gateway 502"); err != nil {
		t.Fatalf("SetRunHarnessStatus(failed): %v", err)
	}

	d2, err := s.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun(结果后): %v", err)
	}
	var hermes, codex *RunHarness
	for i := range d2.HarnessRuns {
		switch d2.HarnessRuns[i].Harness {
		case "hermes":
			hermes = &d2.HarnessRuns[i]
		case "codex":
			codex = &d2.HarnessRuns[i]
		}
	}
	if hermes == nil || codex == nil {
		t.Fatalf("GetRun 缺 harness 行: %+v", d2.HarnessRuns)
	}
	if hermes.Status != RunHarnessDone || hermes.FinalText != "PONG" || hermes.SessionID != "sess-123" ||
		hermes.TokensIn != 10 || hermes.TokensOut != 5 || hermes.CostUSD != 0.0012 || hermes.DurationMs != 1234 {
		t.Errorf("hermes 结果行不一致: %+v", *hermes)
	}
	if hermes.StartedAt.IsZero() || hermes.EndedAt.IsZero() {
		t.Errorf("hermes started_at/ended_at 应已写入: %+v", *hermes)
	}
	if codex.Status != RunHarnessFailed || codex.Error != "gateway 502" {
		t.Errorf("codex 失败行不一致: %+v", *codex)
	}

	// FinishRun 后 ListRuns 含该批次。
	if err := s.FinishRun(runID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	runs, err := s.ListRuns(10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != runID {
		t.Fatalf("ListRuns 应含该批次: %+v", runs)
	}
	if runs[0].Status != RunStatusDone {
		t.Errorf("FinishRun 后 status 应为 done，得到 %q", runs[0].Status)
	}

	// 幂等：重复 FinishRun / SetRunHarnessResult / SetRunHarnessStatus 不改变结果、不报错。
	if err := s.FinishRun(runID); err != nil {
		t.Fatalf("重复 FinishRun: %v", err)
	}
	if err := s.SetRunHarnessResult(runID, "hermes", "PONG", "sess-123", 10, 5, 0.0012, 1234); err != nil {
		t.Fatalf("重复 SetRunHarnessResult: %v", err)
	}
	if err := s.SetRunHarnessStatus(runID, "hermes", RunHarnessDone, ""); err != nil {
		t.Fatalf("重复 SetRunHarnessStatus: %v", err)
	}
	d3, err := s.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun(幂等后): %v", err)
	}
	if len(d3.HarnessRuns) != 2 {
		t.Fatalf("幂等调用不应新增 harness 行，得到 %d", len(d3.HarnessRuns))
	}
	eventsAfter, _ := s.ListRunEvents(runID, "hermes")
	if len(eventsAfter) != 4 {
		t.Errorf("幂等调用不应改变事件数，得到 %d", len(eventsAfter))
	}
}

// TestListRunsOrder 验证 ListRuns 倒序（最新在前）与 limit。
func TestListRunsOrder(t *testing.T) {
	s := newTestStore(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := s.CreateRun("p", []string{"hermes"})
		if err != nil {
			t.Fatalf("CreateRun %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	runs, err := s.ListRuns(0) // 默认 limit
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("应有 3 个批次，得到 %d", len(runs))
	}
	if runs[0].ID != ids[2] || runs[2].ID != ids[0] {
		t.Errorf("ListRuns 应倒序，得到 %d,%d,%d", runs[0].ID, runs[1].ID, runs[2].ID)
	}
	limited, err := s.ListRuns(2)
	if err != nil {
		t.Fatalf("ListRuns(2): %v", err)
	}
	if len(limited) != 2 || limited[0].ID != ids[2] {
		t.Errorf("ListRuns(2) 应为最近两条: %+v", limited)
	}
}

// TestGetRunMissing 验证不存在的批次返回错误。
func TestGetRunMissing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetRun(99999); err == nil {
		t.Fatal("GetRun 不存在的批次应返回错误")
	}
}

// TestCreateRunEmptyHarnesses 验证空 harness 列表被拒绝。
func TestCreateRunEmptyHarnesses(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateRun("p", nil); err == nil {
		t.Fatal("CreateRun 空 harness 列表应返回错误")
	}
}
