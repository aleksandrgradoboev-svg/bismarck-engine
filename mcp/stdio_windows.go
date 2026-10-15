//go:build windows

package mcp

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcessGroup hides the server's console window. Windows has no process
// groups to signal; killProcessTree walks the tree with taskkill instead.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// killProcessTree terminates the server and its children. Windows has no
// graceful signals, and a .cmd shim runs through cmd.exe, so killing only the
// direct child would leave the server running.
func killProcessTree(cmd *exec.Cmd, _ bool) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/pid", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = kill.Run()
}

// killLeftovers is a no-op on Windows: once the server has exited, taskkill
// /T can no longer find its children by walking from it.
func killLeftovers(*exec.Cmd) {}
