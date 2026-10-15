package providers

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestSSEReadersRefuseABodyThatNeverProgresses: a body whose reads return no
// data and no error forever — io.Reader's contract discourages it, and no
// network body does it — fails each SSE reader with the port's guard, which
// says it is the port's and what to report, where pi's reader would wait on
// it forever (docs/UPSTREAM.md D83). What the body delivered first is still
// read; a body that pauses between reads is not refused.
func TestSSEReadersRefuseABodyThatNeverProgresses(t *testing.T) {
	readers := []struct {
		provider string
		first    string
		read     func(io.Reader) error
	}{
		{"anthropic", "event: ping\ndata: {}\n\n", func(body io.Reader) error {
			return iterateAnthropicSSE(body, context.Background(), nil, func(rawObject) error { return nil })
		}},
		{"pi-messages", "data: {\"type\":\"start\"}\n\n", func(body io.Reader) error {
			return readPiMessagesEvents(body, nil, func(piMessagesEvent) (bool, error) { return true, nil })
		}},
		{"google", "data: {\"candidates\":[]}\n\n", func(body io.Reader) error {
			return iterateGoogleSSE(body, nil, func(any) error { return nil })
		}},
	}
	for _, r := range readers {
		t.Run(r.provider, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- r.read(io.MultiReader(strings.NewReader(r.first), emptyReader{})) }()
			select {
			case err := <-done:
				want := "the " + r.provider + " response body returned no data and no error 100 reads in a row; this is a port guard against a broken body reader, report it with the provider: multiple Read calls return no data or error"
				if err == nil || err.Error() != want || !errors.Is(err, io.ErrNoProgress) {
					t.Fatalf("err = %v, want %q wrapping io.ErrNoProgress", err, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("still reading a body that returns nothing forever")
			}
			// A body that returns nothing on every other read still reads,
			// past maxEmptyBodyReads bytes of it.
			body := &stutteringReader{r: strings.NewReader(strings.Repeat(r.first, 10))}
			if err := r.read(body); err != nil {
				t.Fatalf("a body with an empty read between each byte: %v", err)
			}
		})
	}
}
