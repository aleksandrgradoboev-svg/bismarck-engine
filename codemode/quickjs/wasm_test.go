package quickjs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func TestWasmIsThePinnedBinary(t *testing.T) {
	sum := sha256.Sum256(Wasm())
	if got := hex.EncodeToString(sum[:]); got != WasmSHA256 {
		t.Fatalf("embedded quickjs.wasm has SHA-256 %s, want %s (quickjs-wasi %s)", got, WasmSHA256, Version)
	}
}

// TestWasmBootsUnderWazero instantiates the binary with inert imports, starts a
// runtime and context, and reads the engine version: the smallest proof that
// wazero runs it on this platform. The real imports come with the glue.
func TestWasmBootsUnderWazero(t *testing.T) {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	env := r.NewHostModuleBuilder("env")
	for name, fn := range map[string]any{
		"host_call":                func(_, _, _, _, _ uint32) uint32 { return 0 },
		"host_interrupt":           func() uint32 { return 0 },
		"host_promise_rejection":   func(_, _, _ uint32) {},
		"host_module_normalize":    func(_, _ uint32) uint32 { return 0 },
		"host_module_load":         func(_, _ uint32) uint32 { return 0 },
		"host_get_timezone_offset": func(_, _ uint32) uint32 { return 0 },
	} {
		env.NewFunctionBuilder().WithFunc(fn).Export(name)
	}
	if _, err := env.Instantiate(ctx); err != nil {
		t.Fatal(err)
	}
	wasi := r.NewHostModuleBuilder("wasi_snapshot_preview1")
	for name, fn := range map[string]any{
		"clock_time_get": func(_ uint32, _ uint64, _ uint32) uint32 { return 0 },
		"fd_close":       func(_ uint32) uint32 { return 0 },
		"fd_fdstat_get":  func(_, _ uint32) uint32 { return 0 },
		"fd_seek":        func(_ uint32, _ uint64, _, _ uint32) uint32 { return 0 },
		"fd_write":       func(_, _, _, _ uint32) uint32 { return 0 },
		"random_get":     func(_, _ uint32) uint32 { return 0 },
	} {
		wasi.NewFunctionBuilder().WithFunc(fn).Export(name)
	}
	if _, err := wasi.Instantiate(ctx); err != nil {
		t.Fatal(err)
	}

	mod, err := r.InstantiateWithConfig(ctx, Wasm(), wazero.NewModuleConfig().WithStartFunctions())
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, params ...uint64) []uint64 {
		t.Helper()
		res, err := mod.ExportedFunction(name).Call(ctx, params...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}
	call("_initialize")
	if res := call("qjs_init"); res[0] != 0 {
		t.Fatalf("qjs_init returned %d", int32(res[0]))
	}
	version := readCString(t, mod.Memory(), uint32(call("qjs_get_quickjs_version")[0]))
	if version == "" || strings.ContainsFunc(version, func(r rune) bool { return r < ' ' }) {
		t.Fatalf("unexpected QuickJS version %q", version)
	}
	t.Logf("quickjs-wasi %s, QuickJS-ng %s", Version, version)
	call("qjs_destroy")
}

func readCString(t *testing.T, mem api.Memory, ptr uint32) string {
	t.Helper()
	buf, ok := mem.Read(ptr, 64)
	if !ok {
		t.Fatalf("pointer %d is outside memory", ptr)
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		return string(buf[:i])
	}
	t.Fatalf("no terminator within 64 bytes at %d", ptr)
	return ""
}
