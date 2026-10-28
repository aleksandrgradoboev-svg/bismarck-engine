//go:build !windows

package proc

import (
	"errors"
	"slices"
	"syscall"
	"testing"
)

// pi's shell-kill-process-tree.test.ts (27cc6bfc, #11252): stop-before-kill
// is macOS-only and best-effort.

type signalCall struct {
	pid int
	sig syscall.Signal
}

func recordSignals(fail func(pid int, sig syscall.Signal) bool) (*[]signalCall, func(int, syscall.Signal) error) {
	var calls []signalCall
	return &calls, func(pid int, sig syscall.Signal) error {
		calls = append(calls, signalCall{pid, sig})
		if fail != nil && fail(pid, sig) {
			return errors.New("signal failed")
		}
		return nil
	}
}

func TestSignalProcessTree(t *testing.T) {
	stopKill := []signalCall{{-1234, syscall.SIGSTOP}, {-1234, syscall.SIGKILL}}
	withFallback := append(slices.Clone(stopKill), signalCall{1234, syscall.SIGKILL})
	for _, tc := range []struct {
		name      string
		stopFirst bool
		fail      func(int, syscall.Signal) bool
		want      []signalCall
	}{
		{name: "stops the macOS process group before killing it without resuming it", stopFirst: true, want: stopKill},
		{name: "still kills the group when stopping fails", stopFirst: true, fail: func(_ int, sig syscall.Signal) bool { return sig == syscall.SIGSTOP }, want: stopKill},
		{name: "falls back to killing the shell when the group kill fails", stopFirst: true, fail: func(pid int, sig syscall.Signal) bool { return pid < 0 && sig == syscall.SIGKILL }, want: withFallback},
		{name: "tolerates failure of both group signals and the fallback", stopFirst: true, fail: func(int, syscall.Signal) bool { return true }, want: withFallback},
		{name: "keeps Linux group termination unchanged", want: []signalCall{{-1234, syscall.SIGKILL}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, kill := recordSignals(tc.fail)
			_ = signalProcessTree(1234, tc.stopFirst, kill)
			if !slices.Equal(*calls, tc.want) {
				t.Fatalf("signals %v, want %v", *calls, tc.want)
			}
		})
	}
}
