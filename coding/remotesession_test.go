package coding

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/client"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// lifecycleLabel renders a lifecycle the way pi's tests do, so the recorded
// sequences below are the upstream sequences.
func lifecycleLabel(lifecycle RemoteSessionLifecycle) string {
	if lifecycle.Status == LifecycleBusy {
		return string(lifecycle.Status) + ":" + string(lifecycle.Operation)
	}
	return string(lifecycle.Status)
}

// recorder collects what subscribers were told, guarded because notifications
// arrive on both caller and reader goroutines.
type recorder struct {
	mu     sync.Mutex
	states []RemoteSessionState
}

func (r *recorder) record(state RemoteSessionState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, state)
}

func (r *recorder) lifecycles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, state := range r.states {
		out = append(out, lifecycleLabel(state.Lifecycle))
	}
	return out
}

func (r *recorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.states)
}

func subscribeRecorder(t *testing.T, session *RemoteSession) *recorder {
	t.Helper()
	r := &recorder{}
	if _, err := session.Subscribe(r.record); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return r
}

func TestRemoteSessionProjectsProgressForSubscribers(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1",
		withPhase(protocol.PhaseTurn),
		withTranscript(&protocol.AssistantTranscriptItem{
			ID:        "assistant-1",
			Role:      "assistant",
			Content:   []protocol.Content{text("hello")},
			Status:    protocol.AssistantStreaming,
			Model:     protocol.ModelRef{Provider: "faux", ID: "model"},
			Timestamp: 1,
		}),
	), RemoteSessionOptions{})

	var (
		mu    sync.Mutex
		views []string
	)
	if _, err := session.Subscribe(func(state RemoteSessionState) {
		if len(state.Transcript) == 0 {
			return
		}
		assistant, ok := state.Transcript[0].(*protocol.AssistantTranscriptItem)
		if !ok {
			return
		}
		if block, ok := assistant.Content[0].(*protocol.TextContent); ok {
			mu.Lock()
			views = append(views, block.Text)
			mu.Unlock()
		}
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	server.send(t, remoteEvent(&protocol.SessionProgressEvent{
		Type:      "session_progress",
		SessionID: "session-1",
		Progress: &protocol.AssistantDeltaProgress{
			Type: "assistant_delta", MessageID: "assistant-1", ContentIndex: 0,
			Kind: protocol.DeltaText, Delta: " world",
		},
	}))

	eventually(t, "the projected delta", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(views) == 2
	})
	mu.Lock()
	got := append([]string(nil), views...)
	mu.Unlock()
	if want := []string{"hello", "hello world"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("views = %v, want %v", got, want)
	}

	// The authoritative snapshot is untouched by the projection.
	assistant := session.Snapshot().Transcript[0].(*protocol.AssistantTranscriptItem)
	if body := assistant.Content[0].(*protocol.TextContent).Text; body != "hello" {
		t.Fatalf("snapshot text = %q, want %q", body, "hello")
	}
}

func TestRemoteSessionBecomesUnboundAndReopensAfterRemoval(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	server.send(t, remoteEvent(&protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "session-1"}))
	eventually(t, "the session to go unbound", func() bool {
		return session.State().Lifecycle == RemoteSessionLifecycle{Status: LifecycleUnbound}
	})

	if id := session.ID(); id != "" {
		t.Fatalf("id = %q, want empty", id)
	}
	if snapshot := session.Snapshot(); snapshot != nil {
		t.Fatalf("snapshot = %+v, want nil", snapshot)
	}
	if items := session.State().Transcript; len(items) != 0 {
		t.Fatalf("transcript = %v, want empty", items)
	}

	from := len(server.all())
	reopening := goCall(func() error { return session.Open(remoteContext(t), "session-1") })
	request, _ := server.awaitCommand(t, "attach", from)
	if want := (&protocol.AttachCommand{Command: "attach", SessionID: "session-1"}); !reflect.DeepEqual(request.Request, want) {
		t.Fatalf("request = %+v, want %+v", request.Request, want)
	}
	server.send(t, remoteOK(request.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-1", withRevision(2)),
	}))
	if err := awaitErr(t, reopening); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionExposesTheActiveOperationWhilePrompting(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})
	states := subscribeRecorder(t, session)

	from := len(server.all())
	prompting := goCall(func() error { return session.Submit(remoteContext(t), "  first prompt  ") })
	request, _ := server.awaitCommand(t, "prompt", from)
	want := &protocol.PromptCommand{Command: "prompt", SessionID: "session-1", Text: "first prompt"}
	if !reflect.DeepEqual(request.Request, want) {
		t.Fatalf("request = %+v, want %+v", request.Request, want)
	}
	if operation, busy := session.Operation(); !busy || operation != OperationSubmit {
		t.Fatalf("operation = %q busy=%v, want submit", operation, busy)
	}

	server.send(t, remoteOK(request.ID, &protocol.SessionResult{
		Command: "prompt",
		Session: *remoteSnapshot("session-1", withRevision(2), withPhase(protocol.PhaseTurn)),
	}))
	if err := awaitErr(t, prompting); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	wantStates := []string{"ready", "busy:submit", "busy:submit", "ready"}
	if got := states.lifecycles(); !reflect.DeepEqual(got, wantStates) {
		t.Fatalf("lifecycles = %v, want %v", got, wantStates)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionSteersWhileTheServerSessionIsInATurn(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1", withPhase(protocol.PhaseTurn)),
		RemoteSessionOptions{})

	from := len(server.all())
	steering := goCall(func() error { return session.Submit(remoteContext(t), "adjust") })
	request, _ := server.awaitCommand(t, "steer", from)
	want := &protocol.SteerCommand{Command: "steer", SessionID: "session-1", Text: "adjust"}
	if !reflect.DeepEqual(request.Request, want) {
		t.Fatalf("request = %+v, want %+v", request.Request, want)
	}
	server.send(t, remoteOK(request.ID, &protocol.SessionResult{
		Command: "steer",
		Session: *remoteSnapshot("session-1", withRevision(2), withPhase(protocol.PhaseTurn)),
	}))
	if err := awaitErr(t, steering); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

func TestRemoteSessionSubmitDropsBlankText(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	before := len(server.all())
	if err := session.Submit(remoteContext(t), " \t\n"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := len(server.all()); got != before {
		t.Fatalf("blank input sent %d requests, want none", got-before)
	}
}

func TestRemoteSessionSubmitRefusesUnacceptablePhases(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1", withPhase(protocol.PhaseCompaction)),
		RemoteSessionOptions{})

	err := session.Submit(remoteContext(t), "hello")
	if err == nil || err.Error() != "Session cannot accept input during compaction phase" {
		t.Fatalf("Submit error = %v", err)
	}
}

func TestRemoteSessionAbortsWhileAPromptResponseIsPending(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	from := len(server.all())
	prompting := goCall(func() error { return session.Submit(remoteContext(t), "hello") })
	promptRequest, next := server.awaitCommand(t, "prompt", from)

	// The server starts the turn before answering the prompt, so the abort has
	// a live turn to stop.
	server.send(t, remoteEvent(&protocol.SessionSnapshotEvent{
		Type:     "session_snapshot",
		Snapshot: *remoteSnapshot("session-1", withRevision(2), withPhase(protocol.PhaseTurn)),
	}))
	eventually(t, "the turn to start", func() bool {
		phase, known := session.Phase()
		return known && phase == protocol.PhaseTurn
	})

	aborting := goCall(func() error { return session.Abort(remoteContext(t)) })
	abortRequest, _ := server.awaitCommand(t, "abort", next+1)
	if want := (&protocol.AbortCommand{Command: "abort", SessionID: "session-1"}); !reflect.DeepEqual(abortRequest.Request, want) {
		t.Fatalf("request = %+v, want %+v", abortRequest.Request, want)
	}
	if operation, busy := session.Operation(); !busy || operation != OperationAbort {
		t.Fatalf("operation = %q busy=%v, want abort", operation, busy)
	}

	server.send(t, remoteOK(promptRequest.ID, &protocol.SessionResult{
		Command: "prompt",
		Session: *remoteSnapshot("session-1", withRevision(3), withPhase(protocol.PhaseTurn)),
	}))
	if err := awaitErr(t, prompting); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// The preempted submit must not steal the lifecycle back from the abort.
	if operation, busy := session.Operation(); !busy || operation != OperationAbort {
		t.Fatalf("operation after the prompt settled = %q busy=%v, want abort", operation, busy)
	}

	server.send(t, remoteOK(abortRequest.ID, &protocol.SessionResult{
		Command: "abort", Session: *remoteSnapshot("session-1", withRevision(4)),
	}))
	if err := awaitErr(t, aborting); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionAbortRestoresThePreemptedSubmit(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1", withPhase(protocol.PhaseTurn)),
		RemoteSessionOptions{})

	from := len(server.all())
	steering := goCall(func() error { return session.Submit(remoteContext(t), "adjust") })
	steerRequest, next := server.awaitCommand(t, "steer", from)

	aborting := goCall(func() error { return session.Abort(remoteContext(t)) })
	abortRequest, _ := server.awaitCommand(t, "abort", next+1)

	// The abort settles first, so the submit it interrupted gets its lifecycle
	// back instead of the session claiming to be ready mid-turn.
	server.send(t, remoteOK(abortRequest.ID, &protocol.SessionResult{
		Command: "abort", Session: *remoteSnapshot("session-1", withRevision(2), withPhase(protocol.PhaseTurn)),
	}))
	if err := awaitErr(t, aborting); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if operation, busy := session.Operation(); !busy || operation != OperationSubmit {
		t.Fatalf("operation = %q busy=%v, want submit restored", operation, busy)
	}

	server.send(t, remoteOK(steerRequest.ID, &protocol.SessionResult{
		Command: "steer", Session: *remoteSnapshot("session-1", withRevision(3), withPhase(protocol.PhaseTurn)),
	}))
	if err := awaitErr(t, steering); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionAbortIsANoOpWhileIdle(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	before := len(server.all())
	if err := session.Abort(remoteContext(t)); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := len(server.all()); got != before {
		t.Fatalf("aborting an idle session sent %d requests, want none", got-before)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionRejectsConflictingOperationsWhileBusy(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	from := len(server.all())
	prompting := goCall(func() error { return session.Submit(remoteContext(t), "hello") })
	request, _ := server.awaitCommand(t, "prompt", from)

	for _, conflicting := range []struct {
		name string
		run  func() error
	}{
		{"setThinking", func() error { return session.SetThinking(remoteContext(t), protocol.ThinkingHigh) }},
		{"setModel", func() error {
			return session.SetModel(remoteContext(t), protocol.ModelRef{Provider: "faux", ID: "model"})
		}},
		{"open", func() error { return session.Open(remoteContext(t), "session-2") }},
		{"create", func() error { return session.Create(remoteContext(t), CreateRemoteSessionOptions{Cwd: "/other"}) }},
		{"reconnect", func() error { return session.Reconnect(remoteContext(t)) }},
	} {
		t.Run(conflicting.name, func(t *testing.T) {
			err := conflicting.run()
			var busy *RemoteSessionBusyError
			if !errors.As(err, &busy) {
				t.Fatalf("error = %v, want a busy error", err)
			}
			if err.Error() != "Remote session is busy with submit" {
				t.Fatalf("message = %q", err.Error())
			}
		})
	}
	if got := server.commands()[from:]; !reflect.DeepEqual(got, []string{"prompt"}) {
		t.Fatalf("commands = %v, want only the prompt", got)
	}

	server.send(t, remoteOK(request.ID, &protocol.SessionResult{
		Command: "prompt", Session: *remoteSnapshot("session-1", withRevision(2), withPhase(protocol.PhaseTurn)),
	}))
	if err := awaitErr(t, prompting); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

func TestRemoteSessionRefusesReconfigurationOutsideIdle(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1", withPhase(protocol.PhaseTurn)),
		RemoteSessionOptions{})

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"setModel", func() error {
			return session.SetModel(remoteContext(t), protocol.ModelRef{Provider: "faux", ID: "model"})
		}, "Cannot change model while session is turn"},
		{"setThinking", func() error {
			return session.SetThinking(remoteContext(t), protocol.ThinkingHigh)
		}, "Cannot change thinking level while session is turn"},
		{"open", func() error {
			return session.Open(remoteContext(t), "session-2")
		}, "Cannot open a session while session is turn"},
		{"create", func() error {
			return session.Create(remoteContext(t), CreateRemoteSessionOptions{Cwd: "/other"})
		}, "Cannot create a session while session is turn"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.run()
			if err == nil || err.Error() != testCase.want {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestRemoteSessionReportsSubscriberFailures(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	var (
		mu     sync.Mutex
		raised []string
	)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{
		OnListenerError: func(err error) {
			mu.Lock()
			raised = append(raised, err.Error())
			mu.Unlock()
		},
	})

	if _, err := session.Subscribe(func(RemoteSessionState) { panic(errors.New("render failed")) }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	notified := false
	if _, err := session.Subscribe(func(RemoteSessionState) { notified = true }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), raised...)
	mu.Unlock()
	if want := []string{"render failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listener errors = %v, want %v", got, want)
	}
	if !notified {
		t.Fatal("a failing subscriber must not stop the ones behind it")
	}
}

func TestRemoteSessionUnsubscribeStopsNotifications(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	states := &recorder{}
	unsubscribe, err := session.Subscribe(states.record)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	unsubscribe()
	unsubscribe() // repeated calls are harmless

	server.send(t, remoteEvent(&protocol.SessionSnapshotEvent{
		Type:     "session_snapshot",
		Snapshot: *remoteSnapshot("session-1", withRevision(2)),
	}))
	eventually(t, "the snapshot to land", func() bool { return session.Snapshot().Revision == 2 })
	if got := states.len(); got != 1 {
		t.Fatalf("recorded %d states, want only the immediate one", got)
	}
}

func TestRemoteSessionOpenIsANoOpWhenAlreadyReady(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	before := len(server.all())
	if err := session.Open(remoteContext(t), "session-1"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := len(server.all()); got != before {
		t.Fatalf("reopening the ready session sent %d requests, want none", got-before)
	}
}

func TestRemoteSessionOpensAReplacementBeforeDetaching(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	from := len(server.all())
	opening := goCall(func() error { return session.Open(remoteContext(t), "session-2") })
	attachRequest, next := server.awaitCommand(t, "attach", from)
	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-2"),
	}))

	detachRequest, _ := server.awaitCommand(t, "detach", next+1)
	want := &protocol.DetachCommand{Command: "detach", SessionID: "session-1"}
	if !reflect.DeepEqual(detachRequest.Request, want) {
		t.Fatalf("request = %+v, want %+v", detachRequest.Request, want)
	}
	server.send(t, remoteOK(detachRequest.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))
	if err := awaitErr(t, opening); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := session.ID(); got != "session-2" {
		t.Fatalf("id = %q, want session-2", got)
	}
}

func TestRemoteSessionRejectsMutationsWhileAReplacementIsPending(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	from := len(server.all())
	opening := goCall(func() error { return session.Open(remoteContext(t), "session-2") })
	attachRequest, next := server.awaitCommand(t, "attach", from)

	for _, run := range []func() error{
		func() error { return session.Submit(remoteContext(t), "race") },
		func() error { return session.Create(remoteContext(t), CreateRemoteSessionOptions{Cwd: "/other"}) },
	} {
		if err := run(); err == nil || err.Error() != "Remote session is busy with open" {
			t.Fatalf("error = %v, want the open to be reported as busy", err)
		}
	}
	if got := server.commands()[from:]; !reflect.DeepEqual(got, []string{"attach"}) {
		t.Fatalf("commands = %v, want only the attach", got)
	}

	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-2"),
	}))
	detachRequest, _ := server.awaitCommand(t, "detach", next+1)
	server.send(t, remoteOK(detachRequest.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))
	if err := awaitErr(t, opening); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

func TestRemoteSessionRollsBackAReplacementWhenTheCurrentSessionBecomesActive(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	from := len(server.all())
	opening := goCall(func() error { return session.Open(remoteContext(t), "session-2") })
	attachRequest, next := server.awaitCommand(t, "attach", from)

	// The session this coordinator is walking away from starts a turn before the
	// replacement lands, so the replacement is the one that has to go.
	server.send(t, remoteEvent(&protocol.SessionSnapshotEvent{
		Type:     "session_snapshot",
		Snapshot: *remoteSnapshot("session-1", withPhase(protocol.PhaseTurn), withRevision(2)),
	}))
	eventually(t, "the turn to start", func() bool {
		phase, known := session.Phase()
		return known && phase == protocol.PhaseTurn
	})
	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-2"),
	}))

	detachRequest, _ := server.awaitCommand(t, "detach", next+1)
	want := &protocol.DetachCommand{Command: "detach", SessionID: "session-2"}
	if !reflect.DeepEqual(detachRequest.Request, want) {
		t.Fatalf("request = %+v, want the replacement to be given up", detachRequest.Request)
	}
	server.send(t, remoteOK(detachRequest.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-2"}))

	err := awaitErr(t, opening)
	if err == nil || err.Error() != "Cannot open a session while session is turn" {
		t.Fatalf("error = %v", err)
	}
	if got := session.ID(); got != "session-1" {
		t.Fatalf("id = %q, want session-1 to survive", got)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionCloseAwaitsAttachmentCleanupStartedByReconnect(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	c.Disconnect("test reconnect")
	from := len(server.all())
	reconnecting := goCall(func() error { return session.Reconnect(remoteContext(t)) })
	attachRequest, next := server.awaitCommand(t, "attach", from)

	closing := goCall(func() error { return session.Close(remoteContext(t)) })
	if err := awaitErr(t, reconnecting); !errors.Is(err, ErrRemoteSessionDisposed) {
		t.Fatalf("Reconnect error = %v, want the disposal", err)
	}

	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-1", withRevision(2)),
	}))
	detachRequest, _ := server.awaitCommand(t, "detach", next+1)
	select {
	case err := <-closing:
		t.Fatalf("Close settled before the attachment was released: %v", err)
	default:
	}

	server.send(t, remoteOK(detachRequest.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))
	if err := awaitErr(t, closing); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !c.Connected() {
		t.Fatal("the borrowed client must stay connected")
	}
}

func TestRemoteSessionCloseImmediatelyPreemptsPendingWork(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})
	states := subscribeRecorder(t, session)

	from := len(server.all())
	opening := goCall(func() error { return session.Open(remoteContext(t), "session-2") })
	attachRequest, next := server.awaitCommand(t, "attach", from)

	closing := goCall(func() error { return session.Close(remoteContext(t)) })
	currentDetach, _ := server.awaitCommand(t, "detach", next+1)
	if want := (&protocol.DetachCommand{Command: "detach", SessionID: "session-1"}); !reflect.DeepEqual(currentDetach.Request, want) {
		t.Fatalf("request = %+v, want the current attachment released", currentDetach.Request)
	}
	if !c.Connected() {
		t.Fatal("the borrowed client must stay connected")
	}
	eventually(t, "the disposed lifecycle", func() bool {
		return session.State().Lifecycle == RemoteSessionLifecycle{Status: LifecycleDisposed}
	})
	if err := awaitErr(t, opening); !errors.Is(err, ErrRemoteSessionDisposed) {
		t.Fatalf("Open error = %v, want the disposal", err)
	}

	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-2"),
	}))
	server.send(t, remoteOK(currentDetach.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))

	replacementDetach, _ := server.awaitCommand(t, "detach", currentDetach2Index(t, server, currentDetach))
	if want := (&protocol.DetachCommand{Command: "detach", SessionID: "session-2"}); !reflect.DeepEqual(replacementDetach.Request, want) {
		t.Fatalf("request = %+v, want the replacement released", replacementDetach.Request)
	}
	server.send(t, remoteOK(replacementDetach.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-2"}))
	if err := awaitErr(t, closing); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := states.lifecycles(); got[len(got)-1] != "disposed" {
		t.Fatalf("lifecycles = %v, want disposal to be announced", got)
	}
	if _, err := session.Subscribe(func(RemoteSessionState) {}); !errors.Is(err, ErrRemoteSessionDisposed) {
		t.Fatalf("Subscribe after disposal = %v, want the disposal", err)
	}
}

// currentDetach2Index finds where to start looking for the replacement's detach:
// everything at or before the current attachment's detach is already accounted
// for.
func currentDetach2Index(t *testing.T, server *remoteServer, current *protocol.RequestEnvelope) int {
	t.Helper()
	for i, request := range server.all() {
		if request == current {
			return i + 1
		}
	}
	t.Fatal("the current detach is not in the recorded requests")
	return 0
}

func TestRemoteSessionCloseIsIdempotentAndAwaitsDetach(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	server.answerAttach(t, func(sessionID string) *protocol.SessionSnapshot { return remoteSnapshot(sessionID) })
	session, err := OpenRemoteSession(remoteContext(t), c, "session-1", RemoteSessionOptions{})
	if err != nil {
		t.Fatalf("OpenRemoteSession: %v", err)
	}

	from := len(server.all())
	defer func() {
		// pi asserts the two calls returned the same promise. Here the equivalent
		// is that the second caller joined the first disposal rather than starting
		// one of its own: one detach, not two.
		detachs := 0
		for _, command := range server.commands()[from:] {
			if command == "detach" {
				detachs++
			}
		}
		if detachs != 1 {
			t.Errorf("issued %d detaches, want the second Close to have joined the first", detachs)
		}
	}()
	first := goCall(func() error { return session.Close(remoteContext(t)) })
	second := goCall(func() error { return session.Close(remoteContext(t)) })
	detachRequest, _ := server.awaitCommand(t, "detach", from)
	if want := (&protocol.DetachCommand{Command: "detach", SessionID: "session-1"}); !reflect.DeepEqual(detachRequest.Request, want) {
		t.Fatalf("request = %+v, want %+v", detachRequest.Request, want)
	}
	select {
	case err := <-first:
		t.Fatalf("Close settled before the detach was answered: %v", err)
	default:
	}

	server.send(t, remoteOK(detachRequest.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))
	if err := awaitErr(t, first); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := awaitErr(t, second); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !session.Disposed() {
		t.Fatal("the session must report itself disposed")
	}
	if !c.Connected() {
		t.Fatal("the borrowed client must stay connected")
	}
}

func TestRemoteSessionRefusesAnExclusiveLeaseWhileASharedOneIsHeld(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	server.answerAttach(t, func(sessionID string) *protocol.SessionSnapshot { return remoteSnapshot(sessionID) })
	server.answerDetach(t)

	direct, err := c.AttachSession(remoteContext(t), "session-1")
	if err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	from := len(server.all())

	session, err := OpenRemoteSession(remoteContext(t), c, "session-1", RemoteSessionOptions{})
	var ownership *client.SessionOwnershipError
	if !errors.As(err, &ownership) {
		t.Fatalf("OpenRemoteSession error = %v, want an ownership error", err)
	}
	if session != nil {
		t.Fatal("a refused coordinator must not be returned")
	}
	if got := server.commands()[from:]; len(got) != 0 {
		t.Fatalf("commands = %v, want none — the refusal is local", got)
	}
	if !direct.Active() {
		t.Fatal("the direct lease must survive the refusal")
	}
	if err := direct.Detach(remoteContext(t)); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if got := server.commands()[from:]; !reflect.DeepEqual(got, []string{"detach"}) {
		t.Fatalf("commands = %v, want only the detach", got)
	}
}

func TestCreateRemoteSession(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	server.onMessage(func(message protocol.ClientMessage) {
		request, ok := message.(*protocol.RequestEnvelope)
		if !ok {
			return
		}
		if _, ok := request.Request.(*protocol.CreateCommand); !ok {
			return
		}
		server.send(t, remoteOK(request.ID, &protocol.SessionResult{
			Command: "create", Session: *remoteSnapshot("session-1"),
		}))
	})

	session, err := CreateRemoteSession(remoteContext(t), c, CreateRemoteSessionOptions{Cwd: "/workspace"},
		RemoteSessionOptions{})
	if err != nil {
		t.Fatalf("CreateRemoteSession: %v", err)
	}
	closeAtCleanup(t, server, session)

	if got := session.ID(); got != "session-1" {
		t.Fatalf("id = %q, want session-1", got)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionCloseReportsCleanupFailureWithoutRetainingOwnership(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	server.answerAttach(t, func(sessionID string) *protocol.SessionSnapshot { return remoteSnapshot(sessionID) })
	session, err := OpenRemoteSession(remoteContext(t), c, "session-1", RemoteSessionOptions{})
	if err != nil {
		t.Fatalf("OpenRemoteSession: %v", err)
	}

	var (
		mu      sync.Mutex
		detachs int
	)
	server.onMessage(func(message protocol.ClientMessage) {
		request, ok := message.(*protocol.RequestEnvelope)
		if !ok {
			return
		}
		detach, ok := request.Request.(*protocol.DetachCommand)
		if !ok {
			return
		}
		mu.Lock()
		detachs++
		first := detachs == 1
		mu.Unlock()
		if first {
			server.send(t, remoteErr(request.ID, protocol.ErrorInvalidRequest, "no"))
			return
		}
		server.send(t, remoteOK(request.ID, &protocol.DetachResult{Command: "detach", SessionID: detach.SessionID}))
	})

	if err := session.Close(remoteContext(t)); err == nil || err.Error() != "no" {
		t.Fatalf("Close error = %v, want the server's refusal", err)
	}
	if !session.Disposed() {
		t.Fatal("a failed cleanup must still dispose the session")
	}
	if !c.Connected() {
		t.Fatal("the borrowed client must stay connected")
	}

	// The exclusive lease was given up despite the failure, so the session can
	// be taken over again — after the owed detach is paid off.
	replacement, err := OpenRemoteSession(remoteContext(t), c, "session-1", RemoteSessionOptions{})
	if err != nil {
		t.Fatalf("OpenRemoteSession: %v", err)
	}
	// This test answers detaches itself, so closeAtCleanup's responder would
	// answer the cleanup's detach twice and disconnect the client. Bound the
	// context instead, for the same reason closeAtCleanup does.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), remoteTestTimeout)
		defer cancel()
		_ = replacement.Close(ctx)
	})
	if got := replacement.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
	mu.Lock()
	got := detachs
	mu.Unlock()
	if got != 2 {
		t.Fatalf("detach count = %d, want 2", got)
	}
}

func TestRemoteSessionsBorrowOneClientIndependently(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	server.answerAttach(t, func(sessionID string) *protocol.SessionSnapshot { return remoteSnapshot(sessionID) })
	server.answerDetach(t)

	first, err := OpenRemoteSession(remoteContext(t), c, "session-1", RemoteSessionOptions{})
	if err != nil {
		t.Fatalf("OpenRemoteSession: %v", err)
	}
	second, err := OpenRemoteSession(remoteContext(t), c, "session-2", RemoteSessionOptions{})
	if err != nil {
		t.Fatalf("OpenRemoteSession: %v", err)
	}
	if err := first.Close(remoteContext(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !first.Disposed() {
		t.Fatal("the closed session must report itself disposed")
	}
	if second.Disposed() {
		t.Fatal("closing one session must not close the other")
	}
	if !c.Connected() {
		t.Fatal("the borrowed client must stay connected")
	}
	if err := second.Close(remoteContext(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRemoteSessionCloseTreatsAClientFirstDisposalAsReleased(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	server.answerAttach(t, func(sessionID string) *protocol.SessionSnapshot { return remoteSnapshot(sessionID) })
	session, err := OpenRemoteSession(remoteContext(t), c, "session-1", RemoteSessionOptions{})
	if err != nil {
		t.Fatalf("OpenRemoteSession: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("client Close: %v", err)
	}
	from := len(server.all())
	if err := session.Close(remoteContext(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !session.Disposed() {
		t.Fatal("the session must report itself disposed")
	}
	if got := server.commands()[from:]; len(got) != 0 {
		t.Fatalf("commands = %v, want none — the client already released everything", got)
	}
}

func TestRemoteSessionRefusesWorkAfterClose(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	server.answerAttach(t, func(sessionID string) *protocol.SessionSnapshot { return remoteSnapshot(sessionID) })
	server.answerDetach(t)
	session, err := OpenRemoteSession(remoteContext(t), c, "session-1", RemoteSessionOptions{})
	if err != nil {
		t.Fatalf("OpenRemoteSession: %v", err)
	}
	if err := session.Close(remoteContext(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cases := []struct {
		name string
		run  func() error
	}{
		{"open", func() error { return session.Open(remoteContext(t), "session-2") }},
		{"create", func() error { return session.Create(remoteContext(t), CreateRemoteSessionOptions{Cwd: "/x"}) }},
		{"submit", func() error { return session.Submit(remoteContext(t), "hello") }},
		{"abort", func() error { return session.Abort(remoteContext(t)) }},
		{"setModel", func() error {
			return session.SetModel(remoteContext(t), protocol.ModelRef{Provider: "faux", ID: "model"})
		}},
		{"setThinking", func() error { return session.SetThinking(remoteContext(t), protocol.ThinkingHigh) }},
		{"reconnect", func() error { return session.Reconnect(remoteContext(t)) }},
		{"subscribe", func() error { _, err := session.Subscribe(func(RemoteSessionState) {}); return err }},
		{"onConnectionStateChange", func() error {
			_, err := session.OnConnectionStateChange(func(client.ConnectionStateChange) {})
			return err
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.run(); !errors.Is(err, ErrRemoteSessionDisposed) {
				t.Fatalf("error = %v, want the disposal", err)
			}
		})
	}
}

func TestRemoteSessionRequiresAnAttachedSession(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	server.send(t, remoteEvent(&protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "session-1"}))
	eventually(t, "the session to go unbound", func() bool {
		return session.State().Lifecycle == RemoteSessionLifecycle{Status: LifecycleUnbound}
	})

	cases := []struct {
		name string
		run  func() error
	}{
		{"submit", func() error { return session.Submit(remoteContext(t), "hello") }},
		{"abort", func() error { return session.Abort(remoteContext(t)) }},
		{"setModel", func() error {
			return session.SetModel(remoteContext(t), protocol.ModelRef{Provider: "faux", ID: "model"})
		}},
		{"setThinking", func() error { return session.SetThinking(remoteContext(t), protocol.ThinkingHigh) }},
		{"reconnect", func() error { return session.Reconnect(remoteContext(t)) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.run(); !errors.Is(err, ErrNoRemoteSession) {
				t.Fatalf("error = %v, want %v", err, ErrNoRemoteSession)
			}
		})
	}
}

func TestRemoteSessionIgnoresASupersededBindingsSubscriptions(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	// The binding session-1's subscriptions were registered under. Replacing the
	// attachment retires it, and everything registered under it has to go quiet:
	// bind installs the new state before it can unregister the old listeners, so
	// a straggler delivered in that window would otherwise unbind a handle it
	// knows nothing about, or fold the wrong session's snapshot into the
	// projection.
	session.mu.Lock()
	superseded := session.binding
	session.mu.Unlock()

	from := len(server.all())
	opening := goCall(func() error { return session.Open(remoteContext(t), "session-2") })
	attachRequest, next := server.awaitCommand(t, "attach", from)
	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-2"),
	}))
	detachRequest, _ := server.awaitCommand(t, "detach", next+1)
	server.send(t, remoteOK(detachRequest.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))
	if err := awaitErr(t, opening); err != nil {
		t.Fatalf("Open: %v", err)
	}

	session.handleEvent(superseded, &protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "session-1"})
	if got := session.ID(); got != "session-2" {
		t.Fatalf("id = %q, want session-2 — a retired subscription unbound the attachment that replaced it", got)
	}
	session.applySnapshot(superseded, remoteSnapshot("session-1", withRevision(9)))
	if got := session.Snapshot().ID; got != "session-2" {
		t.Fatalf("snapshot = %q, want session-2's — a retired subscription rewrote the projection", got)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionCloseRefusesAnAttachmentItCannotAwait(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})
	server.answerDetach(t)

	// An operation that has passed its disposal check but has not registered its
	// attachment yet is the one window Close cannot see into. Rather than race
	// for it, the window is opened here and disposal run to completion inside it:
	// what matters is that the acquisition on the far side never starts, because
	// Close has already promised nobody is holding this session's lease.
	started, err := session.start(OperationOpen, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := session.Close(remoteContext(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}

	acquired := false
	attachment, err := session.beginAttachment()
	if err == nil {
		err = session.runAttachment(remoteContext(t), attachment, func(context.Context) error {
			acquired = true
			return nil
		})
	}
	session.end(started)

	if acquired {
		t.Fatal("an attachment was acquired after Close returned: the server keeps a lease nobody can release")
	}
	if !errors.Is(err, errDisposedAfterAwait) {
		t.Fatalf("attachment error = %v, want the disposal to have refused it", err)
	}
}

func TestRemoteSessionBindsTheReplacementBeforeItSubscribes(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	// The replacement's lease dies between the detach that gave session-1 up and
	// the subscriptions bind registers, so the client refuses them. pi has
	// swapped the handle and the transcript by that point, and the session has to
	// end up on the attachment it took over rather than on the one it just
	// detached.
	server.onMessage(func(message protocol.ClientMessage) {
		request, ok := message.(*protocol.RequestEnvelope)
		if !ok {
			return
		}
		detach, ok := request.Request.(*protocol.DetachCommand)
		if !ok || detach.SessionID != "session-1" {
			return
		}
		server.send(t, remoteOK(request.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))
		server.send(t, remoteEvent(&protocol.SessionRemovedEvent{Type: "session_removed", SessionID: "session-2"}))
	})

	from := len(server.all())
	opening := goCall(func() error { return session.Open(remoteContext(t), "session-2") })
	attachRequest, _ := server.awaitCommand(t, "attach", from)
	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-2"),
	}))

	var detached *client.SessionDetachedError
	if err := awaitErr(t, opening); !errors.As(err, &detached) {
		t.Fatalf("Open error = %v, want the refused subscription", err)
	}
	if got := session.ID(); got != "session-2" {
		t.Fatalf("id = %q, want session-2 — the session kept an attachment it had already detached", got)
	}
	if got := session.Snapshot().ID; got != "session-2" {
		t.Fatalf("snapshot = %q, want session-2's", got)
	}
	if got := session.State().Lifecycle; got != (RemoteSessionLifecycle{Status: LifecycleReady}) {
		t.Fatalf("lifecycle = %+v, want ready", got)
	}
}

func TestRemoteSessionCloseDoesNotLatchItsFirstCallersDeadline(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	c.Disconnect("test reconnect")
	from := len(server.all())
	reconnecting := goCall(func() error { return session.Reconnect(remoteContext(t)) })
	attachRequest, next := server.awaitCommand(t, "attach", from)

	// The first caller runs out of budget while the reacquired attachment is
	// still being given up. That budget is its own: it says how long this caller
	// waits, not what the disposal achieved.
	impatient, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := session.Close(impatient); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close = %v, want its own deadline", err)
	}
	if err := awaitErr(t, reconnecting); !errors.Is(err, ErrRemoteSessionDisposed) {
		t.Fatalf("Reconnect = %v, want the disposal", err)
	}

	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-1", withRevision(2)),
	}))
	detachRequest, _ := server.awaitCommand(t, "detach", next+1)
	server.send(t, remoteOK(detachRequest.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))

	if err := session.Close(remoteContext(t)); err != nil {
		t.Fatalf("second Close = %v, want the disposal that actually happened", err)
	}
}

func TestRemoteSessionCloseReportsOneFailurePerFailedCleanup(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	from := len(server.all())
	opening := goCall(func() error { return session.Open(remoteContext(t), "session-2") })
	attachRequest, next := server.awaitCommand(t, "attach", from)

	// Neither the replacement nor the detach that pays for it is answered, so the
	// caller's budget expires with two cleanups outstanding. One expired budget is
	// one failure, however many things were waiting on it.
	impatient, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := session.Close(impatient)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want its own deadline", err)
	}
	var aggregate *aggregateError
	if errors.As(err, &aggregate) {
		t.Fatalf("Close = %v, want one failure rather than the same deadline collected per cleanup", err)
	}
	if err := awaitErr(t, opening); !errors.Is(err, ErrRemoteSessionDisposed) {
		t.Fatalf("Open = %v, want the disposal", err)
	}

	// The cleanup runs on: the replacement lands, is given up unused, and the
	// detach the session owed is answered too late to have been waited for.
	currentDetach, _ := server.awaitCommand(t, "detach", next+1)
	server.send(t, remoteOK(attachRequest.ID, &protocol.SessionResult{
		Command: "attach", Session: *remoteSnapshot("session-2"),
	}))
	replacementDetach, _ := server.awaitCommand(t, "detach", currentDetach2Index(t, server, currentDetach))
	server.send(t, remoteOK(replacementDetach.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-2"}))
	server.send(t, remoteOK(currentDetach.ID, &protocol.DetachResult{Command: "detach", SessionID: "session-1"}))

	// A cleanup that genuinely failed stays reported, unlike a caller's deadline.
	if err := session.Close(remoteContext(t)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Close = %v, want the detach that ran out of budget", err)
	}
}

func TestRemoteSessionNotifiesInOrder(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	// One subscriber is slow to render the second revision. State changes behind
	// it must queue up rather than overtake it: a subscriber that renders
	// whatever it was handed last would otherwise be left showing a transcript
	// the session has moved past, with no further event to correct it.
	const slowRender = 100 * time.Millisecond
	var (
		mu        sync.Mutex
		delivered []int64
		rendering sync.Once
	)
	reached := make(chan struct{})
	if _, err := session.Subscribe(func(state RemoteSessionState) {
		revision := int64(0)
		if state.Snapshot != nil {
			revision = state.Snapshot.Revision
		}
		if revision == 2 {
			rendering.Do(func() {
				close(reached)
				time.Sleep(slowRender)
			})
		}
		mu.Lock()
		delivered = append(delivered, revision)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	sending := goCall(func() error {
		server.send(t, remoteEvent(&protocol.SessionSnapshotEvent{
			Type: "session_snapshot", Snapshot: *remoteSnapshot("session-1", withRevision(2)),
		}))
		return nil
	})
	<-reached
	server.send(t, remoteEvent(&protocol.SessionSnapshotEvent{
		Type: "session_snapshot", Snapshot: *remoteSnapshot("session-1", withRevision(3)),
	}))
	if err := awaitErr(t, sending); err != nil {
		t.Fatalf("send: %v", err)
	}

	mu.Lock()
	got := append([]int64(nil), delivered...)
	mu.Unlock()
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("revisions delivered = %v, want them non-decreasing", got)
		}
	}
	if len(got) == 0 || got[len(got)-1] != 3 {
		t.Fatalf("revisions delivered = %v, want the newest one last", got)
	}
	if revision := session.Snapshot().Revision; revision != got[len(got)-1] {
		t.Fatalf("session holds revision %d, subscribers were left on %d", revision, got[len(got)-1])
	}
}

func TestRemoteSessionNotificationSkipsAPeerUnsubscribedDuringIt(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	// pi notifies by iterating the listener set itself, so a subscriber that
	// unsubscribes a peer the notification has not reached yet stops that peer
	// from hearing about a state it just said it no longer wants.
	var (
		mu       sync.Mutex
		peer     client.Unsubscribe
		notified int
	)
	if _, err := session.Subscribe(func(RemoteSessionState) {
		mu.Lock()
		unsubscribe := peer
		mu.Unlock()
		if unsubscribe != nil {
			unsubscribe()
		}
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	unsubscribe, err := session.Subscribe(func(RemoteSessionState) {
		mu.Lock()
		notified++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	mu.Lock()
	peer = unsubscribe
	mu.Unlock()

	server.send(t, remoteEvent(&protocol.SessionSnapshotEvent{
		Type: "session_snapshot", Snapshot: *remoteSnapshot("session-1", withRevision(2)),
	}))
	eventually(t, "the snapshot to land", func() bool { return session.Snapshot().Revision == 2 })

	mu.Lock()
	got := notified
	mu.Unlock()
	if got != 1 {
		t.Fatalf("the peer was notified %d times, want only its own immediate call", got)
	}
}

func TestRemoteSessionModelsAndSessionsAreEmptyBeforeAServerSnapshot(t *testing.T) {
	server := newRemoteServer(t)
	c, err := client.New(client.Options{TransportFactory: server.connect})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	session := newRemoteSession(c, RemoteSessionOptions{})

	if got := session.Models(); got == nil {
		t.Fatal("models = nil, want the empty view pi reports")
	}
	if got := session.Sessions(); got == nil {
		t.Fatal("sessions = nil, want the empty view pi reports")
	}
}

func TestRemoteSessionExposesTheBorrowedClientsView(t *testing.T) {
	server := newRemoteServer(t)
	c := connectRemoteClient(t, server)
	session := openTestSession(t, c, server, remoteSnapshot("session-1"), RemoteSessionOptions{})

	if got := session.ConnectionState(); got != client.Connected {
		t.Fatalf("connection state = %q, want connected", got)
	}
	if got := session.Models(); len(got) != 0 {
		t.Fatalf("models = %v, want none", got)
	}
	if got := session.Sessions(); len(got) != 0 {
		t.Fatalf("sessions = %v, want none", got)
	}

	changes := make(chan client.ConnectionStateChange, 4)
	unsubscribe, err := session.OnConnectionStateChange(func(change client.ConnectionStateChange) {
		select {
		case changes <- change:
		default:
		}
	})
	if err != nil {
		t.Fatalf("OnConnectionStateChange: %v", err)
	}
	defer unsubscribe()

	c.Disconnect("bye")
	select {
	case change := <-changes:
		if change.State != client.Disconnected {
			t.Fatalf("state = %q, want disconnected", change.State)
		}
	case <-remoteContext(t).Done():
		t.Fatal("no connection-state change was delivered")
	}
}
