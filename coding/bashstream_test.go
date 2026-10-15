package coding

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// TestBashStreamsPartialOutput verifies bash emits throttled partial output via
// onUpdate before the command finishes (so a host app can show live progress).
func TestBashStreamsPartialOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh sleep")
	}
	var mu sync.Mutex
	var updates []string
	onUpdate := func(r agent.AgentToolResult) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range r.Content {
			if tc, ok := c.(ai.TextContent); ok {
				updates = append(updates, tc.Text)
			}
		}
	}
	// Each line is followed by a sleep longer than the 100ms throttle, so an
	// intermediate update should fire before the final line.
	final, err := bashTool(t.TempDir(), nil).Execute(context.Background(), "id",
		map[string]any{"command": "echo first; sleep 0.2; echo second; sleep 0.2; echo third"},
		onUpdate)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updates) == 0 {
		t.Fatalf("expected at least one streamed partial update, got none")
	}
	// A partial update should have arrived before "third" was printed.
	sawEarly := false
	for _, u := range updates {
		if strings.Contains(u, "first") && !strings.Contains(u, "third") {
			sawEarly = true
		}
	}
	if !sawEarly {
		t.Fatalf("expected an early partial update containing 'first' but not 'third'; updates=%v", updates)
	}
	// Final result still has everything.
	got := resultText(final)
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(got, want) {
			t.Fatalf("final output missing %q: %q", want, got)
		}
	}
}

// TestBashCapturesOutputPastExit ports upstream 3fa40956 (regression for pi#5303):
// a short-lived shell exits immediately, but a detached subshell holds the merged
// stdout pipe open and emits ticks every 50ms — the last well past the 100ms exit
// grace. The fix re-arms the idle grace per chunk, so the tail must still be
// captured rather than truncated at a fixed deadline from exit.
func TestBashCapturesOutputPastExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh detached subshell")
	}
	// printf HEAD, then a backgrounded subshell that keeps the pipe open and
	// emits TICK1..TICK6, the last ~300ms after the parent shell exits.
	command := `printf "HEAD\n"; ( for i in 1 2 3 4 5 6; do sleep 0.05; printf "TICK$i\n"; done ) &`
	final, err := bashTool(t.TempDir(), nil).Execute(context.Background(), "id",
		map[string]any{"command": command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := resultText(final)
	if !strings.Contains(got, "HEAD") {
		t.Fatalf("expected HEAD in output, got %q", got)
	}
	if !strings.Contains(got, "TICK6") {
		t.Fatalf("expected late TICK6 (emitted past the exit grace) to be captured, got %q", got)
	}
}

// TestBashReleasesPromptlyOnQuietHeldPipe ports the second case of 3fa40956: a
// detached sleeper inherits the stdout pipe and holds it open for a long time
// without writing. EOF never arrives, so we must release via the idle grace
// rather than block on the open handle.
func TestBashReleasesPromptlyOnQuietHeldPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh detached subshell")
	}
	command := `printf "DONE\n"; ( sleep 30 ) &`
	start := time.Now()
	final, err := bashTool(t.TempDir(), nil).Execute(context.Background(), "id",
		map[string]any{"command": command}, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	got := resultText(final)
	if !strings.Contains(got, "DONE") {
		t.Fatalf("expected DONE in output, got %q", got)
	}
	// Must not wait for the 30s sleeper; the idle grace releases us well under a second.
	if elapsed > 2*time.Second {
		t.Fatalf("expected prompt release via idle grace, took %s", elapsed)
	}
}
