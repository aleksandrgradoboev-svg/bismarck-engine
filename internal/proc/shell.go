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

// absoluteWindowsDir is a drive-absolute Windows directory.
var absoluteWindowsDir = regexp.MustCompile(`(?i)^[a-z]:[\\/]`)

// IsLegacyWSLBashPath is pi's isLegacyWslBashPath (upstream 61060ddf): the
// launcher in System32 or Sysnative under SystemRoot, the actual Windows
// directory; only when SystemRoot is unset or empty, under the conventional
// X:\Windows. A SystemRoot that is not drive-absolute matches nothing.
func IsLegacyWSLBashPath(path string) bool {
	normalized := strings.ToLower(WinNormalize(path))
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		if !absoluteWindowsDir.MatchString(systemRoot) {
			return false
		}
		for _, directory := range []string{"System32", "Sysnative"} {
			if normalized == strings.ToLower(WinJoin(systemRoot, directory, "bash.exe")) {
				return true
			}
		}
		return false
	}
	return legacyWSLBash.MatchString(normalized)
}

// WinNormalize is node's path.win32.normalize for the drive-absolute and
// relative paths the shell code compares, on any OS: separators become
// backslashes, repeated ones collapse, "." segments drop, ".." removes the
// segment before it but never the drive, and a trailing separator stays.
func WinNormalize(path string) string {
	if path == "" {
		return "."
	}
	path = strings.ReplaceAll(path, "/", `\`)
	prefix := ""
	if len(path) >= 2 && path[1] == ':' {
		prefix, path = path[:2], path[2:]
	}
	absolute := strings.HasPrefix(path, `\`)
	trailing := strings.HasSuffix(path, `\`)
	var segments []string
	for _, segment := range strings.Split(path, `\`) {
		switch segment {
		case "", ".":
		case "..":
			if len(segments) > 0 && segments[len(segments)-1] != ".." {
				segments = segments[:len(segments)-1]
			} else if !absolute {
				segments = append(segments, segment)
			}
		default:
			segments = append(segments, segment)
		}
	}
	out := strings.Join(segments, `\`)
	if absolute {
		out = `\` + out
	}
	if trailing && out != "" && !strings.HasSuffix(out, `\`) {
		out += `\`
	}
	if out == "" && prefix == "" {
		return "."
	}
	return prefix + out
}

// WinJoin is node's path.win32.join: the non-empty parts joined with
// backslashes, then normalized.
func WinJoin(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return "."
	}
	return WinNormalize(strings.Join(kept, `\`))
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
// process tree does not depend on PATH, porting upstream 7af2d27dc (pi#6596)
// as amended by 61060ddf (pi#9490):
//
//	join(process.env.SystemRoot || "C:\\Windows", "System32", "taskkill.exe")
//
// It has no build constraint so the path arithmetic is testable everywhere.
// `||` falls back for an absent and for an empty SystemRoot alike, so the
// path is always absolute.
func TaskkillPath() string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return filepath.Join(systemRoot, "System32", "taskkill.exe")
}
