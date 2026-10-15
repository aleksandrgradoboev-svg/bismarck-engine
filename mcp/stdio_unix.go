//go:build !windows

package mcp

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the server in its own process group, so closing the
// transport can terminate the server's children too.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree signals the server's whole process group (SIGTERM, or
// SIGKILL when kill is set), falling back to the server alone.
func killProcessTree(cmd *exec.Cmd, kill bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}

// killLeftovers terminates what is left of the server's process group after
// the server itself exited.
func killLeftovers(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}
