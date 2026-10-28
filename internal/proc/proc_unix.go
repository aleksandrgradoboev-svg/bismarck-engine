//go:build !windows

package proc

import (
	"os"
	"os/exec"
	"runtime"
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
	return killProcessTree(cmd, false)
}

// KillShellProcessTree is KillProcessTree as pi's coding agent kills a shell
// (utils/shell.ts, 27cc6bfc, #11252): on macOS the group is stopped first.
// macOS signals group members one by one after releasing the group lock, so
// a killed child can wake its waiting shell before the shell is signalled,
// and `a; b` starts b during cancellation; SIGSTOP narrows that race (it is
// not an atomic freeze), and SIGKILL also kills stopped processes. Linux holds
// the task list lock across group signalling, so it is unchanged. pi's durable
// environment does not stop first, so durable keeps KillProcessTree.
func KillShellProcessTree(cmd *exec.Cmd) error {
	return killProcessTree(cmd, runtime.GOOS == "darwin")
}

func killProcessTree(cmd *exec.Cmd, stopFirst bool) error {
	if cmd.Process == nil {
		return nil
	}
	return signalProcessTree(cmd.Process.Pid, stopFirst, syscall.Kill)
}

// signalProcessTree sends pi killProcessTree's signals through kill: SIGSTOP
// to the group when stopFirst, SIGKILL to the group, and SIGKILL to the leader
// alone if the group kill fails. It returns the fallback's error.
func signalProcessTree(pid int, stopFirst bool, kill func(pid int, sig syscall.Signal) error) error {
	if stopFirst {
		// A failed stop must not prevent the kill or its fallback.
		_ = kill(-pid, syscall.SIGSTOP)
	}
	if err := kill(-pid, syscall.SIGKILL); err != nil {
		// Fall back to killing just the child if the group is gone.
		return kill(pid, syscall.SIGKILL)
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
