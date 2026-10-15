package quickjs

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// WASI errno values the shim returns.
const (
	errnoSuccess = 0
	errnoBadf    = 8
	errnoNosys   = 52
)

// WASI clock IDs.
const (
	ClockRealtime  = 0
	ClockMonotonic = 1
)

// defaultClock reads wall time for both clocks, as quickjs-wasi's shim does
// (it answers CLOCK_MONOTONIC with Date.now() too).
func defaultClock(uint32) uint64 { return uint64(time.Now().UnixNano()) }

func defaultRandom(p []byte) { _, _ = rand.Read(p) }

// instantiateWASI provides the six wasi_snapshot_preview1 functions the binary
// imports. Engine output on fd 1 and 2 is discarded: it belongs to the host
// application, not to the script (pi's codemode does the same). Reporting
// every byte as written keeps libc from retrying.
func (vm *VM) instantiateWASI(ctx context.Context, r wazero.Runtime) error {
	b := r.NewHostModuleBuilder("wasi_snapshot_preview1")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, clockID uint32, _ uint64, resultPtr uint32) uint32 {
		if clockID != ClockRealtime && clockID != ClockMonotonic {
			return errnoNosys
		}
		if !m.Memory().WriteUint64Le(resultPtr, vm.clock(clockID)) {
			return errnoBadf
		}
		return errnoSuccess
	}).Export("clock_time_get")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, fd, iovsPtr, iovsLen, nwrittenPtr uint32) uint32 {
		if fd != 1 && fd != 2 {
			return errnoBadf
		}
		iovs, ok := m.Memory().Read(iovsPtr, iovsLen*8)
		if !ok {
			return errnoBadf
		}
		var written uint32
		for i := uint32(0); i < iovsLen; i++ {
			written += binary.LittleEndian.Uint32(iovs[i*8+4:])
		}
		m.Memory().WriteUint32Le(nwrittenPtr, written)
		return errnoSuccess
	}).Export("fd_write")
	b.NewFunctionBuilder().WithFunc(func(uint32) uint32 { return errnoNosys }).Export("fd_close")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, fd, statPtr uint32) uint32 {
		if fd != 1 && fd != 2 {
			return errnoBadf
		}
		// fs_filetype = CHARACTER_DEVICE; flags and rights zero.
		stat := make([]byte, 24)
		stat[0] = 2
		if !m.Memory().Write(statPtr, stat) {
			return errnoBadf
		}
		return errnoSuccess
	}).Export("fd_fdstat_get")
	b.NewFunctionBuilder().WithFunc(func(uint32, uint64, uint32, uint32) uint32 { return errnoNosys }).Export("fd_seek")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, bufPtr, bufLen uint32) uint32 {
		buf, ok := m.Memory().Read(bufPtr, bufLen)
		if !ok {
			return errnoBadf
		}
		vm.random(buf)
		return errnoSuccess
	}).Export("random_get")
	_, err := b.Instantiate(ctx)
	return err
}
