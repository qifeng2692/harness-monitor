package export

import (
	"strings"
	"testing"
	"time"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

func TestRedactUserPaths(t *testing.T) {
	cases := []struct{ in, want string }{
		{`打开 C:\Users\QF\projects\a.go 查看`, `打开 ` + phUserPath + ` 查看`},
		{`see C:/Users/QF/AppData/x`, `see ` + phUserPath},
		{`json 转义 C:\\Users\\QF\\Desktop\\f.md`, `json 转义 ` + phUserPath},
		{`msys 形态 /c/Users/QF/x`, `msys 形态 ` + phUserPath},
		{`linux /home/alice/f.txt`, `linux ` + phUserPath},
		{`root 目录 /root/.bashrc`, `root 目录 ` + phUserPath},
		{`无用户段 G:\projects\hm\data`, `无用户段 G:\projects\hm\data`},
	}
	for _, c := range cases {
		got, n := Redact(c.in)
		if got != c.want {
			t.Errorf("Redact(%q) = %q, 期望 %q", c.in, got, c.want)
		}
		if strings.Contains(got, phUserPath) && n == 0 {
			t.Errorf("出现占位符但计数为 0：%q", got)
		}
	}
}

func TestRedactEmails(t *testing.T) {
	got, n := Redact("联系 qifeng@aifengxin.com 或 a@b.co")
	if strings.Contains(got, "@") {
		t.Errorf("邮箱应被替换：%q", got)
	}
	if n != 2 {
		t.Errorf("应替换 2 处，实际 %d：%q", n, got)
	}
	if !strings.Contains(got, phEmail) {
		t.Errorf("应含邮箱占位符：%q", got)
	}
}

func TestRedactSecrets(t *testing.T) {
	// 注意：样本 token 一律用拼接构造，源码里不出现完整字面量——
	// 否则 GitHub push protection 会把测试样本当真密钥拦下推送。
	xox := "xox" + "b-123456789-abcdefghijklmnop"
	ghp := "gh" + "p_0123456789abcdefghij0123456789abcdefghij"
	skant := "sk-" + "ant-0123456789abcdef0123456789abcdef"
	skproj := "sk-" + "proj-0123456789abcdef0123456789abcdef"
	akia := "AK" + "IA0123456789ABCDEF"
	pat := "github" + "_pat_0123456789abcdefghij_ABCDEFG"
	cases := []struct{ in string }{
		{`token: ` + ghp},
		{`"api_key": "` + skant + `"`},
		{`password=hunter2secret`},
		{`Authorization: Bearer ` + skproj},
		{akia},
		{xox},
		{pat},
	}
	for _, c := range cases {
		got, n := Redact(c.in)
		if got == c.in {
			t.Errorf("密钥类应被替换：%q", c.in)
		}
		if !strings.Contains(got, phSecret) {
			t.Errorf("应含密钥占位符：in=%q out=%q", c.in, got)
		}
		if n == 0 {
			t.Errorf("出现占位符但计数为 0：%q", got)
		}
	}
}

func TestRedactNoFalsePositiveOnCode(t *testing.T) {
	// 普通代码/路径不应被误伤。
	cases := []string{
		`go build ./...`,
		`G:\projects\harness-monitor\go.mod`,
		`the output shows users table`,
		`export HTTPS_PROXY=http://127.0.0.1:7890`,
	}
	for _, in := range cases {
		got, _ := Redact(in)
		if got != in {
			t.Errorf("不应被误伤：%q → %q", in, got)
		}
	}
}

func TestSessionRedactFlow(t *testing.T) {
	// 端到端：Session 默认开启脱敏，路径/邮箱在导出里变占位符并计数；
	// SessionOpts 关闭脱敏时原文保留。
	events := []model.Event{
		mk(1, model.KindToolCall, "read file", `打开 C:\Users\QF\secret.txt，邮箱 qifeng@aifengxin.com`),
	}
	out := Session("hermes", "编辑 C:\\Users\\QF\\a.go", time.Time{}, time.Time{}, events)
	if strings.Contains(out, `C:\Users\QF`) || strings.Contains(out, "qifeng@aifengxin.com") {
		t.Errorf("默认导出应已脱敏：\n%s", out)
	}
	if !strings.Contains(out, phUserPath) || !strings.Contains(out, phEmail) {
		t.Errorf("默认导出应含占位符：\n%s", out)
	}
	if !strings.Contains(out, "本次替换 3 处") {
		t.Errorf("头部应汇总替换处数（内容 2 + 标题 1）：\n%s", out)
	}
	if !strings.Contains(out, "--no-redact") {
		t.Errorf("头部应提示原文导出开关：\n%s", out)
	}

	out2 := SessionOpts("hermes", "标题", time.Time{}, time.Time{}, events, Opts{Redact: false})
	if !strings.Contains(out2, `C:\Users\QF\secret.txt`) {
		t.Errorf("--no-redact 应保留原文：\n%s", out2)
	}
	if strings.Contains(out2, phUserPath) {
		t.Errorf("--no-redact 不应出现占位符：\n%s", out2)
	}
	if strings.Contains(out2, "脱敏：已开启") {
		t.Errorf("--no-redact 头部不应有脱敏行：\n%s", out2)
	}
}

func TestSessionRedactZeroHit(t *testing.T) {
	// 无敏感内容时也要如实标注「未发现需替换内容」。
	events := []model.Event{mk(1, model.KindLLMResponse, "回答", "你好")}
	out := Session("hermes", "干净会话", time.Time{}, time.Time{}, events)
	if !strings.Contains(out, "本会话未发现需替换内容") {
		t.Errorf("零替换时头部说明不准确：\n%s", out)
	}
}
