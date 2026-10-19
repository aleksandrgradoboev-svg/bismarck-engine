package harness

import (
	"fmt"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Sleeping tasks, timers and wakeups (spec §5.6).
//
// pi recomputes the wake in a microtask and calls onWake and the host's
// setTimer from it, on its one thread. Here both are host code, which never
// runs under mu: a delivery computes the wake under mu, then arms the timer
// or calls OnWake under wakeMu alone, so deliveries reach the host in order.

// wakeLocked says when the Session next needs a process: Active while some
// task can run now or is running, At for the earliest sleep not yet due. It
// reads memory only; a task whose chain is not loaded yet counts as it would
// alone.
func (s *scheduler) wakeLocked() Wake {
	now := s.now().UnixMilli()
	// A reconcile pass with work to do is work too; one with nothing to do is
	// not.
	active := len(s.invocations) > 0 || s.reconcilingWork > 0 || s.reconcileWork()
	var at int64
	hasAt := false
	var snapshot *RegistrySnapshot
	snapshotFailed := false
	for _, record := range s.live {
		state := record.State
		if state.Status == durable.TaskCompleting {
			continue
		}
		if state.Status == durable.TaskWaiting && !record.AbortRequested && s.anyLive(state.On) {
			continue
		}
		if s.notDueAt(record, now) {
			if !hasAt || *state.Until < at {
				at, hasAt = *state.Until, true
			}
			continue
		}
		if active {
			continue
		}
		// An abort invocation waits for the live work below it, which counts
		// itself.
		if record.AbortRequested && len(s.waitingOn(record, true)) > 0 {
			continue
		}
		// A task no definition can take waits for a registry change, except an
		// abort-marked one, which is orphaned.
		if !record.AbortRequested || record.AbortReason != "" {
			if snapshot == nil && !snapshotFailed {
				var err error
				if snapshot, err = s.snapshotSafely(); err != nil {
					snapshotFailed = true
					s.reportLocked(err)
				}
			}
			if snapshot != nil {
				if _, _, reason, _ := s.fit(record, snapshot.Task(record.Kind)); reason != "" {
					continue
				}
			}
		}
		active = true
	}
	wake := Wake{Active: active}
	if hasAt {
		wake.At = time.UnixMilli(at)
	}
	return wake
}

func (s *scheduler) anyLive(ids []durable.TaskID) bool {
	for _, id := range ids {
		if _, live := s.live[id]; live {
			return true
		}
	}
	return false
}

// snapshotSafely takes a registry snapshot; the registry is host code.
func (s *scheduler) snapshotSafely() (snapshot *RegistrySnapshot, err error) {
	defer func() {
		if r := recover(); r != nil {
			snapshot, err = nil, panicError(r)
		}
	}()
	return s.registry.Snapshot(), nil
}

// reconcileWork reports whether a reconcile pass would have work: a cascade
// to derive, held outcomes to check, or failFast waits.
func (s *scheduler) reconcileWork() bool {
	return s.cascadePending || s.finalizeChecks.len() > 0 || s.failFastChecks.len() > 0
}

// scheduleWakeLocked recomputes the wake off mu and off the Session line:
// it arms the sleep timer, or tells OnWake of a change. Once however often it
// is asked for meanwhile.
func (s *scheduler) scheduleWakeLocked() {
	if s.wakeScheduled || s.closing.Load() {
		return
	}
	// Without OnWake only the sleep timer listens: with no sleep and no timer
	// there is nothing to arm or disarm. This keeps a kick cheap where pi's
	// microtask is: a delivery competes for mu.
	if s.onWake == nil && s.sleeping.len() == 0 && !s.timerArmed.Load() {
		return
	}
	s.wakeScheduled = true
	go s.deliverWake()
}

// sleepAtLocked is Wake.At alone, from the sleeping tasks: all a delivery
// without OnWake needs.
func (s *scheduler) sleepAtLocked() time.Time {
	now := s.now().UnixMilli()
	var at int64
	hasAt := false
	for _, id := range s.sleeping.values() {
		record, ok := s.live[id]
		if !ok || !s.notDueAt(record, now) {
			continue
		}
		if until := *record.State.Until; !hasAt || until < at {
			at, hasAt = until, true
		}
	}
	if !hasAt {
		return time.Time{}
	}
	return time.UnixMilli(at)
}

func (s *scheduler) deliverWake() {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	s.mu.Lock()
	// Cleared under wakeMu: a change after the computation below schedules
	// one more delivery, which runs after this one.
	s.wakeScheduled = false
	if s.closing.Load() {
		s.unlock()
		return
	}
	if s.onWake == nil {
		var at time.Time
		if s.enabled {
			at = s.sleepAtLocked()
		}
		s.unlock()
		s.armSleepTimer(at)
		return
	}
	wake := s.wakeLocked()
	s.unlock()
	if last := s.lastWake; last != nil && last.Active == wake.Active && last.At.Equal(wake.At) {
		return
	}
	// Only a delivered wake counts: after a panic, the next change or pass
	// offers it again.
	if err := s.callHost(func() { s.onWake(wake) }); err != nil {
		s.report(err)
		return
	}
	s.lastWake = &wake
}

// armSleepTimer keeps the timer for the earliest sleep, under wakeMu. It
// rechecks the clock when it fires, so a wake before the sleep's time only
// arms it again.
func (s *scheduler) armSleepTimer(at time.Time) {
	if t := s.timer; t != nil && !t.fired.Load() && t.at.Equal(at) {
		return
	}
	if s.timer != nil {
		if err := s.callHost(s.timer.disarm); err != nil {
			s.report(err)
		}
		s.timer = nil
		s.timerArmed.Store(false)
	}
	if at.IsZero() || s.closing.Load() {
		return
	}
	s.timerArmed.Store(true)
	t := &sleepTimer{at: at}
	t.disarm = s.timers.Foreground(func() {
		t.fired.Store(true)
		s.mu.Lock()
		s.wakeDueLocked()
		s.kickLocked()
		s.unlock()
	}, at.Sub(s.now()))
	s.timer = t
}

// callHost runs host code, turning its panic into an error.
func (s *scheduler) callHost(call func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	call()
	return nil
}

// wakeDueLocked makes every sleeping task whose time has come a reservation
// candidate.
func (s *scheduler) wakeDueLocked() {
	now := s.now().UnixMilli()
	for _, id := range s.sleeping.values() {
		if record, ok := s.live[id]; ok && !s.notDueAt(record, now) {
			s.runnable.add(id)
		}
	}
}

// notDue reports a sleep whose time the clock has not reached; an abort mark
// ends a sleep.
func (s *scheduler) notDue(record durable.TaskRecord) bool {
	return s.notDueAt(record, s.now().UnixMilli())
}

func (s *scheduler) notDueAt(record durable.TaskRecord, now int64) bool {
	state := record.State
	return state.Status == durable.TaskSleeping && !record.AbortRequested && state.Until != nil && *state.Until > now
}

// validateSleep checks a sleep a task commits: until is a time on the
// Harness clock, and an abort handler cannot sleep.
func validateSleep(inv *invocation, id durable.TaskID, until *int64) error {
	if inv.mode == modeAbort {
		return fmt.Errorf("Abort handler of task %d cannot sleep", id)
	}
	if until == nil {
		return fmt.Errorf("Task %d cannot sleep until undefined: not a time in milliseconds", id)
	}
	if *until < 0 {
		return fmt.Errorf("Task %d cannot sleep until %d: not a time in milliseconds", id, *until)
	}
	return nil
}
