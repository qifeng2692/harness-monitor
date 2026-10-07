// runner 包的表驱动测试：用 internal/runner/testdata/ 的 8 个真实输出 fixture
// （每家 harness 两个：pong 简单问答 / tool 带工具调用）验证解析器映射。
//
// fixture 全部来自本机真实运行（2026-10-05 实测采集），非手工构造。
package runner

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// fixtureCase 是一个 fixture 的期望。
type fixtureCase struct {
	name     string // fixture 文件名（不含 .jsonl）
	harness  string
	file     string
	finalTxt string // 期望的 final_text
	wantTool bool   // 是否应产出 tool.call + tool.result
}

var fixtureCases = []fixtureCase{
	{name: "hermes_pong", harness: HarnessHermes, file: "hermes_pong.jsonl", finalTxt: "PONG"},
	{name: "hermes_tool", harness: HarnessHermes, file: "hermes_tool.jsonl", finalTxt: "DONE", wantTool: true},
	{name: "codex_pong", harness: HarnessCodex, file: "codex_pong.jsonl", finalTxt: "PONG"},
	{name: "codex_tool", harness: HarnessCodex, file: "codex_tool.jsonl", finalTxt: "DONE", wantTool: true},
	{name: "claude_pong", harness: HarnessClaude, file: "claude_pong.jsonl", finalTxt: "PONG"},
	{name: "claude_tool", harness: HarnessClaude, file: "claude_tool.jsonl", finalTxt: "DONE", wantTool: true},
	{name: "pi_pong", harness: HarnessPi, file: "pi_pong.jsonl", finalTxt: "PONG"},
	{name: "pi_tool", harness: HarnessPi, file: "pi_tool.jsonl", finalTxt: "DONE", wantTool: true},
}

// parseFixture 用某家 harness 的解析器跑一遍 fixture，返回事件流与解析状态。
func parseFixture(t *testing.T, harness, file string) ([]model.Event, *ParseState) {
	t.Helper()
	parse, flush, ok := ParserFor(harness)
	if !ok {
		t.Fatalf("%s: 无解析器", harness)
	}
	f, err := os.Open(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("打开 fixture %s: %v", file, err)
	}
	defer f.Close()

	st := &ParseState{Harness: harness}
	var events []model.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		for _, ev := range parse(line, st) {
			if ev.Harness == "" {
				ev.Harness = harness
			}
			events = append(events, ev)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读取 fixture %s: %v", file, err)
	}
	if flush != nil {
		events = append(events, flush(st)...)
	}
	return events, st
}

// TestParseFixtures 是核心表驱动测试：8 个 fixture 全部走一遍，断言
// final_text、tool 事件、kind 映射与 session/token 解析。
func TestParseFixtures(t *testing.T) {
	for _, tc := range fixtureCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			events, st := parseFixture(t, tc.harness, tc.file)

			// 1) final_text 必须正确（hermes/codex/claude/pi 的 pong 都是 "PONG"）。
			if st.FinalText != tc.finalTxt {
				t.Errorf("final_text = %q，期望 %q", st.FinalText, tc.finalTxt)
			}

			// 2) 事件流非空，且每条事件 harness 已归属。
			if len(events) == 0 {
				t.Fatalf("无事件产出")
			}
			for i, ev := range events {
				if ev.Harness != tc.harness {
					t.Errorf("事件 %d harness = %q，期望 %q", i, ev.Harness, tc.harness)
				}
				if ev.Kind == "" {
					t.Errorf("事件 %d kind 为空: %+v", i, ev)
				}
			}

			// 3) 每家 pong/tool fixture 都应产出 session.start 与 llm.usage。
			assertKind(t, events, model.KindSessionStart)
			assertKind(t, events, model.KindLLMUsage)

			// 4) tool fixture：必须有 tool.call + tool.result，且内容含 LABTEST。
			if tc.wantTool {
				calls := filterKind(events, model.KindToolCall)
				results := filterKind(events, model.KindToolResult)
				if len(calls) == 0 {
					t.Errorf("tool fixture 未产出 tool.call")
				}
				if len(results) == 0 {
					t.Errorf("tool fixture 未产出 tool.result")
				}
				if !anyContains(events, model.KindToolCall, "LABTEST") {
					t.Errorf("tool.call 内容不含 LABTEST: %s", dumpKind(events, model.KindToolCall))
				}
				if !anyContains(events, model.KindToolResult, "LABTEST") {
					t.Errorf("tool.result 内容不含 LABTEST: %s", dumpKind(events, model.KindToolResult))
				}
			}

			// 5) 每家都应有 llm.response（pong 的文本 / tool 的最终回答）。
			assertKind(t, events, model.KindLLMResponse)

			// 6) session_id 已解析。
			if st.SessionID == "" {
				t.Errorf("session_id 未解析出来")
			}

			// 7) token 用量已解析（四家 fixture 都带 usage）。
			if st.TokensIn == 0 && st.TokensOut == 0 {
				t.Errorf("token 用量未解析（in=%d out=%d）", st.TokensIn, st.TokensOut)
			}

			// 8) 协议层完成事件必须被识别（Done=true）。这条断言是真实集成测试
			// 抓出来的教训：pi 漏标 Done → 成功运行被兜底判成 failed。
			if !st.Done {
				t.Errorf("未识别到协议层完成事件（Done=false）")
			}
			if st.Fatal {
				t.Errorf("正常 fixture 不应有致命失败标记: %q", st.ErrText)
			}
		})
	}
}

// TestParseKindCoverage 断言四家都能产出设计规格要求的五种 kind
// （session.start / tool.call / tool.result / llm.response / llm.usage）——
// 用各家 tool fixture 合并事件流验证。
func TestParseKindCoverage(t *testing.T) {
	want := []string{
		model.KindSessionStart,
		model.KindToolCall,
		model.KindToolResult,
		model.KindLLMResponse,
		model.KindLLMUsage,
	}
	for _, harness := range All {
		harness := harness
		t.Run(harness, func(t *testing.T) {
			events, _ := parseFixture(t, harness, harness+"_tool.jsonl")
			seen := map[string]int{}
			for _, ev := range events {
				seen[ev.Kind]++
			}
			for _, k := range want {
				if seen[k] == 0 {
					t.Errorf("%s 缺 kind %q（已出现: %v）", harness, k, seen)
				}
			}
		})
	}
}

// TestParseIgnoresNonJSON 断言非 JSON 噪声行被忽略而非报错或产出垃圾事件。
func TestParseIgnoresNonJSON(t *testing.T) {
	cases := []struct {
		harness string
		line    string
	}{
		{HarnessCodex, "Reading additional input from stdin..."},
		{HarnessPi, `Warning: Extension package "builtin:mcp": Extension ...`},
		{HarnessClaude, `[claude-code:unrecognized_model] {"model":"zcode/GLM-5.3[1m]","query_source":"sdk"}`},
		{HarnessHermes, "session_id: 20261005_122539_3e044a"},
		{HarnessHermes, ""},
		{HarnessPi, "   "},
	}
	for _, tc := range cases {
		parse, _, _ := ParserFor(tc.harness)
		st := &ParseState{Harness: tc.harness}
		got := parse([]byte(tc.line), st)
		if len(got) != 0 {
			t.Errorf("%s 对非 JSON 行 %q 产出了 %d 个事件", tc.harness, tc.line, len(got))
		}
		if st.SessionID != "" || st.FinalText != "" || st.Done {
			t.Errorf("%s 非 JSON 行污染了 state: %+v", tc.harness, st)
		}
	}
}

// TestHermesTextAccumulation 验证 hermes 的文本分片（"P" + "ONG"）被累积成
// 一条完整的 llm.response，而不是两条碎片事件。
func TestHermesTextAccumulation(t *testing.T) {
	events, st := parseFixture(t, HarnessHermes, "hermes_pong.jsonl")
	if st.FinalText != "PONG" {
		t.Fatalf("final_text = %q，期望 PONG", st.FinalText)
	}
	resp := filterKind(events, model.KindLLMResponse)
	if len(resp) != 1 {
		t.Fatalf("文本分片应冲刷为 1 条 llm.response，得到 %d 条: %+v", len(resp), resp)
	}
	if resp[0].Content != "PONG" {
		t.Errorf("累积文本 = %q，期望 PONG", resp[0].Content)
	}
}

// TestHermesNoReasoning 断言 hermes 协议不产出 reasoning（源码明确的协议限制），
// 避免以后有人"顺手"补上不存在的事件类型。
func TestHermesNoReasoning(t *testing.T) {
	for _, f := range []string{"hermes_pong.jsonl", "hermes_tool.jsonl"} {
		events, _ := parseFixture(t, HarnessHermes, f)
		if n := len(filterKind(events, model.KindLLMReasoning)); n != 0 {
			t.Errorf("%s: hermes 不应产出 llm.reasoning，得到 %d 条", f, n)
		}
	}
}

// TestPiTurnEndFinalText 验证 pi 的 final_text 取自最后一个含 text 的 turn_end
// （tool fixture 里第一个 turn_end 只有 toolCall，第二个才有 "DONE"）。
func TestPiTurnEndFinalText(t *testing.T) {
	events, st := parseFixture(t, HarnessPi, "pi_tool.jsonl")
	if st.FinalText != "DONE" {
		t.Errorf("pi final_text = %q，期望 DONE", st.FinalText)
	}
	// tool fixture 有两个 turn_end → 两条 llm.usage（usage 逐 turn 累计）。
	if n := len(filterKind(events, model.KindLLMUsage)); n < 2 {
		t.Errorf("pi tool fixture 应有两个 turn_end 的 usage，得到 %d 条", n)
	}
	// 每个 turn_end 的 usage 是「本 turn」用量；最终值取最后一个 turn。
	if st.TokensOut != 3 {
		t.Errorf("pi 最终 tokens_out = %d，期望 3（最后一个 turn_end）", st.TokensOut)
	}
}

// TestClaudeReasoningAndCost 验证 claude 的 thinking → llm.reasoning 与
// result 的 cost 解析（claude 是四家中唯一带费用的）。
func TestClaudeReasoningAndCost(t *testing.T) {
	events, st := parseFixture(t, HarnessClaude, "claude_pong.jsonl")
	if n := len(filterKind(events, model.KindLLMReasoning)); n == 0 {
		t.Errorf("claude 应产出 llm.reasoning（thinking 块）")
	}
	if st.CostUSD <= 0 {
		t.Errorf("claude cost_usd = %v，期望 > 0", st.CostUSD)
	}
	// thinking_tokens 是流式噪声，不应产出 session.start 之外的 system 事件。
	starts := filterKind(events, model.KindSessionStart)
	if len(starts) != 1 {
		t.Errorf("claude 应恰好 1 条 session.start，得到 %d 条", len(starts))
	}
}

// TestCodexWarningsNotFatal 验证 codex 的「忽略未知配置项」告警事件不把运行
// 判成失败（Done=true 且 Fatal=false）——否则正常成功会被误标。
func TestCodexWarningsNotFatal(t *testing.T) {
	events, st := parseFixture(t, HarnessCodex, "codex_pong.jsonl")
	if !st.Done {
		t.Errorf("codex pong 应标记 Done（收到 turn.completed）")
	}
	if st.Fatal {
		t.Errorf("codex 的配置告警不应置 Fatal（ErrText=%q）", st.ErrText)
	}
	// 告警仍应作为 error 事件记录（如实展示，不静默丢弃）。
	if n := len(filterKind(events, model.KindError)); n == 0 {
		t.Errorf("codex 的 item.type=error 告警应产出 error 事件")
	}
}

// TestParserForUnknown 验证未知 harness 返回 ok=false。
func TestParserForUnknown(t *testing.T) {
	if _, _, ok := ParserFor("magpie"); ok {
		t.Errorf("magpie 不是 harness，不应有解析器")
	}
}

// TestBuildSpecPromptChannel 验证 prompt 传递通道：codex/pi 走 stdin，
// hermes/claude 走参数（docs/DESIGN-runner.md 第 2 节的实测结论）。
func TestBuildSpecPromptChannel(t *testing.T) {
	const prompt = "Reply with exactly: PONG"
	// 用环境变量注入假的可执行文件路径，避免依赖本机 PATH。
	for _, harness := range All {
		t.Setenv(binOverrideEnv(harness), "fake-"+harness)
		spec, err := BuildSpec(harness, `C:\tmp\run\`+harness, prompt)
		if err != nil {
			t.Fatalf("%s: BuildSpec: %v", harness, err)
		}
		wantStdin := harness == HarnessCodex || harness == HarnessPi
		if spec.UseStdin != wantStdin {
			t.Errorf("%s: UseStdin = %v，期望 %v", harness, spec.UseStdin, wantStdin)
		}
		if wantStdin {
			if spec.Prompt != prompt {
				t.Errorf("%s: stdin prompt 丢失", harness)
			}
			if strings.Contains(strings.Join(spec.Args, "\x00"), prompt) {
				t.Errorf("%s: prompt 不应出现在参数里: %v", harness, spec.Args)
			}
		} else {
			if !strings.Contains(strings.Join(spec.Args, "\x00"), prompt) {
				t.Errorf("%s: prompt 应作为参数传递: %v", harness, spec.Args)
			}
		}
		// stdin 的 harness 不应设置 cmd.Stdin 之外的东西；构造 Command 不 panic。
		if spec.Command() == nil {
			t.Errorf("%s: Command() 返回 nil", harness)
		}
	}
}

// TestBuildSpecUnknownHarness 验证未知 harness 被拒绝。
func TestBuildSpecUnknownHarness(t *testing.T) {
	if _, err := BuildSpec("magpie", t.TempDir(), "x"); err == nil {
		t.Errorf("magpie 应被拒绝")
	}
}

// ---------------------------------------------------------------------------
// 断言工具
// ---------------------------------------------------------------------------

func filterKind(events []model.Event, kind string) []model.Event {
	var out []model.Event
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func assertKind(t *testing.T, events []model.Event, kind string) {
	t.Helper()
	if len(filterKind(events, kind)) == 0 {
		t.Errorf("缺 kind %q", kind)
	}
}

func anyContains(events []model.Event, kind, needle string) bool {
	for _, ev := range events {
		if ev.Kind == kind && strings.Contains(ev.Content, needle) {
			return true
		}
	}
	return false
}

func dumpKind(events []model.Event, kind string) string {
	var sb strings.Builder
	for _, ev := range filterKind(events, kind) {
		sb.WriteString("  [" + ev.Title + "] " + truncate(ev.Content, 200) + "\n")
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestSanitizedEnvAddsNoProxy 验证代理环境净化：无论 NO_PROXY 原值如何，
// 子进程环境必须包含回环地址（修复 502 环境变量污染）。
//
// 注意：Windows 环境变量大小写不敏感（os.Setenv/Unsetenv 两个键互相影响），
// 因此每个子用例先统一清空两个键，再按需设置。
func TestSanitizedEnvAddsNoProxy(t *testing.T) {
	cases := []struct {
		name      string
		noProxy   string // 初始 NO_PROXY 值（空串=未设置）
		httpProxy string
	}{
		{"无 NO_PROXY 时追加", "", "http://127.0.0.1:7890"},
		{"已有 NO_PROXY 时合并", "example.com", "http://127.0.0.1:7890"},
		{"小写 no_proxy 也合并", "foo.local", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 统一清空（两个键都清，规避 Windows 大小写不敏感）
			os.Unsetenv("NO_PROXY")
			os.Unsetenv("no_proxy")
			os.Unsetenv("HTTP_PROXY")
			if tc.noProxy != "" {
				os.Setenv("NO_PROXY", tc.noProxy)
			}
			if tc.httpProxy != "" {
				os.Setenv("HTTP_PROXY", tc.httpProxy)
			}
			defer func() {
				os.Unsetenv("NO_PROXY")
				os.Unsetenv("no_proxy")
				os.Unsetenv("HTTP_PROXY")
			}()

			env := sanitizedEnv()
			joined := strings.Join(env, "\n")
			if !strings.Contains(joined, "127.0.0.1") {
				t.Errorf("sanitizedEnv 未包含 127.0.0.1:\n%s", joined)
			}
			if !strings.Contains(joined, "localhost") {
				t.Errorf("sanitizedEnv 未包含 localhost:\n%s", joined)
			}
			// 原有 NO_PROXY 条目保留
			if tc.noProxy != "" && !strings.Contains(joined, tc.noProxy) {
				t.Errorf("原 NO_PROXY 条目 %q 丢失:\n%s", tc.noProxy, joined)
			}
			// 代理变量保留
			if tc.httpProxy != "" && !strings.Contains(joined, tc.httpProxy) {
				t.Errorf("HTTP_PROXY 丢失")
			}
		})
	}
}

// TestMergeNoProxy 验证合并去重。
func TestMergeNoProxy(t *testing.T) {
	got := mergeNoProxy("a.com, 127.0.0.1", "127.0.0.1,localhost")
	if got != "a.com,127.0.0.1,localhost" {
		t.Errorf("mergeNoProxy = %q", got)
	}
	got = mergeNoProxy("", "127.0.0.1,localhost")
	if got != "127.0.0.1,localhost" {
		t.Errorf("mergeNoProxy empty = %q", got)
	}
}
