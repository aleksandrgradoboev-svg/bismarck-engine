package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// testTimeout bounds every blocking wait in these tests so a regression that
// deadlocks fails loudly instead of hanging the suite.
const testTimeout = 5 * time.Second

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// baseSnapshot is the empty server snapshot the upstream tests hand back from
// the handshake.
func baseSnapshot(revision int64) *protocol.ServerSnapshot {
	return &protocol.ServerSnapshot{
		ServerID:        "server-1",
		ProtocolVersion: protocol.ProtocolVersion,
		Revision:        revision,
		Sessions:        []protocol.SessionMetadata{},
		Models:          []protocol.ModelMetadata{},
	}
}

func serverHello(snapshot *protocol.ServerSnapshot) *protocol.ServerHello {
	return &protocol.ServerHello{
		Type:         "hello",
		Version:      protocol.ProtocolVersion,
		ConnectionID: "connection-1",
		Snapshot:     *snapshot,
	}
}

func okResponse(id string, result protocol.CommandResult) *protocol.ResponseEnvelope {
	return &protocol.ResponseEnvelope{Type: "response", ID: id, OK: true, Result: result}
}

func errorResponse(id string, code protocol.ProtocolErrorCode, message string) *protocol.ResponseEnvelope {
	return &protocol.ResponseEnvelope{
		Type:  "response",
		ID:    id,
		OK:    false,
		Error: &protocol.ProtocolError{Code: code, Message: message},
	}
}

func attachResult(snapshot *protocol.SessionSnapshot) *protocol.SessionResult {
	return &protocol.SessionResult{Command: "attach", Session: *snapshot}
}

func detachResult(sessionID string) *protocol.DetachResult {
	return &protocol.DetachResult{Command: "detach", SessionID: sessionID}
}

// memoryServer is the in-process peer the upstream tests use (test/support.ts):
// it decodes what the client sends and pushes bytes back synchronously, which
// is what makes re-entrant listener behaviour reproducible.
type memoryServer struct {
	mu           sync.Mutex
	handlers     TransportHandlers
	decoder      *protocol.ClientMessageDecoder
	listeners    []func(protocol.ClientMessage)
	sentByClient [][]byte
	closeCount   int
}

func newMemoryServer(t *testing.T) *memoryServer {
	t.Helper()
	decoder, err := protocol.NewClientMessageDecoder(nil)
	if err != nil {
		t.Fatalf("NewClientMessageDecoder: %v", err)
	}
	return &memoryServer{decoder: decoder}
}

// memoryTransport delivers the client's frames straight into the server's
// decoder on the calling goroutine.
type memoryTransport struct {
	server *memoryServer
	mu     sync.Mutex
	closed bool
}

func (s *memoryServer) connect(_ context.Context, handlers TransportHandlers) (Transport, error) {
	s.mu.Lock()
	s.handlers = handlers
	s.mu.Unlock()
	return &memoryTransport{server: s}, nil
}

func (t *memoryTransport) Send(chunk []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return errTransportClosed
	}
	return t.server.receive(chunk)
}

func (t *memoryTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.server.mu.Lock()
	t.server.closeCount++
	t.server.mu.Unlock()
	return nil
}

type transportClosedError struct{}

func (transportClosedError) Error() string { return "Transport is closed" }

var errTransportClosed = transportClosedError{}

// receive decodes a client frame and dispatches it. The lock is dropped before
// listeners run so a listener may push bytes back without deadlocking.
func (s *memoryServer) receive(chunk []byte) error {
	s.mu.Lock()
	s.sentByClient = append(s.sentByClient, append([]byte(nil), chunk...))
	messages, err := s.decoder.Push(chunk)
	listeners := append([]func(protocol.ClientMessage){}, s.listeners...)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, message := range messages {
		for _, listener := range listeners {
			listener(message)
		}
	}
	return nil
}

func (s *memoryServer) onMessage(listener func(protocol.ClientMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, listener)
}

func (s *memoryServer) sendRaw(chunk []byte) {
	s.mu.Lock()
	handlers := s.handlers
	s.mu.Unlock()
	if handlers.OnData != nil {
		handlers.OnData(chunk)
	}
}

func (s *memoryServer) send(t *testing.T, message protocol.ServerMessage) {
	t.Helper()
	s.sendRaw(encodeServer(t, message))
}

// sendSplit delivers one message as two chunks, exercising reassembly.
func (s *memoryServer) sendSplit(t *testing.T, message protocol.ServerMessage, splitAt int) {
	t.Helper()
	frame := encodeServer(t, message)
	s.sendRaw(frame[:splitAt])
	s.sendRaw(frame[splitAt:])
}

// sendTogether coalesces several messages into one chunk, exercising the
// decoder's multi-message path and out-of-order response correlation.
func (s *memoryServer) sendTogether(t *testing.T, messages ...protocol.ServerMessage) {
	t.Helper()
	var chunk []byte
	for _, message := range messages {
		chunk = append(chunk, encodeServer(t, message)...)
	}
	s.sendRaw(chunk)
}

func (s *memoryServer) closeTransport() {
	s.mu.Lock()
	handlers := s.handlers
	s.mu.Unlock()
	if handlers.OnClose != nil {
		handlers.OnClose()
	}
}

func (s *memoryServer) fail(err error) {
	s.mu.Lock()
	handlers := s.handlers
	s.mu.Unlock()
	if handlers.OnError != nil {
		handlers.OnError(err)
	}
}

func (s *memoryServer) clientCloseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCount
}

func (s *memoryServer) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sentByClient)
}

func encodeServer(t *testing.T, message protocol.ServerMessage) []byte {
	t.Helper()
	frame, err := protocol.EncodeServerMessage(message, nil)
	if err != nil {
		t.Fatalf("EncodeServerMessage: %v", err)
	}
	return frame
}

// answerHandshake makes the server reply to the client hello with a snapshot.
func (s *memoryServer) answerHandshake(t *testing.T, snapshot *protocol.ServerSnapshot) {
	t.Helper()
	s.onMessage(func(message protocol.ClientMessage) {
		if _, ok := message.(*protocol.ClientHello); ok {
			s.send(t, serverHello(snapshot))
		}
	})
}

func newTestClient(t *testing.T, server *memoryServer) *Client {
	t.Helper()
	client, err := New(Options{TransportFactory: server.connect})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func connectTestClient(t *testing.T, server *memoryServer) *Client {
	t.Helper()
	client := newTestClient(t, server)
	server.answerHandshake(t, baseSnapshot(1))
	if _, err := client.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return client
}

// requestRecorder collects the request envelopes the client sends and signals
// each arrival, so a test can wait for a request to be in flight before
// answering it.
type requestRecorder struct {
	mu       sync.Mutex
	requests []*protocol.RequestEnvelope
	arrived  chan struct{}
}

func recordRequests(server *memoryServer) *requestRecorder {
	r := &requestRecorder{arrived: make(chan struct{}, 64)}
	server.onMessage(func(message protocol.ClientMessage) {
		request, ok := message.(*protocol.RequestEnvelope)
		if !ok {
			return
		}
		r.mu.Lock()
		r.requests = append(r.requests, request)
		r.mu.Unlock()
		select {
		case r.arrived <- struct{}{}:
		default:
		}
	})
	return r
}

// await blocks until at least n requests have been seen.
func (r *requestRecorder) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		r.mu.Lock()
		got := len(r.requests)
		r.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-r.arrived:
		case <-deadline:
			t.Fatalf("timed out waiting for %d requests, saw %d", n, got)
		}
	}
}

func (r *requestRecorder) all() []*protocol.RequestEnvelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*protocol.RequestEnvelope(nil), r.requests...)
}

func (r *requestRecorder) commands() []string {
	out := []string{}
	for _, request := range r.all() {
		out = append(out, request.Request.CommandName())
	}
	return out
}

func (r *requestRecorder) last(t *testing.T) *protocol.RequestEnvelope {
	t.Helper()
	all := r.all()
	if len(all) == 0 {
		t.Fatal("no requests recorded")
	}
	return all[len(all)-1]
}

func (r *requestRecorder) find(t *testing.T, command string) *protocol.RequestEnvelope {
	t.Helper()
	for _, request := range r.all() {
		if request.Request.CommandName() == command {
			return request
		}
	}
	t.Fatalf("no %s request recorded", command)
	return nil
}

// attachTestSession attaches a session and answers the attach request with the
// given snapshot.
func attachTestSession(
	t *testing.T,
	client *Client,
	server *memoryServer,
	snapshot *protocol.SessionSnapshot,
) *SessionHandle {
	t.Helper()
	requests := recordRequests(server)
	type result struct {
		handle *SessionHandle
		err    error
	}
	done := make(chan result, 1)
	go func() {
		handle, err := client.AttachSession(testContext(t), snapshot.ID)
		done <- result{handle, err}
	}()
	requests.await(t, 1)
	server.send(t, okResponse(requests.find(t, "attach").ID, attachResult(snapshot)))
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("AttachSession: %v", got.err)
		}
		return got.handle
	case <-time.After(testTimeout):
		t.Fatal("AttachSession did not complete")
		return nil
	}
}
