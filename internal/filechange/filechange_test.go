package filechange

import (
	"testing"
	"time"
)

// 这些参数原文都取自本机真实库（data/hm.db）的 tool.call content，只做了无关字段的删减。
func TestClassifyPerHarness(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		tool    string
		args    string
		want    string
		wantOp  Op
		isWrite bool
	}{
		{
			name:    "claude Edit 的路径在 file_path",
			harness: "claude", tool: "Edit",
			args: `{"replace_all":false,"file_path":"G:\\projects\\harness-monitor\\internal\\collect\\zz_verify_test.go","old_string":"a","new_string":"b"}`,
			want: "G:/projects/harness-monitor/internal/collect/zz_verify_test.go", wantOp: OpEdit, isWrite: true,
		},
		{
			name:    "claude Write 的路径在 file_path",
			harness: "claude", tool: "Write",
			args: `{"file_path":"G:\\projects\\harness-monitor\\internal\\collect\\zz_verify_test.go","content":"package collect"}`,
			want: "G:/projects/harness-monitor/internal/collect/zz_verify_test.go", wantOp: OpWrite, isWrite: true,
		},
		{
			name:    "hermes patch 的路径在 path",
			harness: "hermes", tool: "patch",
			args: `{"new_string":"x","old_string":"y","path":"C:/Users/QF/AppData/Local/Temp/qm_check_20261023.py"}`,
			want: "C:/Users/QF/AppData/Local/Temp/qm_check_20261023.py", wantOp: OpEdit, isWrite: true,
		},
		{
			name:    "hermes patch 改名时退回 new_path",
			harness: "hermes", tool: "patch",
			args: `{"new_string":"x","old_string":"y","new_path":"G:/projects/a/b.go"}`,
			want: "G:/projects/a/b.go", wantOp: OpEdit, isWrite: true,
		},
		{
			name:    "hermes write_file 的路径在 path",
			harness: "hermes", tool: "write_file",
			args: `{"content":"x","path":"C:/Users/QF/AppData/Local/Temp/hm_probe.py"}`,
			want: "C:/Users/QF/AppData/Local/Temp/hm_probe.py", wantOp: OpWrite, isWrite: true,
		},
		{
			name:    "pi edit 的路径在 path",
			harness: "pi", tool: "edit",
			args: `{"edits":[{"newText":"a","oldText":"b"}],"path":"G:/my-project/scripts/dashboard.mjs"}`,
			want: "G:/my-project/scripts/dashboard.mjs", wantOp: OpEdit, isWrite: true,
		},
		{
			name:    "pi write 的路径在 path",
			harness: "pi", tool: "write",
			args: `{"path":"G:/my-project/.gitignore","content":"node_modules/"}`,
			want: "G:/my-project/.gitignore", wantOp: OpWrite, isWrite: true,
		},
		{
			name:    "写文件工具但参数里没有路径 → 判得出是写文件、给不出路径",
			harness: "pi", tool: "edit",
			args: `{"edits":[{"newText":"a","oldText":"b"}]}`,
			want: "", wantOp: OpEdit, isWrite: true,
		},
		{
			name:    "参数不是合法 JSON → 不猜路径",
			harness: "claude", tool: "Write",
			args: `not json at all`,
			want: "", wantOp: OpWrite, isWrite: true,
		},
		{
			name:    "读文件不是改动",
			harness: "claude", tool: "Read",
			args: `{"file_path":"G:/projects/a/b.go"}`,
			want: "", wantOp: "", isWrite: false,
		},
		{
			name:    "hermes terminal 不算（路径埋在命令行里）",
			harness: "hermes", tool: "terminal",
			args: `{"command":"Set-Content -Path a.ps1 -Value 'x'"}`,
			want: "", wantOp: "", isWrite: false,
		},
		{
			name:    "codex 没有结构化写文件工具",
			harness: "codex", tool: "exec_command",
			args: `{"cmd":"apply_patch <<'EOF'","workdir":"G:/projects/a"}`,
			want: "", wantOp: "", isWrite: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPath, gotOp, gotWrite := Classify(c.harness, c.tool, c.args)
			if gotPath != c.want {
				t.Errorf("路径 = %q，期望 %q", gotPath, c.want)
			}
			if gotOp != c.wantOp {
				t.Errorf("操作 = %q，期望 %q", gotOp, c.wantOp)
			}
			if gotWrite != c.isWrite {
				t.Errorf("isWrite = %v，期望 %v", gotWrite, c.isWrite)
			}
		})
	}
}

func TestSupports(t *testing.T) {
	for _, h := range []string{"claude", "hermes", "pi"} {
		if !Supports(h) {
			t.Errorf("Supports(%q) = false，期望 true", h)
		}
	}
	for _, h := range []string{"codex", "magpie", ""} {
		if Supports(h) {
			t.Errorf("Supports(%q) = true，期望 false", h)
		}
	}
	if got := Supported(); len(got) != 3 || got[0] != "claude" || got[1] != "hermes" || got[2] != "pi" {
		t.Errorf("Supported() = %v，期望 [claude hermes pi]", got)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{`G:\projects\a\b.go`, "G:/projects/a/b.go"},
		{"G:/projects/a/b.go", "G:/projects/a/b.go"},
		{"G:/projects/./a//b.go", "G:/projects/a/b.go"},
		{"  G:/projects/a/b.go  ", "G:/projects/a/b.go"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestSummarizeGroupsSameFileWrittenBothWays(t *testing.T) {
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	calls := []Call{
		// 同一个文件：一次反斜杠写法、一次正斜杠写法 → 必须归成一条
		{Tool: "patch", Args: `{"path":"G:\\projects\\hm\\a.go"}`, Seq: 10, Time: t0},
		{Tool: "write_file", Args: `{"path":"G:/projects/hm/a.go"}`, Seq: 20, Time: t0.Add(time.Minute)},
		// 另一个文件，只改一次
		{Tool: "patch", Args: `{"path":"G:/projects/hm/b.go"}`, Seq: 30, Time: t0.Add(2 * time.Minute)},
		// 写文件但没带路径
		{Tool: "patch", Args: `{"old_string":"x"}`, Seq: 40, Time: t0.Add(3 * time.Minute)},
		// 不是写文件
		{Tool: "read_file", Args: `{"path":"G:/projects/hm/c.go"}`, Seq: 50, Time: t0.Add(4 * time.Minute)},
	}
	sum := Summarize("hermes", calls)

	if sum.Calls != 3 {
		t.Errorf("Calls = %d，期望 3", sum.Calls)
	}
	if sum.NoPath != 1 {
		t.Errorf("NoPath = %d，期望 1", sum.NoPath)
	}
	if sum.Other != 1 {
		t.Errorf("Other = %d，期望 1", sum.Other)
	}
	if len(sum.Changes) != 2 {
		t.Fatalf("Changes 数 = %d，期望 2（%+v）", len(sum.Changes), sum.Changes)
	}

	a := sum.Changes[0] // 改动次数多的排前面
	if a.Path != "G:/projects/hm/a.go" {
		t.Errorf("第一条 Path = %q，期望 G:/projects/hm/a.go", a.Path)
	}
	if a.Count != 2 {
		t.Errorf("a.go Count = %d，期望 2", a.Count)
	}
	if len(a.Ops) != 2 || a.Ops[0] != OpEdit || a.Ops[1] != OpWrite {
		t.Errorf("a.go Ops = %v，期望 [edit write]", a.Ops)
	}
	if len(a.Tools) != 2 || a.Tools[0] != "patch" || a.Tools[1] != "write_file" {
		t.Errorf("a.go Tools = %v，期望 [patch write_file]", a.Tools)
	}
	if a.FirstSeq != 10 || a.LastSeq != 20 {
		t.Errorf("a.go 首末步骤 = %d/%d，期望 10/20", a.FirstSeq, a.LastSeq)
	}
	if !a.FirstTime.Equal(t0) || !a.LastTime.Equal(t0.Add(time.Minute)) {
		t.Errorf("a.go 首末时间 = %v/%v，期望 %v/%v", a.FirstTime, a.LastTime, t0, t0.Add(time.Minute))
	}

	b := sum.Changes[1]
	if b.Path != "G:/projects/hm/b.go" || b.Count != 1 {
		t.Errorf("第二条 = %+v，期望 b.go ×1", b)
	}
}

func TestSummarizeSortsByCountThenPath(t *testing.T) {
	calls := []Call{
		{Tool: "write_file", Args: `{"path":"G:/z.go"}`},
		{Tool: "write_file", Args: `{"path":"G:/a.go"}`},
		{Tool: "write_file", Args: `{"path":"G:/m.go"}`},
		{Tool: "write_file", Args: `{"path":"G:/m.go"}`},
		{Tool: "write_file", Args: `{"path":"G:/a.go"}`},
		{Tool: "write_file", Args: `{"path":"G:/a.go"}`},
	}
	sum := Summarize("hermes", calls)
	want := []string{"G:/a.go", "G:/m.go", "G:/z.go"}
	for i, w := range want {
		if sum.Changes[i].Path != w {
			t.Fatalf("第 %d 条 = %q，期望 %q（全部：%+v）", i, sum.Changes[i].Path, w, sum.Changes)
		}
	}
}

func TestSummarizeEmpty(t *testing.T) {
	sum := Summarize("hermes", nil)
	if len(sum.Changes) != 0 || sum.Calls != 0 || sum.NoPath != 0 || sum.Other != 0 {
		t.Errorf("空输入应得空汇总，实际 %+v", sum)
	}
	// 不是写文件工具的会话：一条改动也没有，但调用要计入 Other（输出据此说「都没改文件」）
	sum = Summarize("hermes", []Call{{Tool: "read_file", Args: `{"path":"G:/a.go"}`}})
	if len(sum.Changes) != 0 || sum.Other != 1 {
		t.Errorf("只读会话应得 0 改动 / Other=1，实际 %+v", sum)
	}
}
