//go:build !windows

package mcp

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// pi skips this on win32: there are no process groups to signal.
func TestStdioTransportKillsStubbornServerAndChildren(t *testing.T) {
	transport := NewStdioTransport(StdioTransportOptions{
		Command:      os.Args[0],
		Env:          map[string]string{"MCP_TEST_SERVER": "stubborn"},
		CloseTimeout: 100 * time.Millisecond,
	})
	c := NewClient(ClientOptions{Implementation: Implementation{Name: "stdio-test", Version: "1.0.0"}})
	if _, err := c.Connect(context.Background(), transport); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`grandchild (\d+)`)
	var grandchild int
	waitFor(t, "grandchild pid", func() bool {
		if m := re.FindStringSubmatch(transport.Stderr()); m != nil {
			grandchild, _ = strconv.Atoi(m[1])
			return true
		}
		return false
	})
	started := time.Now()
	_ = c.Close()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("close took %v", elapsed)
	}
	waitFor(t, "grandchild to die", func() bool {
		return syscall.Kill(grandchild, 0) != nil
	})
}
