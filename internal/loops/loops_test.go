package loops

import "testing"

// TestDetectGroupsIdenticalRepeatedCalls 覆盖最基本的判定：同一会话内、同一个工具、
// 同一组参数被调用 3 次 → 命中一组；参数不同或次数不够 → 不命中。
func TestDetectGroupsIdenticalRepeatedCalls(t *testing.T) {
	calls := []Call{
		{Harness: "hermes", SessionID: "s1", Tool: "terminal", Args: `{"command":"npm test"}`, Key: "c1"},
		{Harness: "hermes", SessionID: "s1", Tool: "terminal", Args: `{"command":"npm test"}`, Key: "c2"},
		{Harness: "hermes", SessionID: "s1", Tool: "terminal", Args: `{"command":"npm test"}`, Key: "c3"},
		// 参数不同 → 另一组，只调用 1 次，不该命中。
		{Harness: "hermes", SessionID: "s1", Tool: "terminal", Args: `{"command":"npm run build"}`, Key: "c4"},
		// 同一工具同一参数，但在另一个会话 → 不该跟 s1 混在一起。
		{Harness: "hermes", SessionID: "s2", Tool: "terminal", Args: `{"command":"npm test"}`, Key: "c5"},
	}

	groups := Detect(calls, nil, 3)
	if len(groups) != 1 {
		t.Fatalf("应有 1 组重复调用，得到 %d 组：%+v", len(groups), groups)
	}
	g := groups[0]
	if g.Harness != "hermes" || g.SessionID != "s1" || g.Tool != "terminal" {
		t.Errorf("组的 harness/会话/工具 = %s/%s/%s，期望 hermes/s1/terminal", g.Harness, g.SessionID, g.Tool)
	}
	if g.Calls != 3 {
		t.Errorf("重复次数 = %d，期望 3", g.Calls)
	}
}

// TestDetectCountsFailuresOnlyWhenJudged 覆盖失败口径：失败只认带失败信号的结果；
// 判不了的（没带信号）不算成功也不算失败，单独用 Judged 体现缺口。
func TestDetectCountsFailuresOnlyWhenJudged(t *testing.T) {
	calls := []Call{
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c2"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c3"},
	}
	results := []Result{
		{Harness: "hermes", SessionID: "s1", Key: "c1", Failed: true, Judged: true},
		{Harness: "hermes", SessionID: "s1", Key: "c2", Failed: true, Judged: true},
		{Harness: "hermes", SessionID: "s1", Key: "c3", Failed: false, Judged: false}, // 判不了
	}

	groups := Detect(calls, results, 3)
	if len(groups) != 1 {
		t.Fatalf("应有 1 组，得到 %d 组", len(groups))
	}
	g := groups[0]
	if g.Failed != 2 {
		t.Errorf("失败数 = %d，期望 2", g.Failed)
	}
	// 判得了 2 条（两条失败），第三条判不了 → 缺口 1。
	if g.Judged != 2 {
		t.Errorf("判定数 = %d，期望 2", g.Judged)
	}
}

// TestDetectIgnoresUnmatchedResults 覆盖：结果挂不上调用（键对不上）时不该污染计数。
func TestDetectIgnoresUnmatchedResults(t *testing.T) {
	calls := []Call{
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c2"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c3"},
	}
	results := []Result{
		{Harness: "hermes", SessionID: "s1", Key: "不存在", Failed: true, Judged: true},
	}

	groups := Detect(calls, results, 3)
	if len(groups) != 1 {
		t.Fatalf("应有 1 组，得到 %d 组", len(groups))
	}
	if groups[0].Failed != 0 {
		t.Errorf("挂不上的结果不该计入失败，得到 %d", groups[0].Failed)
	}
	if groups[0].Judged != 0 {
		t.Errorf("挂不上的结果不该计入判定数，得到 %d", groups[0].Judged)
	}
}

// TestDetectThresholdBoundary 覆盖阈值边界：恰好等于阈值命中，少一次不命中。
func TestDetectThresholdBoundary(t *testing.T) {
	mk := func(n int) []Call {
		out := make([]Call, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, Call{
				Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`,
				Key: "c" + string(rune('a'+i)),
			})
		}
		return out
	}
	if g := Detect(mk(3), nil, 3); len(g) != 1 {
		t.Errorf("恰好 3 次应命中，得到 %d 组", len(g))
	}
	if g := Detect(mk(2), nil, 3); len(g) != 0 {
		t.Errorf("只有 2 次不该命中，得到 %d 组", len(g))
	}
}

// TestDetectDefaultThreshold 覆盖：threshold<=0 时用默认值 3。
func TestDetectDefaultThreshold(t *testing.T) {
	calls := []Call{
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c2"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c3"},
	}
	if g := Detect(calls, nil, 0); len(g) != 1 {
		t.Errorf("threshold=0 应按 3 处理，得到 %d 组", len(g))
	}
}

// TestDetectEmptyInput 不 panic，返回空结果。
func TestDetectEmptyInput(t *testing.T) {
	if g := Detect(nil, nil, 3); len(g) != 0 {
		t.Errorf("空输入应返回空结果，得到 %+v", g)
	}
}

// TestDetectSortsByCallsDescending 覆盖排序：重复次数多的排前面（打转最狠的在前）。
func TestDetectSortsByCallsDescending(t *testing.T) {
	calls := []Call{}
	for i := 0; i < 5; i++ {
		calls = append(calls, Call{Harness: "hermes", SessionID: "s1", Tool: "a", Args: `{"x":1}`, Key: "a" + string(rune('a'+i))})
	}
	for i := 0; i < 3; i++ {
		calls = append(calls, Call{Harness: "hermes", SessionID: "s1", Tool: "b", Args: `{"x":2}`, Key: "b" + string(rune('a'+i))})
	}
	groups := Detect(calls, nil, 3)
	if len(groups) != 2 {
		t.Fatalf("应有 2 组，得到 %d 组", len(groups))
	}
	if groups[0].Calls != 5 || groups[1].Calls != 3 {
		t.Errorf("应按重复次数降序，得到 %d / %d", groups[0].Calls, groups[1].Calls)
	}
}

// TestDetectDeduplicatesRepeatedCallIDs 覆盖真实库里的一个坑：同一次调用可能被采成
// 多行（实测本机库有 5139 个 call_id 重复），按行数算会把「重复采集」误报成
// 「重复调用」。同一个 call_id 只算一次。
func TestDetectDeduplicatesRepeatedCallIDs(t *testing.T) {
	calls := []Call{
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"}, // 同一次调用采了两遍
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c2"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c3"},
	}
	groups := Detect(calls, nil, 3)
	if len(groups) != 1 {
		t.Fatalf("应有 1 组，得到 %d 组", len(groups))
	}
	if groups[0].Calls != 3 {
		t.Errorf("去重后调用数 = %d，期望 3（同一个 call_id 只算一次）", groups[0].Calls)
	}
}

// TestDetectSeparatesHarnesses 覆盖：不同 harness 的同一会话 ID 不该混为一组。
// call_id 在各家之间可能撞车，混组会得出「跨 harness 的重复调用」这种假结论。
func TestDetectSeparatesHarnesses(t *testing.T) {
	calls := []Call{
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"},
		{Harness: "codex", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"},
		{Harness: "pi", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"},
	}
	if g := Detect(calls, nil, 3); len(g) != 0 {
		t.Errorf("三家各一次调用不该命中（不该跨 harness 混组），得到 %+v", g)
	}
}

// TestDetectKeylessCallsStillCount 覆盖：没有 call_id 的调用（理论上不该出现，
// 但取数层不该因此丢行）仍按行数计数。
func TestDetectKeylessCallsStillCount(t *testing.T) {
	calls := []Call{
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`},
	}
	groups := Detect(calls, nil, 3)
	if len(groups) != 1 {
		t.Fatalf("应有 1 组，得到 %d 组", len(groups))
	}
	if groups[0].Calls != 3 {
		t.Errorf("无 call_id 时应按行计数 = %d，期望 3", groups[0].Calls)
	}
}

// TestDetectAnyFailureInGroupCountsAsFailed 覆盖：同一次调用的多条结果里只要有一条
// 判失败，就算失败（宁可报出失败，也不把失败藏起来）。
func TestDetectAnyFailureInGroupCountsAsFailed(t *testing.T) {
	calls := []Call{
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c1"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c2"},
		{Harness: "hermes", SessionID: "s1", Tool: "bash", Args: `{"command":"ls"}`, Key: "c3"},
	}
	results := []Result{
		{Harness: "hermes", SessionID: "s1", Key: "c1", Failed: false, Judged: true},
		{Harness: "hermes", SessionID: "s1", Key: "c1", Failed: true, Judged: true}, // 同一次调用的另一条结果判失败
		{Harness: "hermes", SessionID: "s1", Key: "c2", Failed: false, Judged: true},
	}
	groups := Detect(calls, results, 3)
	if len(groups) != 1 {
		t.Fatalf("应有 1 组，得到 %d 组", len(groups))
	}
	if groups[0].Failed != 1 {
		t.Errorf("失败数 = %d，期望 1", groups[0].Failed)
	}
	// 判得了 2 次调用（c1、c2），c3 没结果 → 判不了。
	if groups[0].Judged != 2 {
		t.Errorf("判定数 = %d，期望 2", groups[0].Judged)
	}
}

// TestSampleTruncatesByRune 覆盖样例截断：中文按字断，不切出乱码。
func TestSampleTruncatesByRune(t *testing.T) {
	long := "重复调用参数样例：这是一个很长很长的中文参数串，用来验证截断不会切出乱码字节"
	got := Sample(long, 10)
	r := []rune(got)
	if len(r) != 10 {
		t.Errorf("截断后长度 = %d rune，期望 10", len(r))
	}
	for _, c := range r {
		if c == '\ufffd' {
			t.Fatalf("截断不应产生替换字符（说明按字节切开了 rune）：%q", got)
		}
	}
	if Sample("短", 10) != "短" {
		t.Errorf("短串不该被截断，得到 %q", Sample("短", 10))
	}
}
