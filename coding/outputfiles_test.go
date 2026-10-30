package coding

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Output files can carry private data: pi creates every one with mode 0600
// and "wx" (utils/output-files.ts, d677d0ee), the bash tool's full output
// included. Windows has no such permission bits for os.FileMode to report.
func TestOutputFilesAreReadableOnlyByTheUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are POSIX")
	}
	t.Setenv("TMPDIR", t.TempDir())

	written, err := WriteOutputFile("pi-test", ".txt", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	acc := newOutputAccumulator(1, 1<<20, "pi-bash")
	acc.append([]byte("one\ntwo\nthree\n"))
	acc.finish()
	streamed := acc.snapshot(true).fullOutputPath
	if err := acc.closeTempFile(); err != nil {
		t.Fatal(err)
	}
	if streamed == "" {
		t.Fatal("the truncated output has no full output file")
	}
	for _, path := range []string{written, streamed} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(path), perm)
		}
	}
	if base := filepath.Base(streamed); !strings.HasPrefix(base, "pi-bash-") || !strings.HasSuffix(base, ".log") {
		t.Errorf("full output file %q, want pi-bash-<hex>.log", base)
	}
}
