package coding

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// entryIDRe is the shape of pi's generateId output: randomUUID().slice(0, 8).
var entryIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

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
	ids := []string{mustUUIDv7(), mustUUIDv7()}
	// Cross a millisecond boundary: the counter must not restart.
	time.Sleep(2 * time.Millisecond)
	ids = append(ids, mustUUIDv7(), mustUUIDv7())

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
// Two such tests never overlap: the helper holds uuidGeneratorTestMu for the
// test's lifetime, so a t.Parallel added later serializes them instead of
// turning their goldens flaky.
func withUUIDGenerator(t *testing.T) {
	t.Helper()
	uuidGeneratorTestMu.Lock()
	uuidMu.Lock()
	savedSeq, savedSeeded, savedLast := uuidSequence, uuidSequenceSeeded, uuidLastOrdinaryTimestamp
	uuidMu.Unlock()
	savedNow, savedRandom := uuidNow, uuidRandom
	t.Cleanup(func() {
		uuidMu.Lock()
		uuidSequence, uuidSequenceSeeded, uuidLastOrdinaryTimestamp = savedSeq, savedSeeded, savedLast
		uuidMu.Unlock()
		uuidNow, uuidRandom = savedNow, savedRandom
		uuidGeneratorTestMu.Unlock()
	})
}

var uuidGeneratorTestMu sync.Mutex

// withUUIDSequence pins the process-wide counter at seq for one test.
func withUUIDSequence(t *testing.T, seq uint64) {
	t.Helper()
	withUUIDGenerator(t)
	uuidMu.Lock()
	uuidSequence, uuidSequenceSeeded = seq, true
	uuidMu.Unlock()
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
	if maxUUIDv7Timestamp != 281474976710655 {
		t.Fatalf("maxUUIDv7Timestamp = %d, want 281474976710655 (2^48-1, pi MAX_UUID_V7_TIMESTAMP)", maxUUIDv7Timestamp)
	}
	// pi: const MAX_SEQUENCE = (1n << 41n) - 1n  (2**41 - 1)
	if maxUUIDv7Sequence != 2199023255551 {
		t.Fatalf("maxUUIDv7Sequence = %d, want 2199023255551 (2^41-1, pi MAX_SEQUENCE)", maxUUIDv7Sequence)
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
	uuidMu.Lock()
	uuidLastOrdinaryTimestamp = -1
	uuidMu.Unlock()
	clock := timestamp
	uuidNow = func() int64 { return clock }

	mint := func() string {
		t.Helper()
		id, err := uuidv7Now()
		if err != nil {
			t.Fatalf("uuidv7Now: %v", err)
		}
		return id
	}

	first, second := mint(), mint()
	clock = timestamp - 1
	afterRollback := mint()
	clock = timestamp + 1
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
	f1, err := uuidv7At(follower)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
	}
	f2, err := uuidv7At(follower)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
	}
	for _, id := range []string{f1, f2} {
		if got := uuidV7Timestamp(t, id); got != follower {
			t.Fatalf("follower timestamp %d not preserved, got %d (%q)", follower, got, id)
		}
	}
	if f1 == f2 {
		t.Fatalf("repeated follower ids must differ: %q", f1)
	}
	clock = timestamp - 1000
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
	uuidRandom = countingRandom(0x10) // random[i] == 0x10+i
	uuidMu.Lock()
	uuidSequence, uuidSequenceSeeded = 0, false
	uuidMu.Unlock()

	id, err := uuidv7At(1)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
	}
	// random[1..5] == 0x11,0x12,0x13,0x14,0x15 packed big-endian.
	const wantSeed = uint64(0x1112131415)
	if got := uuidV7Sequence(t, id); got != wantSeed {
		t.Fatalf("first id carries sequence %#x, want the seed %#x (%q)", got, wantSeed, id)
	}
	// The seed is taken once: the next id is seed+1, not a fresh seed.
	next, err := uuidv7At(1)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
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

	id, err := uuidv7At(ts)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
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
		next, err := uuidv7At(ts)
		if err != nil {
			t.Fatalf("uuidv7At: %v", err)
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
	uuidRandom = countingRandom(0x10)

	id, err := uuidv7At(ts)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
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

	first, err := uuidv7At(follower)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
	}
	second, err := uuidv7At(follower)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
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
	ordinary := uuidV7Timestamp(t, mustUUIDv7())
	if ordinary <= follower {
		t.Fatalf("clock-stamped id took the past follower's timestamp: got %d", ordinary)
	}

	// A follower in the future is preserved too, and must NOT become the floor
	// for the clock-stamped ids that follow it.
	future := time.Now().UnixMilli() + 24*60*60*1000
	ahead, err := uuidv7At(future)
	if err != nil {
		t.Fatalf("uuidv7At: %v", err)
	}
	if got := uuidV7Timestamp(t, ahead); got != future {
		t.Fatalf("future follower timestamp not preserved: got %d, want %d", got, future)
	}
	if got := uuidV7Timestamp(t, mustUUIDv7()); got >= future {
		t.Fatalf("future follower leaked into the clock-stamped high-water mark: got %d", got)
	}
}

// TestUUIDv7TimestampRange pins pi's RangeError boundaries: 0 and 2^48-1 are
// accepted and round-trip, anything outside is rejected. Go's int64 parameter
// makes pi's Number.isInteger/NaN/Infinity half of the guard unrepresentable.
func TestUUIDv7TimestampRange(t *testing.T) {
	for _, ts := range []int64{0, maxUUIDv7Timestamp} {
		id, err := uuidv7At(ts)
		if err != nil {
			t.Fatalf("uuidv7At(%d): %v", ts, err)
		}
		if got := uuidV7Timestamp(t, id); got != ts {
			t.Fatalf("uuidv7At(%d) carries %d (%q)", ts, got, id)
		}
	}
	for _, ts := range []int64{-1, maxUUIDv7Timestamp + 1} {
		if id, err := uuidv7At(ts); err == nil {
			t.Fatalf("uuidv7At(%d) should be out of range, got %q", ts, id)
		}
	}
}

// TestUUIDv7SequenceExhaustion pins pi's "sequence exhausted" throw: the
// counter stops at MAX_SEQUENCE rather than wrapping into ids it has already
// handed out.
func TestUUIDv7SequenceExhaustion(t *testing.T) {
	withUUIDSequence(t, maxUUIDv7Sequence-1)

	last, err := uuidv7At(1)
	if err != nil {
		t.Fatalf("the final sequence value must still mint an id: %v", err)
	}
	if got := uuidV7Sequence(t, last); got != maxUUIDv7Sequence {
		t.Fatalf("final id carries sequence %d, want %d", got, maxUUIDv7Sequence)
	}
	if id, err := uuidv7At(1); err == nil {
		t.Fatalf("an exhausted counter must not mint %q", id)
	}
}

// readSessionFileRoles mirrors pi's test utility of the same name: one label
// per record in a session file, the message role for message entries and the
// entry type otherwise.
func readSessionFileRoles(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	var roles []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record struct {
			Type    string `json:"type"`
			Message *struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if record.Message != nil && record.Message.Role != "" {
			roles = append(roles, record.Message.Role)
		} else {
			roles = append(roles, record.Type)
		}
	}
	return roles
}

// A session with only setup entries stays in memory: no file, and it is
// invisible to ListSessions (pi _persist, upstream ff72faba2).
func TestSessionWithOnlySetupEntriesAbsentFromDisk(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, &ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"}, "off")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Fatalf("session file should not exist yet, stat err=%v", err)
	}
	if infos := ListSessions(cwd, ""); len(infos) != 0 {
		t.Fatalf("ListSessions should be empty, got %+v", infos)
	}
}

// The first user message creates the file, so the prompt survives a first
// turn that never produces an assistant message (pi #10000).
func TestFirstUserMessageCreatesSessionFile(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, &ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rec.RecordMessage(ai.NewUserText("first question", 1))

	want := []string{"session", "model_change", "user"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
	messages, err := LoadSessionMessages(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("reopened session has %d messages, want 1", len(messages))
	}
	if infos := ListSessions(cwd, ""); len(infos) != 1 {
		t.Fatalf("ListSessions should show 1 session, got %+v", infos)
	}
}

// Characterization: once the file exists, each later entry is appended to it
// once. pi appends a custom entry between the messages; the recorder has no
// custom entries, so a thinking-level change stands in for it.
func TestLaterSessionEntriesAppend(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rec.RecordMessage(ai.NewUserText("first question", 1))
	rec.RecordThinkingLevel("medium")
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "first answer"}}, StopReason: ai.StopStop, Timestamp: 2})

	want := []string{"session", "user", "thinking_level_change", "assistant"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
}

// Only a user or assistant message creates the file: a system message or a
// tool result alone stays in memory like the setup entries, and is written
// with them once the conversation starts (pi _hasConversation).
func TestNonConversationMessagesStayOffDisk(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rec.RecordMessage(ai.NewSystemText("sys", 1))
	rec.RecordMessage(ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: "out"}}, Timestamp: 2})
	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Fatalf("a system message and a tool result must not create the file, stat err=%v", err)
	}

	rec.RecordMessage(ai.NewUserText("first question", 3))
	want := []string{"session", "system", "toolResult", "user"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
}

// Once an entry is on disk the recorder lets go of it: pending holds only the
// entries of a session that has no file yet, for a new session and a resumed
// one alike.
func TestSessionRecorderKeepsNoWrittenEntries(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, &ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"}, "off")
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(ai.NewUserText("first question", 1))
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "first answer"}}, StopReason: ai.StopStop, Timestamp: 2})
	if n := len(rec.pending); n != 0 {
		t.Fatalf("new session recorder still holds %d written entries", n)
	}
	rec.Close()

	resumed, err := ResumeSession(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.RecordMessage(ai.NewUserText("second question", 3))
	if n := len(resumed.pending); n != 0 {
		t.Fatalf("resumed session recorder still holds %d written entries", n)
	}
	want := []string{"session", "model_change", "thinking_level_change", "user", "assistant", "user"}
	if got := readSessionFileRoles(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("session file records = %v, want %v", got, want)
	}
}

// TestStartSessionReturnsAnErrorWhenNoIDCanBeMinted pins the shape of the
// failure pi has here: uuidv7() throws a catchable RangeError, and StartSession
// already reports its other failures (no agent dir, MkdirAll) as errors. A
// machine whose RTC has been reset to a pre-1970 date must therefore get an
// error naming the fix, not a process abort from inside the id generator.
func TestStartSessionReturnsAnErrorWhenNoIDCanBeMinted(t *testing.T) {
	setTestHome(t, t.TempDir())
	withUUIDGenerator(t)
	uuidMu.Lock()
	uuidLastOrdinaryTimestamp = -1
	uuidMu.Unlock()
	uuidNow = func() int64 { return -1 } // clock before the epoch

	rec, err := StartSession(t.TempDir(), nil)
	if err == nil {
		t.Fatalf("StartSession must return an error when no session id can be minted, got %+v", rec)
	}
	if !strings.Contains(err.Error(), "Unix time in milliseconds") {
		t.Fatalf("the error must say how to resolve it, got %q", err)
	}
}

// TestResumeSessionMigratesAV1FileToV3 is the write-side half of pi's
// migrateToCurrentVersion: SessionManager._loadEntries migrates in memory and,
// when anything changed, rewrites the whole file (_rewriteFile,
// session-manager.ts:961 @ 64eeb82a4) before appending. A resumed v1 file must
// therefore land on disk as a v3 file whose entries carry ids and a parentId
// chain, and the first entry appended after resume must hang off the last
// migrated entry rather than off a null parent.
func TestResumeSessionMigratesAV1FileToV3(t *testing.T) {
	path := writeSessionLines(t, []string{
		`{"type":"session","id":"0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"q"}],"timestamp":1}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"hookMessage","customType":"x","content":[{"type":"text","text":"hook"}],"timestamp":2}}`,
	})

	rec, err := ResumeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordThinkingLevel("medium")
	rec.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("rewritten file has an unparseable line %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 4 {
		t.Fatalf("expected header + 2 migrated + 1 appended entries, got %d:\n%s", len(entries), data)
	}
	if v, _ := entries[0]["version"].(float64); int(v) != CurrentSessionVersion {
		t.Fatalf("rewritten header version = %v, want %d:\n%s", entries[0]["version"], CurrentSessionVersion, data)
	}
	var prev any // nil: the first entry's parentId is null
	for i, e := range entries[1:] {
		id, _ := e["id"].(string)
		if !entryIDRe.MatchString(id) {
			t.Fatalf("entry %d id %v is not a generated 8-hex entry id:\n%s", i, e["id"], data)
		}
		if e["parentId"] != prev {
			t.Fatalf("entry %d parentId = %v, want %v (the previous entry):\n%s", i, e["parentId"], prev, data)
		}
		prev = id
	}
	if role := entries[2]["message"].(map[string]any)["role"]; role != "custom" {
		t.Fatalf("hookMessage role not migrated to custom on disk: %v", role)
	}
	if entries[3]["type"] != "thinking_level_change" {
		t.Fatalf("appended entry missing after the migrated ones: %v", entries[3])
	}
}

// TestSessionIDAndFilenameShape verifies the header id is a uuidv7 (version 7,
// RFC-4122 variant) and the filename is <iso-with-dashes>_<uuidv7>.jsonl.
func TestSessionIDAndFilenameShape(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(rec.ID()) {
		t.Fatalf("header id is not a uuidv7: %q", rec.ID())
	}
	base := filepath.Base(rec.Path())
	if !strings.HasSuffix(base, "_"+rec.ID()+".jsonl") {
		t.Fatalf("filename does not embed the uuidv7 id: %q", base)
	}
	// The timestamp portion contains no ':' or '.' (replaced with '-').
	tsPart := strings.TrimSuffix(base, "_"+rec.ID()+".jsonl")
	if strings.ContainsAny(tsPart, ":.") {
		t.Fatalf("filename timestamp not sanitized: %q", tsPart)
	}
}

// TestEntryIDFallbackIsARandomUUIDv4 pins pi's generateId fallback
// (session-manager.ts:227 @ 64eeb82a4, `return randomUUID()` after 100
// collisions): a version-4, fully random UUID. It is deliberately NOT the
// time-ordered v7 the port mints for session ids — a v7 there would write a
// different version nibble into the session file, consume a sequence slot, and
// give an id-generation path a failure mode randomUUID() does not have.
func TestEntryIDFallbackIsARandomUUIDv4(t *testing.T) {
	v4Re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	everyIDTaken := func(string) bool { return true } // 100 collisions, then the fallback
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id := genEntryID(everyIDTaken)
		if !v4Re.MatchString(id) {
			t.Fatalf("entry-id fallback is not a v4 UUID: %q", id)
		}
		if seen[id] {
			t.Fatalf("entry-id fallback repeated %q: it is not fully random", id)
		}
		seen[id] = true
	}
}

// TestEntryIDsCollisionChecked verifies entry ids are 8 hex chars and unique
// across many recorded entries.
func TestEntryIDsCollisionChecked(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	rec, _ := StartSession(cwd, nil)
	// Force a flush so entries are observable, then read ids back from disk.
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "a"}}, StopReason: ai.StopStop, Timestamp: 1})
	for i := 0; i < 50; i++ {
		rec.RecordThinkingLevel("medium")
	}
	rec.Close()

	data, _ := os.ReadFile(rec.Path())
	ids := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Type == "session" {
			continue // header id is a uuidv7, not an 8-hex entry id
		}
		if !entryIDRe.MatchString(e.ID) {
			t.Fatalf("entry id not 8 hex chars: %q (type %s)", e.ID, e.Type)
		}
		if ids[e.ID] {
			t.Fatalf("duplicate entry id: %q", e.ID)
		}
		ids[e.ID] = true
	}
}

// TestEncodeCwdStripsOneLeadingSeparator verifies the cwd encoding strips
// exactly ONE leading separator (pi replace(/^[/\\]/, "")) rather than all of
// them (the old TrimLeft bug). filepath.Abs collapses consecutive POSIX slashes,
// so this exercises encodeCwdSafePath directly on inputs that retain multiple
// leading separators (e.g. a Windows-style UNC/backslash path).
func TestEncodeCwdStripsOneLeadingSeparator(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b", "a-b"},                         // single leading slash stripped
		{"//double/leading", "-double-leading"}, // only the FIRST of two stripped
		{`\\unc\share`, `-unc-share`},           // backslash UNC: one backslash stripped
		{"/x", "x"},
	}
	for _, c := range cases {
		if got := encodeCwdSafePath(c.in); got != c.want {
			t.Fatalf("encodeCwdSafePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLoadSessionMessagesCompactedResume verifies a JSONL session containing a
// compaction entry resumes with the summary (pi wrapper text) in place of the
// pre-compaction turns, rather than naively replaying every message.
func TestLoadSessionMessagesCompactedResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	userMsg := func(text string) json.RawMessage {
		b, _ := json.Marshal(ai.NewUserText(text, 1))
		return b
	}
	asstMsg := func(text string) json.RawMessage {
		b, _ := json.Marshal(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, StopReason: ai.StopStop, Timestamp: 1})
		return b
	}

	lines := []map[string]any{
		{"type": "session", "version": CurrentSessionVersion, "id": "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000", "timestamp": "2026-06-08T00:00:00.000Z", "cwd": dir, "id_": ""},
		{"type": "message", "id": "aaaaaaaa", "parentId": nil, "message": userMsg("old question")},
		{"type": "message", "id": "bbbbbbbb", "parentId": "aaaaaaaa", "message": asstMsg("old answer")},
		{"type": "compaction", "id": "cccccccc", "parentId": "bbbbbbbb", "summary": "SUMMARY OF OLD WORK", "firstKeptEntryId": "dddddddd", "tokensBefore": 1234, "timestamp": "2026-06-08T00:01:00.000Z"},
		{"type": "message", "id": "dddddddd", "parentId": "cccccccc", "message": userMsg("recent question")},
		{"type": "message", "id": "eeeeeeee", "parentId": "dddddddd", "message": asstMsg("recent answer")},
	}
	var sb strings.Builder
	for _, l := range lines {
		b, _ := json.Marshal(l)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, err := LoadSessionMessages(path)
	if err != nil {
		t.Fatal(err)
	}

	// First reconstructed message must be the compaction summary (pi wrapper).
	if len(msgs) == 0 {
		t.Fatal("no messages reconstructed")
	}
	first, ok := msgs[0].(ai.UserMessage)
	if !ok {
		t.Fatalf("first message should be the summary user message, got %T", msgs[0])
	}
	if !strings.Contains(textOf(first.Content), "SUMMARY OF OLD WORK") {
		t.Fatalf("compaction summary missing from resumed context: %q", textOf(first.Content))
	}

	all := ""
	for _, m := range msgs {
		switch v := m.(type) {
		case ai.UserMessage:
			all += textOf(v.Content) + "\n"
		case *ai.AssistantMessage:
			for _, c := range v.Content {
				if tc, ok := c.(ai.TextContent); ok {
					all += tc.Text + "\n"
				}
			}
		case ai.AssistantMessage:
			for _, c := range v.Content {
				if tc, ok := c.(ai.TextContent); ok {
					all += tc.Text + "\n"
				}
			}
		}
	}
	// Pre-compaction turns must NOT be replayed.
	if strings.Contains(all, "old question") || strings.Contains(all, "old answer") {
		t.Fatalf("pre-compaction turns leaked into resumed context:\n%s", all)
	}
	// Recent turns must be present.
	if !strings.Contains(all, "recent question") || !strings.Contains(all, "recent answer") {
		t.Fatalf("recent turns missing from resumed context:\n%s", all)
	}
}

var _ = agent.ThinkMedium
