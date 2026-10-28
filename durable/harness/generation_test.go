package harness

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-generation.test.ts.

// liveView is the part of pi.live the generation tests read.
type liveView struct {
	Run        *LiveRun        `json:"run,omitempty"`
	Generation *LiveGeneration `json:"generation,omitempty"`
}

func decodeLive(t *testing.T, value map[string]any) liveView {
	t.Helper()
	var view liveView
	must(t, fromValue(value, &view))
	return view
}

// liveOf is a conversation's pi.live; empty when absent.
func liveOf(t *testing.T, h *Harness, conversation *Conversation) map[string]any {
	t.Helper()
	value, err := h.Snapshot(bg, LiveDoc.Conversation(conversation.ID))
	must(t, err)
	return value
}

// expectIdleLive fails unless pi.live is {} (or absent).
func expectIdleLive(t *testing.T, h *Harness, conversation *Conversation) {
	t.Helper()
	if value := liveOf(t, h, conversation); len(value) != 0 {
		t.Fatalf("pi.live = %v, want {}", value)
	}
}

// runTask waits for the conversation's run and returns its task.
func runTask(t *testing.T, h *Harness, conversation *Conversation) durable.TaskID {
	t.Helper()
	var id durable.TaskID
	eventually(t, func() bool {
		run := decodeLive(t, liveOf(t, h, conversation)).Run
		if run == nil {
			return false
		}
		id = run.TaskID
		return true
	})
	return id
}

// livePublications collects every committed pi.live value of h.
type livePublications struct {
	mu     sync.Mutex
	values []map[string]any
}

func watchLive(t *testing.T, h *Harness) *livePublications {
	t.Helper()
	p := &livePublications{}
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind && document.Value != nil {
				p.mu.Lock()
				p.values = append(p.values, document.Value)
				p.mu.Unlock()
			}
		}
	})
	must(t, err)
	t.Cleanup(unsubscribe)
	return p
}

func (p *livePublications) generations(t *testing.T) []LiveGeneration {
	t.Helper()
	p.mu.Lock()
	values := slices.Clone(p.values)
	p.mu.Unlock()
	var out []LiveGeneration
	for _, value := range values {
		if generation := decodeLive(t, value).Generation; generation != nil {
			out = append(out, *generation)
		}
	}
	return out
}

// streamModels replaces StreamSimple of the models it wraps.
type streamModels struct {
	ai.Models
	stream func() *ai.AssistantMessageEventStream
}

func (m streamModels) StreamSimple(context.Context, *ai.Model, ai.Context, *ai.ModelsSimpleStreamOptions) *ai.AssistantMessageEventStream {
	return m.stream()
}

// scriptedStream pushes one start event with partial, waits, and ends with
// final.
func scriptedStream(partial, final *ai.AssistantMessage, wait time.Duration) func() *ai.AssistantMessageEventStream {
	return func() *ai.AssistantMessageEventStream {
		stream := ai.NewAssistantMessageEventStream()
		go func() {
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: partial})
			time.Sleep(wait)
			stream.End(final)
		}()
		return stream
	}
}

// cancelFailingModels fails every CancelDeferred.
type cancelFailingModels struct{ ai.Models }

func (cancelFailingModels) CancelDeferred(context.Context, *ai.Model, ai.DeferredHandle, *ai.ModelsDeferredCancelOptions) error {
	return errors.New("cancel failed")
}

func retrySettings(enabled bool, maxRetries int, baseDelay time.Duration) func(*HarnessSettings) {
	return func(s *HarnessSettings) {
		s.Retry.Enabled = ptr(enabled)
		s.Retry.MaxRetries = ptr(maxRetries)
		s.Retry.BaseDelay = ptr(baseDelay)
	}
}

func fauxStep(message *ai.AssistantMessage) providers.FauxResponseStep {
	return providers.FauxStatic(message)
}

func entryKinds(entries []durable.EntryRecord) []string {
	return names(entries, func(entry durable.EntryRecord) string { return entry.Kind })
}

func expectKinds(t *testing.T, conversation *Conversation, want ...string) []durable.EntryRecord {
	t.Helper()
	entries := chatEntries(t, conversation)
	if got := entryKinds(entries); !slices.Equal(got, want) {
		t.Fatalf("entry kinds = %v, want %v", got, want)
	}
	return entries
}

func entryAssistant(t *testing.T, entry durable.EntryRecord) ai.AssistantMessage {
	t.Helper()
	if len(entry.Model) == 0 {
		t.Fatalf("entry %d has no model message", entry.ID)
	}
	message, ok := entry.Model[0].(ai.AssistantMessage)
	if !ok {
		t.Fatalf("entry %d holds %T, want an assistant message", entry.ID, entry.Model[0])
	}
	return message
}

func expectSettled(t *testing.T, settled durable.SubmissionRecord, status, reason string) {
	t.Helper()
	if settled.Status != status || settled.Reason != reason {
		t.Fatalf("settled = %s/%q, want %s/%q (%+v)", settled.Status, settled.Reason, status, reason, settled)
	}
}

func TestGenerationRetriesARetryableErrorAfterADurableBackoffAndThenAnswers(t *testing.T) {
	setup := newChatSetup(t)
	addSection(t, setup.registry, &PromptSection{Key: "preamble", Untagged: true, Render: constSection("preamble", "p").Render})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(fauxError("503 Service Unavailable")), fauxText("recovered")})
	setup.setSettings(retrySettings(true, 3, time.Millisecond))
	h, root := openChat(t, memory.New(), setup)
	values := watchLive(t, h)
	h.Resume()
	settled := waitFor(t, submit(t, root, inputDraft("hi")))
	if settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	entries := expectKinds(t, root, "pi.user", "pi.system", "pi.assistant", "pi.assistant")
	if stop := entryAssistant(t, entries[2]).StopReason; stop != ai.StopError {
		t.Fatalf("first answer stop reason = %s, want error", stop)
	}
	generations := values.generations(t)
	if !slices.ContainsFunc(generations, func(g LiveGeneration) bool {
		return g.Retry != nil && g.Retry.Error == "503 Service Unavailable"
	}) {
		t.Fatalf("no published retry backoff in %+v", generations)
	}
	if !slices.ContainsFunc(generations, func(g LiveGeneration) bool { return g.Attempt == 2 }) {
		t.Fatalf("no published second attempt in %+v", generations)
	}
	expectIdleLive(t, h, root)
}

func TestGenerationFailsWithModelErrorOnceRetriesAreExhausted(t *testing.T) {
	setup := newChatSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{
		fauxStep(fauxError("503 Service Unavailable")), fauxStep(fauxError("503 Service Unavailable")), fauxText("never"),
	})
	setup.setSettings(retrySettings(true, 1, time.Millisecond))
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	settled := waitFor(t, submit(t, root, inputDraft("hi")))
	expectSettled(t, settled, durable.SubmissionUnanswered, "model_error")
	if detail := string(settled.Detail); detail != `"503 Service Unavailable"` {
		t.Fatalf("detail = %s, want the error text", detail)
	}
	expectKinds(t, root, "pi.user", "pi.assistant", "pi.assistant")
	if calls := setup.faux.State.CallCount; calls != 2 {
		t.Fatalf("model calls = %d, want 2 (one response left)", calls)
	}
}

func TestGenerationFailsARetryableErrorWithoutRetryingWhenTheRetryPolicyIsDisabled(t *testing.T) {
	setup := newChatSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(fauxError("503 Service Unavailable")), fauxText("never")})
	setup.setSettings(func(s *HarnessSettings) { s.Retry.Enabled = ptr(false) })
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	expectSettled(t, waitFor(t, submit(t, root, inputDraft("hi"))), durable.SubmissionUnanswered, "model_error")
	if calls := setup.faux.State.CallCount; calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
}

func TestGenerationFailsANonRetryableErrorWithoutRetrying(t *testing.T) {
	setup := newChatSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(fauxError("Invalid request")), fauxText("never")})
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	settled := waitFor(t, submit(t, root, inputDraft("hi")))
	expectSettled(t, settled, durable.SubmissionUnanswered, "model_error")
	if detail := string(settled.Detail); detail != `"Invalid request"` {
		t.Fatalf("detail = %s, want the error text", detail)
	}
	var tasks durable.Page[durable.TaskRecord]
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		tasks, err = tx.ScanTasks(durable.TaskQuery{ConversationID: &root.ID}, 10, nil)
		return err
	}))
	outcome := tasks.Items[0].State.Outcome
	if outcome == nil || outcome.Status != durable.OutcomeFailed || outcome.Error.Message != "Invalid request" || string(outcome.Error.Detail) != `{"reason":"model_error"}` {
		t.Fatalf("outcome = %+v, want failed Invalid request {reason: model_error}", outcome)
	}
}

func TestGenerationPollsADeferredResponseUntilItIsReady(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Deferred: &providers.FauxDeferredOptions{PendingFetches: 1, PollAfterMs: 1}})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("deferred answer")})
	setup.setSettings(func(s *HarnessSettings) { s.Stream.Deferred = &StreamDeferral{} })
	h, root := openChat(t, memory.New(), setup)
	values := watchLive(t, h)
	h.Resume()
	settled := waitFor(t, submit(t, root, inputDraft("hi")))
	if settled.Status != durable.SubmissionDone || settled.Answer == nil {
		t.Fatalf("settled = %+v, want done", settled)
	}
	if fetches := setup.faux.State.DeferredFetchCount; fetches != 2 {
		t.Fatalf("deferred fetches = %d, want 2", fetches)
	}
	var pollTimes []int64
	for _, generation := range values.generations(t) {
		if generation.Deferred != nil {
			pollTimes = append(pollTimes, generation.Deferred.PollAt)
		}
	}
	if len(pollTimes) != 2 || pollTimes[1] <= pollTimes[0] {
		t.Fatalf("poll times = %v, want two, strictly later", pollTimes)
	}
	if text := textOf(entryByID(t, root, *settled.Answer)); text != "deferred answer" {
		t.Fatalf("answer = %q", text)
	}
}

func TestGenerationCancelsADeferredResponseWhenAbortedDuringPolling(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Deferred: &providers.FauxDeferredOptions{PendingFetches: 100, PollAfterMs: 60_000}})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("never")})
	setup.setSettings(func(s *HarnessSettings) { s.Stream.Deferred = &StreamDeferral{} })
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	submission := submit(t, root, inputDraft("hi"))
	taskID := runTask(t, h, root)
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Deferred != nil
	})
	_, err := h.AbortTask(bg, taskID)
	must(t, err)
	expectSettled(t, waitFor(t, submission), durable.SubmissionUnanswered, "aborted")
	if cancelled := len(setup.faux.State.CancelledDeferred); cancelled != 1 {
		t.Fatalf("cancelled deferred = %d, want 1", cancelled)
	}
	expectIdleLive(t, h, root)
}

func TestGenerationReportsAFailedDeferredCancellationAndStillEndsTheRunAborted(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Deferred: &providers.FauxDeferredOptions{PendingFetches: 100, PollAfterMs: 60_000}})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("never")})
	setup.models = cancelFailingModels{setup.models}
	setup.setSettings(func(s *HarnessSettings) { s.Stream.Deferred = &StreamDeferral{} })
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	submission := submit(t, root, inputDraft("hi"))
	taskID := runTask(t, h, root)
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Deferred != nil
	})
	_, err := h.AbortTask(bg, taskID)
	must(t, err)
	expectSettled(t, waitFor(t, submission), durable.SubmissionUnanswered, "aborted")
	info, ok := setup.reports.infoOf("cancel failed")
	if !ok {
		t.Fatalf("reports = %v, want cancel failed", setup.reports.all())
	}
	// Beyond pi: the failure is attributed to the host's models callback.
	if info.Callback != durable.CallbackModels {
		t.Fatalf("report = %+v, want the models callback", info)
	}
	expectIdleLive(t, h, root)
}

func TestGenerationReadsSettingsThroughGettersAtEveryDecision(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var seen []int
	record := func(opts *ai.SimpleStreamOptions) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, opts.TimeoutMs)
	}
	setup.faux.SetResponses([]providers.FauxResponseStep{
		func(_ context.Context, _ ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			record(opts)
			// The user changes the setting while the first attempt runs.
			setup.setSettings(func(s *HarnessSettings) { s.Stream.TimeoutMs = ptr(222) })
			return fauxError("503 Service Unavailable")
		},
		func(_ context.Context, _ ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			record(opts)
			return fauxAnswer("ok")
		},
	})
	setup.setSettings(func(s *HarnessSettings) {
		s.Stream.TimeoutMs = ptr(111)
		s.Retry.BaseDelay = ptr(time.Millisecond)
	})
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	if settled := waitFor(t, submit(t, root, inputDraft("hi"))); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	// The retry prepares again, so it resolves the settings again and sends
	// the new timeout.
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []int{111, 222}) {
		t.Fatalf("timeouts = %v, want [111 222]", seen)
	}
}

func TestGenerationCommitsNoPartialForAResponseThatTurnsDeferredAfterAnEmptyStartEvent(t *testing.T) {
	setup := newChatSetup(t)
	handle := ai.DeferredHandle{Provider: "faux", ModelID: "faux-1", Api: "faux", ID: "handle-1", PollAfterMs: 60_000}
	final := providers.FauxAssistantMessage(nil, ai.StopDeferred)
	final.Deferred = &handle
	// Longer than the partial throttle: an empty partial would be committed
	// while it waits.
	setup.models = streamModels{Models: setup.models, stream: scriptedStream(providers.FauxAssistantMessage(nil, ai.StopReason("pending")), final, 300*time.Millisecond)}
	h, root := openChat(t, memory.New(), setup)
	values := watchLive(t, h)
	submission := submit(t, root, inputDraft("hi"))
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Deferred != nil
	})
	if slices.ContainsFunc(values.generations(t), func(g LiveGeneration) bool { return g.Message != nil }) {
		t.Fatalf("a partial was committed: %+v", values.generations(t))
	}
	_, err := h.AbortTask(bg, runTask(t, h, root))
	must(t, err)
	expectSettled(t, waitFor(t, submission), durable.SubmissionUnanswered, "aborted")
	expectKinds(t, root, "pi.user")
}

// pi "attributes model requests to pi by default and merges the Harness
// identity over it" (upstream adc5fa9f, 04a93bad). DIVERGENCE: the default
// has no attribution here (DefaultHarnessIdentity), so the first request
// carries an empty identity where pi's carries PI_ATTRIBUTION.
func TestGenerationMergesTheHarnessIdentityOverTheDefault(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []*ai.ClientIdentity
	)
	capture := func(_ context.Context, _ ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		mu.Lock()
		seen = append(seen, opts.Identity)
		mu.Unlock()
		return providers.FauxAssistantMessage(ai.ContentList{providers.FauxText("ok")}, ai.StopStop)
	}
	short := ai.UserAgentShort
	acme := ai.ClientAttribution{Name: "acme", URL: "https://acme.example"}
	for _, identity := range []*ai.ClientIdentity{nil, {UserAgent: &short, Attribution: &acme}} {
		setup := newChatSetup(t)
		setup.identity = identity
		setup.faux.SetResponses([]providers.FauxResponseStep{capture})
		h, root := openChat(t, memory.New(), setup)
		h.Resume()
		waitFor(t, submit(t, root, inputDraft("hi")))
		must(t, h.Close(bg))
	}
	want := []*ai.ClientIdentity{{}, {UserAgent: &short, Attribution: &acme}}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("identities = %+v, want %+v", seen, want)
	}
}
