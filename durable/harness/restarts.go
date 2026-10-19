package harness

import (
	"context"
	"fmt"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// The restart guard (spec §5.4): open counts every surviving running task as
// cut off, and a task cut off settings.RestartLimit times in a row without
// progress is parked, blocked with reason restart_limit, until RetryTask.

const blockedRestartLimit = "restart_limit"

// restartLimitLocked is settings.RestartLimit, or the default when the host's
// settings panic. Read under mu, as the registry snapshot is: the host's
// settings must not block.
func (s *scheduler) restartLimitLocked() int {
	limit := DefaultRestartLimit
	if err := s.callHost(func() { limit = s.settings().RestartLimit }); err != nil {
		s.reportLocked(err, durable.HostError(durable.CallbackSettings, nil))
		return DefaultRestartLimit
	}
	return limit
}

// parkedLocked reports a task cut off the restart limit's times in a row; an
// abort mark still runs its abort handler.
func (s *scheduler) parkedLocked(record durable.TaskRecord) bool {
	if record.AbortRequested || record.Restarts == 0 {
		return false
	}
	limit := s.restartLimitLocked()
	return limit >= 0 && record.Restarts >= limit
}

// blockedReason is why a live task cannot run here, if it cannot: its
// definition (missing, too old, failed migration) or the restart limit. A
// running task, an abort-marked one, and one holding its outcome are never
// blocked. The task graph derives its nodes' reasons with it.
func (s *scheduler) blockedReason(record durable.TaskRecord) string {
	switch record.State.Status {
	case durable.TaskRunning, durable.TaskCompleting, durable.TaskTerminal:
		return ""
	}
	// An abort request runs its handler or orphans the task; a restart mark
	// waits for the definition, blocked.
	if record.AbortRequested && record.AbortReason == "" {
		return ""
	}
	s.mu.Lock()
	defer s.unlock()
	if s.parkedLocked(record) {
		return blockedRestartLimit
	}
	snapshot, err := s.snapshotSafely()
	if err != nil {
		if !s.snapshotReported {
			s.reportLocked(err, registryError)
		}
		s.snapshotReported = true
		return ""
	}
	task, migrates, reason, _ := s.fit(record, snapshot.Task(record.Kind))
	if reason != "" {
		return reason
	}
	if migrates && !task.HasMigration() {
		return blockedMigrationFailed
	}
	return ""
}

// subscribeBlocked calls listener when blocked reasons may have changed
// without a commit: a registry change, a failed migration, or a changed
// restart limit.
func (s *scheduler) subscribeBlocked(listener func()) func() {
	s.mu.Lock()
	defer s.unlock()
	s.nextBlocked++
	id := s.nextBlocked
	s.blockedListeners[id] = listener
	return func() {
		s.mu.Lock()
		defer s.unlock()
		delete(s.blockedListeners, id)
	}
}

// notifyBlockedLocked calls the blocked listeners once mu is released.
func (s *scheduler) notifyBlockedLocked() {
	for _, listener := range s.blockedListeners {
		s.afterUnlock = append(s.afterUnlock, hostCall{run: listener, info: durable.ListenerError})
	}
}

// unparkRaisedLocked makes parked tasks that a raised restart limit no
// longer parks candidates again, and tells the graph.
func (s *scheduler) unparkRaisedLocked() {
	unparked := false
	for _, id := range s.parkedTasks.values() {
		record, ok := s.live[id]
		if ok && s.parkedLocked(record) {
			continue
		}
		s.parkedTasks.delete(id)
		if !ok {
			continue
		}
		s.runnable.add(id)
		unparked = true
	}
	if unparked {
		s.notifyBlockedLocked()
	}
}

// retry runs a task parked by the restart limit again: it clears its restart
// count and enables scheduling. It does nothing for a task that is not
// parked, and fails for an unknown one.
func (s *scheduler) retry(ctx context.Context, id durable.TaskID) error {
	err := s.session.Commit(ctx, func(tx *durable.Tx) error {
		current, err := tx.Task(id)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("Task %d does not exist", id)
		}
		if status := current.State.Status; status == durable.TaskTerminal || status == durable.TaskCompleting {
			return nil
		}
		s.mu.Lock()
		parked := s.parkedLocked(*current)
		s.unlock()
		if !parked {
			return nil
		}
		return tx.SetTask(progressed(*current))
	})
	if err != nil {
		return err
	}
	s.resume()
	return nil
}

// stoppedBack is what join writes at close: the tasks it stopped, back as
// pending, so a reopen counts no restart for them. It runs on the line after
// the commits admitted before the seal, such as an abort mark, so the write
// keeps them.
func (s *scheduler) stoppedBack(stopped []durable.TaskID) []durable.StorageWrite {
	s.mu.Lock()
	defer s.unlock()
	var writes []durable.StorageWrite
	for _, id := range stopped {
		record, ok := s.live[id]
		if !ok || record.State.Status != durable.TaskRunning {
			continue
		}
		writes = append(writes, durable.TaskWrite{Value: withState(record, durable.TaskState{Status: durable.TaskPending, Checkpoint: record.State.Checkpoint})})
	}
	return writes
}
