package quickjs

import "github.com/tetratelabs/wazero/api"

// Value is a handle to a JavaScript value inside a VM. Owned values must be
// freed with Free; values from Undefined/Null/True/False/Global and the this
// and args of a HostFunc are not owned, and Free on them is a no-op. Copies of
// a Value share one handle, so freeing a copy frees the original. The zero
// Value is invalid; HostFunc may return it to mean undefined.
type Value struct{ h *handle }

type handle struct {
	vm  *VM
	ptr uint32
	// singleton: a cached VM value; borrowed: owned by the C trampoline.
	singleton, borrowed bool
	freed               bool
}

func newValue(vm *VM, ptr uint32, singleton, borrowed bool) Value {
	return Value{&handle{vm: vm, ptr: ptr, singleton: singleton, borrowed: borrowed}}
}

// Valid reports whether v refers to a live value.
func (v Value) Valid() bool { return v.h != nil && !v.h.freed && !v.h.vm.closed }

func (v Value) ptr() uint32 {
	if !v.Valid() {
		panic("quickjs: use of an invalid Value")
	}
	return v.h.ptr
}

// Free releases an owned value. It is safe to call more than once, on a value
// that is not owned, on the zero Value, and after the VM is closed.
func (v Value) Free() {
	if v.h == nil || v.h.singleton || v.h.borrowed || v.h.freed {
		return
	}
	v.h.freed = true
	if !v.h.vm.closed {
		v.h.vm.freePtr(v.h.ptr)
	}
}

// Dup returns a new owned reference to the same value.
func (v Value) Dup() (Value, error) {
	p, err := v.h.vm.call("qjs_dup_value", uint64(v.ptr()))
	return v.h.vm.owned(p), err
}

// Get reads the property name. Names with an embedded NUL are not supported.
func (v Value) Get(name string) (Value, error) {
	vm := v.h.vm
	np, _, err := vm.writeString(name)
	if err != nil {
		return Value{}, err
	}
	defer vm.wasmFree(np)
	p, err := vm.call("qjs_get_prop_string", uint64(v.ptr()), uint64(np))
	if err != nil {
		return Value{}, err
	}
	return vm.checkException(vm.owned(p))
}

// Set writes the property name; value stays owned by the caller.
func (v Value) Set(name string, value Value) error {
	vm := v.h.vm
	np, _, err := vm.writeString(name)
	if err != nil {
		return err
	}
	defer vm.wasmFree(np)
	r, err := vm.call("qjs_set_prop_string", uint64(v.ptr()), uint64(np), uint64(value.ptr()))
	if err != nil {
		return err
	}
	if int32(r) < 0 {
		return vm.takeException()
	}
	return nil
}

func (v Value) is(fn string) bool {
	r, err := v.h.vm.call(fn, uint64(v.ptr()))
	return err == nil && uint32(r) != 0
}

func (v Value) IsUndefined() bool { return v.is("qjs_is_undefined") }
func (v Value) IsNull() bool      { return v.is("qjs_is_null") }
func (v Value) IsString() bool    { return v.is("qjs_is_string") }
func (v Value) IsNumber() bool    { return v.is("qjs_is_number") }
func (v Value) IsFunction() bool  { return v.is("qjs_is_function") }
func (v Value) IsObject() bool    { return v.is("qjs_is_object") }

// Bool returns the value's truthiness.
func (v Value) Bool() bool { return v.is("qjs_get_bool") }

// Float64 returns the value as a number.
func (v Value) Float64() float64 {
	r, err := v.h.vm.call("qjs_get_float64", uint64(v.ptr()))
	if err != nil {
		return 0
	}
	return api.DecodeF64(r)
}

// String converts the value to a string, as JavaScript's String() does. The
// bytes are the engine's WTF-8: valid UTF-8 unless the string holds a lone
// surrogate, which is kept as its 3-byte sequence rather than replaced.
func (v Value) String() string {
	vm := v.h.vm
	lenPtr, err := vm.wasmMalloc(4)
	if err != nil {
		return ""
	}
	defer vm.wasmFree(lenPtr)
	cstr, err := vm.call("qjs_get_string_len", uint64(v.ptr()), uint64(lenPtr))
	if err != nil || uint32(cstr) == 0 {
		return ""
	}
	defer vm.call("qjs_free_cstring", cstr)
	n, _ := vm.mem.ReadUint32Le(lenPtr)
	b, _ := vm.mem.Read(uint32(cstr), n)
	return string(b)
}
