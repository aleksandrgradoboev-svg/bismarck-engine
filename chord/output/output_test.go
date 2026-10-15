package output

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// Ported from pi packages/chord/test/output.test.ts and output-skip.test.ts.
// A lone surrogate in pi's inputs is, once encoded, U+FFFD: the Go inputs use
// that character where pi's use one.

func head(maxLines int, maxBytes ...int) Limits {
	b := 1000
	if len(maxBytes) > 0 {
		b = maxBytes[0]
	}
	return Limits{MaxBytes: b, MaxLines: maxLines, Retain: RetainHead}
}

func tail(maxLines int, maxBytes ...int) Limits {
	l := head(maxLines, maxBytes...)
	l.Retain = RetainTail
	return l
}

type bounded struct {
	kept         string
	droppedBytes int
	droppedLines int
}

func bound(text string, limits Limits) bounded {
	s := BoundOutput(text, limits)
	return bounded{s.Text, s.DroppedBytes, s.DroppedLines}
}

// bufferTail is Node's Buffer tail slice on a character boundary.
func bufferTail(content string, maxBytes int) string {
	b := []byte(content)
	if len(b) <= maxBytes {
		return content
	}
	start := len(b) - maxBytes
	for start < len(b) && b[start]&0xc0 == 0x80 {
		start++
	}
	return string(b[start:])
}

func assertMatchesBufferTail(t *testing.T, input string, maxByteValues []int) {
	t.Helper()
	if maxByteValues == nil {
		for maxBytes := 0; maxBytes < len(input)+5; maxBytes++ {
			maxByteValues = append(maxByteValues, maxBytes)
		}
	}
	for _, maxBytes := range maxByteValues {
		kept := BoundOutput(input, Limits{MaxBytes: maxBytes, MaxLines: 10, Retain: RetainTail}).Text
		if expected := bufferTail(input, maxBytes); kept != expected {
			t.Fatalf("tail mismatch input=%q maxBytes=%d expected=%q actual=%q", input, maxBytes, expected, kept)
		}
		if len(kept) > maxBytes {
			t.Fatalf("tail output exceeded %d bytes", maxBytes)
		}
	}
}

func sampledByteLimits(input string) []int {
	total := len(input)
	var out []int
	for _, v := range []int{0, 1, 2, 3, 4, 5, 8, total / 2, total - 4, total - 1, total, total + 1} {
		if v >= 0 && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

func TestSanitizeOutput(t *testing.T) {
	if got := SanitizeOutput("a\x00b\tc\nd\re\u0007f\ufff9g\ufffbh😀"); got != "ab\tc\ndefgh😀" {
		t.Fatalf("got %q", got)
	}
}

func TestBoundOutput(t *testing.T) {
	cases := []struct {
		text   string
		limits Limits
		want   bounded
	}{
		// Within the limits: unchanged.
		{"a\nb\n", head(2), bounded{"a\nb\n", 0, 0}},
		{"a\nb", tail(2), bounded{"a\nb", 0, 0}},
		{"", tail(2), bounded{"", 0, 0}},
		// A zero limit keeps nothing.
		{"ab\ncd\n", head(10, 0), bounded{"", 6, 2}},
		{"ab\ncd\n", tail(0), bounded{"", 6, 2}},
		// Exact slices of whole lines, trailing newline included.
		{"a\nb\nc\n", head(2), bounded{"a\nb\n", 2, 1}},
		{"a\nb\nc\n", tail(2), bounded{"b\nc\n", 2, 1}},
		{"a\nb\nc", tail(2), bounded{"b\nc", 2, 1}},
		{"a\nb\nc\n\n", tail(3), bounded{"b\nc\n\n", 2, 1}},
		// The byte limit cuts on whole lines when possible.
		{"aa\nbb\ncc\n", head(10, 7), bounded{"aa\nbb\n", 3, 1}},
		{"aa\nbb\ncc\n", tail(10, 7), bounded{"bb\ncc\n", 3, 1}},
		// A single line longer than the byte limit is cut on a character boundary.
		{"ééé\n", head(10, 5), bounded{"éé", 3, 0}},
		{"x\néééé", tail(10, 5), bounded{"éé", 6, 1}},
		// A U+FEFF at the start of a kept slice stays.
		{"x\ufeffa", tail(10, 4), bounded{"\ufeffa", 1, 0}},
	}
	for _, c := range cases {
		if got := bound(c.text, c.limits); got != c.want {
			t.Errorf("bound(%q, %+v) = %+v, want %+v", c.text, c.limits, got, c.want)
		}
	}
}

func TestBoundOutputReadsMalformedBytesAsReplacements(t *testing.T) {
	// pi's "a\ud83d" encodes as "a" + U+FFFD; Go's malformed byte does too.
	if got := bound("a\xff", tail(10, 3)); got != (bounded{"�", 1, 0}) {
		t.Fatalf("got %+v", got)
	}
	if UTF8ByteLength("a\xff") != 4 || UTF8ByteLength("é") != 2 {
		t.Fatal("UTF8ByteLength")
	}
}

func TestBoundOutputTailsLikeBuffer(t *testing.T) {
	// pi's surrogate edge cases, encoded.
	for _, input := range []string{"a�", "�b", "a�b", "�🙂", "🙂�", "👩\u200d💻"} {
		assertMatchesBufferTail(t, input, nil)
	}
	alphabet := []string{"a", "\u007f", "\u0080", "é", "߿", "ࠀ", "中", "퟿", "�", "�", "�", "�", "🙂", "\ue000", "￿"}
	var exhaustive func(prefix string, depth int)
	exhaustive = func(prefix string, depth int) {
		assertMatchesBufferTail(t, prefix, sampledByteLimits(prefix))
		if depth == 0 {
			return
		}
		for _, c := range alphabet {
			exhaustive(prefix+c, depth-1)
		}
	}
	exhaustive("", 3)
	seed := uint32(0x12345678)
	random := func() float64 {
		seed = seed*1664525 + 1013904223
		return float64(seed) / 0x100000000
	}
	for range 1000 {
		var b strings.Builder
		n := int(random() * 80)
		for range n {
			b.WriteString(alphabet[int(random()*float64(len(alphabet)))])
		}
		assertMatchesBufferTail(t, b.String(), sampledByteLimits(b.String()))
	}
}

func mustPush(t *testing.T, o *OutputBuffer, chunk any, skipped *Skip) {
	t.Helper()
	var err error
	switch c := chunk.(type) {
	case string:
		_, err = o.PushString(c, skipped)
	case []byte:
		_, err = o.Push(c, skipped)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestOutputBufferTotalsAndSplitUTF8(t *testing.T) {
	o := NewOutputBuffer(tail(2))
	b := []byte("😀\n")
	mustPush(t, o, "a\nb\n", nil)
	mustPush(t, o, b[:2], nil)
	mustPush(t, o, b[2:], nil)
	if got := o.Snapshot(); got != (Bounded{"b\n😀\n", 2, 1}) {
		t.Fatalf("got %+v", got)
	}
}

func TestOutputBufferSanitizesButCountsRaw(t *testing.T) {
	o := NewOutputBuffer(tail(1))
	mustPush(t, o, "a\u0007\n", nil)
	if got := o.Snapshot(); got != (Bounded{"a\n", 0, 0}) {
		t.Fatalf("got %+v", got)
	}
	mustPush(t, o, "b\u001b\n", nil)
	if got := o.Snapshot(); got != (Bounded{"b\n", 3, 1}) {
		t.Fatalf("got %+v", got)
	}
}

func TestOutputBufferDropsBOMOnlyAtStart(t *testing.T) {
	bom := []byte{0xef, 0xbb, 0xbf}
	o := NewOutputBuffer(tail(10))
	mustPush(t, o, bom[:1], nil)
	mustPush(t, o, []byte{0xbb, 0xbf, 0x61}, nil)
	mustPush(t, o, "b", nil)
	// A U+FEFF after a string chunk is text.
	mustPush(t, o, append(slices.Clone(bom), 0x63), nil)
	o.End()
	if got := o.Snapshot().Text; got != "ab\ufeffc" {
		t.Fatalf("got %q", got)
	}
}

func TestOutputBufferFlushesIncompleteCharacters(t *testing.T) {
	o := NewOutputBuffer(tail(10))
	euro := []byte("€")
	mustPush(t, o, euro[:1], nil)
	mustPush(t, o, "x", nil)
	mustPush(t, o, euro[:2], nil)
	o.End()
	if got := o.Snapshot().Text; got != "�x�" {
		t.Fatalf("got %q", got)
	}
}

func TestOutputBufferMatchesWholeStream(t *testing.T) {
	for _, limits := range []Limits{head(3, 40), tail(3, 40), head(50, 25), tail(50, 25)} {
		o := NewOutputBuffer(limits)
		stream := ""
		for i := range 300 {
			chunk := fmt.Sprintf("line %d\n", i)
			if i%7 == 0 {
				chunk = strings.Repeat("é", i%30) + "\n"
			}
			stream += chunk
			mustPush(t, o, chunk, nil)
			if i%5 != 4 {
				continue
			}
			want := BoundOutput(stream, limits)
			if got := o.Snapshot(); got != (Bounded{want.Text, want.DroppedBytes, want.DroppedLines}) {
				t.Fatalf("%+v at %d: got %+v, want %+v", limits, i, got, want)
			}
		}
	}
}

func TestOutputBufferHeadStopsStoring(t *testing.T) {
	o := NewOutputBuffer(head(2))
	for i := range 1000 {
		mustPush(t, o, fmt.Sprintf("line %d\n", i), nil)
	}
	if o.StoredBytes() >= 20 {
		t.Fatalf("stored %d bytes", o.StoredBytes())
	}
	if got := o.Snapshot(); got != (Bounded{"line 0\nline 1\n", 8876, 998}) {
		t.Fatalf("got %+v", got)
	}
}

func TestOutputBufferTailStoresOnlyTheWindow(t *testing.T) {
	o := NewOutputBuffer(tail(3, 100))
	stream := ""
	for i := range 2000 {
		chunk := fmt.Sprintf("line %d\n\n", i)
		stream += chunk
		mustPush(t, o, chunk, nil)
		snap := o.Snapshot()
		if o.StoredBytes() > 100 {
			t.Fatalf("stored %d bytes", o.StoredBytes())
		}
		if want := BoundOutput(stream, tail(3, 100)).Text; snap.Text != want {
			t.Fatalf("at %d: %q, want %q", i, snap.Text, want)
		}
	}
}

// mulberry32 is pi's deterministic PRNG for the skip tests.
func mulberry32(seed uint32) func() float64 {
	state := seed
	return func() float64 {
		state += 0x6d2b79f5
		t := state
		t = (t ^ t>>15) * (t | 1)
		t ^= t + (t^t>>7)*(t|61)
		return float64(t^t>>14) / 4294967296
	}
}

var skipAlphabet = []string{"a", "b", "z", " ", "\n", "\n", "\n", "\r\n", "\t", "é", "€", "😀", "�", "\x01", "\x1b"}

func randomChunk(next func() float64) string {
	n := int(next() * 12)
	var b strings.Builder
	for range n {
		b.WriteString(skipAlphabet[int(next()*float64(len(skipAlphabet)))])
	}
	return b.String()
}

func measure(text string) Skip {
	return Skip{Bytes: len(text), Newlines: strings.Count(text, "\n"), EndsWithNewline: strings.HasSuffix(text, "\n")}
}

func exceedsWindow(text string, limits Limits) bool {
	m := measure(text)
	return m.Bytes > limits.MaxBytes || m.Newlines > limits.MaxLines
}

// boundaries are the character boundaries of text.
func boundaries(text string) []int {
	out := []int{0}
	for i := range text {
		if i > 0 {
			out = append(out, i)
		}
	}
	return append(out, len(text))
}

func randomTailLimits(next func() float64) Limits {
	return Limits{MaxBytes: 1 + int(next()*40), MaxLines: 1 + int(next()*5), Retain: RetainTail}
}

func TestOutputBufferSameTailWheneverSnapshotsHappen(t *testing.T) {
	for seed := uint32(1); seed <= 3000; seed++ {
		next := mulberry32(seed)
		limits := randomTailLimits(next)
		plain, sampled := NewOutputBuffer(limits), NewOutputBuffer(limits)
		chunks := int(next() * 30)
		for range chunks {
			chunk := randomChunk(next)
			mustPush(t, plain, chunk, nil)
			mustPush(t, sampled, chunk, nil)
			if next() < 0.4 {
				sampled.Snapshot()
			}
		}
		if a, b := sampled.Snapshot(), plain.Snapshot(); a != b {
			t.Fatalf("seed %d: %+v != %+v", seed, a, b)
		}
	}
}

func TestOutputBufferMatchesFullStreamForEverySkip(t *testing.T) {
	for seed := uint32(1); seed <= 3000; seed++ {
		next := mulberry32(seed)
		limits := randomTailLimits(next)
		full, skipping := NewOutputBuffer(limits), NewOutputBuffer(limits)
		pending := ""
		flush := func() {
			if pending == "" {
				return
			}
			var cuts []int
			for _, cut := range boundaries(pending) {
				if cut > 0 && exceedsWindow(pending[cut:], limits) {
					cuts = append(cuts, cut)
				}
			}
			if len(cuts) > 0 && next() < 0.7 {
				cut := cuts[int(next()*float64(len(cuts)))]
				m := measure(pending[:cut])
				mustPush(t, skipping, pending[cut:], &m)
			} else {
				mustPush(t, skipping, pending, nil)
			}
			pending = ""
			if next() < 0.5 {
				skipping.Snapshot()
			}
			if a, b := skipping.Snapshot(), full.Snapshot(); a != b {
				t.Fatalf("seed %d: %+v != %+v", seed, a, b)
			}
		}
		chunks := int(next() * 40)
		for range chunks {
			chunk := randomChunk(next)
			mustPush(t, full, chunk, nil)
			if next() < 0.3 {
				full.Snapshot()
			}
			pending += chunk
			if next() < 0.3 {
				flush()
			}
		}
		flush()
		full.End()
		skipping.End()
		if a, b := skipping.Snapshot(), full.Snapshot(); a != b {
			t.Fatalf("seed %d: %+v != %+v", seed, a, b)
		}
	}
}

func TestOutputBufferCountsSkippedExactly(t *testing.T) {
	limits := Limits{MaxBytes: 1000, MaxLines: 2, Retain: RetainTail}
	full, skipping := NewOutputBuffer(limits), NewOutputBuffer(limits)
	omitted, kept := "one\ntwo\nthr", "ee\nfour\nfive\nsix"
	mustPush(t, full, omitted+kept, nil)
	m := measure(omitted)
	mustPush(t, skipping, kept, &m)
	if a, b := skipping.Snapshot(), full.Snapshot(); a != b {
		t.Fatalf("%+v != %+v", a, b)
	}
	if got := skipping.Snapshot(); got != (Bounded{"five\nsix", 19, 4}) {
		t.Fatalf("got %+v", got)
	}
}

func TestOutputBufferRefusesSkipsForHead(t *testing.T) {
	o := NewOutputBuffer(Limits{MaxBytes: 10, MaxLines: 2, Retain: RetainHead})
	_, err := o.PushString("x\ny\nz\n", &Skip{Bytes: 3, Newlines: 1, EndsWithNewline: true})
	if !errors.Is(err, ErrSkipNeedsTail) || !strings.Contains(err.Error(), "tail retention") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecoderReplacesMaximalSubparts(t *testing.T) {
	var d decoder
	// WHATWG: E2 82 is one incomplete sequence, then "A"; F0 80 is an invalid
	// second byte, so F0 is one error and 80 another.
	got := d.decode([]byte{0xe2, 0x82, 0x41, 0xf0, 0x80, 0x42, 0xed, 0xa0, 0x80})
	if want := "�A��B���"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatal("invalid output")
	}
}

// fakeClock fires timers when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// advance moves the clock and fires the timers that came due, outside its lock.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	keep := c.timers[:0]
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.now) {
			t.stopped = true
			due = append(due, t)
		} else if !t.stopped {
			keep = append(keep, t)
		}
	}
	c.timers = keep
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

func (c *fakeClock) ms() int64 { return c.Now().Sub(time.Unix(0, 0)).Milliseconds() }

// idle waits until no commit is in flight.
func (p *Progress) idle() {
	for {
		p.mu.Lock()
		ch := p.inFlight
		p.mu.Unlock()
		if ch == nil {
			return
		}
		<-ch
	}
}

func newTestProgress(write func() (int, error), onError func(error), minInterval time.Duration) (*Progress, *fakeClock) {
	c := &fakeClock{now: time.Unix(0, 0)}
	p := NewProgress(write, onError, minInterval)
	p.clock = c
	return p, c
}

func TestProgressPacesCommitsBySize(t *testing.T) {
	var mu sync.Mutex
	var commits []int64
	size := 50 * 1024
	var clk *fakeClock
	p, clk := newTestProgress(func() (int, error) {
		mu.Lock()
		defer mu.Unlock()
		commits = append(commits, clk.ms())
		return size, nil
	}, func(error) {}, 100*time.Millisecond)
	got := func() []int64 { p.idle(); mu.Lock(); defer mu.Unlock(); return slices.Clone(commits) }

	p.Mark()
	if c := got(); !slices.Equal(c, []int64{0}) {
		t.Fatalf("commits %v", c)
	}
	// 50 KiB buys 500 ms; changes meanwhile coalesce into one commit.
	mu.Lock()
	size = 10
	mu.Unlock()
	p.Mark()
	p.Mark()
	clk.advance(499 * time.Millisecond)
	if c := got(); !slices.Equal(c, []int64{0}) {
		t.Fatalf("commits %v", c)
	}
	clk.advance(time.Millisecond)
	if c := got(); !slices.Equal(c, []int64{0, 500}) {
		t.Fatalf("commits %v", c)
	}
	// A small commit still waits the minimum 100 ms.
	p.Mark()
	clk.advance(99 * time.Millisecond)
	if c := got(); !slices.Equal(c, []int64{0, 500}) {
		t.Fatalf("commits %v", c)
	}
	clk.advance(time.Millisecond)
	if c := got(); !slices.Equal(c, []int64{0, 500, 600}) {
		t.Fatalf("commits %v", c)
	}
}

func TestProgressWaitsTheMinimumInterval(t *testing.T) {
	var mu sync.Mutex
	var commits []int64
	var clk *fakeClock
	p, clk := newTestProgress(func() (int, error) {
		mu.Lock()
		defer mu.Unlock()
		commits = append(commits, clk.ms())
		return 10, nil
	}, func(error) {}, 500*time.Millisecond)
	got := func() []int64 { p.idle(); mu.Lock(); defer mu.Unlock(); return slices.Clone(commits) }
	p.Mark()
	got()
	p.Mark()
	clk.advance(499 * time.Millisecond)
	if c := got(); !slices.Equal(c, []int64{0}) {
		t.Fatalf("commits %v", c)
	}
	clk.advance(time.Millisecond)
	if c := got(); !slices.Equal(c, []int64{0, 500}) {
		t.Fatalf("commits %v", c)
	}
}

func TestProgressRejectsWaitersOfAFailedCommit(t *testing.T) {
	failure := errors.New("commit failed")
	var mu sync.Mutex
	var reported []error
	p := NewProgress(func() (int, error) { return 0, failure }, func(err error) {
		mu.Lock()
		reported = append(reported, err)
		mu.Unlock()
	}, 100*time.Millisecond)
	w := p.MarkAndWait()
	<-w.Done()
	if w.err != failure {
		t.Fatalf("waiter error %v", w.err)
	}
	p.idle()
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || reported[0] != failure {
		t.Fatalf("reported %v", reported)
	}
}

func TestProgressStopHandsBackUncoveredWaiters(t *testing.T) {
	release := make(chan struct{})
	p, _ := newTestProgress(func() (int, error) { <-release; return 0, nil }, func(error) {}, 100*time.Millisecond)
	first := p.MarkAndWait()
	second := p.MarkAndWait()
	stopped := make(chan []*ProgressWaiter)
	go func() { stopped <- p.Stop() }()
	close(release)
	pending := <-stopped
	<-first.Done()
	if len(pending) != 1 || pending[0] != second {
		t.Fatalf("pending %d waiters", len(pending))
	}
	pending[0].Resolve()
	<-second.Done()
}
