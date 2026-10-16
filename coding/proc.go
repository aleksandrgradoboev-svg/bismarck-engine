package coding

import (
	"os"
	"os/exec"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/proc"
)

// The process helpers live in internal/proc, shared with durable's
// environment; these keep the names the coding tools and their tests use.

// setProcessGroup puts the command in its own process group (proc).
func setProcessGroup(cmd *exec.Cmd) { proc.SetProcessGroup(cmd) }

// killProcessTree kills the command's process tree (proc).
func killProcessTree(cmd *exec.Cmd) error { return proc.KillProcessTree(cmd) }

// taskkillPath is Windows' taskkill.exe by absolute path (proc).
func taskkillPath() string { return proc.TaskkillPath() }

// exitSignalNumber reports the signal that terminated the process (proc).
// Its only caller is exitCode.
func exitSignalNumber(state *os.ProcessState) (int, bool) { return proc.ExitSignalNumber(state) }
