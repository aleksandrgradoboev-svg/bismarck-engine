//go:build unix

package unix_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server/internal/servertest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server/unix"
)

func newServer(t *testing.T, path string) *server.Server {
	t.Helper()
	srv, err := unix.NewServer(servertest.NewService(), unix.ServerOptions{Path: path})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	return srv
}

func identity(t *testing.T, path string) (uint64, uint64) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("this platform does not expose device and inode numbers")
	}
	return uint64(stat.Dev), uint64(stat.Ino)
}

// A second listener on a live socket is a duplicate, not a stale-socket
// cleanup: it must refuse to start and leave the running one untouched.
func TestLiveSocketIsNeverUnlinked(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	first := newServer(t, path)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	dev, ino := identity(t, path)

	second := newServer(t, path)
	err := second.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected an already-running failure, got %v", err)
	}
	if gotDev, gotIno := identity(t, path); gotDev != dev || gotIno != ino {
		t.Fatal("the running listener's socket was replaced")
	}

	client, err := servertest.Dial(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Hello(protocol.ProtocolVersion); err != nil {
		t.Fatalf("the original listener must still be serving: %v", err)
	}
}

func TestRegularFileAtTheSocketPathIsNeverRemoved(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("do not remove"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	srv := newServer(t, path)
	err := srv.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "non-socket") {
		t.Fatalf("expected a non-socket refusal, got %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "do not remove" {
		t.Fatalf("the file was disturbed: %q, %v", contents, err)
	}
}

func TestNestedParentsPermissionsAndSelfCleanup(t *testing.T) {
	t.Parallel()
	directory, err := os.MkdirTemp("", "pis-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "p", "n", "server.sock")

	srv := newServer(t, path)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("mode = %v, want a socket", info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 600", perm)
	}

	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the socket must be removed on close, got %v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 0 {
		t.Fatalf("the listener left files behind: %v, %v", entries, err)
	}
}

// Shutdown removes the inode this listener bound, not whatever happens to be at
// the path — anything else there belongs to somebody else.
func TestReplacementInodeSurvivesShutdown(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	srv := newServer(t, path)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "replacement" {
		t.Fatalf("the replacement was disturbed: %q, %v", contents, err)
	}
}

func TestStaleSocketIsRemovedBeforeBinding(t *testing.T) {
	t.Parallel()
	path := socketPath(t)

	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Leaving the file behind on close is exactly what a killed process does.
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a stale socket at the path: %v, %v", info, err)
	}

	srv := newServer(t, path)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	client, err := servertest.Dial(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Hello(protocol.ProtocolVersion); err != nil {
		t.Fatalf("hello: %v", err)
	}
}

func TestConcurrentStartsDoNotLeakTheListener(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	srv := newServer(t, path)

	// Exactly one of the two calls wins; the loser must say so rather than
	// binding a second socket at the same path.
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- srv.Start(context.Background()) }()
	}
	successes := 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case !strings.Contains(err.Error(), "start"):
			t.Fatalf("unexpected start error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful starts = %d, want exactly 1", successes)
	}

	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := srv.Addresses(); len(got) != 0 {
		t.Fatalf("addresses = %v, want none", got)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the socket must be gone, got %v", err)
	}
}

// The private bind path is derived from the configured one, so it can overflow
// sockaddr_un even when the configured path fits.
func TestOverlongDerivedBindPathIsRejected(t *testing.T) {
	t.Parallel()
	directory, err := os.MkdirTemp("", "pis-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	// The published name is one byte; the derived one is eleven, so a path that
	// fits with ten bytes to spare does not fit once it is derived.
	filler := 100 - len(directory) - 3
	if filler < 1 {
		t.Skip("the temporary directory is already too long to build this case")
	}
	nested := filepath.Join(directory, strings.Repeat("x", filler), "s")
	if err := unix.ValidateSocketPath(nested, "PiServer Unix socket path"); err != nil {
		t.Skipf("this platform cannot hold the configured path either: %v", err)
	}

	srv := newServer(t, nested)
	err = srv.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "private Unix bind path") {
		t.Fatalf("expected a private bind path failure, got %v", err)
	}
}

func TestValidateSocketPath(t *testing.T) {
	t.Parallel()
	if err := unix.ValidateSocketPath("", "Unix socket path"); err == nil {
		t.Fatal("an empty path must be rejected")
	}
	if err := unix.ValidateSocketPath("/tmp/"+strings.Repeat("x", 512), "Unix socket path"); err == nil {
		t.Fatal("an over-long path must be rejected")
	}
	if err := unix.ValidateSocketPath("/tmp/pi.sock", "Unix socket path"); err != nil {
		t.Fatalf("a short path must be accepted: %v", err)
	}
}
