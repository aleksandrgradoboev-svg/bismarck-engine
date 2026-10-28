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

// killProcessTree kills the command's process tree as pi's coding agent kills
// a shell: on macOS the group is stopped first (proc.KillShellProcessTree).
func killProcessTree(cmd *exec.Cmd) error { return proc.KillShellProcessTree(cmd) }

// taskkillPath is Windows' taskkill.exe by absolute path (proc).
func taskkillPath() string { return proc.TaskkillPath() }

// exitSignalNumber reports the signal that terminated the process (proc).
// Its only caller is exitCode.
func exitSignalNumber(state *os.ProcessState) (int, bool) { return proc.ExitSignalNumber(state) }
