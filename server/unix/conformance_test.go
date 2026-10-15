//go:build unix

package unix_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server/internal/servertest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server/unix"
)

// socketPath returns a short temporary socket path. t.TempDir is deliberately
// not used: its directory name embeds the test name, which routinely pushes the
// path past what sockaddr_un can hold.
func socketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "pis-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "server.sock")
}

type harness struct {
	t       *testing.T
	server  *server.Server
	service *servertest.Service
	path    string
}

func start(t *testing.T, service *servertest.Service, mutate func(*unix.ServerOptions)) *harness {
	t.Helper()
	if service == nil {
		service = servertest.NewService()
	}
	path := socketPath(t)
	options := unix.ServerOptions{Path: path}
	if mutate != nil {
		mutate(&options)
	}
	srv, err := unix.NewServer(service, options)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	h := &harness{t: t, server: srv, service: service, path: path}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	return h
}

func (h *harness) connect() *servertest.Client {
	h.t.Helper()
	addresses := h.server.Addresses()
	if len(addresses) != 1 {
		h.t.Fatalf("expected one address, got %v", addresses)
	}
	client, err := servertest.Dial(addresses[0])
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	h.t.Cleanup(func() { _ = client.Close() })
	return client
}

func (h *harness) connected() *servertest.Client {
	h.t.Helper()
	client := h.connect()
	message, err := client.Hello(protocol.ProtocolVersion)
	if err != nil {
		h.t.Fatalf("hello: %v", err)
	}
	if _, ok := message.(*protocol.ServerHello); !ok {
		h.t.Fatalf("expected a server hello, got %#v", message)
	}
	return client
}

func request(t *testing.T, client *servertest.Client, id string, command protocol.Command) *protocol.ResponseEnvelope {
	t.Helper()
	response, err := client.Request(id, command)
	if err != nil {
		t.Fatalf("request %s: %v", id, err)
	}
	return response
}

func mustSession(t *testing.T, response *protocol.ResponseEnvelope) *protocol.SessionSnapshot {
	t.Helper()
	if !response.OK {
		t.Fatalf("expected success, got %#v", response.Error)
	}
	result, ok := response.Result.(*protocol.SessionResult)
	if !ok {
		t.Fatalf("expected a session result, got %#v", response.Result)
	}
	return &result.Session
}

func isEvent[T protocol.ServerEvent](message protocol.ServerMessage) (T, bool) {
	var zero T
	envelope, ok := message.(*protocol.EventEnvelope)
	if !ok {
		return zero, false
	}
	event, ok := envelope.Event.(T)
	return event, ok
}

func TestFragmentedHello(t *testing.T) {
	t.Parallel()
	h := start(t, nil, nil)
	client := h.connect()

	from := client.Count()
	if err := client.SendFragmented(protocol.NewClientHello(), 2); err != nil {
		t.Fatalf("send: %v", err)
	}
	message, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
		_, ok := m.(*protocol.ServerHello)
		return ok
	})
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	hello, ok := message.(*protocol.ServerHello)
	if !ok {
		t.Fatalf("expected a server hello, got %#v", message)
	}
	if hello.Version != protocol.ProtocolVersion {
		t.Fatalf("version = %d, want %d", hello.Version, protocol.ProtocolVersion)
	}
	if hello.ConnectionID == "" {
		t.Fatal("connectionId must not be empty")
	}
}

func TestHandshakeRejections(t *testing.T) {
	t.Parallel()
	h := start(t, nil, nil)

	t.Run("bad version", func(t *testing.T) {
		client := h.connect()
		message, err := client.Hello(protocol.ProtocolVersion + 1)
		if err != nil {
			t.Fatalf("hello: %v", err)
		}
		helloError, ok := message.(*protocol.ServerHelloError)
		if !ok || helloError.Error.Code != protocol.ErrorVersion {
			t.Fatalf("expected a version hello_error, got %#v", message)
		}
		if err := client.WaitForClose(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("request before hello", func(t *testing.T) {
		client := h.connect()
		from := client.Count()
		if err := client.SendMessage(protocol.NewRequest("too-early", protocol.NewListCommand())); err != nil {
			t.Fatalf("send: %v", err)
		}
		message, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
			_, ok := m.(*protocol.ServerHelloError)
			return ok
		})
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		helloError := message.(*protocol.ServerHelloError)
		if helloError.Error.Code != protocol.ErrorInvalidRequest {
			t.Fatalf("code = %q", helloError.Error.Code)
		}
		if helloError.Error.Message != "The first client message must be hello" {
			t.Fatalf("message = %q", helloError.Error.Message)
		}
		if err := client.WaitForClose(); err != nil {
			t.Fatal(err)
		}
	})

	// Two hellos in one write reach the server as one chunk, so the second is
	// judged while the first one's handshake is still open. pi answers with
	// the hello_error alone: the handshake it interrupted never gets to send
	// its own hello.
	t.Run("duplicate hello in one write", func(t *testing.T) {
		client := h.connect()
		frame, err := protocol.EncodeClientMessage(protocol.NewClientHello(), nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		from := client.Count()
		if err := client.SendBytes(append(append([]byte{}, frame...), frame...)); err != nil {
			t.Fatalf("send: %v", err)
		}
		message, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
			_, ok := m.(*protocol.ServerHelloError)
			return ok
		})
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if got := message.(*protocol.ServerHelloError).Error.Message; got != "hello may only be sent as the first message" {
			t.Fatalf("message = %q", got)
		}
		if err := client.WaitForClose(); err != nil {
			t.Fatal(err)
		}
		for _, received := range client.Messages()[from:] {
			if _, ok := received.(*protocol.ServerHello); ok {
				t.Fatal("the interrupted handshake must not also answer with a hello and a full server snapshot")
			}
		}
	})

	t.Run("duplicate hello", func(t *testing.T) {
		client := h.connected()
		from := client.Count()
		if err := client.SendMessage(protocol.NewClientHello()); err != nil {
			t.Fatalf("send: %v", err)
		}
		message, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
			_, ok := m.(*protocol.ServerHelloError)
			return ok
		})
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		helloError := message.(*protocol.ServerHelloError)
		if helloError.Error.Message != "hello may only be sent as the first message" {
			t.Fatalf("message = %q", helloError.Error.Message)
		}
		if err := client.WaitForClose(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHandshakeTimeout(t *testing.T) {
	t.Parallel()
	h := start(t, nil, func(o *unix.ServerOptions) { o.HandshakeTimeout = 20 * time.Millisecond })
	client := h.connect()
	if err := client.WaitForClose(); err != nil {
		t.Fatal(err)
	}
	if !hasHelloError(client, "Handshake timeout") {
		t.Fatalf("expected a handshake timeout hello_error, got %#v", client.Messages())
	}
}

// The timeout must survive into the handshake itself: a service that never
// answers must not buy the peer an unbounded connection.
func TestHandshakeTimeoutCoversTheHandshake(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	gate := service.DelayNextList()
	h := start(t, service, func(o *unix.ServerOptions) { o.HandshakeTimeout = 20 * time.Millisecond })
	client := h.connect()

	if err := client.SendMessage(protocol.NewClientHello()); err != nil {
		t.Fatalf("send: %v", err)
	}
	<-gate.Entered
	if err := client.WaitForClose(); err != nil {
		t.Fatal(err)
	}
	gate.Open()
	if !hasHelloError(client, "Handshake timeout") {
		t.Fatalf("expected a handshake timeout hello_error, got %#v", client.Messages())
	}
}

func hasHelloError(client *servertest.Client, message string) bool {
	for _, received := range client.Messages() {
		helloError, ok := received.(*protocol.ServerHelloError)
		if ok && helloError.Error.Code == protocol.ErrorInvalidRequest && helloError.Error.Message == message {
			return true
		}
	}
	return false
}

func TestMalformedFrameClosesTheConnection(t *testing.T) {
	t.Parallel()
	h := start(t, nil, nil)
	client := h.connect()

	frame, err := protocol.EncodeFrame([]byte{0xff})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	from := client.Count()
	if err := client.SendBytes(frame); err != nil {
		t.Fatalf("send: %v", err)
	}
	message, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
		_, ok := m.(*protocol.ServerHelloError)
		return ok
	})
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if message.(*protocol.ServerHelloError).Error.Code != protocol.ErrorInvalidRequest {
		t.Fatalf("expected invalid_request, got %#v", message)
	}
	if err := client.WaitForClose(); err != nil {
		t.Fatal(err)
	}
}

func TestOversizedInboundFrameClosesTheConnection(t *testing.T) {
	t.Parallel()
	limit := 128
	h := start(t, nil, func(o *unix.ServerOptions) { o.MaxFrameLength = &limit })
	client := h.connect()

	frame := make([]byte, 4+129)
	frame[3] = 129
	if err := client.SendBytes(frame); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := client.WaitForClose(); err != nil {
		t.Fatal(err)
	}
	for _, message := range client.Messages() {
		if _, ok := message.(*protocol.ServerHello); ok {
			t.Fatal("a hello must not be sent for an oversized frame")
		}
	}
}

// A server hello too large for the configured limit is a server-side failure:
// the connection dies without the peer being told anything, because there is no
// frame small enough to tell it in.
func TestOversizedOutboundFrameClosesTheConnection(t *testing.T) {
	t.Parallel()
	limit := 128
	var errs errorLog
	h := start(t, nil, func(o *unix.ServerOptions) {
		o.MaxFrameLength = &limit
		o.OnError = errs.record
	})
	client := h.connect()

	if err := client.SendMessage(protocol.NewClientHello()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := client.WaitForClose(); err != nil {
		t.Fatal(err)
	}
	if got := client.Messages(); len(got) != 0 {
		t.Fatalf("expected no messages, got %#v", got)
	}
	if len(errs.all()) == 0 {
		t.Fatal("the encode failure must be reported to the error observer")
	}
}

type errorLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *errorLog) record(err error) {
	l.mu.Lock()
	l.errs = append(l.errs, err)
	l.mu.Unlock()
}

func (l *errorLog) all() []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.errs...)
}

// A client whose handshake snapshot went stale while it was being built is
// caught up immediately, rather than waiting for the next unrelated broadcast.
func TestHandshakeCatchUpAfterConcurrentChange(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("shared")
	h := start(t, service, nil)
	controller := h.connected()

	gate := service.DelayNextList()
	joining := h.connect()
	helloDone := make(chan protocol.ServerMessage, 1)
	go func() {
		message, err := joining.Hello(protocol.ProtocolVersion)
		if err != nil {
			t.Error(err)
			close(helloDone)
			return
		}
		helloDone <- message
	}()
	<-gate.Entered

	from := controller.Count()
	request(t, controller, "attach", protocol.NewAttachCommand("shared"))
	if _, err := controller.NextFrom(from, func(m protocol.ServerMessage) bool {
		event, ok := isEvent[*protocol.ServerSnapshotEvent](m)
		return ok && event.Snapshot.Revision >= 1
	}); err != nil {
		t.Fatalf("controller snapshot: %v", err)
	}
	gate.Open()

	message := <-helloDone
	hello, ok := message.(*protocol.ServerHello)
	if !ok {
		t.Fatalf("expected a server hello, got %#v", message)
	}
	catchUp, err := joining.NextFrom(0, func(m protocol.ServerMessage) bool {
		event, ok := isEvent[*protocol.ServerSnapshotEvent](m)
		return ok && event.Snapshot.Revision > hello.Snapshot.Revision
	})
	if err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	event, _ := isEvent[*protocol.ServerSnapshotEvent](catchUp)
	// The snapshot's sessions are durable metadata since 6189e53b3, so the
	// former `locked` assertion has no field to read; the catch-up itself is
	// what this case is proving.
	if len(event.Snapshot.Sessions) != 1 {
		t.Fatalf("expected one session, got %#v", event.Snapshot.Sessions)
	}
}

func TestRequestEventAttachmentAndDisconnect(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("first")
	service.Seed("second")
	h := start(t, service, nil)

	client := h.connect()
	message, err := client.Hello(protocol.ProtocolVersion)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	hello := message.(*protocol.ServerHello)
	if len(hello.Snapshot.Sessions) != 2 ||
		hello.Snapshot.Sessions[0].ID != "first" ||
		hello.Snapshot.Sessions[1].ID != "second" {
		t.Fatalf("unexpected handshake sessions: %#v", hello.Snapshot.Sessions)
	}

	listed := request(t, client, "list", protocol.NewListCommand())
	list, ok := listed.Result.(*protocol.ListResult)
	if !listed.OK || !ok || len(list.Sessions) != 2 {
		t.Fatalf("unexpected list result: %#v", listed)
	}

	for _, id := range []string{"first", "second"} {
		session := mustSession(t, request(t, client, "attach-"+id, protocol.NewAttachCommand(id)))
		if session.ID != id || !session.Attached {
			t.Fatalf("attach %s returned %#v", id, session)
		}
	}

	progress := &protocol.AssistantDeltaProgress{
		Type:         "assistant_delta",
		MessageID:    "assistant-1",
		ContentIndex: 0,
		Kind:         protocol.DeltaText,
		Delta:        "hello",
	}
	from := client.Count()
	service.LatestRuntime("first").EmitProgress(progress)
	received, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
		_, ok := isEvent[*protocol.SessionProgressEvent](m)
		return ok
	})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	event, _ := isEvent[*protocol.SessionProgressEvent](received)
	if event.SessionID != "first" {
		t.Fatalf("progress session = %q", event.SessionID)
	}
	delta, ok := event.Progress.(*protocol.AssistantDeltaProgress)
	if !ok || delta.Delta != "hello" || delta.MessageID != "assistant-1" {
		t.Fatalf("progress round-tripped as %#v", event.Progress)
	}

	detached := request(t, client, "detach", protocol.NewDetachCommand("first"))
	detach, ok := detached.Result.(*protocol.DetachResult)
	if !detached.OK || !ok || detach.SessionID != "first" {
		t.Fatalf("unexpected detach result: %#v", detached)
	}
	if got := service.LatestRuntime("first").DisposeCount(); got != 1 {
		t.Fatalf("dispose count = %d, want 1", got)
	}

	thinking := mustSession(t, request(t, client, "thinking",
		protocol.NewSetThinkingCommand("second", protocol.ThinkingHigh)))
	if thinking.ThinkingLevel != protocol.ThinkingHigh {
		t.Fatalf("thinking level = %q", thinking.ThinkingLevel)
	}

	secondRuntime := service.LatestRuntime("second")
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-secondRuntime.Disposed():
	case <-time.After(servertest.Timeout):
		t.Fatal("the disconnect must dispose the session it was holding")
	}
	if got := secondRuntime.DisposeCount(); got != 1 {
		t.Fatalf("dispose count = %d, want 1", got)
	}
}

func TestTerminalRuntimeErrorDisconnectsAttachedClients(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("terminal")
	var errs errorLog
	h := start(t, service, func(o *unix.ServerOptions) { o.OnError = errs.record })

	client := h.connected()
	request(t, client, "attach", protocol.NewAttachCommand("terminal"))
	runtime := service.LatestRuntime("terminal")

	runtime.SetPhase(protocol.PhaseTurn)
	runtime.EmitError(server.NewLockedError("lock ownership lost", nil))

	if err := client.WaitForClose(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.Disposed():
	case <-time.After(servertest.Timeout):
		t.Fatal("a terminated runtime must be disposed")
	}
	if got := runtime.DisposeCount(); got != 1 {
		t.Fatalf("dispose count = %d, want 1", got)
	}
	if service.Locked("terminal") {
		t.Fatal("the service lock must have been released")
	}
	found := false
	for _, err := range errs.all() {
		var serverError *server.Error
		if errors.As(err, &serverError) && serverError.Code == protocol.ErrorSessionLocked {
			found = true
		}
	}
	if !found {
		t.Fatalf("the terminal error must be reported, got %v", errs.all())
	}

	next := h.connected()
	session := mustSession(t, request(t, next, "attach", protocol.NewAttachCommand("terminal")))
	if session.ID != "terminal" {
		t.Fatalf("re-attach returned %#v", session)
	}
	if service.LatestRuntime("terminal") == runtime {
		t.Fatal("re-attaching must acquire a fresh runtime")
	}
}

func TestServiceFailuresAreNotExposedToClients(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.SetListSessionsHook(func(call int) error {
		if call > 1 {
			return errors.New("private service detail")
		}
		return nil
	})
	var errs errorLog
	h := start(t, service, func(o *unix.ServerOptions) {
		o.OnError = func(err error) {
			errs.record(err)
			panic("observer failure")
		}
	})

	client := h.connected()
	response := request(t, client, "list", protocol.NewListCommand())
	if response.OK {
		t.Fatalf("expected a failure, got %#v", response)
	}
	if response.Error.Code != protocol.ErrorInternal || response.Error.Message != "Internal server error" {
		t.Fatalf("leaked service detail: %#v", response.Error)
	}
	found := false
	for _, err := range errs.all() {
		if err.Error() == "private service detail" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the service error must reach the observer, got %v", errs.all())
	}
}

// A service that is missing an operation says so with a fixed code and a fixed
// message. Whatever the error itself said is not the peer's business.
func TestNotImplementedIsReportedWithAStableMessage(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.SetListSessionsHook(func(call int) error {
		if call > 1 {
			return server.NewNotImplementedError()
		}
		return nil
	})
	h := start(t, service, nil)

	client := h.connected()
	response := request(t, client, "list", protocol.NewListCommand())
	if response.OK {
		t.Fatalf("expected a failure, got %#v", response)
	}
	if response.Error.Code != protocol.ErrorNotImplemented {
		t.Fatalf("code = %q, want not_implemented", response.Error.Code)
	}
	if response.Error.Message != "Operation is not implemented" {
		t.Fatalf("message = %q, want the constant not_implemented message", response.Error.Message)
	}
}

// The message a not_implemented failure carries is discarded on the way out,
// even when a service put private detail in it.
func TestNotImplementedDiscardsTheErrorsOwnMessage(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.SetListSessionsHook(func(call int) error {
		if call > 1 {
			return server.NewError(protocol.ErrorNotImplemented, "private backlog detail", map[string]any{
				"ticket": "private ticket",
			})
		}
		return nil
	})
	h := start(t, service, nil)

	client := h.connected()
	response := request(t, client, "list", protocol.NewListCommand())
	if response.OK {
		t.Fatalf("expected a failure, got %#v", response)
	}
	if response.Error.Message != "Operation is not implemented" {
		t.Fatalf("leaked the error's own message: %q", response.Error.Message)
	}
	if response.Error.Details != nil {
		t.Fatalf("leaked the error's details: %#v", *response.Error.Details)
	}
	assertWireIsClean(t, response)
}

// An InternalError hands its cause to the local observer and nothing at all to
// the peer. Both halves matter: dropping the cause loses the only report of the
// failure, and forwarding it leaks what broke.
func TestInternalErrorCauseIsReportedButNeverSerialized(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("private storage detail: %w", errors.New("private root cause"))
	service := servertest.NewService()
	service.SetListSessionsHook(func(call int) error {
		if call > 1 {
			return server.NewInternalError(cause)
		}
		return nil
	})
	var errs errorLog
	h := start(t, service, func(o *unix.ServerOptions) { o.OnError = errs.record })

	client := h.connected()
	response := request(t, client, "list", protocol.NewListCommand())
	if response.OK {
		t.Fatalf("expected a failure, got %#v", response)
	}
	if response.Error.Code != protocol.ErrorInternal || response.Error.Message != "Internal server error" {
		t.Fatalf("wrong sanitized failure: %#v", response.Error)
	}
	assertWireIsClean(t, response)

	reported := errs.all()
	found := false
	for _, err := range reported {
		if err == cause {
			found = true
		}
		var wrapper *server.InternalError
		if errors.As(err, &wrapper) {
			t.Errorf("the wrapper itself must not be reported, only its cause: %v", err)
		}
	}
	if !found {
		t.Fatalf("the retained cause must reach the observer, got %v", reported)
	}
}

// An InternalError may wrap an *Error, and the wrapping is what decides the
// answer: toProtocolError has to match InternalError first or the wrapped
// error's own code and message go out instead of the sanitized pair. This is
// the case that makes the branch order load-bearing rather than incidental.
func TestInternalErrorWrappingAServerErrorStaysSanitized(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.SetListSessionsHook(func(call int) error {
		if call > 1 {
			return server.NewInternalError(
				fmt.Errorf("private: %w", server.NewNotFoundError("private session name", nil)))
		}
		return nil
	})
	h := start(t, service, nil)

	client := h.connected()
	response := request(t, client, "list", protocol.NewListCommand())
	if response.OK {
		t.Fatalf("expected a failure, got %#v", response)
	}
	if response.Error.Code != protocol.ErrorInternal {
		t.Fatalf("code = %q, want internal_error: the wrapped error's code escaped", response.Error.Code)
	}
	if response.Error.Message != "Internal server error" {
		t.Fatalf("message = %q, want the sanitized message: the wrapped error's message escaped",
			response.Error.Message)
	}
	assertWireIsClean(t, response)
}

// assertWireIsClean re-encodes a response exactly as the transport does and
// fails if the word "private" survives into the bytes a peer reads. Checking
// the frame rather than the struct catches detail smuggled into any field.
func assertWireIsClean(t *testing.T, response *protocol.ResponseEnvelope) {
	t.Helper()
	frame, err := protocol.EncodeServerMessage(response, nil)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}
	if bytes.Contains(frame, []byte("private")) {
		t.Fatalf("private detail reached the wire: %q", frame)
	}
}

// Service and SessionRuntime are exported extension points somebody else
// implements. One that panics must fail the request it was called from, on the
// goroutine the server gave it, rather than taking the process with it.
func TestPanickingServiceFailsTheRequestNotTheProcess(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	var exploding atomic.Bool
	service.SetListSessionsHook(func(int) error {
		if exploding.Load() {
			panic("service exploded")
		}
		return nil
	})
	var errs errorLog
	h := start(t, service, func(o *unix.ServerOptions) { o.OnError = errs.record })
	client := h.connected()

	exploding.Store(true)
	response := request(t, client, "list", protocol.NewListCommand())
	exploding.Store(false)
	if response.OK {
		t.Fatalf("expected a failure, got %#v", response)
	}
	if response.Error.Code != protocol.ErrorInternal || response.Error.Message != "Internal server error" {
		t.Fatalf("leaked the panic to the peer: %#v", response.Error)
	}
	found := false
	for _, err := range errs.all() {
		if strings.Contains(err.Error(), "service exploded") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the panic must reach the error observer, got %v", errs.all())
	}

	// The server is still the server: the next request is answered normally.
	if listed := request(t, client, "after", protocol.NewListCommand()); !listed.OK {
		t.Fatalf("the server stopped serving after a service panic: %#v", listed.Error)
	}
}

func TestResponsesMayCompleteOutOfOrder(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("first")
	h := start(t, service, nil)
	client := h.connected()

	gate := service.DelayNextList()
	slow := make(chan *protocol.ResponseEnvelope, 1)
	go func() {
		response, err := client.Request("slow", protocol.NewListCommand())
		if err != nil {
			t.Error(err)
			close(slow)
			return
		}
		slow <- response
	}()
	<-gate.Entered

	fast := request(t, client, "fast", protocol.NewAttachCommand("first"))
	if !fast.OK {
		t.Fatalf("fast request failed: %#v", fast.Error)
	}
	for _, message := range client.Messages() {
		if response, ok := message.(*protocol.ResponseEnvelope); ok && response.ID == "slow" {
			t.Fatal("the slow response must not arrive before the fast one")
		}
	}

	gate.Open()
	if response := <-slow; response == nil || !response.OK {
		t.Fatalf("slow request failed: %#v", response)
	}
}

func TestGracefulShutdownReleasesEverything(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("first")
	h := start(t, service, nil)
	client := h.connected()
	request(t, client, "attach", protocol.NewAttachCommand("first"))
	runtime := service.LatestRuntime("first")

	if err := h.server.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := client.WaitForClose(); err != nil {
		t.Fatal(err)
	}
	if got := runtime.DisposeCount(); got != 1 {
		t.Fatalf("dispose count = %d, want 1", got)
	}
	if got := h.server.Addresses(); len(got) != 0 {
		t.Fatalf("addresses = %v, want none", got)
	}
	if _, err := os.Lstat(h.path); !os.IsNotExist(err) {
		t.Fatalf("socket still present: %v", err)
	}
	if err := h.server.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// The read loop owns the socket, so the socket has to go when the read loop
// does. A peer that only half-closes otherwise never sees a FIN, and a peer
// that churns connections leaves one server-side descriptor behind per round —
// only the netFD finaliser reclaims those, and running out of descriptors
// creates no GC pressure to trigger it.
func TestPeerHalfCloseEndsTheServerSocket(t *testing.T) {
	t.Parallel()
	// The handshake timeout would close the connection on its own; this is
	// about the read loop, not about the timeout rescuing it.
	h := start(t, nil, func(o *unix.ServerOptions) { o.HandshakeTimeout = time.Minute })
	socket, err := net.DialTimeout("unix", h.path, servertest.Timeout)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer socket.Close()

	if err := socket.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}
	if err := socket.SetReadDeadline(time.Now().Add(servertest.Timeout)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if _, err := socket.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read after half-close = %v, want io.EOF: the server never closed its end of the socket", err)
	}
}

func TestMultipleFramedRequestsInOneChunk(t *testing.T) {
	t.Parallel()
	h := start(t, nil, nil)
	client := h.connected()

	first, err := protocol.EncodeClientMessage(protocol.NewRequest("first", protocol.NewListCommand()), nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	second, err := protocol.EncodeClientMessage(protocol.NewRequest("second", protocol.NewListCommand()), nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	from := client.Count()
	if err := client.SendBytes(append(append([]byte{}, first...), second...)); err != nil {
		t.Fatalf("send: %v", err)
	}
	for _, id := range []string{"first", "second"} {
		message, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
			response, ok := m.(*protocol.ResponseEnvelope)
			return ok && response.ID == id
		})
		if err != nil {
			t.Fatalf("response %s: %v", id, err)
		}
		if !message.(*protocol.ResponseEnvelope).OK {
			t.Fatalf("response %s failed", id)
		}
	}
}
