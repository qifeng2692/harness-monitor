//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// killTree 在类 Unix 上杀掉整个进程组（Setpgid 在 Command 中未设置，
// 退化为杀单进程——本工具的目标平台是 Windows）。
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
}
