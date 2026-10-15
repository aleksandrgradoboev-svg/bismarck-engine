package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives the package's tests a temp directory of their own, removed
// afterwards. The shell tools save the full output of a truncated command
// there (pi-bash-*.log), and tests must not leave those files in the user's
// temp directory, where pi's own sessions write the same names.
//
// It also gives them an empty home directory (bismarck-engine): skill and
// session discovery read ~/.agents and ~/.pi, so without this the tests see
// whatever the developer's machine has installed there. Both HOME and
// USERPROFILE are set, because os.UserHomeDir reads USERPROFILE on Windows.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pi-coding-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "coding tests: cannot create a temp directory:", err)
		os.Exit(1)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} { // TMP and TEMP are Windows'
		os.Setenv(key, dir)
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "coding tests: cannot create a home directory:", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// setTestHome points the home directory at dir for one test, on every
// platform: HOME for Unix, USERPROFILE for os.UserHomeDir on Windows. An empty
// dir means "no home directory".
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}
