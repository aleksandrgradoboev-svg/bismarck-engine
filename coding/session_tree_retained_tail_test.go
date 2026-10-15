package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

func userText(m interface{ MessageRole() ai.Role }) string {
	um, ok := m.(ai.UserMessage)
	if !ok {
		return "<non-user>"
	}
	var b strings.Builder
	for _, c := range um.Content {
		if tc, ok := c.(ai.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestBuildContextRetainedTail locks pi's retainedTail reconstruction (upstream
// 9e7582aa): when a compaction entry carries an inlined retainedTail, the context
// is the summary followed by the retained messages — the firstKeptEntryId walk of
// pre-compaction entries is skipped entirely.
func TestBuildContextRetainedTail(t *testing.T) {
	lines := []string{
		`{"type":"session","id":"s1","cwd":"/tmp","timestamp":"2026-07-21T00:00:00Z"}`,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2026-07-21T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"old-pre-compaction"}]}}`,
		`{"type":"compaction","id":"e2","parentId":"e1","timestamp":"2026-07-21T00:00:02Z","summary":"SUM","firstKeptEntryId":"e1","retainedTail":[{"role":"user","content":[{"type":"text","text":"kept-A"}]},{"role":"user","content":[{"type":"text","text":"kept-B"}]}]}`,
		`{"type":"message","id":"e3","parentId":"e2","timestamp":"2026-07-21T00:00:03Z","message":{"role":"user","content":[{"type":"text","text":"after-compaction"}]}}`,
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := LoadSessionTree(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := tree.BuildContext("e3")
	var got []string
	for _, m := range ctx.Messages {
		got = append(got, userText(m))
	}
	want := []string{
		compactionSummaryPrefix + "SUM" + compactionSummarySuffix,
		"kept-A",
		"kept-B",
		"after-compaction",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// The pre-compaction entry must NOT be walked when retainedTail is present.
	for _, g := range got {
		if strings.Contains(g, "old-pre-compaction") {
			t.Errorf("retainedTail present but firstKeptEntryId walk still emitted the pre-compaction entry")
		}
	}
}
