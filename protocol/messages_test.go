package protocol

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol/cbor"
)

// testdata/upstream_messages.json is produced by testdata/gen-messages.ts run
// against upstream pi's own codec.ts and TypeBox schemas (npm i typebox@1.3.7
// alongside a tree of packages/protocol/src). Every frame here was encoded by
// the Node implementation, and every reject was refused by TypeBox's Check.
type upstreamMessages struct {
	Client        []encodedVector `json:"client"`
	Server        []encodedVector `json:"server"`
	ClientRejects []rejectVector  `json:"clientRejects"`
	ServerRejects []rejectVector  `json:"serverRejects"`
}

type encodedVector struct {
	Name  string `json:"name"`
	Frame string `json:"frame"`
	Error string `json:"error"`
}

type rejectVector struct {
	Name     string `json:"name"`
	Accepted bool   `json:"accepted"`
	Error    string `json:"error"`
}

func loadMessages(t testing.TB) upstreamMessages {
	t.Helper()
	raw, err := os.ReadFile("testdata/upstream_messages.json")
	if err != nil {
		t.Fatalf("read message vectors: %v", err)
	}
	var v upstreamMessages
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse message vectors: %v", err)
	}
	return v
}

// The fixture values, mirroring the constants in gen-messages.ts.
func fixtureModel() ModelRef { return ModelRef{Provider: "anthropic", ID: "claude-opus-5"} }

func fixtureUsage() Usage {
	return Usage{
		Input: 10, Output: 20, CacheRead: 0, CacheWrite: 0, TotalTokens: 30,
		Cost: UsageCost{Input: 0.1, Output: 0.2, CacheRead: 0, CacheWrite: 0, Total: 0.3},
	}
}

func fixtureUserItem() *UserTranscriptItem {
	return &UserTranscriptItem{
		ID: "u1", Role: "user",
		Content:   []Content{&TextContent{Type: "text", Text: "hi"}},
		Timestamp: 1000,
	}
}

func fixtureAssistantComplete() *AssistantTranscriptItem {
	return &AssistantTranscriptItem{
		ID: "a1", Role: "assistant",
		Content:    []Content{&TextContent{Type: "text", Text: "hello"}},
		Model:      fixtureModel(),
		Timestamp:  1001,
		Status:     AssistantComplete,
		StopReason: ptr(StopStop),
	}
}

func fixtureAssistantStreaming() *AssistantTranscriptItem {
	usage := fixtureUsage()
	return &AssistantTranscriptItem{
		ID: "a2", Role: "assistant",
		Content:       []Content{&ThinkingContent{Type: "thinking", Thinking: "hmm", Redacted: ptr(false)}},
		Model:         fixtureModel(),
		ResponseModel: ptr("claude-opus-5-20260501"),
		Usage:         &usage,
		Timestamp:     1002,
		Status:        AssistantStreaming,
	}
}

func fixtureAssistantError() *AssistantTranscriptItem {
	return &AssistantTranscriptItem{
		ID: "a3", Role: "assistant", Content: []Content{}, Model: fixtureModel(), Timestamp: 1003,
		Status: AssistantError, StopReason: ptr(StopError), ErrorMessage: ptr("boom"),
	}
}

func fixtureAssistantAborted() *AssistantTranscriptItem {
	return &AssistantTranscriptItem{
		ID: "a4", Role: "assistant", Content: []Content{}, Model: fixtureModel(), Timestamp: 1004,
		Status: AssistantAborted, StopReason: ptr(StopAborted), ErrorMessage: ptr(""),
	}
}

func fixtureToolItem() *ToolTranscriptItem {
	usage := fixtureUsage()
	return &ToolTranscriptItem{
		ID: "t1", Role: "tool", ToolCallID: "tc1", ToolName: "bash",
		Input: map[string]any{
			"args":   []any{int64(1), int64(2), nil},
			"cmd":    "ls",
			"nested": map[string]any{"deep": true},
		},
		Content:   []Content{&TextContent{Type: "text", Text: "out"}},
		Details:   detailsPtr(map[string]any{"exitCode": int64(0)}),
		Usage:     &usage,
		Timestamp: 1005, Status: ToolComplete, IsError: false,
	}
}

func fixtureToolError() *ToolTranscriptItem {
	return &ToolTranscriptItem{
		ID: "t2", Role: "tool", ToolCallID: "tc2", ToolName: "bash",
		Input:     nil,
		Content:   []Content{&ImageContent{Type: "image", Data: "aGk=", MimeType: "image/png"}},
		Timestamp: 1006, Status: ToolError, IsError: true,
	}
}

// Mirrors gen-messages.ts's sessionMetadata fixture exactly, every optional
// field populated, so the encoder is compared against the upstream-derived
// frames on the whole shape.
func fixtureSessionMetadata() SessionMetadata {
	updatedAt := int64(2)
	return SessionMetadata{
		ID: "s1", CreatedAt: 1, UpdatedAt: &updatedAt,
		ParentSessionID: ptr("s0"), SessionName: ptr("work"), Cwd: ptr("/tmp"),
	}
}

func fixtureSessionSnapshot() SessionSnapshot {
	return SessionSnapshot{
		ID: "s1", Name: ptr("work"), Cwd: "/tmp", CreatedAt: 1, UpdatedAt: 2,
		Phase: PhaseIdle, Model: fixtureModel(), ThinkingLevel: ThinkingMedium,
		Attached: true, Locked: false,
		Revision:    7,
		Transcript:  []TranscriptItem{fixtureUserItem(), fixtureAssistantComplete(), fixtureToolItem()},
		QueuedSteer: []UserTranscriptItem{*fixtureUserItem()}, QueuedSteerCount: 1,
	}
}

func fixtureModelMetadata() ModelMetadata {
	return ModelMetadata{
		Provider: "anthropic", ID: "claude-opus-5", Name: "Claude Opus 5", API: "anthropic-messages",
		Reasoning: true, Input: []ModelInputModality{InputText, InputImage},
		ContextWindow: 200000, MaxTokens: 64000,
		Cost:                    ModelCost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
		SupportedThinkingLevels: []ThinkingLevel{ThinkingOff, ThinkingLow, ThinkingHigh},
		Authenticated:           true,
	}
}

func fixtureServerSnapshot() ServerSnapshot {
	return ServerSnapshot{
		ServerID: "srv1", ProtocolVersion: ProtocolVersion, Revision: 3,
		Sessions: []SessionMetadata{fixtureSessionMetadata()},
		Models:   []ModelMetadata{fixtureModelMetadata()},
	}
}

func progressEvent(progress TranscriptProgress) *EventEnvelope {
	return &EventEnvelope{
		Type: "event",
		Event: &SessionProgressEvent{
			Type: "session_progress", SessionID: "s1", Progress: progress,
		},
	}
}

func goClientMessages() map[string]ClientMessage {
	return map[string]ClientMessage{
		"hello":              NewClientHello(),
		"hello_version_zero": &ClientHello{Type: "hello", Version: 0},
		"req_list":           NewRequest("r1", NewListCommand()),
		"req_create_empty":   NewRequest("r2", &CreateCommand{Command: "create"}),
		"req_create_full": NewRequest("r3", &CreateCommand{
			Command: "create", Cwd: ptr("/tmp"), Name: ptr("n"),
			Model: ptr(fixtureModel()), ThinkingLevel: ptr(ThinkingHigh),
		}),
		"req_attach":       NewRequest("r4", NewAttachCommand("s1")),
		"req_detach":       NewRequest("r5", NewDetachCommand("s1")),
		"req_prompt":       NewRequest("r6", NewPromptCommand("s1", "hi")),
		"req_steer":        NewRequest("r7", NewSteerCommand("s1", "no")),
		"req_abort":        NewRequest("r8", NewAbortCommand("s1")),
		"req_set_model":    NewRequest("r9", NewSetModelCommand("s1", fixtureModel())),
		"req_set_thinking": NewRequest("r10", NewSetThinkingCommand("s1", ThinkingMax)),
	}
}

func goServerMessages() map[string]ServerMessage {
	snapshot := fixtureSessionSnapshot()
	return map[string]ServerMessage{
		"hello": &ServerHello{
			Type: "hello", Version: ProtocolVersion, ConnectionID: "c1", Snapshot: fixtureServerSnapshot(),
		},
		"hello_error": &ServerHelloError{
			Type: "hello_error", Error: ProtocolError{Code: ErrorBusy, Message: "server busy"},
		},
		"hello_error_details": &ServerHelloError{
			Type: "hello_error",
			Error: ProtocolError{
				Code: ErrorVersion, Message: "nope",
				Details: detailsPtr(map[string]any{"supported": []any{int64(1)}}),
			},
		},
		"resp_list": &ResponseEnvelope{
			Type: "response", ID: "r1", OK: true,
			Result: &ListResult{Command: "list", Sessions: []SessionMetadata{fixtureSessionMetadata()}},
		},
		"resp_detach": &ResponseEnvelope{
			Type: "response", ID: "r5", OK: true,
			Result: &DetachResult{Command: "detach", SessionID: "s1"},
		},
		"resp_create": &ResponseEnvelope{
			Type: "response", ID: "r2", OK: true,
			Result: &SessionResult{Command: "create", Session: snapshot},
		},
		"resp_prompt": &ResponseEnvelope{
			Type: "response", ID: "r6", OK: true,
			Result: &SessionResult{Command: "prompt", Session: snapshot},
		},
		"resp_error": &ResponseEnvelope{
			Type: "response", ID: "r9", OK: false,
			Error: &ProtocolError{Code: ErrorNotFound, Message: "no such session"},
		},
		"resp_not_implemented": &ResponseEnvelope{
			Type: "response", ID: "r9", OK: false,
			Error: &ProtocolError{Code: ErrorNotImplemented, Message: "Operation is not implemented"},
		},
		"resp_internal_error": &ResponseEnvelope{
			Type: "response", ID: "r9", OK: false,
			Error: &ProtocolError{Code: ErrorInternal, Message: "Internal server error"},
		},
		"evt_server_snapshot": &EventEnvelope{
			Type:  "event",
			Event: &ServerSnapshotEvent{Type: "server_snapshot", Snapshot: fixtureServerSnapshot()},
		},
		"evt_session_snapshot": &EventEnvelope{
			Type:  "event",
			Event: &SessionSnapshotEvent{Type: "session_snapshot", Snapshot: snapshot},
		},
		"evt_removed": &EventEnvelope{
			Type:  "event",
			Event: &SessionRemovedEvent{Type: "session_removed", SessionID: "s1"},
		},
		"evt_item_started": progressEvent(&ItemStartedProgress{Type: "item_started", Item: fixtureUserItem()}),
		"evt_delta": progressEvent(&AssistantDeltaProgress{
			Type: "assistant_delta", MessageID: "a1", ContentIndex: 0, Kind: DeltaText, Delta: "he",
		}),
		"evt_item_updated": progressEvent(&ItemUpdatedProgress{
			Type: "item_updated", Item: fixtureAssistantStreaming(),
		}),
		"evt_item_finished_assistant": progressEvent(&ItemFinishedProgress{
			Type: "item_finished", Item: fixtureAssistantComplete(),
		}),
		"evt_item_finished_error": progressEvent(&ItemFinishedProgress{
			Type: "item_finished", Item: fixtureAssistantError(),
		}),
		"evt_item_finished_aborted": progressEvent(&ItemFinishedProgress{
			Type: "item_finished", Item: fixtureAssistantAborted(),
		}),
		"evt_item_finished_tool_error": progressEvent(&ItemFinishedProgress{
			Type: "item_finished", Item: fixtureToolError(),
		}),
	}
}

// TestEncodeClientMessagesMatchUpstream is the interop contract for everything
// a Go client sends.
func TestEncodeClientMessagesMatchUpstream(t *testing.T) {
	values := goClientMessages()
	vectors := loadMessages(t)
	if len(vectors.Client) == 0 {
		t.Fatal("no client vectors loaded")
	}
	for _, vector := range vectors.Client {
		message, ok := values[vector.Name]
		if !ok {
			t.Errorf("client vector %q has no Go value", vector.Name)
			continue
		}
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Error != "" {
				t.Fatalf("upstream failed to encode: %s", vector.Error)
			}
			got, err := EncodeClientMessage(message, nil)
			if err != nil {
				t.Fatalf("EncodeClientMessage: %v", err)
			}
			if hex.EncodeToString(got) != vector.Frame {
				t.Errorf("frame diverges from upstream\n got %s\nwant %s", hex.EncodeToString(got), vector.Frame)
			}
		})
	}
}

// TestEncodeServerMessagesMatchUpstream is the same contract for everything a
// Go server sends.
func TestEncodeServerMessagesMatchUpstream(t *testing.T) {
	values := goServerMessages()
	vectors := loadMessages(t)
	if len(vectors.Server) == 0 {
		t.Fatal("no server vectors loaded")
	}
	for _, vector := range vectors.Server {
		message, ok := values[vector.Name]
		if !ok {
			t.Errorf("server vector %q has no Go value", vector.Name)
			continue
		}
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Error != "" {
				t.Fatalf("upstream failed to encode: %s", vector.Error)
			}
			got, err := EncodeServerMessage(message, nil)
			if err != nil {
				t.Fatalf("EncodeServerMessage: %v", err)
			}
			if hex.EncodeToString(got) != vector.Frame {
				t.Errorf("frame diverges from upstream\n got %s\nwant %s", hex.EncodeToString(got), vector.Frame)
			}
		})
	}
}

// TestDecodeUpstreamFrames proves the other direction: every frame the Node
// implementation produced is accepted and round-trips to the same bytes.
func TestDecodeUpstreamFrames(t *testing.T) {
	vectors := loadMessages(t)

	for _, vector := range vectors.Client {
		t.Run("client/"+vector.Name, func(t *testing.T) {
			decoder, err := NewClientMessageDecoder(nil)
			if err != nil {
				t.Fatalf("NewClientMessageDecoder: %v", err)
			}
			messages, err := decoder.Push(mustHex(t, vector.Frame))
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if len(messages) != 1 {
				t.Fatalf("got %d messages, want 1", len(messages))
			}
			if err := decoder.End(); err != nil {
				t.Fatalf("End: %v", err)
			}
			again, err := EncodeClientMessage(messages[0], nil)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if hex.EncodeToString(again) != vector.Frame {
				t.Errorf("round-trip diverges\n got %s\nwant %s", hex.EncodeToString(again), vector.Frame)
			}
		})
	}

	for _, vector := range vectors.Server {
		t.Run("server/"+vector.Name, func(t *testing.T) {
			decoder, err := NewServerMessageDecoder(nil)
			if err != nil {
				t.Fatalf("NewServerMessageDecoder: %v", err)
			}
			messages, err := decoder.Push(mustHex(t, vector.Frame))
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if len(messages) != 1 {
				t.Fatalf("got %d messages, want 1", len(messages))
			}
			if err := decoder.End(); err != nil {
				t.Fatalf("End: %v", err)
			}
			again, err := EncodeServerMessage(messages[0], nil)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if hex.EncodeToString(again) != vector.Frame {
				t.Errorf("round-trip diverges\n got %s\nwant %s", hex.EncodeToString(again), vector.Frame)
			}
		})
	}
}

// goRejectValues mirrors gen-messages.ts's mutations. These are decoded-CBOR
// shapes (map[string]any), i.e. exactly what arrives off the wire, so the test
// exercises the same parser path a hostile peer would reach.
func goRejectValues() (client, server map[string]map[string]any) {
	model := map[string]any{"provider": "anthropic", "id": "claude-opus-5"}
	serverSnapshot := func(mutate func(map[string]any)) map[string]any {
		metadata := map[string]any{
			"provider": "anthropic", "id": "claude-opus-5", "name": "Claude Opus 5",
			"api": "anthropic-messages", "reasoning": true,
			"input":         []any{"text", "image"},
			"contextWindow": int64(200000), "maxTokens": int64(64000),
			"cost": map[string]any{
				"input": int64(3), "output": int64(15), "cacheRead": 0.3, "cacheWrite": 3.75,
			},
			"supportedThinkingLevels": []any{"off", "low", "high"},
			"authenticated":           true,
		}
		summary := map[string]any{
			"id": "s1", "name": "work", "cwd": "/tmp", "createdAt": int64(1), "updatedAt": int64(2),
			"phase": "idle", "model": model, "thinkingLevel": "medium",
			"attached": true, "locked": false,
		}
		snapshot := map[string]any{
			"serverId": "srv1", "protocolVersion": int64(1), "revision": int64(3),
			"sessions": []any{summary}, "models": []any{metadata},
		}
		if mutate != nil {
			mutate(snapshot)
		}
		return snapshot
	}
	userItem := func(mutate func(map[string]any)) map[string]any {
		item := map[string]any{
			"id": "u1", "role": "user",
			"content":   []any{map[string]any{"type": "text", "text": "hi"}},
			"timestamp": int64(1000),
		}
		if mutate != nil {
			mutate(item)
		}
		return item
	}
	assistantStreaming := func(mutate func(map[string]any)) map[string]any {
		item := map[string]any{
			"id": "a2", "role": "assistant",
			"content":   []any{map[string]any{"type": "thinking", "thinking": "hmm", "redacted": false}},
			"model":     model,
			"timestamp": int64(1002), "status": "streaming",
		}
		if mutate != nil {
			mutate(item)
		}
		return item
	}
	progress := func(p map[string]any) map[string]any {
		return map[string]any{
			"type": "event",
			"event": map[string]any{
				"type": "session_progress", "sessionId": "s", "progress": p,
			},
		}
	}

	client = map[string]map[string]any{
		"unknown_property": {"type": "hello", "version": int64(1), "extra": int64(1)},
		"credential_field": {"type": "hello", "version": int64(1), "token": "secret"},
		"version_string":   {"type": "hello", "version": "1"},
		"version_float":    {"type": "hello", "version": 1.5},
		"version_negative": {"type": "hello", "version": int64(-1)},
		"unknown_type":     {"type": "goodbye"},
		"unknown_command": {
			"type": "request", "id": "r", "request": map[string]any{"command": "nope"},
		},
		"nested_unknown_property": {
			"type": "request", "id": "r",
			"request": map[string]any{"command": "attach", "sessionId": "s", "extra": true},
		},
		"empty_session_id": {
			"type": "request", "id": "r",
			"request": map[string]any{"command": "attach", "sessionId": ""},
		},
		"bad_thinking_level": {
			"type": "request", "id": "r",
			"request": map[string]any{"command": "set_thinking", "sessionId": "s", "thinkingLevel": "ultra"},
		},
	}

	server = map[string]map[string]any{
		"bad_protocol_version": {
			"type": "hello", "version": int64(2), "connectionId": "c", "snapshot": serverSnapshot(nil),
		},
		"snapshot_bad_version": {
			"type": "hello", "version": int64(1), "connectionId": "c",
			"snapshot": serverSnapshot(func(s map[string]any) { s["protocolVersion"] = int64(2) }),
		},
		"response_ok_with_error": {
			"type": "response", "id": "r", "ok": true,
			"error": map[string]any{"code": "not_found", "message": "m"},
		},
		"response_missing_result": {"type": "response", "id": "r", "ok": true},
		"bad_error_code": {
			"type": "hello_error", "error": map[string]any{"code": "teapot", "message": "m"},
		},
		"auth_error_code": {
			"type":  "hello_error",
			"error": map[string]any{"code": "auth", "message": "Authentication failed"},
		},
		"streaming_with_stop_reason": progress(map[string]any{
			"type": "item_started",
			"item": assistantStreaming(func(i map[string]any) { i["stopReason"] = "stop" }),
		}),
		"item_finished_streaming": progress(map[string]any{
			"type": "item_finished", "item": assistantStreaming(nil),
		}),
		"tool_running_is_error_true": progress(map[string]any{
			"type": "item_started",
			"item": map[string]any{
				"id": "t1", "role": "tool", "toolCallId": "tc1", "toolName": "bash",
				"input":   map[string]any{"cmd": "ls"},
				"content": []any{map[string]any{"type": "text", "text": "out"}},
				"details": map[string]any{"exitCode": int64(0)},
				"usage": map[string]any{
					"input": int64(10), "output": int64(20), "cacheRead": int64(0),
					"cacheWrite": int64(0), "totalTokens": int64(30),
					"cost": map[string]any{
						"input": 0.1, "output": 0.2, "cacheRead": int64(0),
						"cacheWrite": int64(0), "total": 0.3,
					},
				},
				"timestamp": int64(1005), "status": "running", "isError": true,
			},
		}),
		"assistant_with_image_content": progress(map[string]any{
			"type": "item_started",
			"item": map[string]any{
				"id": "a1", "role": "assistant",
				"content": []any{map[string]any{
					"type": "image", "data": "x", "mimeType": "image/png",
				}},
				"model": model, "timestamp": int64(1001),
				"status": "complete", "stopReason": "stop",
			},
		}),
		"user_with_thinking_content": progress(map[string]any{
			"type": "item_started",
			"item": userItem(func(i map[string]any) {
				i["content"] = []any{map[string]any{"type": "thinking", "thinking": "x"}}
			}),
		}),
		"negative_timestamp": progress(map[string]any{
			"type": "item_started",
			"item": userItem(func(i map[string]any) { i["timestamp"] = int64(-1) }),
		}),
		"context_window_zero": {
			"type": "hello", "version": int64(1), "connectionId": "c",
			"snapshot": serverSnapshot(func(s map[string]any) {
				models := s["models"].([]any)
				models[0].(map[string]any)["contextWindow"] = int64(0)
			}),
		},
		"empty_thinking_levels": {
			"type": "hello", "version": int64(1), "connectionId": "c",
			"snapshot": serverSnapshot(func(s map[string]any) {
				models := s["models"].([]any)
				models[0].(map[string]any)["supportedThinkingLevels"] = []any{}
			}),
		},
		"negative_cost": {
			"type": "hello", "version": int64(1), "connectionId": "c",
			"snapshot": serverSnapshot(func(s map[string]any) {
				models := s["models"].([]any)
				models[0].(map[string]any)["cost"] = map[string]any{
					"input": int64(-1), "output": int64(0), "cacheRead": int64(0), "cacheWrite": int64(0),
				}
			}),
		},
	}
	return client, server
}

// TestRejectsMatchUpstream is the security-relevant half: a message TypeBox
// refuses must not slip past the Go parser. Error *text* is deliberately not
// compared — upstream collapses every failure to "Invalid <kind> protocol
// message" while the port reports which property failed, which is strictly more
// useful and not peer-visible.
func TestRejectsMatchUpstream(t *testing.T) {
	// Values whose JS form is not a Go map: pi rejects these at the
	// isProtocolValue gate, and the Go parser rejects them at the
	// discriminant lookup. Covered separately below.
	notAMap := map[string]bool{"null_message": true, "array_message": true, "string_message": true}

	clientValues, serverValues := goRejectValues()
	vectors := loadMessages(t)

	for _, vector := range vectors.ClientRejects {
		if notAMap[vector.Name] {
			continue
		}
		value, ok := clientValues[vector.Name]
		if !ok {
			t.Errorf("client reject %q unaccounted for", vector.Name)
			continue
		}
		t.Run("client/"+vector.Name, func(t *testing.T) {
			if vector.Accepted {
				t.Fatal("upstream accepted this value; it is not a reject case")
			}
			if _, err := ParseClientMessage(value); err == nil {
				t.Error("ParseClientMessage accepted a message upstream rejects")
			}
		})
	}

	for _, vector := range vectors.ServerRejects {
		value, ok := serverValues[vector.Name]
		if !ok {
			t.Errorf("server reject %q unaccounted for", vector.Name)
			continue
		}
		t.Run("server/"+vector.Name, func(t *testing.T) {
			if vector.Accepted {
				t.Fatal("upstream accepted this value; it is not a reject case")
			}
			if _, err := ParseServerMessage(value); err == nil {
				t.Error("ParseServerMessage accepted a message upstream rejects")
			}
		})
	}
}

func TestRejectsNonObjectMessages(t *testing.T) {
	for name, value := range map[string]any{
		"null":   nil,
		"array":  []any{},
		"string": "hello",
		"number": int64(1),
		"bool":   true,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseClientMessage(value); err == nil {
				t.Error("ParseClientMessage accepted a non-object")
			}
			if _, err := ParseServerMessage(value); err == nil {
				t.Error("ParseServerMessage accepted a non-object")
			}
		})
	}
}

// TestDecoderPoisonedAfterInvalidMessage: a peer that sent one invalid frame
// has lost the right to be interpreted.
func TestDecoderPoisonedAfterInvalidMessage(t *testing.T) {
	decoder, err := NewClientMessageDecoder(nil)
	if err != nil {
		t.Fatalf("NewClientMessageDecoder: %v", err)
	}
	// A well-framed CBOR payload that is not a legal client message.
	frame, err := EncodeFrame(mustHex(t, "a1647479706567676f6f64627965")) // {"type":"goodbye"}
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if _, err := decoder.Push(frame); err == nil {
		t.Fatal("expected the invalid message to be rejected")
	}

	valid, err := EncodeClientMessage(NewClientHello(), nil)
	if err != nil {
		t.Fatalf("EncodeClientMessage: %v", err)
	}
	if _, err := decoder.Push(valid); err == nil {
		t.Error("decoder accepted a message after a failure")
	}
}

func TestIsSupportedVersion(t *testing.T) {
	if !IsSupportedVersion(ProtocolVersion) {
		t.Errorf("IsSupportedVersion(%d) = false", ProtocolVersion)
	}
	for _, v := range []int64{0, 2, 3, -1} {
		if IsSupportedVersion(v) {
			t.Errorf("IsSupportedVersion(%d) = true", v)
		}
	}
}

// TestJSONValueKeyOrderDivergence records the one place the sorted-map
// divergence reaches a real protocol message: tool input and details are
// JsonValue, so their key order follows Go's sort rather than the peer's
// insertion order. The bytes differ; the decoded value does not. Every other
// property on the wire comes from a struct and keeps declaration order, which
// is why the vectors above are byte-exact.
func TestJSONValueKeyOrderDivergence(t *testing.T) {
	build := func(input map[string]any) []byte {
		item := fixtureToolItem()
		item.Input = input
		frame, err := EncodeServerMessage(progressEvent(&ItemStartedProgress{
			Type: "item_started", Item: item,
		}), nil)
		if err != nil {
			t.Fatalf("EncodeServerMessage: %v", err)
		}
		return frame
	}

	// Same value, two different Go map literals: encoding must be identical,
	// because Go sorts. That determinism is the point of the divergence.
	first := build(map[string]any{"zebra": int64(1), "apple": int64(2)})
	second := build(map[string]any{"apple": int64(2), "zebra": int64(1)})
	if hex.EncodeToString(first) != hex.EncodeToString(second) {
		t.Error("encoding is not deterministic across map literal orders")
	}

	// And it must round-trip to the same value a peer would have decoded.
	decoder, err := NewServerMessageDecoder(nil)
	if err != nil {
		t.Fatalf("NewServerMessageDecoder: %v", err)
	}
	messages, err := decoder.Push(first)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	event := messages[0].(*EventEnvelope).Event.(*SessionProgressEvent)
	item := event.Progress.(*ItemStartedProgress).Item.(*ToolTranscriptItem)
	input, ok := item.Input.(map[string]any)
	if !ok {
		t.Fatalf("input decoded as %T", item.Input)
	}
	if input["zebra"] != int64(1) || input["apple"] != int64(2) {
		t.Errorf("round-trip lost data: %#v", input)
	}
}

// detailsPtr wraps a details value for the *any field: a nil pointer is the
// absent property, a pointer to a nil interface is JSON null.
func detailsPtr(v any) *any { return &v }

// TestSessionSnapshotMetadataProjection pins pi's toMetadata field for field.
// The projection decides what every peer sees in `list` and `server_snapshot`,
// and before this test five behavioural mutations of it survived the whole
// suite: emitting a nil Cwd, emitting a nil UpdatedAt, inventing a
// ParentSessionID, dropping the SessionName carry, and (via Validate) dropping
// the snapshot's own cwd requirement.
func TestSessionSnapshotMetadataProjection(t *testing.T) {
	snapshot := fixtureSessionSnapshot()
	got := snapshot.Metadata()

	if got.ID != "s1" {
		t.Errorf("ID = %q, want s1", got.ID)
	}
	if got.CreatedAt != 1 {
		t.Errorf("CreatedAt = %d, want 1", got.CreatedAt)
	}
	if got.UpdatedAt == nil || *got.UpdatedAt != 2 {
		t.Errorf("UpdatedAt = %v, want 2 (a snapshot always has one)", got.UpdatedAt)
	}
	if got.Cwd == nil || *got.Cwd != "/tmp" {
		t.Errorf("Cwd = %v, want /tmp (a snapshot always has one)", got.Cwd)
	}
	// The rename is the easy thing to get wrong: a snapshot's `name` becomes
	// metadata's `sessionName`.
	if got.SessionName == nil || *got.SessionName != "work" {
		t.Errorf("SessionName = %v, want work", got.SessionName)
	}
	// pi's toMetadata never produces a parentSessionId; inventing one here
	// would let the server's overlay clobber the stored value.
	if got.ParentSessionID != nil {
		t.Errorf("ParentSessionID = %v, want nil (toMetadata never sets it)", *got.ParentSessionID)
	}

	// A snapshot with no name projects no sessionName, which is what lets the
	// server's overlay clear a stored one.
	unnamed := fixtureSessionSnapshot()
	unnamed.Name = nil
	if projected := unnamed.Metadata(); projected.SessionName != nil {
		t.Errorf("SessionName = %v, want nil for an unnamed snapshot", *projected.SessionName)
	}
}

// A snapshot still requires cwd and updatedAt even though metadata made both
// optional; the two shapes stopped sharing a field list in 6189e53b3.
func TestSessionSnapshotStillRequiresCwdAndUpdatedAt(t *testing.T) {
	withoutCwd := fixtureSessionSnapshot()
	withoutCwd.Cwd = ""
	if err := withoutCwd.Validate(); err == nil {
		t.Error("a snapshot with no cwd must be rejected")
	}
	// 0 is a legitimate timestamp here (requireTimestamp is non-negative), so
	// a negative one is what proves the snapshot still checks the field at all.
	badUpdated := fixtureSessionSnapshot()
	badUpdated.UpdatedAt = -1
	if err := badUpdated.Validate(); err == nil {
		t.Error("a snapshot with a negative updatedAt must be rejected")
	}

	// Metadata, by contrast, accepts both as absent -- and still validates
	// them when present.
	minimal := SessionMetadata{ID: "s1", CreatedAt: 1}
	if err := minimal.Validate(); err != nil {
		t.Errorf("id+createdAt alone must be a valid metadata: %v", err)
	}
	emptyCwd := SessionMetadata{ID: "s1", CreatedAt: 1, Cwd: ptr("")}
	if err := emptyCwd.Validate(); err == nil {
		t.Error("a present but empty cwd must be rejected (minLength 1)")
	}
	badMetaUpdated := SessionMetadata{ID: "s1", CreatedAt: 1, UpdatedAt: ptr(int64(-1))}
	if err := badMetaUpdated.Validate(); err == nil {
		t.Error("a present but negative updatedAt must be rejected")
	}
}

// Mirrors the strict-reject case upstream added alongside 6189e53b3: a listed
// session carrying a field the new shape dropped must be rejected outright,
// not silently ignored.
func TestListResultRejectsLeftoverSummaryFields(t *testing.T) {
	for _, field := range []string{"phase", "attached", "locked", "thinkingLevel", "name"} {
		t.Run(field, func(t *testing.T) {
			session := map[string]any{"id": "s1", "createdAt": int64(1), field: "x"}
			payload := map[string]any{
				"type": "response", "id": "r1", "ok": true,
				"result": map[string]any{"command": "list", "sessions": []any{session}},
			}
			frame, err := cbor.Encode(payload, nil)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if _, err := ParseServerMessage(frame); err == nil {
				t.Fatalf("a list session carrying %q must be rejected", field)
			}
		})
	}
}
