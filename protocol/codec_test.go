package protocol

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBoundedErrorMessageCutsOnARuneBoundary: pi slices a UTF-16 string, where
// the cut cannot land inside a code unit. Go slices bytes, and the text being
// quoted here can carry a peer's own property name, so a blind msg[:497] turns
// the peer's last character into replacement bytes — mojibake in a diagnostic
// that exists to explain what the peer did wrong.
func TestBoundedErrorMessageCutsOnARuneBoundary(t *testing.T) {
	// 600 bytes of two-byte runes: the 497-byte cut lands inside one.
	long := strings.Repeat("é", 300)
	if utf8.ValidString(long[:maxEmbeddedErrorLength-len("...")]) {
		t.Fatal("the fixture no longer splits a rune at the cut; pick a different one")
	}

	got := boundedErrorMessage(errors.New(long))
	if !utf8.ValidString(got) {
		t.Errorf("truncated message is not valid UTF-8: %q", got)
	}
	if len(got) > maxEmbeddedErrorLength {
		t.Errorf("truncated message is %d bytes, want at most %d", len(got), maxEmbeddedErrorLength)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated message does not mark the cut: %q", got)
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got, "...")) {
		t.Errorf("truncated message is not a prefix of the original: %q", got)
	}

	// The ASCII path keeps pi's exact arithmetic, which is peer-visible in
	// nothing but is the behaviour the vectors were generated against.
	ascii := strings.Repeat("x", 600)
	if want := ascii[:maxEmbeddedErrorLength-len("...")] + "..."; boundedErrorMessage(errors.New(ascii)) != want {
		t.Errorf("ASCII truncation diverges from upstream's slice")
	}

	// Short messages and a nil error are untouched.
	if got := boundedErrorMessage(errors.New("short")); got != "short" {
		t.Errorf("short message was rewritten: %q", got)
	}
	if got := boundedErrorMessage(nil); got != "Unknown codec error" {
		t.Errorf("nil error: %q", got)
	}
}

// TestMessageDecoderFailureIsTerminal: the decoder is poisoned by design, and
// the error has to say so — "has failed" alone reads like something worth
// retrying, and no retry can ever succeed.
func TestMessageDecoderFailureIsTerminal(t *testing.T) {
	decoder, err := NewClientMessageDecoder(nil)
	if err != nil {
		t.Fatalf("NewClientMessageDecoder: %v", err)
	}
	frame, err := EncodeFrame(mustHex(t, "a1647479706567676f6f64627965")) // {"type":"goodbye"}
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if _, err := decoder.Push(frame); err == nil {
		t.Fatal("expected the invalid message to be rejected")
	}

	assertTerminal := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s after failure: got no error", what)
		}
		if !strings.HasPrefix(err.Error(), "client message decoder has failed") {
			t.Errorf("%s after failure: %q does not lead with upstream's wording", what, err)
		}
		if !strings.Contains(err.Error(), "re-establish the connection") {
			t.Errorf("%s after failure: %q does not say the decoder is unrecoverable", what, err)
		}
	}
	valid, err := EncodeClientMessage(NewClientHello(), nil)
	if err != nil {
		t.Fatalf("EncodeClientMessage: %v", err)
	}
	_, pushErr := decoder.Push(valid)
	assertTerminal("Push", pushErr)
	assertTerminal("End", decoder.End())
}
