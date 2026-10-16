package coding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/uuidv7"
)

// entryIDRe is the shape of pi's generateId output: randomUUID().slice(0, 8).
var entryIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// readSessionFileRoles mirrors pi's test utility of the same name: one label
// per record in a session file, the message role for message entries and the
// entry type otherwise.
func readSessionFileRoles(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	var roles []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record struct {
			Type    string `json:"type"`
			Message *struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if record.Message != nil && record.Message.Role != "" {
			roles = append(roles, record.Message.Role)
		} else {
			roles = append(roles, record.Type)
		}
	}
	return roles
}

// A session with only setup entries stays in memory: no file, and it is
// invisible to ListSessions (pi _persist, upstream ff72faba2).
func TestSessionWithOnlySetupEntriesAbsentFromDisk(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, &ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"}, "off")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Fatalf("session file should not exist yet, stat err=%v", err)
	}
	if infos := ListSessions(cwd, ""); len(infos) != 0 {
		t.Fatalf("ListSessions should be empty, got %+v", infos)
	}
}

// The first user message creates the file, so the prompt survives a first
// turn that never produces an assistant message (pi #10000).
func TestFirstUserMessageCreatesSessionFile(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, &ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rec.RecordMessage(ai.NewUserText("first question", 1))

	want := []string{"session", "model_change", "user"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
	messages, err := LoadSessionMessages(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("reopened session has %d messages, want 1", len(messages))
	}
	if infos := ListSessions(cwd, ""); len(infos) != 1 {
		t.Fatalf("ListSessions should show 1 session, got %+v", infos)
	}
}

// Characterization: once the file exists, each later entry is appended to it
// once. pi appends a custom entry between the messages; the recorder has no
// custom entries, so a thinking-level change stands in for it.
func TestLaterSessionEntriesAppend(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rec.RecordMessage(ai.NewUserText("first question", 1))
	rec.RecordThinkingLevel("medium")
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "first answer"}}, StopReason: ai.StopStop, Timestamp: 2})

	want := []string{"session", "user", "thinking_level_change", "assistant"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
}

// Only a user or assistant message creates the file: a system message or a
// tool result alone stays in memory like the setup entries, and is written
// with them once the conversation starts (pi _hasConversation).
func TestNonConversationMessagesStayOffDisk(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rec.RecordMessage(ai.NewSystemText("sys", 1))
	rec.RecordMessage(ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: "out"}}, Timestamp: 2})
	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Fatalf("a system message and a tool result must not create the file, stat err=%v", err)
	}

	rec.RecordMessage(ai.NewUserText("first question", 3))
	want := []string{"session", "system", "toolResult", "user"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
}

// Once an entry is on disk the recorder lets go of it: pending holds only the
// entries of a session that has no file yet, for a new session and a resumed
// one alike.
func TestSessionRecorderKeepsNoWrittenEntries(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, &ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"}, "off")
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(ai.NewUserText("first question", 1))
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "first answer"}}, StopReason: ai.StopStop, Timestamp: 2})
	if n := len(rec.pending); n != 0 {
		t.Fatalf("new session recorder still holds %d written entries", n)
	}
	rec.Close()

	resumed, err := ResumeSession(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.RecordMessage(ai.NewUserText("second question", 3))
	if n := len(resumed.pending); n != 0 {
		t.Fatalf("resumed session recorder still holds %d written entries", n)
	}
	want := []string{"session", "model_change", "thinking_level_change", "user", "assistant", "user"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
}

// TestStartSessionReturnsAnErrorWhenNoIDCanBeMinted pins the shape of the
// failure pi has here: uuidv7() throws a catchable RangeError, and StartSession
// already reports its other failures (no agent dir, MkdirAll) as errors. A
// machine whose RTC has been reset to a pre-1970 date must therefore get an
// error naming the fix, not a process abort from inside the id generator.
func TestStartSessionReturnsAnErrorWhenNoIDCanBeMinted(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Cleanup(uuidv7.OverrideClockForTest(func() int64 { return -1 })) // clock before the epoch

	rec, err := StartSession(t.TempDir(), nil)
	if err == nil {
		t.Fatalf("StartSession must return an error when no session id can be minted, got %+v", rec)
	}
	if !strings.Contains(err.Error(), "Unix time in milliseconds") {
		t.Fatalf("the error must say how to resolve it, got %q", err)
	}
}

// TestResumeSessionMigratesAV1FileToV3 is the write-side half of pi's
// migrateToCurrentVersion: SessionManager._loadEntries migrates in memory and,
// when anything changed, rewrites the whole file (_rewriteFile,
// session-manager.ts:961 @ 64eeb82a4) before appending. A resumed v1 file must
// therefore land on disk as a v3 file whose entries carry ids and a parentId
// chain, and the first entry appended after resume must hang off the last
// migrated entry rather than off a null parent.
func TestResumeSessionMigratesAV1FileToV3(t *testing.T) {
	path := writeSessionLines(t, []string{
		`{"type":"session","id":"0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"q"}],"timestamp":1}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"hookMessage","customType":"x","content":[{"type":"text","text":"hook"}],"timestamp":2}}`,
	})

	rec, err := ResumeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordThinkingLevel("medium")
	rec.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("rewritten file has an unparseable line %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 4 {
		t.Fatalf("expected header + 2 migrated + 1 appended entries, got %d:\n%s", len(entries), data)
	}
	if v, _ := entries[0]["version"].(float64); int(v) != CurrentSessionVersion {
		t.Fatalf("rewritten header version = %v, want %d:\n%s", entries[0]["version"], CurrentSessionVersion, data)
	}
	var prev any // nil: the first entry's parentId is null
	for i, e := range entries[1:] {
		id, _ := e["id"].(string)
		if !entryIDRe.MatchString(id) {
			t.Fatalf("entry %d id %v is not a generated 8-hex entry id:\n%s", i, e["id"], data)
		}
		if e["parentId"] != prev {
			t.Fatalf("entry %d parentId = %v, want %v (the previous entry):\n%s", i, e["parentId"], prev, data)
		}
		prev = id
	}
	if role := entries[2]["message"].(map[string]any)["role"]; role != "custom" {
		t.Fatalf("hookMessage role not migrated to custom on disk: %v", role)
	}
	if entries[3]["type"] != "thinking_level_change" {
		t.Fatalf("appended entry missing after the migrated ones: %v", entries[3])
	}
}

// TestSessionIDAndFilenameShape verifies the header id is a uuidv7 (version 7,
// RFC-4122 variant) and the filename is <iso-with-dashes>_<uuidv7>.jsonl.
func TestSessionIDAndFilenameShape(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(rec.ID()) {
		t.Fatalf("header id is not a uuidv7: %q", rec.ID())
	}
	base := filepath.Base(rec.Path())
	if !strings.HasSuffix(base, "_"+rec.ID()+".jsonl") {
		t.Fatalf("filename does not embed the uuidv7 id: %q", base)
	}
	// The timestamp portion contains no ':' or '.' (replaced with '-').
	tsPart := strings.TrimSuffix(base, "_"+rec.ID()+".jsonl")
	if strings.ContainsAny(tsPart, ":.") {
		t.Fatalf("filename timestamp not sanitized: %q", tsPart)
	}
}

// TestEntryIDFallbackIsARandomUUIDv4 pins pi's generateId fallback
// (session-manager.ts:227 @ 64eeb82a4, `return randomUUID()` after 100
// collisions): a version-4, fully random UUID. It is deliberately NOT the
// time-ordered v7 the port mints for session ids — a v7 there would write a
// different version nibble into the session file, consume a sequence slot, and
// give an id-generation path a failure mode randomUUID() does not have.
func TestEntryIDFallbackIsARandomUUIDv4(t *testing.T) {
	v4Re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	everyIDTaken := func(string) bool { return true } // 100 collisions, then the fallback
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id := genEntryID(everyIDTaken)
		if !v4Re.MatchString(id) {
			t.Fatalf("entry-id fallback is not a v4 UUID: %q", id)
		}
		if seen[id] {
			t.Fatalf("entry-id fallback repeated %q: it is not fully random", id)
		}
		seen[id] = true
	}
}

// TestEntryIDsCollisionChecked verifies entry ids are 8 hex chars and unique
// across many recorded entries.
func TestEntryIDsCollisionChecked(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	rec, _ := StartSession(cwd, nil)
	// Force a flush so entries are observable, then read ids back from disk.
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "a"}}, StopReason: ai.StopStop, Timestamp: 1})
	for i := 0; i < 50; i++ {
		rec.RecordThinkingLevel("medium")
	}
	rec.Close()

	data, _ := os.ReadFile(rec.Path())
	ids := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Type == "session" {
			continue // header id is a uuidv7, not an 8-hex entry id
		}
		if !entryIDRe.MatchString(e.ID) {
			t.Fatalf("entry id not 8 hex chars: %q (type %s)", e.ID, e.Type)
		}
		if ids[e.ID] {
			t.Fatalf("duplicate entry id: %q", e.ID)
		}
		ids[e.ID] = true
	}
}

// TestEncodeCwdStripsOneLeadingSeparator verifies the cwd encoding strips
// exactly ONE leading separator (pi replace(/^[/\\]/, "")) rather than all of
// them (the old TrimLeft bug). filepath.Abs collapses consecutive POSIX slashes,
// so this exercises encodeCwdSafePath directly on inputs that retain multiple
// leading separators (e.g. a Windows-style UNC/backslash path).
func TestEncodeCwdStripsOneLeadingSeparator(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b", "a-b"},                         // single leading slash stripped
		{"//double/leading", "-double-leading"}, // only the FIRST of two stripped
		{`\\unc\share`, `-unc-share`},           // backslash UNC: one backslash stripped
		{"/x", "x"},
	}
	for _, c := range cases {
		if got := encodeCwdSafePath(c.in); got != c.want {
			t.Fatalf("encodeCwdSafePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLoadSessionMessagesCompactedResume verifies a JSONL session containing a
// compaction entry resumes with the summary (pi wrapper text) in place of the
// pre-compaction turns, rather than naively replaying every message.
func TestLoadSessionMessagesCompactedResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	userMsg := func(text string) json.RawMessage {
		b, _ := json.Marshal(ai.NewUserText(text, 1))
		return b
	}
	asstMsg := func(text string) json.RawMessage {
		b, _ := json.Marshal(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, StopReason: ai.StopStop, Timestamp: 1})
		return b
	}

	lines := []map[string]any{
		{"type": "session", "version": CurrentSessionVersion, "id": "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000", "timestamp": "2026-06-08T00:00:00.000Z", "cwd": dir, "id_": ""},
		{"type": "message", "id": "aaaaaaaa", "parentId": nil, "message": userMsg("old question")},
		{"type": "message", "id": "bbbbbbbb", "parentId": "aaaaaaaa", "message": asstMsg("old answer")},
		{"type": "compaction", "id": "cccccccc", "parentId": "bbbbbbbb", "summary": "SUMMARY OF OLD WORK", "firstKeptEntryId": "dddddddd", "tokensBefore": 1234, "timestamp": "2026-06-08T00:01:00.000Z"},
		{"type": "message", "id": "dddddddd", "parentId": "cccccccc", "message": userMsg("recent question")},
		{"type": "message", "id": "eeeeeeee", "parentId": "dddddddd", "message": asstMsg("recent answer")},
	}
	var sb strings.Builder
	for _, l := range lines {
		b, _ := json.Marshal(l)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, err := LoadSessionMessages(path)
	if err != nil {
		t.Fatal(err)
	}

	// First reconstructed message must be the compaction summary (pi wrapper).
	if len(msgs) == 0 {
		t.Fatal("no messages reconstructed")
	}
	first, ok := msgs[0].(ai.UserMessage)
	if !ok {
		t.Fatalf("first message should be the summary user message, got %T", msgs[0])
	}
	if !strings.Contains(textOf(first.Content), "SUMMARY OF OLD WORK") {
		t.Fatalf("compaction summary missing from resumed context: %q", textOf(first.Content))
	}

	all := ""
	for _, m := range msgs {
		switch v := m.(type) {
		case ai.UserMessage:
			all += textOf(v.Content) + "\n"
		case *ai.AssistantMessage:
			for _, c := range v.Content {
				if tc, ok := c.(ai.TextContent); ok {
					all += tc.Text + "\n"
				}
			}
		case ai.AssistantMessage:
			for _, c := range v.Content {
				if tc, ok := c.(ai.TextContent); ok {
					all += tc.Text + "\n"
				}
			}
		}
	}
	// Pre-compaction turns must NOT be replayed.
	if strings.Contains(all, "old question") || strings.Contains(all, "old answer") {
		t.Fatalf("pre-compaction turns leaked into resumed context:\n%s", all)
	}
	// Recent turns must be present.
	if !strings.Contains(all, "recent question") || !strings.Contains(all, "recent answer") {
		t.Fatalf("recent turns missing from resumed context:\n%s", all)
	}
}

var _ = agent.ThinkMedium
