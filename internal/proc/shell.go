// Package proc holds what the engine's two command runners share — the
// coding tools' and durable's environment: pi's shell resolution, process
// groups, killing a process tree and reading a terminating signal.
package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// legacyWSLBash matches the legacy Windows-bundled WSL launcher
// (C:\Windows\System32\bash.exe, or the WoW64 sysnative redirect) after path
// normalization. It mishandles `-c "<cmd>"` quoting, so commands go via stdin.
var legacyWSLBash = regexp.MustCompile(`^[a-z]:\\windows\\(?:system32|sysnative)\\bash\.exe$`)

// IsLegacyWSLBashPath is pi's isLegacyWslBashPath.
func IsLegacyWSLBashPath(path string) bool {
	return legacyWSLBash.MatchString(strings.ToLower(strings.ReplaceAll(path, "/", `\`)))
}

// Shell is how commands run: Shell with Args and the command appended, or,
// for legacy WSL bash, with the command on stdin.
type Shell struct {
	Path  string
	Args  []string
	Stdin bool
}

// BashShell is pi's getBashShellConfig: legacy WSL bash takes the command on
// stdin (`bash -s`); every other bash takes `bash -c <command>`.
func BashShell(path string) Shell {
	if IsLegacyWSLBashPath(path) {
		return Shell{Path: path, Args: []string{"-s"}, Stdin: true}
	}
	return Shell{Path: path, Args: []string{"-c"}}
}

// ErrCustomShellMissing is a configured shell path that does not exist.
var ErrCustomShellMissing = errors.New("Custom shell path not found")

// ResolveShell is pi's getShellConfig. pi never uses $SHELL or cmd: a
// configured path must exist; Unix resolves /bin/bash, then bash on PATH,
// then sh; Windows resolves Git Bash in its install locations, then bash.exe
// on PATH, else fails with a message that ends with hint, the third way to
// get a shell. exists reports whether a path exists.
func ResolveShell(custom string, exists func(string) bool, hint string) (Shell, error) {
	if custom != "" {
		if exists(custom) {
			return BashShell(custom), nil
		}
		return Shell{}, fmt.Errorf("%w: %s", ErrCustomShellMissing, custom)
	}
	if runtime.GOOS == "windows" {
		var candidates []string
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			candidates = append(candidates, pf+`\Git\bin\bash.exe`)
		}
		if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
			candidates = append(candidates, pf86+`\Git\bin\bash.exe`)
		}
		for _, p := range candidates {
			if exists(p) {
				return BashShell(p), nil
			}
		}
		if p, err := exec.LookPath("bash.exe"); err == nil && p != "" {
			return BashShell(p), nil
		}
		var b strings.Builder
		b.WriteString("No bash shell found. Options:\n")
		b.WriteString("  1. Install Git for Windows: https://git-scm.com/download/win\n")
		b.WriteString("  2. Add your bash to PATH (Cygwin, MSYS2, etc.)\n")
		b.WriteString("  3. " + hint + "\n\n")
		b.WriteString("Searched Git Bash in:\n")
		for i, p := range candidates {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("  " + p)
		}
		return Shell{}, errors.New(b.String())
	}
	if exists("/bin/bash") {
		return BashShell("/bin/bash"), nil
	}
	if p, err := exec.LookPath("bash"); err == nil && p != "" {
		return BashShell(p), nil
	}
	return Shell{Path: "sh", Args: []string{"-c"}}, nil
}

// TaskkillPath resolves Windows' taskkill.exe by absolute path so killing a
// process tree does not depend on PATH, porting upstream 7af2d27dc (pi#6596):
//
//	join(process.env.SystemRoot ?? "C:\\Windows", "System32", "taskkill.exe")
//
// It has no build constraint so the path arithmetic is testable everywhere.
// LookupEnv rather than Getenv reproduces `??`: only an absent SystemRoot
// takes the fallback, while a present-but-empty one is kept and join() drops
// the empty leading segment, as pi does.
func TaskkillPath() string {
	systemRoot, ok := os.LookupEnv("SystemRoot")
	if !ok {
		systemRoot = `C:\Windows`
	}
	return filepath.Join(systemRoot, "System32", "taskkill.exe")
}
