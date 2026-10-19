package durable

import (
	"errors"
	"sync/atomic"
	"time"
)

// SetTimer is a host timer, given as SessionOptions.SetTimer and the
// Harness's Options.SetTimer: call callback once, about delay from now, and
// return a function that disarms it. Background timers only free memory, and
// a host may decline one by returning nil, for example where a pending timer
// keeps the process from hibernating; the Session then frees that memory at
// later commits. Other timers pace or resume work: one the host declines is
// armed with time.AfterFunc instead. A timer may fire early or late: callers
// check their clock again.
//
// Unlike pi's setTimeout, a Go timer never keeps the process alive, so the
// two kinds differ only in what a host may decline.
type SetTimer func(callback func(), delay time.Duration, background bool) (disarm func())

// NativeTimer is the default SetTimer: time.AfterFunc, for both kinds.
func NativeTimer(callback func(), delay time.Duration, _ bool) func() {
	timer := time.AfterFunc(max(0, delay), callback)
	return func() { timer.Stop() }
}

// Timers are the timers a Session and a Harness arm through the host's
// SetTimer.
type Timers struct {
	arm      SetTimer
	report   func(error)
	reported atomic.Bool
}

// HostTimers wraps a host SetTimer; nil is NativeTimer. A foreground timer
// must fire, or work stalls: one the host panics on or declines is armed with
// NativeTimer, and the first such failure is reported. A background one the
// host panics on is reported and declined.
func HostTimers(setTimer SetTimer, report func(error)) *Timers {
	if setTimer == nil {
		setTimer = NativeTimer
	}
	if report == nil {
		report = func(error) {}
	}
	return &Timers{arm: setTimer, report: report}
}

// Foreground arms a timer that paces or resumes work.
func (t *Timers) Foreground(callback func(), delay time.Duration) func() {
	disarm, err := t.call(callback, delay, false)
	if err == nil && disarm != nil {
		return disarm
	}
	if err == nil {
		err = errors.New("HarnessOptions.setTimer declined a timer that paces work; using setTimeout")
	}
	t.fallBack(err)
	return NativeTimer(callback, delay, false)
}

// Background arms a timer that only frees memory; nil when the host declines
// it.
func (t *Timers) Background(callback func(), delay time.Duration) func() {
	disarm, err := t.call(callback, delay, true)
	if err != nil {
		t.report(err)
		return nil
	}
	return disarm
}

func (t *Timers) call(callback func(), delay time.Duration, background bool) (disarm func(), err error) {
	defer func() {
		if r := recover(); r != nil {
			disarm, err = nil, panicError(r)
		}
	}()
	return t.arm(callback, delay, background), nil
}

// fallBack reports the first failed foreground timer. Task code arms timers
// from its own goroutines, so the flag is atomic.
func (t *Timers) fallBack(err error) {
	if t.reported.CompareAndSwap(false, true) {
		t.report(err)
	}
}
