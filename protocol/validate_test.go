package protocol

import (
	"strings"
	"testing"
)

// TestJSONValueAcceptsEveryNumericKind: pi's isProtocolValue tests `typeof
// value === "number"`, which every Go numeric kind spells. Matching only
// int64/float64 rejected the literal a Go caller actually writes — Input: 5 —
// while the encoder was perfectly happy to put it on the wire, so a message
// could be built, refused by Validate, and yet be legal pi.
func TestJSONValueAcceptsEveryNumericKind(t *testing.T) {
	type namedString string

	accepted := []struct {
		name  string
		input any
	}{
		{"untyped_int_literal", 5},
		{"int8", int8(5)},
		{"uint64", uint64(5)},
		{"float32", float32(1.5)},
		{"int64", int64(5)},
		{"float64", 1.5},
		{"map_of_int", map[string]any{"n": 5}},
		{"slice_of_any", []any{1, 2}},
		{"typed_map", map[string]string{"a": "b"}},
		{"typed_slice", []int{1, 2}},
		{"named_string_keyed_map", map[namedString]int{"a": 1}},
		{"array", [2]int{1, 2}},
		{"nil", nil},
		{"nested", map[string]any{"a": []any{map[string]any{"b": 1}}}},
	}
	for _, test := range accepted {
		t.Run("accepts/"+test.name, func(t *testing.T) {
			content := &ToolCallContent{Type: "toolCall", ToolCallID: "tc1", ToolName: "bash", Input: test.input}
			if err := content.Validate(); err != nil {
				t.Errorf("Validate rejected a value the encoder accepts: %v", err)
			}
		})
	}

	rejected := []struct {
		name  string
		input any
	}{
		// pi's isProtocolValue refuses a Uint8Array, so a Go peer must not be
		// able to send one either.
		{"bytes", []byte{1, 2}},
		{"nested_bytes", map[string]any{"blob": []byte{1}}},
		// A non-string key has no object form on the wire.
		{"int_keyed_map", map[int]string{1: "a"}},
		// pi has no representation for these at all, and the encoder refuses
		// them too, so accepting them would only defer the failure.
		{"channel", make(chan int)},
		{"func", func() {}},
	}
	for _, test := range rejected {
		t.Run("rejects/"+test.name, func(t *testing.T) {
			content := &ToolCallContent{Type: "toolCall", ToolCallID: "tc1", ToolName: "bash", Input: test.input}
			if err := content.Validate(); err == nil {
				t.Error("Validate accepted a value pi has no representation for")
			}
		})
	}
}

// TestValidateRejectsTypedNilUnionMembers: a typed nil satisfies a union
// interface, so these messages type-check and used to panic inside Validate on
// the nil dereference. The decoder always allocates, so this is not reachable
// from the wire — it is reachable from the public API, which is now the only way
// to build a message, and a library that panics on a caller's nil is a worse
// bug than one that rejects it.
func TestValidateRejectsTypedNilUnionMembers(t *testing.T) {
	// Every union member, as the typed nil a caller can hold.
	members := map[string]Validator{
		"TextContent":             (*TextContent)(nil),
		"ThinkingContent":         (*ThinkingContent)(nil),
		"ImageContent":            (*ImageContent)(nil),
		"ToolCallContent":         (*ToolCallContent)(nil),
		"UserTranscriptItem":      (*UserTranscriptItem)(nil),
		"AssistantTranscriptItem": (*AssistantTranscriptItem)(nil),
		"ToolTranscriptItem":      (*ToolTranscriptItem)(nil),
		"ItemStartedProgress":     (*ItemStartedProgress)(nil),
		"AssistantDeltaProgress":  (*AssistantDeltaProgress)(nil),
		"ItemUpdatedProgress":     (*ItemUpdatedProgress)(nil),
		"ItemFinishedProgress":    (*ItemFinishedProgress)(nil),
		"ListCommand":             (*ListCommand)(nil),
		"CreateCommand":           (*CreateCommand)(nil),
		"AttachCommand":           (*AttachCommand)(nil),
		"DetachCommand":           (*DetachCommand)(nil),
		"AbortCommand":            (*AbortCommand)(nil),
		"PromptCommand":           (*PromptCommand)(nil),
		"SteerCommand":            (*SteerCommand)(nil),
		"SetModelCommand":         (*SetModelCommand)(nil),
		"SetThinkingCommand":      (*SetThinkingCommand)(nil),
		"ListResult":              (*ListResult)(nil),
		"DetachResult":            (*DetachResult)(nil),
		"SessionResult":           (*SessionResult)(nil),
		"ClientHello":             (*ClientHello)(nil),
		"RequestEnvelope":         (*RequestEnvelope)(nil),
		"ServerSnapshotEvent":     (*ServerSnapshotEvent)(nil),
		"SessionSnapshotEvent":    (*SessionSnapshotEvent)(nil),
		"SessionProgressEvent":    (*SessionProgressEvent)(nil),
		"SessionRemovedEvent":     (*SessionRemovedEvent)(nil),
		"ServerHello":             (*ServerHello)(nil),
		"ServerHelloError":        (*ServerHelloError)(nil),
		"ResponseEnvelope":        (*ResponseEnvelope)(nil),
		"EventEnvelope":           (*EventEnvelope)(nil),
	}
	for name, member := range members {
		t.Run(name, func(t *testing.T) {
			err := member.Validate()
			if err == nil {
				t.Fatal("a nil member validated clean")
			}
			if !strings.Contains(err.Error(), "nil pointer") {
				t.Errorf("error does not name the problem: %q", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name the member: %q", err)
			}
		})
	}
}

// TestValidateRejectsTypedNilItemsInsideProgress covers the containers, which
// dereference the member themselves before any Validate is reached:
// ItemFinishedProgress reads the item's status to decide whether the arm is
// legal at all, and ItemUpdatedProgress type-switches on it.
func TestValidateRejectsTypedNilItemsInsideProgress(t *testing.T) {
	items := map[string]TranscriptItem{
		"assistant": (*AssistantTranscriptItem)(nil),
		"tool":      (*ToolTranscriptItem)(nil),
	}
	for name, item := range items {
		t.Run("item_finished/"+name, func(t *testing.T) {
			progress := &ItemFinishedProgress{Type: "item_finished", Item: item}
			if err := progress.Validate(); err == nil {
				t.Error("item_finished accepted a nil item")
			}
		})
		t.Run("item_updated/"+name, func(t *testing.T) {
			progress := &ItemUpdatedProgress{Type: "item_updated", Item: item}
			if err := progress.Validate(); err == nil {
				t.Error("item_updated accepted a nil item")
			}
		})
		t.Run("item_started/"+name, func(t *testing.T) {
			progress := &ItemStartedProgress{Type: "item_started", Item: item}
			if err := progress.Validate(); err == nil {
				t.Error("item_started accepted a nil item")
			}
		})
	}

	// The same shape one level up: a snapshot's transcript and a content block
	// list both walk members the caller supplied.
	snapshot := fixtureSessionSnapshot()
	snapshot.Transcript = []TranscriptItem{(*UserTranscriptItem)(nil)}
	if err := snapshot.Validate(); err == nil {
		t.Error("a snapshot accepted a nil transcript item")
	}

	item := fixtureUserItem()
	item.Content = []Content{(*TextContent)(nil)}
	if err := item.Validate(); err == nil {
		t.Error("a transcript item accepted a nil content block")
	}
}

// TestEncodeRejectsTypedNilUnionMembers: the guard has to hold on the path that
// matters, which is the one that would otherwise put the message on a socket.
func TestEncodeRejectsTypedNilUnionMembers(t *testing.T) {
	message := &EventEnvelope{
		Type: "event",
		Event: &SessionProgressEvent{
			Type: "session_progress", SessionID: "s1",
			Progress: &ItemFinishedProgress{
				Type: "item_finished", Item: (*AssistantTranscriptItem)(nil),
			},
		},
	}
	if _, err := EncodeServerMessage(message, nil); err == nil {
		t.Error("EncodeServerMessage encoded a message with a nil union member")
	}
}
