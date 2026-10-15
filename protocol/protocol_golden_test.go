package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol/cbor"
)

// testdata/upstream_protocol_v8.json is produced by testdata/gen-protocol-v8.ts
// run under node against upstream pi's packages/protocol (last at 49681e1b7,
// typebox 1.3.27): its real codec.ts, TypeBox schemas and CBOR encoder. Every frame here was encoded by
// the Node implementation and every reject was refused by it, so the file is an
// interop contract with a real peer, never a record of what Go happens to emit.
//
// What these vectors pin beyond protocol_test.go's case-for-case port of
// upstream's tests is the wire: that the v8 decoder reads a Node frame, and
// that re-encoding what it read reproduces the frame byte for byte — including
// the opaque `call`, `result` and `update` payloads, whose authored key order a
// Go map would not survive (protocol/cbor RawItem).
type upstreamProtocolV8 struct {
	ProtocolVersion int64            `json:"protocolVersion"`
	Client          []v8FrameVector  `json:"client"`
	Server          []v8FrameVector  `json:"server"`
	ClientRejects   []v8RejectVector `json:"clientRejects"`
	ServerRejects   []v8RejectVector `json:"serverRejects"`
}

// v8FrameVector is one accepted message: the JSON the Node side encoded and the
// complete length-prefixed frame it produced.
type v8FrameVector struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
	Frame string          `json:"frame"`
}

// v8RejectVector is one value upstream's parser refuses. Frame is the framed
// wire form when the value has one that upstream itself also refuses after
// decoding; NoWire says why there is none (the encoder refuses the value, or
// drops what made it invalid so the bytes decode to a valid message).
type v8RejectVector struct {
	Name   string          `json:"name"`
	Source string          `json:"source"`
	Value  json.RawMessage `json:"value"`
	Frame  string          `json:"frame"`
	NoWire string          `json:"noWire"`
	Error  string          `json:"error"`
}

func loadProtocolV8(t testing.TB) upstreamProtocolV8 {
	t.Helper()
	raw, err := os.ReadFile("testdata/upstream_protocol_v8.json")
	if err != nil {
		t.Fatalf("read v8 protocol vectors: %v", err)
	}
	var v upstreamProtocolV8
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse v8 protocol vectors: %v", err)
	}
	if len(v.Client) == 0 || len(v.Server) == 0 || len(v.ClientRejects) == 0 || len(v.ServerRejects) == 0 {
		t.Fatalf("v8 protocol vectors are incomplete: client %d, server %d, clientRejects %d, serverRejects %d",
			len(v.Client), len(v.Server), len(v.ClientRejects), len(v.ServerRejects))
	}
	return v
}

// vectorType reads the discriminant of a vector's JSON value.
func vectorType(t *testing.T, name string, value json.RawMessage) string {
	t.Helper()
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(value, &header); err != nil || header.Type == "" {
		t.Fatalf("%s: vector value has no type discriminant: %v", name, err)
	}
	return header.Type
}

// messageTag names the wire discriminant a decoded v8 message stands for, so a
// vector's `type` can be checked against the arm the decoder chose.
func messageTag(message any) string {
	switch message.(type) {
	case *ClientHelloV8, *ServerHelloV8:
		return "hello"
	case *RequestEnvelopeV8:
		return "request"
	case *CancelEnvelope:
		return "cancel"
	case *ServerHelloErrorV8:
		return "hello_error"
	case *ResponseEnvelopeV8:
		return "response"
	case *ServiceEventEnvelope:
		return "service_update"
	case *AttachmentEnvelope:
		return "attachment"
	default:
		return fmt.Sprintf("%T", message)
	}
}

// opaquePayloads returns the raw spans a decoded message carries, by field.
func opaquePayloads(message any) map[string]cbor.RawItem {
	switch m := message.(type) {
	case *RequestEnvelopeV8:
		return map[string]cbor.RawItem{"call": m.Call}
	case *ResponseEnvelopeV8:
		if m.Result == nil {
			return nil
		}
		return map[string]cbor.RawItem{"result": m.Result}
	case *ServiceEventEnvelope:
		return map[string]cbor.RawItem{"update": m.Update}
	default:
		return nil
	}
}

func TestV8GoldenProtocolVersion(t *testing.T) {
	if got := loadProtocolV8(t).ProtocolVersion; got != ProtocolVersionV8 {
		t.Fatalf("upstream PROTOCOL_VERSION is %d, the port speaks %d", got, ProtocolVersionV8)
	}
}

// decodeOneClient decodes exactly one framed client message through the real
// message decoder, the path a peer's bytes take.
func decodeOneClient(t *testing.T, name string, frame []byte) ClientMessageV8 {
	t.Helper()
	decoder, err := NewClientMessageDecoderV8(nil)
	if err != nil {
		t.Fatalf("NewClientMessageDecoderV8: %v", err)
	}
	messages, err := decoder.Push(frame)
	if err != nil {
		t.Fatalf("%s: the port rejects a frame upstream produced: %v", name, err)
	}
	if len(messages) != 1 {
		t.Fatalf("%s: decoded %d messages from one frame, want 1", name, len(messages))
	}
	if err := decoder.End(); err != nil {
		t.Fatalf("%s: End: %v", name, err)
	}
	return messages[0]
}

func decodeOneServer(t *testing.T, name string, frame []byte) ServerMessageV8 {
	t.Helper()
	decoder, err := NewServerMessageDecoderV8(nil)
	if err != nil {
		t.Fatalf("NewServerMessageDecoderV8: %v", err)
	}
	messages, err := decoder.Push(frame)
	if err != nil {
		t.Fatalf("%s: the port rejects a frame upstream produced: %v", name, err)
	}
	if len(messages) != 1 {
		t.Fatalf("%s: decoded %d messages from one frame, want 1", name, len(messages))
	}
	if err := decoder.End(); err != nil {
		t.Fatalf("%s: End: %v", name, err)
	}
	return messages[0]
}

// checkGoldenFrame runs the shared assertions on one decoded vector: the
// decoder chose the arm the JSON names, re-encoding reproduces upstream's bytes
// exactly, and any opaque payload came through as the span it arrived in.
func checkGoldenFrame(t *testing.T, vector v8FrameVector, frame []byte, message any, reencoded []byte) {
	t.Helper()
	if want, got := vectorType(t, vector.Name, vector.Value), messageTag(message); got != want {
		t.Errorf("%s: decoded as %q, the vector is a %q", vector.Name, got, want)
	}
	if !bytes.Equal(reencoded, frame) {
		t.Errorf("%s: re-encoding upstream's frame diverges\n got %x\nwant %x", vector.Name, reencoded, frame)
	}
	for field, raw := range opaquePayloads(message) {
		if len(raw) == 0 {
			t.Errorf("%s: %s came through empty, want the wire span", vector.Name, field)
			continue
		}
		if !bytes.Contains(frame, []byte(raw)) {
			t.Errorf("%s: %s is not a span of the frame it was decoded from\n span %x", vector.Name, field, raw)
		}
		// The point of the raw span: a vector named "ordered" carries a payload
		// whose authored key order is not the sorted order Go's map encoder
		// would impose, so decoding it to a map and encoding that back could
		// not have reproduced the frame. If this ever passes through a map
		// unchanged, the vector no longer proves anything and must be fixed.
		if strings.Contains(vector.Name, "ordered") {
			decoded, err := cbor.Decode(raw, nil)
			if err != nil {
				t.Fatalf("%s: %s does not decode: %v", vector.Name, field, err)
			}
			sorted, err := cbor.Encode(decoded, nil)
			if err != nil {
				t.Fatalf("%s: re-encoding decoded %s: %v", vector.Name, field, err)
			}
			if bytes.Equal(sorted, []byte(raw)) {
				t.Errorf("%s: %s survives a map round trip, so it is in sorted key order and does not exercise the raw span", vector.Name, field)
			}
		}
	}
}

func TestV8GoldenClientFramesRoundTrip(t *testing.T) {
	vectors := loadProtocolV8(t)
	seen := map[string]bool{}
	for _, vector := range vectors.Client {
		t.Run(vector.Name, func(t *testing.T) {
			frame := mustHex(t, vector.Frame)
			message := decodeOneClient(t, vector.Name, frame)
			reencoded, err := EncodeClientMessageV8(message, nil)
			if err != nil {
				t.Fatalf("%s: EncodeClientMessageV8 refuses what the decoder accepted: %v", vector.Name, err)
			}
			checkGoldenFrame(t, vector, frame, message, reencoded)
			seen[messageTag(message)] = true
		})
	}
	for _, tag := range []string{"hello", "request", "cancel"} {
		if !seen[tag] {
			t.Errorf("no client vector exercises the %q arm", tag)
		}
	}
}

func TestV8GoldenServerFramesRoundTrip(t *testing.T) {
	vectors := loadProtocolV8(t)
	seen := map[string]bool{}
	for _, vector := range vectors.Server {
		t.Run(vector.Name, func(t *testing.T) {
			frame := mustHex(t, vector.Frame)
			message := decodeOneServer(t, vector.Name, frame)
			reencoded, err := EncodeServerMessageV8(message, nil)
			if err != nil {
				t.Fatalf("%s: EncodeServerMessageV8 refuses what the decoder accepted: %v", vector.Name, err)
			}
			checkGoldenFrame(t, vector, frame, message, reencoded)
			seen[messageTag(message)] = true
		})
	}
	for _, tag := range []string{"hello", "hello_error", "response", "service_update", "attachment"} {
		if !seen[tag] {
			t.Errorf("no server vector exercises the %q arm", tag)
		}
	}
}

// The void response is the one vector whose Go form has a presence
// distinction the wire does not spell out: no result field at all is a nil
// RawItem, and must re-encode without the field, which the byte comparison
// above already proves; here the decoded form is pinned too, next to the
// explicit-null result that must NOT collapse into it.
func TestV8GoldenResponseResultPresence(t *testing.T) {
	var void, null *ResponseEnvelopeV8
	for _, vector := range loadProtocolV8(t).Server {
		switch vector.Name {
		case "response_ok_void":
			void, _ = decodeOneServer(t, vector.Name, mustHex(t, vector.Frame)).(*ResponseEnvelopeV8)
		case "response_ok_null":
			null, _ = decodeOneServer(t, vector.Name, mustHex(t, vector.Frame)).(*ResponseEnvelopeV8)
		}
	}
	if void == nil || null == nil {
		t.Fatal("vectors response_ok_void and response_ok_null are required")
	}
	if void.Result != nil {
		t.Errorf("void response decoded with a result: %x", void.Result)
	}
	if !bytes.Equal(null.Result, []byte{0xf6}) {
		t.Errorf("null result decoded to %x, want the CBOR null item f6", null.Result)
	}
}

// A stream of every vector, delivered a byte at a time, must yield each
// message and each must re-encode to its own frame: the opaque spans are
// captured from a reassembled frame, never from the chunk they arrived in.
func TestV8GoldenStreamsByteAtATime(t *testing.T) {
	vectors := loadProtocolV8(t)

	t.Run("client", func(t *testing.T) {
		decoder, err := NewClientMessageDecoderV8(nil)
		if err != nil {
			t.Fatal(err)
		}
		var messages []ClientMessageV8
		for _, vector := range vectors.Client {
			for _, b := range mustHex(t, vector.Frame) {
				got, err := decoder.Push([]byte{b})
				if err != nil {
					t.Fatalf("%s: Push: %v", vector.Name, err)
				}
				messages = append(messages, got...)
			}
		}
		if err := decoder.End(); err != nil {
			t.Fatalf("End: %v", err)
		}
		if len(messages) != len(vectors.Client) {
			t.Fatalf("decoded %d messages from %d frames", len(messages), len(vectors.Client))
		}
		for i, vector := range vectors.Client {
			reencoded, err := EncodeClientMessageV8(messages[i], nil)
			if err != nil {
				t.Fatalf("%s: %v", vector.Name, err)
			}
			if !bytes.Equal(reencoded, mustHex(t, vector.Frame)) {
				t.Errorf("%s: message %d of the stream re-encodes differently\n got %x\nwant %s", vector.Name, i, reencoded, vector.Frame)
			}
		}
	})

	t.Run("server", func(t *testing.T) {
		decoder, err := NewServerMessageDecoderV8(nil)
		if err != nil {
			t.Fatal(err)
		}
		var messages []ServerMessageV8
		for _, vector := range vectors.Server {
			for _, b := range mustHex(t, vector.Frame) {
				got, err := decoder.Push([]byte{b})
				if err != nil {
					t.Fatalf("%s: Push: %v", vector.Name, err)
				}
				messages = append(messages, got...)
			}
		}
		if err := decoder.End(); err != nil {
			t.Fatalf("End: %v", err)
		}
		if len(messages) != len(vectors.Server) {
			t.Fatalf("decoded %d messages from %d frames", len(messages), len(vectors.Server))
		}
		for i, vector := range vectors.Server {
			reencoded, err := EncodeServerMessageV8(messages[i], nil)
			if err != nil {
				t.Fatalf("%s: %v", vector.Name, err)
			}
			if !bytes.Equal(reencoded, mustHex(t, vector.Frame)) {
				t.Errorf("%s: message %d of the stream re-encodes differently\n got %x\nwant %s", vector.Name, i, reencoded, vector.Frame)
			}
		}
	})
}

// checkGoldenRejects pushes every reject that has a wire form through a fresh
// decoder and requires a ValidationError, then requires the decoder to be
// poisoned, as upstream's is. A reject without a wire form must say why.
func checkGoldenRejects(t *testing.T, rejects []v8RejectVector, push func([]byte) error) {
	t.Helper()
	wired := 0
	for _, reject := range rejects {
		t.Run(reject.Name, func(t *testing.T) {
			if reject.Error == "" {
				t.Fatalf("%s: upstream did not record a rejection; the generator must refuse to emit an accepted reject", reject.Name)
			}
			if reject.Frame == "" {
				if reject.NoWire == "" {
					t.Fatalf("%s: no frame and no reason why", reject.Name)
				}
				t.Logf("%s has no wire form: %s", reject.Name, reject.NoWire)
				return
			}
			wired++
			frame := mustHex(t, reject.Frame)
			err := push(frame)
			assertValidationError(t, reject.Name+" ("+reject.Source+")", err)
		})
	}
	if wired == 0 {
		t.Fatal("no reject has a wire form; nothing was exercised")
	}
}

func TestV8GoldenClientRejects(t *testing.T) {
	checkGoldenRejects(t, loadProtocolV8(t).ClientRejects, func(frame []byte) error {
		decoder, err := NewClientMessageDecoderV8(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decoder.Push(frame); err != nil {
			// Poisoned: a valid hello must now be refused too.
			hello, encodeErr := EncodeClientMessageV8(NewClientHelloV8(), nil)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if _, again := decoder.Push(hello); again == nil {
				t.Error("decoder accepted a message after an invalid one")
			}
			return err
		}
		return nil
	})
}

func TestV8GoldenServerRejects(t *testing.T) {
	checkGoldenRejects(t, loadProtocolV8(t).ServerRejects, func(frame []byte) error {
		decoder, err := NewServerMessageDecoderV8(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decoder.Push(frame); err != nil {
			hello, encodeErr := EncodeServerMessageV8(NewServerHelloV8(testServerID), nil)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if _, again := decoder.Push(hello); again == nil {
				t.Error("decoder accepted a message after an invalid one")
			}
			return err
		}
		return nil
	})
}
