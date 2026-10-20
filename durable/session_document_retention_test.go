package durable_test

import (
	"errors"
	"math"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/session-document-retention.test.ts: the Session's
// document cache is bounded by use. Unattached documents leave it after the
// retention period and load from Storage again; documents a watch or a
// document state observes never leave it. The cases that need the harness
// (a settings change, the same events with and without eviction) come with
// it; pi's timers without unref and non-numeric retentions have no Go form.

var retentionCounter = durable.MustDefineDoc(durable.DocDef{
	CheckpointWhen: durable.NeverCheckpoint,
	Kind:           "test.retention.counter", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryLatest, Fork: durable.ForkInitial,
	Initial: func(any) map[string]any { return map[string]any{"count": 0} },
})

var retentionRewindable = durable.MustDefineDoc(durable.DocDef{
	CheckpointWhen: durable.NeverCheckpoint,
	Kind:           "test.retention.rewindable", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf,
	Initial: func(any) map[string]any { return map[string]any{"count": 0} },
})

// retentionEvent is a third-party conversation family: one member per event.
var retentionEvent = durable.MustDefineDoc(durable.DocDef{
	CheckpointWhen: durable.NeverCheckpoint,
	Kind:           "test.retention.event", Version: 1, Family: true, Scope: durable.ScopeConversation, History: durable.HistoryLatest, Fork: durable.ForkInitial,
	Initial: func(seq any) map[string]any { return map[string]any{"seq": seq} },
})

var retentionSessionCounter = sessionDoc(durable.DocDef{
	Kind: "test.retention.session-counter", Initial: func(any) map[string]any { return map[string]any{"count": 0} },
})

// retentionFailing has a checkpoint predicate that panics, so a commit that
// changes it fails while it settles.
var retentionFailing = sessionDoc(durable.DocDef{
	Kind: "test.retention.failing", Initial: func(any) map[string]any { return map[string]any{"count": 0} },
	CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { panic(errors.New("checkpoint failed")) },
})

// fakeClock is a controllable wall clock in milliseconds.
type fakeClock struct{ ms atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ms.Store(1_000)
	return c
}

func (c *fakeClock) now() time.Time   { return time.UnixMilli(c.ms.Load()) }
func (c *fakeClock) advance(ms int64) { c.ms.Add(ms) }
func fixedRetention(d time.Duration) func() time.Duration {
	return func() time.Duration { return d }
}

type retentionSession struct {
	storage *controlledStorage
	session *durable.Session
	clock   *fakeClock
}

// openRetention opens a Session with a controllable clock and retention.
func openRetention(t *testing.T, retention time.Duration) *retentionSession {
	t.Helper()
	r := &retentionSession{storage: newControlledStorage(), clock: newFakeClock()}
	r.session = durable.NewSession(r.storage, durable.SessionOptions{Now: r.clock.now, DocumentRetention: fixedRetention(retention)})
	t.Cleanup(func() { _ = r.session.Close(bg) })
	return r
}

func cached(s *durable.Session) int {
	c, _, _, _ := s.DocumentCacheSizes()
	return c
}

// settleLine lets the line run what is queued on it: an eviction job, then
// return.
func settleLine(t *testing.T, s *durable.Session) {
	t.Helper()
	must(s.ReadOnLine(func() error { return nil }))
}

func bump(t *testing.T, s *durable.Session, conversationID durable.ConversationID, by int) {
	t.Helper()
	commit(t, s, func(tx *durable.Tx) error {
		d := mustDoc(tx, retentionCounter.Conversation(conversationID))
		count, _ := d.Get("count")
		return d.Set("count", count.(float64)+float64(by))
	})
}

func emptyCommit(t *testing.T, s *durable.Session) {
	t.Helper()
	commit(t, s, func(*durable.Tx) error { return nil })
}

func evictedEventually(t *testing.T, s *durable.Session) {
	t.Helper()
	eventually(t, func() bool { settleLine(t, s); return cached(s) == 0 })
}

func TestRetentionEvictsAnUnattachedDocumentOnceItsRetentionPassed(t *testing.T) {
	r := openRetention(t, time.Minute)
	conversationID := createConversation(t, r.session)
	bump(t, r.session, conversationID, 3)
	if cached(r.session) == 0 {
		t.Fatal("nothing is cached")
	}
	// Not yet: inside the retention period.
	r.clock.advance(59_000)
	bump(t, r.session, conversationID, 0)
	settleLine(t, r.session)
	if cached(r.session) == 0 {
		t.Fatal("evicted inside the retention period")
	}
	// Past it, with no use since: the next commit evicts it.
	r.clock.advance(61_000)
	emptyCommit(t, r.session)
	touch(t, r.session, retentionSessionCounter.Session())
	settleLine(t, r.session)
	reads := r.storage.documentReads()
	expectJSON(t, snapshot(t, r.session, retentionCounter.Conversation(conversationID)), `{"count":3}`)
	if r.storage.documentReads() <= reads {
		t.Fatal("the evicted document did not load from Storage")
	}
}

func TestRetentionZeroEvictsRightAfterUse(t *testing.T) {
	r := openRetention(t, 0)
	conversationID := createConversation(t, r.session)
	bump(t, r.session, conversationID, 1)
	evictedEventually(t, r.session)
	expectJSON(t, snapshot(t, r.session, retentionCounter.Conversation(conversationID)), `{"count":1}`)
}

func TestRetentionNeverEvictsAnObservedDocument(t *testing.T) {
	r := openRetention(t, time.Second)
	conversationID := createConversation(t, r.session)
	ref := retentionCounter.Conversation(conversationID)
	bump(t, r.session, conversationID, 1)
	watch := watchDoc(t, r.session, ref)
	state := documentState(t, r.session, ref)
	seen := newRecorder(false)
	must(watch.Start(seen.listener))
	r.clock.advance(1_000_000)
	bump(t, r.session, conversationID, 1)
	settleLine(t, r.session)
	// Still cached: a read long after its retention passed loads nothing.
	eventually(t, func() bool { return get(state.Value(), "count") == 2.0 })
	eventually(t, func() bool { return len(seen.all()) == 1 })
	reads := r.storage.documentReads()
	r.clock.advance(1_000_000)
	touch(t, r.session, retentionSessionCounter.Session())
	settleLine(t, r.session)
	expectJSON(t, snapshot(t, r.session, ref), `{"count":2}`)
	if r.storage.documentReads() != reads {
		t.Fatal("an observed document was evicted")
	}
	watch.Stop()
	r.clock.advance(1_000_000)
	bump(t, r.session, conversationID, 1)
	settleLine(t, r.session)
	eventually(t, func() bool { return get(state.Value(), "count") == 3.0 })
	state.Dispose()
	r.clock.advance(1_000_000)
	emptyCommit(t, r.session)
	touch(t, r.session, retentionSessionCounter.Session())
	settleLine(t, r.session)
	// The session document was just used; the counter is gone.
	if n := cached(r.session); n != 1 {
		t.Fatalf("cached = %d", n)
	}
}

func TestRetentionNeverTakesATrackerFromATransactionThatHoldsIt(t *testing.T) {
	r := openRetention(t, 0)
	conversationID := createConversation(t, r.session)
	ref := retentionCounter.Conversation(conversationID)
	bump(t, r.session, conversationID, 1)
	inside, release := make(chan struct{}), make(chan struct{})
	result := async(func() error {
		return r.session.Commit(bg, func(tx *durable.Tx) error {
			draft := mustDoc(tx, ref)
			close(inside)
			<-release
			count, _ := draft.Get("count")
			return draft.Set("count", count.(float64)+10)
		})
	})
	<-inside
	// Retention passes while the transaction holds the document; eviction
	// waits on the line behind it.
	r.clock.advance(1_000_000)
	close(release)
	must(wait(t, result))
	expectJSON(t, snapshot(t, r.session, ref), `{"count":11}`)
	bump(t, r.session, conversationID, 1)
	expectJSON(t, snapshot(t, r.session, ref), `{"count":12}`)
}

func TestRetentionWritesAfterAReloadContinueTheStoredIncarnation(t *testing.T) {
	r := openRetention(t, 0)
	conversationID := createConversation(t, r.session)
	for range 5 {
		bump(t, r.session, conversationID, 1)
		settleLine(t, r.session)
	}
	expectJSON(t, snapshot(t, r.session, retentionCounter.Conversation(conversationID)), `{"count":5}`)
	// Every bump after the first changes the same incarnation.
	creations := 0
	r.storage.mu.Lock()
	for _, writes := range r.storage.commits {
		for _, w := range writes {
			if c, ok := w.(durable.DocumentCreateWrite); ok && c.Record.Kind == "test.retention.counter" {
				creations++
			}
		}
	}
	r.storage.mu.Unlock()
	if creations != 1 {
		t.Fatalf("creations = %d", creations)
	}
	must(r.session.Close(bg))
	// A fresh Session over the same storage reads the same value.
	reopened := durable.NewSession(r.storage.reopen(), durable.SessionOptions{DocumentRetention: fixedRetention(0)})
	defer reopened.Close(bg)
	expectJSON(t, snapshot(t, reopened, retentionCounter.Conversation(conversationID)), `{"count":5}`)
}

func TestRetentionKeepsHistoryAndForksOfRewindableDocumentsAfterEviction(t *testing.T) {
	r := openRetention(t, 0)
	conversationID := createConversation(t, r.session)
	ref := retentionRewindable.Conversation(conversationID)
	note := func(count int) durable.EntryID {
		id, err := durable.CommitValue(bg, r.session, func(tx *durable.Tx) (durable.EntryID, error) {
			must(mustDoc(tx, ref).Set("count", count))
			e, err := tx.AppendEntry(conversationID, durable.EntryDraft{Kind: "note", Data: []byte("null")})
			return e.ID, err
		})
		must(err)
		settleLine(t, r.session)
		return id
	}
	first, second := note(1), note(2)
	for at, want := range map[durable.EntryID]string{first: `{"count":1}`, second: `{"count":2}`} {
		v, err := r.session.SnapshotAsOf(bg, ref, at)
		must(err)
		expectJSON(t, v, want)
	}
	child := fork(t, r.session, conversationID, first)
	settleLine(t, r.session)
	expectJSON(t, snapshot(t, r.session, retentionRewindable.Conversation(child)), `{"count":1}`)
	expectJSON(t, snapshot(t, r.session, ref), `{"count":2}`)
}

func TestRetentionBoundsAThirdPartyFamilyWithAMemberPerEvent(t *testing.T) {
	r := openRetention(t, 0)
	conversationID := createConversation(t, r.session)
	for seq := range 200 {
		commit(t, r.session, func(tx *durable.Tx) error {
			_, err := tx.Doc(retentionEvent.Conversation(conversationID).Member(strconv.Itoa(seq), seq))
			return err
		})
	}
	evictedEventually(t, r.session)
	expectJSON(t, snapshot(t, r.session, retentionEvent.Conversation(conversationID).Member("123", nil)), `{"seq":123}`)
}

func TestRetentionKeepsALongLivedCacheBoundedAcrossManyConversations(t *testing.T) {
	r := openRetention(t, 10*time.Second)
	for range 300 {
		conversationID := createConversation(t, r.session)
		bump(t, r.session, conversationID, 1)
		r.clock.advance(1_000)
	}
	settleLine(t, r.session)
	// Only what was used in the last 10 s, about ten conversations' worth.
	if n := cached(r.session); n > 12 {
		t.Fatalf("cached = %d", n)
	}
}

func TestRetentionAPanickingClockNeverChangesACommitsOutcome(t *testing.T) {
	clock := newFakeClock()
	var fail atomic.Bool
	storage := newControlledStorage()
	session := durable.NewSession(storage, durable.SessionOptions{
		Now: func() time.Time {
			if fail.Load() {
				panic(errors.New("clock"))
			}
			return clock.now()
		},
		DocumentRetention: fixedRetention(0),
	})
	defer session.Close(bg)
	conversationID := createConversation(t, session)
	fail.Store(true)
	bump(t, session, conversationID, 42)
	fail.Store(false)
	expectJSON(t, snapshot(t, session, retentionCounter.Conversation(conversationID)), `{"count":42}`)
}

func TestRetentionEvictsAfterACommitWhoseSettlementFails(t *testing.T) {
	r := openRetention(t, 10*time.Second)
	conversationID := createConversation(t, r.session)
	bump(t, r.session, conversationID, 1)
	// Created first: the predicate runs only for changes to an existing
	// document.
	touch(t, r.session, retentionFailing.Session())
	settleLine(t, r.session)
	// Retention passes inside a commit whose checkpoint predicate panics
	// while it settles.
	err := r.session.Commit(bg, func(tx *durable.Tx) error {
		must(increment(tx, retentionFailing.Session(), "count"))
		r.clock.advance(1_000_000)
		return nil
	})
	errorContains(t, err, "checkpoint failed")
	if n := cached(r.session); n != 0 {
		t.Fatalf("cached = %d", n)
	}
}

func TestRetentionEvictsPromptlyOnceTheRetentionIsShortened(t *testing.T) {
	var retention atomic.Int64
	retention.Store(int64(10 * time.Minute))
	clock := newFakeClock()
	session := durable.NewSession(newControlledStorage(), durable.SessionOptions{
		Now:               clock.now,
		DocumentRetention: func() time.Duration { return time.Duration(retention.Load()) },
	})
	defer session.Close(bg)
	conversationID := createConversation(t, session)
	bump(t, session, conversationID, 1)
	retention.Store(0)
	clock.advance(2_000)
	// A read, no commit: the sweep pending for ten minutes from now moves up.
	snapshot(t, session, retentionCounter.Conversation(conversationID))
	evictedEventually(t, session)
}

func TestRetentionKeepsNoReplacementCachedForARetiredIncarnationsObserver(t *testing.T) {
	r := openRetention(t, 0)
	conversationID := createConversation(t, r.session)
	ref := retentionCounter.Conversation(conversationID)
	bump(t, r.session, conversationID, 1)
	state := documentState(t, r.session, ref)
	defer state.Dispose()
	commit(t, r.session, func(tx *durable.Tx) error { return tx.RetireDoc(ref) })
	eventually(t, func() bool { return state.Value() == nil })
	bump(t, r.session, conversationID, 5)
	evictedEventually(t, r.session)
	expectJSON(t, snapshot(t, r.session, ref), `{"count":5}`)
}

func TestRetentionLeavesNoPinWhenAnObserversLoadLosesARaceWithClose(t *testing.T) {
	r := openRetention(t, 0)
	conversationID := createConversation(t, r.session)
	bump(t, r.session, conversationID, 1)
	evictedEventually(t, r.session)
	g := r.storage.holdFindDocument()
	attaching := async(func() error {
		_, err := r.session.DocumentState(bg, retentionCounter.Conversation(conversationID))
		return err
	})
	g.Entered(t)
	closing := async(func() error { return r.session.Close(bg) })
	eventually(t, func() bool { return durable.Closing(r.session) })
	g.Release()
	if err := wait(t, attaching); err == nil {
		t.Fatal("the state attached after close began")
	}
	must(wait(t, closing))
	if c, idle, attached, _ := r.session.DocumentCacheSizes(); c != 0 || idle != 0 || attached != 0 {
		t.Fatalf("cached %d, idle %d, attached %d", c, idle, attached)
	}
}

func TestRetentionEvictsOnItsTimerWithoutACommit(t *testing.T) {
	session := durable.NewSession(newControlledStorage(), durable.SessionOptions{DocumentRetention: fixedRetention(20 * time.Millisecond)})
	defer session.Close(bg)
	conversationID := createConversation(t, session)
	bump(t, session, conversationID, 1)
	state := documentState(t, session, retentionCounter.Conversation(conversationID))
	// A detach called twice is tolerated.
	state.Dispose()
	state.Dispose()
	eventually(t, func() bool { return cached(session) == 0 })
}

func TestRetentionMigratesADocumentAgainAfterItsEviction(t *testing.T) {
	v1 := sessionDoc(durable.DocDef{Kind: "test.retention.migrating", Initial: func(any) map[string]any { return map[string]any{"count": 1} }})
	var migrations atomic.Int64
	v2 := sessionDoc(durable.DocDef{
		Kind: "test.retention.migrating", Version: 2, Initial: func(any) map[string]any { return map[string]any{"count": 0, "migrated": true} },
		Migrate: func(value map[string]any, _ int) map[string]any {
			migrations.Add(1)
			return map[string]any{"count": value["count"], "migrated": true}
		},
	})
	r := openRetention(t, 0)
	touch(t, r.session, v1.Session())
	settleLine(t, r.session)
	expectJSON(t, snapshot(t, r.session, v2.Session()), `{"count":1,"migrated":true}`)
	settleLine(t, r.session)
	expectJSON(t, snapshot(t, r.session, v2.Session()), `{"count":1,"migrated":true}`)
	// Not written back, so each cold load migrates again.
	if migrations.Load() != 2 {
		t.Fatalf("migrations = %d", migrations.Load())
	}
}

func TestRetentionTreatsANegativeRetentionAsTheDefaultAndAHugeOneAsNever(t *testing.T) {
	// The default as shipped, also in a run that changes it.
	previous := durable.SetDefaultDocumentRetention(10 * time.Minute)
	defer durable.SetDefaultDocumentRetention(previous)
	r := openRetention(t, -5*time.Millisecond)
	conversationID := createConversation(t, r.session)
	bump(t, r.session, conversationID, 1)
	r.clock.advance(60_000)
	emptyCommit(t, r.session)
	settleLine(t, r.session)
	// Not dropped at once, as a retention of 0 would: the ten-minute default
	// holds it.
	if n := cached(r.session); n != 1 {
		t.Fatalf("cached = %d", n)
	}
	never := openRetention(t, time.Duration(math.MaxInt64))
	conversationID = createConversation(t, never.session)
	bump(t, never.session, conversationID, 1)
	never.clock.advance(1e15)
	emptyCommit(t, never.session)
	settleLine(t, never.session)
	if n := cached(never.session); n != 1 {
		t.Fatalf("cached = %d", n)
	}
}

func TestRetentionKeepsANewerVersionCachedWhileAnOlderObserverIsAttached(t *testing.T) {
	v1 := sessionDoc(durable.DocDef{Kind: "test.retention.versioned", Initial: func(any) map[string]any { return map[string]any{"count": 3} }})
	v2 := sessionDoc(durable.DocDef{
		Kind: "test.retention.versioned", Version: 2, Initial: func(any) map[string]any { return map[string]any{"count": 0, "v2": true} },
		Migrate: func(value map[string]any, _ int) map[string]any {
			return map[string]any{"count": value["count"], "v2": true}
		},
	})
	r := openRetention(t, 0)
	touch(t, r.session, v1.Session())
	state := documentState(t, r.session, v1.Session())
	expectJSON(t, snapshot(t, r.session, v2.Session()), `{"count":3,"v2":true}`)
	settleLine(t, r.session)
	if n := cached(r.session); n != 1 {
		t.Fatalf("cached = %d", n)
	}
	state.Dispose()
	evictedEventually(t, r.session)
}

func TestRetentionQueuesOneSweepJobWhileACommitHoldsTheLine(t *testing.T) {
	r := openRetention(t, 0)
	var conversationIDs []durable.ConversationID
	for range 50 {
		id := createConversation(t, r.session)
		bump(t, r.session, id, 1)
		conversationIDs = append(conversationIDs, id)
	}
	evictedEventually(t, r.session)
	_, _, _, before := r.session.DocumentCacheSizes()
	// One commit loads 50 evicted documents, each touched as it loads; the
	// sweep job stays pending while the line is held.
	commit(t, r.session, func(tx *durable.Tx) error {
		for _, id := range conversationIDs {
			must(increment(tx, retentionCounter.Conversation(id), "count"))
			time.Sleep(time.Millisecond)
		}
		return nil
	})
	settleLine(t, r.session)
	c, _, _, after := r.session.DocumentCacheSizes()
	if c != 0 || after-before > 2 {
		t.Fatalf("cached %d, sweeps %d", c, after-before)
	}
}
