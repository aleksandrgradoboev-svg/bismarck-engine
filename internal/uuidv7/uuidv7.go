// Package uuidv7 is pi's uuidv7() (packages/ai/src/utils/uuid.ts): one
// process-wide generator of time-ordered v7 UUIDs, shared by the coding agent's
// session ids and the durable harness's provider identities, as the one
// module-scoped generator is upstream.
package uuidv7

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const (
	// MaxTimestamp is pi's MAX_UUID_V7_TIMESTAMP: the largest millisecond
	// timestamp a v7 UUID's 48-bit time field can carry.
	MaxTimestamp = int64(0xffffffffffff)
	// MaxSequence is pi's MAX_SEQUENCE: the ceiling of the 41-bit counter
	// packed into bytes 6-11.
	MaxSequence = uint64(1)<<41 - 1
)

// New mirrors pi's uuidv7() with no argument: a time-ordered v7 UUID (version
// nibble 7, RFC-4122 variant) in canonical 8-4-4-4-12 form, stamped with the
// wall clock — never earlier than the last clock-stamped id, so a clock that
// steps backwards cannot un-order ids.
//
// It returns an error exactly where pi throws its RangeError and no id can be
// produced: a system clock outside [1970-01-01, +10889] (unrepresentable in 48
// bits — a machine whose RTC has been reset to a pre-1970 date reaches this)
// or the process's 41-bit sequence exhausted. Both mean the generator cannot
// mint a valid, unique id at all; the alternative would be silently emitting
// duplicate ids. The counter is seeded with 40 random bits, so exhaustion needs
// upwards of 2^40 ids from one process; restarting the process reseeds it.
func New() (string, error) {
	return next(nil)
}

// Must is the panicking convenience form of New, for callers that cannot yet
// propagate an error. Every caller that returns an error uses New instead, and
// so should any new one.
func Must() string {
	id, err := New()
	if err != nil {
		panic(err)
	}
	return id
}

// At mirrors pi's uuidv7(timestampMs): a v7 UUID carrying timestampMs
// verbatim, for "follower" ids that must sort at a known point in time. Unlike
// New it does not advance — or read — the clock-stamped high-water mark, so a
// follower id cannot drag ordinary ids forward.
func At(timestampMs int64) (string, error) {
	return next(&timestampMs)
}

// next is the shared generator. A nil timestampMs is pi's
// `timestampMs === undefined`: read the clock, and floor the result at (and
// store it as) the clock-stamped high-water mark.
//
// Go has no BigInt, and none is needed: the sequence is 41 bits and the
// timestamp 48, both of which fit a uint64/int64 exactly.
func next(timestampMs *int64) (string, error) {
	clockStamped := timestampMs == nil
	requested := clock()
	if !clockStamped {
		requested = *timestampMs
	}
	if requested < 0 || requested > MaxTimestamp {
		return "", fmt.Errorf(
			"UUIDv7 timestamp must be an integer between 0 and %d, got %d: pass a Unix time in milliseconds (a value outside that range does not fit a v7 UUID's 48-bit time field)",
			MaxTimestamp, requested)
	}

	var random [16]byte
	fill(random[:])
	// Seed 40 of the 41 counter bits from random bytes 1-5, leaving at least
	// 2^40 ordered values before exhaustion. Those bytes are then overwritten by
	// the timestamp; the tail bytes 12-15 stay fresh on every id.
	seed := uint64(random[1])<<32 |
		uint64(random[2])<<24 |
		uint64(random[3])<<16 |
		uint64(random[4])<<8 |
		uint64(random[5])

	seq, effective, err := reserve(seed, requested, clockStamped)
	if err != nil {
		return "", err
	}

	var b [16]byte
	ts := uint64(effective)
	for i := 0; i < 6; i++ {
		b[i] = byte(ts >> ((5 - i) * 8))
	}
	b[6] = 0x70 | byte((seq>>37)&0x0f)
	b[7] = byte((seq >> 29) & 0xff)
	b[8] = 0x80 | byte((seq>>23)&0x3f)
	b[9] = byte((seq >> 15) & 0xff)
	b[10] = byte((seq >> 7) & 0xff)
	b[11] = byte((seq&0x7f)<<1) | (random[11] & 0x01)
	copy(b[12:], random[12:])

	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// reserve advances the process-wide generator state under mu and hands back
// the sequence value and effective timestamp for one id. seed supplies the 40
// random bits used the first (and only) time the counter is seeded. Keeping the
// whole critical section in one function means the unlock is a single defer
// rather than one per exit path.
func reserve(seed uint64, requestedMs int64, clockStamped bool) (seq uint64, effectiveMs int64, err error) {
	mu.Lock()
	defer mu.Unlock()

	effectiveMs = requestedMs
	if clockStamped {
		if effectiveMs < lastOrdinaryTimestamp {
			effectiveMs = lastOrdinaryTimestamp
		}
		lastOrdinaryTimestamp = effectiveMs
	}
	switch {
	case !sequenceSeeded:
		sequence = seed
		sequenceSeeded = true
	case sequence == MaxSequence:
		return 0, 0, fmt.Errorf(
			"UUIDv7 generator sequence exhausted after %d ids: restart the process to reseed the counter",
			MaxSequence)
	default:
		sequence++
	}
	return sequence, effectiveMs, nil
}

var (
	mu sync.Mutex
	// lastOrdinaryTimestamp is pi's lastOrdinaryTimestamp, starting below every
	// valid timestamp so the first clock-stamped id is never floored.
	lastOrdinaryTimestamp int64 = -1
	// sequence is pi's module-scoped `sequence`; sequenceSeeded stands in for
	// its `undefined` state, which a uint64 cannot express (0 is a reachable
	// seed).
	sequence       uint64
	sequenceSeeded bool

	// clock and fill are the generator's clock and randomness. They are vars
	// because that is the only seam pi's own tests use: uuid.test.ts drives the
	// clock-rollback floor with vi.setSystemTime and pins which random byte
	// reaches which uuid byte with vi.stubGlobal("crypto", ...). Production code
	// must never reassign them; tests of other packages go through
	// OverrideClockForTest.
	clock = func() int64 { return time.Now().UnixMilli() }
	fill  = func(b []byte) { _, _ = rand.Read(b) }
)

// testMu serializes tests that take over the process-wide generator, so a
// t.Parallel added later serializes them instead of turning goldens flaky.
var testMu sync.Mutex

// takeOver saves the whole generator — counter, high-water mark, clock and
// randomness — and returns the function restoring it. pi keeps exactly this
// state at module scope and its own tests take it over the same way
// (vi.setSystemTime, vi.stubGlobal("crypto", ...), afterEach restore).
func takeOver() (restore func()) {
	testMu.Lock()
	mu.Lock()
	savedSeq, savedSeeded, savedLast := sequence, sequenceSeeded, lastOrdinaryTimestamp
	mu.Unlock()
	savedClock, savedFill := clock, fill
	return func() {
		mu.Lock()
		sequence, sequenceSeeded, lastOrdinaryTimestamp = savedSeq, savedSeeded, savedLast
		mu.Unlock()
		clock, fill = savedClock, savedFill
		testMu.Unlock()
	}
}

// OverrideClockForTest is the seam for tests of other packages: it takes the
// generator over, clears the high-water mark, reads ms as the clock, and
// returns the function restoring everything. Production code must never call
// it.
func OverrideClockForTest(ms func() int64) (restore func()) {
	restore = takeOver()
	mu.Lock()
	lastOrdinaryTimestamp = -1
	mu.Unlock()
	clock = ms
	return restore
}
