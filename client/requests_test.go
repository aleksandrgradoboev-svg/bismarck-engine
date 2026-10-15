package client

import (
	"context"
	"errors"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// TestCoalescedOutOfOrderResponses locks correlation by request id: two
// responses arriving in one chunk, in the opposite order to the requests, must
// still reach the right callers.
func TestCoalescedOutOfOrderResponses(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)

	type listOutcome struct {
		sessions []protocol.SessionMetadata
		err      error
	}
	listed := make(chan listOutcome, 1)
	go func() {
		sessions, err := client.ListSessions(testContext(t))
		listed <- listOutcome{sessions, err}
	}()
	requests.await(t, 1)

	type attachOutcome struct {
		handle *SessionHandle
		err    error
	}
	attached := make(chan attachOutcome, 1)
	go func() {
		handle, err := client.AttachSession(testContext(t), "session-1")
		attached <- attachOutcome{handle, err}
	}()
	requests.await(t, 2)

	server.sendTogether(t,
		okResponse(requests.find(t, "attach").ID, attachResult(sessionSnapshot("session-1", 1, true))),
		okResponse(requests.find(t, "list").ID, &protocol.ListResult{
			Command:  "list",
			Sessions: []protocol.SessionMetadata{},
		}),
	)

	list := <-listed
	if list.err != nil {
		t.Fatalf("ListSessions: %v", list.err)
	}
	if len(list.sessions) != 0 {
		t.Errorf("ListSessions returned %d sessions, want 0", len(list.sessions))
	}
	attach := <-attached
	if attach.err != nil {
		t.Fatalf("AttachSession: %v", attach.err)
	}
	if !attach.handle.Attached() {
		t.Error("attached handle reports itself detached")
	}
}

// TestMismatchedResponseFailsRequestAndConnection: a response whose command
// does not match the request it answers means the correlation is unreliable, so
// the request fails and the connection goes with it.
func TestMismatchedResponseFailsRequestAndConnection(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	listErr := make(chan error, 1)
	go func() {
		_, err := client.ListSessions(testContext(t))
		listErr <- err
	}()
	requests.await(t, 1)

	server.send(t, okResponse(requests.last(t).ID, attachResult(sessionSnapshot("session-1", 1, true))))

	err := <-listErr
	var validation *protocol.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %#v, want *protocol.ValidationError", err)
	}
	if validation.Msg != "Response command attach does not match list" {
		t.Errorf("message = %q, want pi's wording", validation.Msg)
	}
	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
}

// TestUnsolicitedResponseFailsConnection: a response with no matching request
// is the same correlation failure seen from the other side.
func TestUnsolicitedResponseFailsConnection(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)

	server.send(t, okResponse("request-999", &protocol.ListResult{
		Command:  "list",
		Sessions: []protocol.SessionMetadata{},
	}))

	if client.ConnectionState() != Disconnected {
		t.Errorf("ConnectionState() = %q, want %q", client.ConnectionState(), Disconnected)
	}
}

// TestTypedRequestErrorsSurface: the server's error code has to reach the
// caller intact, since that is what a caller branches on.
func TestTypedRequestErrorsSurface(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)
	attachErr := make(chan error, 1)
	go func() {
		_, err := client.AttachSession(testContext(t), "locked")
		attachErr <- err
	}()
	requests.await(t, 1)

	server.send(t, errorResponse(requests.last(t).ID, protocol.ErrorSessionLocked, "Already attached"))

	err := <-attachErr
	var serverErr *ServerError
	if !errors.As(err, &serverErr) {
		t.Fatalf("error = %#v, want *ServerError", err)
	}
	if serverErr.Code != protocol.ErrorSessionLocked || serverErr.Message != "Already attached" {
		t.Errorf("server error = %#v, want the locked error the server sent", serverErr)
	}
	if client.ConnectionState() != Connected {
		t.Errorf("ConnectionState() = %q, want %q; a typed error is not a transport failure",
			client.ConnectionState(), Connected)
	}
}

// TestRequestWhileDisconnectedFails locks the two refusals that happen before
// anything is encoded.
func TestRequestWhileDisconnectedFails(t *testing.T) {
	server := newMemoryServer(t)
	client := newTestClient(t, server)

	_, err := client.ListSessions(testContext(t))
	var disconnected *DisconnectedError
	if !errors.As(err, &disconnected) {
		t.Fatalf("error = %#v, want *DisconnectedError", err)
	}

	_ = client.Close()
	_, err = client.ListSessions(testContext(t))
	var disposed *DisposedError
	if !errors.As(err, &disposed) {
		t.Fatalf("error = %#v, want *DisposedError", err)
	}
}

// TestCancelledRequestKeepsCorrelation is Go-specific: abandoning the wait must
// not forget the request, or the server's eventual answer would look
// unsolicited and take the connection down with it.
func TestCancelledRequestKeepsCorrelation(t *testing.T) {
	server := newMemoryServer(t)
	client := connectTestClient(t, server)
	requests := recordRequests(server)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listErr := make(chan error, 1)
	go func() {
		_, err := client.ListSessions(ctx)
		listErr <- err
	}()
	requests.await(t, 1)
	cancel()
	if err := <-listErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %#v, want context.Canceled", err)
	}

	server.send(t, okResponse(requests.last(t).ID, &protocol.ListResult{
		Command:  "list",
		Sessions: []protocol.SessionMetadata{},
	}))

	if client.ConnectionState() != Connected {
		t.Errorf("ConnectionState() = %q, want %q; the late response must be discarded quietly",
			client.ConnectionState(), Connected)
	}
}
