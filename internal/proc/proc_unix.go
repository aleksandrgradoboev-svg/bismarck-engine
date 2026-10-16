//go:build !windows

package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// SetProcessGroup puts the command in its own process group so the whole tree
// (including backgrounded grandchildren) can be signalled together.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillProcessTree kills the command's entire process group (pi's
// killProcessTree). The child is the group leader, so signalling -pid reaches
// every descendant, not just the direct child.
func KillProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// Fall back to killing just the child if the group is gone.
		return cmd.Process.Kill()
	}
	return nil
}

// ExitSignalNumber reports the number of the signal that terminated the
// process, standing in for the signal name Node passes to a child's "exit"
// event alongside a null exit code. It lives in a build-tagged file because
// syscall.WaitStatus is a different type per platform.
func ExitSignalNumber(state *os.ProcessState) (int, bool) {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	return int(status.Signal()), true
}
