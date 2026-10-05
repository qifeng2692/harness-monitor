//go:build windows

package runner

import (
	"os/exec"
	"strconv"
)

// killTree 在 Windows 上杀掉整棵进程树。
//
// codex / pi 是 npm 的 .cmd 包装器，cmd.Process.Kill() 只杀 cmd.exe 外壳，
// 真正干活的 node 子进程会变成孤儿继续跑（占用网关配额、锁住工作目录）。
// taskkill /T 按进程树终止，是本机唯一可靠的杀法。
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// 先尝试整树；失败再退回单进程 kill（进程可能已退出）。
	if err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
