package client

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol/cbor"
)

// TestConnectSendsHelloAndAcceptsFragmentedServerHello locks the handshake: the
// client's first frame is a versioned hello carrying no credential, and the
// server's answer is accepted even when it arrives split across chunks.
func TestConnectSendsHelloAndAcceptsFragmentedServerHello(t *testing.T) {
	server := newMemoryServer(t)
	var received []protocol.ClientMessage
	snapshot := baseSnapshot(1)
	server.onMessage(func(message protocol.ClientMessage) {
		received = append(received, message)
		if _, ok := message.(*protocol.ClientHello); ok {
			server.sendSplit(t, serverHello(snapshot), 3)
		}
	})
	client := newTestClient(t, server)

	got, err := client.Connect(testContext(t))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got.Revision != snapshot.Revision || got.ServerID != snapshot.ServerID {
		t.Errorf("Connect returned %#v, want the server's snapshot", got)
	}
	if len(received) != 1 {
		t.Fatalf("server saw %d messages, want 1", len(received))
	}
	hello, ok := received[0].(*protocol.ClientHello)
	if !ok {
		t.Fatalf("first client message was %T, want *protocol.ClientHello", received[0])
	}
	if hello.Version != protocol.ProtocolVersion {
		t.Errorf("hello = %#v, want version %d", hello, protocol.ProtocolVersion)
	}
	if client.ConnectionState() != Connected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Connected)
	}
}

// TestConnectRejectsServerDataBeforeClientHello covers a transport that hands
// back bytes before it has even returned: nothing may be sent, the transport is
// closed exactly once, and the handshake fails as a protocol violation.
func TestConnectRejectsServerDataBeforeClientHello(t *testing.T) {
	var mu sync.Mutex
	sendCount, closeCount := 0, 0
	client, err := New(Options{
		TransportFactory: func(ctx context.Context, handlers TransportHandlers) (Transport, error) {
			handlers.OnData(encodeServer(t, serverHello(baseSnapshot(1))))
			return &countingTransport{mu: &mu, sends: &sendCount, closes: &closeCount}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	_, connectErr := client.Connect(testContext(t))
	var validation *protocol.ValidationError
	if !errors.As(connectErr, &validation) {
		t.Fatalf("Connect error = %#v, want *protocol.ValidationError", connectErr)
	}
	if validation.Msg != "Received server data before the client hello was sent" {
		t.Errorf("message = %q, want pi's wording", validation.Msg)
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
	mu.Lock()
	defer mu.Unlock()
	if sendCount != 0 {
		t.Errorf("sendCount = %d, want 0", sendCount)
	}
	if closeCount != 1 {
		t.Errorf("closeCount = %d, want 1", closeCount)
	}
}

type countingTransport struct {
	mu     *sync.Mutex
	sends  *int
	closes *int
}

func (c *countingTransport) Send([]byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.sends++
	return nil
}

func (c *countingTransport) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.closes++
	return nil
}

// TestSubscriberPanicDoesNotBreakHandshake locks pi's isolation guarantee: a
// broken subscriber is contained and reported, and the connection survives it.
func TestSubscriberPanicDoesNotBreakHandshake(t *testing.T) {
	for _, tc := range []struct {
		name    string
		observe bool
	}{
		{"without a listener-error handler", false},
		{"with a listener-error handler", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newMemoryServer(t)
			var mu sync.Mutex
			var listenerErrors []error
			opts := Options{TransportFactory: server.connect}
			if tc.observe {
				opts.OnListenerError = func(err error) {
					mu.Lock()
					defer mu.Unlock()
					listenerErrors = append(listenerErrors, err)
				}
			}
			client, err := New(opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = client.Close() })
			server.answerHandshake(t, baseSnapshot(1))
			if _, err := client.Subscribe(func(*protocol.ServerSnapshot) {
				panic(errors.New("consumer failure"))
			}); err != nil {
				t.Fatalf("Subscribe: %v", err)
			}

			if _, err := client.Connect(testContext(t)); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if client.ConnectionState() != Connected {
				t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Connected)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.observe {
				if len(listenerErrors) != 1 || listenerErrors[0].Error() != "consumer failure" {
					t.Errorf("listener errors = %v, want one \"consumer failure\"", listenerErrors)
				}
			} else if len(listenerErrors) != 0 {
				t.Errorf("listener errors = %v, want none", listenerErrors)
			}
		})
	}
}

// TestHandshakeIsNotRestoredAfterListenerDisconnects: a snapshot listener that
// disconnects mid-handshake must win. Reporting "connected" afterwards would
// hand the caller a connection that no longer exists.
func TestHandshakeIsNotRestoredAfterListenerDisconnects(t *testing.T) {
	server := newMemoryServer(t)
	client := newTestClient(t, server)
	server.answerHandshake(t, baseSnapshot(1))
	if _, err := client.Subscribe(func(*protocol.ServerSnapshot) { client.Disconnect("") }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	_, err := client.Connect(testContext(t))
	var disconnected *DisconnectedError
	if !errors.As(err, &disconnected) {
		t.Fatalf("Connect error = %#v, want *DisconnectedError", err)
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
	if got := server.clientCloseCount(); got != 1 {
		t.Errorf("transport closes = %d, want 1", got)
	}
}

// TestStaleHandshakeIsNotRestoredWhenListenerReconnects: the listener replaces
// the connection outright. The superseded attempt must not report success, and
// the new one must survive.
func TestStaleHandshakeIsNotRestoredWhenListenerReconnects(t *testing.T) {
	first, second := newMemoryServer(t), newMemoryServer(t)
	first.answerHandshake(t, baseSnapshot(1))
	second.answerHandshake(t, baseSnapshot(2))

	var mu sync.Mutex
	attempts := 0
	client, err := New(Options{
		TransportFactory: func(ctx context.Context, handlers TransportHandlers) (Transport, error) {
			mu.Lock()
			attempt := attempts
			attempts++
			mu.Unlock()
			if attempt == 0 {
				return first.connect(ctx, handlers)
			}
			return second.connect(ctx, handlers)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	var reconnected *protocol.ServerSnapshot
	var reconnectErr error
	requested := false
	if _, err := client.Subscribe(func(*protocol.ServerSnapshot) {
		if requested {
			return
		}
		requested = true
		client.Disconnect("")
		reconnected, reconnectErr = client.Reconnect(testContext(t))
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	_, err = client.Connect(testContext(t))
	var disconnected *DisconnectedError
	if !errors.As(err, &disconnected) {
		t.Fatalf("Connect error = %#v, want *DisconnectedError", err)
	}
	if reconnectErr != nil {
		t.Fatalf("Reconnect: %v", reconnectErr)
	}
	if reconnected == nil || reconnected.Revision != 2 {
		t.Errorf("Reconnect snapshot = %#v, want revision 2", reconnected)
	}
	if client.ConnectionState() != Connected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Connected)
	}
	if got := first.clientCloseCount(); got != 1 {
		t.Errorf("first transport closes = %d, want 1", got)
	}
}

// TestReconnectFromEventListenerIgnoresTheRestOfTheChunk: a listener may
// replace the connection from inside an event — the pattern ConnectionOptions
// documents — and the bytes that shared that chunk belong to the transport that
// is now dead. Handing them to the new connection attributes a stranger's
// response to it, and an unsolicited response tears a connection down.
func TestReconnectFromEventListenerIgnoresTheRestOfTheChunk(t *testing.T) {
	first, second := newMemoryServer(t), newMemoryServer(t)
	first.answerHandshake(t, baseSnapshot(1))
	second.answerHandshake(t, baseSnapshot(2))
	var mu sync.Mutex
	attempts := 0
	client, err := New(Options{
		TransportFactory: func(ctx context.Context, handlers TransportHandlers) (Transport, error) {
			mu.Lock()
			attempt := attempts
			attempts++
			mu.Unlock()
			if attempt == 0 {
				return first.connect(ctx, handlers)
			}
			return second.connect(ctx, handlers)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var reconnectErr error
	reconnects := 0
	if _, err := client.OnEvent(func(event protocol.ServerEvent) {
		if _, ok := event.(*protocol.SessionRemovedEvent); !ok || reconnects > 0 {
			return
		}
		reconnects++
		client.Disconnect("")
		_, reconnectErr = client.Reconnect(testContext(t))
	}); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}

	// One chunk, two messages: the event the listener reacts to, and a response
	// the new connection never asked for.
	first.sendTogether(t,
		&protocol.EventEnvelope{
			Type:  "event",
			Event: &protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "session-1"},
		},
		okResponse("request-stray", detachResult("session-1")),
	)

	if reconnects != 1 {
		t.Fatalf("listener ran %d times, want 1", reconnects)
	}
	if reconnectErr != nil {
		t.Fatalf("Reconnect: %v", reconnectErr)
	}
	if client.ConnectionState() != Connected {
		t.Errorf("ConnectionState() = %q, want %q — the dead transport's trailing bytes"+
			" were applied to the new connection", client.ConnectionState(), Connected)
	}
	if got := second.clientCloseCount(); got != 0 {
		t.Errorf("the new transport was closed %d times, want 0", got)
	}
}

// TestConnectResetsOnlyForTheAttemptItClaims: the owner's cache is dropped in
// the same critical section that claims the attempt, so a Connect that is
// refused because someone else got there first cannot wipe the cache that
// someone else has already filled.
func TestConnectResetsOnlyForTheAttemptItClaims(t *testing.T) {
	server := newMemoryServer(t)
	server.answerHandshake(t, baseSnapshot(1))
	resets := 0
	conn, err := NewConnection(ConnectionOptions{
		TransportFactory: server.connect,
		OnReset:          func() { resets++ },
		OnHandshake:      func(*protocol.ServerSnapshot) {},
		OnMessage:        func(protocol.ServerMessage) {},
		OnStateChange:    func(ConnectionStateChange) {},
	})
	if err != nil {
		t.Fatalf("NewConnection: %v", err)
	}

	if _, err := conn.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if resets != 1 {
		t.Fatalf("resets = %d after the first Connect, want 1", resets)
	}

	if _, err := conn.Connect(testContext(t)); err == nil {
		t.Fatal("a second Connect on a connected Connection succeeded")
	}
	if resets != 1 {
		t.Errorf("resets = %d, want 1 — a refused Connect must not reset the cache", resets)
	}

	conn.Disconnect(nil)
	if _, err := conn.Connect(testContext(t)); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if resets != 2 {
		t.Errorf("resets = %d, want 2 — a fresh attempt starts from a clean cache", resets)
	}
}

// TestReconnectStartsFromACleanCache is the same rule seen from the Client: a
// restarted server must not be described by the previous connection's
// leftovers.
func TestReconnectStartsFromACleanCache(t *testing.T) {
	first, second := newMemoryServer(t), newMemoryServer(t)
	first.answerHandshake(t, baseSnapshot(1))
	second.answerHandshake(t, baseSnapshot(2))
	var mu sync.Mutex
	attempts := 0
	client, err := New(Options{
		TransportFactory: func(ctx context.Context, handlers TransportHandlers) (Transport, error) {
			mu.Lock()
			attempt := attempts
			attempts++
			mu.Unlock()
			if attempt == 0 {
				return first.connect(ctx, handlers)
			}
			return second.connect(ctx, handlers)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	handle := attachTestSession(t, client, first, sessionSnapshot("session-1", 1, true))
	if handle.Snapshot() == nil {
		t.Fatal("the session snapshot was not cached")
	}

	client.Disconnect("")
	if _, err := client.Reconnect(testContext(t)); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	if snapshot := client.Snapshot(); snapshot == nil || snapshot.Revision != 2 {
		t.Errorf("Snapshot() = %#v, want the new server's revision 2", snapshot)
	}
	if got := client.state.SessionSnapshot("session-1"); got != nil {
		t.Errorf("the previous connection's session snapshot survived the reconnect: %#v", got)
	}
}

// TestHelloEncodeFailureIsReportedAsADisconnect: pi evaluates the hello encode
// inside the try that wraps transport.send, so a hello too large for the frame
// limit comes back as PiDisconnectedError like any other failure to get the
// hello out. Reporting the raw encoder error instead would make a caller
// branching on the connection's failure mode miss this one.
func TestHelloEncodeFailureIsReportedAsADisconnect(t *testing.T) {
	server := newMemoryServer(t)
	limit := 1
	client, err := New(Options{
		MaxFrameLength:   &limit,
		TransportFactory: server.connect,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	_, connectErr := client.Connect(testContext(t))
	var disconnected *DisconnectedError
	if !errors.As(connectErr, &disconnected) {
		t.Fatalf("Connect error = %#v, want *DisconnectedError", connectErr)
	}
	// The encoder's complaint is what the message has to say — pi's
	// toDisconnectedError keeps it verbatim — and the cause stays matchable.
	var validation *protocol.ValidationError
	if !errors.As(connectErr, &validation) {
		t.Fatalf("Connect error = %#v, want the encoder's failure as its cause", connectErr)
	}
	if disconnected.Error() != validation.Error() {
		t.Errorf("message = %q, want the encoder's %q", disconnected.Error(), validation.Error())
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
	if got := server.clientCloseCount(); got != 1 {
		t.Errorf("transport closes = %d, want 1", got)
	}
	if got := server.sentCount(); got != 0 {
		t.Errorf("client sent %d chunks, want 0 — the hello never encoded", got)
	}
}

// TestHandshakeCallbackPanicFailsTheConnection: ConnectionOptions is exported,
// so OnHandshake can be a handler this package did not write. pi wraps it in
// try/catch and fails the connection; letting a panic unwind through the
// transport's reading goroutine instead would take the process with it.
func TestHandshakeCallbackPanicFailsTheConnection(t *testing.T) {
	server := newMemoryServer(t)
	server.answerHandshake(t, baseSnapshot(1))
	conn, err := NewConnection(ConnectionOptions{
		TransportFactory: server.connect,
		OnHandshake:      func(*protocol.ServerSnapshot) { panic(errors.New("handler blew up")) },
		OnMessage:        func(protocol.ServerMessage) {},
		OnStateChange:    func(ConnectionStateChange) {},
	})
	if err != nil {
		t.Fatalf("NewConnection: %v", err)
	}

	_, connectErr := conn.Connect(testContext(t))
	if connectErr == nil || connectErr.Error() != "handler blew up" {
		t.Fatalf("Connect error = %#v, want the handler's failure", connectErr)
	}
	if conn.State() != Disconnected {
		t.Errorf("State() = %q, want %q", conn.State(), Disconnected)
	}
	if got := server.clientCloseCount(); got != 1 {
		t.Errorf("transport closes = %d, want 1", got)
	}
}

// TestConnectRejectsHandshakeError locks the typed rejection: the server's
// error code has to survive as something a caller can branch on.
func TestConnectRejectsHandshakeError(t *testing.T) {
	server := newMemoryServer(t)
	server.onMessage(func(protocol.ClientMessage) {
		server.send(t, &protocol.ServerHelloError{
			Type: "hello_error",
			Error: protocol.ProtocolError{
				Code: protocol.ErrorVersion, Message: "Unsupported protocol version",
			},
		})
	})
	client := newTestClient(t, server)

	_, err := client.Connect(testContext(t))
	var serverErr *ServerError
	if !errors.As(err, &serverErr) {
		t.Fatalf("Connect error = %#v, want *ServerError", err)
	}
	if serverErr.Code != protocol.ErrorVersion || serverErr.Message != "Unsupported protocol version" {
		t.Errorf("server error = %#v, want the version error the server sent", serverErr)
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
	if got := server.clientCloseCount(); got != 1 {
		t.Errorf("transport closes = %d, want 1", got)
	}
}

// TestCloseRejectsPendingRequestsAndAllowsReconnect covers the full lifecycle
// sequence a consumer observes across a drop and a reconnect through a fresh
// transport.
func TestCloseRejectsPendingRequestsAndAllowsReconnect(t *testing.T) {
	first, second := newMemoryServer(t), newMemoryServer(t)
	first.answerHandshake(t, baseSnapshot(1))
	second.answerHandshake(t, baseSnapshot(2))
	var mu sync.Mutex
	attempts := 0
	client, err := New(Options{
		TransportFactory: func(ctx context.Context, handlers TransportHandlers) (Transport, error) {
			mu.Lock()
			attempt := attempts
			attempts++
			mu.Unlock()
			if attempt == 0 {
				return first.connect(ctx, handlers)
			}
			return second.connect(ctx, handlers)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	var states []ConnectionState
	if _, err := client.OnConnectionStateChange(func(change ConnectionStateChange) {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, change.State)
	}); err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}
	if _, err := client.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	requests := recordRequests(first)
	listErr := make(chan error, 1)
	go func() {
		_, err := client.ListSessions(testContext(t))
		listErr <- err
	}()
	requests.await(t, 1)
	first.closeTransport()

	var disconnected *DisconnectedError
	if err := <-listErr; !errors.As(err, &disconnected) {
		t.Fatalf("pending request error = %#v, want *DisconnectedError", err)
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}

	snapshot, err := client.Reconnect(testContext(t))
	if err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if snapshot.Revision != 2 {
		t.Errorf("Reconnect snapshot revision = %d, want 2", snapshot.Revision)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []ConnectionState{Connecting, Connected, Disconnected, Connecting, Connected}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
}

// TestReconnectFromDisconnectionListener: a listener is allowed to bring the
// connection straight back up, which is how a supervisor is written.
func TestReconnectFromDisconnectionListener(t *testing.T) {
	first, second := newMemoryServer(t), newMemoryServer(t)
	first.answerHandshake(t, baseSnapshot(1))
	second.answerHandshake(t, baseSnapshot(2))
	var mu sync.Mutex
	attempts := 0
	client, err := New(Options{
		TransportFactory: func(ctx context.Context, handlers TransportHandlers) (Transport, error) {
			mu.Lock()
			attempt := attempts
			attempts++
			mu.Unlock()
			if attempt == 0 {
				return first.connect(ctx, handlers)
			}
			return second.connect(ctx, handlers)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var reconnected *protocol.ServerSnapshot
	var reconnectErr error
	if _, err := client.OnConnectionStateChange(func(change ConnectionStateChange) {
		if change.State == Disconnected && reconnected == nil && reconnectErr == nil {
			reconnected, reconnectErr = client.Reconnect(testContext(t))
		}
	}); err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}

	first.closeTransport()

	if reconnectErr != nil {
		t.Fatalf("Reconnect: %v", reconnectErr)
	}
	if reconnected == nil || reconnected.Revision != 2 {
		t.Errorf("Reconnect snapshot = %#v, want revision 2", reconnected)
	}
	if client.ConnectionState() != Connected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Connected)
	}
}

// TestTransportErrorRejectsPendingRequests locks that the transport's own
// message survives onto the disconnect the caller sees.
func TestTransportErrorRejectsPendingRequests(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	listErr := make(chan error, 1)
	go func() {
		_, err := client.ListSessions(testContext(t))
		listErr <- err
	}()
	requests.await(t, 1)
	server.fail(errors.New("read failed"))

	err := <-listErr
	var disconnected *DisconnectedError
	if !errors.As(err, &disconnected) {
		t.Fatalf("error = %#v, want *DisconnectedError", err)
	}
	if disconnected.Error() != "read failed" {
		t.Errorf("message = %q, want the transport's message", disconnected.Error())
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
}

// TestFrameLimitAppliesBothDirections: the configured limit has to bound what
// is sent as well as what is accepted, or a peer could pick the limit for us.
func TestFrameLimitAppliesBothDirections(t *testing.T) {
	server := newMemoryServer(t)
	limit := 512
	client, err := New(Options{
		MaxFrameLength:   &limit,
		TransportFactory: server.connect,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server.answerHandshake(t, baseSnapshot(1))
	if _, err := client.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	handle := attachTestSession(t, client, server, sessionSnapshot("session-1", 1, true))

	sentBefore := server.sentCount()
	_, err = handle.Prompt(testContext(t), strings.Repeat("x", 1000))
	// EncodeClientMessage reports every refusal as a ValidationError, framing
	// included. Accepting a FrameError as well would let a regression that
	// changed which error the encoder produced pass unnoticed.
	var validation *protocol.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Prompt error = %#v, want *protocol.ValidationError", err)
	}
	if got := server.sentCount(); got != sentBefore {
		t.Errorf("client sent %d chunks after the over-long prompt, want %d", got, sentBefore)
	}

	// An inbound frame that declares a length over the limit is fatal too.
	server.sendRaw([]byte{0, 0, 2, 1})
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
}

// TestInvalidProtocolDataDisconnects: a structurally decodable frame carrying
// the wrong types is still a protocol violation, and the stream position after
// one is not trustworthy.
func TestInvalidProtocolDataDisconnects(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)

	payload, err := cbor.Encode(map[string]any{
		"type":  "event",
		"event": map[string]any{"type": "session_removed", "sessionId": int64(1)},
	}, nil)
	if err != nil {
		t.Fatalf("cbor.Encode: %v", err)
	}
	frame, err := protocol.EncodeFrame(payload)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	server.sendRaw(frame)

	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
}

// TestTruncatedFrameOnCloseIsReported: a stream that stops mid-frame is a
// protocol error, not a clean close, and the pending request must say so.
func TestTruncatedFrameOnCloseIsReported(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	listErr := make(chan error, 1)
	go func() {
		_, err := client.ListSessions(testContext(t))
		listErr <- err
	}()
	requests.await(t, 1)
	server.sendRaw([]byte{0, 0, 0, 2, 1})
	server.closeTransport()

	err := <-listErr
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "truncated") {
		t.Fatalf("error = %#v, want a truncated-frame report", err)
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
}

// TestNewRejectsOutOfRangeFrameLimits locks the constructor's bounds. pi throws
// a TypeError here; a Go caller gets an error instead of an unusable client.
func TestNewRejectsOutOfRangeFrameLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
	}{
		{"zero", 0},
		{"negative", -1},
		{"above the 32-bit prefix", int(maxUint32 + 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newMemoryServer(t)
			limit := tc.limit
			_, err := New(Options{
				MaxFrameLength:   &limit,
				TransportFactory: server.connect,
			})
			if err == nil || !strings.Contains(err.Error(), "maxFrameLength") {
				t.Fatalf("New error = %v, want a maxFrameLength complaint", err)
			}
		})
	}
}

// TestConnectRejectsWhenNotIdle locks the guard against two overlapping
// attempts on one connection.
func TestConnectRejectsWhenNotIdle(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)

	_, err := client.Connect(testContext(t))
	var disconnected *DisconnectedError
	if !errors.As(err, &disconnected) {
		t.Fatalf("Connect error = %#v, want *DisconnectedError", err)
	}
	if disconnected.Error() != "PiClient is already connected" {
		t.Errorf("message = %q, want pi's wording", disconnected.Error())
	}
}

// TestConnectHonoursContextCancellation is Go-specific: pi has no cancellation,
// so a handshake that never answers hangs forever there. Here it must not.
func TestConnectHonoursContextCancellation(t *testing.T) {
	server := newMemoryServer(t)
	// No handshake answer is registered, so the server never replies.
	client := newTestClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	if _, err := client.Connect(ctx); err == nil {
		t.Fatal("Connect returned no error for a cancelled context")
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
}

// TestConnectCancelsTransportEstablishment: the context has to reach the
// transport factory, not just the wait for the handshake. A dial that blocks —
// a Unix socket whose accept backlog is full does exactly this — would
// otherwise hold Connect for as long as the kernel felt like, deadline or not.
func TestConnectCancelsTransportEstablishment(t *testing.T) {
	dialing := make(chan struct{})
	abandoned := make(chan error, 1)
	client, err := New(Options{
		TransportFactory: func(ctx context.Context, _ TransportHandlers) (Transport, error) {
			close(dialing)
			<-ctx.Done()
			abandoned <- ctx.Err()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-dialing
		cancel()
	}()

	_, connectErr := client.Connect(ctx)
	if connectErr == nil {
		t.Fatal("Connect succeeded against a factory that never returned a transport")
	}
	if !errors.Is(connectErr, context.Canceled) {
		t.Errorf("Connect error = %#v, want it to carry context.Canceled", connectErr)
	}
	select {
	case err := <-abandoned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("factory saw %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the factory was never told to give up")
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
}
