package modernc_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/durabletest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/sqlite"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/sqlite/modernc"
)

// pi test/sqlite-storage.test.ts: the storage conformance suite, directly
// and across a reopen after every commit.

var ctx = context.Background()

var conformance = durabletest.StorageOptions{RejectsInvalidUTF8: true}

func tempPath(t *testing.T) string { return filepath.Join(t.TempDir(), "storage.sqlite") }

func openStorage(t *testing.T, path string, options modernc.Options) *sqlite.Storage {
	t.Helper()
	s, err := modernc.OpenStorage(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(ctx) })
	return s
}

func TestSqliteStorageConformance(t *testing.T) {
	durabletest.TestStorageWith(t, conformance, func(t *testing.T) durable.Storage {
		return openStorage(t, tempPath(t), modernc.Options{})
	})
}

// reopeningStorage closes and reopens its storage after every commit.
type reopeningStorage struct {
	t       *testing.T
	path    string
	mu      sync.Mutex
	current *sqlite.Storage
	closed  bool
}

func (r *reopeningStorage) cur() *sqlite.Storage {
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
	next, openErr := modernc.OpenStorage(ctx, r.path, modernc.Options{})
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

func TestSqliteStorageConformanceAcrossReopen(t *testing.T) {
	durabletest.TestStorageWith(t, conformance, func(t *testing.T) durable.Storage {
		path := tempPath(t)
		r := &reopeningStorage{t: t, path: path, current: openStorage(t, path, modernc.Options{})}
		t.Cleanup(func() { _ = r.Close(ctx) })
		return r
	})
}
