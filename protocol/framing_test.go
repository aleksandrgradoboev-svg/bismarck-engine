package protocol

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// testdata/upstream_framing.json is produced by testdata/gen-framing.ts run
// against upstream pi's packages/protocol/src/framing.ts. Framing is peer-
// visible, so these vectors are an interop contract, not a regression fence.
type upstreamFraming struct {
	Stream string `json:"stream"`
	Splits []struct {
		At     int      `json:"at"`
		Frames []string `json:"frames"`
		Error  string   `json:"error"`
	} `json:"splits"`
	Drip  []string `json:"drip"`
	Cases []struct {
		Name   string   `json:"name"`
		Frames []string `json:"frames"`
		Ended  bool     `json:"ended"`
		Error  string   `json:"error"`
	} `json:"cases"`
	AssertCases []struct {
		Hex   string `json:"hex"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	} `json:"assertCases"`
}

func loadFraming(t testing.TB) upstreamFraming {
	t.Helper()
	raw, err := os.ReadFile("testdata/upstream_framing.json")
	if err != nil {
		t.Fatalf("read framing vectors: %v", err)
	}
	var v upstreamFraming
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse framing vectors: %v", err)
	}
	return v
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func hexFrames(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, hex.EncodeToString(f))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEncodeFrameMatchesUpstream checks the length prefix against the frame
// bytes upstream produced for every CBOR vector.
func TestEncodeFrameMatchesUpstream(t *testing.T) {
	raw, err := os.ReadFile("cbor/testdata/upstream_vectors.json")
	if err != nil {
		t.Fatalf("read cbor vectors: %v", err)
	}
	var vectors struct {
		Encoded []struct {
			Name  string `json:"name"`
			Cbor  string `json:"cbor"`
			Frame string `json:"frame"`
		} `json:"encoded"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parse cbor vectors: %v", err)
	}
	if len(vectors.Encoded) == 0 {
		t.Fatal("no vectors loaded")
	}
	for _, vector := range vectors.Encoded {
		if vector.Cbor == "" {
			continue
		}
		got, err := EncodeFrame(mustHex(t, vector.Cbor))
		if err != nil {
			t.Fatalf("%s: EncodeFrame: %v", vector.Name, err)
		}
		if hex.EncodeToString(got) != vector.Frame {
			t.Errorf("%s: frame diverges\n got %s\nwant %s", vector.Name, hex.EncodeToString(got), vector.Frame)
		}
	}
}

// TestFrameDecoderChunkBoundaries feeds the same stream split at every possible
// boundary. Where a chunk lands is a property of the network, so the frame
// sequence must never depend on it.
func TestFrameDecoderChunkBoundaries(t *testing.T) {
	vectors := loadFraming(t)
	stream := mustHex(t, vectors.Stream)

	for _, split := range vectors.Splits {
		if split.Error != "" {
			t.Fatalf("upstream failed at split %d: %s", split.At, split.Error)
		}
		decoder, err := NewFrameDecoder(nil)
		if err != nil {
			t.Fatalf("NewFrameDecoder: %v", err)
		}
		var got []string
		for _, chunk := range [][]byte{stream[:split.At], stream[split.At:]} {
			if len(chunk) == 0 {
				continue
			}
			frames, err := decoder.Push(chunk)
			if err != nil {
				t.Fatalf("split %d: Push: %v", split.At, err)
			}
			got = append(got, hexFrames(frames)...)
		}
		if err := decoder.End(); err != nil {
			t.Fatalf("split %d: End: %v", split.At, err)
		}
		if !equalStrings(got, split.Frames) {
			t.Errorf("split %d: frames diverge\n got %v\nwant %v", split.At, got, split.Frames)
		}
	}
}

// TestFrameDecoderByteAtATime is the worst-case delivery pattern: every header
// and payload boundary is crossed mid-chunk.
func TestFrameDecoderByteAtATime(t *testing.T) {
	vectors := loadFraming(t)
	decoder, err := NewFrameDecoder(nil)
	if err != nil {
		t.Fatalf("NewFrameDecoder: %v", err)
	}
	var got []string
	for _, b := range mustHex(t, vectors.Stream) {
		frames, err := decoder.Push([]byte{b})
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		got = append(got, hexFrames(frames)...)
	}
	if err := decoder.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	if !equalStrings(got, vectors.Drip) {
		t.Errorf("frames diverge\n got %v\nwant %v", got, vectors.Drip)
	}
}

func TestFrameDecoderCasesMatchUpstream(t *testing.T) {
	limit8 := 8
	chunksByCase := map[string][]string{
		"truncated_header":     {"0000"},
		"truncated_payload":    {"0000000461"},
		"over_limit":           {"00000010"},
		"at_limit":             {"000000080011223344556677"},
		"zero_length_frames":   {"000000000000000000000000"},
		"two_frames_one_chunk": {"0000000161000000026263"},
		"push_after_failure":   {"00000010", "61"},
	}
	optsByCase := map[string]*FrameOptions{
		"over_limit":         {MaxFrameLength: &limit8},
		"at_limit":           {MaxFrameLength: &limit8},
		"push_after_failure": {MaxFrameLength: &limit8},
	}

	for _, vector := range loadFraming(t).Cases {
		chunks, ok := chunksByCase[vector.Name]
		if !ok {
			t.Errorf("case %q unaccounted for", vector.Name)
			continue
		}
		t.Run(vector.Name, func(t *testing.T) {
			decoder, err := NewFrameDecoder(optsByCase[vector.Name])
			if err != nil {
				t.Fatalf("NewFrameDecoder: %v", err)
			}
			var got []string
			var failure error
			for _, chunk := range chunks {
				frames, err := decoder.Push(mustHex(t, chunk))
				got = append(got, hexFrames(frames)...)
				if err != nil {
					failure = err
					break
				}
			}
			if failure == nil {
				failure = decoder.End()
			}

			if !equalStrings(got, vector.Frames) {
				t.Errorf("frames diverge\n got %v\nwant %v", got, vector.Frames)
			}
			if vector.Error == "" {
				if failure != nil {
					t.Errorf("unexpected error %q; upstream completed cleanly", failure)
				}
				return
			}
			if failure == nil {
				t.Fatalf("expected error %q, got none", vector.Error)
			}
			if failure.Error() != vector.Error {
				t.Errorf("error text diverges\n got %q\nwant %q", failure, vector.Error)
			}
		})
	}
}

// TestFrameDecoderPoisonedAfterFailure locks the "a framing error is
// unrecoverable" rule: once the stream position is unknown, no later byte can
// be interpreted.
//
// The message must keep upstream's wording as its lead — that is what a reader
// comparing the two implementations greps for — and must also say what the
// caller has to do, because "has failed" alone reads like a transient condition
// worth retrying and no retry can ever succeed.
func TestFrameDecoderPoisonedAfterFailure(t *testing.T) {
	limit := 8
	decoder, err := NewFrameDecoder(&FrameOptions{MaxFrameLength: &limit})
	if err != nil {
		t.Fatalf("NewFrameDecoder: %v", err)
	}
	if _, err := decoder.Push(mustHex(t, "00000010")); err == nil {
		t.Fatal("expected an over-limit failure")
	}

	assertFailedMessage := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s after failure: got no error", what)
		}
		if !strings.HasPrefix(err.Error(), "Frame decoder has failed") {
			t.Errorf("%s after failure: %q does not lead with upstream's wording", what, err)
		}
		if !strings.Contains(err.Error(), "re-establish the connection") {
			t.Errorf("%s after failure: %q does not say the decoder is unrecoverable", what, err)
		}
	}
	_, pushErr := decoder.Push([]byte{0x61})
	assertFailedMessage("Push", pushErr)
	assertFailedMessage("End", decoder.End())
}

// TestFrameOptionRangeErrorsAreTyped: an out-of-range limit is the caller's bug,
// not the peer's, and callers branch on that distinction — server.go already
// matches *FrameError to decide whether a connection is at fault. An untyped
// error here made a configuration mistake indistinguishable from a hostile peer.
func TestFrameOptionRangeErrorsAreTyped(t *testing.T) {
	// Only the low end is portable to assert: the high end is 2^32, which is
	// not representable as an int on a 32-bit build, and this package must keep
	// compiling there.
	limit := -1
	opts := &FrameOptions{MaxFrameLength: &limit}

	var rangeErr *RangeError
	_, err := NewFrameDecoder(opts)
	if err == nil {
		t.Fatal("NewFrameDecoder accepted a negative maxFrameLength")
	}
	if !errors.As(err, &rangeErr) {
		t.Errorf("NewFrameDecoder: got %T, want *RangeError", err)
	}

	err = AssertCompleteFrame(mustHex(t, "0000000161"), opts)
	if err == nil {
		t.Fatal("AssertCompleteFrame accepted a negative maxFrameLength")
	}
	if !errors.As(err, &rangeErr) {
		t.Errorf("AssertCompleteFrame: got %T, want *RangeError", err)
	}
	var frameErr *FrameError
	if errors.As(err, &frameErr) {
		t.Error("a caller-configuration error is reported as *FrameError, i.e. as the peer's fault")
	}

	// The same distinction on the encode side, where pi also throws RangeError.
	if _, err := NewClientMessageDecoder(opts); err == nil || !errors.As(err, &rangeErr) {
		t.Errorf("NewClientMessageDecoder: got %v (%T), want *RangeError", err, err)
	}
}

// TestAssertCompleteFrameMatchesUpstream pins AssertCompleteFrame against
// vectors captured from upstream's assertCompleteFrame.
//
// Upstream DELETED assertCompleteFrame in the 2026-09-03 rewrite, so
// gen-framing.ts can no longer emit this section and the vectors are FROZEN at
// the last sha that had it (96317e50b). The Go function is still live —
// codec.go calls it on every encode — so the vectors keep their value as a
// regression pin even though they can no longer be regenerated. The emptiness
// guard below is load-bearing: dropping the section from the JSON would
// otherwise leave this test iterating nothing and passing, which is exactly
// what happened when the section was first removed.
func TestAssertCompleteFrameMatchesUpstream(t *testing.T) {
	cases := loadFraming(t).AssertCases
	if len(cases) == 0 {
		t.Fatal("assertCases is empty: this test pins AssertCompleteFrame and cannot vacate silently. " +
			"The section is frozen at 96317e50b because upstream deleted assertCompleteFrame; " +
			"restore it from git rather than removing the test, or delete AssertCompleteFrame and this test together.")
	}
	for _, vector := range cases {
		err := AssertCompleteFrame(mustHex(t, vector.Hex), nil)
		if vector.OK {
			if err != nil {
				t.Errorf("%q: rejected a frame upstream accepts: %v", vector.Hex, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q: accepted a frame upstream rejects with %q", vector.Hex, vector.Error)
			continue
		}
		if err.Error() != vector.Error {
			t.Errorf("%q: error text diverges\n got %q\nwant %q", vector.Hex, err, vector.Error)
		}
	}
}

// TestFrameDecoderDoesNotPreallocateDeclaredLength guards the DoS property pi
// gets from its 64 KiB block list: announcing a large frame must not cost that
// much memory until the bytes actually arrive.
func TestFrameDecoderDoesNotPreallocateDeclaredLength(t *testing.T) {
	decoder, err := NewFrameDecoder(nil)
	if err != nil {
		t.Fatalf("NewFrameDecoder: %v", err)
	}
	// Declare 16 MiB, deliver one byte.
	if _, err := decoder.Push([]byte{0x01, 0x00, 0x00, 0x00, 0x41}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := decoder.payload.Cap(); got > 1<<16 {
		t.Errorf("buffer grew to %d bytes for a 1-byte payload; it should follow the data, not the declaration", got)
	}
}
