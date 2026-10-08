//go:build linux

package server

import (
	"os/exec"
	"syscall"
)

// withPdeathsig 让子进程在 server（父进程）死亡时收到 SIGTERM——
// Linux 专属能力；崩溃场景下转发通道不会变孤儿。
func withPdeathsig(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

// withSetsid 让子进程进入独立会话（弹出窗口不受 server 停止/重启牵连）。
func withSetsid(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
