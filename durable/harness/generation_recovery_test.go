package harness

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-generation-recovery.test.ts.
// pi reopens a SQLite file; here a JSONL directory, as in tasks recovery:
// both catch what serialization loses, and SQLite lives in a submodule.

// clock is a settable Harness clock in Unix milliseconds (pi's `let now`).
type msClock struct{ ms atomic.Int64 }

func newMsClock(setup *chatSetup, ms int64) *msClock {
	c := &msClock{}
	c.ms.Store(ms)
	setup.setNow(func() time.Time { return time.UnixMilli(c.ms.Load()) })
	return c
}

func (c *msClock) set(ms int64) { c.ms.Store(ms) }

// runTaskOf is the conversation's run task, read from pi.live.
func runTaskOf(t *testing.T, h *Harness, conversation *Conversation) durable.TaskID {
	t.Helper()
	run := decodeLive(t, liveOf(t, h, conversation)).Run
	if run == nil {
		t.Fatal("pi.live has no run")
	}
	return run.TaskID
}

// checkpointOf is a generation's checkpoint; nil once terminal.
func generationCheckpoint(t *testing.T, h *Harness, id durable.TaskID) *GenerationCheckpoint {
	t.Helper()
	record := getTask(t, h, id)
	if record == nil || record.State.Status == durable.TaskTerminal {
		return nil
	}
	var checkpoint GenerationCheckpoint
	must(t, json.Unmarshal(record.State.Checkpoint, &checkpoint))
	return &checkpoint
}

// waitSubmission waits for a submission by ID, as a reopened Harness finds it.
func waitSubmission(t *testing.T, h *Harness, id durable.SubmissionID) durable.SubmissionRecord {
	t.Helper()
	submission, err := h.Submission(bg, id)
	must(t, err)
	if submission == nil {
		t.Fatalf("submission %d not found", id)
	}
	return waitFor(t, submission)
}

func roles(messages []ai.Message) []string {
	return names(messages, func(message ai.Message) string {
		switch message.(type) {
		case ai.SystemMessage:
			return "system"
		case ai.UserMessage:
			return "user"
		case ai.AssistantMessage:
			return "assistant"
		default:
			return "other"
		}
	})
}

func expectWake(t *testing.T, h *Harness, at int64) {
	t.Helper()
	eventually(t, func() bool {
		wake, err := h.Wake()
		must(t, err)
		return !wake.Active
	})
	wake, err := h.Wake()
	must(t, err)
	if wake.Active || !wake.At.Equal(time.UnixMilli(at)) {
		t.Fatalf("wake = %+v, want inactive at %d", wake, at)
	}
}

func TestGenerationRecoveryRerunsPreparationInterruptedBeforeItsCommit(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	reached := make(chan struct{})
	var block atomic.Bool
	block.Store(true)
	addSection(t, setup.registry, Section("preamble", func(ctx context.Context, _ PromptInput) (string, bool, error) {
		if block.CompareAndSwap(true, false) {
			close(reached)
			<-ctx.Done()
			return "", false, context.Cause(ctx)
		}
		return "p", true, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("answer")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	id := submit(t, root, inputDraft("hi")).ID
	awaitSignal(t, reached, "the preparation")
	taskID := runTaskOf(t, h, root)
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, dir), setup)
	if got := generationCheckpoint(t, h, taskID); !reflect.DeepEqual(got, &GenerationCheckpoint{Phase: phasePrepare, Attempt: 1}) {
		t.Fatalf("checkpoint = %+v, want prepare attempt 1", got)
	}
	h.Resume()
	if settled := waitSubmission(t, h, id); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	expectKinds(t, root, "pi.user", "pi.system", "pi.assistant")
}

func TestGenerationRecoveryResendsARequestInterruptedBeforeAnyPartialWithoutRepeatingPreparation(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	preamble := constSection("preamble", "p")
	preamble.Untagged = true
	addSection(t, setup.registry, preamble)
	step, reached := unanswered()
	var mu sync.Mutex
	var sent [][]string
	var timeouts []int
	setup.faux.SetResponses([]providers.FauxResponseStep{
		step,
		func(_ context.Context, req ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			mu.Lock()
			sent = append(sent, roles(req.Messages))
			timeouts = append(timeouts, opts.TimeoutMs)
			mu.Unlock()
			return fauxAnswer("answer")
		},
	})
	h, root := openChat(t, openJSONL(t, dir), setup)
	setup.setSettings(func(s *HarnessSettings) { s.Stream = ConversationStreamOptions{TimeoutMs: ptr(1234)} })
	h.Resume()
	id := submit(t, root, inputDraft("hi")).ID
	awaitSignal(t, reached, "the request")
	taskID := runTaskOf(t, h, root)
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, dir), setup)
	checkpoint := generationCheckpoint(t, h, taskID)
	if checkpoint == nil || checkpoint.Phase != phaseRequest || checkpoint.Attempt != 1 || checkpoint.ThinkingLevel != "off" ||
		checkpoint.StreamOptions == nil || checkpoint.StreamOptions.TimeoutMs == nil || *checkpoint.StreamOptions.TimeoutMs != 1234 {
		t.Fatalf("checkpoint = %+v, want the pinned request", checkpoint)
	}
	// The resend uses the pinned request, not options changed after
	// preparation.
	setup.setSettings(func(s *HarnessSettings) { s.Stream = ConversationStreamOptions{TimeoutMs: ptr(999)} })
	if generation := decodeLive(t, liveOf(t, h, root)).Generation; generation == nil || generation.Attempt != 1 {
		t.Fatalf("pi.live generation = %+v, want attempt 1", generation)
	}
	h.Resume()
	if settled := waitSubmission(t, h, id); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(sent, [][]string{{"system", "user"}}) || !slices.Equal(timeouts, []int{1234}) {
		t.Fatalf("sent %v with timeouts %v, want [[system user]] with [1234]", sent, timeouts)
	}
	expectKinds(t, root, "pi.user", "pi.system", "pi.assistant")
}

func TestGenerationRecoveryConvertsACommittedPartialIntoAnAbortedEntryAndResendsTheSameMessages(t *testing.T) {
	dir := t.TempDir()
	slow := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 20, MinTokenSize: 1, MaxTokenSize: 1})
	slow.faux.SetResponses([]providers.FauxResponseStep{fauxText(interruptedText("z"))})
	h, root := openChat(t, openJSONL(t, dir), slow)
	h.Resume()
	values := watchLive(t, h)
	id := submit(t, root, inputDraft("hi")).ID
	var watched string
	eventually(t, func() bool {
		for _, generation := range values.generations(t) {
			if generation.Message != nil {
				watched = textOf(durable.EntryRecord{Model: []ai.Message{*generation.Message}})
			}
		}
		return watched != ""
	})
	must(t, h.Close(bg))

	setup := newChatSetup(t)
	var mu sync.Mutex
	var sent [][]string
	setup.faux.SetResponses([]providers.FauxResponseStep{
		func(_ context.Context, req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			mu.Lock()
			sent = append(sent, roles(req.Messages))
			mu.Unlock()
			return fauxAnswer("answer")
		},
	})
	h, root = openChat(t, openJSONL(t, dir), setup)
	stored := decodeLive(t, liveOf(t, h, root)).Generation
	if stored == nil || stored.Message == nil {
		t.Fatal("no committed partial after reopen")
	}
	partial := textOf(durable.EntryRecord{Model: []ai.Message{*stored.Message}})
	// Everything observers saw before the crash is durable.
	if !strings.HasPrefix(partial, watched) {
		t.Fatalf("stored partial %q does not extend the watched %q", partial, watched)
	}
	h.Resume()
	if settled := waitSubmission(t, h, id); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	mu.Lock()
	if !reflect.DeepEqual(sent, [][]string{{"user"}}) {
		t.Fatalf("sent = %v, want [[user]]", sent)
	}
	mu.Unlock()
	entries := expectKinds(t, root, "pi.user", "pi.assistant", "pi.assistant")
	if converted := entryAssistant(t, entries[1]); converted.StopReason != ai.StopAborted || textOf(entries[1]) != partial {
		t.Fatalf("converted = %s %q, want the aborted partial", converted.StopReason, textOf(entries[1]))
	}
	expectIdleLive(t, h, root)
}

func TestGenerationRecoveryResumesARetryBackoffAfterReopen(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	now := newMsClock(setup, 1_000)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(fauxError("503 Service Unavailable")), fauxText("recovered")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	setup.setSettings(retrySettings(true, 2, time.Minute))
	id := submit(t, root, inputDraft("hi")).ID
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Retry != nil
	})
	taskID := runTaskOf(t, h, root)
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, dir), setup)
	state := getTask(t, h, taskID).State
	if state.Status != durable.TaskSleeping || state.Until == nil || *state.Until != 61_000 ||
		string(state.Checkpoint) != `{"phase":"retry","attempt":1,"until":61000}` {
		t.Fatalf("state = %s %v %s, want sleeping until 61000 in retry", state.Status, state.Until, state.Checkpoint)
	}
	expectWake(t, h, 61_000)
	// An early wake retries nothing.
	h.Resume()
	time.Sleep(20 * time.Millisecond)
	if calls := setup.faux.State.CallCount; calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
	want := liveView{
		Run:        &LiveRun{TaskID: taskID, Inputs: []durable.SubmissionID{id}},
		Generation: &LiveGeneration{Attempt: 1, Retry: &LiveRetry{At: 61_000, Error: "503 Service Unavailable"}},
	}
	if got := decodeLive(t, liveOf(t, h, root)); !reflect.DeepEqual(got, want) {
		t.Fatalf("pi.live = %+v, want %+v", got, want)
	}
	now.set(61_000)
	h.Resume()
	if settled := waitSubmission(t, h, id); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	expectKinds(t, root, "pi.user", "pi.assistant", "pi.assistant")
}

func TestGenerationRecoverySleepsAgainForARetryStoredAsARunningPhaseByAnEarlierVersion(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	now := newMsClock(setup, 1_000)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(fauxError("503 Service Unavailable")), fauxText("recovered")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	setup.setSettings(retrySettings(true, 2, time.Minute))
	id := submit(t, root, inputDraft("hi")).ID
	eventually(t, func() bool {
		run := decodeLive(t, liveOf(t, h, root)).Run
		return run != nil && getTask(t, h, run.TaskID).State.Status == durable.TaskSleeping
	})
	taskID := runTaskOf(t, h, root)
	must(t, h.Close(bg))
	// Earlier versions slept inside a running retry phase.
	storage := openJSONL(t, dir)
	record, err := storage.Task(bg, taskID)
	must(t, err)
	record.State.Status = durable.TaskRunning
	record.State.Until = nil
	_, err = storage.Commit(bg, []durable.StorageWrite{durable.TaskWrite{Value: *record}})
	must(t, err)
	must(t, storage.Close(bg))

	h, _ = openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	eventually(t, func() bool { return getTask(t, h, taskID).State.Status == durable.TaskSleeping })
	if until := getTask(t, h, taskID).State.Until; until == nil || *until != 61_000 {
		t.Fatalf("until = %v, want 61000", until)
	}
	if calls := setup.faux.State.CallCount; calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
	now.set(61_000)
	h.Resume()
	if settled := waitSubmission(t, h, id); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	if calls := setup.faux.State.CallCount; calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
}

func TestGenerationRecoveryResumesPollingADeferredResponseAfterReopen(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Deferred: &providers.FauxDeferredOptions{PollAfterMs: 60_000}})
	now := newMsClock(setup, 1_000)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("deferred answer")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	setup.setSettings(func(s *HarnessSettings) { s.Stream.Deferred = &StreamDeferral{} })
	id := submit(t, root, inputDraft("hi")).ID
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Deferred != nil
	})
	taskID := runTaskOf(t, h, root)
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, dir), setup)
	checkpoint := generationCheckpoint(t, h, taskID)
	if checkpoint == nil || checkpoint.Phase != phasePoll || checkpoint.Attempt != 1 || checkpoint.PollAt == nil || *checkpoint.PollAt != 61_000 {
		t.Fatalf("checkpoint = %+v, want poll at 61000", checkpoint)
	}
	if state := getTask(t, h, taskID).State; state.Status != durable.TaskSleeping || state.Until == nil || *state.Until != 61_000 {
		t.Fatalf("state = %s %v, want sleeping until 61000", state.Status, state.Until)
	}
	expectWake(t, h, 61_000)
	now.set(61_000)
	h.Resume()
	settled := waitSubmission(t, h, id)
	if settled.Status != durable.SubmissionDone || settled.Answer == nil {
		t.Fatalf("settled = %+v, want done", settled)
	}
	if text := textOf(entryByID(t, root, *settled.Answer)); text != "deferred answer" {
		t.Fatalf("answer = %q", text)
	}
	if fetches := setup.faux.State.DeferredFetchCount; fetches != 1 {
		t.Fatalf("deferred fetches = %d, want 1", fetches)
	}
}

func TestGenerationRecoveryFailsNoModelWhenThePinnedModelIsGoneAfterReopenInRequestAndInPoll(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Deferred: &providers.FauxDeferredOptions{PollAfterMs: 60_000}})
	now := newMsClock(setup, 1_000)
	step, reached := unanswered()
	setup.faux.SetResponses([]providers.FauxResponseStep{step, fauxText("deferred")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	requesting := submit(t, root, inputDraft("one")).ID
	awaitSignal(t, reached, "the request")
	must(t, h.Close(bg))

	// Reopened without the faux provider: the request's pinned model is
	// unknown.
	empty := &chatSetup{faux: setup.faux, models: ai.CreateModels(nil), registry: setup.registry, reports: setup.reports, now: func() time.Time { return time.UnixMilli(now.ms.Load()) }}
	h, _ = openChat(t, openJSONL(t, dir), empty)
	h.Resume()
	expectSettled(t, waitSubmission(t, h, requesting), durable.SubmissionUnanswered, "no_model")
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, dir), setup)
	setup.setSettings(func(s *HarnessSettings) { s.Stream.Deferred = &StreamDeferral{} })
	h.Resume()
	polling := submit(t, root, inputDraft("two")).ID
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Deferred != nil
	})
	must(t, h.Close(bg))

	// The poll sleeps until it is due; then the model is resolved.
	now.set(61_000)
	h, _ = openChat(t, openJSONL(t, dir), empty)
	h.Resume()
	expectSettled(t, waitSubmission(t, h, polling), durable.SubmissionUnanswered, "no_model")
}

func TestGenerationRecoveryRunsAPrintStyleTurnAndReadsTheDurableAnswerAfterReopen(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	preamble := constSection("preamble", "You are terse.")
	preamble.Untagged = true
	addSection(t, setup.registry, preamble)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("42")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	submission := submit(t, root, inputDraft("answer?"))
	settled := waitFor(t, submission)
	if settled.Status != durable.SubmissionDone || settled.Answer == nil {
		t.Fatalf("settled = %+v, want done", settled)
	}
	answer := entryByID(t, root, *settled.Answer)
	if text := textOf(answer); text != "42" {
		t.Fatalf("answer = %q", text)
	}
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, dir), setup)
	reopened, err := h.Submission(bg, submission.ID)
	must(t, err)
	status, err := reopened.Status(bg)
	must(t, err)
	if !reflect.DeepEqual(status, settled) {
		t.Fatalf("status after reopen = %+v, want %+v", status, settled)
	}
	if got := entryByID(t, root, *settled.Answer); !reflect.DeepEqual(got, answer) {
		t.Fatalf("answer after reopen = %+v, want %+v", got, answer)
	}
}

// From pi test/harness-inbox.test.ts, held back from slice 1 (c) for the
// generation recovery it needs.
func TestInboxKeepsUsageTotalsExactAcrossReopenCountingAPartialConvertedAfterReopenOnce(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 200, MinTokenSize: 1, MaxTokenSize: 1})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("first"), fauxText(interruptedText("x")), fauxText("again")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	waitFor(t, submit(t, root, inputDraft("a")))
	submit(t, root, inputDraft("b"))
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Message != nil
	})
	// Closing mid-stream keeps the committed partial; the reopened request
	// converts it into an aborted entry.
	must(t, h.Close(bg))
	h, root = openChat(t, openJSONL(t, dir), setup)
	must(t, h.Resume())
	must(t, h.WaitForIdle(bg))
	var assistants []ai.AssistantMessage
	for _, entry := range chatEntries(t, root) {
		if entry.Kind == "pi.assistant" {
			assistants = append(assistants, entryAssistant(t, entry))
		}
	}
	if got := names(assistants, func(m ai.AssistantMessage) ai.StopReason { return m.StopReason }); !slices.Equal(got, []ai.StopReason{ai.StopStop, ai.StopAborted, ai.StopStop}) {
		t.Fatalf("stop reasons = %v, want stop, aborted, stop", got)
	}
	usage, err := h.Usage(bg)
	must(t, err)
	total := usage.Models["faux/faux-1"]
	var input, output, tokens int
	for _, message := range assistants {
		input += message.Usage.Input
		output += message.Usage.Output
		tokens += message.Usage.TotalTokens
	}
	if total.Input != input || total.Output != output || total.TotalTokens != tokens {
		t.Fatalf("ledger = %d/%d/%d, entries sum to %d/%d/%d", total.Input, total.Output, total.TotalTokens, input, output, tokens)
	}
}

// Beyond pi: no case of pi's runs the poll phase before it is due. A poll
// stored as a running phase (an earlier version slept inside it) must sleep
// again instead of fetching.
func TestGenerationRecoverySleepsAgainForAPollStoredAsARunningPhaseByAnEarlierVersion(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Deferred: &providers.FauxDeferredOptions{PollAfterMs: 60_000}})
	now := newMsClock(setup, 1_000)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("deferred answer")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	setup.setSettings(func(s *HarnessSettings) { s.Stream.Deferred = &StreamDeferral{} })
	id := submit(t, root, inputDraft("hi")).ID
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Deferred != nil
	})
	taskID := runTaskOf(t, h, root)
	must(t, h.Close(bg))
	storage := openJSONL(t, dir)
	record, err := storage.Task(bg, taskID)
	must(t, err)
	record.State.Status = durable.TaskRunning
	record.State.Until = nil
	_, err = storage.Commit(bg, []durable.StorageWrite{durable.TaskWrite{Value: *record}})
	must(t, err)
	must(t, storage.Close(bg))

	h, _ = openChat(t, openJSONL(t, dir), setup)
	h.Resume()
	eventually(t, func() bool { return getTask(t, h, taskID).State.Status == durable.TaskSleeping })
	if until := getTask(t, h, taskID).State.Until; until == nil || *until != 61_000 {
		t.Fatalf("until = %v, want 61000", until)
	}
	if fetches := setup.faux.State.DeferredFetchCount; fetches != 0 {
		t.Fatalf("deferred fetches = %d, want none before the poll is due", fetches)
	}
	now.set(61_000)
	h.Resume()
	if settled := waitSubmission(t, h, id); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	if fetches := setup.faux.State.DeferredFetchCount; fetches != 1 {
		t.Fatalf("deferred fetches = %d, want 1", fetches)
	}
}

// clockBackModels steps the Harness clock back after each deferred fetch.
type clockBackModels struct {
	ai.Models
	back func()
}

func (m clockBackModels) FetchDeferred(ctx context.Context, model *ai.Model, handle ai.DeferredHandle, opts *ai.ModelsDeferredFetchOptions) *ai.AssistantMessage {
	message := m.Models.FetchDeferred(ctx, model, handle, opts)
	m.back()
	return message
}

// Beyond pi: pi's case polls strictly later only when two polls share a
// millisecond. A clock stepped back during the poll makes it certain: the
// next poll would otherwise be due no later than the one just made.
func TestGenerationPollsAStillDeferredResultStrictlyLaterThanThePollThatReturnedIt(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Deferred: &providers.FauxDeferredOptions{PendingFetches: 1, PollAfterMs: 1}})
	now := newMsClock(setup, 1_000)
	setup.models = clockBackModels{Models: setup.models, back: func() { now.set(1_000) }}
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("deferred answer")})
	setup.setSettings(func(s *HarnessSettings) { s.Stream.Deferred = &StreamDeferral{} })
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	id := submit(t, root, inputDraft("hi")).ID
	pollAt := func() int64 {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		if generation == nil || generation.Deferred == nil {
			return 0
		}
		return generation.Deferred.PollAt
	}
	eventually(t, func() bool { return pollAt() == 1_001 })
	now.set(1_001)
	h.Resume()
	// The poll still finds it deferred, at 1000 on the stepped-back clock.
	eventually(t, func() bool { return pollAt() != 1_001 })
	if got := pollAt(); got != 1_002 {
		t.Fatalf("next poll at %d, want 1002, strictly after the poll at 1001", got)
	}
	now.set(1_002)
	h.Resume()
	if settled := waitSubmission(t, h, id); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
}
