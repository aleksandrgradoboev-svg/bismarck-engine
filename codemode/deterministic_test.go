package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ported from pi v1.2.0 packages/codemode/test/deterministic.test.ts.

// startedAt is Date.UTC(2026, 0, 15, 10, 0, 0).
var startedAt = float64(time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC).UnixMilli())

// The first two Math.random() values with seed 0, and the first with seed 1,
// pinned in pi's own test: equal values here mean the Go VM observes exactly
// what pi's does.
var (
	pinnedSeedZero    = "[0.6711372530266764,0.7258461452833667]"
	pinnedFirstRandom = 0.34227450605335275
)

type recorded struct {
	run         *Run
	mu          sync.Mutex
	calls       []HostCall
	idles       [][2]int
	events      []string
	fingerprint string
}

func (r *recorded) snapshot() ([]HostCall, [][2]int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]HostCall(nil), r.calls...), append([][2]int(nil), r.idles...), append([]string(nil), r.events...)
}

type startOpts struct {
	RunOptions
	answer func(HostCall) Outcome
	ctx    context.Context
	// seedZero runs with seed 0; otherwise a zero Seed means 1.
	seedZero bool
}

// start runs code with tools echo and wait. Every call but wait is answered
// at once (with its argument by default), as result 1, 2, ... with the clock at
// startedAt + 1000*seq; wait is left for the test.
func start(t *testing.T, s *Sandbox, code string, o startOpts) *recorded {
	t.Helper()
	rec := &recorded{}
	answer := o.answer
	if answer == nil {
		answer = func(c HostCall) Outcome { return Outcome{OK: true, Value: c.Args} }
	}
	opts := o.RunOptions
	if opts.StartedAt == 0 {
		opts.StartedAt = startedAt
	}
	if opts.Tools == nil {
		opts.Tools = []DeclaredTool{{Name: "echo"}, {Name: "wait"}}
	}
	if opts.Timeout == 0 {
		opts.Timeout = 20 * time.Second
	}
	var seqMu sync.Mutex
	seq := 0
	ready := make(chan struct{})
	opts.OnStarted = func(fp string) {
		rec.mu.Lock()
		rec.fingerprint = fp
		rec.events = append(rec.events, "started")
		rec.mu.Unlock()
	}
	opts.OnCall = func(c HostCall) {
		rec.mu.Lock()
		rec.calls = append(rec.calls, c)
		rec.events = append(rec.events, fmt.Sprint("call ", c.ID))
		rec.mu.Unlock()
		if c.Name == "wait" {
			return
		}
		// pi answers in a microtask; a goroutine is the closest Go form.
		go func() {
			<-ready
			seqMu.Lock()
			seq++
			n := seq
			if err := rec.run.Deliver(n, startedAt+1000*float64(n), c.ID, answer(c)); err != nil {
				t.Error(err)
			}
			seqMu.Unlock()
		}()
	}
	opts.OnIdle = func(seq, issued int) {
		rec.mu.Lock()
		rec.idles = append(rec.idles, [2]int{seq, issued})
		rec.events = append(rec.events, fmt.Sprint("idle ", seq))
		rec.mu.Unlock()
	}
	ctx := o.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Seed == 0 && !o.seedZero {
		opts.Seed = 1
	}
	run, err := s.Start(ctx, code, opts)
	if err != nil {
		t.Fatal(err)
	}
	rec.run = run
	close(ready)
	return rec
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func timeless(r Result) Result {
	calls := make([]Call, len(r.Calls))
	for i, c := range r.Calls {
		calls[i] = Call{Name: c.Name, Status: c.Status}
	}
	r.Calls = calls
	return r
}

func TestDeterministicSameRunsSameObservations(t *testing.T) {
	s := newSandbox(t, Options{})
	code := `
		const values = [Date.now(), performance.now(), Math.random(), Math.random(), new Date().toISOString()];
		const first = await tools.echo(values);
		text(JSON.stringify([Date.now(), performance.now(), Math.random()]));
		return await tools.echo(first.concat(Math.random()));
	`
	one := start(t, s, code, startOpts{})
	two := start(t, s, code, startOpts{})
	a, b := timeless(one.run.Wait()), timeless(two.run.Wait())
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("runs differ:\n%+v %s\n%+v %s", a, a.Value, b, b.Value)
	}
	calls1, _, _ := one.snapshot()
	calls2, _, _ := two.snapshot()
	if fmt.Sprint(calls1) != fmt.Sprint(calls2) {
		t.Fatalf("calls differ: %v vs %v", calls1, calls2)
	}
	var first []any
	json.Unmarshal(calls1[0].Args, &first)
	if first[0].(float64) != startedAt || first[1].(float64) != 0 || first[4] != "2026-01-15T10:00:00.000Z" {
		t.Fatalf("first call args %v", first)
	}
	var printed []float64
	json.Unmarshal([]byte(a.Output[0].Text), &printed)
	if printed[0] != startedAt+1000 || printed[1] != 1000 {
		t.Fatalf("printed %v", printed)
	}
	random := first[2].(float64)
	if random != pinnedFirstRandom {
		t.Fatalf("first random %v, pi pins %v", random, pinnedFirstRandom)
	}

	// The seed decides the random values, not the start time.
	later := start(t, s, code, startOpts{RunOptions: RunOptions{StartedAt: startedAt + 5000}})
	reseeded := start(t, s, code, startOpts{RunOptions: RunOptions{Seed: 2}})
	later.run.Wait()
	reseeded.run.Wait()
	lc, _, _ := later.snapshot()
	rc, _, _ := reseeded.snapshot()
	var lv, rv []any
	json.Unmarshal(lc[0].Args, &lv)
	json.Unmarshal(rc[0].Args, &rv)
	if lv[2].(float64) != random || rv[2].(float64) == random {
		t.Fatalf("later %v reseeded %v", lv[2], rv[2])
	}
	// Seed 0 has its own values: QuickJS turns a zero state into 1, which
	// must not make it seed 1's.
	zero := start(t, s, "return [Math.random(), Math.random()]", startOpts{seedZero: true})
	wantValue(t, zero.run.Wait(), pinnedSeedZero)
}

func TestDeterministicUTCAndNoWeakRef(t *testing.T) {
	s := newSandbox(t, Options{})
	code := "return [new Date(0).getHours(), new Date(0).getTimezoneOffset(), typeof WeakRef, typeof FinalizationRegistry]"
	wantValue(t, start(t, s, code, startOpts{}).run.Wait(), `[0,0,"undefined","undefined"]`)
	// Execute is unchanged: every intrinsic.
	r := exec(t, s, code)
	var v []any
	json.Unmarshal(r.Value, &v)
	if v[2] != "function" || v[3] != "function" {
		t.Fatalf("execute: %s", r.Value)
	}
}

func TestDeterministicSeqOrder(t *testing.T) {
	s := newSandbox(t, Options{})
	rec := start(t, s, `const order = [];
		await Promise.all([tools.wait("a").then(() => order.push("a")), tools.wait("b").then(() => order.push("b"))]);
		return order;`, startOpts{})
	waitFor(t, func() bool { c, i, _ := rec.snapshot(); return len(c) == 2 && len(i) == 1 })
	calls, _, _ := rec.snapshot()
	// b is result 1 and a result 2, but a arrives first.
	if err := rec.run.Deliver(2, startedAt+2000, calls[0].ID, Outcome{OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := rec.run.Deliver(1, startedAt+1000, calls[1].ID, Outcome{OK: true}); err != nil {
		t.Fatal(err)
	}
	wantValue(t, rec.run.Wait(), `["b","a"]`)
	// No idle after the last result: the script finished in that drain.
	if _, idles, _ := rec.snapshot(); fmt.Sprint(idles) != "[[0 2] [1 2]]" {
		t.Fatalf("idles %v", idles)
	}
}

func TestDeterministicFingerprintAndIdles(t *testing.T) {
	s := newSandbox(t, Options{MemoryLimitBytes: 64 * 1024 * 1024})
	rec := start(t, s, "await tools.echo(1); await Promise.all([tools.echo(2), tools.echo(3)]);", startOpts{})
	if r := rec.run.Wait(); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	calls, idles, events := rec.snapshot()
	if strings.Join(events[:3], ",") != "started,call 1,idle 0" {
		t.Fatalf("events %v", events)
	}
	if fmt.Sprint(idles) != "[[0 1] [1 3] [2 3]]" {
		t.Fatalf("idles %v", idles)
	}
	for i, c := range calls {
		if c.ID != i+1 || c.Target != "tool" || c.Name != "echo" || string(c.Args) != fmt.Sprint(i+1) {
			t.Fatalf("call %d: %+v", i, c)
		}
	}
	fp := rec.fingerprint
	if !regexp.MustCompile(`^quickjs-wasi \S+, quickjs \S+, prelude [0-9a-f]{8}, deterministic \d+, stack \d+, heap 67108864, budget none$`).MatchString(fp) {
		t.Fatalf("fingerprint %q", fp)
	}
	same := start(t, s, "return 1", startOpts{})
	other := start(t, newSandbox(t, Options{MemoryLimitBytes: 32 * 1024 * 1024}), "return 1", startOpts{})
	same.run.Wait()
	other.run.Wait()
	if same.fingerprint != fp || other.fingerprint == fp {
		t.Fatalf("same %q other %q", same.fingerprint, other.fingerprint)
	}
}

func TestDeterministicGlobalsAndRejections(t *testing.T) {
	s := newSandbox(t, Options{})
	rec := start(t, s, `const found = await search("x", 2);
		try { await tools.echo("fail"); } catch (error) { text(error.message); }
		return found;`, startOpts{
		RunOptions: RunOptions{Globals: []DeclaredGlobal{{Name: "search", Spread: true}}},
		answer: func(c HostCall) Outcome {
			if string(c.Args) == `"fail"` {
				return Outcome{Message: "no"}
			}
			if c.Name == "search" {
				return Outcome{OK: true, Value: []string{"found"}}
			}
			return Outcome{OK: true, Value: c.Args}
		},
	})
	r := rec.run.Wait()
	wantValue(t, r, `["found"]`)
	calls, idles, _ := rec.snapshot()
	if c := calls[0]; c.ID != 1 || c.Target != "global" || c.Name != "search" || string(c.Args) != `["x",2]` {
		t.Fatalf("call %+v", c)
	}
	if c := calls[1]; c.ID != 2 || c.Target != "tool" || c.Name != "echo" {
		t.Fatalf("call %+v", c)
	}
	if r.Output[0].Text != "no" || fmt.Sprint(idles[:2]) != "[[0 1] [1 2]]" {
		t.Fatalf("output %+v idles %v", r.Output, idles)
	}
	if len(r.Calls) != 1 || r.Calls[0].Status != CallError {
		t.Fatalf("calls %+v", r.Calls)
	}
}

func TestDeterministicMonotonicClock(t *testing.T) {
	s := newSandbox(t, Options{})
	rec := start(t, s, "const before = Date.now(); await tools.wait(1); return [before, Date.now(), performance.now()]", startOpts{})
	waitFor(t, func() bool { c, _, _ := rec.snapshot(); return len(c) == 1 })
	if err := rec.run.Deliver(1, nan(), 1, Outcome{OK: true}); err == nil || !strings.Contains(err.Error(), "not a finite number") {
		t.Fatalf("NaN clock: %v", err)
	}
	// A resumed host whose clock is behind the run's start.
	if err := rec.run.Deliver(1, startedAt-2000, 1, Outcome{OK: true}); err != nil {
		t.Fatal(err)
	}
	wantValue(t, rec.run.Wait(), fmt.Sprintf("[%v,%v,0]", startedAt, startedAt))
}

func nan() float64 { var zero float64; return zero / zero }

func TestDeterministicDuring(t *testing.T) {
	s := newSandbox(t, Options{})
	returned := start(t, s, "await tools.echo(1); return await tools.echo(2)", startOpts{})
	thrown := start(t, s, "await tools.echo(1); await Promise.all([tools.echo(2), tools.echo(3)]); throw new Error('x')", startOpts{})
	first := start(t, s, "return 1", startOpts{})
	if r := returned.run.Wait(); !r.OK || string(r.Value) != "2" || *r.During != (During{Seq: 2, Issued: 2}) {
		t.Fatalf("returned %+v %s", r, r.Value)
	}
	if r := thrown.run.Wait(); r.OK || *r.During != (During{Seq: 3, Issued: 3}) {
		t.Fatalf("thrown %+v", r)
	}
	if r := first.run.Wait(); !r.OK || *r.During != (During{}) {
		t.Fatalf("first %+v", r)
	}
	if r := exec(t, s, "return 1"); r.During != nil {
		t.Fatal("execute reports during")
	}
}

func TestDeterministicUnserializableAnswerKeepsCallWaiting(t *testing.T) {
	s := newSandbox(t, Options{})
	rec := start(t, s, "return await tools.wait(1)", startOpts{})
	waitFor(t, func() bool { c, _, _ := rec.snapshot(); return len(c) == 1 })
	if err := rec.run.Deliver(1, startedAt, 1, Outcome{OK: true, Value: make(chan int)}); err == nil {
		t.Fatal("unserializable value accepted")
	}
	if err := rec.run.Deliver(1, startedAt, 1, Outcome{Message: "x", Properties: map[string]any{"big": make(chan int)}}); err == nil {
		t.Fatal("unserializable properties accepted")
	}
	if err := rec.run.Deliver(1, startedAt, 1, Outcome{OK: true, Value: "fine"}); err != nil {
		t.Fatal(err)
	}
	wantValue(t, rec.run.Wait(), `"fine"`)
}

func TestDeterministicRejectionProperties(t *testing.T) {
	s := newSandbox(t, Options{})
	rec := start(t, s, "try { await tools.echo(1); } catch (e) { return [e instanceof Error, e.message, e.name, e.structuredOutput]; }", startOpts{
		answer: func(HostCall) Outcome {
			return Outcome{Message: "denied", Properties: map[string]any{"structuredOutput": map[string]int{"code": 7}, "name": "Ignored", "message": "ignored"}}
		},
	})
	wantValue(t, rec.run.Wait(), `[true,"denied","Error",{"code":7}]`)

	_, err := s.Start(context.Background(), "", RunOptions{StartedAt: startedAt, Seed: 1, Tools: []DeclaredTool{{Name: "a-b"}, {Name: "a_b"}}, OnCall: func(HostCall) {}})
	if err == nil || !strings.Contains(err.Error(), "script names must be unique") {
		t.Fatalf("%v", err)
	}
}

func TestDeterministicScriptNames(t *testing.T) {
	s := newSandbox(t, Options{})
	rec := start(t, s, "return [typeof tools.renamed, Object.keys(tools), ALL_TOOLS.map((tool) => tool.name)]", startOpts{
		RunOptions: RunOptions{Tools: []DeclaredTool{{Name: "my-tool", JSName: "renamed"}}},
	})
	wantValue(t, rec.run.Wait(), `["function",["renamed","my-tool"],["renamed"]]`)
}

func TestDeterministicAbortIsUncatchable(t *testing.T) {
	s := newSandbox(t, Options{})
	ctx, cancel := context.WithCancelCause(context.Background())
	rec := start(t, s, `try { await tools.wait(1); } catch { text("caught"); }
		text("after");`, startOpts{ctx: ctx})
	waitFor(t, func() bool { c, _, _ := rec.snapshot(); return len(c) == 1 })
	cancel(errors.New("diverged"))
	r := rec.run.Wait()
	if e := wantError(t, r, ErrorAborted); e.Message != "diverged" || len(r.Output) != 0 {
		t.Fatalf("%+v output %+v", e, r.Output)
	}
	if err := rec.run.Deliver(1, startedAt, 1, Outcome{OK: true}); err != nil {
		t.Fatalf("deliver after the end: %v", err)
	}
}

func TestDeterministicFinished(t *testing.T) {
	s := newSandbox(t, Options{})
	rec := start(t, s, "await tools.wait(1); return 2", startOpts{})
	waitFor(t, func() bool { c, _, _ := rec.snapshot(); return len(c) == 1 })
	if rec.run.Finished() {
		t.Fatal("finished early")
	}
	rec.run.Deliver(1, startedAt, 1, Outcome{OK: true})
	rec.run.Wait()
	if !rec.run.Finished() {
		t.Fatal("not finished")
	}
}

func TestDeterministicDeliverValidation(t *testing.T) {
	s := newSandbox(t, Options{})
	rec := start(t, s, "await Promise.all([tools.wait(1), tools.wait(2)])", startOpts{})
	waitFor(t, func() bool { c, _, _ := rec.snapshot(); return len(c) == 2 })
	run := rec.run
	if err := run.Deliver(1, startedAt, 99, Outcome{OK: true}); err == nil || !strings.Contains(err.Error(), "Call 99 is not waiting") {
		t.Fatalf("%v", err)
	}
	if err := run.Deliver(1, startedAt, 1, Outcome{OK: true}); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{1, 0} {
		if err := run.Deliver(seq, startedAt, 2, Outcome{OK: true}); err == nil || !strings.Contains(err.Error(), "used once") {
			t.Fatalf("seq %d: %v", seq, err)
		}
	}
	if err := run.Deliver(2, startedAt, 2, Outcome{OK: true}); err != nil {
		t.Fatal(err)
	}
	if r := run.Wait(); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
}

func TestExecuteIsNotDeterministic(t *testing.T) {
	s := newSandbox(t, Options{})
	before := float64(time.Now().UnixMilli())
	first := exec(t, s, "return [Date.now(), Math.random()]")
	second := exec(t, s, "return [Date.now(), Math.random()]")
	var a, b []float64
	json.Unmarshal(first.Value, &a)
	json.Unmarshal(second.Value, &b)
	if a[0] < before || a[0] > float64(time.Now().UnixMilli()) || a[1] == b[1] {
		t.Fatalf("%v %v", a, b)
	}
}
