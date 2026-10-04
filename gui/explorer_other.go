// explorer_other.go：非 Windows 平台的回退实现（编译用）。
//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

func openExplorer(dir string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", dir).Start()
	default:
		return exec.Command("xdg-open", dir).Start()
	}
}
