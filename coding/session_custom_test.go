package coding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

func customStrings(raws []json.RawMessage) []string {
	out := make([]string, len(raws))
	for i, raw := range raws {
		out[i] = string(raw)
	}
	return out
}

func userMessage(text string) agent.AgentMessage {
	return ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: text}}}
}

// Custom entries are read off the current branch, root first, filtered by
// type; a fork moves the branch (pi appendCustomEntry + getBranch).
func TestRecorderCustomEntriesFollowTheBranch(t *testing.T) {
	setTestHome(t, t.TempDir())
	rec, err := StartSession(t.TempDir(), &ai.Model{ID: "m", Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	prompt := rec.RecordMessage(userMessage("go"))
	rec.AppendCustomEntry("mine", map[string]int{"n": 1})
	rec.AppendCustomEntry("other-extension", map[string]int{"n": 9})
	rec.AppendCustomEntry("mine", map[string]int{"n": 2})
	if got, want := customStrings(rec.CustomEntries("mine")), []string{`{"n":1}`, `{"n":2}`}; !slices.Equal(got, want) {
		t.Fatalf("entries %q, want %q", got, want)
	}

	rec.ForkFrom(prompt)
	rec.AppendCustomEntry("mine", map[string]int{"n": 3})
	if got, want := customStrings(rec.CustomEntries("mine")), []string{`{"n":3}`}; !slices.Equal(got, want) {
		t.Fatalf("entries on the fork %q, want %q", got, want)
	}
	if got := rec.CustomEntries("absent"); len(got) != 0 {
		t.Fatalf("absent type gave %q", customStrings(got))
	}
}

// A custom entry without data writes no data field, as JSON.stringify drops
// an undefined one, and it reads back as nil.
func TestRecorderCustomEntryWithoutData(t *testing.T) {
	setTestHome(t, t.TempDir())
	rec, err := StartSession(t.TempDir(), &ai.Model{ID: "m", Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(userMessage("go"))
	rec.AppendCustomEntry("mine", nil)
	path := rec.Path()
	rec.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, `"customType":"mine"`) && strings.Contains(line, `"data"`) {
			t.Fatalf("data field written: %s", line)
		}
	}
	if got := rec.CustomEntries("mine"); len(got) != 1 || got[0] != nil {
		t.Fatalf("entries %q, want one without data", customStrings(got))
	}
}

// Resuming reads the custom entries back from the file, keeping each data
// object's key order as written, which decoding into a map would sort.
func TestRecorderResumeKeepsCustomEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"sid","timestamp":"2026-10-16T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"message","id":"a1","parentId":null,"timestamp":"2026-10-16T00:00:01.000Z","message":{"role":"user","content":"go","timestamp":1}}`,
		`{"type":"custom","customType":"mine","data":{"z":1,"a":2},"id":"a2","parentId":"a1","timestamp":"2026-10-16T00:00:02.000Z"}`,
		`{"type":"custom","customType":"mine","data":{"n":2},"id":"b2","parentId":"a1","timestamp":"2026-10-16T00:00:03.000Z"}`,
		`{"type":"custom","customType":"mine","data":{"n":3},"id":"a3","parentId":"a2","timestamp":"2026-10-16T00:00:04.000Z"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := ResumeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	// The leaf is the last entry, a3: its branch runs a1 → a2 → a3, not b2.
	if got, want := customStrings(rec.CustomEntries("mine")), []string{`{"z":1,"a":2}`, `{"n":3}`}; !slices.Equal(got, want) {
		t.Fatalf("entries %q, want %q", got, want)
	}
	rec.ForkFrom("b2")
	if got, want := customStrings(rec.CustomEntries("mine")), []string{`{"n":2}`}; !slices.Equal(got, want) {
		t.Fatalf("entries from b2 %q, want %q", got, want)
	}
}

// A parent cycle in a damaged file ends the walk instead of hanging it.
func TestRecorderCustomEntriesStopOnACycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"sid","timestamp":"2026-10-16T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"custom","customType":"mine","data":{"n":1},"id":"x","parentId":"y","timestamp":"2026-10-16T00:00:01.000Z"}`,
		`{"type":"custom","customType":"mine","data":{"n":2},"id":"y","parentId":"x","timestamp":"2026-10-16T00:00:02.000Z"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := ResumeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if got := rec.CustomEntries("mine"); len(got) > 3 {
		t.Fatalf("walk did not stop: %d entries", len(got))
	}
}

// Session reports whether a recorder is attached, so codemode can fall back
// to its in-memory store for an unrecorded session.
func TestSessionCustomEntriesNeedARecorder(t *testing.T) {
	s := NewSession(SessionOptions{Model: &ai.Model{ID: "m", Provider: "p"}, Cwd: t.TempDir(), NoContextFiles: true, NoSkills: true})
	if _, ok := s.CustomEntries("mine"); ok {
		t.Fatal("ok without a recorder")
	}
	if _, ok := s.AppendCustomEntry("mine", 1); ok {
		t.Fatal("appended without a recorder")
	}
}
