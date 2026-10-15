package coding

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// writeUnterminatedSession writes a real session file (header + entries) and
// then strips its trailing newline, reproducing the crash-interrupted files of
// pi issue #8345.
func writeUnterminatedSession(t *testing.T, cwd string) string {
	t.Helper()
	rec, err := StartSession(cwd, &ai.Model{ID: "m", Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(ai.NewUserText("first question", 1))
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "first reply"}}, StopReason: ai.StopStop, Timestamp: 2})
	rec.Close()
	path := rec.Path()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSuffix(string(data), "\n")
	if trimmed == string(data) {
		t.Fatalf("session file was expected to end with a newline:\n%s", data)
	}
	if err := os.WriteFile(path, []byte(trimmed), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// pi 0b5ee5d8b: resuming a session whose last line lacks a trailing newline must
// terminate that line before appending, otherwise the next entry fuses onto the
// unterminated tail and both entries are lost to every reader.
func TestResumeSessionRepairsUnterminatedTail(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	path := writeUnterminatedSession(t, cwd)

	rec, err := ResumeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(ai.NewUserText("second question", 3))
	rec.Close()

	// session, model_change, message(user), message(assistant), message(user).
	types := sessionEntryTypes(t, path)
	want := []string{"session", "model_change", "message", "message", "message"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		data, _ := os.ReadFile(path)
		t.Fatalf("entry types = %v, want %v; file:\n%s", types, want, data)
	}
	// Every line must still be exactly one JSON object; a fused pair is not.
	for _, line := range strings.Split(strings.TrimSuffix(readFileString(t, path), "\n"), "\n") {
		var one map[string]any
		if err := json.Unmarshal([]byte(line), &one); err != nil {
			t.Fatalf("line is not a single JSON entry (%v): %q", err, line)
		}
	}
	// The appended entry must be visible to the tree reader too.
	msgs, err := LoadSessionMessages(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after resume, got %d", len(msgs))
	}
}

// The repair must not double-append: a file that already ends in a newline is
// left byte-identical by a resume that writes nothing.
func TestResumeSessionKeepsTerminatedFileIntact(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	rec, err := StartSession(cwd, &ai.Model{ID: "m", Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}, StopReason: ai.StopStop, Timestamp: 1})
	rec.Close()
	path := rec.Path()
	before := readFileString(t, path)

	rec2, err := ResumeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	rec2.Close()

	if after := readFileString(t, path); after != before {
		t.Fatalf("resume modified an already-terminated file:\nbefore:\n%q\nafter:\n%q", before, after)
	}
}

// Upstream validates the session header BEFORE repairing, and returns early for
// empty or non-session files: a file that is not a pi session is never written
// to. In the port the same early return is the "not a pi session file" error.
func TestResumeSessionDoesNotRepairInvalidFiles(t *testing.T) {
	setTestHome(t, t.TempDir())
	cases := map[string]string{
		"empty":          "",
		"no header":      `{"type":"message","id":"1"}`,
		"blank tail":     "\n  ",
		"malformed only": "not json",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := t.TempDir() + "/session.jsonl"
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ResumeSession(path); err == nil {
				t.Fatalf("expected ResumeSession to reject a non-session file")
			}
			if got := readFileString(t, path); got != content {
				t.Fatalf("non-session file was modified: %q, want %q", got, content)
			}
		})
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
