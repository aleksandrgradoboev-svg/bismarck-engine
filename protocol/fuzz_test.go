package protocol

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// This package's entire job is turning bytes from an untrusted peer into typed
// messages, across four layers that each have their own idea of what is legal:
// framing, CBOR, union dispatch, and constraint validation. The fuzz targets
// below drive all four the way a socket does and assert the properties that
// matter for a relay:
//
//   - nothing panics, whatever the bytes are;
//   - anything accepted can be re-encoded, because a message we can read but not
//     write is one a relay silently drops;
//   - re-encoding is a fixed point, so a message does not drift as it is
//     forwarded from hop to hop.
//
// The seed corpus is derived from testdata/upstream_messages.json rather than
// checked in under testdata/fuzz: those frames are generated from upstream pi,
// and reading them here means the seeds cannot drift from the goldens. Go runs
// every seed on an ordinary `go test`, so they are also a cheap regression suite
// for the whole stack.

// seedCorpus adds one direction's upstream frames plus the chunk shapes a real
// connection produces: a split frame, several frames in one chunk, a bare
// header, and nothing at all.
func seedCorpus(f *testing.F, frames []encodedVector) {
	f.Helper()
	var concatenated []byte
	for _, vector := range frames {
		frame := mustHex(f, vector.Frame)
		f.Add(frame)
		f.Add(frame[:len(frame)/2])
		concatenated = append(concatenated, frame...)
	}
	f.Add(concatenated)
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x61})
}

func FuzzClientDecoder(f *testing.F) {
	seedCorpus(f, loadMessages(f).Client)

	f.Fuzz(func(t *testing.T, data []byte) {
		decoder, err := NewClientMessageDecoder(nil)
		if err != nil {
			t.Fatalf("NewClientMessageDecoder: %v", err)
		}
		messages, err := decoder.Push(data)
		if err != nil {
			// Rejection is always a legal outcome; not panicking is the claim.
			return
		}
		for _, message := range messages {
			frame, err := EncodeClientMessage(message, nil)
			if err != nil {
				t.Fatalf("accepted a message that cannot be re-encoded: %v", err)
			}
			again, err := NewClientMessageDecoder(nil)
			if err != nil {
				t.Fatalf("NewClientMessageDecoder: %v", err)
			}
			relayed, err := again.Push(frame)
			if err != nil {
				t.Fatalf("re-encoded frame %s was rejected: %v", hex.EncodeToString(frame), err)
			}
			if len(relayed) != 1 {
				t.Fatalf("re-encoded frame decoded to %d messages, want 1", len(relayed))
			}
			twice, err := EncodeClientMessage(relayed[0], nil)
			if err != nil {
				t.Fatalf("re-encode is not idempotent: %v", err)
			}
			if !bytes.Equal(frame, twice) {
				t.Fatalf("message drifts across a relay hop\nfirst  %s\nsecond %s",
					hex.EncodeToString(frame), hex.EncodeToString(twice))
			}
		}
	})
}

func FuzzServerDecoder(f *testing.F) {
	seedCorpus(f, loadMessages(f).Server)

	f.Fuzz(func(t *testing.T, data []byte) {
		decoder, err := NewServerMessageDecoder(nil)
		if err != nil {
			t.Fatalf("NewServerMessageDecoder: %v", err)
		}
		messages, err := decoder.Push(data)
		if err != nil {
			return
		}
		for _, message := range messages {
			frame, err := EncodeServerMessage(message, nil)
			if err != nil {
				t.Fatalf("accepted a message that cannot be re-encoded: %v", err)
			}
			again, err := NewServerMessageDecoder(nil)
			if err != nil {
				t.Fatalf("NewServerMessageDecoder: %v", err)
			}
			relayed, err := again.Push(frame)
			if err != nil {
				t.Fatalf("re-encoded frame %s was rejected: %v", hex.EncodeToString(frame), err)
			}
			if len(relayed) != 1 {
				t.Fatalf("re-encoded frame decoded to %d messages, want 1", len(relayed))
			}
			twice, err := EncodeServerMessage(relayed[0], nil)
			if err != nil {
				t.Fatalf("re-encode is not idempotent: %v", err)
			}
			if !bytes.Equal(frame, twice) {
				t.Fatalf("message drifts across a relay hop\nfirst  %s\nsecond %s",
					hex.EncodeToString(frame), hex.EncodeToString(twice))
			}
		}
	})
}
