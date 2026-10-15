package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// piMessagesHoleStream is a stream that starts text blocks at each of starts,
// appends "a" to the last one and finishes.
func piMessagesHoleStream(starts ...int) string {
	frames := []string{`{"type":"start"}`}
	for _, at := range starts {
		frames = append(frames, fmt.Sprintf(`{"type":"text_start","contentIndex":%d}`, at))
	}
	last := starts[len(starts)-1]
	frames = append(frames,
		fmt.Sprintf(`{"type":"text_delta","contentIndex":%d,"delta":"a"}`, last),
		fmt.Sprintf(`{"type":"text_end","contentIndex":%d,"content":"a"}`, last),
		`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`)
	return piMessagesSSE(frames...)
}

// TestPiMessagesContentHolesAreBounded pins the port's bound on the unset
// slots a message's content holds (piMessagesMaxContentHoles). Up to the
// bound the message is pi's: every hole is kept, and a block started in a
// hole fills it and frees its room. Past it the stream fails with an error
// that says what to check, where pi's sparse array would take any slot: a
// backend numbers its blocks from 0 without gaps, and a far slot — one frame
// can name 2^31-2 — would otherwise have every pushed event copy that many
// slots. The error counts the content's slots and, apart, the ones a block
// is set in.
func TestPiMessagesContentHolesAreBounded(t *testing.T) {
	overflow := func(slot, slots, set, unset int) string {
		return fmt.Sprintf("pi-messages backend started a block at contentIndex %d, past the %d content slots the message has (%d set); that would leave %d of its content slots unset, and the port holds at most 1024 (pi's sparse array holds any number). Check that the backend numbers content blocks from 0 without gaps", slot, slots, set, unset)
	}
	for _, tc := range []struct {
		name   string
		starts []int
		// want is the error the stream fails with; "" for pi's finished
		// message, whose last block is "a" at the last start.
		want string
	}{
		{name: "holesUpToTheBound", starts: []int{1024}},
		{name: "oneHolePastTheBound", starts: []int{1025}, want: overflow(1025, 0, 0, 1025)},
		{name: "aFilledHoleFreesItsRoom", starts: []int{1000, 500, 1026}},
		{name: "pastTheBoundAfterHoles", starts: []int{1000, 2030}, want: overflow(2030, 1001, 1, 2029)},
		{name: "pastTheBoundAfterAFilledHole", starts: []int{500, 0, 1600}, want: overflow(1600, 501, 2, 1598)},
		{name: "farSlot", starts: []int{1<<31 - 2}, want: overflow(1<<31-2, 0, 0, 1<<31-2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, pushed, final := streamPiMessagesEvents(t, context.Background(), piMessagesHoleStream(tc.starts...), ai.StreamOptions{})
			if tc.want != "" {
				if final.StopReason != ai.StopError || final.ErrorMessage != tc.want {
					t.Fatalf("stopReason = %s, errorMessage = %q; want error %q", final.StopReason, final.ErrorMessage, tc.want)
				}
				// start, a text_start for each start before the last, error.
				types := make([]ai.EventType, len(pushed))
				for i, ev := range pushed {
					types[i] = ev.Type
				}
				want := []ai.EventType{ai.EventStart}
				for range tc.starts[1:] {
					want = append(want, ai.EventTextStart)
				}
				want = append(want, ai.EventError)
				if !slices.Equal(types, want) {
					t.Errorf("pushed %v, want %v", types, want)
				}
				return
			}
			if final.StopReason != ai.StopStop {
				t.Fatalf("stopReason = %s (%s), want stop", final.StopReason, final.ErrorMessage)
			}
			last := tc.starts[len(tc.starts)-1]
			if len(final.Content) != last+1 || final.Content[last] != (ai.TextContent{Text: "a"}) {
				t.Fatalf("content has %d blocks, want %d ending in the text block", len(final.Content), last+1)
			}
			for i, block := range final.Content[:last] {
				started := false
				for _, at := range tc.starts {
					started = started || at == i
				}
				if (block != nil) != started {
					t.Fatalf("content[%d] = %#v; want a hole except where a block started", i, block)
				}
			}
			if _, err := json.Marshal(final); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestPiMessagesFarContentIndexFailsFast requires a frame naming a slot far
// past the content to fail the stream at once, without growing the content
// toward it. The allocation bound is what proves it; a wall-clock bound would
// only add a flake under -race on a loaded machine.
func TestPiMessagesFarContentIndexFailsFast(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, final := streamPiMessagesEvents(t, t.Context(), piMessagesHoleStream(10_000_000), ai.StreamOptions{})
	runtime.ReadMemStats(&after)
	if final.StopReason != ai.StopError {
		t.Errorf("stopReason = %s, want error: slot 10000000 is past the bound", final.StopReason)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Errorf("streaming allocated %d MB for one far contentIndex; the content must not grow toward it", allocated>>20)
	}
}
