// Command hmd 是 harness-monitor 的常驻守护进程（M2 实时采集，M1 仅占位）。
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "hmd: 守护进程将在 M2 实现（实时通道 + 周期采集）")
	os.Exit(1)
}
