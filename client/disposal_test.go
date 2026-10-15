package client

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// TestConnectHelperBuildsAConnectedClient locks the one-call entry point.
func TestConnectHelperBuildsAConnectedClient(t *testing.T) {
	server := newMemoryServer(t)
	server.answerHandshake(t, baseSnapshot(1))

	client, err := Connect(testContext(t), Options{TransportFactory: server.connect})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if !client.Connected() {
		t.Error("Connect returned a client that is not connected")
	}
	if snapshot := client.Snapshot(); snapshot == nil || snapshot.Revision != 1 {
		t.Errorf("Snapshot() = %#v, want the handshake snapshot", snapshot)
	}
}

// TestConnectHelperDisposesOnHandshakeFailure: a caller that never received a
// client cannot be expected to clean one up.
func TestConnectHelperDisposesOnHandshakeFailure(t *testing.T) {
	server := newMemoryServer(t)
	server.onMessage(func(protocol.ClientMessage) {
		server.send(t, &protocol.ServerHelloError{
			Type: "hello_error",
			Error: protocol.ProtocolError{
				Code: protocol.ErrorVersion, Message: "Unsupported protocol version",
			},
		})
	})

	client, err := Connect(testContext(t), Options{TransportFactory: server.connect})
	if client != nil {
		t.Error("Connect returned a client alongside its error")
	}
	var serverErr *ServerError
	if !errors.As(err, &serverErr) {
		t.Fatalf("error = %#v, want *ServerError", err)
	}
}

// TestCloseDisconnectsInvalidatesAndRejects covers everything disposal owes its
// caller in one place: it is idempotent, it drops the connection, it retires
// child handles, and nothing is left waiting.
func TestCloseDisconnectsInvalidatesAndRejects(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	handle := attachTestSession(t, client, server, sessionSnapshot("session-1", 1, true))
	requests := recordRequests(server)

	listErr := make(chan error, 1)
	go func() {
		_, err := client.ListSessions(testContext(t))
		listErr <- err
	}()
	requests.await(t, 1)

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if !client.Disposed() {
		t.Error("Disposed() is false after Close")
	}
	if client.Connected() {
		t.Error("Connected() is true after Close")
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
	if handle.Attached() {
		t.Error("a child handle survived disposal")
	}

	var disposed *DisposedError
	if err := <-listErr; !errors.As(err, &disposed) {
		t.Fatalf("pending request error = %#v, want *DisposedError", err)
	}
	if _, err := handle.Prompt(testContext(t), "after disposal"); !errors.As(err, &disposed) {
		t.Fatalf("Prompt after disposal = %#v, want *DisposedError", err)
	}
}

// TestSubscriptionsAreRefusedAfterClose: a disposed client has dropped its
// listeners, so accepting a new one would register something that never fires.
func TestSubscriptionsAreRefusedAfterClose(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var disposed *DisposedError
	if _, err := client.Subscribe(func(*protocol.ServerSnapshot) {}); !errors.As(err, &disposed) {
		t.Errorf("Subscribe = %#v, want *DisposedError", err)
	}
	if _, err := client.OnEvent(func(protocol.ServerEvent) {}); !errors.As(err, &disposed) {
		t.Errorf("OnEvent = %#v, want *DisposedError", err)
	}
	if _, err := client.OnConnectionStateChange(func(ConnectionStateChange) {}); !errors.As(err, &disposed) {
		t.Errorf("OnConnectionStateChange = %#v, want *DisposedError", err)
	}
	if _, err := client.Connect(testContext(t)); !errors.As(err, &disposed) {
		t.Errorf("Connect = %#v, want *DisposedError", err)
	}
	if _, err := client.AcquireSession(testContext(t), "session-1", LeaseShared); !errors.As(err, &disposed) {
		t.Errorf("AcquireSession = %#v, want *DisposedError", err)
	}
}

// TestCloseNotifiesConnectionStateListeners: subscribers are cleared last, so
// the disconnect disposal caused is still delivered.
func TestCloseNotifiesConnectionStateListeners(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	var changes []ConnectionStateChange
	if _, err := client.OnConnectionStateChange(func(change ConnectionStateChange) {
		changes = append(changes, change)
	}); err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(changes) != 1 || changes[0].State != Disconnected {
		t.Fatalf("changes = %#v, want a single disconnect", changes)
	}
	var disposed *DisposedError
	if !errors.As(changes[0].Err, &disposed) {
		t.Errorf("disconnect cause = %#v, want *DisposedError", changes[0].Err)
	}
}

// TestCloseFromAConnectionStateListener: "tear the client down when the
// connection dies" is a listener a reader would think is allowed, and it calls
// Close from inside the very notification Close produced. Every other
// re-entrant path in the package survives this — Connection.fail retires the
// attempt before it notifies — so disposal must too.
func TestCloseFromAConnectionStateListener(t *testing.T) {
	server := newMemoryServer(t)
	// Deliberately not registered for cleanup: a regression here deadlocks, and
	// a deadlocked cleanup would hang the whole package rather than fail one
	// test.
	client, err := New(Options{TransportFactory: server.connect})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server.answerHandshake(t, baseSnapshot(1))
	if _, err := client.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	closes := 0
	if _, err := client.OnConnectionStateChange(func(change ConnectionStateChange) {
		if change.State == Disconnected {
			closes++
			_ = client.Close()
		}
	}); err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.Close()
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Close deadlocked against a listener that closed the client")
	}
	if closes != 1 {
		t.Errorf("the listener ran %d times, want 1", closes)
	}
	if !client.Disposed() {
		t.Error("Disposed() is false after Close")
	}
}

// TestConcurrentCloseWaitsForTeardown: a second caller is told the client is
// closed only once it actually is, so a caller that closes and then inspects
// does not see a half-disposed client. This is pi's guarantee, not a Go
// embellishment — pi's dispose() memoises an already-resolved promise and tears
// down synchronously, so run-to-completion gives every caller, re-entrant ones
// included, a view of a fully torn-down client.
//
// Every invariant is checked on every caller, and none of them short-circuits.
// That matters: the bug this test was written against
// (a client-global "am I a callback?" flag that any goroutine could trip) broke
// all three at once, and a `t.Fatal`-style early exit made it look like only the
// first one was reachable, which is what kept it filed as an over-strict test
// rather than a race for four cycles.
//
// Snapshot() is the load-bearing third check. Attached() and ConnectionState()
// are settled before connection-state listeners run, so a fix that merely
// narrows the window satisfies those two while still letting a caller observe a
// client whose State has not been disposed. Only Snapshot() distinguishes
// "fully disposed" from "far enough along to look disposed".
func TestConcurrentCloseWaitsForTeardown(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	handle := attachTestSession(t, client, server, sessionSnapshot("session-1", 1, true))

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := client.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			if handle.Attached() {
				t.Error("Close returned while a child handle was still attached")
			}
			if client.ConnectionState() != Disconnected {
				t.Error("Close returned while the connection was still up")
			}
			if client.Snapshot() != nil {
				t.Error("Close returned while the client still held server state")
			}
			if !client.Disposed() {
				t.Error("Close returned while the client did not report itself disposed")
			}
		}()
	}
	close(start)
	wg.Wait()
}

// TestCloseFromAListenerDoesNotStrandAnotherCloser pins the interaction the two
// disposal contracts have to satisfy at once, which is what the deleted
// re-entrancy flag could not do: the listener's own nested Close must return
// (it cannot wait for a teardown it is part of), while an unrelated goroutine
// closing at the same moment must still be held until disposal is complete.
//
// The unrelated closer is started only once the listener is running, so it
// cannot be the goroutine performing teardown — that is what makes it
// unrelated rather than just a second racer. The listener then blocks, so if
// disposal published completion only AFTER notifying, the unrelated closer is
// never released and this test times out rather than failing an assertion.
// That is the honest failure mode for a lost guarantee: the old code deadlocks
// here, it does not merely observe a half-disposed client.
func TestCloseFromAListenerDoesNotStrandAnotherCloser(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	handle := attachTestSession(t, client, server, sessionSnapshot("session-1", 1, true))

	inListener := make(chan struct{})
	released := make(chan struct{})
	nestedReturned := make(chan struct{})
	if _, err := client.OnConnectionStateChange(func(change ConnectionStateChange) {
		if change.State != Disconnected {
			return
		}
		close(inListener)
		// Nested Close: must not wait for the teardown running this callback.
		_ = client.Close()
		close(nestedReturned)
		<-released
	}); err != nil {
		t.Fatal(err)
	}

	teardown := make(chan struct{})
	go func() {
		defer close(teardown)
		_ = client.Close()
	}()

	select {
	case <-inListener:
	case <-time.After(testTimeout):
		t.Fatal("the disconnect listener never ran")
	}
	select {
	case <-nestedReturned:
	case <-time.After(testTimeout):
		t.Fatal("a Close issued from the listener waited for the teardown it is part of")
	}

	unrelated := make(chan struct{})
	go func() {
		defer close(unrelated)
		_ = client.Close()
		if handle.Attached() || client.Snapshot() != nil || !client.Disposed() {
			t.Error("an unrelated closer was released before teardown finished")
		}
	}()
	select {
	case <-unrelated:
	case <-time.After(testTimeout):
		t.Error("an unrelated closer was never released while a listener held teardown open")
	}

	close(released)
	select {
	case <-teardown:
	case <-time.After(testTimeout):
		t.Fatal("disposal never finished")
	}
}

// TestConnectionStateListenersAreIteratedLive: pi's
// #notifyConnectionStateListeners walks a live Set, so a listener registered
// during a notification is reached by it and one cancelled during it is not.
// The pattern this protects is a supervisor whose disconnect handler swaps its
// own subscriptions out.
func TestConnectionStateListenersAreIteratedLive(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	var order []string
	var stopSecond Unsubscribe
	if _, err := client.OnConnectionStateChange(func(ConnectionStateChange) {
		order = append(order, "a")
		stopSecond()
		if _, err := client.OnConnectionStateChange(func(ConnectionStateChange) {
			order = append(order, "late")
		}); err != nil {
			t.Errorf("OnConnectionStateChange from inside a listener: %v", err)
		}
	}); err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}
	var err error
	if stopSecond, err = client.OnConnectionStateChange(func(ConnectionStateChange) {
		order = append(order, "b")
	}); err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}
	if _, err := client.OnConnectionStateChange(func(ConnectionStateChange) {
		order = append(order, "c")
	}); err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}

	client.Disconnect("")

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

// TestConnectionStateUnsubscribeIsIdempotent: callers hold these and run them
// from a defer as well as explicitly.
func TestConnectionStateUnsubscribeIsIdempotent(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	calls := 0
	unsubscribe, err := client.OnConnectionStateChange(func(ConnectionStateChange) { calls++ })
	if err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}
	unsubscribe()
	unsubscribe()

	client.Disconnect("")
	if calls != 0 {
		t.Errorf("listener ran %d times after unsubscribing", calls)
	}
}
