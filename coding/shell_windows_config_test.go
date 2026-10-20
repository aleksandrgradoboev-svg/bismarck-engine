package coding

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// Ported from pi packages/coding-agent/test/windows-shell-config.test.ts and
// powershell-config.test.ts (upstream 61060ddf, pi#9490): shells found from
// the actual Windows and installation directories, not assumed drives.

// TestLegacyWslBashUnderSystemRoot identifies the legacy WSL launcher under
// SystemRoot; only an unset one falls back to the conventional X:\Windows.
func TestLegacyWslBashUnderSystemRoot(t *testing.T) {
	for _, c := range []struct {
		systemRoot *string
		shell      string
		stdin      bool
	}{
		{ptrTo(`D:\WinNT\`), `d:/WINNT/System32/BASH.EXE`, true},
		{ptrTo(`D:\WinNT`), `D:\WinNT\Sysnative\bash.exe`, true},
		{nil, `E:\Windows\System32\bash.exe`, true},
		{ptrTo(`D:\WinNT`), `D:\Git\bin\bash.exe`, false},
		{ptrTo(`D:\WinNT`), `E:\Windows\System32\bash.exe`, false},
		{ptrTo(`WinNT`), `C:\Windows\System32\bash.exe`, false},
	} {
		t.Run(c.shell, func(t *testing.T) {
			setSystemRoot(t, c.systemRoot)
			shell, args, stdin := getBashShellConfig(c.shell)
			want := []string{"-c"}
			if c.stdin {
				want = []string{"-s"}
			}
			if shell != c.shell || !slices.Equal(args, want) || stdin != c.stdin {
				t.Fatalf("getBashShellConfig(%q) = %q %v %v, want args %v stdin %v", c.shell, shell, args, stdin, want, c.stdin)
			}
		})
	}
}

func ptrTo(s string) *string { return &s }

// setSystemRoot sets SystemRoot for the test, or unsets it for nil; the
// process's own value is restored on cleanup.
func setSystemRoot(t *testing.T, value *string) {
	t.Helper()
	t.Setenv("SystemRoot", "")
	if value == nil {
		if err := os.Unsetenv("SystemRoot"); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv("SystemRoot", *value)
}

// powerShellHost fakes what powerShellConfig looks up: executables on PATH
// and existing files.
type powerShellHost struct {
	onPath   map[string]string
	existing []string
}

func (h powerShellHost) lookPath(name string) (string, error) {
	if p, ok := h.onPath[name]; ok {
		return p, nil
	}
	return "", errors.New("not found")
}

func (h powerShellHost) exists(path string) bool { return slices.Contains(h.existing, path) }

func (powerShellHost) getenv(name string) string {
	switch name {
	case "ProgramFiles":
		return `D:\Applications`
	case "SystemRoot":
		return `E:\WinNT`
	}
	return ""
}

func TestPowerShellConfigFindsShellsOutsidePathWithoutAssumingADrive(t *testing.T) {
	const pwsh7 = `D:\Applications\PowerShell\7\pwsh.exe`
	const windowsPowerShell = `E:\WinNT\System32\WindowsPowerShell\v1.0\powershell.exe`
	for _, c := range []struct {
		name string
		host powerShellHost
		want string
	}{
		{"prefers PowerShell 7 on PATH over known installation locations",
			powerShellHost{onPath: map[string]string{"pwsh.exe": `E:\Tools\pwsh.exe`}, existing: []string{pwsh7, windowsPowerShell}}, `E:\Tools\pwsh.exe`},
		{"finds PowerShell 7 under ProgramFiles before Windows PowerShell on PATH",
			powerShellHost{onPath: map[string]string{"powershell.exe": windowsPowerShell}, existing: []string{pwsh7, windowsPowerShell}}, pwsh7},
		{"prefers Windows PowerShell on PATH over its system installation",
			powerShellHost{onPath: map[string]string{"powershell.exe": `E:\Tools\powershell.exe`}, existing: []string{`E:\Tools\powershell.exe`, windowsPowerShell}}, `E:\Tools\powershell.exe`},
		{"finds Windows PowerShell under SystemRoot when PATH lookup finds nothing",
			powerShellHost{existing: []string{windowsPowerShell}}, windowsPowerShell},
	} {
		t.Run(c.name, func(t *testing.T) {
			shell, args, stdin, err := powerShellConfig(c.host.lookPath, c.host.exists, c.host.getenv)
			if err != nil || shell != c.want || !slices.Equal(args, powershellArgs) || stdin {
				t.Fatalf("powerShellConfig = %q %v %v %v, want %q", shell, args, stdin, err, c.want)
			}
		})
	}
	_, _, _, err := powerShellConfig(powerShellHost{}.lookPath, powerShellHost{}.exists, powerShellHost{}.getenv)
	if err == nil || !strings.Contains(err.Error(), "No PowerShell executable found") {
		t.Fatalf("no installation: %v", err)
	}
}
