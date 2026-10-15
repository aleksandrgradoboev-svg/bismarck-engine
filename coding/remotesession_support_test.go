package coding

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/client"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// remoteTestTimeout bounds every blocking wait in these tests so a regression
// that deadlocks fails loudly instead of hanging the suite.
const remoteTestTimeout = 5 * time.Second

func remoteContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), remoteTestTimeout)
	t.Cleanup(cancel)
	return ctx
}

// remoteServer is the in-process peer from pi's test/client/support.ts: it
// decodes what the client sends and pushes bytes back on the calling goroutine.
type remoteServer struct {
	mu        sync.Mutex
	handlers  client.TransportHandlers
	decoder   *protocol.ClientMessageDecoder
	listeners []func(protocol.ClientMessage)
	requests  []*protocol.RequestEnvelope
	arrived   chan struct{}
	// autoDetach guards answerDetach against registering twice. Two responders
	// would answer one request twice, and a response the client cannot match to
	// a pending request disconnects it.
	autoDetach bool
}

func newRemoteServer(t *testing.T) *remoteServer {
	t.Helper()
	decoder, err := protocol.NewClientMessageDecoder(nil)
	if err != nil {
		t.Fatalf("NewClientMessageDecoder: %v", err)
	}
	s := &remoteServer{decoder: decoder, arrived: make(chan struct{}, 256)}
	s.onMessage(func(message protocol.ClientMessage) {
		request, ok := message.(*protocol.RequestEnvelope)
		if !ok {
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, request)
		s.mu.Unlock()
		select {
		case s.arrived <- struct{}{}:
		default:
		}
	})
	return s
}

type remoteTransport struct {
	server *remoteServer
	mu     sync.Mutex
	closed bool
}

func (s *remoteServer) connect(_ context.Context, handlers client.TransportHandlers) (client.Transport, error) {
	s.mu.Lock()
	s.handlers = handlers
	s.mu.Unlock()
	return &remoteTransport{server: s}, nil
}

func (t *remoteTransport) Send(chunk []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return errRemoteTransportClosed
	}
	return t.server.receive(chunk)
}

func (t *remoteTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

type remoteTransportClosedError struct{}

func (remoteTransportClosedError) Error() string { return "Transport is closed" }

var errRemoteTransportClosed = remoteTransportClosedError{}

func (s *remoteServer) receive(chunk []byte) error {
	s.mu.Lock()
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

func (s *remoteServer) onMessage(listener func(protocol.ClientMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, listener)
}

// send pushes one message at the client. It reports failures with Errorf rather
// than Fatalf because it runs on whatever goroutine the client is sending or
// reading on: Fatalf there would unwind a client goroutine through Goexit and
// hang the test instead of failing it.
func (s *remoteServer) send(t *testing.T, message protocol.ServerMessage) {
	t.Helper()
	frame, err := protocol.EncodeServerMessage(message, nil)
	if err != nil {
		t.Errorf("EncodeServerMessage: %v", err)
		return
	}
	s.mu.Lock()
	handlers := s.handlers
	s.mu.Unlock()
	if handlers.OnData != nil {
		handlers.OnData(frame)
	}
}

func (s *remoteServer) all() []*protocol.RequestEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*protocol.RequestEnvelope(nil), s.requests...)
}

func (s *remoteServer) commands() []string {
	out := []string{}
	for _, request := range s.all() {
		out = append(out, request.Request.CommandName())
	}
	return out
}

// await blocks until at least n requests have been recorded.
func (s *remoteServer) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(remoteTestTimeout)
	for {
		s.mu.Lock()
		got := len(s.requests)
		s.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-s.arrived:
		case <-deadline:
			t.Fatalf("timed out waiting for %d requests, saw %d", n, got)
		}
	}
}

// awaitCommand blocks until a request for the given command arrives at or after
// index from, and returns it with the index it was found at.
func (s *remoteServer) awaitCommand(t *testing.T, command string, from int) (*protocol.RequestEnvelope, int) {
	t.Helper()
	deadline := time.After(remoteTestTimeout)
	for {
		for i, request := range s.all() {
			if i >= from && request.Request.CommandName() == command {
				return request, i
			}
		}
		select {
		case <-s.arrived:
		case <-deadline:
			t.Fatalf("timed out waiting for a %s request, saw %v", command, s.commands())
			return nil, 0
		}
	}
}

func (s *remoteServer) last(t *testing.T) *protocol.RequestEnvelope {
	t.Helper()
	all := s.all()
	if len(all) == 0 {
		t.Fatal("no requests recorded")
	}
	return all[len(all)-1]
}

// answerAttach makes the server answer every attach with a snapshot built by
// build, which is how pi's respondToAttach works.
func (s *remoteServer) answerAttach(t *testing.T, build func(sessionID string) *protocol.SessionSnapshot) {
	t.Helper()
	s.onMessage(func(message protocol.ClientMessage) {
		request, ok := message.(*protocol.RequestEnvelope)
		if !ok {
			return
		}
		attach, ok := request.Request.(*protocol.AttachCommand)
		if !ok {
			return
		}
		s.send(t, remoteOK(request.ID, &protocol.SessionResult{
			Command: "attach",
			Session: *build(attach.SessionID),
		}))
	})
}

// answerDetach makes the server answer every detach it is sent. It is safe to
// call more than once — only the first call registers a responder.
func (s *remoteServer) answerDetach(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	registered := s.autoDetach
	s.autoDetach = true
	s.mu.Unlock()
	if registered {
		return
	}
	s.onMessage(func(message protocol.ClientMessage) {
		request, ok := message.(*protocol.RequestEnvelope)
		if !ok {
			return
		}
		detach, ok := request.Request.(*protocol.DetachCommand)
		if !ok {
			return
		}
		s.send(t, remoteOK(request.ID, &protocol.DetachResult{
			Command:   "detach",
			SessionID: detach.SessionID,
		}))
	})
}

func remoteOK(id string, result protocol.CommandResult) *protocol.ResponseEnvelope {
	return &protocol.ResponseEnvelope{Type: "response", ID: id, OK: true, Result: result}
}

func remoteErr(id string, code protocol.ProtocolErrorCode, message string) *protocol.ResponseEnvelope {
	return &protocol.ResponseEnvelope{
		Type:  "response",
		ID:    id,
		OK:    false,
		Error: &protocol.ProtocolError{Code: code, Message: message},
	}
}

func remoteEvent(event protocol.ServerEvent) *protocol.EventEnvelope {
	return &protocol.EventEnvelope{Type: "event", Event: event}
}

// remoteServerSnapshot is pi's serverSnapshot fixture.
func remoteServerSnapshot() *protocol.ServerSnapshot {
	return &protocol.ServerSnapshot{
		ServerID:        "server-1",
		ProtocolVersion: protocol.ProtocolVersion,
		Revision:        1,
		Sessions:        []protocol.SessionMetadata{},
		Models:          []protocol.ModelMetadata{},
	}
}

// remoteSnapshot is pi's sessionSnapshot fixture, with the overrides applied by
// a mutator rather than a partial struct.
func remoteSnapshot(id string, overrides ...func(*protocol.SessionSnapshot)) *protocol.SessionSnapshot {
	snapshot := &protocol.SessionSnapshot{
		ID:               id,
		Cwd:              "/workspace",
		CreatedAt:        1,
		UpdatedAt:        1,
		Phase:            protocol.PhaseIdle,
		Model:            protocol.ModelRef{Provider: "faux", ID: "model"},
		ThinkingLevel:    protocol.ThinkingOff,
		Attached:         true,
		Locked:           true,
		Revision:         1,
		Transcript:       []protocol.TranscriptItem{},
		QueuedSteer:      []protocol.UserTranscriptItem{},
		QueuedSteerCount: 0,
	}
	for _, override := range overrides {
		override(snapshot)
	}
	return snapshot
}

func withRevision(revision int64) func(*protocol.SessionSnapshot) {
	return func(s *protocol.SessionSnapshot) { s.Revision = revision }
}

func withPhase(phase protocol.SessionPhase) func(*protocol.SessionSnapshot) {
	return func(s *protocol.SessionSnapshot) { s.Phase = phase }
}

func withTranscript(items ...protocol.TranscriptItem) func(*protocol.SessionSnapshot) {
	return func(s *protocol.SessionSnapshot) { s.Transcript = items }
}

// connectRemoteClient builds a connected client talking to server.
func connectRemoteClient(t *testing.T, server *remoteServer) *client.Client {
	t.Helper()
	c, err := client.New(client.Options{TransportFactory: server.connect})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	server.onMessage(func(message protocol.ClientMessage) {
		if _, ok := message.(*protocol.ClientHello); !ok {
			return
		}
		server.send(t, &protocol.ServerHello{
			Type:         "hello",
			Version:      protocol.ProtocolVersion,
			ConnectionID: "connection-1",
			Snapshot:     *remoteServerSnapshot(),
		})
	})
	if _, err := c.Connect(remoteContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c
}

// openTestSession is pi's openRemoteSession helper: it starts the open, answers
// the attach the client issues, and returns the ready session.
func openTestSession(
	t *testing.T,
	c *client.Client,
	server *remoteServer,
	snapshot *protocol.SessionSnapshot,
	opts RemoteSessionOptions,
) *RemoteSession {
	t.Helper()
	from := len(server.all())
	type result struct {
		session *RemoteSession
		err     error
	}
	done := make(chan result, 1)
	go func() {
		session, err := OpenRemoteSession(remoteContext(t), c, snapshot.ID, opts)
		done <- result{session, err}
	}()
	request, _ := server.awaitCommand(t, "attach", from)
	server.send(t, remoteOK(request.ID, &protocol.SessionResult{Command: "attach", Session: *snapshot}))
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("OpenRemoteSession: %v", got.err)
		}
		closeAtCleanup(t, server, got.session)
		return got.session
	case <-time.After(remoteTestTimeout):
		t.Fatal("OpenRemoteSession did not complete")
		return nil
	}
}

// closeAtCleanup disposes session when the test ends, and holds that disposal to
// its contract: a cleanup that swallows the error is exactly what hides a
// disposal that quietly failed to release what it owned. A session the test
// disposed itself is left alone — it already owns that outcome, and closing it
// again would only re-report it.
//
// pi's harness disposes nothing — vitest just drops the session. Go wants the
// goroutines unwound, but Close awaits the detach round-trip (pi's
// release(relinquishOnFailure: true)), so the cleanup has to answer it.
// Installing the responder here rather than at open keeps it invisible to tests
// that drive detach themselves: it only ever sees requests issued after the
// body finished. The context is bounded because t.Context is already cancelled
// by the time cleanups run, and so a disposal regression fails loudly instead
// of hanging the suite.
func closeAtCleanup(t *testing.T, server *remoteServer, session *RemoteSession) {
	t.Helper()
	t.Cleanup(func() {
		if session.Disposed() {
			return
		}
		server.answerDetach(t)
		ctx, cancel := context.WithTimeout(context.Background(), remoteTestTimeout)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

// goCall runs a blocking session call on its own goroutine, returning a channel
// that carries its error. pi's operations return promises; here they block, so
// tests that need a request in flight start it this way.
func goCall(run func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- run() }()
	return done
}

func awaitErr(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(remoteTestTimeout):
		t.Fatal("operation did not complete")
		return nil
	}
}

// eventually polls until condition holds, which is how these tests wait for a
// notification delivered on the client's reading goroutine.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(remoteTestTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
