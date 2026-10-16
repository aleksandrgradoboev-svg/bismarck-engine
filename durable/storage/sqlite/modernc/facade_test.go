package modernc_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/sqlite/modernc"
)

// pi test/sqlite-facade.test.ts, the cases that apply to database/sql. pi's
// facade has its own operation queue; here the adapter's single connection
// is the queue, and database/sql owns statement caching and stale
// transaction handles (sql.ErrTxDone), so those cases are not ported.

func TestSerializesConcurrentTransactions(t *testing.T) {
	db := openDatabase(t, ":memory:")
	must(db.Exec("CREATE TABLE transaction_queue (value INTEGER)"))
	first := must(db.BeginTx(ctx, nil))
	must(first.Exec("INSERT INTO transaction_queue (value) VALUES (1)"))

	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		second, err := db.BeginTx(ctx, nil)
		close(started)
		if err == nil {
			_, err = second.Exec("INSERT INTO transaction_queue (value) VALUES (2)")
			err = errors.Join(err, second.Commit())
		}
		done <- err
	}()
	select {
	case <-started:
		t.Fatal("a second transaction started while the first was open")
	case <-time.After(50 * time.Millisecond):
	}
	noErr(t, first.Commit())
	noErr(t, <-done)
	rows := must(db.Query("SELECT value FROM transaction_queue ORDER BY value"))
	defer rows.Close()
	var values []int64
	for rows.Next() {
		var v int64
		noErr(t, rows.Scan(&v))
		values = append(values, v)
	}
	equal(t, values, []int64{1, 2}, "values")
}

func TestClosesIdempotently(t *testing.T) {
	s := must(modernc.OpenStorage(ctx, tempPath(t), modernc.Options{}))
	createRoot(t, s)
	noErr(t, s.Close(ctx))
	noErr(t, s.Close(ctx))
	_, err := s.Conversation(ctx, root)
	fails(t, err, "SqliteStorage is closed")
}

// TestLetsAdmittedReadsFinishBeforeClose: a read racing Close either
// finishes with its result or is refused as closed; none reaches a closed
// database.
func TestLetsAdmittedReadsFinishBeforeClose(t *testing.T) {
	for range 20 {
		s := must(modernc.OpenStorage(ctx, ":memory:", modernc.Options{}))
		entryID := durable.EntryID(2)
		commit(t, s, durable.ConversationWrite{Value: durable.ConversationRecord{ID: root}}, durable.EntryWrite{Value: durable.EntryRecord{ID: entryID, ConversationID: root, Kind: "probe"}})
		var wg sync.WaitGroup
		errs := make(chan error, 300)
		for range 100 {
			wg.Add(3)
			go func() {
				defer wg.Done()
				page, err := s.ScanEntries(ctx, durable.EntryQuery{ConversationID: root}, 10, nil)
				if err == nil && (len(page.Items) != 1 || page.Items[0].ID != entryID) {
					err = errors.New("scan returned the wrong entries")
				}
				errs <- err
			}()
			go func() {
				defer wg.Done()
				e, err := s.VisibleEntry(ctx, root, entryID)
				if err == nil && (e == nil || e.Entry.Kind != "probe") {
					err = errors.New("the visible entry is wrong")
				}
				errs <- err
			}()
			go func() {
				defer wg.Done()
				head, err := s.FindLatestHeadMarker(ctx, root, nil)
				if err == nil && head != nil {
					err = errors.New("a head marker appeared")
				}
				errs <- err
			}()
		}
		closed := make(chan error, 2)
		go func() { closed <- s.Close(ctx) }()
		go func() { closed <- s.Close(ctx) }()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && err.Error() != "SqliteStorage is closed" {
				t.Fatalf("a read racing close failed: %v", err)
			}
		}
		noErr(t, <-closed)
		noErr(t, <-closed)
		_, err := s.ScanEntries(ctx, durable.EntryQuery{ConversationID: root}, 10, nil)
		fails(t, err, "SqliteStorage is closed")
	}
}

func TestAdoptsIDsOnlyAfterASuccessfulCommit(t *testing.T) {
	s := openStorage(t, ":memory:", modernc.Options{})
	equal(t, mint[durable.EntryID](s), durable.EntryID(2), "first minted ID")
	equal(t, commit(t, s, durable.EntryWrite{Value: durable.EntryRecord{ID: 100, ConversationID: root, Kind: "settled"}}), durable.Seq(1), "sequence")
	equal(t, mint[durable.EntryID](s), durable.EntryID(101), "minted ID after a commit")

	_, err := s.Commit(ctx, []durable.StorageWrite{
		durable.EntryWrite{Value: durable.EntryRecord{ID: 200, ConversationID: root, Kind: "rejected"}},
		durable.ConversationWrite{Value: durable.ConversationRecord{ID: 100}},
	})
	fails(t, err, "ID 100 already belongs to entry")
	equal(t, mint[durable.EntryID](s), durable.EntryID(102), "minted ID after a rejected commit")
	equal(t, must(s.Entry(ctx, 200)), (*durable.StoredEntry)(nil), "rejected entry")
}

func TestReadsADocumentFromOneCommittedStateWhileItsBaseIsReplaced(t *testing.T) {
	id := durable.DocumentID(5)
	for range 16 {
		s := must(modernc.OpenStorage(ctx, ":memory:", modernc.Options{}))
		commit(t, s, durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: id, Kind: "replaced", Scope: durable.SessionScope()}, Content: base(obj{"value": 1.0})})
		read := make(chan *durable.StoredDocument, 1)
		go func() { read <- must(s.Document(ctx, id, durable.Current)) }()
		commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"value": 2.0})})
		stored := <-read
		if v := stored.Value["value"]; v != 1.0 && v != 2.0 {
			t.Fatalf("value %#v, want one committed state", stored.Value)
		}
		noErr(t, s.Close(ctx))
	}
}
