package client

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// autoRespond answers attach and detach requests immediately, which is what
// lets the lease tests read as straight-line code.
func autoRespond(t *testing.T, server *memoryServer, snapshotFor func(sessionID string) *protocol.SessionSnapshot) {
	t.Helper()
	server.onMessage(func(message protocol.ClientMessage) {
		request, ok := message.(*protocol.RequestEnvelope)
		if !ok {
			return
		}
		switch command := request.Request.(type) {
		case *protocol.AttachCommand:
			server.send(t, okResponse(request.ID, attachResult(snapshotFor(command.SessionID))))
		case *protocol.DetachCommand:
			server.send(t, okResponse(request.ID, detachResult(command.SessionID)))
		}
	})
}

func attachedSnapshot(sessionID string) *protocol.SessionSnapshot {
	return sessionSnapshot(sessionID, 1, true)
}

// TestSessionHandlesAreIndependent locks that one lease ending says nothing
// about another session's lease, and that a spent handle refuses to act.
func TestSessionHandlesAreIndependent(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	autoRespond(t, server, attachedSnapshot)
	ctx := testContext(t)

	first, err := client.AttachSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("AttachSession(session-1): %v", err)
	}
	second, err := client.AttachSession(ctx, "session-2")
	if err != nil {
		t.Fatalf("AttachSession(session-2): %v", err)
	}
	if !first.Attached() || !second.Attached() {
		t.Fatal("both handles should start attached")
	}

	if err := first.Detach(ctx); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if first.Attached() {
		t.Error("detached handle still reports itself attached")
	}
	if !second.Attached() {
		t.Error("detaching session-1 detached session-2")
	}

	_, err = first.Abort(ctx)
	var detached *SessionDetachedError
	if !errors.As(err, &detached) {
		t.Fatalf("Abort after detach = %#v, want *SessionDetachedError", err)
	}
	if detached.SessionID != "session-1" {
		t.Errorf("SessionID = %q, want %q", detached.SessionID, "session-1")
	}
}

// TestSharedSessionDetachesOnlyOnFinalLease locks the reference counting: the
// server is told once, when the last holder lets go.
func TestSharedSessionDetachesOnlyOnFinalLease(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	autoRespond(t, server, attachedSnapshot)
	ctx := testContext(t)

	first, err := client.AttachSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("first AttachSession: %v", err)
	}
	second, err := client.AttachSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("second AttachSession: %v", err)
	}
	if first == second {
		t.Error("the second acquisition returned the same handle")
	}
	assertCommands(t, requests, "attach")

	if err := first.Detach(ctx); err != nil {
		t.Fatalf("first Detach: %v", err)
	}
	if first.Attached() {
		t.Error("released handle still reports itself attached")
	}
	if !second.Attached() {
		t.Error("the surviving lease was detached with the first")
	}
	assertCommands(t, requests, "attach")

	if err := second.Detach(ctx); err != nil {
		t.Fatalf("second Detach: %v", err)
	}
	if second.Attached() {
		t.Error("final handle still reports itself attached")
	}
	assertCommands(t, requests, "attach", "detach")
}

// TestLeaseModesAreEnforced locks the ownership rules an exclusive lease exists
// to provide.
func TestLeaseModesAreEnforced(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	autoRespond(t, server, attachedSnapshot)
	ctx := testContext(t)

	shared, err := client.AcquireSession(ctx, "session-1", LeaseShared)
	if err != nil {
		t.Fatalf("AcquireSession(shared): %v", err)
	}
	_, err = client.AcquireSession(ctx, "session-1", LeaseExclusive)
	assertOwnershipError(t, err, "session-1", "already has an active lease")
	if err := shared.Close(ctx); err != nil {
		t.Fatalf("Close(shared): %v", err)
	}

	exclusive, err := client.AcquireSession(ctx, "session-1", LeaseExclusive)
	if err != nil {
		t.Fatalf("AcquireSession(exclusive): %v", err)
	}
	_, err = client.AcquireSession(ctx, "session-1", LeaseShared)
	assertOwnershipError(t, err, "session-1", "has an exclusive lease")
	if err := exclusive.Close(ctx); err != nil {
		t.Fatalf("Close(exclusive): %v", err)
	}
}

// TestInvalidatedLeaseClosesWithoutProtocolCleanup: once the connection is
// gone, there is nobody to detach from, so closing the lease must not try.
func TestInvalidatedLeaseClosesWithoutProtocolCleanup(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	autoRespond(t, server, attachedSnapshot)
	ctx := testContext(t)

	lease, err := client.AcquireSession(ctx, "session-1", LeaseExclusive)
	if err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}

	client.Disconnect("")

	if err := lease.Close(ctx); err != nil {
		t.Fatalf("Close on an invalidated lease = %v, want nil", err)
	}
	if lease.Active() {
		t.Error("an invalidated lease reports itself active")
	}
	assertCommands(t, requests, "attach")
}

// TestSessionRemovalInvalidatesLeases: the session going away retires its
// leases, and the removal event still reaches the lease's own subscriber —
// that event is the explanation for everything going quiet.
func TestSessionRemovalInvalidatesLeases(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	autoRespond(t, server, attachedSnapshot)
	ctx := testContext(t)

	lease, err := client.AttachSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	var events []protocol.ServerEvent
	if _, err := lease.OnEvent(func(event protocol.ServerEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}

	server.send(t, &protocol.EventEnvelope{
		Type:  "event",
		Event: &protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "session-1"},
	})

	if lease.Active() {
		t.Error("a removed session's lease reports itself active")
	}
	if len(events) != 1 {
		t.Fatalf("subscriber saw %d events, want the removal", len(events))
	}
	if _, ok := events[0].(*protocol.SessionRemovedEvent); !ok {
		t.Errorf("event = %T, want *protocol.SessionRemovedEvent", events[0])
	}
	if err := lease.Close(ctx); err != nil {
		t.Errorf("Close on a removed session's lease = %v, want nil", err)
	}
}

// TestFailedDetachKeepsLeaseUsable locks the difference between Detach and
// Close: an explicit detach that fails must leave the caller able to retry,
// and must refuse commands while it is in flight.
func TestFailedDetachKeepsLeaseUsable(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	ctx := testContext(t)

	acquired := make(chan *SessionHandle, 1)
	acquireErr := make(chan error, 1)
	go func() {
		handle, err := client.AcquireSession(ctx, "session-1", LeaseExclusive)
		if err != nil {
			acquireErr <- err
			return
		}
		acquired <- handle
	}()
	requests.await(t, 1)
	server.send(t, okResponse(requests.last(t).ID, attachResult(attachedSnapshot("session-1"))))
	var lease *SessionHandle
	select {
	case lease = <-acquired:
	case err := <-acquireErr:
		t.Fatalf("AcquireSession: %v", err)
	}

	firstDetach := make(chan error, 1)
	go func() { firstDetach <- lease.Detach(ctx) }()
	requests.await(t, 2)
	failedDetach := requests.last(t)

	// The lease is releasing, so it must refuse work rather than send a
	// command for a session it is in the middle of giving up.
	_, err := lease.Abort(ctx)
	var detached *SessionDetachedError
	if !errors.As(err, &detached) {
		t.Fatalf("Abort while releasing = %#v, want *SessionDetachedError", err)
	}

	server.send(t, errorResponse(failedDetach.ID, protocol.ErrorInvalidRequest, "retry"))
	if err := <-firstDetach; err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("Detach error = %#v, want the server's \"retry\"", err)
	}
	if !lease.Active() {
		t.Error("a lease whose detach failed should stay usable")
	}

	secondDetach := make(chan error, 1)
	go func() { secondDetach <- lease.Detach(ctx) }()
	requests.await(t, 3)
	server.send(t, okResponse(requests.last(t).ID, detachResult("session-1")))
	if err := <-secondDetach; err != nil {
		t.Fatalf("retried Detach: %v", err)
	}
	if lease.Active() {
		t.Error("a released lease reports itself active")
	}
}

// TestClosedLeaseAfterFailedDetachReconciles: Close gives the lease up even
// when the detach fails, so the owed detach is paid off before the next
// acquisition attaches again.
func TestClosedLeaseAfterFailedDetachReconciles(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	ctx := testContext(t)

	lease := attachTestSession(t, client, server, attachedSnapshot("session-1"))

	closed := make(chan error, 1)
	go func() { closed <- lease.Close(ctx) }()
	requests.await(t, 2)
	server.send(t, errorResponse(requests.last(t).ID, protocol.ErrorInvalidRequest, "nope"))
	if err := <-closed; err == nil {
		t.Fatal("Close swallowed the failed detach")
	}
	if lease.Active() {
		t.Error("Close left the lease active after a failed detach")
	}

	reacquired := make(chan error, 1)
	go func() {
		_, err := client.AttachSession(ctx, "session-1")
		reacquired <- err
	}()
	// The owed detach is paid off first, then the attach is reissued.
	requests.await(t, 3)
	server.send(t, okResponse(requests.last(t).ID, detachResult("session-1")))
	requests.await(t, 4)
	server.send(t, okResponse(requests.last(t).ID, attachResult(attachedSnapshot("session-1"))))
	if err := <-reacquired; err != nil {
		t.Fatalf("reacquisition: %v", err)
	}
	assertCommands(t, requests, "attach", "detach", "detach", "attach")
}

// TestAcquisitionParkedOnAFailedDetachReconciles is the concurrent half of
// TestClosedLeaseAfterFailedDetachReconciles: the acquisition is already
// waiting on the detachment when it fails, rather than arriving after the
// failure was recorded. Whether the session still owes the server a detach can
// only be known once that wait is over — so the acquisition must read it after
// the wait, and the detachment must not report itself over until the failure
// has been booked. Get either wrong and the lease is minted on a stale
// "attached" bit, with the owed detach left to fire later against a session a
// live lease is holding.
//
// The test pins both. synctest.Wait names the moment the acquisition has parked
// with no sleep standing in for it, and holding the lease's own mutex freezes
// the failed Close between its detach and its bookkeeping — which is precisely
// the window a detachment settled too early lets an acquirer through.
func TestAcquisitionParkedOnAFailedDetachReconciles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := newMemoryServer(t)
		client := connectTestClient(t, server)
		requests := recordRequests(server)
		ctx := testContext(t)

		handle := attachTestSession(t, client, server, attachedSnapshot("session-1"))

		closed := make(chan error, 1)
		go func() { closed <- handle.Close(ctx) }()
		requests.await(t, 2)
		failedDetach := requests.last(t)

		reacquiring := make(chan error, 1)
		go func() {
			_, err := client.AttachSession(ctx, "session-1")
			reacquiring <- err
		}()
		synctest.Wait()
		// Nothing was sent: the acquisition is parked on the in-flight detach
		// rather than racing it.
		assertCommands(t, requests, "attach", "detach")

		server.send(t, errorResponse(failedDetach.ID, protocol.ErrorInvalidRequest, "nope"))
		if err := <-closed; err == nil {
			t.Fatal("Close swallowed the failed detach")
		}

		// The parked acquisition inherits the detach the failed Close still owes,
		// pays it off, and only then reattaches.
		requests.await(t, 3)
		server.send(t, okResponse(requests.last(t).ID, detachResult("session-1")))
		requests.await(t, 4)
		server.send(t, okResponse(requests.last(t).ID, attachResult(attachedSnapshot("session-1"))))
		if err := <-reacquiring; err != nil {
			t.Fatalf("reacquisition: %v", err)
		}
		assertCommands(t, requests, "attach", "detach", "detach", "attach")
	})
}

// TestReacquisitionSerializesBehindDetach: reattaching while a detach is still
// in flight must wait for it, or the server could apply the two out of order
// and leave the session attached to nobody.
func TestReacquisitionSerializesBehindDetach(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	ctx := testContext(t)

	first := attachTestSession(t, client, server, attachedSnapshot("session-1"))

	detaching := make(chan error, 1)
	go func() { detaching <- first.Detach(ctx) }()
	requests.await(t, 2)
	detachRequest := requests.last(t)

	reacquiring := make(chan error, 1)
	go func() {
		_, err := client.AttachSession(ctx, "session-1")
		reacquiring <- err
	}()
	// Give the reacquisition room to send an attach if it were going to; it
	// must not, because the detach has not been answered yet.
	time.Sleep(50 * time.Millisecond)
	assertCommands(t, requests, "attach", "detach")

	server.send(t, okResponse(detachRequest.ID, detachResult("session-1")))
	if err := <-detaching; err != nil {
		t.Fatalf("Detach: %v", err)
	}
	requests.await(t, 3)
	if got := requests.last(t).Request.CommandName(); got != "attach" {
		t.Fatalf("third request was %q, want attach", got)
	}
	server.send(t, okResponse(requests.last(t).ID, attachResult(sessionSnapshot("session-1", 2, true))))
	if err := <-reacquiring; err != nil {
		t.Fatalf("reacquisition: %v", err)
	}
}

// TestReattachAcceptsLowerRevision: a fresh attachment restarts the session's
// revision counter, so the cached snapshot has to be forgotten first or the
// staleness guard would discard the server's answer.
func TestReattachAcceptsLowerRevision(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	attachCount := 0
	autoRespond(t, server, func(sessionID string) *protocol.SessionSnapshot {
		attachCount++
		if attachCount == 1 {
			return sessionSnapshot(sessionID, 10, true)
		}
		return sessionSnapshot(sessionID, 0, true)
	})
	ctx := testContext(t)

	first, err := client.AttachSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	if snapshot := first.Snapshot(); snapshot == nil || snapshot.Revision != 10 {
		t.Fatalf("first snapshot = %#v, want revision 10", snapshot)
	}
	if err := first.Detach(ctx); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	reopened, err := client.AttachSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if reopened == first {
		t.Error("reattaching returned the spent handle")
	}
	if snapshot := reopened.Snapshot(); snapshot == nil || snapshot.Revision != 0 {
		t.Fatalf("reopened snapshot = %#v, want revision 0", snapshot)
	}
}

// TestCreateSessionTakesExclusiveLease: whoever created a session owns it, so
// a second claim on it has to be refused without being asked.
func TestCreateSessionTakesExclusiveLease(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	ctx := testContext(t)

	created := make(chan *SessionHandle, 1)
	createErr := make(chan error, 1)
	go func() {
		cwd := "/workspace"
		handle, err := client.CreateSession(ctx, CreateSessionOptions{Cwd: &cwd})
		if err != nil {
			createErr <- err
			return
		}
		created <- handle
	}()
	requests.await(t, 1)
	create, ok := requests.last(t).Request.(*protocol.CreateCommand)
	if !ok {
		t.Fatalf("request was %T, want *protocol.CreateCommand", requests.last(t).Request)
	}
	if create.Cwd == nil || *create.Cwd != "/workspace" {
		t.Errorf("create cwd = %v, want /workspace", create.Cwd)
	}
	if create.Name != nil || create.Model != nil || create.ThinkingLevel != nil {
		t.Error("unset create options must not be sent")
	}
	server.send(t, okResponse(requests.last(t).ID, &protocol.SessionResult{
		Command: "create",
		Session: *attachedSnapshot("session-1"),
	}))

	select {
	case err := <-createErr:
		t.Fatalf("CreateSession: %v", err)
	case handle := <-created:
		if handle.ID() != "session-1" {
			t.Errorf("handle ID = %q, want session-1", handle.ID())
		}
		_, err := client.AcquireSession(ctx, "session-1", LeaseShared)
		assertOwnershipError(t, err, "session-1", "has an exclusive lease")
	}
}

func assertCommands(t *testing.T, requests *requestRecorder, want ...string) {
	t.Helper()
	got := requests.commands()
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("requests = %v, want %v", got, want)
		}
	}
}

func assertOwnershipError(t *testing.T, err error, sessionID, wantMessage string) {
	t.Helper()
	var ownership *SessionOwnershipError
	if !errors.As(err, &ownership) {
		t.Fatalf("error = %#v, want *SessionOwnershipError", err)
	}
	if ownership.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", ownership.SessionID, sessionID)
	}
	if !strings.Contains(ownership.Error(), wantMessage) {
		t.Errorf("message = %q, want it to contain %q", ownership.Error(), wantMessage)
	}
}
