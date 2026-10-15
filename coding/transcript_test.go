package coding

import (
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

func fauxModel() protocol.ModelRef { return protocol.ModelRef{Provider: "faux", ID: "faux-1"} }

// assistantItem is pi's transcript.test.ts assistant fixture.
func assistantItem(id string, content ...protocol.Content) *protocol.AssistantTranscriptItem {
	return &protocol.AssistantTranscriptItem{
		ID:        id,
		Role:      "assistant",
		Content:   content,
		Status:    protocol.AssistantStreaming,
		Model:     fauxModel(),
		Timestamp: 1,
	}
}

func text(s string) *protocol.TextContent { return &protocol.TextContent{Type: "text", Text: s} }

func toolCall(input any) *protocol.ToolCallContent {
	return &protocol.ToolCallContent{Type: "toolCall", ToolCallID: "call-1", ToolName: "bash", Input: input}
}

// transcriptSnapshot is pi's transcript.test.ts snapshot fixture.
func transcriptSnapshot(revision int64, body string) *protocol.SessionSnapshot {
	return &protocol.SessionSnapshot{
		ID:               "session-1",
		Cwd:              "/workspace",
		CreatedAt:        1,
		UpdatedAt:        revision + 1,
		Phase:            protocol.PhaseTurn,
		Model:            fauxModel(),
		ThinkingLevel:    protocol.ThinkingOff,
		Attached:         true,
		Locked:           true,
		Revision:         revision,
		Transcript:       []protocol.TranscriptItem{assistantItem("assistant-1", text(body))},
		QueuedSteer:      []protocol.UserTranscriptItem{},
		QueuedSteerCount: 0,
	}
}

func textDelta(kind protocol.AssistantDeltaKind, index int64, delta string) *protocol.AssistantDeltaProgress {
	return &protocol.AssistantDeltaProgress{
		Type:         "assistant_delta",
		MessageID:    "assistant-1",
		ContentIndex: index,
		Kind:         kind,
		Delta:        delta,
	}
}

// firstText reads the text of the first content block of the item at index.
func firstText(t *testing.T, items []protocol.TranscriptItem, index int) string {
	t.Helper()
	if index >= len(items) {
		t.Fatalf("transcript has %d items, wanted index %d", len(items), index)
	}
	assistant, ok := items[index].(*protocol.AssistantTranscriptItem)
	if !ok {
		t.Fatalf("item %d is %T, wanted an assistant item", index, items[index])
	}
	block, ok := assistant.Content[0].(*protocol.TextContent)
	if !ok {
		t.Fatalf("content 0 is %T, wanted text", assistant.Content[0])
	}
	return block.Text
}

// firstToolInput reads the input of the first content block of the first item.
func firstToolInput(t *testing.T, items []protocol.TranscriptItem) any {
	t.Helper()
	assistant, ok := items[0].(*protocol.AssistantTranscriptItem)
	if !ok {
		t.Fatalf("item 0 is %T, wanted an assistant item", items[0])
	}
	block, ok := assistant.Content[0].(*protocol.ToolCallContent)
	if !ok {
		t.Fatalf("content 0 is %T, wanted a tool call", assistant.Content[0])
	}
	return block.Input
}

func TestTranscriptProjectsProgressWithoutMutatingSnapshot(t *testing.T) {
	state := NewTranscriptState(transcriptSnapshot(1, "saved"))
	state = state.ApplyProgress(textDelta(protocol.DeltaText, 0, " response"))

	if got := firstText(t, state.Snapshot().Transcript, 0); got != "saved" {
		t.Fatalf("authoritative snapshot = %q, want %q", got, "saved")
	}
	if got := firstText(t, state.Transcript(), 0); got != "saved response" {
		t.Fatalf("projection = %q, want %q", got, "saved response")
	}
}

func TestTranscriptDoesNotAliasTheCallersSnapshot(t *testing.T) {
	snapshot := transcriptSnapshot(1, "saved")
	state := NewTranscriptState(snapshot)

	// The producer keeps mutating what it handed over; the projection must not
	// see any of it. This is what pi buys with structuredClone.
	snapshot.Transcript[0].(*protocol.AssistantTranscriptItem).Content[0].(*protocol.TextContent).Text = "tampered"
	snapshot.Transcript[0] = assistantItem("assistant-2", text("injected"))
	snapshot.Revision = 99

	if got := firstText(t, state.Transcript(), 0); got != "saved" {
		t.Fatalf("projection = %q, want %q", got, "saved")
	}
	if got := state.Snapshot().Revision; got != 1 {
		t.Fatalf("revision = %d, want 1", got)
	}
}

func TestTranscriptAppliesStreamedToolCallDeltas(t *testing.T) {
	state := NewTranscriptState(transcriptSnapshot(1, "unused"))
	state = NewTranscriptState(func() *protocol.SessionSnapshot {
		s := transcriptSnapshot(1, "unused")
		s.Transcript = []protocol.TranscriptItem{assistantItem("assistant-1", toolCall(nil))}
		return s
	}())

	state = state.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, `{"command":`))
	if got := firstToolInput(t, state.Transcript()); got != `{"command":` {
		t.Fatalf("partial input = %#v, want the raw prefix", got)
	}

	// An item_updated resets the item but must not reset the buffered prefix.
	state = state.ApplyProgress(&protocol.ItemUpdatedProgress{
		Type: "item_updated",
		Item: assistantItem("assistant-1", toolCall(nil)),
	})
	state = state.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, `"pwd"}`))

	want := map[string]any{"command": "pwd"}
	if got := firstToolInput(t, state.Transcript()); !reflect.DeepEqual(got, want) {
		t.Fatalf("input = %#v, want %#v", got, want)
	}
}

func TestTranscriptResumesAPartialInputRestoredFromASnapshot(t *testing.T) {
	snapshot := transcriptSnapshot(1, "unused")
	snapshot.Transcript = []protocol.TranscriptItem{assistantItem("assistant-1", toolCall(`{"command":`))}
	state := NewTranscriptState(snapshot)

	state = state.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, `"pwd"}`))

	want := map[string]any{"command": "pwd"}
	if got := firstToolInput(t, state.Transcript()); !reflect.DeepEqual(got, want) {
		t.Fatalf("input = %#v, want %#v", got, want)
	}
}

func TestTranscriptDoesNotResumeFromAParsedInput(t *testing.T) {
	// A non-string input is a completed parse, not a prefix: resuming from it
	// would concatenate a rendering with a raw fragment.
	snapshot := transcriptSnapshot(1, "unused")
	snapshot.Transcript = []protocol.TranscriptItem{
		assistantItem("assistant-1", toolCall(map[string]any{"command": "ls"})),
	}
	state := NewTranscriptState(snapshot)

	state = state.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, `{"command":"pwd"}`))

	want := map[string]any{"command": "pwd"}
	if got := firstToolInput(t, state.Transcript()); !reflect.DeepEqual(got, want) {
		t.Fatalf("input = %#v, want %#v", got, want)
	}
}

func TestTranscriptClearsToolCallBuffersWhenAnItemFinishes(t *testing.T) {
	snapshot := transcriptSnapshot(1, "unused")
	snapshot.Transcript = []protocol.TranscriptItem{assistantItem("assistant-1", toolCall(nil))}
	state := NewTranscriptState(snapshot)
	state = state.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, `{"a":`))

	stop := protocol.StopToolUse
	finished := assistantItem("assistant-1", toolCall(nil))
	finished.Status = protocol.AssistantComplete
	finished.StopReason = &stop
	state = state.ApplyProgress(&protocol.ItemFinishedProgress{Type: "item_finished", Item: finished})

	// With the buffer forgotten, the next delta starts a fresh prefix rather
	// than appending to the spent one.
	state = state.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, `{"b":1}`))
	want := map[string]any{"b": float64(1)}
	if got := firstToolInput(t, state.Transcript()); !reflect.DeepEqual(got, want) {
		t.Fatalf("input = %#v, want %#v", got, want)
	}
}

func TestTranscriptResumesFromAnEmptyBufferItAlreadyHas(t *testing.T) {
	// A buffered prefix that happens to be empty is still a buffered prefix: pi
	// reads the map with ?? and falls back to the item's own input only when the
	// key is absent. Falling back on emptiness instead would splice an input the
	// projection has already superseded onto the front of the stream.
	snapshot := transcriptSnapshot(1, "unused")
	snapshot.Transcript = []protocol.TranscriptItem{assistantItem("assistant-1", toolCall(nil))}
	state := NewTranscriptState(snapshot)

	// A zero-length delta buffers "" for this block.
	state = state.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, ""))
	// The item is then replaced by one carrying a partial input of its own, which
	// the live buffer outranks.
	state = state.ApplyProgress(&protocol.ItemUpdatedProgress{
		Type: "item_updated",
		Item: assistantItem("assistant-1", toolCall(`{"a":`)),
	})
	state = state.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, `1}`))

	if got := firstToolInput(t, state.Transcript()); got != `1}` {
		t.Fatalf("input = %#v, want the buffered prefix to have won", got)
	}
}

func TestTranscriptZeroValueProjectsWithoutPanicking(t *testing.T) {
	// TranscriptState and its methods are exported, so its zero value has to be
	// an empty projection rather than a panic waiting for the first delta.
	var state TranscriptState

	next := state.ApplyProgress(&protocol.ItemStartedProgress{
		Type: "item_started",
		Item: assistantItem("assistant-1", toolCall(nil)),
	})
	if got := len(next.Transcript()); got != 1 {
		t.Fatalf("transcript has %d items, want the one progress introduced", got)
	}
	next = next.ApplyProgress(textDelta(protocol.DeltaToolCall, 0, `{"a":1}`))

	want := map[string]any{"a": float64(1)}
	if got := firstToolInput(t, next.Transcript()); !reflect.DeepEqual(got, want) {
		t.Fatalf("input = %#v, want %#v", got, want)
	}
	if got := len(state.Transcript()); got != 0 {
		t.Fatalf("the zero state itself has %d items, want none", got)
	}
}

func TestTranscriptDoesNotAliasACallerSuppliedProgressItem(t *testing.T) {
	// The producer keeps mutating the item it handed over, exactly as it may with
	// a snapshot. Only ingest is copied, so this is the copy that has to survive
	// splitting it from the items the projection builds for itself.
	state := NewTranscriptState(transcriptSnapshot(1, "saved"))
	item := assistantItem("assistant-2", text("streaming"))
	state = state.ApplyProgress(&protocol.ItemStartedProgress{Type: "item_started", Item: item})

	item.Content[0].(*protocol.TextContent).Text = "tampered"
	item.Content = append(item.Content, text("extra"))

	if got := firstText(t, state.Transcript(), 1); got != "streaming" {
		t.Fatalf("projection = %q, want %q", got, "streaming")
	}
	if got := len(state.Transcript()[1].(*protocol.AssistantTranscriptItem).Content); got != 1 {
		t.Fatalf("projected content has %d blocks, want 1", got)
	}
}

func TestTranscriptAppendsThinkingDeltas(t *testing.T) {
	snapshot := transcriptSnapshot(1, "unused")
	snapshot.Transcript = []protocol.TranscriptItem{
		assistantItem("assistant-1", &protocol.ThinkingContent{Type: "thinking", Thinking: "step"}),
	}
	state := NewTranscriptState(snapshot)
	state = state.ApplyProgress(textDelta(protocol.DeltaThinking, 0, " one"))

	item := state.Transcript()[0].(*protocol.AssistantTranscriptItem)
	thinking, ok := item.Content[0].(*protocol.ThinkingContent)
	if !ok {
		t.Fatalf("content 0 is %T, wanted thinking", item.Content[0])
	}
	if thinking.Thinking != "step one" {
		t.Fatalf("thinking = %q, want %q", thinking.Thinking, "step one")
	}
}

func TestTranscriptIgnoresDeltasItCannotApply(t *testing.T) {
	base := NewTranscriptState(transcriptSnapshot(1, "saved"))

	cases := []struct {
		name     string
		progress protocol.TranscriptProgress
		want     string
	}{
		{"unknown message", &protocol.AssistantDeltaProgress{
			Type: "assistant_delta", MessageID: "missing", ContentIndex: 0,
			Kind: protocol.DeltaText, Delta: "x",
		}, "saved"},
		{"index out of range", textDelta(protocol.DeltaText, 7, "x"), "saved"},
		{"kind does not match the block", textDelta(protocol.DeltaThinking, 0, "x"), "saved"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state := base.ApplyProgress(testCase.progress)
			if got := firstText(t, state.Transcript(), 0); got != testCase.want {
				t.Fatalf("projection = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestTranscriptIgnoresDeltasForNonAssistantItems(t *testing.T) {
	snapshot := transcriptSnapshot(1, "saved")
	snapshot.Transcript = []protocol.TranscriptItem{&protocol.UserTranscriptItem{
		ID: "assistant-1", Role: "user", Content: []protocol.Content{text("hi")}, Timestamp: 1,
	}}
	state := NewTranscriptState(snapshot)

	if next := state.ApplyProgress(textDelta(protocol.DeltaText, 0, "x")); next != state {
		t.Fatal("a delta naming a non-assistant item must leave the state untouched")
	}
}

func TestTranscriptAppendsTransientToolProgressAndReplacesItByID(t *testing.T) {
	toolItem := func(content ...protocol.Content) *protocol.ToolTranscriptItem {
		return &protocol.ToolTranscriptItem{
			ID:         "tool-call-1",
			Role:       "tool",
			ToolCallID: "call-1",
			ToolName:   "bash",
			Input:      map[string]any{"command": "printf hi"},
			Content:    content,
			Status:     protocol.ToolRunning,
			IsError:    false,
			Timestamp:  2,
		}
	}
	state := NewTranscriptState(transcriptSnapshot(1, "saved"))
	state = state.ApplyProgress(&protocol.ItemStartedProgress{Type: "item_started", Item: toolItem()})

	items := state.Transcript()
	if len(items) != 2 {
		t.Fatalf("transcript has %d items, want 2", len(items))
	}
	running := items[1].(*protocol.ToolTranscriptItem)
	if running.ID != "tool-call-1" || running.Status != protocol.ToolRunning || len(running.Content) != 0 {
		t.Fatalf("appended item = %+v", running)
	}

	state = state.ApplyProgress(&protocol.ItemUpdatedProgress{
		Type: "item_updated",
		Item: toolItem(&protocol.TextContent{Type: "text", Text: "hi"}),
	})

	items = state.Transcript()
	if len(items) != 2 {
		t.Fatalf("transcript has %d items, want 2 — an update must replace, not append", len(items))
	}
	updated := items[1].(*protocol.ToolTranscriptItem)
	if len(updated.Content) != 1 || updated.Content[0].(*protocol.TextContent).Text != "hi" {
		t.Fatalf("updated item content = %+v", updated.Content)
	}
}

func TestTranscriptResetsWhenTheSameSessionRuntimeIsReacquired(t *testing.T) {
	state := NewTranscriptState(transcriptSnapshot(50, "old runtime"))
	state = NewTranscriptState(transcriptSnapshot(0, "new runtime"))

	if got := state.Snapshot().Revision; got != 0 {
		t.Fatalf("revision = %d, want 0", got)
	}
	if got := firstText(t, state.Transcript(), 0); got != "new runtime" {
		t.Fatalf("projection = %q, want %q", got, "new runtime")
	}
}

func TestTranscriptAcceptsALowerRevisionFromADifferentSession(t *testing.T) {
	state := NewTranscriptState(transcriptSnapshot(50, "old session"))
	next := transcriptSnapshot(0, "new session")
	next.ID = "session-2"
	state = state.ApplySnapshot(next)

	if got := state.Snapshot().ID; got != "session-2" {
		t.Fatalf("id = %q, want session-2", got)
	}
	if got := firstText(t, state.Transcript(), 0); got != "new session" {
		t.Fatalf("projection = %q, want %q", got, "new session")
	}
}

func TestTranscriptRendersAcceptedSteeringMessages(t *testing.T) {
	snapshot := transcriptSnapshot(2, "saved")
	snapshot.QueuedSteerCount = 1
	snapshot.QueuedSteer = []protocol.UserTranscriptItem{{
		ID:        "user-steer",
		Role:      "user",
		Content:   []protocol.Content{text("adjust the approach")},
		Timestamp: 2,
	}}
	state := NewTranscriptState(snapshot)

	items := state.Transcript()
	queued, ok := items[len(items)-1].(*protocol.UserTranscriptItem)
	if !ok {
		t.Fatalf("last item is %T, wanted a user item", items[len(items)-1])
	}
	if got := queued.Content[0].(*protocol.TextContent).Text; got != "adjust the approach" {
		t.Fatalf("queued steer = %q", got)
	}
}

func TestTranscriptSkipsAQueuedSteerAlreadyInTheTranscript(t *testing.T) {
	snapshot := transcriptSnapshot(2, "saved")
	steer := protocol.UserTranscriptItem{
		ID: "user-steer", Role: "user", Content: []protocol.Content{text("adjust")}, Timestamp: 2,
	}
	snapshot.Transcript = append(snapshot.Transcript, &steer)
	snapshot.QueuedSteer = []protocol.UserTranscriptItem{steer}
	snapshot.QueuedSteerCount = 1
	state := NewTranscriptState(snapshot)

	if got := len(state.Transcript()); got != 2 {
		t.Fatalf("transcript has %d items, want 2", got)
	}
}

func TestTranscriptNewerSnapshotIsAuthoritativeAndStaleOnesAreIgnored(t *testing.T) {
	state := NewTranscriptState(transcriptSnapshot(3, "new"))
	state = state.ApplyProgress(textDelta(protocol.DeltaText, 0, " transient"))
	state = state.ApplySnapshot(transcriptSnapshot(4, "authoritative"))
	stale := state.ApplySnapshot(transcriptSnapshot(2, "stale"))

	if stale != state {
		t.Fatal("a stale snapshot must be dropped, not folded in")
	}
	if got := state.Snapshot().Revision; got != 4 {
		t.Fatalf("revision = %d, want 4", got)
	}
	if got := firstText(t, state.Transcript(), 0); got != "authoritative" {
		t.Fatalf("projection = %q, want %q", got, "authoritative")
	}
}

func TestTranscriptProgressOrderSurvivesUpdates(t *testing.T) {
	state := NewTranscriptState(transcriptSnapshot(1, "saved"))
	for _, id := range []string{"tool-1", "tool-2"} {
		state = state.ApplyProgress(&protocol.ItemStartedProgress{Type: "item_started", Item: &protocol.ToolTranscriptItem{
			ID: id, Role: "tool", ToolCallID: id, ToolName: "bash", Input: nil,
			Content: []protocol.Content{}, Status: protocol.ToolRunning, Timestamp: 2,
		}})
	}
	// Updating the first one must not move it behind the second.
	state = state.ApplyProgress(&protocol.ItemUpdatedProgress{Type: "item_updated", Item: &protocol.ToolTranscriptItem{
		ID: "tool-1", Role: "tool", ToolCallID: "tool-1", ToolName: "bash", Input: nil,
		Content: []protocol.Content{text("done")}, Status: protocol.ToolRunning, Timestamp: 3,
	}})

	got := []string{}
	for _, item := range state.Transcript() {
		got = append(got, transcriptItemID(item))
	}
	want := []string{"assistant-1", "tool-1", "tool-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestTranscriptIgnoresUnknownProgress(t *testing.T) {
	state := NewTranscriptState(transcriptSnapshot(1, "saved"))
	// protocol.TranscriptProgress is a sealed union, so no out-of-package
	// variant can be constructed to reach the default arm. A nil progress is
	// what remains of that case, and it must be as inert as pi's miss.
	if next := state.ApplyProgress(nil); next != state {
		t.Fatal("an unrecognized progress variant must leave the state untouched")
	}
}

func TestParsePartialToolInput(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  any
	}{
		{"incomplete object stays raw", `{"command":`, `{"command":`},
		{"complete object parses", `{"a":1}`, map[string]any{"a": float64(1)}},
		{"array parses", `[1,"x"]`, []any{float64(1), "x"}},
		{"bare string parses", `"abc"`, "abc"},
		{"null parses", `null`, nil},
		{"empty stays raw", ``, ``},
		{"trailing garbage stays raw", `{} {}`, `{} {}`},
		// JSON.parse turns this into Infinity, which pi's isJsonValue rejects;
		// encoding/json refuses it outright, reaching the same raw prefix.
		{"overflowing number stays raw", `1e999`, `1e999`},
		{"NaN stays raw", `NaN`, `NaN`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parsePartialToolInput(testCase.value); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("parsePartialToolInput(%q) = %#v, want %#v", testCase.value, got, testCase.want)
			}
		})
	}
}

// TestTranscriptItemIDReadsEveryRole covers the roles rather than the pointer
// and value forms it used to: protocol's union markers now take pointer
// receivers, so the value form no longer satisfies TranscriptItem and the
// normalization this once exercised is a compile-time guarantee.
func TestTranscriptItemIDReadsEveryRole(t *testing.T) {
	cases := []struct {
		name string
		item protocol.TranscriptItem
	}{
		{"user", &protocol.UserTranscriptItem{ID: "x"}},
		{"assistant", &protocol.AssistantTranscriptItem{ID: "x"}},
		{"tool", &protocol.ToolTranscriptItem{ID: "x"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := transcriptItemID(testCase.item); got != "x" {
				t.Fatalf("transcriptItemID = %q, want x", got)
			}
		})
	}
}

func TestCloneJSONValueIsDeep(t *testing.T) {
	original := map[string]any{"list": []any{map[string]any{"n": float64(1)}}}
	clone := cloneJSONValue(original).(map[string]any)
	clone["list"].([]any)[0].(map[string]any)["n"] = float64(2)

	if got := original["list"].([]any)[0].(map[string]any)["n"]; got != float64(1) {
		t.Fatalf("original mutated: n = %v", got)
	}
}
