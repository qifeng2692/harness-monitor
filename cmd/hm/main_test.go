package main

import (
	"strings"
	"testing"
)

func TestMatchWindowBasic(t *testing.T) {
	got := matchWindow("hello world this is a long line about proxy settings", "proxy", 10)
	if !strings.Contains(got, "proxy") {
		t.Fatalf("窗口应包含命中词，got %q", got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("命中点之前有内容时应以 … 开头，got %q", got)
	}
}

func TestMatchWindowCJK(t *testing.T) {
	// 中文按 rune 截窗，不能切出乱码字节。
	line := "这一步把采集器的水位写进数据库，然后继续增量采集后面的会话内容"
	got := matchWindow(line, "水位", 6)
	if !strings.Contains(got, "水位") {
		t.Fatalf("窗口应包含命中词，got %q", got)
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatalf("窗口不应出现替换符（说明按字节切开了 rune），got %q", got)
	}
}

func TestMatchWindowCRLF(t *testing.T) {
	// Windows 内容按 \r\n 分行：命中行首时 rune 计数不能把 \r 算进窗口宽度。
	content := "first line\r\nneedle at line start\r\nthird"
	got := matchWindow(content, "needle", 5)
	if !strings.Contains(got, "needle") {
		t.Fatalf("窗口应包含命中词，got %q", got)
	}
}

func TestMatchWindowNoHit(t *testing.T) {
	// 命中在 title 或大小写不同时（SQL LIKE 的匹配面更宽），内容里找不到——退回首部截断。
	got := matchWindow("plain content here", "NEEDED", 10)
	if got == "" {
		t.Fatal("找不到命中时应退回首部截断而不是空串")
	}
	if strings.Contains(got, "NEEDED") {
		t.Fatalf("内容里没有这个词，不应出现，got %q", got)
	}
}

func TestMatchWindowContextZero(t *testing.T) {
	got := matchWindow(strings.Repeat("x", 300), "y", 0)
	if len([]rune(got)) > 161 {
		t.Fatalf("context=0 应回退到 160 字符截断，got %d rune", len([]rune(got)))
	}
}
