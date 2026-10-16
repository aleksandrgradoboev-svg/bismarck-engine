package uuidv7

import (
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// uuidV7Bytes decodes a canonical UUID into its 16 bytes.
func uuidV7Bytes(t *testing.T, id string) []byte {
	t.Helper()
	if !uuidV7Re.MatchString(id) {
		t.Fatalf("not a uuidv7: %q", id)
	}
	b, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil {
		t.Fatalf("undecodable uuid %q: %v", id, err)
	}
	return b
}

// uuidV7Sequence extracts the 41-bit counter pi packs into bytes 6-11
// (uuid.ts: 0x70|seq>>37, seq>>29, 0x80|seq>>23, seq>>15, seq>>7, seq<<1).
func uuidV7Sequence(t *testing.T, id string) uint64 {
	t.Helper()
	b := uuidV7Bytes(t, id)
	return uint64(b[6]&0x0f)<<37 |
		uint64(b[7])<<29 |
		uint64(b[8]&0x3f)<<23 |
		uint64(b[9])<<15 |
		uint64(b[10])<<7 |
		uint64(b[11]>>1)
}

// uuidV7Timestamp extracts the 48-bit millisecond timestamp from bytes 0-5.
func uuidV7Timestamp(t *testing.T, id string) int64 {
	t.Helper()
	b := uuidV7Bytes(t, id)
	var ts int64
	for _, x := range b[:6] {
		ts = ts<<8 | int64(x)
	}
	return ts
}

// TestUUIDv7SequenceIsAMonotonicCounter pins pi's post-ef3786544 counter: a
// single 41-bit sequence seeded once per process and incremented by exactly one
// on every id, whatever the clock does. Before ef3786544 the sequence was 32
// bits, was RE-SEEDED from fresh random bytes every time the millisecond
// advanced, and only its top bits reached the uuid (bytes 10-11 were mostly
// random), so neither the +1 step nor the layout held.
func TestUUIDv7SequenceIsAMonotonicCounter(t *testing.T) {
	ids := []string{Must(), Must()}
	// Cross a millisecond boundary: the counter must not restart.
	time.Sleep(2 * time.Millisecond)
	ids = append(ids, Must(), Must())

	for i := 1; i < len(ids); i++ {
		prev, cur := uuidV7Sequence(t, ids[i-1]), uuidV7Sequence(t, ids[i])
		if cur != prev+1 {
			t.Fatalf("sequence must step by exactly 1 per id: id[%d]=%d, id[%d]=%d (%q -> %q)",
				i-1, prev, i, cur, ids[i-1], ids[i])
		}
		if ids[i] <= ids[i-1] {
			t.Fatalf("ids must be lexicographically ordered: %q then %q", ids[i-1], ids[i])
		}
	}
}

// withUUIDGenerator takes ownership of the process-wide generator for one test
// — the counter, the clock-stamped high-water mark, and the clock/randomness
// sources — and restores whatever the rest of the suite left behind. pi keeps
// exactly this state at module scope and its own tests take it over the same
// way (vi.setSystemTime, vi.stubGlobal("crypto", ...), afterEach restore).
// Two such tests never overlap: takeOver holds testMu for the test's lifetime.
func withUUIDGenerator(t *testing.T) {
	t.Helper()
	t.Cleanup(takeOver())
}

// withUUIDSequence pins the process-wide counter at seq for one test.
func withUUIDSequence(t *testing.T, seq uint64) {
	t.Helper()
	withUUIDGenerator(t)
	mu.Lock()
	sequence, sequenceSeeded = seq, true
	mu.Unlock()
}

// countingRandom fills every buffer with base, base+1, base+2, ... so each byte
// index carries a distinct, known value. It stands in for upstream's
// vi.stubGlobal("crypto", {getRandomValues: bytes => bytes.fill(++randomByte)}),
// but varies per index as well so a test can tell byte 12 from byte 8.
func countingRandom(base byte) func([]byte) {
	return func(b []byte) {
		for i := range b {
			b[i] = base + byte(i)
		}
	}
}

// TestUUIDv7ConstantsMatchPi pins the two magic numbers to literals rather than
// to themselves. Every other assertion in this file reads the constants, so it
// moves with them; upstream's own test writes `2 ** 48 - 1` out longhand for
// exactly this reason (uuid.test.ts @ 64eeb82a4).
func TestUUIDv7ConstantsMatchPi(t *testing.T) {
	// pi: const MAX_UUID_V7_TIMESTAMP = 0xffffffffffff  (2**48 - 1)
	if MaxTimestamp != 281474976710655 {
		t.Fatalf("MaxTimestamp = %d, want 281474976710655 (2^48-1, pi MAX_UUID_V7_TIMESTAMP)", MaxTimestamp)
	}
	// pi: const MAX_SEQUENCE = (1n << 41n) - 1n  (2**41 - 1)
	if MaxSequence != 2199023255551 {
		t.Fatalf("MaxSequence = %d, want 2199023255551 (2^41-1, pi MAX_SEQUENCE)", MaxSequence)
	}
}

// TestUUIDv7ClockRollbackFloor is upstream uuid.test.ts's "generates ordered
// UUIDv7s while preserving follower timestamps" row: the clock-stamped branch
// floors its timestamp at the last clock-stamped one
// (`Math.max(requestedTimestamp, lastOrdinaryTimestamp)`), so a clock that steps
// backwards cannot un-order ids, while a supplied follower timestamp is written
// verbatim and never touches that high-water mark.
func TestUUIDv7ClockRollbackFloor(t *testing.T) {
	const timestamp = int64(0x0123456789ab) // upstream's TIMESTAMP

	withUUIDGenerator(t)
	mu.Lock()
	lastOrdinaryTimestamp = -1
	mu.Unlock()
	now := timestamp
	clock = func() int64 { return now }

	mint := func() string {
		t.Helper()
		id, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return id
	}

	first, second := mint(), mint()
	now = timestamp - 1
	afterRollback := mint()
	now = timestamp + 1
	afterAdvance := mint()

	ordinary := []string{first, second, afterRollback, afterAdvance}
	want := []int64{timestamp, timestamp, timestamp, timestamp + 1}
	for i, id := range ordinary {
		if got := uuidV7Timestamp(t, id); got != want[i] {
			t.Fatalf("clock-stamped id[%d] carries timestamp %d, want %d (%q)", i, got, want[i], id)
		}
	}
	for i := 1; i < len(ordinary); i++ {
		if ordinary[i] <= ordinary[i-1] {
			t.Fatalf("ids must stay ordered across a clock rollback: %q then %q", ordinary[i-1], ordinary[i])
		}
	}

	// A follower is preserved verbatim and leaves the high-water mark alone, so
	// the clock-stamped id after it is still floored at timestamp+1.
	const follower = timestamp - 1000
	f1, err := At(follower)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	f2, err := At(follower)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	for _, id := range []string{f1, f2} {
		if got := uuidV7Timestamp(t, id); got != follower {
			t.Fatalf("follower timestamp %d not preserved, got %d (%q)", follower, got, id)
		}
	}
	if f1 == f2 {
		t.Fatalf("repeated follower ids must differ: %q", f1)
	}
	now = timestamp - 1000
	if got := uuidV7Timestamp(t, mint()); got != timestamp+1 {
		t.Fatalf("clock-stamped id after a past follower carries %d, want the floor %d", got, timestamp+1)
	}
}

// TestUUIDv7SeedsTheCounterFromRandomBytes1Through5 pins pi's seed expression
// (uuid.ts: bytes[1]<<32 | bytes[2]<<24 | bytes[3]<<16 | bytes[4]<<8 | bytes[5],
// moved off bytes 6-9 by ef3786544 because those are overwritten by the
// counter). With one known byte per index the seeded counter is a literal, so
// changing any shift, dropping a term, or reading a different byte range all go
// red — none of which the +1-step test next to this one can see.
func TestUUIDv7SeedsTheCounterFromRandomBytes1Through5(t *testing.T) {
	withUUIDGenerator(t)
	fill = countingRandom(0x10) // random[i] == 0x10+i
	mu.Lock()
	sequence, sequenceSeeded = 0, false
	mu.Unlock()

	id, err := At(1)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	// random[1..5] == 0x11,0x12,0x13,0x14,0x15 packed big-endian.
	const wantSeed = uint64(0x1112131415)
	if got := uuidV7Sequence(t, id); got != wantSeed {
		t.Fatalf("first id carries sequence %#x, want the seed %#x (%q)", got, wantSeed, id)
	}
	// The seed is taken once: the next id is seed+1, not a fresh seed.
	next, err := At(1)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	if got := uuidV7Sequence(t, next); got != wantSeed+1 {
		t.Fatalf("second id carries sequence %#x, want %#x", got, wantSeed+1)
	}
}

// TestUUIDv7ByteLayout pins the absolute bit layout of ef3786544 against a
// golden computed from pi's own formula (uuid.ts: timestamp in bytes 0-5, then
// 0x70|seq>>37, seq>>29, 0x80|seq>>23, seq>>15, seq>>7, (seq&0x7f)<<1). The
// counter test next to this one only sees the +1 step between neighbouring ids,
// which survives a shift being off by one; this does not. Byte 11's low bit is
// the single random bit pi keeps there, and bytes 12-15 are the random tail.
func TestUUIDv7ByteLayout(t *testing.T) {
	const (
		// Every nibble of this counter changes when any of pi's six shifts moves
		// by one bit, so the golden below cannot survive an off-by-one.
		seq  = uint64(0x123456789AB)
		ts   = int64(0x0123456789ab)
		want = "0123456789ab791a8acf1356" // bytes 0-11, byte 11's random bit cleared
	)
	withUUIDSequence(t, seq-1) // the generator increments before it encodes

	id, err := At(ts)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	b := uuidV7Bytes(t, id)
	b[11] &^= 0x01
	if got := hex.EncodeToString(b[:12]); got != want {
		t.Fatalf("uuid bytes 0-11 = %s, want %s (id %q)", got, want, id)
	}

	// Every one of the four tail bytes is fresh randomness on each id, and byte
	// 11's low bit stays random too: (seq&0x7f)<<1 always leaves that bit clear,
	// so a set bit can only have come from the random byte pi ORs in there.
	first := uuidV7Bytes(t, id)
	varies := map[int]bool{}
	for i := 0; i < 40; i++ {
		next, err := At(ts)
		if err != nil {
			t.Fatalf("At: %v", err)
		}
		b := uuidV7Bytes(t, next)
		for idx := 12; idx < 16; idx++ {
			if b[idx] != first[idx] {
				varies[idx] = true
			}
		}
		if b[11]&0x01 != first[11]&0x01 {
			varies[11] = true
		}
	}
	for _, idx := range []int{11, 12, 13, 14, 15} {
		if !varies[idx] {
			t.Fatalf("byte %d never varied across 40 ids: it is not carrying fresh randomness", idx)
		}
	}
}

// TestUUIDv7RandomBytesReachTheTail is upstream uuid.test.ts's "uses fresh
// randomness for every UUID tail" row, made stricter: with one known byte per
// index the whole 16-byte id is a literal golden, so it pins WHICH random bytes
// pi keeps — the low bit of byte 11 and bytes 12-15 — not merely that something
// there varies. Upstream fills every byte with the same value, which cannot tell
// bytes 12-15 from bytes 8-11; this can.
func TestUUIDv7RandomBytesReachTheTail(t *testing.T) {
	const (
		seq = uint64(0x123456789AB)
		ts  = int64(0x0123456789ab)
		// bytes 0-5 timestamp, 6-11 counter with byte 11's low bit from
		// random[11] (0x1b, odd), 12-15 verbatim random[12:16].
		want = "0123456789ab791a8acf13571c1d1e1f"
	)
	withUUIDSequence(t, seq-1) // the generator increments before it encodes
	fill = countingRandom(0x10)

	id, err := At(ts)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	if got := hex.EncodeToString(uuidV7Bytes(t, id)); got != want {
		t.Fatalf("uuid bytes = %s, want %s (id %q)", got, want, id)
	}
}

// TestUUIDv7FollowerTimestampPreserved covers pi's follower ids (ef3786544): a
// supplied timestamp is written verbatim — never floored to the clock-stamped
// high-water mark — repeated follower ids stay distinct via the counter, and a
// follower does not drag the next clock-stamped id back to its timestamp.
func TestUUIDv7FollowerTimestampPreserved(t *testing.T) {
	const follower = int64(0x0123456789ab) // upstream uuid.test.ts's TIMESTAMP

	first, err := At(follower)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	second, err := At(follower)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	for _, id := range []string{first, second} {
		if got := uuidV7Timestamp(t, id); got != follower {
			t.Fatalf("follower timestamp not preserved: got %d, want %d (%q)", got, follower, id)
		}
	}
	if first == second {
		t.Fatalf("repeated follower ids must differ: %q", first)
	}
	if got, want := uuidV7Sequence(t, second), uuidV7Sequence(t, first)+1; got != want {
		t.Fatalf("follower ids share the counter: got %d, want %d", got, want)
	}

	// A follower in the past is preserved rather than floored to the clock,
	// and it does not drag the clock-stamped high-water mark backwards either.
	if follower >= time.Now().UnixMilli() {
		t.Fatalf("test premise broken: %d is no longer in the past", follower)
	}
	ordinary := uuidV7Timestamp(t, Must())
	if ordinary <= follower {
		t.Fatalf("clock-stamped id took the past follower's timestamp: got %d", ordinary)
	}

	// A follower in the future is preserved too, and must NOT become the floor
	// for the clock-stamped ids that follow it.
	future := time.Now().UnixMilli() + 24*60*60*1000
	ahead, err := At(future)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	if got := uuidV7Timestamp(t, ahead); got != future {
		t.Fatalf("future follower timestamp not preserved: got %d, want %d", got, future)
	}
	if got := uuidV7Timestamp(t, Must()); got >= future {
		t.Fatalf("future follower leaked into the clock-stamped high-water mark: got %d", got)
	}
}

// TestUUIDv7TimestampRange pins pi's RangeError boundaries: 0 and 2^48-1 are
// accepted and round-trip, anything outside is rejected. Go's int64 parameter
// makes pi's Number.isInteger/NaN/Infinity half of the guard unrepresentable.
func TestUUIDv7TimestampRange(t *testing.T) {
	for _, ts := range []int64{0, MaxTimestamp} {
		id, err := At(ts)
		if err != nil {
			t.Fatalf("At(%d): %v", ts, err)
		}
		if got := uuidV7Timestamp(t, id); got != ts {
			t.Fatalf("At(%d) carries %d (%q)", ts, got, id)
		}
	}
	for _, ts := range []int64{-1, MaxTimestamp + 1} {
		if id, err := At(ts); err == nil {
			t.Fatalf("At(%d) should be out of range, got %q", ts, id)
		}
	}
}

// TestUUIDv7SequenceExhaustion pins pi's "sequence exhausted" throw: the
// counter stops at MAX_SEQUENCE rather than wrapping into ids it has already
// handed out.
func TestUUIDv7SequenceExhaustion(t *testing.T) {
	withUUIDSequence(t, MaxSequence-1)

	last, err := At(1)
	if err != nil {
		t.Fatalf("the final sequence value must still mint an id: %v", err)
	}
	if got := uuidV7Sequence(t, last); got != MaxSequence {
		t.Fatalf("final id carries sequence %d, want %d", got, MaxSequence)
	}
	if id, err := At(1); err == nil {
		t.Fatalf("an exhausted counter must not mint %q", id)
	}
}
