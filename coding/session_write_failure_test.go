package coding

import (
	"errors"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// pi's file-operations.test.ts cases for 807a5c05 (#10166, #10661, #11460): an
// entry that fails to persist is not kept, so later entries chain to the last
// one written. pi makes the writes fail with file permissions; a recorder
// appends through a file it holds open, which a read-only mode does not stop
// (and on Windows a read-only directory does not stop creating a file), so the
// failures are injected: a closed file, a create that errors.

func assistantText(text string, ts int64) *ai.AssistantMessage {
	return &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, StopReason: ai.StopStop, Timestamp: ts}
}

func branchIDs(t *testing.T, path string) []string {
	t.Helper()
	tree, err := LoadSessionTree(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range tree.Branch() {
		ids = append(ids, e.ID)
	}
	return ids
}

func TestRecorderDoesNotKeepAnEntryWhenWritingItFails(t *testing.T) {
	rec, err := StartSessionIn(t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	firstID := rec.RecordMessage(ai.NewUserText("first question", 1))
	if firstID == "" || rec.Err() != nil {
		t.Fatalf("first: id %q, err %v", firstID, rec.Err())
	}

	rec.file.Close()
	if id := rec.RecordMessage(assistantText("lost answer", 2)); id != "" {
		t.Fatalf("a failed write returned id %q", id)
	}
	if rec.Err() == nil {
		t.Fatal("Err is nil after a failed write")
	}
	if got := rec.LastEntryID(); got != firstID {
		t.Fatalf("leaf %q, want %q", got, firstID)
	}
	if len(rec.byID) != 1 || rec.byID[firstID] == nil {
		t.Fatalf("entries %v, want only %s", slices.Collect(maps.Keys(rec.byID)), firstID)
	}

	f, err := os.OpenFile(rec.Path(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rec.file = f
	secondID := rec.RecordMessage(assistantText("second answer", 3))
	if secondID == "" {
		t.Fatalf("second: %v", rec.Err())
	}
	if parent := rec.byID[secondID].parentID; parent != firstID {
		t.Fatalf("second's parent %q, want %q", parent, firstID)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if got := branchIDs(t, rec.Path()); !slices.Equal(got, []string{firstID, secondID}) {
		t.Fatalf("reopened branch %v, want [%s %s]", got, firstID, secondID)
	}
}

func TestRecorderDoesNotKeepAnEntryWhenCreatingTheFileFails(t *testing.T) {
	rec, err := StartSessionIn(t.TempDir(), t.TempDir(), &ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	modelChangeID := rec.LastEntryID()
	create := rec.createFn
	rec.createFn = func() (*os.File, error) { return nil, errors.New("permission denied") }
	if id := rec.RecordMessage(ai.NewUserText("first question", 1)); id != "" {
		t.Fatalf("a failed create returned id %q", id)
	}
	if rec.Err() == nil {
		t.Fatal("Err is nil after a failed create")
	}
	if got := rec.LastEntryID(); got != modelChangeID {
		t.Fatalf("leaf %q, want %q", got, modelChangeID)
	}
	if len(rec.byID) != 1 {
		t.Fatalf("%d entries kept, want the model change only", len(rec.byID))
	}
	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Fatalf("session file exists after a failed create: %v", err)
	}

	rec.createFn = create
	if id := rec.RecordMessage(ai.NewUserText("second question", 2)); id == "" {
		t.Fatalf("second: %v", rec.Err())
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, []string{"session", "model_change", "user"}) {
		t.Fatalf("file %v, want [session model_change user]", got)
	}
}

// Beyond pi's suite: the file is created but the flush's write fails (pi's
// writeFileSync after openSync). The entry is not kept and the session stays
// unflushed, as pi's does.
func TestRecorderDoesNotKeepAnEntryWhenTheFlushFails(t *testing.T) {
	rec, err := StartSessionIn(t.TempDir(), t.TempDir(), &ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	modelChangeID := rec.LastEntryID()
	rec.createFn = func() (*os.File, error) {
		f, err := os.Create(rec.Path())
		if err != nil {
			return nil, err
		}
		f.Close()
		return os.Open(rec.Path()) // read-only: every write fails
	}
	if id := rec.RecordMessage(ai.NewUserText("first question", 1)); id != "" {
		t.Fatalf("a failed flush returned id %q", id)
	}
	if rec.Err() == nil || rec.flushed || rec.file != nil {
		t.Fatalf("err %v, flushed %v, file %v", rec.Err(), rec.flushed, rec.file)
	}
	if got := rec.LastEntryID(); got != modelChangeID || len(rec.byID) != 1 || len(rec.pending) != 2 {
		t.Fatalf("leaf %q, %d entries, %d held back; want %q, 1, the header and the model change", got, len(rec.byID), len(rec.pending), modelChangeID)
	}
}

// Beyond pi's suite: a system message held back before the conversation
// starts does not make the conversation start, and a conversation message that
// fails to persist does not either, so the next message is held back again
// until one that completes the conversation is kept.
func TestRecorderFailedConversationMessageDoesNotStartTheConversation(t *testing.T) {
	rec, err := StartSessionIn(t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	create := rec.createFn
	rec.createFn = func() (*os.File, error) { return nil, errors.New("disk full") }
	rec.RecordMessage(ai.NewUserText("lost", 1))
	rec.createFn = create
	if rec.hasConversation {
		t.Fatal("a message that was not kept started the conversation")
	}
	rec.RecordThinkingLevel("high")
	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Fatalf("a held-back entry created the file: %v", err)
	}
}
