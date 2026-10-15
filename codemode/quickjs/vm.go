package quickjs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// MaxStackSize is the largest guard QuickJS accepts on its own stack, and the
// default: deep recursion past it throws a catchable RangeError instead of
// trapping the wasm instance.
const MaxStackSize = 512 * 1024

// Intrinsic groups for Options.Intrinsics, as quickjs-wasi defines them.
const (
	IntrinsicDate         uint32 = 1 << 0
	IntrinsicEval         uint32 = 1 << 1
	IntrinsicRegExp       uint32 = 1 << 2
	IntrinsicJSON         uint32 = 1 << 3
	IntrinsicProxy        uint32 = 1 << 4
	IntrinsicMapSet       uint32 = 1 << 5
	IntrinsicTypedArrays  uint32 = 1 << 6
	IntrinsicPromise      uint32 = 1 << 7
	IntrinsicBigInt       uint32 = 1 << 8
	IntrinsicWeakRef      uint32 = 1 << 9
	IntrinsicPerformance  uint32 = 1 << 10
	IntrinsicDOMException uint32 = 1 << 11
	IntrinsicAtobBtoa     uint32 = 1 << 12
	IntrinsicsAll         uint32 = 0xFFFFFFFF
)

// Options configures a VM. The zero value is a VM with every intrinsic, no
// memory limit, the MaxStackSize guard, wall-clock time, crypto randomness and
// the host's time zone.
type Options struct {
	// MemoryLimit caps the QuickJS heap in bytes; 0 means no limit.
	MemoryLimit uint32
	// MaxStackSize is QuickJS's guard on its own stack; 0 means MaxStackSize.
	MaxStackSize uint32
	// Intrinsics selects the built-in groups; 0 means IntrinsicsAll.
	Intrinsics uint32
	// Interrupt is polled by QuickJS while a script runs; returning true
	// throws an uncatchable error in the script.
	Interrupt func() bool
	// Clock returns the time in nanoseconds for a WASI clock (ClockRealtime or
	// ClockMonotonic). Nil reads the wall clock for both.
	Clock func(clockID uint32) uint64
	// Random fills p with random bytes. Nil uses crypto/rand.
	Random func(p []byte)
	// TimezoneOffset returns the UTC offset in seconds, positive east of UTC,
	// at the given Unix time in seconds. Nil uses the host's time zone.
	TimezoneOffset func(unixSeconds int64) int32
}

// HostFunc is a Go function callable from JavaScript. this and args are
// borrowed: they are valid only during the call, and Free on them is a no-op;
// Dup one to keep it. Ownership of the returned Value passes to the VM. A
// returned error is thrown into the script as an Error with its message.
type HostFunc func(this Value, args []Value) (Value, error)

// VM is one QuickJS runtime and context in its own wasm instance. It is not
// safe for concurrent use: drive it from one goroutine at a time.
type VM struct {
	ctx     context.Context
	runtime wazero.Runtime
	mod     api.Module
	mem     api.Memory
	fns     map[string]api.Function

	interrupt func() bool
	clock     func(uint32) uint64
	random    func([]byte)
	tzOffset  func(int64) int32

	hostFuncs map[string]HostFunc
	// hostPanic carries a panic out of a host function across the wasm frames.
	hostPanic any

	undefined, null, vtrue, vfalse, global Value
	quickjsVersion                         string
	closed                                 bool
}

var (
	cacheOnce sync.Once
	cache     wazero.CompilationCache
)

// compilationCache is shared by every VM in the process, so the binary is
// compiled to machine code once rather than per VM.
func compilationCache() wazero.CompilationCache {
	cacheOnce.Do(func() { cache = wazero.NewCompilationCache() })
	return cache
}

// New starts a VM. ctx bounds the VM's life: canceling it aborts a running
// script and every later call fails.
func New(ctx context.Context, opts Options) (*VM, error) {
	if opts.MaxStackSize > MaxStackSize {
		return nil, fmt.Errorf("quickjs: MaxStackSize %d exceeds %d", opts.MaxStackSize, MaxStackSize)
	}
	vm := &VM{
		ctx:       ctx,
		fns:       map[string]api.Function{},
		interrupt: opts.Interrupt,
		clock:     opts.Clock,
		random:    opts.Random,
		tzOffset:  opts.TimezoneOffset,
		hostFuncs: map[string]HostFunc{},
	}
	if vm.clock == nil {
		vm.clock = defaultClock
	}
	if vm.random == nil {
		vm.random = defaultRandom
	}
	if vm.tzOffset == nil {
		vm.tzOffset = hostTimezoneOffset
	}

	cfg := wazero.NewRuntimeConfig().
		WithCompilationCache(compilationCache()).
		WithCloseOnContextDone(true)
	vm.runtime = wazero.NewRuntimeWithConfig(ctx, cfg)
	if err := vm.boot(ctx, opts); err != nil {
		_ = vm.runtime.Close(ctx)
		return nil, err
	}
	return vm, nil
}

func (vm *VM) boot(ctx context.Context, opts Options) error {
	if err := vm.instantiateEnv(ctx); err != nil {
		return err
	}
	if err := vm.instantiateWASI(ctx, vm.runtime); err != nil {
		return err
	}
	// Compile explicitly: InstantiateWithConfig on raw bytes bypasses the
	// compilation cache (measured: 320 ms per VM against 10 ms on a cache hit).
	compiled, err := vm.runtime.CompileModule(ctx, wasm)
	if err != nil {
		return fmt.Errorf("quickjs: compile: %w", err)
	}
	mod, err := vm.runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("quickjs").WithStartFunctions())
	if err != nil {
		return fmt.Errorf("quickjs: instantiate: %w", err)
	}
	vm.mod = mod
	vm.mem = mod.Memory()

	if _, err := vm.call("_initialize"); err != nil {
		return err
	}
	var rc uint64
	if opts.Intrinsics == 0 {
		rc, err = vm.call("qjs_init")
	} else {
		rc, err = vm.call("qjs_init2", uint64(opts.Intrinsics))
	}
	if err != nil {
		return err
	}
	if rc != 0 {
		return errors.New("quickjs: failed to initialize the runtime")
	}
	if opts.MemoryLimit != 0 {
		if _, err := vm.call("qjs_set_memory_limit", uint64(opts.MemoryLimit)); err != nil {
			return err
		}
	}
	stack := opts.MaxStackSize
	if stack == 0 {
		stack = MaxStackSize
	}
	if _, err := vm.call("qjs_set_max_stack_size", uint64(stack)); err != nil {
		return err
	}
	if vm.interrupt != nil {
		if _, err := vm.call("qjs_set_interrupt_handler", 1); err != nil {
			return err
		}
	}

	singleton := func(fn string) (Value, error) {
		p, err := vm.call(fn)
		return newValue(vm, uint32(p), true, false), err
	}
	for _, s := range []struct {
		dst *Value
		fn  string
	}{
		{&vm.undefined, "qjs_get_undefined"},
		{&vm.null, "qjs_get_null"},
		{&vm.vtrue, "qjs_get_true"},
		{&vm.vfalse, "qjs_get_false"},
		{&vm.global, "qjs_get_global"},
	} {
		if *s.dst, err = singleton(s.fn); err != nil {
			return err
		}
	}
	p, err := vm.call("qjs_get_quickjs_version")
	if err != nil {
		return err
	}
	vm.quickjsVersion = vm.readCString(uint32(p))
	return nil
}

// instantiateEnv provides the binary's six env.host_* imports.
func (vm *VM) instantiateEnv(ctx context.Context) error {
	b := vm.runtime.NewHostModuleBuilder("env")
	b.NewFunctionBuilder().WithFunc(vm.hostCall).Export("host_call")
	b.NewFunctionBuilder().WithFunc(func() uint32 {
		if vm.interrupt != nil && vm.interrupt() {
			return 1
		}
		return 0
	}).Export("host_interrupt")
	// No rejection tracker is installed (qjs_set_promise_rejection_handler is
	// never enabled); free what the engine hands over, as quickjs-wasi does.
	b.NewFunctionBuilder().WithFunc(func(promisePtr, reasonPtr, _ uint32) {
		vm.freePtr(promisePtr)
		vm.freePtr(reasonPtr)
	}).Export("host_promise_rejection")
	// Modules are not supported: normalize returns the specifier unchanged and
	// load finds nothing, as quickjs-wasi does without a module loader.
	b.NewFunctionBuilder().WithFunc(func(_, namePtr uint32) uint32 {
		ptr, _, err := vm.writeString(vm.readCString(namePtr))
		if err != nil {
			return 0
		}
		return ptr
	}).Export("host_module_normalize")
	b.NewFunctionBuilder().WithFunc(func(uint32, uint32) uint32 { return 0 }).Export("host_module_load")
	b.NewFunctionBuilder().WithFunc(func(hi, lo uint32) uint32 {
		return uint32(vm.tzOffset(int64(uint64(hi)<<32 | uint64(lo))))
	}).Export("host_get_timezone_offset")
	_, err := b.Instantiate(ctx)
	return err
}

func hostTimezoneOffset(unixSeconds int64) int32 {
	_, offset := time.Unix(unixSeconds, 0).Zone()
	return int32(offset)
}

// QuickJSVersion is the version of the QuickJS-ng engine inside the binary.
func (vm *VM) QuickJSVersion() string { return vm.quickjsVersion }

// Close releases the wasm instance. Values of this VM must not be used after.
func (vm *VM) Close() error {
	if vm.closed {
		return nil
	}
	vm.closed = true
	vm.hostFuncs = nil
	return vm.runtime.Close(context.WithoutCancel(vm.ctx))
}

// Undefined, Null, True, False and Global are cached; Free on them is a no-op.
func (vm *VM) Undefined() Value { return vm.undefined }
func (vm *VM) Null() Value      { return vm.null }
func (vm *VM) True() Value      { return vm.vtrue }
func (vm *VM) False() Value     { return vm.vfalse }
func (vm *VM) Global() Value    { return vm.global }

// Bool returns True or False.
func (vm *VM) Bool(b bool) Value {
	if b {
		return vm.vtrue
	}
	return vm.vfalse
}

// NewString creates a string value.
func (vm *VM) NewString(s string) (Value, error) {
	ptr, n, err := vm.writeString(s)
	if err != nil {
		return Value{}, err
	}
	defer vm.wasmFree(ptr)
	p, err := vm.call("qjs_new_string", uint64(ptr), uint64(n))
	return vm.owned(p), err
}

// NewNumber creates a number value.
func (vm *VM) NewNumber(f float64) (Value, error) {
	p, err := vm.call("qjs_new_number", api.EncodeF64(f))
	return vm.owned(p), err
}

// NewObject creates an empty object.
func (vm *VM) NewObject() (Value, error) {
	p, err := vm.call("qjs_new_object")
	return vm.owned(p), err
}

// NewFunction creates a JavaScript function that calls fn. name identifies the
// function to the engine and must be unique in the VM.
func (vm *VM) NewFunction(name string, fn HostFunc) (Value, error) {
	if _, dup := vm.hostFuncs[name]; dup {
		return Value{}, fmt.Errorf("quickjs: host function %q is already registered", name)
	}
	ptr, n, err := vm.writeString(name)
	if err != nil {
		return Value{}, err
	}
	defer vm.wasmFree(ptr)
	vm.hostFuncs[name] = fn
	p, err := vm.call("qjs_new_host_function", uint64(ptr), uint64(n), 0)
	return vm.owned(p), err
}

// NewError creates an Error with the given message.
func (vm *VM) NewError(message string) (Value, error) {
	p, err := vm.call("qjs_new_error")
	if err != nil {
		return Value{}, err
	}
	e := vm.owned(p)
	msg, err := vm.NewString(message)
	if err != nil {
		e.Free()
		return Value{}, err
	}
	defer msg.Free()
	if err := e.Set("message", msg); err != nil {
		e.Free()
		return Value{}, err
	}
	return e, nil
}

// Eval evaluates code as a script. A thrown JavaScript exception is returned as
// a *Exception error.
func (vm *VM) Eval(code, filename string) (Value, error) {
	cp, cn, err := vm.writeString(code)
	if err != nil {
		return Value{}, err
	}
	defer vm.wasmFree(cp)
	fp, _, err := vm.writeString(filename)
	if err != nil {
		return Value{}, err
	}
	defer vm.wasmFree(fp)
	p, err := vm.call("qjs_eval", uint64(cp), uint64(cn), uint64(fp), 0)
	if err != nil {
		return Value{}, err
	}
	return vm.checkException(vm.owned(p))
}

// Call calls fn with this and args. A thrown JavaScript exception is returned
// as a *Exception error.
func (vm *VM) Call(fn, this Value, args ...Value) (Value, error) {
	var argv uint32
	if len(args) > 0 {
		var err error
		argv, err = vm.wasmMalloc(uint32(4 * len(args)))
		if err != nil {
			return Value{}, err
		}
		defer vm.wasmFree(argv)
		for i, a := range args {
			vm.mem.WriteUint32Le(argv+uint32(4*i), a.ptr())
		}
	}
	p, err := vm.call("qjs_call", uint64(fn.ptr()), uint64(this.ptr()), uint64(len(args)), uint64(argv))
	if err != nil {
		return Value{}, err
	}
	return vm.checkException(vm.owned(p))
}

// IsJobPending reports whether a promise job is queued.
func (vm *VM) IsJobPending() (bool, error) {
	r, err := vm.call("qjs_is_job_pending")
	return uint32(r) != 0, err
}

// ExecutePendingJob runs one queued promise job. A job fails only with an
// uncatchable error, such as an interrupt; that comes back as a *Exception.
func (vm *VM) ExecutePendingJob() error {
	r, err := vm.call("qjs_execute_pending_job")
	if err != nil {
		return err
	}
	if int32(r) < 0 {
		return vm.takeException()
	}
	return nil
}

// Exception is a JavaScript exception that reached the host.
type Exception struct {
	Name    string
	Message string
	// Stack is the engine's stack trace, empty when the value had none.
	Stack string
}

func (e *Exception) Error() string {
	if e.Message == "" {
		return e.Name
	}
	return e.Name + ": " + e.Message
}

func (vm *VM) checkException(v Value) (Value, error) {
	r, err := vm.call("qjs_is_exception", uint64(v.h.ptr))
	if err != nil {
		v.Free()
		return Value{}, err
	}
	if uint32(r) == 0 {
		return v, nil
	}
	v.Free()
	return Value{}, vm.takeException()
}

// takeException reads and clears the pending exception, the way quickjs-wasi's
// JSException does: name defaults to "Error", message falls back to the
// value's string form.
func (vm *VM) takeException() error {
	p, err := vm.call("qjs_get_exception")
	if err != nil {
		return err
	}
	exc := vm.owned(p)
	defer exc.Free()
	e := &Exception{Name: "Error"}
	if name, err := exc.Get("name"); err == nil {
		if !name.IsUndefined() {
			e.Name = name.String()
		}
		name.Free()
	}
	if msg, err := exc.Get("message"); err == nil {
		if msg.IsUndefined() {
			e.Message = exc.String()
		} else {
			e.Message = msg.String()
		}
		msg.Free()
	}
	if stack, err := exc.Get("stack"); err == nil {
		if !stack.IsUndefined() {
			e.Stack = stack.String()
		}
		stack.Free()
	}
	return e
}

// hostCall is env.host_call: the engine calls a host function by name. this
// and argv belong to the C trampoline, which frees them after the call.
func (vm *VM) hostCall(_ context.Context, namePtr, nameLen, thisPtr, argc, argvPtr uint32) (ret uint32) {
	nameBytes, _ := vm.mem.Read(namePtr, nameLen)
	name := string(nameBytes)
	fn := vm.hostFuncs[name]
	if fn == nil {
		return vm.throwError(fmt.Sprintf("Host callback %q is not registered", name))
	}
	this := newValue(vm, thisPtr, false, true)
	args := make([]Value, argc)
	if argc > 0 {
		raw, _ := vm.mem.Read(argvPtr, 4*argc)
		for i := range args {
			args[i] = newValue(vm, leUint32(raw[4*i:]), false, true)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			// Unwinding through wasm frames is not safe to resume from; throw
			// into the script, then record the panic for call to re-raise once
			// the outermost export returns. Recorded last, or the calls inside
			// throwError would re-raise it here, where wazero swallows it.
			ret = vm.throwError(fmt.Sprint(r))
			vm.hostPanic = r
		}
	}()
	result, err := fn(this, args)
	if err != nil {
		result.Free()
		return vm.throwError(err.Error())
	}
	if result.h == nil {
		result = vm.undefined
	}
	dup, err2 := vm.call("qjs_dup_value", uint64(result.ptr()))
	result.Free()
	if err2 != nil {
		panic(err2)
	}
	return uint32(dup)
}

// throwError throws a new Error into the script and returns the NULL that
// tells the trampoline an exception is pending.
func (vm *VM) throwError(message string) uint32 {
	e, err := vm.NewError(message)
	if err != nil {
		panic(err)
	}
	defer e.Free()
	if _, err := vm.call("qjs_throw", uint64(e.ptr())); err != nil {
		panic(err)
	}
	return 0
}

// call invokes an export. Errors here are wasm-level failures (a trap, a
// canceled context, a closed VM), not JavaScript exceptions.
func (vm *VM) call(name string, params ...uint64) (uint64, error) {
	if vm.closed {
		return 0, errors.New("quickjs: VM is closed")
	}
	fn := vm.fns[name]
	if fn == nil {
		fn = vm.mod.ExportedFunction(name)
		if fn == nil {
			return 0, fmt.Errorf("quickjs: binary has no export %q", name)
		}
		vm.fns[name] = fn
	}
	res, err := fn.Call(vm.ctx, params...)
	if p := vm.hostPanic; p != nil {
		vm.hostPanic = nil
		panic(p)
	}
	if err != nil {
		return 0, fmt.Errorf("quickjs: %s: %w", name, err)
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0], nil
}

func (vm *VM) owned(p uint64) Value { return newValue(vm, uint32(p), false, false) }

func (vm *VM) freePtr(ptr uint32) {
	if ptr != 0 {
		_, _ = vm.call("qjs_free_value", uint64(ptr))
	}
}

func (vm *VM) wasmMalloc(n uint32) (uint32, error) {
	p, err := vm.call("wasm_malloc", uint64(n))
	if err != nil {
		return 0, err
	}
	if uint32(p) == 0 {
		return 0, errors.New("quickjs: wasm_malloc failed")
	}
	return uint32(p), nil
}

func (vm *VM) wasmFree(ptr uint32) { _, _ = vm.call("wasm_free", uint64(ptr)) }

// writeString copies s into guest memory NUL-terminated; the caller frees it.
// Go strings carry bytes as they are, so WTF-8 from the guest round-trips.
func (vm *VM) writeString(s string) (ptr, n uint32, err error) {
	if len(s) >= math.MaxUint32 {
		return 0, 0, errors.New("quickjs: string too long")
	}
	n = uint32(len(s))
	ptr, err = vm.wasmMalloc(n + 1)
	if err != nil {
		return 0, 0, err
	}
	buf, _ := vm.mem.Read(ptr, n+1)
	copy(buf, s)
	buf[n] = 0
	return ptr, n, nil
}

func (vm *VM) readCString(ptr uint32) string {
	var b []byte
	for i := ptr; ; i++ {
		c, ok := vm.mem.ReadByte(i)
		if !ok || c == 0 {
			return string(b)
		}
		b = append(b, c)
	}
}

func leUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
