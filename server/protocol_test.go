package server_test

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server"
)

var bridgeModel = ai.Model{
	ID:            "model-1",
	Name:          "Model One",
	Api:           "test-api",
	Provider:      "test-provider",
	BaseURL:       "https://example.test",
	Reasoning:     true,
	Input:         []string{"text", "image"},
	Cost:          ai.ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0.2},
	ContextWindow: 100_000,
	MaxTokens:     10_000,
}

// assertServerPayload encodes the item inside a real server message, which is
// what proves the bridge produces something a peer will actually accept: the
// protocol schemas are enforced by the encoder, not by this package.
func assertServerPayload(t *testing.T, item protocol.TranscriptItem) {
	t.Helper()
	metadata, err := server.ToProtocolModelMetadata(&bridgeModel, true)
	if err != nil {
		t.Fatalf("model metadata: %v", err)
	}
	// The snapshot keeps the full live shape; only the server snapshot's
	// session list narrowed to durable metadata in 6189e53b3.
	snapshot := protocol.SessionSnapshot{
		ID:            "session-1",
		Cwd:           "/workspace",
		CreatedAt:     1,
		UpdatedAt:     1,
		Phase:         protocol.PhaseIdle,
		Model:         protocol.ModelRef{Provider: "test-provider", ID: "model-1"},
		ThinkingLevel: protocol.ThinkingOff,
		Attached:      true,
		Locked:        true,
		Revision:      1,
		Transcript:    []protocol.TranscriptItem{item},
	}
	// Written out rather than projected from the snapshot: feeding
	// snapshot.Metadata() in here would make the encode self-consistent under
	// any mutation of the projection.
	metaUpdated := int64(1)
	metaCwd := "/workspace"
	wantMetadata := protocol.SessionMetadata{
		ID: "session-1", CreatedAt: 1, UpdatedAt: &metaUpdated, Cwd: &metaCwd,
	}
	if got := snapshot.Metadata(); !reflect.DeepEqual(got, wantMetadata) {
		t.Fatalf("Metadata() = %+v, want %+v", got, wantMetadata)
	}

	hello := &protocol.ServerHello{
		Type:         "hello",
		Version:      protocol.ProtocolVersion,
		ConnectionID: "connection-1",
		Snapshot: protocol.ServerSnapshot{
			ServerID:        "server-1",
			ProtocolVersion: protocol.ProtocolVersion,
			Revision:        0,
			Sessions:        []protocol.SessionMetadata{wantMetadata},
			Models:          []protocol.ModelMetadata{*metadata},
		},
	}
	if _, err := protocol.EncodeServerMessage(hello, nil); err != nil {
		t.Fatalf("encode hello: %v", err)
	}

	event := &protocol.EventEnvelope{
		Type:  "event",
		Event: &protocol.SessionSnapshotEvent{Type: "session_snapshot", Snapshot: snapshot},
	}
	if _, err := protocol.EncodeServerMessage(event, nil); err != nil {
		t.Fatalf("encode session snapshot: %v", err)
	}
}

func TestModelMetadataBridge(t *testing.T) {
	t.Parallel()
	metadata, err := server.ToProtocolModelMetadata(&bridgeModel, true)
	if err != nil {
		t.Fatalf("model metadata: %v", err)
	}
	if metadata.Provider != "test-provider" || metadata.ID != "model-1" || metadata.API != "test-api" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if len(metadata.Input) != 2 || metadata.Input[0] != protocol.InputText || metadata.Input[1] != protocol.InputImage {
		t.Fatalf("input = %#v", metadata.Input)
	}
	if !metadata.Authenticated {
		t.Fatal("authenticated must be carried through")
	}
	if !slices.Contains(metadata.SupportedThinkingLevels, protocol.ThinkingOff) {
		t.Fatalf("supported thinking levels = %#v", metadata.SupportedThinkingLevels)
	}
	if err := metadata.Validate(); err != nil {
		t.Fatalf("the mapped metadata must satisfy the protocol schema: %v", err)
	}

	// Sizes are clamped rather than allowed to reach the wire as zero, which
	// the schema would reject.
	degenerate := bridgeModel
	degenerate.ContextWindow = 0
	degenerate.MaxTokens = -5
	degenerate.Cost = ai.ModelCost{Input: -1, Output: math.Inf(1)}
	clamped, err := server.ToProtocolModelMetadata(&degenerate, false)
	if err != nil {
		t.Fatalf("model metadata: %v", err)
	}
	if clamped.ContextWindow != 1 || clamped.MaxTokens != 1 {
		t.Fatalf("sizes = %d/%d, want 1/1", clamped.ContextWindow, clamped.MaxTokens)
	}
	if clamped.Cost.Input != 0 || clamped.Cost.Output != 0 {
		t.Fatalf("costs = %#v, want zeroes", clamped.Cost)
	}

	missing := bridgeModel
	missing.Provider = ""
	if _, err := server.ToProtocolModelMetadata(&missing, true); err == nil {
		t.Fatal("a model without a provider must be rejected")
	}
}

func TestAssistantContentAndStopReasons(t *testing.T) {
	t.Parallel()
	message := ai.AssistantMessage{
		Content: ai.ContentList{
			ai.TextContent{Text: "hello"},
			ai.ThinkingContent{Thinking: "hmm"},
			ai.ToolCall{ID: "call-1", Name: "read", Arguments: map[string]any{"path": "README.md"}},
		},
		Api:        "test-api",
		Provider:   "test-provider",
		Model:      "model-1",
		Usage:      ai.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, TotalTokens: 10},
		StopReason: ai.StopToolUse,
		Timestamp:  123,
	}

	item, err := server.ToProtocolAssistantMessage(message, "message-1")
	if err != nil {
		t.Fatalf("assistant: %v", err)
	}
	if item.Status != protocol.AssistantComplete || item.StopReason == nil || *item.StopReason != protocol.StopToolUse {
		t.Fatalf("status/stopReason = %q/%v", item.Status, item.StopReason)
	}
	if item.Model.Provider != "test-provider" || item.Model.ID != "model-1" {
		t.Fatalf("model = %#v", item.Model)
	}
	if len(item.Content) != 3 {
		t.Fatalf("content = %#v", item.Content)
	}
	text, ok := item.Content[0].(*protocol.TextContent)
	if !ok || text.Text != "hello" {
		t.Fatalf("content[0] = %#v", item.Content[0])
	}
	thinking, ok := item.Content[1].(*protocol.ThinkingContent)
	if !ok || thinking.Thinking != "hmm" || thinking.Redacted != nil {
		t.Fatalf("content[1] = %#v", item.Content[1])
	}
	call, ok := item.Content[2].(*protocol.ToolCallContent)
	if !ok || call.ToolCallID != "call-1" || call.ToolName != "read" {
		t.Fatalf("content[2] = %#v", item.Content[2])
	}
	input, ok := call.Input.(map[string]any)
	if !ok || input["path"] != "README.md" {
		t.Fatalf("tool input = %#v", call.Input)
	}
	assertServerPayload(t, item)

	for _, testCase := range []struct {
		reason ai.StopReason
		status protocol.AssistantStatus
		stop   *protocol.StopReason
	}{
		{ai.StopPending, protocol.AssistantStreaming, nil},
		{ai.StopStop, protocol.AssistantComplete, ptr(protocol.StopStop)},
		{ai.StopLength, protocol.AssistantComplete, ptr(protocol.StopLength)},
		{ai.StopToolUse, protocol.AssistantComplete, ptr(protocol.StopToolUse)},
		{ai.StopError, protocol.AssistantError, ptr(protocol.StopError)},
		{ai.StopAborted, protocol.AssistantAborted, ptr(protocol.StopAborted)},
	} {
		variant := message
		variant.StopReason = testCase.reason
		mapped, err := server.ToProtocolAssistantMessage(variant, "message-1")
		if err != nil {
			t.Fatalf("stop reason %q: %v", testCase.reason, err)
		}
		if mapped.Status != testCase.status {
			t.Fatalf("stop reason %q mapped to status %q, want %q", testCase.reason, mapped.Status, testCase.status)
		}
		switch {
		case testCase.stop == nil && mapped.StopReason != nil:
			t.Fatalf("a streaming item must not carry a stopReason, got %q", *mapped.StopReason)
		case testCase.stop != nil && (mapped.StopReason == nil || *mapped.StopReason != *testCase.stop):
			t.Fatalf("stopReason = %v, want %q", mapped.StopReason, *testCase.stop)
		}
		assertServerPayload(t, mapped)
	}

	unknown := message
	unknown.StopReason = "not-a-stop-reason"
	if _, err := server.ToProtocolAssistantMessage(unknown, "message-1"); err == nil {
		t.Fatal("an unknown stop reason must be rejected rather than guessed at")
	}

	// pi 382aa641c: protocol v1 has no deferred status, and upstream refuses
	// the message rather than putting an unrepresentable reason on the wire.
	deferred := message
	deferred.StopReason = ai.StopDeferred
	deferred.Deferred = &ai.DeferredHandle{Provider: "test", ModelID: "model-1", Api: "api", ID: "resp-1"}
	_, err = server.ToProtocolAssistantMessage(deferred, "message-1")
	if err == nil || !strings.Contains(err.Error(), "not supported by protocol v1") {
		t.Fatalf("a deferred message must be refused by protocol v1, got %v", err)
	}
}

func ptr[T any](value T) *T { return &value }

func TestAssistantErrorMessages(t *testing.T) {
	t.Parallel()
	message := ai.AssistantMessage{
		Api: "test-api", Provider: "test-provider", Model: "model-1",
		StopReason: ai.StopError, Timestamp: 123,
	}

	withoutMessage, err := server.ToProtocolAssistantMessage(message, "message-error")
	if err != nil {
		t.Fatalf("assistant: %v", err)
	}
	if withoutMessage.Status != protocol.AssistantError || withoutMessage.ErrorMessage != nil {
		t.Fatalf("item = %#v", withoutMessage)
	}
	assertServerPayload(t, withoutMessage)

	// An empty error message is indistinguishable from an absent one in Go, and
	// the schema forbids the empty string on this arm — so it must be omitted.
	empty := message
	empty.ErrorMessage = ""
	omitted, err := server.ToProtocolAssistantMessage(empty, "message-error")
	if err != nil {
		t.Fatalf("assistant: %v", err)
	}
	if omitted.ErrorMessage != nil {
		t.Fatalf("errorMessage = %q, want it omitted", *omitted.ErrorMessage)
	}
	assertServerPayload(t, omitted)

	withMessage := message
	withMessage.ErrorMessage = "failed"
	mapped, err := server.ToProtocolAssistantMessage(withMessage, "message-error")
	if err != nil {
		t.Fatalf("assistant: %v", err)
	}
	if mapped.ErrorMessage == nil || *mapped.ErrorMessage != "failed" {
		t.Fatalf("errorMessage = %v", mapped.ErrorMessage)
	}
	assertServerPayload(t, mapped)
}

func TestUserAndToolMessages(t *testing.T) {
	t.Parallel()
	user := ai.NewUserText("hello", 1)
	userItem, err := server.ToProtocolUserMessage(user, "user-1")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if len(userItem.Content) != 1 {
		t.Fatalf("content = %#v", userItem.Content)
	}
	if text, ok := userItem.Content[0].(*protocol.TextContent); !ok || text.Text != "hello" {
		t.Fatalf("content[0] = %#v", userItem.Content[0])
	}
	assertServerPayload(t, userItem)

	circular := map[string]any{}
	circular["self"] = circular
	call := ai.ToolCall{ID: "call-1", Name: "read", Arguments: map[string]any{"path": "README.md"}}
	result := ai.ToolResultMessage{
		ToolCallID: "call-1",
		ToolName:   "read",
		Content:    ai.ContentList{ai.TextContent{Text: "result"}},
		Details:    circular,
		Timestamp:  2,
	}

	toolItem, err := server.ToProtocolToolResultMessage(result, "tool-1", call)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if toolItem.Status != protocol.ToolComplete || toolItem.IsError {
		t.Fatalf("item = %#v", toolItem)
	}
	details, ok := (*toolItem.Details).(map[string]any)
	if !ok || details["self"] != "[Circular]" {
		t.Fatalf("details = %#v, want the cycle replaced", toolItem.Details)
	}
	assertServerPayload(t, toolItem)

	failed := result
	failed.Details = nil
	failed.IsError = true
	errorItem, err := server.ToProtocolToolResultMessage(failed, "tool-2", call)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if errorItem.Status != protocol.ToolError || !errorItem.IsError {
		t.Fatalf("item = %#v", errorItem)
	}
	assertServerPayload(t, errorItem)
}

// pi has carried usage on a tool result since 2026-05-04 and puts it on the
// wire when it is there. The decoded form is what a caller receives from a
// session file or an SDK, so that is what is mapped here.
func TestToolResultUsageReachesTheWire(t *testing.T) {
	t.Parallel()
	const encoded = `{
		"role": "toolResult",
		"toolCallId": "call-1",
		"toolName": "read",
		"content": [{"type": "text", "text": "result"}],
		"usage": {"input": 3, "output": 5, "cacheRead": 1, "cacheWrite": 2, "totalTokens": 8,
			"cost": {"input": 0.5, "output": 0.25, "cacheRead": 0, "cacheWrite": 0, "total": 0.75}},
		"isError": false,
		"timestamp": 2
	}`
	var message ai.ToolResultMessage
	if err := json.Unmarshal([]byte(encoded), &message); err != nil {
		t.Fatalf("decode: %v", err)
	}

	call := ai.ToolCall{ID: "call-1", Name: "read", Arguments: map[string]any{"path": "README.md"}}
	item, err := server.ToProtocolToolResultMessage(message, "tool-1", call)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if item.Usage == nil {
		t.Fatal("the tool result's usage never reached the transcript item")
	}
	if item.Usage.Input != 3 || item.Usage.Output != 5 || item.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %#v", item.Usage)
	}
	if item.Usage.Cost.Total != 0.75 {
		t.Fatalf("cost = %#v", item.Usage.Cost)
	}
	assertServerPayload(t, item)

	// Nothing populates it today, so a result without usage must stay exactly
	// as it was — the field is absent from the frame, not null in it.
	const withoutUsage = `{
		"role": "toolResult",
		"toolCallId": "call-1",
		"toolName": "read",
		"content": [{"type": "text", "text": "result"}],
		"isError": false,
		"timestamp": 2
	}`
	var bare ai.ToolResultMessage
	if err := json.Unmarshal([]byte(withoutUsage), &bare); err != nil {
		t.Fatalf("decode: %v", err)
	}
	without, err := server.ToProtocolToolResultMessage(bare, "tool-1", call)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if without.Usage != nil {
		t.Fatalf("usage = %#v, want it omitted", without.Usage)
	}
	assertServerPayload(t, without)
}

// A tool result is only meaningful against the call it answers; a mismatch is a
// bug in the caller, not something to relabel.
func TestToolResultMustMatchItsCall(t *testing.T) {
	t.Parallel()
	call := ai.ToolCall{ID: "call-1", Name: "read", Arguments: map[string]any{"path": "README.md"}}
	base := ai.ToolResultMessage{
		ToolCallID: "call-2",
		ToolName:   "read",
		Content:    ai.ContentList{ai.TextContent{Text: "result"}},
		Timestamp:  2,
	}
	if _, err := server.ToProtocolToolResultMessage(base, "tool-1", call); err == nil ||
		!strings.Contains(err.Error(), "tool call") {
		t.Fatalf("a mismatched call id must be rejected, got %v", err)
	}

	renamed := base
	renamed.ToolCallID = "call-1"
	renamed.ToolName = "write"
	if _, err := server.ToProtocolToolResultMessage(renamed, "tool-1", call); err == nil ||
		!strings.Contains(err.Error(), "tool call") {
		t.Fatalf("a mismatched tool name must be rejected, got %v", err)
	}
}

// A pointer that refers to itself is a cycle the seen-set never sees: nothing
// in the chain is a slice or a map, so the walk never reaches the point where
// cycles are detected. pi cannot express this value at all; Go can, and both
// converters must still terminate.
func TestSelfReferentialPointerTerminates(t *testing.T) {
	t.Parallel()
	var value any
	value = &value

	finished := make(chan error, 1)
	go func() {
		_, err := server.ToProtocolJSONValue(value)
		finished <- err
	}()
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "circular") {
			t.Fatalf("error = %v, want a circular-reference refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ToProtocolJSONValue never returned for a self-referential pointer")
	}

	sanitized := make(chan *any, 1)
	go func() { sanitized <- server.SanitizeProtocolDetails(value) }()
	select {
	case details := <-sanitized:
		if details == nil || *details != "[Circular]" {
			t.Fatalf("details = %v, want the cycle replaced", details)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SanitizeProtocolDetails never returned for a self-referential pointer")
	}
}

func TestInvalidIdentifiersAndTimestamps(t *testing.T) {
	t.Parallel()
	message := ai.AssistantMessage{
		Content:    ai.ContentList{ai.ToolCall{ID: "", Name: "read", Arguments: map[string]any{}}},
		Api:        "test-api",
		Provider:   "test-provider",
		Model:      "model-1",
		StopReason: ai.StopToolUse,
		Timestamp:  1,
	}
	if _, err := server.ToProtocolAssistantMessage(message, "assistant-1"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "tool call id") {
		t.Fatalf("an empty tool call id must be rejected, got %v", err)
	}

	user := ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "hello"}}, Timestamp: -1}
	if _, err := server.ToProtocolUserMessage(user, "user-1"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "timestamp") {
		t.Fatalf("a negative timestamp must be rejected, got %v", err)
	}
	// pi's timestamps are JavaScript numbers, so anything past 2^53-1 is a
	// value the peer cannot round-trip. Int64 can hold it; the wire cannot.
	unsafe := ai.UserMessage{
		Content:   ai.ContentList{ai.TextContent{Text: "hello"}},
		Timestamp: 1<<53 + 1,
	}
	if _, err := server.ToProtocolUserMessage(unsafe, "user-1"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "timestamp") {
		t.Fatalf("a timestamp beyond the safe integer range must be rejected, got %v", err)
	}
	if _, err := server.ToProtocolUserMessage(ai.NewUserText("hello", 1), ""); err == nil {
		t.Fatal("an empty transcript item id must be rejected")
	}
}

func TestToProtocolJSONValueRejectsLossyConversions(t *testing.T) {
	t.Parallel()
	circular := map[string]any{}
	circular["self"] = circular

	for name, value := range map[string]any{
		"positive infinity": math.Inf(1),
		"negative infinity": math.Inf(-1),
		"not a number":      math.NaN(),
		"circular":          circular,
		"channel":           make(chan int),
		"non-string keys":   map[int]any{1: "one"},
	} {
		if _, err := server.ToProtocolJSONValue(value); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}

	// A cycle through a slice is caught too, not only one through a map.
	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice
	if _, err := server.ToProtocolJSONValue(cyclicSlice); err == nil {
		t.Fatal("a slice cycle must be rejected")
	}

	// A repeated but acyclic reference is not a cycle and must survive.
	shared := map[string]any{"n": 1}
	value, err := server.ToProtocolJSONValue([]any{shared, shared})
	if err != nil {
		t.Fatalf("a shared reference must be accepted: %v", err)
	}
	entries, ok := value.([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("value = %#v", value)
	}

	// nil inside a slice is a JSON null, which pi accepts as an explicit null.
	value, err = server.ToProtocolJSONValue([]any{nil, "value"})
	if err != nil {
		t.Fatalf("a null entry must be accepted: %v", err)
	}
	entries, _ = value.([]any)
	if len(entries) != 2 || entries[0] != nil || entries[1] != "value" {
		t.Fatalf("value = %#v", value)
	}

	// Every integer kind reaches the wire as a number, which the CBOR encoder
	// then writes back as an integer.
	value, err = server.ToProtocolJSONValue(map[string]any{"n": int32(7), "u": uint8(3)})
	if err != nil {
		t.Fatalf("integers must be accepted: %v", err)
	}
	object := value.(map[string]any)
	if object["n"] != float64(7) || object["u"] != float64(3) {
		t.Fatalf("value = %#v", object)
	}
}

func TestSanitizeProtocolDetails(t *testing.T) {
	t.Parallel()
	circular := map[string]any{}
	circular["self"] = circular
	if got := server.SanitizeProtocolDetails(circular); (*got).(map[string]any)["self"] != "[Circular]" {
		t.Fatalf("details = %#v", got)
	}

	// Details never affect execution, so a value that cannot be represented is
	// coerced instead of failing the turn.
	cases := []struct {
		name  string
		value any
		want  any
	}{
		{"nan", math.NaN(), "NaN"},
		{"positive infinity", math.Inf(1), "Infinity"},
		{"negative infinity", math.Inf(-1), "-Infinity"},
		{"time", time.Date(2020, 1, 2, 3, 4, 5, 678_000_000, time.UTC), "2020-01-02T03:04:05.678Z"},
		{"string", "plain", "plain"},
		{"bool", true, true},
		{"nil", nil, nil},
	}
	for _, testCase := range cases {
		got := server.SanitizeProtocolDetails(testCase.value)
		if testCase.want == nil {
			// A nil value is pi's undefined, and pi omits the property for
			// undefined while keeping it for null. Absent is a nil pointer.
			if got != nil {
				t.Fatalf("%s = %#v, want the property absent", testCase.name, *got)
			}
			continue
		}
		if got == nil || *got != testCase.want {
			t.Fatalf("%s = %#v, want %#v", testCase.name, got, testCase.want)
		}
	}

	// A value with nothing to say is dropped from an object and becomes null
	// inside an array, which is what pi does with undefined.
	object := server.SanitizeProtocolDetails(map[string]any{"fn": func() {}, "keep": "yes"})
	entries := (*object).(map[string]any)
	if _, present := entries["fn"]; present || entries["keep"] != "yes" {
		t.Fatalf("object = %#v", entries)
	}
	array := (*server.SanitizeProtocolDetails([]any{func() {}, "value"})).([]any)
	if len(array) != 2 || array[0] != nil || array[1] != "value" {
		t.Fatalf("array = %#v", array)
	}

	// A struct is Go's plain object; unexported fields are as invisible as
	// non-enumerable ones are to Object.entries.
	type detail struct {
		Kind   string
		Count  int
		hidden string
	}
	fields := (*server.SanitizeProtocolDetails(detail{Kind: "read", Count: 2, hidden: "x"})).(map[string]any)
	if fields["Kind"] != "read" || fields["Count"] != float64(2) {
		t.Fatalf("struct = %#v", fields)
	}
	if _, present := fields["hidden"]; present {
		t.Fatal("unexported fields must not reach the wire")
	}

	// Whatever it produces must be a legal protocol JSON value.
	if err := (&protocol.ToolTranscriptItem{
		ID: "tool-1", Role: "tool", ToolCallID: "call-1", ToolName: "read",
		Input: map[string]any{}, Timestamp: 1, Status: protocol.ToolComplete,
		Details: server.SanitizeProtocolDetails(map[string]any{
			"nested": []any{math.NaN(), map[string]any{"deep": true}},
		}),
	}).Validate(); err != nil {
		t.Fatalf("sanitized details must validate: %v", err)
	}
}

func TestUsageBridge(t *testing.T) {
	t.Parallel()
	if server.ToProtocolUsage(nil) != nil {
		t.Fatal("a missing usage must stay missing")
	}
	usage := server.ToProtocolUsage(&ai.Usage{
		Input: -5, Output: 2, CacheRead: 3, CacheWrite: 4, TotalTokens: 10,
		Cost: ai.CostBreakdown{Input: -1, Output: 0.5, Total: math.Inf(1)},
	})
	if usage.Input != 0 || usage.Output != 2 || usage.TotalTokens != 10 {
		t.Fatalf("usage = %#v", usage)
	}
	if usage.Cost.Input != 0 || usage.Cost.Output != 0.5 || usage.Cost.Total != 0 {
		t.Fatalf("cost = %#v", usage.Cost)
	}
	if usage.Reasoning != nil {
		t.Fatalf("an unreported reasoning count must be omitted, got %d", *usage.Reasoning)
	}
	reported := server.ToProtocolUsage(&ai.Usage{Reasoning: 7})
	if reported.Reasoning == nil || *reported.Reasoning != 7 {
		t.Fatalf("reasoning = %v", reported.Reasoning)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("mapped usage must validate: %v", err)
	}
}
