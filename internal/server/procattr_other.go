//go:build darwin

package server

import (
	"os/exec"
	"syscall"
)

// 非 Linux（darwin 等）没有 Pdeathsig 等价物：失去"父死即通知"的
// 快速收场，转发的清理退回 Shutdown/Stop 的主动收割路径（正常停止/
// 重启不受影响；server 崩溃时的孤儿 ssh 由远端 sshd 的
// StreamLocalBindUnlink 自愈机制兜底）。
func withPdeathsig(c *exec.Cmd) {}

// darwin 的 SysProcAttr 有 Setsid，语义与 Linux 相同。
func withSetsid(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
