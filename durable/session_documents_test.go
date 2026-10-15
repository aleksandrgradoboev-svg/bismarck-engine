package durable_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/session-documents.test.ts

var liveInitCount atomic.Int64

func liveInitial() map[string]any {
	return map[string]any{"items": []any{}, "nested": map[string]any{"count": 0}, "other": map[string]any{"label": "x"}}
}

var LiveDoc = durable.MustDefineDoc(durable.DocDef{
	Kind: "test.live", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryLatest, Fork: durable.ForkInitial,
	Initial: func(any) map[string]any { liveInitCount.Add(1); return liveInitial() },
})

var RewindableLiveDoc = durable.MustDefineDoc(durable.DocDef{
	Kind: "test.live", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf,
	Initial: func(any) map[string]any { return liveInitial() },
})

var LiveDocV2 = durable.MustDefineDoc(durable.DocDef{
	Kind: "test.live", Version: 2, Scope: durable.ScopeConversation, History: durable.HistoryLatest, Fork: durable.ForkInitial,
	Initial: func(any) map[string]any { return liveInitial() },
})

var CounterDoc = durable.MustDefineDoc(durable.DocDef{
	Kind: "test.counter", Version: 1, Scope: durable.ScopeSession,
	Initial: func(any) map[string]any { return map[string]any{"count": 0} },
})

var (
	seedsMu sync.Mutex
	seeds   []string
)

var MemberDoc = durable.MustDefineDoc(durable.DocDef{
	Kind: "test.member", Version: 1, Family: true, Scope: durable.ScopeSession,
	Initial: func(seed any) map[string]any {
		seedsMu.Lock()
		seeds = append(seeds, seed.(string))
		seedsMu.Unlock()
		return map[string]any{"seed": seed, "hits": 0}
	},
})

func setupLive(t *testing.T) (*testSession, durable.ConversationID) {
	t.Helper()
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	commit(t, ts.session, func(tx *durable.Tx) error {
		_, err := mustDoc(tx, LiveDoc.Conversation(conversationID)).At("items").Push("a", "b")
		return err
	})
	return ts, conversationID
}

func findWrite[W durable.StorageWrite](writes []durable.StorageWrite) (W, bool) {
	for _, w := range writes {
		if v, ok := w.(W); ok {
			return v, true
		}
	}
	var zero W
	return zero, false
}

func countWrites[W durable.StorageWrite](writes []durable.StorageWrite) int {
	n := 0
	for _, w := range writes {
		if _, ok := w.(W); ok {
			n++
		}
	}
	return n
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDocumentCreatesInitialBaseOnFirstAccess(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	commit(t, ts.session, func(tx *durable.Tx) error {
		return mustDoc(tx, LiveDoc.Conversation(conversationID)).Set("message", "hello")
	})
	writes := ts.storage.lastCommit()
	if len(writes) != 1 {
		t.Fatalf("writes = %d", len(writes))
	}
	create, ok := writes[0].(durable.DocumentCreateWrite)
	if !ok {
		t.Fatalf("write = %T", writes[0])
	}
	r := create.Record
	if r.Kind != "test.live" || r.Scope != durable.ConversationScope(conversationID) || r.History != durable.HistoryLatest || r.Fork != durable.ForkInitial {
		t.Fatalf("record = %+v", r)
	}
	if create.Content.Kind != durable.ContentBase || create.Content.Version != 1 ||
		jsonOf(t, create.Content.Value) != `{"items":[],"message":"hello","nested":{"count":0},"other":{"label":"x"}}` {
		t.Fatalf("content = %+v", create.Content)
	}
	snap := snapshot(t, ts.session, LiveDoc.Conversation(conversationID))
	if jsonOf(t, snap) != `{"items":[],"message":"hello","nested":{"count":0},"other":{"label":"x"}}` {
		t.Fatalf("snapshot = %v", snap)
	}
	publication := ts.lastPublication()
	published := documentChanges(publication)[0]
	if published.Record.CreatedAt != publication.Seq || !same(published.Value, snap) || published.ConversationID != conversationID {
		t.Fatalf("published = %+v", published)
	}
	admitted, _ := findWrite[durable.DocumentCreateWrite](ts.storage.lastAdmitted())
	if !same(admitted.Content.Value, published.Value) {
		t.Fatal("the admitted base is not the published value")
	}
}

func TestDocumentSnapshotNeverCreates(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	before := ts.storage.commitCount()
	if snapshot(t, ts.session, LiveDoc.Conversation(conversationID)) != nil ||
		snapshot(t, ts.session, CounterDoc.Session()) != nil ||
		snapshot(t, ts.session, MemberDoc.Session().Member("k", nil)) != nil {
		t.Fatal("an absent document has a snapshot")
	}
	if ts.storage.commitCount() != before || ts.storage.mints() != 1 {
		t.Fatalf("commits %d→%d, mints %d", before, ts.storage.commitCount(), ts.storage.mints())
	}
}

func TestDocumentSnapshotsAreSharedAndPriorRevisionsStable(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	first := snapshot(t, ts.session, ref)
	if !same(snapshot(t, ts.session, ref), first) {
		t.Fatal("snapshots of one revision differ")
	}
	commit(t, ts.session, func(tx *durable.Tx) error {
		return mustDoc(tx, ref).At("nested").Set("count", 1)
	})
	second := snapshot(t, ts.session, ref)
	if same(second, first) || get(first, "nested", "count") != 0.0 || get(second, "nested", "count") != 1.0 {
		t.Fatalf("first %v, second %v", first, second)
	}
	// Unchanged subtrees are structurally shared between immutable revisions.
	if !same(second["items"], first["items"]) || !same(second["other"], first["other"]) {
		t.Fatal("unchanged subtrees are not shared")
	}
}

func TestDocumentAdoptsByPointerSwapAndSharesOperationPayloads(t *testing.T) {
	ts, conversationID := setupLive(t)
	commit(t, ts.session, func(tx *durable.Tx) error {
		live := mustDoc(tx, LiveDoc.Conversation(conversationID))
		must(live.Set("other", map[string]any{"label": "y"}))
		_, err := live.At("items").Push("c")
		return err
	})
	snap := snapshot(t, ts.session, LiveDoc.Conversation(conversationID))
	published := documentChanges(ts.lastPublication())[0]
	if !same(published.Value, snap) {
		t.Fatal("the published value is not the snapshot")
	}
	admitted, _ := findWrite[durable.DocumentChangeWrite](ts.storage.lastAdmitted())
	if admitted.Content.Kind != durable.ContentDelta {
		t.Fatalf("content = %+v", admitted.Content)
	}
	if !same(published.Ops, admitted.Content.Ops) {
		t.Fatal("the published ops are not the admitted ops")
	}
	var set *delta.Set
	for _, op := range published.Ops {
		if s, ok := op.(delta.Set); ok {
			set = &s
		}
	}
	if set == nil || jsonOf(t, *set) != `["s",["other"],{"label":"y"}]` {
		t.Fatalf("set = %v", set)
	}
	// Trusted immutability: the Session makes no second copy of operation
	// payloads.
	if !same(set.Value, snap["other"]) {
		t.Fatal("the operation payload is a copy")
	}
}

func TestDocumentCopiesAssignedValuesPerPlacement(t *testing.T) {
	ts, conversationID := setupLive(t)
	value := map[string]any{"label": "shared"}
	commit(t, ts.session, func(tx *durable.Tx) error {
		live := mustDoc(tx, LiveDoc.Conversation(conversationID))
		must(live.Set("other", value))
		must(live.Set("copy", value))
		value["label"] = "mutated"
		return nil
	})
	snap := snapshot(t, ts.session, LiveDoc.Conversation(conversationID))
	if jsonOf(t, snap["other"]) != `{"label":"shared"}` || jsonOf(t, snap["copy"]) != `{"label":"shared"}` {
		t.Fatalf("snapshot = %v", snap)
	}
	if same(snap["copy"], snap["other"]) || same(snap["other"], value) {
		t.Fatal("placements share a container")
	}
}

func TestDocumentSuppressesEmptyBatches(t *testing.T) {
	ts, conversationID := setupLive(t)
	commits, published := ts.storage.commitCount(), ts.publicationCount()
	commit(t, ts.session, func(tx *durable.Tx) error {
		live := mustDoc(tx, LiveDoc.Conversation(conversationID))
		must(live.At("nested").Set("count", 0))
		_, err := live.At("items").Push("z")
		must(err)
		_, _, err = live.At("items").Pop()
		return err
	})
	if ts.storage.commitCount() != commits || ts.publicationCount() != published {
		t.Fatal("an empty batch was written or published")
	}
}

func TestDocumentWritesReplayableStructuralNoOps(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	before := snapshot(t, ts.session, ref)
	commit(t, ts.session, func(tx *durable.Tx) error {
		items := mustDoc(tx, ref).At("items")
		first, _, err := items.Shift()
		must(err)
		_, err = items.Unshift(first)
		return err
	})
	change, ok := ts.storage.lastCommit()[0].(durable.DocumentChangeWrite)
	if !ok || change.Content.Kind != durable.ContentDelta {
		t.Fatalf("write = %+v", ts.storage.lastCommit()[0])
	}
	after := snapshot(t, ts.session, ref)
	if !reflect.DeepEqual(after, before) || same(after, before) {
		t.Fatal("the no-op did not produce an equal new revision")
	}
	if len(documentChanges(ts.lastPublication())[0].Ops) == 0 {
		t.Fatal("the no-op published no operations")
	}
}

func panics(f func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	f()
	return false
}

func TestDocumentRevokesEscapedDrafts(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	var escaped, items *delta.Draft
	commit(t, ts.session, func(tx *durable.Tx) error {
		escaped = mustDoc(tx, ref)
		items = escaped.At("items")
		return escaped.Set("message", "inside")
	})
	if !panics(func() { escaped.Get("message") }) || escaped.Set("message", "outside") == nil {
		t.Fatal("the escaped draft is usable")
	}
	if !panics(func() { items.Len() }) {
		t.Fatal("the escaped member draft is readable")
	}
	if _, err := items.Push("outside"); err == nil {
		t.Fatal("the escaped member draft is writable")
	}
	if snapshot(t, ts.session, ref)["message"] != "inside" {
		t.Fatal("the message was not committed")
	}
	returned, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (*delta.Draft, error) { return tx.Doc(ref) })
	if err != nil {
		t.Fatal(err)
	}
	if !panics(func() { returned.Get("message") }) {
		t.Fatal("the returned draft is readable")
	}
}

func TestDocumentAbortsEveryChangeWhenTheCallbackFails(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	before := snapshot(t, ts.session, ref)
	commits := ts.storage.commitCount()
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		must(mustDoc(tx, ref).Set("message", "lost"))
		must(mustDoc(tx, CounterDoc.Session()).Set("count", 5))
		return errors.New("callback failed")
	})
	errorContains(t, err, "callback failed")
	if ts.storage.commitCount() != commits || !same(snapshot(t, ts.session, ref), before) || snapshot(t, ts.session, CounterDoc.Session()) != nil {
		t.Fatal("a failed callback changed state")
	}
	commit(t, ts.session, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("message", "kept") })
	if snapshot(t, ts.session, ref)["message"] != "kept" {
		t.Fatal("the next commit was lost")
	}
}

func TestDocumentMemoizesConcurrentDuplicateAcquisition(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	ref := LiveDoc.Conversation(conversationID)
	initCount, mints := liveInitCount.Load(), ts.storage.mints()
	commit(t, ts.session, func(tx *durable.Tx) error {
		var first, second *delta.Draft
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); first = mustDoc(tx, ref) }()
		go func() { defer wg.Done(); second = mustDoc(tx, ref) }()
		wg.Wait()
		if first != second || mustDoc(tx, ref) != first {
			t.Error("duplicate acquisitions differ")
		}
		return first.Set("message", "once")
	})
	if liveInitCount.Load() != initCount+1 || ts.storage.mints() != mints+1 {
		t.Fatalf("initialized %d, minted %d", liveInitCount.Load()-initCount, ts.storage.mints()-mints)
	}
	if countWrites[durable.DocumentCreateWrite](ts.storage.lastCommit()) != 1 {
		t.Fatal("not one create")
	}
}

func TestDocumentUsesTheFirstFamilySeed(t *testing.T) {
	ts := openTestSession(t)
	seedsMu.Lock()
	seeds = nil
	seedsMu.Unlock()
	hit := func(d *delta.Draft) error {
		hits, _ := d.Get("hits")
		return d.Set("hits", hits.(float64)+1)
	}
	commit(t, ts.session, func(tx *durable.Tx) error {
		first := mustDoc(tx, MemberDoc.Session().Member("k", "first"))
		if second := mustDoc(tx, MemberDoc.Session().Member("k", "second")); second != first {
			t.Error("a second seed made another draft")
		}
		return hit(first)
	})
	commit(t, ts.session, func(tx *durable.Tx) error {
		must(hit(mustDoc(tx, MemberDoc.Session().Member("k", "third"))))
		return hit(mustDoc(tx, MemberDoc.Session().Member("other", "fourth")))
	})
	if !reflect.DeepEqual(seeds, []string{"first", "fourth"}) {
		t.Fatalf("seeds = %v", seeds)
	}
	if jsonOf(t, snapshot(t, ts.session, MemberDoc.Session().Member("k", nil))) != `{"hits":2,"seed":"first"}` ||
		jsonOf(t, snapshot(t, ts.session, MemberDoc.Session().Member("other", nil))) != `{"hits":1,"seed":"fourth"}` {
		t.Fatal("members differ")
	}
}

func TestDocumentRejectsASucceedingCallbackWithAPendingAcquisition(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	must(ts.session.UnloadDocuments())
	g := ts.storage.holdFindDocument()
	commits := ts.storage.commitCount()
	var pending <-chan error
	var captured *durable.Tx
	result := async(func() error {
		return ts.session.Commit(bg, func(tx *durable.Tx) error {
			pending = async(func() error { _, err := tx.Doc(ref); return err })
			captured = tx
			<-g.entered // the acquisition is registered and waits on Storage
			return nil
		})
	})
	g.Entered(t)
	eventually(t, func() bool { return durable.TxSealed(captured) })
	select {
	case <-result:
		t.Fatal("the commit settled while its acquisition was pending")
	case <-time.After(20 * time.Millisecond):
	}
	g.Release()
	errorContains(t, wait(t, result), "pending Tx operations")
	errorContains(t, wait(t, pending), "Transaction has settled")
	if ts.storage.commitCount() != commits {
		t.Fatal("the rejected commit wrote")
	}
	commit(t, ts.session, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("message", "after") })
	if snapshot(t, ts.session, ref)["message"] != "after" {
		t.Fatal("the next commit was lost")
	}
}

func TestDocumentLateAbsentAcquisitionNeitherInitializesNorMints(t *testing.T) {
	ts := openTestSession(t)
	var initialized atomic.Int64
	lateDoc := durable.MustDefineDoc(durable.DocDef{
		Kind: "test.late", Version: 1, Scope: durable.ScopeSession,
		Initial: func(any) map[string]any { initialized.Add(1); return map[string]any{"count": 0} },
	})
	g := ts.storage.holdFindDocument()
	mints := ts.storage.mints()
	var pending <-chan error
	var captured *durable.Tx
	result := async(func() error {
		return ts.session.Commit(bg, func(tx *durable.Tx) error {
			pending = async(func() error { _, err := tx.Doc(lateDoc.Session()); return err })
			captured = tx
			<-g.entered // the acquisition is registered and waits on Storage
			return nil
		})
	})
	g.Entered(t)
	eventually(t, func() bool { return durable.TxSealed(captured) })
	g.Release()
	errorContains(t, wait(t, result), "pending Tx operations")
	errorContains(t, wait(t, pending), "Transaction has settled")
	if initialized.Load() != 0 || ts.storage.mints() != mints {
		t.Fatalf("initialized %d, minted %d", initialized.Load(), ts.storage.mints()-mints)
	}
}

func TestDocumentFailingCallbackWithAPendingAcquisitionReturnsItsError(t *testing.T) {
	ts, conversationID := setupLive(t)
	must(ts.session.UnloadDocuments())
	g := ts.storage.holdFindDocument()
	var pending <-chan error
	var captured *durable.Tx
	result := async(func() error {
		return ts.session.Commit(bg, func(tx *durable.Tx) error {
			pending = async(func() error { _, err := tx.Doc(LiveDoc.Conversation(conversationID)); return err })
			captured = tx
			<-g.entered // the acquisition is registered and waits on Storage
			return errors.New("callback failed")
		})
	})
	g.Entered(t)
	eventually(t, func() bool { return durable.TxSealed(captured) })
	g.Release()
	errorContains(t, wait(t, result), "callback failed")
	errorContains(t, wait(t, pending), "Transaction has settled")
}

func TestDocumentRejectsTxUseAfterTheCallbackSettles(t *testing.T) {
	ts, conversationID := setupLive(t)
	var captured *durable.Tx
	commit(t, ts.session, func(tx *durable.Tx) error { captured = tx; return nil })
	_, err := captured.Doc(LiveDoc.Conversation(conversationID))
	errorContains(t, err, "Transaction has settled")
	_, err = captured.Conversation(conversationID)
	errorContains(t, err, "Transaction has settled")
	errorContains(t, captured.SetTask(durable.TaskRecord{}), "Transaction has settled")
}

func TestDocumentRejectsDefinitionsDisagreeingWithTheStoredIncarnation(t *testing.T) {
	ts, conversationID := setupLive(t)
	_, err := ts.session.Snapshot(bg, RewindableLiveDoc.Conversation(conversationID))
	errorContains(t, err, "does not match the supplied definition semantics")
	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.Doc(RewindableLiveDoc.Conversation(conversationID))
		return err
	})
	errorContains(t, err, "does not match the supplied definition semantics")
	err = ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(LiveDocV2.Conversation(conversationID)); return err })
	errorContains(t, err, "requires migration from version 1")
}

func TestDocumentRejectsNonJSONValuesBeforeAdmission(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	dateDoc := durable.MustDefineDoc(durable.DocDef{
		Kind: "test.date", Version: 1, Scope: durable.ScopeSession,
		Initial: func(any) map[string]any { return map[string]any{"at": time.Now()} },
	})
	err := ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(dateDoc.Session()); return err })
	errorContains(t, err, "strict JSON")
	commits := ts.storage.commitCount()
	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := mustDoc(tx, LiveDoc.Conversation(conversationID)).At("items").Push(func() {})
		return err
	})
	errorContains(t, err, "strict JSON")
	if ts.storage.commitCount() != commits || snapshot(t, ts.session, LiveDoc.Conversation(conversationID)) != nil {
		t.Fatal("a rejected value was committed")
	}
}

func TestDocumentRollsBackPreparedDocumentsWhenAssemblyFails(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	commit(t, ts.session, func(tx *durable.Tx) error { return mustDoc(tx, CounterDoc.Session()).Set("count", 1) })
	live, counter := snapshot(t, ts.session, ref), snapshot(t, ts.session, CounterDoc.Session())
	commits := ts.storage.commitCount()
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		must(mustDoc(tx, ref).Set("message", "lost"))
		must(mustDoc(tx, CounterDoc.Session()).Set("count", 2))
		// Replacing a missing task fails at assembly, after every change was
		// prepared.
		return tx.SetTask(durable.TaskRecord{
			ID: 999, ConversationID: conversationID, Kind: "missing", Version: 1, Input: json.RawMessage("null"),
			State: durable.TaskState{Status: durable.TaskPending, Checkpoint: json.RawMessage(`{"phase":"start"}`)},
		})
	})
	errorContains(t, err, "Task 999 does not exist")
	if ts.storage.commitCount() != commits || !same(snapshot(t, ts.session, ref), live) || !same(snapshot(t, ts.session, CounterDoc.Session()), counter) {
		t.Fatal("a failed assembly changed state")
	}
	commit(t, ts.session, func(tx *durable.Tx) error {
		must(mustDoc(tx, ref).Set("message", "next"))
		return mustDoc(tx, CounterDoc.Session()).Set("count", 3)
	})
	if snapshot(t, ts.session, CounterDoc.Session())["count"] != 3.0 {
		t.Fatal("the next commit was lost")
	}
}

func TestDocumentStorageFailureFailsTheSession(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	before := snapshot(t, ts.session, ref)
	published := ts.publicationCount()
	ts.storage.failNextCommit(errors.New("disk vanished"))
	err := ts.session.Commit(bg, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("message", "uncertain") })
	errorContains(t, err, "disk vanished")
	if ts.publicationCount() != published {
		t.Fatal("a failed commit was published")
	}
	if _, ok := before["message"]; ok {
		t.Fatal("the previous revision changed")
	}
	var failed *durable.SessionFailedError
	if _, err := ts.session.Snapshot(bg, ref); !errors.As(err, &failed) {
		t.Fatalf("snapshot error = %v", err)
	}
	if err := ts.session.Commit(bg, func(*durable.Tx) error { return nil }); !errors.As(err, &failed) {
		t.Fatalf("commit error = %v", err)
	}
	if err := ts.session.Close(bg); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentKeepsThePreviousRevisionThroughStorageSettlement(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	before := snapshot(t, ts.session, ref)
	copied := jsonOf(t, before)
	g := ts.storage.holdCommits()
	result := async(func() error {
		return ts.session.Commit(bg, func(tx *durable.Tx) error {
			live := mustDoc(tx, ref)
			_, err := live.At("items").Push("c")
			must(err)
			return live.At("nested").Set("count", 9)
		})
	})
	g.Entered(t)
	if !same(snapshot(t, ts.session, ref), before) || jsonOf(t, before) != copied {
		t.Fatal("the previous revision changed before adoption")
	}
	g.Release()
	must(wait(t, result))
	if jsonOf(t, snapshot(t, ts.session, ref)["items"]) != `["a","b","c"]` || jsonOf(t, before) != copied {
		t.Fatal("adoption changed the previous revision")
	}
}

func TestDocumentRetiresAndCreatesANewIncarnationAtTheSameAddress(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	oldID := documentChanges(ts.lastPublication())[0].Record.ID
	commit(t, ts.session, func(tx *durable.Tx) error {
		live := mustDoc(tx, ref)
		must(live.Set("message", "final"))
		must(tx.RetireDoc(ref))
		replacement := mustDoc(tx, ref)
		if replacement == live {
			t.Error("the replacement is the retired draft")
		}
		return replacement.Set("message", "new")
	})
	writes := ts.storage.lastCommit()
	change, _ := findWrite[durable.DocumentChangeWrite](writes)
	retire, _ := findWrite[durable.DocumentRetireWrite](writes)
	create, _ := findWrite[durable.DocumentCreateWrite](writes)
	if len(writes) != 3 || change.ID != oldID || retire.ID != oldID || create.Record.Kind != "test.live" {
		t.Fatalf("writes = %+v", writes)
	}
	publication := ts.lastPublication()
	changes := documentChanges(publication)
	retired, created := changes[0], changes[1]
	if retired.Record.ID != oldID || retired.Value != nil || len(retired.Ops) != 0 || retired.Record.RetiredAt == nil || *retired.Record.RetiredAt != publication.Seq {
		t.Fatalf("retired = %+v", retired)
	}
	if created.Record.ID == oldID || created.Record.CreatedAt != publication.Seq || created.Value["message"] != "new" ||
		jsonOf(t, created.Value["items"]) != `[]` || len(created.Ops) != 0 {
		t.Fatalf("created = %+v", created)
	}
	if !same(snapshot(t, ts.session, ref), created.Value) {
		t.Fatal("the snapshot is not the new incarnation")
	}
	commit(t, ts.session, func(tx *durable.Tx) error { return tx.RetireDoc(ref) })
	if snapshot(t, ts.session, ref) != nil {
		t.Fatal("a retired document has a snapshot")
	}
	must(ts.session.UnloadDocuments())
	if snapshot(t, ts.session, ref) != nil {
		t.Fatal("a retired document loads")
	}
	// Retiring an absent address is a no-op.
	commits := ts.storage.commitCount()
	commit(t, ts.session, func(tx *durable.Tx) error { return tx.RetireDoc(ref) })
	if ts.storage.commitCount() != commits {
		t.Fatal("retiring an absent address wrote")
	}
}

func TestDocumentRetiresWithoutAcquisitionAndRecreates(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	oldID := documentChanges(ts.lastPublication())[0].Record.ID
	must(ts.session.UnloadDocuments())
	// The retirement is registered and waits on Storage while the
	// replacement is acquired, as pi's concurrent calls interleave.
	retireAndRecreate := func(ref durable.DocRef, mutate func(*delta.Draft) error) {
		g := ts.storage.holdFindDocument()
		commit(t, ts.session, func(tx *durable.Tx) error {
			retired := async(func() error { return tx.RetireDoc(ref) })
			g.Entered(t)
			replacement := mustDoc(tx, ref)
			g.Release()
			must(wait(t, retired))
			return mutate(replacement)
		})
	}
	retireAndRecreate(ref, func(d *delta.Draft) error { return d.Set("message", "replacement") })
	writes := ts.storage.lastCommit()
	retire, _ := findWrite[durable.DocumentRetireWrite](writes)
	create, _ := findWrite[durable.DocumentCreateWrite](writes)
	if len(writes) != 2 || retire.ID != oldID || create.Record.Kind != "test.live" {
		t.Fatalf("writes = %+v", writes)
	}
	absent := MemberDoc.Session().Member("absent", "seed")
	retireAndRecreate(absent, func(d *delta.Draft) error { return d.Set("hits", 1) })
	writes = ts.storage.lastCommit()
	if countWrites[durable.DocumentRetireWrite](writes) != 0 || countWrites[durable.DocumentCreateWrite](writes) != 1 {
		t.Fatalf("writes = %+v", writes)
	}
	if jsonOf(t, snapshot(t, ts.session, absent)) != `{"hits":1,"seed":"seed"}` {
		t.Fatal("the member differs")
	}
}

func TestDocumentRetirementRacingAPendingAcquisitionRetiresTheExistingIncarnation(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	oldID := documentChanges(ts.lastPublication())[0].Record.ID
	must(ts.session.UnloadDocuments())
	g := ts.storage.holdFindDocument()
	commit(t, ts.session, func(tx *durable.Tx) error {
		var acquired *delta.Draft
		acquisition := async(func() (err error) { acquired, err = tx.Doc(ref); return })
		g.Entered(t)
		retired := async(func() error { return tx.RetireDoc(ref) })
		eventually(t, func() bool { return durable.TxPending(tx) == 2 })
		g.Release()
		must(wait(t, acquisition))
		must(acquired.Set("message", "final"))
		return wait(t, retired)
	})
	writes := ts.storage.lastCommit()
	change, _ := findWrite[durable.DocumentChangeWrite](writes)
	retire, _ := findWrite[durable.DocumentRetireWrite](writes)
	if len(writes) != 2 || change.ID != oldID || change.Content.Kind != durable.ContentDelta || retire.ID != oldID {
		t.Fatalf("writes = %+v", writes)
	}
	if snapshot(t, ts.session, ref) != nil {
		t.Fatal("the retired document has a snapshot")
	}

	commit(t, ts.session, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("message", "second") })
	secondID := documentChanges(ts.lastPublication())[0].Record.ID
	must(ts.session.UnloadDocuments())
	g = ts.storage.holdFindDocument()
	commit(t, ts.session, func(tx *durable.Tx) error {
		var old *delta.Draft
		acquisition := async(func() (err error) { old, err = tx.Doc(ref); return })
		g.Entered(t)
		retired := async(func() error { return tx.RetireDoc(ref) })
		eventually(t, func() bool { return durable.TxPending(tx) == 2 })
		fresh := mustDoc(tx, ref)
		g.Release()
		must(wait(t, acquisition))
		must(wait(t, retired))
		if fresh == old {
			t.Error("the recreation is the retired draft")
		}
		return fresh.Set("message", "third")
	})
	writes = ts.storage.lastCommit()
	retire, _ = findWrite[durable.DocumentRetireWrite](writes)
	create, _ := findWrite[durable.DocumentCreateWrite](writes)
	if len(writes) != 2 || retire.ID != secondID || create.Record.Kind != "test.live" {
		t.Fatalf("writes = %+v", writes)
	}
	if snapshot(t, ts.session, ref)["message"] != "third" {
		t.Fatal("the recreation was lost")
	}
}

func TestDocumentReloadsAnUnloadedDocument(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	loaded := snapshot(t, ts.session, ref)
	must(ts.session.UnloadDocuments())
	reloaded := snapshot(t, ts.session, ref)
	if same(reloaded, loaded) || !reflect.DeepEqual(reloaded, loaded) {
		t.Fatal("the reload differs or is the cached value")
	}
	commit(t, ts.session, func(tx *durable.Tx) error { _, err := mustDoc(tx, ref).At("items").Push("c"); return err })
	must(ts.session.UnloadDocuments())
	if jsonOf(t, snapshot(t, ts.session, ref)["items"]) != `["a","b","c"]` {
		t.Fatal("the change did not persist")
	}
}

type ctxKey struct{}

func TestDocumentDeliversCompletePublicationsSynchronously(t *testing.T) {
	ts, conversationID := setupLive(t)
	published := ts.publicationCount()
	commitCtx := context.WithValue(bg, ctxKey{}, "commit")
	var listenerCtx context.Context
	unsubscribe, err := ts.session.SubscribeCommits(func(_ durable.CommitPublication, ctx context.Context) { listenerCtx = ctx })
	must(err)
	defer unsubscribe()
	var result string
	err = ts.session.Commit(commitCtx, func(tx *durable.Tx) error {
		result = "done"
		return mustDoc(tx, LiveDoc.Conversation(conversationID)).Set("message", "m")
	})
	must(err)
	if result != "done" || listenerCtx != commitCtx || ts.publicationCount() != published+1 {
		t.Fatal("the publication was not delivered within the commit")
	}
}

func TestSessionPublishesCloseSynchronously(t *testing.T) {
	ts := openTestSession(t)
	var calls []string
	_, err := ts.session.SubscribeClose(func() { calls = append(calls, "active") })
	must(err)
	unsubscribe, err := ts.session.SubscribeClose(func() { calls = append(calls, "removed") })
	must(err)
	unsubscribe()
	must(ts.session.Close(bg))
	if !reflect.DeepEqual(calls, []string{"active"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestSessionSettlesAdmittedCommitsBeforeClose(t *testing.T) {
	ts, conversationID := setupLive(t)
	ref := LiveDoc.Conversation(conversationID)
	g := ts.storage.holdCommits()
	admitted := async(func() error {
		return ts.session.Commit(bg, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("message", "admitted") })
	})
	g.Entered(t)
	queued := async(func() error {
		return ts.session.Commit(bg, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("message", "queued") })
	})
	eventually(t, func() bool { return durable.LineWaiting(ts.session) == 1 })
	var absent map[string]any
	admittedSnapshot := async(func() (err error) {
		absent, err = ts.session.Snapshot(bg, MemberDoc.Session().Member("absent", nil))
		return
	})
	eventually(t, func() bool { return durable.LineWaiting(ts.session) == 2 })
	closed := async(func() error { return ts.session.Close(bg) })
	eventually(t, func() bool { return durable.Closing(ts.session) })
	errorContains(t, ts.session.Commit(bg, func(*durable.Tx) error { return nil }), "closed")
	_, err := ts.session.Snapshot(bg, ref)
	errorContains(t, err, "closed")
	g.Release()
	must(wait(t, admitted))
	must(wait(t, queued))
	must(wait(t, admittedSnapshot))
	if absent != nil {
		t.Fatal("an absent member has a snapshot")
	}
	must(wait(t, closed))
	if _, err := ts.storage.FindDocument(bg, durable.DocumentAddress{Kind: "test.live", Scope: durable.ConversationScope(conversationID)}, durable.Current); err == nil {
		t.Fatal("Storage is still open")
	}
}
