package jsonl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/durabletest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/jsonl"
)

// pi test/jsonl-storage.test.ts.

var ctx = context.Background()

const root = durable.RootConversationID

func openStorage(t *testing.T, directory string, fsys env.FileSystem, options jsonl.Options) *jsonl.Storage {
	t.Helper()
	if fsys == nil {
		fsys = env.NewLocal("")
	}
	s, err := jsonl.Open(ctx, directory, fsys, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(ctx) })
	return s
}

func reopen(t *testing.T, directory string) *jsonl.Storage {
	t.Helper()
	return openStorage(t, directory, nil, jsonl.Options{})
}

var conformance = durabletest.StorageOptions{RejectsInvalidUTF8: true}

func TestJsonlStorageConformance(t *testing.T) {
	durabletest.TestStorageWith(t, conformance, func(t *testing.T) durable.Storage {
		return openStorage(t, t.TempDir(), nil, jsonl.Options{})
	})
}

// reopeningStorage closes and reopens its JSONL storage after every commit,
// so the suite reads only what the files recover.
type reopeningStorage struct {
	t         *testing.T
	directory string
	mu        sync.Mutex
	current   *jsonl.Storage
	closed    bool
}

func (r *reopeningStorage) cur() *jsonl.Storage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

func (r *reopeningStorage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.current.Commit(ctx, writes)
	}
	seq, err := r.current.Commit(ctx, writes)
	_ = r.current.Close(ctx)
	next, openErr := jsonl.OpenDir(ctx, r.directory, jsonl.Options{})
	if openErr != nil {
		r.t.Errorf("reopen: %v", openErr)
		return seq, errors.Join(err, openErr)
	}
	r.current = next
	return seq, err
}

func (r *reopeningStorage) MintID(ctx context.Context) (durable.ID, error) {
	return r.cur().MintID(ctx)
}
func (r *reopeningStorage) Conversation(ctx context.Context, id durable.ConversationID) (*durable.ConversationRecord, error) {
	return r.cur().Conversation(ctx, id)
}
func (r *reopeningStorage) ScanConversations(ctx context.Context, q durable.ConversationQuery, limit int, c durable.Cursor) (durable.Page[durable.ConversationRecord], error) {
	return r.cur().ScanConversations(ctx, q, limit, c)
}
func (r *reopeningStorage) Entry(ctx context.Context, id durable.EntryID) (*durable.StoredEntry, error) {
	return r.cur().Entry(ctx, id)
}
func (r *reopeningStorage) VisibleEntry(ctx context.Context, cid durable.ConversationID, id durable.EntryID) (*durable.StoredEntry, error) {
	return r.cur().VisibleEntry(ctx, cid, id)
}
func (r *reopeningStorage) FindLatestHeadMarker(ctx context.Context, cid durable.ConversationID, at *durable.EntryID) (*durable.EntryRecord, error) {
	return r.cur().FindLatestHeadMarker(ctx, cid, at)
}
func (r *reopeningStorage) ScanEntries(ctx context.Context, q durable.EntryQuery, limit int, c durable.Cursor) (durable.Page[durable.EntryRecord], error) {
	return r.cur().ScanEntries(ctx, q, limit, c)
}
func (r *reopeningStorage) Task(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	return r.cur().Task(ctx, id)
}
func (r *reopeningStorage) ScanTasks(ctx context.Context, q durable.TaskQuery, limit int, c durable.Cursor) (durable.Page[durable.TaskRecord], error) {
	return r.cur().ScanTasks(ctx, q, limit, c)
}
func (r *reopeningStorage) Submission(ctx context.Context, id durable.SubmissionID) (*durable.SubmissionRecord, error) {
	return r.cur().Submission(ctx, id)
}
func (r *reopeningStorage) ScanSubmissions(ctx context.Context, q durable.SubmissionQuery, limit int, c durable.Cursor) (durable.Page[durable.SubmissionRecord], error) {
	return r.cur().ScanSubmissions(ctx, q, limit, c)
}
func (r *reopeningStorage) SubmissionByRequest(ctx context.Context, cid durable.ConversationID, request string) (*durable.SubmissionRecord, error) {
	return r.cur().SubmissionByRequest(ctx, cid, request)
}
func (r *reopeningStorage) FindDocument(ctx context.Context, a durable.DocumentAddress, at durable.DocumentPoint) (*durable.DocumentRecord, error) {
	return r.cur().FindDocument(ctx, a, at)
}
func (r *reopeningStorage) Document(ctx context.Context, id durable.DocumentID, at durable.DocumentPoint) (*durable.StoredDocument, error) {
	return r.cur().Document(ctx, id, at)
}
func (r *reopeningStorage) ScanDocuments(ctx context.Context, q durable.DocumentQuery, limit int, c durable.Cursor) (durable.Page[durable.DocumentRecord], error) {
	return r.cur().ScanDocuments(ctx, q, limit, c)
}
func (r *reopeningStorage) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.current.Close(ctx)
}

func TestJsonlStorageConformanceAcrossReopen(t *testing.T) {
	durabletest.TestStorageWith(t, conformance, func(t *testing.T) durable.Storage {
		directory := t.TempDir()
		r := &reopeningStorage{t: t, directory: directory, current: openStorage(t, directory, nil, jsonl.Options{})}
		t.Cleanup(func() { _ = r.Close(ctx) })
		return r
	})
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func pendingTask(id durable.TaskID, phase string) durable.TaskRecord {
	if phase == "" {
		phase = "ready"
	}
	checkpoint, _ := json.Marshal(map[string]string{"phase": phase})
	return durable.TaskRecord{
		ID: id, ConversationID: root, Kind: "test.task", Version: 1, Input: raw("null"),
		State: durable.TaskState{Status: durable.TaskPending, Checkpoint: checkpoint},
	}
}

func terminalTask(id durable.TaskID) durable.TaskRecord {
	return durable.TaskRecord{
		ID: id, ConversationID: root, Kind: "test.task", Version: 1, Input: raw("null"),
		State: durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: durable.OutcomeCompleted, Result: raw("null")}},
	}
}

func sessionDocument(id durable.DocumentID, kind string) durable.DocumentCreate {
	if kind == "" {
		kind = "test.document"
	}
	return durable.DocumentCreate{ID: id, Kind: kind, Scope: durable.SessionScope()}
}

type obj = map[string]any

func base(value obj) durable.DocumentContent {
	return durable.DocumentContent{Kind: durable.ContentBase, Version: 1, Value: value}
}

func deltaOps(ops ...delta.Op) durable.DocumentContent {
	return durable.DocumentContent{Kind: durable.ContentDelta, Version: 1, Ops: ops}
}

func setCount(v float64) delta.Op { return delta.Set{Path: delta.Path{delta.Key("count")}, Value: v} }

// must returns v, panicking on err: a test failure with a stack, usable around
// a call with two results.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, s durable.Storage, writes ...durable.StorageWrite) durable.Seq {
	t.Helper()
	return must(s.Commit(ctx, writes))
}

func createRoot(t *testing.T, s durable.Storage) {
	t.Helper()
	commit(t, s, durable.ConversationWrite{Value: durable.ConversationRecord{ID: root}})
}

func mint[I ~int64](t *testing.T, s durable.Storage) I {
	t.Helper()
	return I(must(s.MintID(ctx)))
}

func equal(t *testing.T, got, want any, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got  %#v\n want %#v", what, got, want)
	}
}

func fails(t *testing.T, err error, includes string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), includes) {
		t.Fatalf("error %v, want one containing %q", err, includes)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data := must(os.ReadFile(path))
	text := strings.TrimRight(string(data), "\n\r\t ")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	return must(os.Stat(path)).Size()
}

func reclaimFiles(t *testing.T, directory string) []string {
	t.Helper()
	var out []string
	for _, e := range must(os.ReadDir(directory)) {
		if strings.HasSuffix(e.Name(), ".reclaim") {
			out = append(out, e.Name())
		}
	}
	return out
}

func document(t *testing.T, s durable.Storage, id durable.DocumentID, at durable.DocumentPoint) *durable.StoredDocument {
	t.Helper()
	return must(s.Document(ctx, id, at))
}

func documentValue(t *testing.T, s durable.Storage, id durable.DocumentID) obj {
	t.Helper()
	d := document(t, s, id, durable.Current)
	if d == nil {
		t.Fatalf("document %d is absent", id)
	}
	return d.Value
}

type failure struct {
	operation string // "append", "flush", "write", "rename" or "remove"
	call      int
	mode      string // "before", "after" or "short"
}

func (f failure) String() string { return fmt.Sprintf("%s %d %s", f.operation, f.call, f.mode) }

// instrumentedFS records file operations and injects one failure.
type instrumentedFS struct {
	env.Local
	mu         sync.Mutex
	operations []string
	failure    *failure
	calls      map[string]int
}

func (f *instrumentedFS) fail(failure failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = &failure
	f.operations, f.calls = nil, nil
}

func (f *instrumentedFS) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = nil
	f.operations, f.calls = nil, nil
}

func (f *instrumentedFS) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.operations...)
}

// observe records an operation and returns the failure mode to inject, or
// "".
func (f *instrumentedFS) observe(operation, description string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[operation]++
	f.operations = append(f.operations, operation+":"+description)
	if f.failure == nil || f.failure.operation != operation || f.failure.call != f.calls[operation] {
		return ""
	}
	return f.failure.mode
}

func half(data []byte) []byte { return data[:max(1, len(data)/2)] }

func (f *instrumentedFS) AppendFile(ctx context.Context, path string, data []byte) error {
	switch f.observe("append", filepath.Base(path)) {
	case "":
		return f.Local.AppendFile(ctx, path, data)
	case "before":
		return errors.New("injected append failure")
	case "short":
		if err := f.Local.AppendFile(ctx, path, half(data)); err != nil {
			return err
		}
		return errors.New("injected short append")
	}
	if err := f.Local.AppendFile(ctx, path, data); err != nil {
		return err
	}
	return errors.New("injected post-append failure")
}

func (f *instrumentedFS) FlushFile(ctx context.Context, path string) error {
	switch f.observe("flush", filepath.Base(path)) {
	case "":
		return f.Local.FlushFile(ctx, path)
	case "after":
		if err := f.Local.FlushFile(ctx, path); err != nil {
			return err
		}
	}
	return errors.New("injected flush failure")
}

func (f *instrumentedFS) WriteFile(ctx context.Context, path string, data []byte) error {
	switch f.observe("write", filepath.Base(path)) {
	case "":
		return f.Local.WriteFile(ctx, path, data)
	case "before":
		return errors.New("injected write failure")
	case "short":
		if err := f.Local.WriteFile(ctx, path, half(data)); err != nil {
			return err
		}
		return errors.New("injected short write")
	}
	if err := f.Local.WriteFile(ctx, path, data); err != nil {
		return err
	}
	return errors.New("injected post-write failure")
}

func (f *instrumentedFS) RenameFile(ctx context.Context, oldPath, newPath string) error {
	switch f.observe("rename", filepath.Base(oldPath)+"->"+filepath.Base(newPath)) {
	case "":
		return f.Local.RenameFile(ctx, oldPath, newPath)
	case "after":
		if err := f.Local.RenameFile(ctx, oldPath, newPath); err != nil {
			return err
		}
	}
	return errors.New("injected rename failure")
}

func (f *instrumentedFS) Remove(ctx context.Context, path string, options env.RemoveOptions) error {
	switch f.observe("remove", filepath.Base(path)) {
	case "":
		return f.Local.Remove(ctx, path, options)
	case "after":
		if err := f.Local.Remove(ctx, path, options); err != nil {
			return err
		}
	}
	return errors.New("injected remove failure")
}

func TestOpensThroughTheLocalAdapter(t *testing.T) {
	s := must(jsonl.OpenDir(ctx, t.TempDir(), jsonl.Options{}))
	t.Cleanup(func() { _ = s.Close(ctx) })
	createRoot(t, s)
	equal(t, must(s.Conversation(ctx, root)), &durable.ConversationRecord{ID: root}, "root")
}

func TestPublishesEveryCommitBeforeReclaimingSidecars(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	taskID := mint[durable.TaskID](t, s)
	documentID := mint[durable.DocumentID](t, s)
	commit(t, s, durable.TaskWrite{Value: pendingTask(taskID, "")})
	commit(t, s, durable.DocumentCreateWrite{Record: sessionDocument(documentID, ""), Content: base(obj{"count": 0.0})})
	commit(t, s, durable.DocumentChangeWrite{ID: documentID, Content: deltaOps()})
	commit(t, s, durable.DocumentChangeWrite{ID: documentID, Content: base(obj{"count": 1.0})})
	taskPath := filepath.Join(directory, fmt.Sprintf("task-%d.jsonl", taskID))
	docPath := filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", documentID))
	equal(t, len(readLines(t, taskPath)), 1, "task sidecar lines")
	equal(t, len(readLines(t, docPath)), 1, "document sidecar lines")

	commit(t, s, durable.DocumentRetireWrite{ID: documentID})
	commit(t, s, durable.TaskWrite{Value: terminalTask(taskID)})

	lines := readLines(t, filepath.Join(directory, "main.jsonl"))
	equal(t, len(lines), 7, "main lines")
	equal(t, fileExists(t, taskPath), false, "task sidecar exists")
	equal(t, fileExists(t, docPath), false, "document sidecar exists")
	for _, line := range lines {
		var marker struct{ Type string }
		noErr(t, json.Unmarshal([]byte(line), &marker))
		equal(t, marker.Type, "commit", "marker type")
	}
}

func TestOrdersLiveTaskReplacementsByCommitOrdinal(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	taskID := mint[durable.TaskID](t, s)
	commit(t, s, durable.TaskWrite{Value: pendingTask(taskID, "first")}, durable.TaskWrite{Value: pendingTask(taskID, "second")})
	equal(t, len(readLines(t, filepath.Join(directory, fmt.Sprintf("task-%d.jsonl", taskID)))), 2, "task sidecar lines")

	reopened := reopen(t, directory)
	want := pendingTask(taskID, "second")
	equal(t, must(reopened.Task(ctx, taskID)), &want, "task")
}

func TestRemovesACompletePlusTornUnconfirmedSidecarAppend(t *testing.T) {
	directory := t.TempDir()
	fsys := &instrumentedFS{}
	s := openStorage(t, directory, fsys, jsonl.Options{})
	createRoot(t, s)
	taskID := mint[durable.TaskID](t, s)
	fsys.fail(failure{"append", 1, "short"})
	_, err := s.Commit(ctx, []durable.StorageWrite{
		durable.TaskWrite{Value: pendingTask(taskID, "first")},
		durable.TaskWrite{Value: pendingTask(taskID, "second-"+strings.Repeat("x", 512))},
	})
	fails(t, err, "poisoned")
	taskPath := filepath.Join(directory, fmt.Sprintf("task-%d.jsonl", taskID))
	partial := must(os.ReadFile(taskPath))
	if partial[len(partial)-1] == '\n' {
		t.Fatal("the partial append ends with a newline")
	}
	equal(t, bytes.Count(partial, []byte("\n")), 1, "complete lines in the partial append")

	reopened := reopen(t, directory)
	equal(t, must(reopened.Task(ctx, taskID)), (*durable.TaskRecord)(nil), "task")
	equal(t, fileSize(t, taskPath), int64(0), "task sidecar size")
}

func TestSerializesTheCandidateBeforeIOAndStaysUsable(t *testing.T) {
	directory := t.TempDir()
	fsys := &instrumentedFS{}
	s := openStorage(t, directory, fsys, jsonl.Options{})
	createRoot(t, s)
	fsys.clear()
	id := mint[durable.EntryID](t, s)
	// pi's case is a bigint; in Go a payload that is not JSON fails to encode.
	_, err := s.Commit(ctx, []durable.StorageWrite{durable.EntryWrite{Value: durable.EntryRecord{ID: id, ConversationID: root, Kind: "bad", Data: raw("{bad")}}})
	if err == nil {
		t.Fatal("a payload that is not JSON was committed")
	}
	equal(t, fsys.ops(), []string(nil), "operations")
	equal(t, must(s.Entry(ctx, id)), (*durable.StoredEntry)(nil), "entry")
	equal(t, commit(t, s, durable.EntryWrite{Value: durable.EntryRecord{ID: id, ConversationID: root, Kind: "good"}}), durable.Seq(2), "sequence")
}

func TestRejectsInvalidUTF8BeforeIO(t *testing.T) {
	directory := t.TempDir()
	fsys := &instrumentedFS{}
	s := openStorage(t, directory, fsys, jsonl.Options{})
	createRoot(t, s)
	fsys.clear()
	id := mint[durable.EntryID](t, s)
	for _, e := range []durable.EntryRecord{
		{ID: id, ConversationID: root, Kind: "\xed\xa0\x80"},
		{ID: id, ConversationID: root, Kind: "data", Data: raw("\"\xff\"")},
	} {
		_, err := s.Commit(ctx, []durable.StorageWrite{durable.EntryWrite{Value: e}})
		fails(t, err, "UTF-8")
	}
	docID := mint[durable.DocumentID](t, s)
	_, err := s.Commit(ctx, []durable.StorageWrite{durable.DocumentCreateWrite{Record: sessionDocument(docID, ""), Content: base(obj{"\xff": 1.0})}})
	fails(t, err, "UTF-8")
	equal(t, fsys.ops(), []string(nil), "operations")
	equal(t, commit(t, s, durable.EntryWrite{Value: durable.EntryRecord{ID: id, ConversationID: root, Kind: "good"}}), durable.Seq(2), "sequence")
}

func TestRefusesALoneSurrogateEscapeOnRead(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	id := mint[durable.EntryID](t, s)
	commit(t, s, durable.EntryWrite{Value: durable.EntryRecord{ID: id, ConversationID: root, Kind: "\U0001F600 paired"}})
	noErr(t, s.Close(ctx))
	// A pair, written as escapes, is valid.
	mainPath := filepath.Join(directory, "main.jsonl")
	data := must(os.ReadFile(mainPath))
	paired := bytes.Replace(data, []byte("\U0001F600"), []byte(`\ud83d\ude00`), 1)
	noErr(t, os.WriteFile(mainPath, paired, 0o644))
	reopened := reopen(t, directory)
	equal(t, must(reopened.Entry(ctx, id)).Entry.Kind, "\U0001F600 paired", "paired kind")
	noErr(t, reopened.Close(ctx))

	lone := bytes.Replace(paired, []byte(`\ud83d\ude00`), []byte(`\ud800`), 1)
	noErr(t, os.WriteFile(mainPath, lone, 0o644))
	_, err := jsonl.OpenDir(ctx, directory, jsonl.Options{})
	fails(t, err, "Unsupported lone surrogate escape in complete main.jsonl line 2")
	var corrupt *jsonl.CorruptionError
	if !errors.As(err, &corrupt) {
		t.Fatalf("error %T, want *CorruptionError", err)
	}
}

func twoDocuments(firstID, secondID durable.DocumentID, firstKind, secondKind string, first, second obj) []durable.StorageWrite {
	return []durable.StorageWrite{
		durable.DocumentCreateWrite{Record: sessionDocument(firstID, firstKind), Content: base(first)},
		durable.DocumentCreateWrite{Record: sessionDocument(secondID, secondKind), Content: base(second)},
	}
}

func TestPoisonsAfterAppendFailureAndRecoversOnlyConfirmedState(t *testing.T) {
	for _, f := range []failure{
		{"append", 1, "before"}, {"append", 1, "after"}, {"append", 1, "short"},
		{"append", 2, "before"}, {"append", 2, "after"},
		{"append", 3, "before"}, {"append", 3, "short"}, {"append", 3, "after"},
	} {
		t.Run(f.String(), func(t *testing.T) {
			directory := t.TempDir()
			fsys := &instrumentedFS{}
			s := openStorage(t, directory, fsys, jsonl.Options{})
			createRoot(t, s)
			firstID, secondID := mint[durable.DocumentID](t, s), mint[durable.DocumentID](t, s)
			fsys.fail(f)
			_, err := s.Commit(ctx, twoDocuments(firstID, secondID, "first", "second", obj{"text": "α"}, obj{"text": "β"}))
			fails(t, err, "poisoned")
			_, err = s.Document(ctx, firstID, durable.Current)
			fails(t, err, "poisoned")

			reopened := reopen(t, directory)
			if f.call == 3 && f.mode == "after" {
				equal(t, documentValue(t, reopened, firstID), obj{"text": "α"}, "first")
				equal(t, documentValue(t, reopened, secondID), obj{"text": "β"}, "second")
			} else {
				equal(t, document(t, reopened, firstID, durable.Current), (*durable.StoredDocument)(nil), "first")
				equal(t, document(t, reopened, secondID, durable.Current), (*durable.StoredDocument)(nil), "second")
			}
		})
	}
}

func TestPoisonsAfterFlushFailureAndNeverWritesAMarker(t *testing.T) {
	for _, f := range []failure{
		{"flush", 1, "before"}, {"flush", 1, "after"}, {"flush", 2, "before"}, {"flush", 2, "after"},
	} {
		t.Run(f.String(), func(t *testing.T) {
			directory := t.TempDir()
			fsys := &instrumentedFS{}
			s := openStorage(t, directory, fsys, jsonl.Options{Fsync: true})
			createRoot(t, s)
			firstID, secondID := mint[durable.DocumentID](t, s), mint[durable.DocumentID](t, s)
			fsys.fail(f)
			_, err := s.Commit(ctx, twoDocuments(firstID, secondID, "flush.first", "flush.second", obj{}, obj{}))
			fails(t, err, "poisoned")
			reopened := reopen(t, directory)
			equal(t, document(t, reopened, firstID, durable.Current), (*durable.StoredDocument)(nil), "first")
			equal(t, document(t, reopened, secondID, durable.Current), (*durable.StoredDocument)(nil), "second")
		})
	}
}

func TestOrdersPublicationFlushesExactly(t *testing.T) {
	for _, fsync := range []bool{false, true} {
		t.Run(fmt.Sprint("fsync=", fsync), func(t *testing.T) {
			directory := t.TempDir()
			fsys := &instrumentedFS{}
			s := openStorage(t, directory, fsys, jsonl.Options{Fsync: fsync})
			createRoot(t, s)
			firstID, secondID := mint[durable.DocumentID](t, s), mint[durable.DocumentID](t, s)
			first, second := fmt.Sprintf("doc-%d.jsonl", firstID), fmt.Sprintf("doc-%d.jsonl", secondID)
			when := func(ops ...string) []string {
				if fsync {
					return ops
				}
				return nil
			}
			join := func(parts ...[]string) []string {
				var out []string
				for _, p := range parts {
					out = append(out, p...)
				}
				return out
			}
			fsys.clear()
			commit(t, s, twoDocuments(secondID, firstID, "second", "first", obj{}, obj{})...)
			equal(t, fsys.ops(), join(
				[]string{"append:" + second, "append:" + first},
				when("flush:"+second, "flush:"+first),
				[]string{"append:main.jsonl"},
			), "creation operations")

			fsys.clear()
			commit(t, s, durable.DocumentChangeWrite{ID: firstID, Content: base(obj{"checkpoint": true})})
			equal(t, fsys.ops(), join(
				[]string{"append:" + first},
				when("flush:"+first),
				[]string{"append:main.jsonl"},
				when("flush:main.jsonl"),
				[]string{"write:" + first + ".reclaim"},
				when("flush:"+first+".reclaim"),
				[]string{"rename:" + first + ".reclaim->" + first},
			), "base operations")

			fsys.clear()
			commit(t, s, durable.EntryWrite{Value: durable.EntryRecord{ID: mint[durable.EntryID](t, s), ConversationID: root, Kind: "main-only"}})
			equal(t, fsys.ops(), []string{"append:main.jsonl"}, "entry operations")

			taskID := mint[durable.TaskID](t, s)
			commit(t, s, durable.TaskWrite{Value: pendingTask(taskID, "")})
			fsys.clear()
			commit(t, s, durable.TaskWrite{Value: terminalTask(taskID)})
			equal(t, fsys.ops(), join(
				[]string{"append:main.jsonl"},
				when("flush:main.jsonl"),
				[]string{fmt.Sprintf("remove:task-%d.jsonl", taskID)},
			), "terminal task operations")
		})
	}
}

func TestRecoversACommittedBaseAcrossReclaimFailure(t *testing.T) {
	for _, f := range []failure{
		{"write", 1, "before"}, {"write", 1, "short"}, {"write", 1, "after"},
		{"rename", 1, "before"}, {"rename", 1, "after"},
	} {
		t.Run(f.String(), func(t *testing.T) {
			directory := t.TempDir()
			fsys := &instrumentedFS{}
			s := openStorage(t, directory, fsys, jsonl.Options{})
			createRoot(t, s)
			id := mint[durable.DocumentID](t, s)
			commit(t, s, durable.DocumentCreateWrite{Record: sessionDocument(id, ""), Content: base(obj{"count": 0.0})})
			commit(t, s, durable.DocumentChangeWrite{ID: id, Content: deltaOps(setCount(1))})

			fsys.fail(f)
			equal(t, commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"count": 2.0})}), durable.Seq(4), "sequence")
			equal(t, documentValue(t, s, id), obj{"count": 2.0}, "value")
			noErr(t, s.Close(ctx))

			reopened := reopen(t, directory)
			equal(t, documentValue(t, reopened, id), obj{"count": 2.0}, "reopened value")
			equal(t, len(readLines(t, filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", id)))), 1, "document sidecar lines")
			equal(t, reclaimFiles(t, directory), []string(nil), "reclaim files")
		})
	}
}

func TestRecoversDocumentRetirementReclamationAcrossRemove(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			fsys := &instrumentedFS{}
			s := openStorage(t, directory, fsys, jsonl.Options{})
			createRoot(t, s)
			taskID := mint[durable.TaskID](t, s)
			id := mint[durable.DocumentID](t, s)
			commit(t, s,
				durable.TaskWrite{Value: pendingTask(taskID, "")},
				durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: id, Kind: "task.document", Scope: durable.TaskScope(taskID)}, Content: base(obj{"count": 1.0})},
			)
			fsys.fail(failure{"remove", 1, mode})
			equal(t, commit(t, s, durable.DocumentRetireWrite{ID: id}), durable.Seq(3), "sequence")
			equal(t, document(t, s, id, durable.Current), (*durable.StoredDocument)(nil), "document")
			noErr(t, s.Close(ctx))

			reopened := reopen(t, directory)
			equal(t, document(t, reopened, id, durable.Current), (*durable.StoredDocument)(nil), "reopened document")
			want := pendingTask(taskID, "")
			equal(t, must(reopened.Task(ctx, taskID)), &want, "task")
			equal(t, fileExists(t, filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", id))), false, "document sidecar exists")
			equal(t, reclaimFiles(t, directory), []string(nil), "reclaim files")
		})
	}
}

func TestDefersReclamationAfterAuthorizingMainFlushFailure(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			fsys := &instrumentedFS{}
			s := openStorage(t, directory, fsys, jsonl.Options{Fsync: true})
			createRoot(t, s)
			id := mint[durable.DocumentID](t, s)
			docName := fmt.Sprintf("doc-%d.jsonl", id)
			docPath := filepath.Join(directory, docName)
			commit(t, s, durable.DocumentCreateWrite{Record: sessionDocument(id, ""), Content: base(obj{"count": 0.0})})
			fsys.fail(failure{"flush", 2, mode})
			equal(t, commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"count": 2.0})}), durable.Seq(3), "sequence")
			equal(t, fsys.ops(), []string{"append:" + docName, "flush:" + docName, "append:main.jsonl", "flush:main.jsonl"}, "operations")
			equal(t, documentValue(t, s, id), obj{"count": 2.0}, "value")
			equal(t, len(readLines(t, docPath)), 2, "document sidecar lines")
			noErr(t, s.Close(ctx))

			recovery := &instrumentedFS{}
			recovery.fail(failure{"flush", 1, mode})
			deferred := openStorage(t, directory, recovery, jsonl.Options{Fsync: true})
			equal(t, documentValue(t, deferred, id), obj{"count": 2.0}, "deferred value")
			equal(t, recovery.ops(), []string{"flush:main.jsonl"}, "recovery operations")
			equal(t, len(readLines(t, docPath)), 2, "deferred document sidecar lines")
			noErr(t, deferred.Close(ctx))

			reclaimed := openStorage(t, directory, nil, jsonl.Options{Fsync: true})
			equal(t, documentValue(t, reclaimed, id), obj{"count": 2.0}, "reclaimed value")
			equal(t, len(readLines(t, docPath)), 1, "reclaimed document sidecar lines")
		})
	}
}

func TestKeepsACommittedBaseUsableAfterReclaimTempFlushFailure(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			fsys := &instrumentedFS{}
			s := openStorage(t, directory, fsys, jsonl.Options{Fsync: true})
			createRoot(t, s)
			id := mint[durable.DocumentID](t, s)
			commit(t, s, durable.DocumentCreateWrite{Record: sessionDocument(id, ""), Content: base(obj{"count": 0.0})})
			fsys.fail(failure{"flush", 3, mode})
			equal(t, commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"count": 2.0})}), durable.Seq(3), "sequence")
			fsys.clear()
			commit(t, s, durable.DocumentChangeWrite{ID: id, Content: deltaOps(setCount(3))})
			noErr(t, s.Close(ctx))

			reopened := openStorage(t, directory, nil, jsonl.Options{Fsync: true})
			equal(t, documentValue(t, reopened, id), obj{"count": 3.0}, "value")
			equal(t, len(readLines(t, filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", id)))), 2, "document sidecar lines")
		})
	}
}

func TestRecoversTerminalTaskReclamationAcrossRemove(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			fsys := &instrumentedFS{}
			s := openStorage(t, directory, fsys, jsonl.Options{})
			createRoot(t, s)
			id := mint[durable.TaskID](t, s)
			commit(t, s, durable.TaskWrite{Value: pendingTask(id, "")})
			fsys.fail(failure{"remove", 1, mode})
			equal(t, commit(t, s, durable.TaskWrite{Value: terminalTask(id)}), durable.Seq(3), "sequence")
			want := terminalTask(id)
			equal(t, must(s.Task(ctx, id)), &want, "task")
			noErr(t, s.Close(ctx))

			reopened := reopen(t, directory)
			equal(t, must(reopened.Task(ctx, id)), &want, "reopened task")
			equal(t, fileExists(t, filepath.Join(directory, fmt.Sprintf("task-%d.jsonl", id))), false, "task sidecar exists")
			equal(t, reclaimFiles(t, directory), []string(nil), "reclaim files")
		})
	}
}

func TestAppendsLaterDeltasToTheReplacementSidecar(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	id := mint[durable.DocumentID](t, s)
	commit(t, s, durable.DocumentCreateWrite{Record: sessionDocument(id, ""), Content: base(obj{"count": 0.0})})
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"count": 10.0})})
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: deltaOps(setCount(11))})
	equal(t, len(readLines(t, filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", id)))), 2, "document sidecar lines")
	equal(t, documentValue(t, reopen(t, directory), id), obj{"count": 11.0}, "value")
}

func TestNeverReclaimsRewindableDocumentHistory(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	id := mint[durable.DocumentID](t, s)
	record := durable.DocumentCreate{ID: id, Kind: "rewindable", Scope: durable.ConversationScope(root), History: durable.HistoryRewindable, Fork: durable.ForkAsOf}
	createdAt := commit(t, s, durable.DocumentCreateWrite{Record: record, Content: base(obj{"count": 0.0})})
	changedAt := commit(t, s, durable.DocumentChangeWrite{ID: id, Content: deltaOps(setCount(1))})
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"count": 2.0})})
	commit(t, s, durable.DocumentRetireWrite{ID: id})

	docPath := filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", id))
	equal(t, len(readLines(t, docPath)), 3, "document sidecar lines")
	reopened := reopen(t, directory)
	equal(t, document(t, reopened, id, createdAt).Value, obj{"count": 0.0}, "value at creation")
	equal(t, document(t, reopened, id, changedAt).Value, obj{"count": 1.0}, "value at change")
	equal(t, document(t, reopened, id, durable.Current), (*durable.StoredDocument)(nil), "current")
	equal(t, len(readLines(t, docPath)), 3, "reopened document sidecar lines")
}

func TestReclaimsRetiredTaskSessionAndLatestConversationSidecars(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	taskID := mint[durable.TaskID](t, s)
	sessionID := mint[durable.DocumentID](t, s)
	latestID := mint[durable.DocumentID](t, s)
	taskDocumentID := mint[durable.DocumentID](t, s)
	createdAt := commit(t, s,
		durable.TaskWrite{Value: pendingTask(taskID, "")},
		durable.DocumentCreateWrite{Record: sessionDocument(sessionID, "session"), Content: base(obj{})},
		durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: latestID, Kind: "latest", Scope: durable.ConversationScope(root), History: durable.HistoryLatest, Fork: durable.ForkCurrent}, Content: base(obj{})},
		durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: taskDocumentID, Kind: "task", Scope: durable.TaskScope(taskID)}, Content: base(obj{})},
	)
	retiredAt := commit(t, s,
		durable.DocumentRetireWrite{ID: sessionID},
		durable.DocumentRetireWrite{ID: latestID},
		durable.DocumentRetireWrite{ID: taskDocumentID},
		durable.TaskWrite{Value: terminalTask(taskID)},
	)
	for _, file := range []string{
		fmt.Sprintf("doc-%d.jsonl", sessionID), fmt.Sprintf("doc-%d.jsonl", latestID),
		fmt.Sprintf("doc-%d.jsonl", taskDocumentID), fmt.Sprintf("task-%d.jsonl", taskID),
	} {
		equal(t, fileExists(t, filepath.Join(directory, file)), false, file+" exists")
	}

	reopened := reopen(t, directory)
	want := terminalTask(taskID)
	equal(t, must(reopened.Task(ctx, taskID)), &want, "task")
	for _, id := range []durable.DocumentID{sessionID, latestID, taskDocumentID} {
		equal(t, document(t, reopened, id, durable.Current), (*durable.StoredDocument)(nil), fmt.Sprint("document ", id))
	}
	address := durable.DocumentAddress{Kind: "session", Scope: durable.SessionScope()}
	found := must(reopened.FindDocument(ctx, address, createdAt))
	if found == nil || found.ID != sessionID || found.CreatedAt != createdAt || found.RetiredAt == nil || *found.RetiredAt != retiredAt {
		t.Fatalf("session document at creation: %#v", found)
	}
	equal(t, must(reopened.FindDocument(ctx, address, retiredAt)), (*durable.DocumentRecord)(nil), "session document at retirement")
}

func TestTruncatesTornUTF8TailsAndReusesTheUnconfirmedSequence(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	documentID := mint[durable.DocumentID](t, s)
	commit(t, s, durable.DocumentCreateWrite{Record: sessionDocument(documentID, ""), Content: base(obj{"text": "kept"})})
	noErr(t, s.Close(ctx))
	sidecarPath := filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", documentID))
	mainPath := filepath.Join(directory, "main.jsonl")
	sidecarSize, mainSize := fileSize(t, sidecarPath), fileSize(t, mainPath)
	torn := []byte(`{"text":"€`)
	torn = torn[:len(torn)-1]
	for _, path := range []string{sidecarPath, mainPath} {
		noErr(t, env.NewLocal("").AppendFile(ctx, path, torn))
	}

	reopened := reopen(t, directory)
	equal(t, fileSize(t, sidecarPath), sidecarSize, "sidecar size")
	equal(t, fileSize(t, mainPath), mainSize, "main size")
	equal(t, commit(t, reopened, durable.DocumentChangeWrite{ID: documentID, Content: deltaOps()}), durable.Seq(3), "sequence")
}

func TestRemovesCompleteUnconfirmedTailsWithoutResurrectingTerminalTasks(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	taskID := mint[durable.TaskID](t, s)
	commit(t, s, durable.TaskWrite{Value: pendingTask(taskID, "")})
	commit(t, s, durable.TaskWrite{Value: terminalTask(taskID)})
	noErr(t, s.Close(ctx))
	sidecarPath := filepath.Join(directory, fmt.Sprintf("task-%d.jsonl", taskID))
	equal(t, fileExists(t, sidecarPath), false, "task sidecar exists")
	stale := must(json.Marshal(map[string]any{"format": 1, "type": "record", "seq": 4, "ordinal": 0, "payload": map[string]any{"type": "task", "value": pendingTask(taskID, "stale")}}))
	noErr(t, env.NewLocal("").AppendFile(ctx, sidecarPath, append(stale, '\n')))

	reopened := reopen(t, directory)
	equal(t, fileExists(t, sidecarPath), false, "task sidecar exists after reopen")
	want := terminalTask(taskID)
	equal(t, must(reopened.Task(ctx, taskID)), &want, "task")
	equal(t, commit(t, reopened), durable.Seq(4), "sequence")
}

func createdDocument(t *testing.T, value obj) (string, durable.DocumentID) {
	t.Helper()
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	id := mint[durable.DocumentID](t, s)
	commit(t, s, durable.DocumentCreateWrite{Record: sessionDocument(id, ""), Content: base(value)})
	noErr(t, s.Close(ctx))
	return directory, id
}

func TestFailsOpenWhenConfirmedSidecarDataIsMissing(t *testing.T) {
	directory, id := createdDocument(t, obj{})
	noErr(t, os.WriteFile(filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", id)), nil, 0o644))
	_, err := jsonl.OpenDir(ctx, directory, jsonl.Options{})
	fails(t, err, "Missing confirmed sidecar record")
}

func TestRejectsAConfirmedRecordAfterAnUnconfirmedOne(t *testing.T) {
	directory, id := createdDocument(t, obj{"count": 0.0})
	s := reopen(t, directory)
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: deltaOps(setCount(1))})
	noErr(t, s.Close(ctx))
	path := filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", id))
	lines := readLines(t, path)
	unconfirmed := must(json.Marshal(map[string]any{"format": 1, "type": "record", "seq": 2, "ordinal": 999, "payload": map[string]any{"type": "document", "id": id, "content": map[string]any{"kind": "delta", "version": 1, "ops": []any{}}}}))
	noErr(t, os.WriteFile(path, []byte(lines[0]+"\n"+string(unconfirmed)+"\n"+lines[1]+"\n"), 0o644))
	_, err := jsonl.OpenDir(ctx, directory, jsonl.Options{})
	fails(t, err, "Confirmed record follows an unconfirmed tail")
}

func TestRejectsNonIncreasingMainCommitSequences(t *testing.T) {
	directory := t.TempDir()
	s := reopen(t, directory)
	createRoot(t, s)
	noErr(t, s.Close(ctx))
	path := filepath.Join(directory, "main.jsonl")
	noErr(t, env.NewLocal("").AppendFile(ctx, path, must(os.ReadFile(path))))
	_, err := jsonl.OpenDir(ctx, directory, jsonl.Options{})
	fails(t, err, "Commit sequence does not strictly increase")
}

func TestRejectsStructurallyInvalidConfirmedDocumentContent(t *testing.T) {
	directory, id := createdDocument(t, obj{})
	path := filepath.Join(directory, fmt.Sprintf("doc-%d.jsonl", id))
	var record map[string]any
	noErr(t, json.Unmarshal(must(os.ReadFile(path)), &record))
	delete(record["payload"].(map[string]any)["content"].(map[string]any), "value")
	noErr(t, os.WriteFile(path, append(must(json.Marshal(record)), '\n'), 0o644))
	_, err := jsonl.OpenDir(ctx, directory, jsonl.Options{})
	fails(t, err, "Invalid document content")
}

func TestRejectsMalformedCompleteMainAndSidecarLines(t *testing.T) {
	mainDirectory := t.TempDir()
	s := reopen(t, mainDirectory)
	createRoot(t, s)
	noErr(t, s.Close(ctx))
	noErr(t, env.NewLocal("").AppendFile(ctx, filepath.Join(mainDirectory, "main.jsonl"), []byte("{bad}\n")))
	_, err := jsonl.OpenDir(ctx, mainDirectory, jsonl.Options{})
	fails(t, err, "Malformed complete main.jsonl")

	sidecarDirectory := t.TempDir()
	s = reopen(t, sidecarDirectory)
	createRoot(t, s)
	noErr(t, s.Close(ctx))
	noErr(t, os.WriteFile(filepath.Join(sidecarDirectory, "doc-99.jsonl"), []byte("{bad}\n"), 0o644))
	_, err = jsonl.OpenDir(ctx, sidecarDirectory, jsonl.Options{})
	fails(t, err, "Malformed complete doc-99.jsonl")
}
