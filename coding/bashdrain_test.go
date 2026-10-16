package coding

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowSink takes its time over every write, as a busy accumulator does on a
// loaded machine.
type slowSink struct {
	mu    sync.Mutex
	delay time.Duration
	data  []byte
}

func (s *slowSink) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append(s.data, p...)
	return len(p), nil
}

// TestBashDrainHelperProcess is the child of
// TestBashDrainKeepsOutputWhileTheSinkIsSlow, not a test of its own.
func TestBashDrainHelperProcess(t *testing.T) {
	if os.Getenv("BASH_DRAIN_HELPER") != "1" {
		t.Skip("helper process")
	}
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(os.Stdout, "%d\n", i)
	}
	os.Exit(0)
}

// The grace after exit releases a pipe nobody writes to, not one whose
// output the sink is still taking: time spent writing a chunk is not idle.
func TestBashDrainKeepsOutputWhileTheSinkIsSlow(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestBashDrainHelperProcess$")
	cmd.Env = append(os.Environ(), "BASH_DRAIN_HELPER=1")
	sink := &slowSink{delay: 3 * bashExitStdioGrace / 2}
	if err := runBashCommand(cmd, sink); err != nil {
		t.Fatal(err)
	}
	out := string(sink.data)
	if !strings.HasPrefix(out, "1\n2\n") || !strings.HasSuffix(out, "\n20000\n") {
		t.Fatalf("lost output: got %d bytes ending %q", len(out), out[max(0, len(out)-20):])
	}
}
