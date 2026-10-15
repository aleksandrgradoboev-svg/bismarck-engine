// Package quickjs runs the QuickJS-ng JavaScript engine, compiled to
// WebAssembly by the quickjs-wasi project, on the pure-Go wazero runtime. It is
// the sandbox under codemode: the Go form of the JavaScript glue that pi's
// packages/codemode gets from the quickjs-wasi npm package.
//
// The wasm binary is quickjs-wasi 3.6.2's quickjs.wasm, unmodified (see
// NOTICE.md). Its C layer exports the qjs_* functions this package drives and
// imports six env.host_* functions alongside six WASI ones, so the glue is
// rewritten here rather than reused. The version is pinned exactly: pi drives
// internal exports such as qjs_is_job_pending and qjs_execute_pending_job, and a
// different binary may change them.
package quickjs

import _ "embed"

// Version is the quickjs-wasi release the embedded binary comes from.
const Version = "3.6.2"

// WasmSHA256 is the SHA-256 of the embedded binary, checked by the tests so a
// swapped file cannot pass silently under the pinned Version.
const WasmSHA256 = "d4c9375f2b1ca4dc95f72c8aa2982a7a9951ac8011490d79c6582df732b4bbd9"

//go:embed quickjs.wasm
var wasm []byte

// Wasm returns the embedded quickjs.wasm. The slice is shared; callers must not
// modify it.
func Wasm() []byte { return wasm }
