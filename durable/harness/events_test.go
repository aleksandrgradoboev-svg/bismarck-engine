package harness

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-events.test.ts, without the
// cases of nested calls (slice 2c) and compaction (slice 3).

// eventLog records every delivered batch of a stream.
type eventLog struct {
	mu      sync.Mutex
	batches [][]AgentEvent
}

func (l *eventLog) record(_ context.Context, events []AgentEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.batches = append(l.batches, slices.Clone(events))
	return nil
}

func (l *eventLog) all() [][]AgentEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.batches)
}

func (l *eventLog) events() []AgentEvent {
	var out []AgentEvent
	for _, batch := range l.all() {
		out = append(out, batch...)
	}
	return out
}

// listen attaches and starts a stream that records every batch.
func listen(t *testing.T, conversation *Conversation) (*AgentEventStream, *eventLog) {
	t.Helper()
	stream, err := conversation.WatchEvents(bg)
	must(t, err)
	t.Cleanup(func() { stream.Stop() })
	log := &eventLog{}
	must(t, stream.Start(log.record))
	return stream, log
}

// quiet waits until no batch arrives for 50 ms: batches are delivered off
// the commit (pi waits a macrotask).
func (l *eventLog) quiet(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	count := len(l.all())
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		next := len(l.all())
		if next == count {
			return
		}
		count = next
	}
	t.Fatal("event batches kept arriving")
}

func eventTypes(events []AgentEvent) []string {
	return names(events, func(event AgentEvent) string { return event.EventType() })
}

func partialText(message ai.Message) string {
	return textOf(durable.EntryRecord{Model: []ai.Message{message}})
}

// streamedText rebuilds the streamed text of block 0 from message_start and
// the text changes that follow it.
func streamedText(events []AgentEvent) string {
	text := ""
	for _, event := range events {
		switch e := event.(type) {
		case MessageStartEvent:
			if message, ok := e.Message.(ai.AssistantMessage); ok {
				text = ""
				if len(message.Content) > 0 {
					if block, ok := message.Content[0].(ai.TextContent); ok {
						text = block.Text
					}
				}
			}
		case MessageUpdateEvent:
			for _, change := range e.Changes {
				switch c := change.(type) {
				case BlockStartChange:
					if block, ok := c.Block.(ai.TextContent); ok && c.ContentIndex == 0 {
						text = block.Text
					}
				case TextDeltaChange:
					if c.Kind == "text_delta" && c.ContentIndex == 0 {
						text += c.Delta
					}
				}
			}
		}
	}
	return text
}

// applyChanges applies message changes to a copy of message, as an
// events-only consumer would (text and thinking only).
func applyChanges(message ai.AssistantMessage, changes []MessageChange) ai.AssistantMessage {
	next := message
	next.Content = slices.Clone(message.Content)
	for _, change := range changes {
		switch c := change.(type) {
		case MessageReplaceChange:
			next = c.Message
		case BlockChange:
			next.Content[c.ContentIndex] = c.Block
		case BlockStartChange:
			next.Content = slices.Insert(next.Content, c.ContentIndex, c.Block)
		case TextDeltaChange:
			switch block := next.Content[c.ContentIndex].(type) {
			case ai.TextContent:
				block.Text += c.Delta
				next.Content[c.ContentIndex] = block
			case ai.ThinkingContent:
				block.Thinking += c.Delta
				next.Content[c.ContentIndex] = block
			}
		}
	}
	return next
}

// heldStep answers text once release closes or the request is cancelled.
func heldStep(release <-chan struct{}, text string) providers.FauxResponseStep {
	return func(ctx context.Context, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return fauxAnswer(text)
	}
}

func TestEventsStreamARunAsLifecycleEventsAndTextDeltasThatRebuildTheAnswer(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 400, MinTokenSize: 1, MaxTokenSize: 1})
	text := strings.Repeat("streamed answer text ", 20)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText(text)})
	h, root := openChat(t, memory.New(), setup)
	stream, log := listen(t, root)
	if len(stream.Snapshot.Entries) != 0 || len(stream.Snapshot.Inbox) != 0 {
		t.Fatalf("snapshot = %+v, want no entries and no inbox", stream.Snapshot)
	}
	var mu sync.Mutex
	var partials []string
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind && document.Value != nil {
				if generation := decodeLive(t, document.Value).Generation; generation != nil && generation.Message != nil {
					mu.Lock()
					partials = append(partials, partialText(*generation.Message))
					mu.Unlock()
				}
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	submission := submit(t, root, inputDraft("hi"))
	waitFor(t, submission)
	log.quiet(t)
	all := log.events()
	var types []string
	for _, kind := range eventTypes(all) {
		if len(types) == 0 || types[len(types)-1] != kind {
			types = append(types, kind)
		}
	}
	want := []string{"message_start", "message_end", "submission", "run_start", "turn_start", "message_start", "message_update", "message_end", "turn_end", "run_end", "submission", "usage_changed"}
	if !slices.Equal(types, want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	for _, event := range all {
		if start, ok := event.(RunStartEvent); ok && !reflect.DeepEqual(start.Inputs, []durable.SubmissionID{submission.ID}) {
			t.Fatalf("run_start = %+v, want the input", start)
		}
	}
	// After each event, the rebuilt text equals the committed partial of
	// that commit.
	var rebuilt []string
	for index, event := range all {
		start, isStart := event.(MessageStartEvent)
		_, assistant := start.Message.(ai.AssistantMessage)
		if _, isUpdate := event.(MessageUpdateEvent); (isStart && assistant) || isUpdate {
			rebuilt = append(rebuilt, streamedText(all[:index+1]))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(rebuilt, partials) || len(partials) < 2 {
		t.Fatalf("rebuilt %d texts, committed %d partials; want them equal and more than one", len(rebuilt), len(partials))
	}
	var end MessageEndEvent
	for _, event := range all {
		if e, ok := event.(MessageEndEvent); ok {
			end = e
		}
	}
	if textOf(end.Entry) != text {
		t.Fatalf("last message_end = %q", textOf(end.Entry))
	}
}

func TestEventsReportQueuedSubmissionsInboxChangesAndRetries(t *testing.T) {
	setup := newChatSetup(t)
	release := make(chan struct{})
	setup.faux.SetResponses([]providers.FauxResponseStep{heldStep(release, "first"), fauxStep(fauxError("503 Service Unavailable")), fauxText("second")})
	h, root := openChat(t, memory.New(), setup)
	_ = h
	setup.setSettings(retrySettings(true, 1, time.Millisecond))
	_, log := listen(t, root)
	submit(t, root, inputDraft("a"))
	followUp := submit(t, root, inputDraft("f"))
	log.quiet(t)
	var inboxUpdates []AgentEvent
	for _, event := range log.events() {
		if _, ok := event.(InboxUpdateEvent); ok {
			inboxUpdates = append(inboxUpdates, event)
		}
	}
	if want := []AgentEvent{InboxUpdateEvent{Items: []QueuedItem{{ID: followUp.ID, Mode: "followUp"}}}}; !reflect.DeepEqual(inboxUpdates, want) {
		t.Fatalf("inbox updates = %+v, want %+v", inboxUpdates, want)
	}
	close(release)
	waitFor(t, followUp)
	log.quiet(t)
	types := eventTypes(log.events())
	if !slices.Contains(types, "auto_retry_start") || !slices.Contains(types, "auto_retry_end") {
		t.Fatalf("event types = %v, want the retry", types)
	}
	if runs := len(slices.DeleteFunc(slices.Clone(types), func(kind string) bool { return kind != "run_start" })); runs != 2 {
		t.Fatalf("run starts = %d, want 2", runs)
	}
	var statuses []string
	for _, event := range log.events() {
		if e, ok := event.(SubmissionEvent); ok {
			statuses = append(statuses, e.Record.Status)
		}
	}
	if want := []string{"placed", "queued", "done", "placed", "done"}; !slices.Equal(statuses, want) {
		t.Fatalf("submission statuses = %v, want %v", statuses, want)
	}
}

func commitNote(t *testing.T, conversation *Conversation) {
	t.Helper()
	must(t, conversation.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.AppendEntry(conversation.ID, durable.EntryDraft{Kind: "note"})
		return err
	}))
}

func TestEventsReplaceUndeliveredBatchesWithOneSnapshotAfter100PendingBatches(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	stream, err := root.WatchEvents(bg)
	must(t, err)
	defer stream.Stop()
	for range 101 {
		commitNote(t, root)
	}
	log := &eventLog{}
	must(t, stream.Start(log.record))
	log.quiet(t)
	batches := log.all()
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("batches = %d (%v), want one batch of one snapshot", len(batches), batches)
	}
	snapshot, ok := batches[0][0].(SnapshotEvent)
	if !ok || len(snapshot.Entries) != 101 {
		t.Fatalf("batch = %+v, want a snapshot of 101 entries", batches[0][0])
	}
}

func TestEventsEmitOneExactBatchWhenARunEndsAndAQueuedFollowUpStartsTheNext(t *testing.T) {
	setup := newChatSetup(t)
	release := make(chan struct{})
	setup.faux.SetResponses([]providers.FauxResponseStep{heldStep(release, "first"), fauxText("second")})
	_, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	followUp := submit(t, root, inputDraft("f"))
	_, log := listen(t, root)
	close(release)
	waitFor(t, followUp)
	log.quiet(t)
	var boundary []AgentEvent
	for _, batch := range log.all() {
		if slices.ContainsFunc(batch, func(event AgentEvent) bool { return event.EventType() == "run_end" }) {
			boundary = batch
			break
		}
	}
	want := []string{"message_start", "message_end", "message_start", "message_end", "turn_end", "run_end", "submission", "submission", "inbox_update", "usage_changed", "run_start", "turn_start"}
	if got := eventTypes(boundary); !slices.Equal(got, want) {
		t.Fatalf("boundary batch = %v, want %v", got, want)
	}
	var ids []durable.SubmissionID
	for _, event := range boundary {
		if e, ok := event.(SubmissionEvent); ok {
			ids = append(ids, e.Record.ID)
		}
	}
	if !slices.Equal(ids, []durable.SubmissionID{input.ID, followUp.ID}) {
		t.Fatalf("submission records = %v, want the input then the follow-up", ids)
	}
}

func TestEventsRejectAnAttachmentCancelledWhileItWaitsForTheSessionLine(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	entered := make(chan struct{})
	release := make(chan struct{})
	blocking := make(chan error, 1)
	go func() {
		blocking <- root.Commit(bg, func(*durable.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithCancelCause(bg)
	attaching := make(chan error, 1)
	go func() {
		_, err := root.WatchEvents(ctx)
		attaching <- err
	}()
	cancel(errors.New("cancelled"))
	close(release)
	must(t, <-blocking)
	if err := <-attaching; err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("attach = %v, want cancelled", err)
	}
}

func TestEventsApplyDeltasAfterAnOverflowSnapshotThatHoldsAnInFlightPartial(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	partial := fauxAnswer("hel")
	stream, err := root.WatchEvents(bg)
	must(t, err)
	defer stream.Stop()
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		live, err := liveDraft(tx, root.ID)
		if err != nil {
			return err
		}
		return setDraftField(live, "generation", LiveGeneration{Attempt: 1, Message: partial})
	}))
	for range 101 {
		commitNote(t, root)
	}
	log := &eventLog{}
	must(t, stream.Start(log.record))
	log.quiet(t)
	snapshot, ok := log.all()[0][0].(SnapshotEvent)
	if !ok || snapshot.Generation == nil || snapshot.Generation.Message == nil {
		t.Fatalf("first batch = %+v, want a snapshot with the partial", log.all()[0])
	}
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		live, err := liveDraft(tx, root.ID)
		if err != nil {
			return err
		}
		return live.At("generation").At("message").At("content").At(0).Set("text", "hello")
	}))
	log.quiet(t)
	batches := log.all()
	update, ok := batches[len(batches)-1][0].(MessageUpdateEvent)
	if !ok {
		t.Fatalf("last batch = %+v, want a message update", batches[len(batches)-1])
	}
	if want := []MessageChange{TextDeltaChange{Kind: "text_delta", ContentIndex: 0, Delta: "lo"}}; !reflect.DeepEqual(update.Changes, want) {
		t.Fatalf("changes = %+v, want %+v", update.Changes, want)
	}
	rebuilt := applyChanges(*snapshot.Generation.Message, update.Changes)
	if !reflect.DeepEqual(rebuilt.Content, ai.ContentList{ai.TextContent{Text: "hello"}}) {
		t.Fatalf("rebuilt content = %+v, want hello", rebuilt.Content)
	}
}

func TestEventsReportUsageOnlyUpdatesClearedToolProgressToolsEndingWithoutEntriesAndDeferredPolls(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	_, log := listen(t, root)
	change := func(edit func(tx *durable.Tx) error) {
		t.Helper()
		must(t, root.Commit(bg, edit))
	}
	live := func(tx *durable.Tx) (*delta.Draft, error) { return liveDraft(tx, root.ID) }
	partial := fauxAnswer("partial")
	change(func(tx *durable.Tx) error {
		draft, err := live(tx)
		if err != nil {
			return err
		}
		return setDraftField(draft, "generation", LiveGeneration{Attempt: 1, Message: partial})
	})
	// A usage-only change sends the usage and no changes.
	change(func(tx *durable.Tx) error {
		draft, err := live(tx)
		if err != nil {
			return err
		}
		return draft.At("generation").At("message").At("usage").Set("input", float64(42))
	})
	change(func(tx *durable.Tx) error {
		draft, err := live(tx)
		if err != nil {
			return err
		}
		return setDraftField(draft, "generation", LiveGeneration{Attempt: 1, Deferred: &LiveDeferred{PollAt: 1}})
	})
	change(func(tx *durable.Tx) error {
		draft, err := live(tx)
		if err != nil {
			return err
		}
		return draft.At("generation").At("deferred").Set("pollAt", float64(2))
	})
	change(func(tx *durable.Tx) error {
		draft, err := live(tx)
		if err != nil {
			return err
		}
		return draft.Set("tools", []any{map[string]any{"callId": "c1", "name": "t", "status": slotRunning, "details": map[string]any{"n": float64(1)}, "diagnostics": []any{}}})
	})
	// A safe replay clears the running slot's progress.
	change(func(tx *durable.Tx) error {
		draft, err := live(tx)
		if err != nil {
			return err
		}
		slot := draft.At("tools").At(0)
		if err := slot.Delete("details"); err != nil {
			return err
		}
		return slot.Delete("diagnostics")
	})
	// A fault marks the slot done without an entry.
	change(func(tx *durable.Tx) error {
		draft, err := live(tx)
		if err != nil {
			return err
		}
		return draft.At("tools").At(0).Set("status", slotDone)
	})
	change(func(tx *durable.Tx) error { return tx.RetireDoc(UsageDoc.Conversation(root.ID)) })
	change(func(tx *durable.Tx) error { return tx.RetireDoc(AgentDoc.Conversation(root.ID)) })
	log.quiet(t)
	usage := partial.Usage
	usage.Input = 42
	initialUsage, err := usageOf(nil)
	must(t, err)
	want := [][]AgentEvent{
		{MessageStartEvent{Message: *partial}},
		{MessageUpdateEvent{Usage: usage, Changes: []MessageChange{}}},
		{DeferredPollEvent{PollAt: 1}},
		{DeferredPollEvent{PollAt: 2}},
		{ToolExecutionStartEvent{ToolEventCall: ToolEventCall{ToolCallID: "c1", ToolName: "t"}, Args: map[string]any{}}},
		{ToolExecutionUpdateEvent{ToolEventCall: ToolEventCall{ToolCallID: "c1", ToolName: "t"}, DetailsChanged: true, DiagnosticsChanged: true}},
		{ToolExecutionEndEvent{ToolEventCall: ToolEventCall{ToolCallID: "c1", ToolName: "t"}}},
		// Retired documents read as their initial values.
		{UsageChangedEvent{Usage: initialUsage}},
		{AgentChangedEvent{Agent: AgentState{}}},
	}
	var got [][]AgentEvent
	for _, batch := range log.all() {
		got = append(got, slices.DeleteFunc(batch, func(event AgentEvent) bool { return event.EventType() == "task_failed" }))
	}
	if len(got) != len(want) {
		t.Fatalf("batches = %d %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if gotJSON, wantJSON := jsonOf(t, got[i]), jsonOf(t, want[i]); gotJSON != wantJSON {
			t.Fatalf("batch %d = %s, want %s", i, gotJSON, wantJSON)
		}
	}
	// The tool events as pi's JSON has them.
	for i, wantJSON := range map[int]string{
		4: `[{"type":"tool_execution_start","toolCallId":"c1","toolName":"t","args":{}}]`,
		5: `[{"type":"tool_execution_update","toolCallId":"c1","toolName":"t","details":null,"diagnostics":[]}]`,
		6: `[{"type":"tool_execution_end","toolCallId":"c1","toolName":"t"}]`,
	} {
		if gotJSON := jsonOf(t, got[i]); gotJSON != wantJSON {
			t.Fatalf("batch %d = %s, want %s", i, gotJSON, wantJSON)
		}
	}
}

func TestEventsEndTheStreamWithTheHarness(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	stream, err := root.WatchEvents(bg)
	must(t, err)
	must(t, h.Close(bg))
	select {
	case <-stream.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
	if end := stream.End(); end != (durable.WatchEnd{Reason: durable.WatchSessionClosed}) {
		t.Fatalf("end = %+v, want session_closed", end)
	}
}

func TestEventsStartAMessageAtTheFirstCommittedPartialAndEndItWithTheConvertedEntryOnAbort(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 100, MinTokenSize: 1, MaxTokenSize: 1})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText(strings.Repeat("x", 400))})
	h, root := openChat(t, memory.New(), setup)
	_, log := listen(t, root)
	submission := submit(t, root, inputDraft("hi"))
	assistantStarts := func() int {
		count := 0
		for _, event := range log.events() {
			if e, ok := event.(MessageStartEvent); ok {
				if _, assistant := e.Message.(ai.AssistantMessage); assistant {
					count++
				}
			}
		}
		return count
	}
	eventually(t, func() bool { return assistantStarts() > 0 })
	_, err := h.AbortTask(bg, runTaskOf(t, h, root))
	must(t, err)
	waitFor(t, submission)
	log.quiet(t)
	ends := 0
	for _, event := range log.events() {
		if e, ok := event.(MessageEndEvent); ok && e.Entry.Kind == durable.AssistantEntry.Kind {
			ends++
		}
	}
	if ends != 1 || assistantStarts() != 1 {
		t.Fatalf("assistant starts %d, ends %d; want one of each", assistantStarts(), ends)
	}
}

// Beyond pi: no case of pi's commits while a partial is in flight without
// changing it. Such a commit sends no message_update.
func TestEventsSendNoMessageUpdateForACommitThatLeavesTheInFlightPartialUnchanged(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	_, log := listen(t, root)
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		live, err := liveDraft(tx, root.ID)
		if err != nil {
			return err
		}
		return setDraftField(live, "generation", LiveGeneration{Attempt: 1, Message: fauxAnswer("partial")})
	}))
	commitNote(t, root)
	log.quiet(t)
	batches := log.all()
	if len(batches) != 2 {
		t.Fatalf("batches = %d %v, want the partial's and the note's", len(batches), batches)
	}
	if got := eventTypes(batches[1]); !slices.Equal(got, []string{"entry_appended"}) {
		t.Fatalf("note batch = %v, want only entry_appended", got)
	}
}
