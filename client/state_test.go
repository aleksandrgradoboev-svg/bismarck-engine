package client

import (
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

func serverSnapshot(revision int64, sessions ...protocol.SessionMetadata) *protocol.ServerSnapshot {
	return &protocol.ServerSnapshot{
		ServerID: "srv", ProtocolVersion: protocol.ProtocolVersion,
		Revision: revision, Sessions: sessions,
	}
}

// metadata builds the durable session view a server snapshot now carries.
// Since 6189e53b3 it says nothing about attachment, which is the whole point
// of the tests below.
func metadata(id string) protocol.SessionMetadata {
	cwd := "/tmp"
	return protocol.SessionMetadata{ID: id, CreatedAt: 1, Cwd: &cwd}
}

func sessionSnapshot(id string, revision int64, attached bool) *protocol.SessionSnapshot {
	return &protocol.SessionSnapshot{
		ID: id, Cwd: "/tmp", CreatedAt: 1, UpdatedAt: 1,
		Phase: protocol.PhaseIdle, Model: protocol.ModelRef{Provider: "p", ID: "m"},
		ThinkingLevel: protocol.ThinkingOff, Attached: attached,
		Revision: revision, Transcript: []protocol.TranscriptItem{},
		QueuedSteer: []protocol.UserTranscriptItem{},
	}
}

func TestApplyServerSnapshotStoresAndNotifies(t *testing.T) {
	state := NewState(nil)
	var seen []int64
	state.Subscribe(func(s *protocol.ServerSnapshot) { seen = append(seen, s.Revision) })

	state.ApplyServerSnapshot(serverSnapshot(1, metadata("s1"), metadata("s2")))

	if got := state.Snapshot(); got == nil || got.Revision != 1 {
		t.Errorf("Snapshot() = %#v, want revision 1", got)
	}
	if len(seen) != 1 || seen[0] != 1 {
		t.Errorf("listener saw %v, want [1]", seen)
	}
	if got := state.Snapshot().Sessions; len(got) != 2 || got[0].ID != "s1" || got[1].ID != "s2" {
		t.Errorf("stored sessions = %#v, want s1 and s2", got)
	}
}

// TestApplyServerSnapshotIgnoresStale locks the revision guard: snapshots can
// arrive out of order behind a reconnect, and an older one must not clobber a
// newer view.
func TestApplyServerSnapshotIgnoresStale(t *testing.T) {
	state := NewState(nil)
	notifications := 0
	state.Subscribe(func(*protocol.ServerSnapshot) { notifications++ })

	state.ApplyServerSnapshot(serverSnapshot(5, metadata("s1")))
	state.ApplyServerSnapshot(serverSnapshot(4, metadata("s2")))

	if got := state.Snapshot(); got == nil || got.Revision != 5 {
		t.Errorf("Snapshot() = %#v, want revision 5", got)
	}
	// The stale snapshot must not have replaced the stored sessions either.
	if got := state.Snapshot().Sessions; len(got) != 1 || got[0].ID != "s1" {
		t.Errorf("stored sessions = %#v, want the revision-5 view", got)
	}
	if notifications != 1 {
		t.Errorf("listener called %d times, want 1", notifications)
	}
}

// TestApplyServerSnapshotKeepsAttachments mirrors pi's "keeps session leases
// attached across server metadata snapshots" (upstream 6189e53b3). This
// replaces the former RebuildsAttachments test, whose contract the same commit
// deleted: a server snapshot now carries durable metadata with no `attached`
// field, so it can no longer speak to attachment at all. Rebuilding from it
// would silently detach every live session.
func TestApplyServerSnapshotKeepsAttachments(t *testing.T) {
	state := NewState(nil)
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 1, true)})
	if !state.IsSessionAttached("s1") {
		t.Fatal("attach path did not record the attachment")
	}

	state.ApplyServerSnapshot(serverSnapshot(2, metadata("s1"), metadata("s2")))

	if !state.IsSessionAttached("s1") {
		t.Error("a server snapshot cleared an attachment it says nothing about")
	}
	if state.IsSessionAttached("s2") {
		t.Error("a server snapshot invented an attachment")
	}
}

func TestApplyEventSessionSnapshotUpdatesAndNotifies(t *testing.T) {
	state := NewState(nil)
	var sessionSeen []int64
	state.SubscribeSession("s1", func(s *protocol.SessionSnapshot) { sessionSeen = append(sessionSeen, s.Revision) })
	var events []string
	state.OnEvent(func(e protocol.ServerEvent) { events = append(events, e.EventType()) })

	state.ApplyEvent(&protocol.SessionSnapshotEvent{
		Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 3, true),
	})

	if got := state.SessionSnapshot("s1"); got == nil || got.Revision != 3 {
		t.Errorf("SessionSnapshot(s1) = %#v, want revision 3", got)
	}
	if !state.IsSessionAttached("s1") {
		t.Error("s1 should be attached after an attached snapshot")
	}
	if len(sessionSeen) != 1 || sessionSeen[0] != 3 {
		t.Errorf("session listener saw %v, want [3]", sessionSeen)
	}
	if len(events) != 1 || events[0] != "session_snapshot" {
		t.Errorf("event listener saw %v, want [session_snapshot]", events)
	}
}

func TestApplySessionSnapshotIgnoresStale(t *testing.T) {
	state := NewState(nil)
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 5, true)})
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 4, false)})

	if got := state.SessionSnapshot("s1"); got == nil || got.Revision != 5 {
		t.Errorf("SessionSnapshot = %#v, want revision 5", got)
	}
	if !state.IsSessionAttached("s1") {
		t.Error("stale session snapshot cleared the attachment")
	}
}

func TestApplyEventSessionRemovedDropsState(t *testing.T) {
	state := NewState(nil)
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 1, true)})
	state.ApplyEvent(&protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "s1"})

	if state.SessionSnapshot("s1") != nil {
		t.Error("snapshot survived session_removed")
	}
	if state.IsSessionAttached("s1") {
		t.Error("attachment survived session_removed")
	}
}

// TestApplyEventRoutesToSessionListeners: a per-session subscriber must see
// only its own session's events.
func TestApplyEventRoutesToSessionListeners(t *testing.T) {
	state := NewState(nil)
	var s1, s2 int
	state.OnSessionEvent("s1", func(protocol.ServerEvent) { s1++ })
	state.OnSessionEvent("s2", func(protocol.ServerEvent) { s2++ })

	state.ApplyEvent(&protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "s1"})
	state.ApplyEvent(&protocol.SessionProgressEvent{
		Type: "session_progress", SessionID: "s1",
		Progress: &protocol.AssistantDeltaProgress{
			Type: "assistant_delta", MessageID: "a1", ContentIndex: 0,
			Kind: protocol.DeltaText, Delta: "x",
		},
	})
	state.ApplyEvent(&protocol.ServerSnapshotEvent{Type: "server_snapshot", Snapshot: *serverSnapshot(1)})

	if s1 != 2 {
		t.Errorf("s1 listener called %d times, want 2", s1)
	}
	if s2 != 0 {
		t.Errorf("s2 listener called %d times, want 0", s2)
	}
}

func TestApplyResultListIsIgnored(t *testing.T) {
	state := NewState(nil)
	calls := 0
	state.Subscribe(func(*protocol.ServerSnapshot) { calls++ })
	state.ApplyResult(&protocol.ListResult{Command: "list", Sessions: []protocol.SessionMetadata{metadata("s1")}})

	if state.IsSessionAttached("s1") {
		t.Error("a list result must not change attachments")
	}
	if calls != 0 {
		t.Errorf("listener called %d times, want 0", calls)
	}
}

// TestApplyResultDetachForcesAttachedFalse locks the one place pi bypasses the
// revision guard: a detach result carries no new snapshot, so the cached one is
// force-updated to attached:false even though its revision did not move.
func TestApplyResultDetachForcesAttachedFalse(t *testing.T) {
	state := NewState(nil)
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 9, true)})
	var seen []bool
	state.SubscribeSession("s1", func(s *protocol.SessionSnapshot) { seen = append(seen, s.Attached) })

	state.ApplyResult(&protocol.DetachResult{Command: "detach", SessionID: "s1"})

	if state.IsSessionAttached("s1") {
		t.Error("s1 should be detached")
	}
	got := state.SessionSnapshot("s1")
	if got == nil || got.Attached {
		t.Errorf("cached snapshot = %#v, want attached false", got)
	}
	if got != nil && got.Revision != 9 {
		t.Errorf("revision = %d, want the original 9", got.Revision)
	}
	if len(seen) != 1 || seen[0] != false {
		t.Errorf("session listener saw %v, want [false]", seen)
	}
}

func TestApplyResultSessionUpdatesSnapshot(t *testing.T) {
	state := NewState(nil)
	state.ApplyResult(&protocol.SessionResult{Command: "attach", Session: *sessionSnapshot("s1", 2, true)})

	if got := state.SessionSnapshot("s1"); got == nil || got.Revision != 2 {
		t.Errorf("SessionSnapshot = %#v, want revision 2", got)
	}
	if !state.IsSessionAttached("s1") {
		t.Error("attach result should mark the session attached")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	state := NewState(nil)
	calls := 0
	unsubscribe := state.Subscribe(func(*protocol.ServerSnapshot) { calls++ })
	state.ApplyServerSnapshot(serverSnapshot(1))
	unsubscribe()
	state.ApplyServerSnapshot(serverSnapshot(2))

	if calls != 1 {
		t.Errorf("listener called %d times, want 1", calls)
	}
}

// TestUnsubscribeIsIdempotent: callers hold on to these and may call them from
// a defer as well as explicitly.
func TestUnsubscribeIsIdempotent(t *testing.T) {
	state := NewState(nil)
	first := state.Subscribe(func(*protocol.ServerSnapshot) {})
	second := state.Subscribe(func(*protocol.ServerSnapshot) {})
	first()
	first()
	if state.snapshotListeners.len() != 1 {
		t.Errorf("listener count = %d, want 1 (the second subscription)", state.snapshotListeners.len())
	}
	second()
}

// TestUnsubscribeSessionReleasesMapEntry: pi deletes the per-session bucket
// when its last listener leaves, so a long-lived client does not accumulate an
// entry per session it ever watched.
func TestUnsubscribeSessionReleasesMapEntry(t *testing.T) {
	state := NewState(nil)
	first := state.SubscribeSession("s1", func(*protocol.SessionSnapshot) {})
	second := state.SubscribeSession("s1", func(*protocol.SessionSnapshot) {})
	first()
	if len(state.sessionSnapshotListeners) != 1 {
		t.Errorf("bucket count = %d, want 1 while a listener remains", len(state.sessionSnapshotListeners))
	}
	second()
	if len(state.sessionSnapshotListeners) != 0 {
		t.Errorf("bucket count = %d, want 0 once the last listener left", len(state.sessionSnapshotListeners))
	}
}

// TestListenerPanicIsContainedAndReported: a subscriber must not be able to
// corrupt client state or stop other subscribers. pi wraps each callback in
// try/catch; Go's equivalent is recover, which is justified here for exactly
// that reason.
func TestListenerPanicIsContainedAndReported(t *testing.T) {
	var reported []string
	state := NewState(func(err error) { reported = append(reported, err.Error()) })

	state.Subscribe(func(*protocol.ServerSnapshot) { panic("subscriber blew up") })
	secondCalled := false
	state.Subscribe(func(*protocol.ServerSnapshot) { secondCalled = true })

	state.ApplyServerSnapshot(serverSnapshot(1, metadata("s1")))

	if !secondCalled {
		t.Error("a panicking listener stopped later listeners")
	}
	if len(reported) != 1 {
		t.Errorf("onListenerError called %d times, want 1", len(reported))
	}
	// Probes the stored snapshot rather than attachment: since 6189e53b3 a
	// server snapshot no longer conveys attachment, so it is no longer a
	// witness that the state survived the panic.
	if got := state.Snapshot(); got == nil || got.Revision != 1 || len(got.Sessions) != 1 {
		t.Errorf("a panicking listener corrupted client state: %#v", got)
	}
}

// TestListenerErrorHandlerPanicIsSwallowed: diagnostics cannot affect client
// state, so a broken error handler must not escape either.
func TestListenerErrorHandlerPanicIsSwallowed(t *testing.T) {
	state := NewState(func(error) { panic("handler blew up too") })
	state.Subscribe(func(*protocol.ServerSnapshot) { panic("subscriber blew up") })
	state.ApplyServerSnapshot(serverSnapshot(1))
	if state.Snapshot() == nil {
		t.Error("state was not updated")
	}
}

func TestForgetAndRestoreSessionSnapshot(t *testing.T) {
	state := NewState(nil)
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 4, true)})

	previous := state.ForgetSessionSnapshot("s1")
	if previous == nil || previous.Revision != 4 {
		t.Errorf("ForgetSessionSnapshot = %#v, want revision 4", previous)
	}
	if state.SessionSnapshot("s1") != nil {
		t.Error("snapshot was not forgotten")
	}

	state.RestoreSessionSnapshot(previous)
	if got := state.SessionSnapshot("s1"); got == nil || got.Revision != 4 {
		t.Errorf("restore failed: %#v", got)
	}
}

// TestRestoreSessionSnapshotDoesNotOverwriteFresher: restore exists to undo a
// failed detach, and a newer snapshot may have arrived in the meantime.
func TestRestoreSessionSnapshotDoesNotOverwriteFresher(t *testing.T) {
	state := NewState(nil)
	stale := sessionSnapshot("s1", 1, true)
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 7, true)})

	state.RestoreSessionSnapshot(stale)

	if got := state.SessionSnapshot("s1"); got == nil || got.Revision != 7 {
		t.Errorf("SessionSnapshot = %#v, want the fresher revision 7", got)
	}
}

func TestResetKeepsListenersButDropsState(t *testing.T) {
	state := NewState(nil)
	calls := 0
	state.Subscribe(func(*protocol.ServerSnapshot) { calls++ })
	state.ApplyServerSnapshot(serverSnapshot(1, metadata("s1")))
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 1, true)})

	state.Reset()

	if state.Snapshot() != nil {
		t.Error("server snapshot survived Reset")
	}
	if state.SessionSnapshot("s1") != nil {
		t.Error("session snapshot survived Reset")
	}
	if state.IsSessionAttached("s1") {
		t.Error("attachment survived Reset")
	}
	state.ApplyServerSnapshot(serverSnapshot(1))
	if calls != 2 {
		t.Errorf("listener called %d times, want 2 — Reset must keep subscribers", calls)
	}
}

// TestClearAttachmentsKeepsSnapshots: on reconnect the attachments are gone but
// the cached transcripts are still worth showing.
func TestClearAttachmentsKeepsSnapshots(t *testing.T) {
	state := NewState(nil)
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 1, true)})

	state.ClearAttachments()

	if state.IsSessionAttached("s1") {
		t.Error("attachment survived ClearAttachments")
	}
	if state.SessionSnapshot("s1") == nil {
		t.Error("ClearAttachments dropped the cached snapshot")
	}
}

func TestDisposeDropsListeners(t *testing.T) {
	state := NewState(nil)
	calls := 0
	state.Subscribe(func(*protocol.ServerSnapshot) { calls++ })
	state.SubscribeSession("s1", func(*protocol.SessionSnapshot) { calls++ })
	state.OnEvent(func(protocol.ServerEvent) { calls++ })
	state.OnSessionEvent("s1", func(protocol.ServerEvent) { calls++ })

	state.Dispose()

	state.ApplyServerSnapshot(serverSnapshot(1))
	state.ApplyEvent(&protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 1, true)})

	if calls != 0 {
		t.Errorf("listener called %d times after Dispose, want 0", calls)
	}
}

// TestDisposeIsALatch: the snapshot a disposed State drops must stay dropped.
// Disposal cannot be ordered against the connection that feeds it — the
// handshake callback runs with no lock held, so the check and the apply can
// never be one step — and a snapshot landing a moment late would otherwise
// resurrect the cache of a client its owner has already thrown away.
func TestDisposeIsALatch(t *testing.T) {
	state := NewState(nil)
	state.Dispose()

	state.ApplyServerSnapshot(serverSnapshot(1, metadata("s1")))
	if got := state.Snapshot(); got != nil {
		t.Errorf("Snapshot() = %#v after Dispose, want nil", got)
	}
	if state.IsSessionAttached("s1") {
		t.Error("a snapshot applied after Dispose re-attached a session")
	}

	state.ApplyEvent(&protocol.SessionSnapshotEvent{
		Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", 1, true),
	})
	if got := state.SessionSnapshot("s1"); got != nil {
		t.Errorf("SessionSnapshot(s1) = %#v after Dispose, want nil", got)
	}
	if state.IsSessionAttached("s1") {
		t.Error("an event applied after Dispose re-attached a session")
	}

	state.ApplyResult(&protocol.SessionResult{Command: "attach", Session: *sessionSnapshot("s2", 1, true)})
	if got := state.SessionSnapshot("s2"); got != nil {
		t.Errorf("SessionSnapshot(s2) = %#v after Dispose, want nil", got)
	}
}

// TestDisposeKeepsListenerIdsUnique: Dispose drops the listeners but not the
// counter that names them. Restarting it would let an unsubscribe held from
// before disposal cancel a listener registered after it.
func TestDisposeKeepsListenerIdsUnique(t *testing.T) {
	state := NewState(nil)
	stale := state.Subscribe(func(*protocol.ServerSnapshot) {})

	state.Dispose()

	calls := 0
	state.Subscribe(func(*protocol.ServerSnapshot) { calls++ })
	stale()

	if state.snapshotListeners.len() != 1 {
		t.Fatalf("listener count = %d, want the post-Dispose subscription to survive",
			state.snapshotListeners.len())
	}
	_ = calls
}

// TestConcurrentReadsAndApplies is the reason State has a mutex at all: pi's
// ClientState is single-threaded, but here the reader goroutine mutates while
// callers read. Meaningful only under -race.
func TestConcurrentReadsAndApplies(t *testing.T) {
	state := NewState(nil)
	state.Subscribe(func(*protocol.ServerSnapshot) {})
	state.SubscribeSession("s1", func(*protocol.SessionSnapshot) {})

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 200 {
			state.ApplyServerSnapshot(serverSnapshot(int64(i), metadata("s1")))
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 200 {
			state.ApplyEvent(&protocol.SessionSnapshotEvent{
				Type: "session_snapshot", Snapshot: *sessionSnapshot("s1", int64(i), true),
			})
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			_ = state.Snapshot()
			_ = state.SessionSnapshot("s1")
			_ = state.IsSessionAttached("s1")
		}
	}()
	wg.Wait()
}

// TestListenersFireInRegistrationOrder: pi stores listeners in a Set, which
// iterates in insertion order, so a subscriber registered first observes each
// change first. Go map iteration is randomised, so preserving this takes
// deliberate effort — and without it the order silently varies run to run.
func TestListenersFireInRegistrationOrder(t *testing.T) {
	state := NewState(nil)
	var order []int
	for i := range 8 {
		state.Subscribe(func(*protocol.ServerSnapshot) { order = append(order, i) })
	}
	state.ApplyServerSnapshot(serverSnapshot(1))

	for i := range 8 {
		if i >= len(order) || order[i] != i {
			t.Fatalf("listeners fired in order %v, want ascending registration order", order)
		}
	}
}

// TestUnsubscribeFromInsideListener: notifications run outside the lock
// precisely so a subscriber can call back into State. If that ever regresses to
// notifying under the lock, this deadlocks rather than failing.
func TestUnsubscribeFromInsideListener(t *testing.T) {
	state := NewState(nil)
	calls := 0
	var unsubscribe Unsubscribe
	unsubscribe = state.Subscribe(func(*protocol.ServerSnapshot) {
		calls++
		unsubscribe()
	})

	state.ApplyServerSnapshot(serverSnapshot(1))
	state.ApplyServerSnapshot(serverSnapshot(2))

	if calls != 1 {
		t.Errorf("listener called %d times, want 1 — it unsubscribed itself on the first call", calls)
	}
}

// TestSubscribeFromInsideListener is the other half: registering during a
// notification must not deadlock, and the new subscriber is reached by the
// notification it registered during. pi iterates a live Set, and a JS Set
// iterator visits entries added while it runs — so the listener list cannot be
// snapshotted before delivery.
func TestSubscribeFromInsideListener(t *testing.T) {
	state := NewState(nil)
	late := 0
	registered := false
	state.Subscribe(func(*protocol.ServerSnapshot) {
		if registered {
			return
		}
		registered = true
		state.Subscribe(func(*protocol.ServerSnapshot) { late++ })
	})

	state.ApplyServerSnapshot(serverSnapshot(1))
	if late != 1 {
		t.Errorf("late subscriber called %d times during its own registration, want 1", late)
	}
	state.ApplyServerSnapshot(serverSnapshot(2))
	if late != 2 {
		t.Errorf("late subscriber called %d times in total, want 2", late)
	}
}

// TestUnsubscribeAPeerFromInsideListener is the mirror image: a listener
// cancelled by an earlier listener during the same notification is not called.
// Snapshotting the list first would deliver to a subscriber that has already
// been told to stop — and the subscriber most likely to be cancelled mid-
// notification is one whose owner has just been torn down.
func TestUnsubscribeAPeerFromInsideListener(t *testing.T) {
	state := NewState(nil)
	var order []string
	var stopSecond Unsubscribe
	state.Subscribe(func(*protocol.ServerSnapshot) {
		order = append(order, "a")
		stopSecond()
	})
	stopSecond = state.Subscribe(func(*protocol.ServerSnapshot) { order = append(order, "b") })
	state.Subscribe(func(*protocol.ServerSnapshot) { order = append(order, "c") })

	state.ApplyServerSnapshot(serverSnapshot(1))

	if len(order) != 2 || order[0] != "a" || order[1] != "c" {
		t.Errorf("listeners fired %v, want [a c] — b was cancelled before its turn", order)
	}
}

// TestSessionListenersAreIteratedLive: the per-session lists are reached
// through a map, so they need the same treatment as the top-level ones.
func TestSessionListenersAreIteratedLive(t *testing.T) {
	state := NewState(nil)
	var order []string
	var stopSecond Unsubscribe
	state.OnSessionEvent("s1", func(protocol.ServerEvent) {
		order = append(order, "a")
		stopSecond()
		state.OnSessionEvent("s1", func(protocol.ServerEvent) { order = append(order, "late") })
	})
	stopSecond = state.OnSessionEvent("s1", func(protocol.ServerEvent) { order = append(order, "b") })
	state.OnSessionEvent("s1", func(protocol.ServerEvent) { order = append(order, "c") })

	state.ApplyEvent(&protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "s1"})

	want := []string{"a", "c", "late"}
	if len(order) != len(want) {
		t.Fatalf("listeners fired %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("listeners fired %v, want %v", order, want)
		}
	}
}

// TestUnsubscribeAfterDisposeKeepsALiveBucket: a per-session unsubscribe held
// from before Dispose names a listener set that is no longer in the map. It
// must not take the bucket that replaced it down with it.
func TestUnsubscribeAfterDisposeKeepsALiveBucket(t *testing.T) {
	state := NewState(nil)
	stale := state.OnSessionEvent("s1", func(protocol.ServerEvent) {})

	state.Dispose()
	calls := 0
	state.OnSessionEvent("s1", func(protocol.ServerEvent) { calls++ })
	stale()

	if len(state.sessionEventListeners) != 1 {
		t.Fatalf("bucket count = %d, want the live bucket to survive a stale unsubscribe",
			len(state.sessionEventListeners))
	}
	_ = calls
}
