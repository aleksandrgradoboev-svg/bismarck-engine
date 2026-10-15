package server_test

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server/internal/servertest"
)

// fakeListener records what the server asked of it without binding anything.
type fakeListener struct {
	mu         sync.Mutex
	address    string
	accept     server.Acceptor
	startCount int
	closeCount int
	startErr   error
}

func (l *fakeListener) Address() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.address
}

func (l *fakeListener) Start(_ context.Context, accept server.Acceptor) error {
	l.mu.Lock()
	l.startCount++
	l.accept = accept
	err := l.startErr
	l.mu.Unlock()
	return err
}

func (l *fakeListener) Close(context.Context) error {
	l.mu.Lock()
	l.closeCount++
	l.address = ""
	l.mu.Unlock()
	return nil
}

func (l *fakeListener) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.startCount, l.closeCount
}

func (l *fakeListener) acceptor() server.Acceptor {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accept
}

func TestListenerComposition(t *testing.T) {
	t.Parallel()
	first := &fakeListener{address: "first"}
	second := &fakeListener{address: "second"}
	srv, err := server.New(servertest.NewService(), server.Options{
		Listeners: []server.Listener{first, second},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if got := srv.Addresses(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("addresses = %v", got)
	}
	if first.acceptor() == nil || second.acceptor() == nil {
		t.Fatal("every listener must be given the server's acceptor")
	}

	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, listener := range []*fakeListener{first, second} {
		if _, closes := listener.counts(); closes != 1 {
			t.Fatalf("close count = %d, want 1", closes)
		}
	}
	if got := srv.Addresses(); len(got) != 0 {
		t.Fatalf("addresses = %v, want none", got)
	}
}

// A failed start must not leave half the transports bound.
func TestStartFailureClosesAlreadyStartedListeners(t *testing.T) {
	t.Parallel()
	failure := errors.New("listener failed")
	first := &fakeListener{address: "first"}
	second := &fakeListener{address: "second", startErr: failure}
	srv, err := server.New(servertest.NewService(), server.Options{
		Listeners: []server.Listener{first, second},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("start error = %v, want the listener's own failure", err)
	}
	if _, closes := first.counts(); closes != 1 {
		t.Fatalf("the started listener must be closed, close count = %d", closes)
	}
	if _, closes := second.counts(); closes != 0 {
		t.Fatalf("the failing listener must not be closed, close count = %d", closes)
	}
}

func TestOptionValidation(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	zero, huge := 0, 1<<33

	cases := []struct {
		name    string
		options server.Options
		want    string
	}{
		{"nil listeners", server.Options{}, "listeners"},
		{"zero frame length", server.Options{
			Listeners: []server.Listener{}, MaxFrameLength: &zero,
		}, "maxFrameLength"},
		{"huge frame length", server.Options{
			Listeners: []server.Listener{}, MaxFrameLength: &huge,
		}, "maxFrameLength"},
		{"huge handshake timeout", server.Options{
			Listeners: []server.Listener{}, HandshakeTimeout: 2_147_483_648 * time.Millisecond,
		}, "handshakeTimeout"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := server.New(service, testCase.options)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, testCase.want)
			}
		})
	}

	// An empty listener slice is a server with no transport, which is a valid
	// configuration and how the core is exercised directly.
	if _, err := server.New(service, server.Options{Listeners: []server.Listener{}}); err != nil {
		t.Fatalf("an empty listener slice must be accepted: %v", err)
	}
}

func TestStartTwiceIsRejected(t *testing.T) {
	t.Parallel()
	srv, err := server.New(servertest.NewService(), server.Options{
		Listeners: []server.Listener{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("a second start must be rejected")
	}
	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("a closed server must not restart")
	}
}

// blockedConn accepts nothing and reports nothing, so a cleanup path that
// waited on its output would never finish.
type blockedConn struct {
	mu         sync.Mutex
	closed     bool
	finalChunk []byte
	done       chan struct{}
}

func newBlockedConn() *blockedConn { return &blockedConn{done: make(chan struct{})} }

func (c *blockedConn) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *blockedConn) Send([]byte) error {
	select {}
}

func (c *blockedConn) Close(finalChunk []byte) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.finalChunk = append([]byte(nil), finalChunk...)
	c.mu.Unlock()
	close(c.done)
	return nil
}

func (c *blockedConn) final() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finalChunk
}

// The handshake timeout must close a connection whose output queue never
// drains, and it must carry the reason in the connection's final bytes.
func TestHandshakeTimeoutDoesNotWaitForOutput(t *testing.T) {
	t.Parallel()
	srv, err := server.New(servertest.NewService(), server.Options{
		Listeners:        []server.Listener{},
		HandshakeTimeout: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	conn := newBlockedConn()
	srv.Accept(conn)

	select {
	case <-conn.done:
	case <-time.After(servertest.Timeout):
		t.Fatal("the handshake timeout must close the connection")
	}

	decoder, err := protocol.NewServerMessageDecoder(nil)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	messages, err := decoder.Push(conn.final())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected exactly one final message, got %d", len(messages))
	}
	helloError, ok := messages[0].(*protocol.ServerHelloError)
	if !ok || helloError.Error.Code != protocol.ErrorInvalidRequest {
		t.Fatalf("final message = %#v", messages[0])
	}
	if helloError.Error.Message != "Handshake timeout" {
		t.Fatalf("message = %q", helloError.Error.Message)
	}

	// THE WIRE IS GOLDEN: a Node peer parses these exact bytes, so the frame
	// layout and the CBOR key order are pinned here rather than left to
	// whatever the encoder happens to do.
	const want = "00000048" +
		"a2" +
		"6474797065" + "6b68656c6c6f5f6572726f72" +
		"656572726f72" + "a2" +
		"64636f6465" + "6f696e76616c69645f72657175657374" +
		"676d657373616765" + "7148616e647368616b652074696d656f7574"
	if got := hex.EncodeToString(conn.final()); got != want {
		t.Fatalf("final frame bytes:\n got %s\nwant %s", got, want)
	}

	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A connection accepted after the server started closing is closed at once
// rather than being adopted.
func TestAcceptAfterCloseClosesTheConnection(t *testing.T) {
	t.Parallel()
	srv, err := server.New(servertest.NewService(), server.Options{
		Listeners: []server.Listener{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	conn := newBlockedConn()
	handler := srv.Accept(conn)
	if !conn.Closed() {
		t.Fatal("a connection accepted after close must be closed")
	}
	// The handler is still safe to drive: a transport does not know the server
	// gave up on this connection until it observes the close.
	handler.OnData([]byte{0x00})
	handler.OnError(errors.New("late failure"))
	handler.OnClose()
}

// The sequential case is the easy half. A connection that arrives while Close
// is running must be closed too — whichever side of the shutdown it lands on.
func TestAcceptRacingCloseNeverOutlivesTheServer(t *testing.T) {
	t.Parallel()
	for round := range 3000 {
		srv, err := server.New(servertest.NewService(), server.Options{
			Listeners: []server.Listener{},
			// A minute, so it is the shutdown that closes the connection and
			// never the handshake timeout stepping in to cover for it.
			HandshakeTimeout: time.Minute,
		})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		conn := newBlockedConn()
		var accepted sync.WaitGroup
		accepted.Add(1)
		go func() {
			defer accepted.Done()
			srv.Accept(conn)
		}()
		if err := srv.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
		accepted.Wait()
		if !conn.Closed() {
			t.Fatalf("round %d: a connection accepted around Close was adopted and then left open, "+
				"with its socket, its goroutines and its handshake timer outliving the server", round)
		}
	}
}

func TestServerIDIsStableAndGeneratedWhenAbsent(t *testing.T) {
	t.Parallel()
	explicit, err := server.New(servertest.NewService(), server.Options{
		Listeners: []server.Listener{}, ServerID: "fixed",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if explicit.ID() != "fixed" {
		t.Fatalf("id = %q, want the configured one", explicit.ID())
	}

	first, err := server.New(servertest.NewService(), server.Options{
		Listeners: []server.Listener{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	second, err := server.New(servertest.NewService(), server.Options{
		Listeners: []server.Listener{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if first.ID() == "" || first.ID() == second.ID() {
		t.Fatalf("generated ids must be non-empty and distinct: %q, %q", first.ID(), second.ID())
	}
	if len(first.ID()) != 36 {
		t.Fatalf("generated id %q is not in the canonical UUID form", first.ID())
	}
}

func TestErrorConstructors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err     *server.Error
		code    protocol.ProtocolErrorCode
		message string
	}{
		{server.NewBusyError("", nil), protocol.ErrorBusy, "Session is busy"},
		{server.NewLockedError("", nil), protocol.ErrorSessionLocked, "Session is locked"},
		{server.NewNotFoundError("", nil), protocol.ErrorNotFound, "Session was not found"},
		{server.NewBusyError("custom", nil), protocol.ErrorBusy, "custom"},
	}
	for _, testCase := range cases {
		if testCase.err.Code != testCase.code || testCase.err.Error() != testCase.message {
			t.Fatalf("error = %#v, want %q/%q", testCase.err, testCase.code, testCase.message)
		}
	}

	// Wrapped errors must still be recognised, which is the whole reason these
	// are typed rather than formatted.
	wrapped := errors.Join(errors.New("context"), server.NewNotFoundError("gone", nil))
	var found *server.Error
	if !errors.As(wrapped, &found) || found.Code != protocol.ErrorNotFound {
		t.Fatalf("errors.As did not recover the server error from %v", wrapped)
	}
}
