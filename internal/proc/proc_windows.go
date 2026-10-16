package proc

import (
	"os"
	"os/exec"
	"strconv"
)

// SetProcessGroup is a no-op on Windows (process groups work differently; pi
// only sets detached on non-win32 and relies on taskkill /T for the tree).
func SetProcessGroup(cmd *exec.Cmd) {}

// KillProcessTree kills the command's entire process tree via taskkill, as
// pi does on win32: `taskkill /F /T /PID <pid>`. /T terminates the process
// and any child processes it started; /F forces termination. The binary is
// named by absolute path (TaskkillPath). pi's `child.once("error", ...)` has
// no Go counterpart: Node reports a failed spawn asynchronously, Run
// returns it, and the fallback handles it.
func KillProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := exec.Command(TaskkillPath(), "/F", "/T", "/PID", strconv.Itoa(pid)).Run(); err != nil {
		// Fall back to killing just the child if taskkill is unavailable.
		return cmd.Process.Kill()
	}
	return nil
}

// ExitSignalNumber always reports "no signal" on Windows: a process there
// ends with an exit code and syscall.WaitStatus carries no signal to read.
// This is the asymmetry that makes upstream skip its signal regression test
// on win32 (c2d3dc55b).
func ExitSignalNumber(*os.ProcessState) (int, bool) { return 0, false }
