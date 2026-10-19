package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// Invocation modes.
const (
	modeRun   = "run"
	modeAbort = "abort"
)

// Why a pending task cannot be reserved under a registry snapshot. Derived,
// never persisted.
const (
	blockedMissingTask     = "missing_task"
	blockedTaskTooOld      = "task_too_old"
	blockedMigrationFailed = "migration_failed"
)

// schedulerOptions are what a scheduler needs of its Harness.
type schedulerOptions struct {
	session  *durable.Session
	registry RegistryReader
	models   ai.Models
	// agent resolves a conversation's agent against a snapshot.
	agent    func(ctx context.Context, id durable.ConversationID, snapshot *RegistrySnapshot) (*Agent, error)
	settings func() Settings
	env      func(ctx context.Context, id durable.ConversationID) (env.ExecutionEnv, error)
	now      func() time.Time
	// onWake is Options.OnWake: it replaces the timer for sleeping tasks.
	onWake func(Wake)
	// timers are the host's (Options.SetTimer).
	timers *durable.Timers
	report func(error)
	// settleOutcome is the Harness cleanup staged in the commit that makes an
	// outcome the scheduler wrote itself terminal.
	settleOutcome func(tx *durable.Tx, record durable.TaskRecord, outcome durable.TaskOutcome) error
	// withdrawInputs withdraws a conversation's queued inputs, for
	// conversation abort and abort cascades.
	withdrawInputs func(tx *durable.Tx, id durable.ConversationID) error
	// ctx is the context of scheduler commits and invocations; it carries no
	// caller cancellation.
	ctx context.Context
}

// invocation is one in-memory execution of a task in run or abort mode.
type invocation struct {
	taskID         durable.TaskID
	conversationID durable.ConversationID
	mode           string
	// ctx is passed to handlers; cancel ends it.
	ctx    context.Context
	cancel context.CancelCauseFunc
	// watches acquired through the runtime; stopped at invocation end.
	watches map[*durable.DocumentWatch]struct{}
	// timers armed through the runtime; disarmed at invocation end.
	timers map[*invocationTimer]struct{}
	// ended is guarded by scheduler.mu.
	ended bool
	done  chan struct{}
}

// invocationTimer is one timer of TaskRuntime.SetTimer; disarm is set once
// the host armed it.
type invocationTimer struct {
	disarm func()
}

// sleepTimer is the timer for the earliest sleep; fired is set when it ran,
// so the next wake arms it again even for the same time.
type sleepTimer struct {
	at     time.Time
	disarm func()
	fired  atomic.Bool
}

type reservation struct {
	invocation *invocation
	task       Task
	snapshot   *RegistrySnapshot
}

type failedMigration struct {
	task Task
	err  error
}

// scheduler is the durable task scheduler of one Harness (pi TaskScheduler).
//
// live mirrors every committed non-terminal task record: pending, running,
// waiting and completing. The commit observer updates it on the Session line,
// so code running on the line reads exactly the committed state from it.
//
// Every task transition is decided and written by one callback serialized on
// the Session line: reservation, marks, runtime commits, finalization, and
// the step before each phase. Handlers and joins run off the line. An
// invocation ends inside the step that decides its end, so a runtime commit
// it queued either lands before that decision or is rejected.
//
// mu guards all state. Under it nothing joins the Session line (a commit or a
// line read) and no task or host code runs; reports made under it are
// delivered once it is released. The commit callbacks of the line hold it
// while they read Storage: the line runs one job at a time, so only readers
// off the line wait for it.
type scheduler struct {
	schedulerOptions

	mu sync.Mutex
	// pendingReports are reported once mu is released.
	pendingReports []error
	// afterUnlock are host calls decided under mu, such as disarming a timer,
	// run once it is released.
	afterUnlock []func()

	live             map[durable.TaskID]durable.TaskRecord
	invocations      map[durable.TaskID]*invocation
	taskWaiters      *waiters[durable.TaskID, durable.TaskRecord]
	idleWaiters      *waiters[durable.ConversationID, struct{}]
	failedMigrations map[durable.TaskID]failedMigration
	// edges holds the owner task of each known conversation, 0 when
	// ownerless.
	edges map[durable.ConversationID]durable.TaskID
	// settled holds the ownership fields of the known terminal tasks, which
	// walks pass through.
	settled   map[durable.TaskID]taskNode
	dropQueue *set[nodeKey]
	// below is the tree downward around live work: each node's child nodes
	// that are live tasks or have nodes listed below them.
	below          map[nodeKey]*set[nodeKey]
	roots          *set[durable.ConversationID]
	unloaded       *set[durable.TaskID]
	intent         *set[durable.TaskID]
	runnable       *set[durable.TaskID]
	finalizeChecks *set[durable.TaskID]
	waitersOn      map[durable.TaskID]*set[durable.TaskID]
	failFastChecks *set[durable.TaskID]
	abandoned      *set[durable.TaskID]
	// sleeping are the sleeping live tasks, due or not: the candidates of
	// wakeDueLocked.
	sleeping *set[durable.TaskID]

	// wakeMu orders wake deliveries: OnWake calls and the arming of the sleep
	// timer, which are host code and so run outside mu. It is taken before mu.
	wakeMu sync.Mutex
	// timer is the timer for the earliest sleep, without OnWake while
	// scheduling runs; guarded by wakeMu.
	timer *sleepTimer
	// timerArmed mirrors timer != nil for readers under mu.
	timerArmed atomic.Bool
	// lastWake is the last wake OnWake took, so an unchanged one is not
	// repeated; guarded by wakeMu.
	lastWake      *Wake
	wakeScheduled bool

	reconcileScheduled bool
	// reconcilingWork counts the reconcile passes underway that have work:
	// they count for Wake.Active.
	reconcilingWork int
	// reconciling counts reconcile passes from the moment they clear
	// reconcileScheduled until their commit returns, so "nothing is scheduled"
	// has no gap while a pass waits for the line.
	reconciling         int
	sweepScheduled      bool
	cascadePending      bool
	unsubscribeRegistry func()
	enabled             bool
	dirty               bool
	draining            bool

	// closing is set at the seal, synchronously, even from a goroutine that
	// holds mu: a failed Storage call inside a commit callback fails the
	// Session and runs the seal right there.
	closing atomic.Bool
	// sealed is closed once the seal has rejected the waits.
	sealed chan struct{}
}

func newScheduler(options schedulerOptions) *scheduler {
	return &scheduler{
		schedulerOptions:    options,
		live:                map[durable.TaskID]durable.TaskRecord{},
		invocations:         map[durable.TaskID]*invocation{},
		taskWaiters:         newWaiters[durable.TaskID, durable.TaskRecord](),
		idleWaiters:         newWaiters[durable.ConversationID, struct{}](),
		failedMigrations:    map[durable.TaskID]failedMigration{},
		edges:               map[durable.ConversationID]durable.TaskID{},
		settled:             map[durable.TaskID]taskNode{},
		dropQueue:           newSet[nodeKey](),
		below:               map[nodeKey]*set[nodeKey]{},
		roots:               newSet[durable.ConversationID](),
		unloaded:            newSet[durable.TaskID](),
		intent:              newSet[durable.TaskID](),
		runnable:            newSet[durable.TaskID](),
		finalizeChecks:      newSet[durable.TaskID](),
		waitersOn:           map[durable.TaskID]*set[durable.TaskID]{},
		failFastChecks:      newSet[durable.TaskID](),
		abandoned:           newSet[durable.TaskID](),
		sleeping:            newSet[durable.TaskID](),
		unsubscribeRegistry: func() {},
		sealed:              make(chan struct{}),
	}
}

// unlock releases mu and delivers the reports made under it.
func (s *scheduler) unlock() {
	reports := s.pendingReports
	s.pendingReports = nil
	calls := s.afterUnlock
	s.afterUnlock = nil
	s.mu.Unlock()
	for _, call := range calls {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.report(panicError(r))
				}
			}()
			call()
		}()
	}
	for _, err := range reports {
		s.report(err)
	}
}

// reportLocked reports err once mu is released.
func (s *scheduler) reportLocked(err error) { s.pendingReports = append(s.pendingReports, err) }

// open loads live tasks and changes surviving running tasks back to pending.
// It dispatches nothing.
func (s *scheduler) open(ctx context.Context) error {
	if _, err := s.session.ObserveCommits(func(publication durable.CommitPublication, _ context.Context) {
		s.observe(publication)
	}); err != nil {
		return err
	}
	if _, err := s.session.SubscribeClose(s.seal); err != nil {
		return err
	}
	s.mu.Lock()
	s.unsubscribeRegistry = s.registry.Subscribe(s.kick)
	s.mu.Unlock()
	err := s.session.Commit(ctx, func(tx *durable.Tx) error {
		s.mu.Lock()
		defer s.unlock()
		// Every table read before the first write.
		var scans [][]durable.TaskRecord
		for _, status := range []string{durable.TaskPending, durable.TaskRunning, durable.TaskWaiting, durable.TaskSleeping, durable.TaskCompleting} {
			records, err := scanAll(func(cursor durable.Cursor) (durable.Page[durable.TaskRecord], error) {
				return tx.ScanTasks(durable.TaskQuery{Status: status}, scanPageSize, cursor)
			})
			if err != nil {
				return err
			}
			scans = append(scans, records)
		}
		for _, records := range scans {
			for _, record := range records {
				s.track(nil, record)
				if record.AbandonOnRestart {
					s.abandoned.add(record.ID)
				}
				if record.State.Status == durable.TaskRunning {
					if err := tx.SetTask(withState(record, durable.TaskState{Status: durable.TaskPending, Checkpoint: record.State.Checkpoint})); err != nil {
						return err
					}
				}
				if record.State.Status == durable.TaskWaiting && record.State.Policy == durable.JoinFailFast {
					s.failFastChecks.add(record.ID)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	// Derive abort marks a crash left unapplied below cancelled owners, and
	// finalize held outcomes.
	s.cascadePending = true
	s.scheduleReconcile()
	s.scheduleWakeLocked()
	s.unlock()
	return nil
}

// resume enables scheduling, and makes sleeping tasks that are due runnable:
// a host wakeup, such as an alarm, calls it. Idempotent; the kick does
// nothing once closing.
func (s *scheduler) resume() {
	s.mu.Lock()
	s.enabled = true
	s.wakeDueLocked()
	s.kickLocked()
	s.unlock()
}

// join signals every invocation and waits for them; it writes nothing. Close
// calls it after every close listener has run, so waits, watches and streams
// a task holds end with the Session's reason, not as cancelled by its signal.
func (s *scheduler) join() {
	<-s.sealed
	s.mu.Lock()
	var done []chan struct{}
	for _, inv := range s.invocations {
		inv.cancel(context.Canceled)
		done = append(done, inv.done)
	}
	s.unlock()
	for _, d := range done {
		<-d
	}
}

// abort commits the abort mark, or settles a task no registered definition
// can take as orphaned when nothing it owns is live, then joins the run
// invocation seen on the line; the commit observer signalled it. The abort
// invocation starts once the task's ordinary owned work is gone. A completing
// task is only marked. A request replaces a restart mark, so a task abandoned
// after a restart that waits for its definition is orphaned; with
// keepRestart, as for an owner's own cleanup, such a task keeps its mark and
// waits on.
func (s *scheduler) abort(ctx context.Context, id durable.TaskID, keepRestart bool) (string, error) {
	result := "marked"
	var run *invocation
	err := s.session.Commit(ctx, func(tx *durable.Tx) error {
		s.mu.Lock()
		defer s.unlock()
		current, err := tx.Task(id)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("Task %d does not exist", id)
		}
		if current.State.Status == durable.TaskTerminal {
			result = "terminal"
			return nil
		}
		inv := s.invocations[id]
		// A restart-marked task has no run invocation; it only waits for its
		// abort.
		if keepRestart && current.AbortReason == durable.AbortReasonRestart {
			return nil
		}
		if inv == nil && current.State.Status != durable.TaskCompleting {
			if _, err := s.loadScopes(false); err != nil {
				return err
			}
			if !s.hasOwnedLive(id, nil, nil) {
				if _, _, reason := s.resolve(*current, s.registry.Snapshot()); reason != "" {
					return s.terminate(tx, *current, durable.TaskOutcome{Status: durable.OutcomeOrphaned, Reason: reason})
				}
			}
		}
		if !current.AbortRequested || current.AbortReason != "" {
			if err := tx.SetTask(withAbortMark(*current, "")); err != nil {
				return err
			}
		}
		if inv != nil && inv.mode == modeRun {
			run = inv
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	// The commit observer signalled the run; join it. A run that ignores its
	// signal can outlive a failed Session, which ends the wait instead.
	if run != nil {
		select {
		case <-run.done:
		case <-s.session.Failed():
			return "", closedError(s.session)
		case <-ctx.Done():
			return "", context.Cause(ctx)
		}
	}
	return result, nil
}

// waitForTask waits until the task is terminal and returns its record.
func (s *scheduler) waitForTask(ctx context.Context, id durable.TaskID) (durable.TaskRecord, error) {
	var w *waiter[durable.TaskRecord]
	var settled *durable.TaskRecord
	// Check and register on the line so no terminal publication falls between
	// them.
	err := s.session.ReadOnLine(func() error {
		s.mu.Lock()
		if s.closing.Load() {
			s.unlock()
			return closedError(s.session)
		}
		if _, live := s.live[id]; live {
			w = s.taskWaiters.add(id)
			s.unlock()
			return nil
		}
		s.unlock()
		record, err := s.session.Storage().Task(ctx, id)
		if err != nil {
			return err
		}
		if record == nil {
			return fmt.Errorf("Task %d does not exist", id)
		}
		settled = record
		return nil
	})
	if err != nil {
		return durable.TaskRecord{}, err
	}
	if settled != nil {
		return *settled, nil
	}
	return wait(ctx, &s.mu, s.taskWaiters, id, w)
}

// waitForIdle resolves when ordinary traversal from the conversation, or from
// every ownerless conversation for 0, reaches no live non-background task.
func (s *scheduler) waitForIdle(ctx context.Context, conversationID durable.ConversationID) error {
	s.mu.Lock()
	if s.closing.Load() {
		s.unlock()
		return closedError(s.session)
	}
	if s.idle(conversationID) {
		s.unlock()
		return nil
	}
	s.scheduleReconcile()
	w := s.idleWaiters.add(conversationID)
	s.unlock()
	_, err := wait(ctx, &s.mu, s.idleWaiters, conversationID, w)
	return err
}

// abortConversation withdraws the queued inputs and marks every live
// non-background task that ordinary traversal from the conversation reaches,
// in one commit, and resolves once the scope is idle. With background,
// traversal crosses background boundaries, and the wait also covers every
// task it reached.
func (s *scheduler) abortConversation(ctx context.Context, conversationID durable.ConversationID, background bool) error {
	var reached []durable.TaskID
	err := s.session.Commit(ctx, func(tx *durable.Tx) error {
		s.mu.Lock()
		defer s.unlock()
		reached = nil
		queued, err := s.loadScopes(true)
		if err != nil {
			return err
		}
		var walkErr error
		s.walkDown(conversationKey(conversationID), walkOptions{crossBackground: background}, func(_ durable.TaskID, node taskNode, live *durable.TaskRecord) bool {
			if live == nil || (node.background && !background) {
				return true
			}
			reached = append(reached, live.ID)
			if !live.AbortRequested || live.AbortReason != "" {
				walkErr = tx.SetTask(withAbortMark(*live, ""))
			}
			return walkErr == nil
		})
		if walkErr != nil {
			return walkErr
		}
		for _, id := range queued {
			if in, _ := s.inScope(conversationKey(id), scope{conversation: conversationID}, background); in {
				if err := s.withdrawInputs(tx, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if background {
		for _, id := range reached {
			if _, err := s.waitForTask(ctx, id); err != nil {
				return err
			}
		}
	}
	return s.waitForIdle(ctx, conversationID)
}

// ─── Scheduling ────────────────────────────────────────────────────────────

func (s *scheduler) observe(publication durable.CommitPublication) {
	s.mu.Lock()
	defer s.unlock()
	var updated []durable.TaskRecord
	var failed []durable.TaskID
	changed := false
	// Edges first, so the tasks a commit creates in a new conversation hang
	// from it.
	for _, change := range publication.Changes {
		if write, ok := change.(durable.ConversationWrite); ok {
			if _, known := s.edges[write.Value.ID]; !known {
				var owner durable.TaskID
				if write.Value.Owner != nil {
					owner = write.Value.Owner.TaskID
				}
				s.setEdge(write.Value.ID, owner)
			}
		}
	}
	for _, change := range publication.Changes {
		write, ok := change.(durable.TaskWrite)
		if !ok {
			continue
		}
		changed = true
		record := write.Value.Clone()
		var previous *durable.TaskRecord
		if p, ok := s.live[record.ID]; ok {
			previous = &p
		}
		if failedOutcome(record) && (previous == nil || !failedOutcome(*previous)) {
			failed = append(failed, record.ID)
		}
		s.track(previous, record)
		if record.State.Status == durable.TaskTerminal {
			delete(s.failedMigrations, record.ID)
			s.failFastChecks.delete(record.ID)
			s.taskWaiters.resolve(record.ID, record.Clone())
			// Its owner may finalize now.
			s.scheduleReconcile()
			continue
		}
		if record.AbortRequested && (previous == nil || !previous.AbortRequested) {
			s.cascadePending = true
			// Signal a run invocation of the newly marked task; its next step
			// ends it.
			if inv := s.invocations[record.ID]; inv != nil && inv.mode == modeRun {
				inv.cancel(context.Canceled)
			}
		}
		// A request replacing a restart mark upgrades the marks of the work
		// below.
		if previous != nil && previous.AbortReason != "" && record.AbortReason == "" {
			s.cascadePending = true
		}
		status := record.State.Status
		if status == durable.TaskCompleting && (previous == nil || previous.State.Status != durable.TaskCompleting) {
			if cancellationIntent(record) {
				s.cascadePending = true
			}
			s.scheduleReconcile()
		}
		if status == durable.TaskWaiting && record.State.Policy == durable.JoinFailFast && (previous == nil || previous.State.Status != durable.TaskWaiting) {
			s.failFastChecks.add(record.ID)
			s.scheduleReconcile()
		}
		// Its owners and place never change: what is above it is checked once,
		// when it becomes live.
		if previous == nil {
			updated = append(updated, record)
		}
	}
	for _, id := range failed {
		if waiting := s.waitersOn[id]; waiting != nil {
			for _, waiter := range waiting.values() {
				if live, ok := s.live[waiter]; ok && live.State.Status == durable.TaskWaiting && live.State.Policy == durable.JoinFailFast {
					s.failFastChecks.add(waiter)
					s.scheduleReconcile()
				}
			}
		}
	}
	for _, change := range publication.Changes {
		// A queued input below a cancelled owner is withdrawn, even after its
		// cascade.
		write, ok := change.(durable.SubmissionWrite)
		if !ok || write.Value.Status != durable.SubmissionQueued || write.Value.Type != durable.SubmissionTypeInput {
			continue
		}
		// Only an owner with cancellation intent withdraws it; a later
		// intent's mark runs the cascade then.
		if s.intent.len() == 0 {
			continue
		}
		conversation := conversationKey(write.Value.ConversationID)
		if !s.known(conversation) || s.belowCancelled(conversation) {
			s.cascadePending = true
		}
	}
	for _, record := range updated {
		// Work created below a cancelled owner, even after its cascade, is
		// aborted too.
		if s.unloaded.has(record.ID) {
			s.scheduleReconcile()
		} else if !record.Background && !record.AbortRequested && s.belowCancelled(parentOf(nodeOf(record))) {
			s.cascadePending = true
		}
	}
	// A cascade that a reservation or step found pending runs with the next
	// commit of any kind.
	if s.cascadePending {
		s.scheduleReconcile()
	}
	s.sweep()
	if !changed {
		return
	}
	s.settleIdle()
	s.kickLocked()
}

// settleIdle resolves the idle waiters whose scope is idle.
func (s *scheduler) settleIdle() {
	for _, conversationID := range s.idleWaiters.keys() {
		if s.idle(conversationID) {
			s.idleWaiters.resolve(conversationID, struct{}{})
		}
	}
}

// scheduleReconcile runs a reconcile pass on the line after the operation
// underway, once however often it is asked for meanwhile.
func (s *scheduler) scheduleReconcile() {
	if s.reconcileScheduled || s.closing.Load() {
		return
	}
	s.reconcileScheduled = true
	go s.reconcile()
}

// reconcile is one commit that applies what committed records imply: abort
// marks below live owners with cancellation intent (spec §5.4), failFast
// marks (spec §5.5), withdrawn queued inputs below cancelled owners, and the
// final terminal record of every completing task whose ordinary owned work is
// gone. The durable records are the intent, so this also repairs whatever a
// crash left unapplied. It resolves idle waiters that the loaded edges
// decide.
func (s *scheduler) reconcile() {
	s.mu.Lock()
	s.reconcileScheduled = false
	s.reconciling++
	// A pass with work is work for Wake: it finalizes held outcomes and marks
	// cascades, at open as recovery. A pass with nothing to do is not.
	work := s.reconcileWork()
	if work {
		s.reconcilingWork++
	}
	defer func() {
		s.mu.Lock()
		s.reconciling--
		if work {
			s.reconcilingWork--
		}
		s.scheduleWakeLocked()
		s.unlock()
	}()
	cascade := s.cascadePending
	s.cascadePending = false
	checks := s.failFastChecks.values()
	s.failFastChecks.clear()
	s.unlock()
	err := s.session.Commit(s.ctx, func(tx *durable.Tx) error {
		s.mu.Lock()
		defer s.unlock()
		if s.closing.Load() {
			return nil
		}
		queued, err := s.loadScopes(cascade)
		if err != nil {
			return err
		}
		// A cascade from an abandoned owner passes its restart reason on; any
		// other intent marks, or upgrades a restart mark, as a request.
		// Collected first, so a request wins over a restart mark of the same
		// pass.
		type mark struct {
			record durable.TaskRecord
			reason string
		}
		marks := map[durable.TaskID]mark{}
		var order []durable.TaskID
		add := func(record durable.TaskRecord, reason string) {
			if staged, ok := marks[record.ID]; ok && staged.reason == "" {
				return
			}
			if record.AbortRequested && (record.AbortReason == "" || reason != "") {
				return
			}
			if _, ok := marks[record.ID]; !ok {
				order = append(order, record.ID)
			}
			marks[record.ID] = mark{record, reason}
		}
		// Every live foreground task below a live owner with cancellation
		// intent, which is its nearest one: a walk down from each owner with
		// intent does not enter another, whose own walk covers what is below
		// it.
		for _, id := range s.intent.values() {
			owner := s.live[id]
			// Only an owner abandoned after a restart, and nothing else, passes
			// its reason on.
			reason := ""
			if owner.AbortReason == durable.AbortReasonRestart && !failedOutcome(owner) {
				reason = durable.AbortReasonRestart
			}
			s.walkDown(taskKey(id), walkOptions{enter: func(below durable.TaskID) bool { return !s.intent.has(below) }},
				func(_ durable.TaskID, node taskNode, live *durable.TaskRecord) bool {
					if live != nil && !node.background {
						add(*live, reason)
					}
					return true
				})
		}
		for _, id := range checks {
			waiter, ok := s.live[id]
			if !ok || waiter.State.Status != durable.TaskWaiting {
				continue
			}
			anyFailed, err := s.anyFailed(waiter.State.On)
			if err != nil {
				return err
			}
			if !anyFailed {
				continue
			}
			// Every other live task: the failed one keeps its own outcome.
			for _, member := range waiter.State.On {
				if record, ok := s.live[member]; ok && !failedOutcome(record) {
					add(record, "")
				}
			}
		}
		for _, id := range order {
			m := marks[id]
			if err := tx.SetTask(withAbortMark(m.record, m.reason)); err != nil {
				return err
			}
		}
		for _, id := range queued {
			if s.belowCancelled(conversationKey(id)) {
				if err := s.withdrawInputs(tx, id); err != nil {
					return err
				}
			}
		}
		return s.finalize(tx)
	})
	if err != nil {
		// No extension code runs in this commit: an error is a storage failure,
		// a host callback, or a bug. None is fixed by running the pass again,
		// so it fails the Session, which reports it.
		s.failSession(err)
	}
	s.mu.Lock()
	s.settleIdle()
	s.unlock()
}

// anyFailed reports whether any of ids holds or ended with an outcome other
// than completed.
func (s *scheduler) anyFailed(ids []durable.TaskID) (bool, error) {
	for _, id := range ids {
		record, ok := s.live[id]
		if !ok {
			stored, err := s.session.Storage().Task(context.WithoutCancel(s.ctx), id)
			if err != nil {
				return false, err
			}
			if stored == nil {
				continue
			}
			record = *stored
		}
		if failedOutcome(record) {
			return true, nil
		}
	}
	return false, nil
}

// finalize writes the terminal record of every completing task without live
// ordinary owned work. Finalizing one can free its owner, so this repeats over
// the commit's candidates, in rounds, until nothing changes. A held scheduler
// outcome gets its Harness cleanup here.
func (s *scheduler) finalize(tx *durable.Tx) error {
	round := s.finalizeChecks.values()
	s.finalizeChecks.clear()
	done := map[durable.TaskID]bool{}
	o := overlayOf(tx)
	for len(round) > 0 {
		next := newSet[durable.TaskID]()
		for _, id := range round {
			record, ok := s.record(id, o)
			if !ok || record.State.Status != durable.TaskCompleting || done[id] {
				continue
			}
			if s.hasOwnedLive(id, o, done) {
				continue
			}
			outcome := *record.State.Outcome
			terminal := withState(record, durable.TaskState{Status: durable.TaskTerminal, Outcome: &outcome})
			if err := tx.SetTask(terminal); err != nil {
				return err
			}
			if _, staged := o.tasks[id]; !staged {
				o.order = append(o.order, id)
			}
			o.tasks[id] = terminal
			o.links = nil
			done[id] = true
			// Only the scheduler writes faulted and orphaned (spec §5.4); their
			// cleanup waits for this commit.
			if outcome.Status == durable.OutcomeFaulted || outcome.Status == durable.OutcomeOrphaned {
				if err := s.settleOutcome(tx, record, outcome); err != nil {
					return err
				}
			}
			if above, ok := s.nearestLiveAbove(nodeOf(record), o); ok && above.State.Status == durable.TaskCompleting {
				next.add(above.ID)
			}
		}
		round = next.values()
	}
	return nil
}

// failSession fails the Session, which reports it, for an error in one of the
// scheduler's own commits; ignored once closing. Never called under mu: the
// failure seals synchronously.
//
// A close seals the Session's admission a moment before its close listeners
// run, so a commit of the scheduler can be refused as closed before the seal
// here; in pi nothing runs between the two. That refusal is the close, not a
// failure.
func (s *scheduler) failSession(err error) {
	if s.closing.Load() || errors.Is(err, durable.ErrSessionClosed) {
		return
	}
	s.session.Fail(err)
}

// seal is the close listener: it runs synchronously once admission is sealed,
// or when the Session fails. It may run on a goroutine that holds mu, so it
// only sets closing here and does the rest on its own goroutine, which join
// waits for.
func (s *scheduler) seal() {
	if s.closing.Swap(true) {
		return
	}
	go func() {
		s.mu.Lock()
		unsubscribe := s.unsubscribeRegistry
		err := closedError(s.session)
		s.taskWaiters.rejectAll(err)
		s.idleWaiters.rejectAll(err)
		s.unlock()
		// After closing is set, a wake delivery arms nothing more.
		s.wakeMu.Lock()
		if s.timer != nil {
			if err := s.callHost(s.timer.disarm); err != nil {
				s.report(err)
			}
			s.timer = nil
			s.timerArmed.Store(false)
		}
		s.wakeMu.Unlock()
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.report(panicError(r))
				}
			}()
			unsubscribe()
		}()
		close(s.sealed)
	}()
}

// kick asks for a reservation pass.
func (s *scheduler) kick() {
	s.mu.Lock()
	s.kickLocked()
	s.unlock()
}

func (s *scheduler) kickLocked() {
	s.scheduleWakeLocked()
	s.dirty = true
	if s.draining || !s.enabled || s.closing.Load() {
		return
	}
	s.draining = true
	// Never commit synchronously from a commit or registry listener.
	go s.drain()
}

func (s *scheduler) drain() {
	defer func() {
		s.mu.Lock()
		s.draining = false
		// A wakeup that arrived after the loop's last check still needs its
		// pass.
		if s.dirty {
			s.kickLocked()
		}
		s.unlock()
	}()
	for {
		s.mu.Lock()
		if !s.dirty || !s.enabled || s.closing.Load() {
			s.unlock()
			return
		}
		s.dirty = false
		s.unlock()
		reservations, err := s.reserve()
		if err != nil {
			// As in reconcile: the reservation commit runs no extension code.
			s.failSession(err)
			return
		}
		for _, r := range reservations {
			s.start(r)
		}
	}
}

// reserve reserves every eligible task in one commit, and orphans
// abort-marked tasks no definition can take, unless abandoned after a
// restart. The first pass after open only abort-marks the abandoned tasks, so
// later passes see the marks.
func (s *scheduler) reserve() ([]reservation, error) {
	var reservations []reservation
	var abandoned []durable.TaskID
	err := s.session.Commit(s.ctx, func(tx *durable.Tx) error {
		s.mu.Lock()
		defer s.unlock()
		if !s.enabled || s.closing.Load() {
			return nil
		}
		if s.abandoned.len() > 0 {
			for _, id := range s.abandoned.values() {
				if record, ok := s.live[id]; ok && !record.AbortRequested {
					if err := tx.SetTask(withAbortMark(record, durable.AbortReasonRestart)); err != nil {
						return err
					}
				}
			}
			abandoned = s.abandoned.values()
			return nil
		}
		if _, err := s.loadScopes(false); err != nil {
			return err
		}
		// Every pass picks up sleeps whose time has come: a pass started by any
		// change may be the first to run after a deadline.
		s.wakeDueLocked()
		// Taken once per pass, and only when some task is a candidate.
		var snapshot *RegistrySnapshot
		for _, id := range s.runnable.values() {
			record, ok := s.live[id]
			if !ok || record.State.Status == durable.TaskCompleting || s.invocations[id] != nil {
				s.runnable.delete(id)
				continue
			}
			// It returns once what it waits for ends, its record changes, or the
			// work below it ends.
			if len(s.waitingOn(record, true)) > 0 {
				s.runnable.delete(id)
				continue
			}
			// It returns at the first pass after its time comes, which the timer,
			// resume, or any change starts.
			if s.notDue(record) {
				s.runnable.delete(id)
				continue
			}
			mode := modeRun
			if record.AbortRequested {
				mode = modeAbort
			}
			// Work below an owner with cancellation intent waits for its cascade
			// mark instead of running a phase; schedule the cascade, which may
			// not have run yet.
			if mode == modeRun && !record.Background && s.belowCancelled(parentOf(nodeOf(record))) {
				s.cascadePending = true
				s.scheduleReconcile()
				continue
			}
			if snapshot == nil {
				snapshot = s.registry.Snapshot()
			}
			task, resolved, reason := s.resolve(record, snapshot)
			if reason != "" {
				// An abandoned task waits for its definition, so its abort handler
				// can clean up.
				if mode == modeAbort && record.AbortReason == "" {
					if err := s.terminate(tx, record, durable.TaskOutcome{Status: durable.OutcomeOrphaned, Reason: reason}); err != nil {
						return err
					}
				}
				continue
			}
			if resolved.Version != record.Version || record.State.Status != durable.TaskRunning {
				if err := tx.SetTask(withState(resolved, durable.TaskState{Status: durable.TaskRunning, Checkpoint: resolved.State.Checkpoint})); err != nil {
					return err
				}
			}
			// Registered on the line, so marks and later reservations see it and
			// close joins it; end makes it a candidate again.
			s.runnable.delete(id)
			reservations = append(reservations, reservation{invocation: s.createInvocation(record, mode), task: task, snapshot: snapshot})
		}
		return nil
	})
	if err != nil {
		s.mu.Lock()
		for _, r := range reservations {
			delete(s.invocations, r.invocation.taskID)
			s.runnable.add(r.invocation.taskID)
			r.invocation.cancel(context.Canceled)
			close(r.invocation.done)
		}
		s.unlock()
		return nil, err
	}
	s.mu.Lock()
	// Marked, or already marked or gone: done with them. Reserve again, now
	// seeing the marks.
	for _, id := range abandoned {
		s.abandoned.delete(id)
	}
	if len(abandoned) > 0 {
		s.dirty = true
	}
	s.unlock()
	return reservations, nil
}

// waitingOn returns the live tasks a task waits for before its next
// invocation: its live ordinary owned work when abort-marked, since abort runs
// bottom-up, otherwise the live part of the on of a wait.
func (s *scheduler) waitingOn(record durable.TaskRecord, first bool) []durable.TaskID {
	if record.AbortRequested {
		return s.ownedLive(record.ID, nil, first, nil)
	}
	if record.State.Status != durable.TaskWaiting {
		return nil
	}
	var on []durable.TaskID
	for _, id := range record.State.On {
		if _, live := s.live[id]; live {
			on = append(on, id)
			if first {
				break
			}
		}
	}
	return on
}

// resolve resolves the record's definition by kind, migrating an older stored
// version; reason is set when it is blocked.
func (s *scheduler) resolve(record durable.TaskRecord, snapshot *RegistrySnapshot) (Task, durable.TaskRecord, string) {
	task, migrates, reason, _ := s.fit(record, snapshot.Task(record.Kind))
	if reason != "" {
		return nil, record, reason
	}
	if !migrates {
		return task, record, ""
	}
	migrated, err := migrate(task, record)
	if err != nil {
		s.failedMigrations[record.ID] = failedMigration{task: task, err: err}
		// Blocked now without a commit: the host's wake must hear it.
		s.scheduleWakeLocked()
		s.reportLocked(err)
		return nil, record, blockedMigrationFailed
	}
	return task, migrated, ""
}

func migrate(task Task, record durable.TaskRecord) (migrated durable.TaskRecord, err error) {
	if !task.HasMigration() {
		return record, missingMigration(record, task)
	}
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	input, checkpoint, err := task.Migrate(record.Input, record.State.Checkpoint, record.Version)
	if err != nil {
		return record, err
	}
	if !json.Valid(input) || !json.Valid(checkpoint) {
		return record, fmt.Errorf("Task %s migration returned invalid JSON", record.Kind)
	}
	migrated = record.Clone()
	migrated.Version = task.Version()
	migrated.Input = input
	migrated.State.Checkpoint = checkpoint
	return migrated, nil
}

func missingMigration(record durable.TaskRecord, task Task) error {
	return fmt.Errorf("Task %s version %d has no migration from %d", record.Kind, task.Version(), record.Version)
}

// fit decides whether a definition can take a record, or why none can;
// deciding it runs no task code.
func (s *scheduler) fit(record durable.TaskRecord, task Task) (Task, bool, string, error) {
	if task == nil {
		return nil, false, blockedMissingTask, nil
	}
	version := task.Version()
	if version == record.Version {
		return task, false, "", nil
	}
	if version < record.Version {
		return nil, false, blockedTaskTooOld, nil
	}
	if failed, ok := s.failedMigrations[record.ID]; ok && failed.task == task {
		return nil, false, blockedMigrationFailed, failed.err
	}
	return task, true, "", nil
}

// canReserve reports whether a definition can take the task at reservation:
// same version, or newer with a migration.
func canReserve(task Task, record durable.TaskRecord) bool {
	return task.Version() == record.Version || (task.Version() > record.Version && task.HasMigration())
}

func (s *scheduler) createInvocation(record durable.TaskRecord, mode string) *invocation {
	ctx, cancel := context.WithCancelCause(s.ctx)
	inv := &invocation{
		taskID:         record.ID,
		conversationID: record.ConversationID,
		mode:           mode,
		ctx:            ctx,
		cancel:         cancel,
		watches:        map[*durable.DocumentWatch]struct{}{},
		timers:         map[*invocationTimer]struct{}{},
		done:           make(chan struct{}),
	}
	s.invocations[record.ID] = inv
	return inv
}

func (s *scheduler) start(r reservation) {
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.report(panicError(p))
			}
			s.mu.Lock()
			s.end(r.invocation)
			s.unlock()
			close(r.invocation.done)
			s.kick()
		}()
		if r.invocation.mode == modeRun {
			s.run(r)
		} else {
			s.runAbort(r)
		}
	}()
}

// phaseResult is the outcome of the phase that just returned, judged by the
// next step.
type phaseResult struct {
	checkpoint json.RawMessage
	phase      string
	failure    error
}

// runState is what a run invocation tracks across its phases.
type runState struct {
	task     Task
	snapshot *RegistrySnapshot
	// reported is the replacement definition already reported as unable to
	// take over; reportedSet tells a reported nil from none.
	reported    Task
	reportedSet bool
}

// decision is a step's verdict: continue with the next phase, end the
// invocation, or end it by writing faulted.
type decision struct {
	proceed bool
	fault   error
}

// run runs phase handlers, each preceded by a step that decides on the line
// whether the invocation continues.
func (s *scheduler) run(r reservation) {
	inv := r.invocation
	state := &runState{task: r.task, snapshot: r.snapshot}
	ph := &phase{snapshot: func() *RegistrySnapshot { return state.snapshot }, task: func() Task { return state.task }}
	runtime := s.newRuntime(inv, ph)
	var previous *phaseResult
	for {
		current := s.step(inv, func(tx *durable.Tx, current durable.TaskRecord) decision {
			return s.decide(tx, current, previous, state)
		})
		// Close may seal between the decision and dispatch.
		if current == nil || s.closing.Load() {
			return
		}
		checkpoint := current.State.Checkpoint
		// Each phase handler resolves its agent afresh, at first use.
		ph.reset()
		name, err := CheckpointPhase(checkpoint)
		if err == nil {
			handler := state.task.Phases()[name]
			if handler == nil {
				err = fmt.Errorf("Task %s has no phase %s", current.Kind, name)
			} else {
				err = callPhase(handler, inv.ctx, current, runtime)
			}
		}
		previous = &phaseResult{checkpoint: checkpoint, phase: name, failure: err}
	}
}

func callPhase(handler PhaseHandler, ctx context.Context, task *durable.TaskRecord, runtime *TaskRuntime) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return handler(ctx, task, runtime)
}

// decide applies the precedence rules for a run invocation, on the line.
// Rules 1 (terminal, completing or waiting) and 2 (closing) are applied by
// step.
func (s *scheduler) decide(tx *durable.Tx, current durable.TaskRecord, previous *phaseResult, state *runState) decision {
	// 3. Abort mark: end; a fresh abort invocation starts once the task's
	// ordinary owned work is gone.
	if current.AbortRequested {
		return decision{}
	}
	// An owner's cancellation intent ends it too, before its cascade marks it;
	// reservation then holds it back.
	if !current.Background && s.belowCancelled(parentOf(nodeOf(current))) {
		s.cascadePending = true
		s.scheduleReconcile()
		return decision{}
	}
	if previous == nil {
		return decision{proceed: true}
	}
	// 4. Uncaught error.
	if previous.failure != nil {
		return decision{fault: previous.failure}
	}
	// 6. No durable progress.
	if jsonEqual(current.State.Checkpoint, previous.checkpoint) {
		return decision{fault: fmt.Errorf("Task %s phase %s returned without durable progress", current.Kind, previous.phase)}
	}
	// 5. Progress: refresh the snapshot; hand over to a replacement definition
	// that can take the task.
	state.snapshot = s.registry.Snapshot()
	next := state.snapshot.Task(current.Kind)
	if next != state.task {
		if next != nil && canReserve(next, current) {
			if err := tx.SetTask(withState(current, durable.TaskState{Status: durable.TaskPending, Checkpoint: current.State.Checkpoint})); err != nil {
				return decision{fault: err}
			}
			return decision{}
		}
		if !state.reportedSet || state.reported != next {
			state.reported, state.reportedSet = next, true
			cause := "incompatible_task"
			if next == nil {
				cause = blockedMissingTask
			}
			s.reportLocked(fmt.Errorf("Task %d keeps running under its old %s definition: %s", current.ID, current.Kind, cause))
		}
	}
	return decision{proceed: true}
}

// runAbort runs the abort handler once; rules 1, 2 and 4 apply, and returning
// without an outcome faults.
func (s *scheduler) runAbort(r reservation) {
	inv := r.invocation
	s.mu.Lock()
	current, ok := s.live[inv.taskID]
	s.unlock()
	if !ok || s.closing.Load() {
		return
	}
	ph := &phase{snapshot: func() *RegistrySnapshot { return r.snapshot }, task: func() Task { return r.task }}
	runtime := s.newRuntime(inv, ph)
	failure := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = panicError(p)
			}
		}()
		return r.task.Abort(inv.ctx, &current, runtime)
	}()
	if failure == nil {
		failure = fmt.Errorf("Abort handler of task %d returned without a terminal outcome", inv.taskID)
	}
	s.step(inv, func(*durable.Tx, durable.TaskRecord) decision { return decision{fault: failure} })
}

// step is one synchronous decision on the Session line. A task that is no
// longer running (rule 1: terminal, completing or waiting) or a closing
// Harness (rule 2) ends the invocation without a write; otherwise decide may
// stage a write and returns whether the invocation continues. Ending happens
// inside the callback, before a fault's Harness cleanup. A rejected step,
// such as admission after close, also ends the invocation.
func (s *scheduler) step(inv *invocation, decide func(tx *durable.Tx, current durable.TaskRecord) decision) *durable.TaskRecord {
	var result *durable.TaskRecord
	err := s.session.Commit(s.ctx, func(tx *durable.Tx) error {
		s.mu.Lock()
		defer s.unlock()
		found, ok := s.live[inv.taskID]
		running := ok && found.State.Status == durable.TaskRunning
		d := decision{}
		if running && !s.closing.Load() {
			d = decide(tx, found)
		}
		if d.proceed {
			current := found.Clone()
			result = &current
			return nil
		}
		s.end(inv)
		if d.fault != nil {
			return s.terminate(tx, found, durable.TaskOutcome{Status: durable.OutcomeFaulted, Error: &durable.TaskOutcomeError{Message: errorMessage(d.fault)}})
		}
		return nil
	})
	if err != nil {
		// The step writes the scheduler's own decision, a fault or a terminal
		// record; an error there would otherwise leave the task running, to be
		// reserved and run again.
		s.mu.Lock()
		s.end(inv)
		s.unlock()
		s.failSession(err)
		return nil
	}
	return result
}

// terminate writes an outcome the scheduler decided. While the task's
// ordinary owned work is live it holds as completing and its Harness cleanup
// waits for the final commit (spec §5.5, rule 4); otherwise it is terminal
// with its cleanup.
func (s *scheduler) terminate(tx *durable.Tx, record durable.TaskRecord, outcome durable.TaskOutcome) error {
	if _, err := s.loadScopes(false); err != nil {
		return err
	}
	if s.hasOwnedLive(record.ID, overlayOf(tx), nil) {
		return tx.SetTask(withState(record, durable.TaskState{Status: durable.TaskCompleting, Outcome: &outcome}))
	}
	if err := tx.SetTask(withState(record, durable.TaskState{Status: durable.TaskTerminal, Outcome: &outcome})); err != nil {
		return err
	}
	return s.settleOutcome(tx, record, outcome)
}

// commitState replaces a running task's state with what it committed. A
// terminal state holds as completing while ordinary owned work is live,
// judged on the commit's candidates, so work the same commit creates below the
// task counts. A wait is validated first.
func (s *scheduler) commitState(tx *durable.Tx, inv *invocation, current durable.TaskRecord, next durable.TaskState) error {
	switch next.Status {
	case durable.TaskRunning:
	case durable.TaskWaiting:
		if err := s.validateWait(tx, inv, current, next.On, next.Policy); err != nil {
			return err
		}
	case durable.TaskSleeping:
		if err := validateSleep(inv, current.ID, next.Until); err != nil {
			return err
		}
	case durable.TaskTerminal:
		if next.Outcome == nil {
			return fmt.Errorf("Task %d: a terminal state needs an outcome", current.ID)
		}
		o := overlayOf(tx)
		if _, err := s.loadScopes(false); err != nil {
			return err
		}
		for _, id := range o.order {
			if err := s.loadChain(parentOf(nodeOf(o.tasks[id])), o); err != nil {
				return err
			}
		}
		if s.hasOwnedLive(current.ID, o, nil) {
			return tx.SetTask(withState(current, durable.TaskState{Status: durable.TaskCompleting, Outcome: next.Outcome}))
		}
	default:
		return fmt.Errorf("Task %d cannot commit the state %s", current.ID, next.Status)
	}
	return tx.SetTask(withState(current, next))
}

// validateWait checks that a wait names existing tasks, and failFast only
// tasks the waiter owns, and that it does not close a cycle in the wait-for
// graph, whose edges run from each task to what it waits on and to its live
// ordinary owned work (spec §5.5). An abort handler cannot wait.
//
// Invariant: the wait-for graph is acyclic. Only a wait adds an edge between
// existing tasks, as a new task has only incoming edges, so checking each wait
// maintains it.
func (s *scheduler) validateWait(tx *durable.Tx, inv *invocation, current durable.TaskRecord, on []durable.TaskID, policy durable.JoinPolicy) error {
	if inv.mode == modeAbort {
		return fmt.Errorf("Abort handler of task %d cannot wait", current.ID)
	}
	o := overlayOf(tx)
	var targets []durable.TaskID
	for _, id := range on {
		member, ok := s.record(id, o)
		if !ok {
			stored, err := s.session.Storage().Task(context.WithoutCancel(s.ctx), id)
			if err != nil {
				return err
			}
			if stored == nil {
				return fmt.Errorf("Task %d does not exist", id)
			}
			member = *stored
		}
		ownedByWaiter := member.Owner != nil && *member.Owner == current.ID
		if policy == durable.JoinFailFast && !ownedByWaiter {
			return fmt.Errorf("Task %d can wait failFast only on tasks it owns; %d is not one", current.ID, id)
		}
		// A live child already holds the waiter (task-owned tasks are never
		// background): waiting on it adds no edge. A terminal task has no
		// edges.
		if member.State.Status != durable.TaskTerminal && !ownedByWaiter {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	cycle, err := s.waitCycle(current.ID, targets, o)
	if err != nil {
		return err
	}
	if cycle != nil {
		path := fmt.Sprint(current.ID)
		for _, id := range cycle {
			path += fmt.Sprintf(" -> %d", id)
		}
		return fmt.Errorf("Task %d cannot wait on %d: %s is a wait cycle", current.ID, cycle[0], path)
	}
	return nil
}

// waitCycle returns the path from a member of on back to task in the wait-for
// graph, if the wait would close a cycle. An edge X -> Y means X cannot settle
// before Y: Y is in the on of waiting X that is not abort-marked, or Y is live
// ordinary owned work of X, which holds X as completing. Each task is visited
// once and each subtree walked once.
func (s *scheduler) waitCycle(task durable.TaskID, on []durable.TaskID, o *overlay) ([]durable.TaskID, error) {
	// Owned work is found by walks down, which need the chains of every live
	// task loaded.
	if _, err := s.loadScopes(false); err != nil {
		return nil, err
	}
	from := map[durable.TaskID]durable.TaskID{}
	reached := map[durable.TaskID]bool{}
	walked := map[durable.TaskID]bool{}
	var pending []durable.TaskID
	reach := func(id, via durable.TaskID) {
		if reached[id] {
			return
		}
		reached[id] = true
		from[id] = via
		pending = append(pending, id)
	}
	path := func(end durable.TaskID) []durable.TaskID {
		var steps []durable.TaskID
		for at := end; at != 0; at = from[at] {
			steps = append([]durable.TaskID{at}, steps...)
		}
		return steps
	}
	for _, id := range on {
		reach(id, 0)
	}
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if id == task {
			return path(id), nil
		}
		record, ok := s.record(id, o)
		if !ok || record.State.Status == durable.TaskTerminal {
			continue
		}
		// An abort-marked task waits only for its owned work, as abort runs
		// bottom-up.
		if record.State.Status == durable.TaskWaiting && !record.AbortRequested {
			for _, member := range record.State.On {
				reach(member, id)
			}
		}
		if walked[id] {
			continue
		}
		walked[id] = true
		// Below a walked task the walk does not go again: its own walk covered
		// what lies below.
		var found []durable.TaskID
		s.walkDown(taskKey(id), walkOptions{overlay: o, enter: func(below durable.TaskID) bool { return !walked[below] }},
			func(below durable.TaskID, node taskNode, live *durable.TaskRecord) bool {
				if live != nil && !node.background {
					found = append(found, below)
					reach(below, id)
				}
				return true
			})
		for _, below := range found {
			walked[below] = true
		}
	}
	return nil, nil
}

// end ends an invocation, under mu: its runtime operations fail from now on,
// its context is cancelled, its watches stop, and its task is free.
func (s *scheduler) end(inv *invocation) {
	if inv.ended {
		return
	}
	inv.ended = true
	if s.invocations[inv.taskID] == inv {
		delete(s.invocations, inv.taskID)
		if _, live := s.live[inv.taskID]; live {
			s.runnable.add(inv.taskID)
		}
	}
	// Stopped before the context is cancelled, so they end as stopped rather
	// than cancelled; Stop does not wait for a running listener.
	watches := inv.watches
	inv.watches = map[*durable.DocumentWatch]struct{}{}
	for watch := range watches {
		watch.Stop()
	}
	// Disarmed once mu is released: disarming is host code.
	for timer := range inv.timers {
		if timer.disarm != nil {
			s.afterUnlock = append(s.afterUnlock, timer.disarm)
		}
	}
	clear(inv.timers)
	// Pending waits bound to the invocation, such as a tool's waitForTask(),
	// fail with it.
	inv.cancel(endedError(inv.taskID))
}

// idle reports whether no live non-background task is in the scope of the
// conversation, or of every ownerless one for 0; a task whose owner edges are
// not loaded yet counts as inside.
func (s *scheduler) idle(conversationID durable.ConversationID) bool {
	sc := scope{conversation: conversationID}
	// Tasks whose chain is not loaded are not listed below their scope yet;
	// one counts as inside unless its walk up decides otherwise.
	for _, id := range s.unloaded.values() {
		record, ok := s.live[id]
		if !ok || record.Background {
			continue
		}
		if in, known := s.inScope(parentOf(nodeOf(record)), sc, false); in || !known {
			return false
		}
	}
	starts := []durable.ConversationID{conversationID}
	if conversationID == 0 {
		starts = s.roots.values()
	}
	for _, start := range starts {
		busy := false
		s.walkDown(conversationKey(start), walkOptions{}, func(_ durable.TaskID, node taskNode, live *durable.TaskRecord) bool {
			busy = live != nil && !node.background
			return !busy
		})
		if busy {
			return false
		}
	}
	return true
}

// TaskInspection is a live task and what the scheduler would do with it under
// the current registry.
type TaskInspection struct {
	Record durable.TaskRecord
	// Kind is "running" (an invocation is active), "ready" (the next pass
	// reserves it; Migrates when its definition is newer and has a
	// migration), "waiting" (On lists the live tasks it waits for: the live
	// part of its wait, or, when abort-marked, its live ordinary owned work),
	// "sleeping" (until the Harness clock reaches Until), "completing"
	// (outcome held until its owned work drains), or "blocked" (Reason says
	// why; aborting it settles it as orphaned).
	Kind     string
	Migrates bool
	On       []durable.TaskID
	Until    time.Time
	Reason   string
	Error    error
}

// inspect returns the scheduling state and every live task with its derived
// state; it runs on the Session line and runs no task code.
func (s *scheduler) inspect(snapshot *RegistrySnapshot) (string, []TaskInspection, error) {
	s.mu.Lock()
	defer s.unlock()
	if _, err := s.loadScopes(false); err != nil {
		return "", nil, err
	}
	ids := make([]durable.TaskID, 0, len(s.live))
	for id := range s.live {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	tasks := make([]TaskInspection, 0, len(ids))
	for _, id := range ids {
		tasks = append(tasks, s.inspectTask(s.live[id].Clone(), snapshot))
	}
	scheduling := "paused"
	if s.closing.Load() {
		scheduling = "closing"
	} else if s.enabled {
		scheduling = "running"
	}
	return scheduling, tasks, nil
}

func (s *scheduler) inspectTask(record durable.TaskRecord, snapshot *RegistrySnapshot) TaskInspection {
	inspection := TaskInspection{Record: record}
	if s.invocations[record.ID] != nil {
		inspection.Kind = "running"
		return inspection
	}
	if record.State.Status == durable.TaskCompleting {
		inspection.Kind = "completing"
		return inspection
	}
	if on := s.waitingOn(record, false); len(on) > 0 {
		inspection.Kind, inspection.On = "waiting", on
		return inspection
	}
	if s.notDue(record) {
		inspection.Kind, inspection.Until = "sleeping", time.UnixMilli(*record.State.Until)
		return inspection
	}
	task, migrates, reason, err := s.fit(record, snapshot.Task(record.Kind))
	if reason != "" {
		inspection.Kind, inspection.Reason, inspection.Error = "blocked", reason, err
		return inspection
	}
	if migrates && !task.HasMigration() {
		inspection.Kind, inspection.Reason, inspection.Error = "blocked", blockedMigrationFailed, missingMigration(record, task)
		return inspection
	}
	inspection.Kind, inspection.Migrates = "ready", migrates
	return inspection
}
