// explorer_windows.go：Windows 下用资源管理器打开目录。
package main

import "os/exec"

func openExplorer(dir string) error {
	return exec.Command("explorer", dir).Start()
}
