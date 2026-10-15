package protocol

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol/cbor"
)

// The wire forms below are hand-built structs rather than the protocol's own
// message types: these tests are about the bytes a peer sends, and they have to
// stay legible independently of how the port models them. cbor.Encode writes
// struct fields in declaration order, so each mirrors the property order of the
// message it stands in for.

type wireEventEnvelope struct {
	Type  string `cbor:"type"`
	Event any    `cbor:"event"`
}

type wireProgressEvent struct {
	Type      string `cbor:"type"`
	SessionID string `cbor:"sessionId"`
	Progress  any    `cbor:"progress"`
}

type wireItemProgress struct {
	Type string `cbor:"type"`
	Item any    `cbor:"item"`
}

type wireTextContent struct {
	Type string `cbor:"type"`
	Text string `cbor:"text"`
}

// mustFrame encodes a hand-built wire value into one complete frame.
func mustFrame(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := cbor.Encode(value, nil)
	if err != nil {
		t.Fatalf("cbor.Encode: %v", err)
	}
	frame, err := EncodeFrame(payload)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	return frame
}

// decodeServerFrame pushes one complete frame and returns the single message.
func decodeServerFrame(t *testing.T, frame []byte) ServerMessage {
	t.Helper()
	decoder, err := NewServerMessageDecoder(nil)
	if err != nil {
		t.Fatalf("NewServerMessageDecoder: %v", err)
	}
	messages, err := decoder.Push(frame)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if err := decoder.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	return messages[0]
}

// spliceFrame rewrites one byte sequence inside a frame's payload and fixes the
// length prefix. It exists because Encode cannot produce every wire form the
// protocol must accept: an integral float is always folded to a CBOR integer, so
// planting the bytes is the only way to speak as a third-party peer would.
func spliceFrame(t *testing.T, frame, old, replacement []byte) []byte {
	t.Helper()
	payload := frame[frameHeaderLength:]
	if got := bytes.Count(payload, old); got != 1 {
		t.Fatalf("found %d occurrences of %x in %x, want exactly 1", got, old, payload)
	}
	spliced, err := EncodeFrame(bytes.Replace(payload, old, replacement, 1))
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	return spliced
}

// TestDecodeIntegerFieldAcceptsIntegralFloat: TypeBox types contentIndex with
// Integer, which checks Number.isInteger — and JavaScript has one number type,
// so a peer that reached for the float encoding is sending a value pi accepts.
// pi's own encoder folds integral floats to CBOR integers, so only a
// third-party peer produces these bytes, which is exactly why they are worth a
// test: rejecting them is an interop failure nobody would see in a Go-to-Go run.
func TestDecodeIntegerFieldAcceptsIntegralFloat(t *testing.T) {
	contentIndexKey := append([]byte{0x6c}, "contentIndex"...) // CBOR text(12)

	canonical := func(index int64) []byte {
		t.Helper()
		frame, err := EncodeServerMessage(progressEvent(&AssistantDeltaProgress{
			Type: "assistant_delta", MessageID: "a1", ContentIndex: index, Kind: DeltaText, Delta: "he",
		}), nil)
		if err != nil {
			t.Fatalf("EncodeServerMessage: %v", err)
		}
		return frame
	}

	tests := []struct {
		name    string
		index   int64
		encoded string // the CBOR item planted in place of the integer
		want    int64
		reject  bool
	}{
		{name: "positive_zero", index: 0, encoded: "fb0000000000000000", want: 0},
		{name: "negative_zero", index: 0, encoded: "fb8000000000000000", want: 0},
		{name: "one", index: 1, encoded: "fb3ff0000000000000", want: 1},
		{name: "fractional", index: 1, encoded: "fb3ff8000000000000", reject: true},
		{name: "infinity", index: 1, encoded: "fb7ff0000000000000", reject: true},
		{name: "nan", index: 1, encoded: "fb7ff8000000000000", reject: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := canonical(test.index)
			integerForm := base[frameHeaderLength:]
			old := append(append([]byte{}, contentIndexKey...), integerForm[bytes.Index(integerForm, contentIndexKey)+len(contentIndexKey)])
			planted := spliceFrame(t, base, old, append(append([]byte{}, contentIndexKey...), mustHex(t, test.encoded)...))

			decoder, err := NewServerMessageDecoder(nil)
			if err != nil {
				t.Fatalf("NewServerMessageDecoder: %v", err)
			}
			messages, err := decoder.Push(planted)
			if test.reject {
				if err == nil {
					t.Fatalf("accepted contentIndex encoded as %s", test.encoded)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected contentIndex encoded as %s: %v", test.encoded, err)
			}
			if len(messages) != 1 {
				t.Fatalf("got %d messages, want 1", len(messages))
			}
			delta := messages[0].(*EventEnvelope).
				Event.(*SessionProgressEvent).
				Progress.(*AssistantDeltaProgress)
			if delta.ContentIndex != test.want {
				t.Errorf("contentIndex = %d, want %d", delta.ContentIndex, test.want)
			}

			// Relaying it back must produce pi's canonical form, not the peer's:
			// the float spelling is accepted, never propagated.
			again, err := EncodeServerMessage(messages[0], nil)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(again, canonical(test.want)) {
				t.Errorf("re-encoded frame is not the canonical integer form\n got %s\nwant %s",
					hex.EncodeToString(again), hex.EncodeToString(canonical(test.want)))
			}
		})
	}
}

// TestDecodeKeepsExplicitNullDetails is the *any contract on ProtocolError: pi
// types details as Optional(JsonValue), and JsonValue includes Null, so absent
// and null are different messages. A real pi server sends both —
// sanitizeProtocolDetails(null) returns null, which the server spreads into the
// error — so a relay that collapses them silently rewrites a peer's message.
func TestDecodeKeepsExplicitNullDetails(t *testing.T) {
	type wireProtocolError struct {
		Code    string `cbor:"code"`
		Message string `cbor:"message"`
		Details any    `cbor:"details"`
	}
	type wireProtocolErrorNoDetails struct {
		Code    string `cbor:"code"`
		Message string `cbor:"message"`
	}
	type wireHelloError struct {
		Type  string `cbor:"type"`
		Error any    `cbor:"error"`
	}

	t.Run("null_is_preserved", func(t *testing.T) {
		frame := mustFrame(t, wireHelloError{
			Type:  "hello_error",
			Error: wireProtocolError{Code: "busy", Message: "server busy", Details: nil},
		})
		message := decodeServerFrame(t, frame)
		details := message.(*ServerHelloError).Error.Details
		if details == nil {
			t.Fatal("details:null decoded as an absent property")
		}
		if *details != nil {
			t.Fatalf("details:null decoded as %#v", *details)
		}
		again, err := EncodeServerMessage(message, nil)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(again, frame) {
			t.Errorf("relaying dropped or altered details:null\n got %s\nwant %s",
				hex.EncodeToString(again), hex.EncodeToString(frame))
		}
	})

	t.Run("absent_stays_absent", func(t *testing.T) {
		frame := mustFrame(t, wireHelloError{
			Type:  "hello_error",
			Error: wireProtocolErrorNoDetails{Code: "busy", Message: "server busy"},
		})
		message := decodeServerFrame(t, frame)
		if details := message.(*ServerHelloError).Error.Details; details != nil {
			t.Fatalf("an absent details decoded as a present %#v", *details)
		}
		again, err := EncodeServerMessage(message, nil)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(again, frame) {
			t.Errorf("relaying invented a details property\n got %s\nwant %s",
				hex.EncodeToString(again), hex.EncodeToString(frame))
		}
	})
}

// TestDecodeKeepsExplicitNullToolDetails is the same contract on the tool item,
// which is where pi actually emits details:null: sanitizeProtocolDetails is
// applied to every tool result before it goes on the wire.
func TestDecodeKeepsExplicitNullToolDetails(t *testing.T) {
	// Field order mirrors ToolTranscriptItem, minus the optional usage.
	type wireToolItem struct {
		ID         string `cbor:"id"`
		Role       string `cbor:"role"`
		ToolCallID string `cbor:"toolCallId"`
		ToolName   string `cbor:"toolName"`
		Input      any    `cbor:"input"`
		Content    []any  `cbor:"content"`
		Details    any    `cbor:"details"`
		Timestamp  int64  `cbor:"timestamp"`
		Status     string `cbor:"status"`
		IsError    bool   `cbor:"isError"`
	}
	type wireToolItemNoDetails struct {
		ID         string `cbor:"id"`
		Role       string `cbor:"role"`
		ToolCallID string `cbor:"toolCallId"`
		ToolName   string `cbor:"toolName"`
		Input      any    `cbor:"input"`
		Content    []any  `cbor:"content"`
		Timestamp  int64  `cbor:"timestamp"`
		Status     string `cbor:"status"`
		IsError    bool   `cbor:"isError"`
	}
	wrap := func(item any) any {
		return wireEventEnvelope{
			Type: "event",
			Event: wireProgressEvent{
				Type: "session_progress", SessionID: "s1",
				Progress: wireItemProgress{Type: "item_started", Item: item},
			},
		}
	}
	content := []any{wireTextContent{Type: "text", Text: "out"}}
	input := map[string]any{"cmd": "ls"}

	toolItem := func(message ServerMessage) *ToolTranscriptItem {
		t.Helper()
		return message.(*EventEnvelope).
			Event.(*SessionProgressEvent).
			Progress.(*ItemStartedProgress).
			Item.(*ToolTranscriptItem)
	}

	t.Run("null_is_preserved", func(t *testing.T) {
		frame := mustFrame(t, wrap(wireToolItem{
			ID: "t1", Role: "tool", ToolCallID: "tc1", ToolName: "bash",
			Input: input, Content: content, Details: nil,
			Timestamp: 1005, Status: "complete", IsError: false,
		}))
		message := decodeServerFrame(t, frame)
		details := toolItem(message).Details
		if details == nil {
			t.Fatal("details:null decoded as an absent property")
		}
		if *details != nil {
			t.Fatalf("details:null decoded as %#v", *details)
		}
		again, err := EncodeServerMessage(message, nil)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(again, frame) {
			t.Errorf("relaying dropped or altered details:null\n got %s\nwant %s",
				hex.EncodeToString(again), hex.EncodeToString(frame))
		}
	})

	t.Run("absent_stays_absent", func(t *testing.T) {
		frame := mustFrame(t, wrap(wireToolItemNoDetails{
			ID: "t1", Role: "tool", ToolCallID: "tc1", ToolName: "bash",
			Input: input, Content: content,
			Timestamp: 1005, Status: "complete", IsError: false,
		}))
		message := decodeServerFrame(t, frame)
		if details := toolItem(message).Details; details != nil {
			t.Fatalf("an absent details decoded as a present %#v", *details)
		}
		again, err := EncodeServerMessage(message, nil)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(again, frame) {
			t.Errorf("relaying invented a details property\n got %s\nwant %s",
				hex.EncodeToString(again), hex.EncodeToString(frame))
		}
	})
}

// TestDecodeRejectsNonStringMapKeys: the reflective decoder is reachable with
// any target a caller declares, and a CBOR object only ever has string keys. The
// mismatch used to reach reflect.SetMapIndex, which panics.
func TestDecodeRejectsNonStringMapKeys(t *testing.T) {
	var target struct {
		Entries map[int]string `cbor:"entries"`
	}
	err := decodeInto(map[string]any{"entries": map[string]any{"1": "one"}}, &target)
	if err == nil {
		t.Fatal("decoded an object into a map with int keys")
	}
	if _, ok := err.(*ValidationError); !ok {
		t.Errorf("got %T, want *ValidationError", err)
	}
}

// TestDecodeIntoNamedStringKeyedMap: a named string type is still a legal object
// key, and the map assignment must convert to the map's exact key type.
func TestDecodeIntoNamedStringKeyedMap(t *testing.T) {
	type modelID string
	var target struct {
		Entries map[modelID]string `cbor:"entries"`
	}
	if err := decodeInto(map[string]any{"entries": map[string]any{"opus": "5"}}, &target); err != nil {
		t.Fatalf("decodeInto: %v", err)
	}
	want := map[modelID]string{"opus": "5"}
	if !reflect.DeepEqual(target.Entries, want) {
		t.Errorf("decoded %#v, want %#v", target.Entries, want)
	}
}
