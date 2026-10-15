//go:build unix

package unix_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server/internal/servertest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server/unix"
)

func TestServerSnapshotRevisionsAreSerialized(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	h := start(t, service, nil)
	client := h.connected()
	from := client.Count()

	// Each broadcast reads the model list once, so gating that call is a way to
	// hold one broadcast open and watch whether the next one starts anyway.
	var calls atomic.Int32
	firstEntered, secondEntered := make(chan struct{}), make(chan struct{})
	firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
	service.SetListModelsHook(func(int) error {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-firstRelease
		case 2:
			close(secondEntered)
			<-secondRelease
		}
		return nil
	})

	created := make(chan struct{}, 2)
	for _, id := range []string{"first", "second"} {
		go func() {
			request(t, client, id, &protocol.CreateCommand{Command: "create"})
			created <- struct{}{}
		}()
	}
	<-created
	<-created

	<-firstEntered
	select {
	case <-secondEntered:
		t.Fatal("a second broadcast must not start while the first is still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(firstRelease)
	<-secondEntered
	close(secondRelease)

	if _, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
		event, ok := isEvent[*protocol.ServerSnapshotEvent](m)
		return ok && event.Snapshot.Revision == 2
	}); err != nil {
		t.Fatalf("second broadcast: %v", err)
	}

	var revisions []int64
	for _, message := range client.Messages()[from:] {
		if event, ok := isEvent[*protocol.ServerSnapshotEvent](message); ok {
			revisions = append(revisions, event.Snapshot.Revision)
		}
	}
	// Every broadcast is its own pass and they never overlap, so the revisions
	// a client sees are consecutive and ascending.
	for i, revision := range revisions {
		if revision != int64(i+1) {
			t.Fatalf("revisions = %v, want 1..n ascending", revisions)
		}
	}
	if len(revisions) < 2 {
		t.Fatalf("revisions = %v, want at least two broadcasts", revisions)
	}
}

// pi sends progress straight from the emit and only suspends on the snapshot
// arm, so a runtime that emits a snapshot and then progress in one call stack
// puts the progress on the wire first. A client applying deltas onto its last
// snapshot double-applies one if the order is reversed.
func TestProgressOvertakesASnapshotStillBeingBuilt(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("session-1")
	h := start(t, service, nil)
	client := h.connected()
	request(t, client, "attach", protocol.NewAttachCommand("session-1"))
	runtime := service.LatestRuntime("session-1")

	from := client.Count()
	// Holding the snapshot read open is what makes the order observable: the
	// snapshot event cannot be built yet, and the progress must not be waiting
	// on it.
	runtime.BlockSnapshots()
	// Released on the way out too, so a failure here does not leave the
	// shutdown waiting on a snapshot that never comes.
	defer runtime.ReleaseSnapshots()
	runtime.EmitSnapshot()
	runtime.EmitProgress(&protocol.AssistantDeltaProgress{
		Type: "assistant_delta", MessageID: "assistant-1", ContentIndex: 0,
		Kind: protocol.DeltaText, Delta: "hello",
	})

	if _, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
		_, ok := isEvent[*protocol.SessionProgressEvent](m)
		return ok
	}); err != nil {
		t.Fatalf("progress must reach the peer ahead of the snapshot emitted before it: %v", err)
	}

	runtime.ReleaseSnapshots()
	if _, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
		_, ok := isEvent[*protocol.SessionSnapshotEvent](m)
		return ok
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
}

// A Close given a deadline must return by it. A service that never answers
// otherwise holds the caller for as long as it likes.
func TestCloseGivesUpWhenItsContextExpires(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("session-1")
	h := start(t, service, nil)
	client := h.connected()

	gate := service.DelayNextList()
	request(t, client, "attach", protocol.NewAttachCommand("session-1"))
	<-gate.Entered
	defer gate.Open()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- h.server.Close(ctx) }()
	select {
	case err := <-closed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("close error = %v, want the deadline that was set", err)
		}
	case <-time.After(servertest.Timeout):
		t.Fatal("Close ignored its context and waited on a service that never answered")
	}
}

func TestCreateListAttachDetach(t *testing.T) {
	t.Parallel()
	h := start(t, nil, nil)
	client := h.connected()

	cwd, name := "/work", "Created"
	created := mustSession(t, request(t, client, "create", &protocol.CreateCommand{
		Command: "create", Cwd: &cwd, Name: &name,
	}))
	if created.ID != h.service.LastCreatedID() {
		t.Fatalf("session id %q is not the server-assigned id %q", created.ID, h.service.LastCreatedID())
	}
	if created.Cwd != cwd || created.Name == nil || *created.Name != name {
		t.Fatalf("create returned %#v", created)
	}
	if !created.Attached || !created.Locked {
		t.Fatalf("a created session must be attached and locked: %#v", created)
	}

	listed := request(t, client, "list", protocol.NewListCommand())
	list := listed.Result.(*protocol.ListResult)
	// 6189e53b3 removed attached/locked from the listed shape; the live view of
	// those still rides on the session snapshot asserted just above.
	if len(list.Sessions) != 1 || list.Sessions[0].ID != created.ID {
		t.Fatalf("list returned %#v", list.Sessions)
	}

	detached := request(t, client, "detach-1", protocol.NewDetachCommand(created.ID))
	if !detached.OK {
		t.Fatalf("detach failed: %#v", detached.Error)
	}
	// Detaching twice is not idempotent: the second one no longer holds the
	// session, and pi answers that with invalid_request rather than pretending.
	again := request(t, client, "detach-2", protocol.NewDetachCommand(created.ID))
	if again.OK {
		t.Fatalf("a second detach must fail, got %#v", again.Result)
	}
	if again.Error.Code != protocol.ErrorInvalidRequest {
		t.Fatalf("code = %q, want invalid_request", again.Error.Code)
	}
	if again.Error.Message != "Connection is not attached to session "+created.ID {
		t.Fatalf("message = %q", again.Error.Message)
	}
	if got := h.service.LatestRuntime(created.ID).DisposeCount(); got != 1 {
		t.Fatalf("dispose count = %d, want 1", got)
	}

	reattached := mustSession(t, request(t, client, "reattach", protocol.NewAttachCommand(created.ID)))
	if reattached.ID != created.ID {
		t.Fatalf("re-attach returned %q", reattached.ID)
	}
	if got := len(h.service.Runtimes(created.ID)); got != 2 {
		t.Fatalf("runtimes = %d, want 2", got)
	}
}

func TestAttachmentsOnOneConnectionAreIndependent(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("first")
	service.Seed("second")
	h := start(t, service, nil)
	client := h.connected()

	request(t, client, "a1", protocol.NewAttachCommand("first"))
	request(t, client, "a2", protocol.NewAttachCommand("second"))
	request(t, client, "d1", protocol.NewDetachCommand("first"))

	if got := service.LatestRuntime("first").DisposeCount(); got != 1 {
		t.Fatalf("first dispose count = %d, want 1", got)
	}
	if got := service.LatestRuntime("second").DisposeCount(); got != 0 {
		t.Fatalf("second dispose count = %d, want 0", got)
	}
	session := mustSession(t, request(t, client, "t",
		protocol.NewSetThinkingCommand("second", protocol.ThinkingMedium)))
	if session.ID != "second" || session.ThinkingLevel != protocol.ThinkingMedium {
		t.Fatalf("set_thinking returned %#v", session)
	}
}

func TestSessionEventsReachOnlyAttachedClients(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("session-1")
	h := start(t, service, nil)
	attached := h.connected()
	unattached := h.connected()
	request(t, attached, "attach", protocol.NewAttachCommand("session-1"))

	runtime := service.LatestRuntime("session-1")
	from := attached.Count()
	runtime.EmitProgress(&protocol.AssistantDeltaProgress{
		Type: "assistant_delta", MessageID: "assistant-1", ContentIndex: 0,
		Kind: protocol.DeltaText, Delta: "hello",
	})
	if _, err := attached.NextFrom(from, func(m protocol.ServerMessage) bool {
		_, ok := isEvent[*protocol.SessionProgressEvent](m)
		return ok
	}); err != nil {
		t.Fatalf("progress: %v", err)
	}

	from = attached.Count()
	runtime.EmitSnapshot()
	if _, err := attached.NextFrom(from, func(m protocol.ServerMessage) bool {
		event, ok := isEvent[*protocol.SessionSnapshotEvent](m)
		return ok && event.Snapshot.ID == "session-1" && event.Snapshot.Attached && event.Snapshot.Locked
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	for _, message := range unattached.Messages() {
		if _, ok := isEvent[*protocol.SessionProgressEvent](message); ok {
			t.Fatal("progress must not reach an unattached client")
		}
		if _, ok := isEvent[*protocol.SessionSnapshotEvent](message); ok {
			t.Fatal("session snapshots must not reach an unattached client")
		}
	}
}

func TestOneLiveRuntimeIsSharedByEveryAttachedClient(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("session-1")
	h := start(t, service, nil)
	first := h.connected()
	second := h.connected()
	request(t, first, "attach", protocol.NewAttachCommand("session-1"))

	listed := request(t, second, "list", protocol.NewListCommand())
	sessions := listed.Result.(*protocol.ListResult).Sessions
	// Before 6189e53b3 this asserted the session listed as locked-but-unattached
	// for the second connection. That distinction left the wire with the
	// per-connection fields, so what remains to check is that the session is
	// listed at all -- the shared-runtime assertion below is the real subject.
	if len(sessions) != 1 || sessions[0].ID != "session-1" {
		t.Fatalf("a session held by another connection must still be listed: %#v", sessions)
	}

	request(t, second, "attach", protocol.NewAttachCommand("session-1"))
	if got := len(service.Runtimes("session-1")); got != 1 {
		t.Fatalf("runtimes = %d, want 1 shared runtime", got)
	}

	from := first.Count()
	session := mustSession(t, request(t, second, "model",
		protocol.NewSetModelCommand("session-1", protocol.ModelRef{Provider: "test", ID: "large"})))
	if session.Model.ID != "large" {
		t.Fatalf("set_model returned %#v", session.Model)
	}
	if _, err := first.NextFrom(from, func(m protocol.ServerMessage) bool {
		event, ok := isEvent[*protocol.SessionSnapshotEvent](m)
		return ok && event.Snapshot.Model.ID == "large"
	}); err != nil {
		t.Fatalf("the other attached client must observe the change: %v", err)
	}

	thinking := mustSession(t, request(t, first, "thinking",
		protocol.NewSetThinkingCommand("session-1", protocol.ThinkingHigh)))
	if thinking.ThinkingLevel != protocol.ThinkingHigh {
		t.Fatalf("set_thinking returned %#v", thinking)
	}
}

func TestPromptsAreNotQueuedAndSteerAndAbortRunDuringOne(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("session-1")
	h := start(t, service, nil)
	client := h.connected()
	request(t, client, "attach", protocol.NewAttachCommand("session-1"))

	from := client.Count()
	prompt := make(chan *protocol.ResponseEnvelope, 1)
	go func() {
		response, err := client.Request("prompt", protocol.NewPromptCommand("session-1", "first"))
		if err != nil {
			t.Error(err)
			close(prompt)
			return
		}
		prompt <- response
	}()
	if _, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
		event, ok := isEvent[*protocol.SessionSnapshotEvent](m)
		return ok && event.Snapshot.Phase == protocol.PhaseTurn
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}

	busy := request(t, client, "busy", protocol.NewPromptCommand("session-1", "second"))
	if busy.OK || busy.Error.Code != protocol.ErrorBusy {
		t.Fatalf("a second prompt must be rejected as busy, got %#v", busy)
	}

	steer := request(t, client, "steer", protocol.NewSteerCommand("session-1", "adjust"))
	if !steer.OK {
		t.Fatalf("steer failed: %#v", steer.Error)
	}
	if got := service.LatestRuntime("session-1").Steers(); len(got) != 1 || got[0].Text != "adjust" {
		t.Fatalf("steers = %#v", got)
	}

	abort := request(t, client, "abort", protocol.NewAbortCommand("session-1"))
	if !abort.OK {
		t.Fatalf("abort failed: %#v", abort.Error)
	}
	response := <-prompt
	if response == nil || !response.OK {
		t.Fatalf("prompt failed: %#v", response)
	}
	if session := response.Result.(*protocol.SessionResult).Session; session.Phase != protocol.PhaseIdle {
		t.Fatalf("phase = %q, want idle", session.Phase)
	}
}

// An operation's result reports attachment from the requesting connection's
// point of view, even when that connection detached while the work ran.
func TestOperationResultsReportTheRequesterAttachment(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("session-1")
	h := start(t, service, nil)
	first := h.connected()
	second := h.connected()
	request(t, first, "a1", protocol.NewAttachCommand("session-1"))
	request(t, second, "a2", protocol.NewAttachCommand("session-1"))

	from := first.Count()
	prompt := make(chan *protocol.ResponseEnvelope, 1)
	go func() {
		response, err := first.Request("prompt", protocol.NewPromptCommand("session-1", "hello"))
		if err != nil {
			t.Error(err)
			close(prompt)
			return
		}
		prompt <- response
	}()
	if _, err := first.NextFrom(from, func(m protocol.ServerMessage) bool {
		event, ok := isEvent[*protocol.SessionSnapshotEvent](m)
		return ok && event.Snapshot.Phase == protocol.PhaseTurn
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	request(t, first, "detach", protocol.NewDetachCommand("session-1"))
	if err := service.LatestRuntime("session-1").FinishPrompt(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	response := <-prompt
	if response == nil || !response.OK {
		t.Fatalf("prompt failed: %#v", response)
	}
	session := response.Result.(*protocol.SessionResult).Session
	if session.ID != "session-1" || session.Attached {
		t.Fatalf("expected an unattached result for the detached requester, got %#v", session)
	}
}

// Work in flight outlives the connection that started it; the runtime is only
// released once it next goes idle.
func TestBusyWorkSurvivesDisconnect(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("session-1")
	h := start(t, service, nil)
	client := h.connected()
	request(t, client, "attach", protocol.NewAttachCommand("session-1"))

	from := client.Count()
	go func() { _, _ = client.Request("prompt", protocol.NewPromptCommand("session-1", "survive")) }()
	if _, err := client.NextFrom(from, func(m protocol.ServerMessage) bool {
		event, ok := isEvent[*protocol.SessionSnapshotEvent](m)
		return ok && event.Snapshot.Phase == protocol.PhaseTurn
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}

	runtime := service.LatestRuntime("session-1")
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := runtime.DisposeCount(); got != 0 {
		t.Fatalf("dispose count = %d, want 0 while the turn is running", got)
	}
	if err := runtime.FinishPrompt(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	select {
	case <-runtime.Disposed():
	case <-time.After(servertest.Timeout):
		t.Fatal("the runtime must be released once it goes idle")
	}

	reconnect := h.connected()
	session := mustSession(t, request(t, reconnect, "attach", protocol.NewAttachCommand("session-1")))
	if len(session.Transcript) != 2 {
		t.Fatalf("transcript = %#v, want the user turn and its reply", session.Transcript)
	}
	assistant, ok := session.Transcript[1].(*protocol.AssistantTranscriptItem)
	if !ok {
		t.Fatalf("second item = %#v", session.Transcript[1])
	}
	text, ok := assistant.Content[0].(*protocol.TextContent)
	if !ok || text.Text != "reply:survive" {
		t.Fatalf("assistant content = %#v", assistant.Content)
	}
}

func TestSessionsAreRestoredLazilyAfterRestart(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("session-1")

	first := start(t, service, nil)
	firstClient := first.connected()
	request(t, firstClient, "attach", protocol.NewAttachCommand("session-1"))
	request(t, firstClient, "thinking", protocol.NewSetThinkingCommand("session-1", protocol.ThinkingHigh))
	if err := firstClient.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := first.server.Close(context.Background()); err != nil {
		t.Fatalf("close server: %v", err)
	}

	second := start(t, service, nil)
	if got := len(service.Runtimes("session-1")); got != 1 {
		t.Fatalf("a restarted server must not acquire runtimes eagerly: %d", got)
	}
	secondClient := second.connected()
	restored := mustSession(t, request(t, secondClient, "attach", protocol.NewAttachCommand("session-1")))
	if restored.ThinkingLevel != protocol.ThinkingHigh {
		t.Fatalf("thinking level = %q, want the persisted one", restored.ThinkingLevel)
	}
	if got := len(service.Runtimes("session-1")); got != 2 {
		t.Fatalf("runtimes = %d, want 2", got)
	}
}

func TestServiceRuntimeWithTheWrongIDIsRejectedAndDisposed(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.SetCreateSessionIDOverride("wrong-id")
	h := start(t, service, nil)
	client := h.connected()

	response := request(t, client, "create", &protocol.CreateCommand{Command: "create"})
	if response.OK || response.Error.Code != protocol.ErrorInvalidRequest {
		t.Fatalf("expected an invalid_request failure, got %#v", response)
	}
	// invalid_request crosses the protocol boundary, so this message reaches the
	// client verbatim. Pin the exact wording: a Go server and a Node server must
	// report the same text for the same protocol error.
	wantMessage := fmt.Sprintf(
		"Service returned session wrong-id for server-assigned session %s", service.LastAssignedID())
	if response.Error.Message != wantMessage {
		t.Fatalf("wire message = %q, want %q", response.Error.Message, wantMessage)
	}
	runtime := service.LatestRuntime("wrong-id")
	if runtime == nil {
		t.Fatal("the service runtime must have been acquired before it was rejected")
	}
	if got := runtime.DisposeCount(); got != 1 {
		t.Fatalf("dispose count = %d, want 1", got)
	}
}

func TestLockedSessionsAndUnattachedControl(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()
	service.Seed("locked")
	service.ForceLock("locked")
	h := start(t, service, nil)
	client := h.connected()

	locked := request(t, client, "attach", protocol.NewAttachCommand("locked"))
	if locked.OK || locked.Error.Code != protocol.ErrorSessionLocked {
		t.Fatalf("expected session_locked, got %#v", locked)
	}
	unattached := request(t, client, "abort", protocol.NewAbortCommand("locked"))
	if unattached.OK || unattached.Error.Code != protocol.ErrorInvalidRequest {
		t.Fatalf("expected invalid_request for an unattached session, got %#v", unattached)
	}
	if unattached.Error.Message != "Connection is not attached to session locked" {
		t.Fatalf("message = %q", unattached.Error.Message)
	}
}

func TestPresetRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	service := servertest.NewService()

	long := "/tmp/" + string(make([]byte, 512))
	if _, err := unix.NewServer(service, unix.ServerOptions{Path: long}); err == nil {
		t.Fatal("an over-long socket path must be rejected")
	}
	limit := 128
	if _, err := unix.NewServer(service, unix.ServerOptions{
		Path: "/tmp/pi.sock", MaxFrameLength: &limit, MaxPendingBytes: 131,
	}); err == nil {
		t.Fatal("a pending-byte limit below one maximum frame must be rejected")
	}
	if _, err := unix.NewServer(service, unix.ServerOptions{
		Path: "/tmp/pi.sock", HandshakeTimeout: 2_147_483_648 * time.Millisecond,
	}); err == nil {
		t.Fatal("a handshake timeout above Node's maximum timer delay must be rejected")
	}
	if _, err := unix.NewServer(service, unix.ServerOptions{
		Path: "/tmp/pi.sock", GracefulCloseTimeout: 2_147_483_648 * time.Millisecond,
	}); err == nil {
		t.Fatal("a graceful close timeout above Node's maximum timer delay must be rejected")
	}

	// Both timeouts are Node timer delays in whole milliseconds; anything
	// finer is a configuration a Node pi server refuses to start on.
	if _, err := unix.NewServer(service, unix.ServerOptions{
		Path: "/tmp/pi.sock", HandshakeTimeout: 500 * time.Microsecond,
	}); err == nil {
		t.Fatal("a sub-millisecond handshake timeout must be rejected")
	}
	if _, err := unix.NewServer(service, unix.ServerOptions{
		Path: "/tmp/pi.sock", GracefulCloseTimeout: 500 * time.Microsecond,
	}); err == nil {
		t.Fatal("a sub-millisecond graceful close timeout must be rejected")
	}
}
