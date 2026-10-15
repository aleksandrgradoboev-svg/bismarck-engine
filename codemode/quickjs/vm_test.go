package quickjs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newVM(t *testing.T, opts Options) *VM {
	t.Helper()
	vm, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vm.Close() })
	return vm
}

func evalString(t *testing.T, vm *VM, code string) string {
	t.Helper()
	v, err := vm.Eval(code, "test.js")
	if err != nil {
		t.Fatalf("eval %q: %v", code, err)
	}
	defer v.Free()
	return v.String()
}

func TestEval(t *testing.T) {
	vm := newVM(t, Options{})
	if got := evalString(t, vm, "1 + 2"); got != "3" {
		t.Fatalf("1 + 2 = %q", got)
	}
	if got := evalString(t, vm, "JSON.stringify({a: [1, 'x']})"); got != `{"a":[1,"x"]}` {
		t.Fatalf("got %q", got)
	}
	if !strings.HasPrefix(vm.QuickJSVersion(), "0.") {
		t.Fatalf("version %q", vm.QuickJSVersion())
	}
}

func TestEvalException(t *testing.T) {
	vm := newVM(t, Options{})
	_, err := vm.Eval("function f() { throw new TypeError('boom') }\nf()", "script.js")
	var exc *Exception
	if !errors.As(err, &exc) {
		t.Fatalf("want *Exception, got %v", err)
	}
	if exc.Name != "TypeError" || exc.Message != "boom" || !strings.Contains(exc.Stack, "script.js:1") {
		t.Fatalf("exception %+v", exc)
	}
	_, err = vm.Eval("(", "bad.js")
	if !errors.As(err, &exc) || exc.Name != "SyntaxError" {
		t.Fatalf("want SyntaxError, got %v", err)
	}
	// A thrown non-Error keeps its string form as the message.
	_, err = vm.Eval("throw 'plain'", "t.js")
	if !errors.As(err, &exc) || exc.Message != "plain" {
		t.Fatalf("want message plain, got %v", err)
	}
	// The VM is usable after an exception.
	if got := evalString(t, vm, "'ok'"); got != "ok" {
		t.Fatal(got)
	}
}

func TestHostFunction(t *testing.T) {
	vm := newVM(t, Options{})
	var seen []string
	fn, err := vm.NewFunction("join", func(this Value, args []Value) (Value, error) {
		for _, a := range args {
			seen = append(seen, a.String())
		}
		return vm.NewString(strings.Join(seen, "+"))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fn.Free()
	if err := vm.Global().Set("join", fn); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, vm, "join('a', 2, null)"); got != "a+2+null" {
		t.Fatalf("got %q", got)
	}

	failing, _ := vm.NewFunction("fail", func(Value, []Value) (Value, error) {
		return Value{}, fmt.Errorf("host says no")
	})
	defer failing.Free()
	vm.Global().Set("fail", failing)
	if got := evalString(t, vm, "try { fail() } catch (e) { e instanceof Error && e.message }"); got != "host says no" {
		t.Fatalf("got %q", got)
	}
	if _, err := vm.NewFunction("join", nil); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestHostFunctionArgsSurviveViaDup(t *testing.T) {
	vm := newVM(t, Options{})
	var kept Value
	keep, _ := vm.NewFunction("keep", func(_ Value, args []Value) (Value, error) {
		args[0].Free() // borrowed: a no-op
		var err error
		kept, err = args[0].Dup()
		return Value{}, err
	})
	defer keep.Free()
	vm.Global().Set("keep", keep)
	if _, err := vm.Eval("keep({n: 41})", "t.js"); err != nil {
		t.Fatal(err)
	}
	defer kept.Free()
	n, err := kept.Get("n")
	if err != nil {
		t.Fatal(err)
	}
	defer n.Free()
	if n.Float64() != 41 {
		t.Fatalf("n = %v", n.Float64())
	}
}

func TestCallAndPromiseJobs(t *testing.T) {
	vm := newVM(t, Options{})
	fn, err := vm.Eval("(async (x) => { const y = await Promise.resolve(x); globalThis.out = y * 2 })", "t.js")
	if err != nil {
		t.Fatal(err)
	}
	defer fn.Free()
	arg, _ := vm.NewNumber(21)
	defer arg.Free()
	p, err := vm.Call(fn, vm.Undefined(), arg)
	if err != nil {
		t.Fatal(err)
	}
	p.Free()
	for {
		pending, err := vm.IsJobPending()
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			break
		}
		if err := vm.ExecutePendingJob(); err != nil {
			t.Fatal(err)
		}
	}
	if got := evalString(t, vm, "out"); got != "42" {
		t.Fatalf("out = %q", got)
	}
}

func TestInterrupt(t *testing.T) {
	polls := 0
	vm := newVM(t, Options{Interrupt: func() bool { polls++; return polls > 3 }})
	_, err := vm.Eval("try { while (true) {} } catch (e) { 'caught' }", "loop.js")
	var exc *Exception
	if !errors.As(err, &exc) || !strings.Contains(exc.Message, "interrupted") {
		t.Fatalf("want an uncatchable interrupt, got %v", err)
	}
}

func TestContextCancelAbortsScript(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vm, err := New(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err = vm.Eval("while (true) {}", "loop.js")
	var exc *Exception
	if err == nil || errors.As(err, &exc) {
		t.Fatalf("want a wasm-level abort, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel took too long")
	}
}

func TestMemoryLimit(t *testing.T) {
	vm := newVM(t, Options{MemoryLimit: 8 << 20})
	_, err := vm.Eval("const a = []; for (;;) a.push('x'.repeat(1024) + a.length)", "oom.js")
	var exc *Exception
	if !errors.As(err, &exc) {
		t.Fatalf("want an out-of-memory exception, got %v", err)
	}
	if got := evalString(t, vm, "'alive'"); got != "alive" {
		t.Fatal(got)
	}
}

func TestDeepRecursionIsCatchable(t *testing.T) {
	vm := newVM(t, Options{})
	got := evalString(t, vm, "function r(n) { return r(n + 1) + 1 }\ntry { r(0) } catch (e) { e.name }")
	if got != "RangeError" {
		t.Fatalf("got %q", got)
	}
}

// TestClockAndRandomHooks: with fixed hooks two VMs observe the same time and
// the same Math.random() sequence, which codemode's deterministic mode needs.
func TestClockAndRandomHooks(t *testing.T) {
	run := func() string {
		vm := newVM(t, Options{
			Clock:          func(uint32) uint64 { return 1_700_000_000_000 * 1_000_000 },
			Random:         func(p []byte) { clear(p) },
			TimezoneOffset: func(int64) int32 { return 0 },
		})
		return evalString(t, vm, "[Date.now(), new Date(0).getHours(), Math.random(), Math.random()].join(',')")
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("runs differ: %s vs %s", a, b)
	}
	if !strings.HasPrefix(a, "1700000000000,0,") {
		t.Fatalf("got %s", a)
	}
}

func TestStringsRoundTrip(t *testing.T) {
	vm := newVM(t, Options{})
	for _, s := range []string{"", "ascii", "кириллица", "emoji 🦅", "nul\x00inside"} {
		v, err := vm.NewString(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
		v.Free()
	}
	// A lone surrogate survives as its WTF-8 bytes.
	if got := evalString(t, vm, "'\\uD800'"); got != "\xed\xa0\x80" {
		t.Fatalf("lone surrogate = %x", got)
	}
}

func TestHostPanicPropagates(t *testing.T) {
	vm := newVM(t, Options{})
	fn, _ := vm.NewFunction("boom", func(Value, []Value) (Value, error) { panic("kaboom") })
	defer fn.Free()
	vm.Global().Set("boom", fn)
	defer func() {
		if r := recover(); r != "kaboom" {
			t.Fatalf("recovered %v", r)
		}
	}()
	vm.Eval("boom()", "t.js")
	t.Fatal("no panic")
}

func TestClosedVM(t *testing.T) {
	vm, err := New(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := vm.NewString("x")
	vm.Close()
	v.Free() // no-op after close
	if _, err := vm.Eval("1", "t.js"); err == nil {
		t.Fatal("eval on a closed VM succeeded")
	}
}
