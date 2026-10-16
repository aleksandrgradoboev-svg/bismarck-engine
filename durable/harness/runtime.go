package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// phase is what a runtime reads for the phase handler it serves: the phase's
// snapshot and task, and its lazily resolved agent.
type phase struct {
	snapshot func() *RegistrySnapshot
	task     func() Task
	mu       sync.Mutex
	agent    *agentCall
}

// agentCall is one agent resolution shared by the callers of a phase.
type agentCall struct {
	done  chan struct{}
	agent *Agent
	err   error
}

func (p *phase) reset() {
	p.mu.Lock()
	p.agent = nil
	p.mu.Unlock()
}

// TaskRuntime is the operations of one task invocation. Every operation fails
// once the invocation has ended; watches acquired through it stop then. The
// context a phase or abort handler receives is the invocation's: it is
// cancelled when the task is abort-marked, when the Harness closes, and when
// the invocation ends.
type TaskRuntime struct {
	TaskID         durable.TaskID
	ConversationID durable.ConversationID
	s              *scheduler
	inv            *invocation
	phase          *phase
}

func (s *scheduler) newRuntime(inv *invocation, ph *phase) *TaskRuntime {
	return &TaskRuntime{TaskID: inv.taskID, ConversationID: inv.conversationID, s: s, inv: inv, phase: ph}
}

// check fails once the invocation has ended.
func (rt *TaskRuntime) check() error {
	rt.s.mu.Lock()
	ended := rt.inv.ended
	rt.s.unlock()
	if ended {
		return endedError(rt.TaskID)
	}
	return nil
}

// bind returns ctx cancelled also when the invocation's context is.
func (rt *TaskRuntime) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	bound, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(rt.inv.ctx, func() { cancel(context.Cause(rt.inv.ctx)) })
	return bound, func() {
		stop()
		cancel(context.Canceled)
	}
}

// Registry returns the registry snapshot of the current phase; it is
// refreshed at every phase boundary.
func (rt *TaskRuntime) Registry() *RegistrySnapshot { return rt.phase.snapshot() }

// Settings returns the Harness settings, resolved at each call.
func (rt *TaskRuntime) Settings() Settings { return rt.s.settings() }

// Models returns the Harness model access.
func (rt *TaskRuntime) Models() ai.Models { return rt.s.models }

// Agent returns the task's conversation's agent, resolved at most once per
// phase, at first use, with the invocation's context, and fixed for the
// phase. Cancelling ctx stops only this wait.
func (rt *TaskRuntime) Agent(ctx context.Context) (*Agent, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	p := rt.phase
	p.mu.Lock()
	call := p.agent
	if call == nil {
		call = &agentCall{done: make(chan struct{})}
		p.agent = call
		snapshot := p.snapshot()
		go func() {
			defer close(call.done)
			defer func() {
				// Host settings and registries may panic; that is the
				// resolution's failure, as a throw is in pi.
				if r := recover(); r != nil {
					call.agent, call.err = nil, panicError(r)
				}
			}()
			call.agent, call.err = rt.s.agent(rt.inv.ctx, rt.ConversationID, snapshot)
		}()
	}
	p.mu.Unlock()
	select {
	case <-call.done:
		return call.agent, call.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// Env builds the environment of the task's conversation.
func (rt *TaskRuntime) Env(ctx context.Context) (env.ExecutionEnv, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	return rt.s.env(ctx, rt.ConversationID)
}

// Commit commits on the Session line after rereading the task. It fails when
// the task is no longer running, the invocation ended, the Harness is
// closing, or, in a run invocation, the task carries an abort mark. A
// returned state replaces the task's state in the same commit; nil leaves it
// unchanged. Tx.CreateTask defaults to the task's conversation. Inside change
// read only through tx: the Session's and the runtime's own reads queue on
// the line change holds, and never return.
func (rt *TaskRuntime) Commit(ctx context.Context, change func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error)) error {
	return rt.gated(ctx, func(tx *durable.Tx, current durable.TaskRecord) error {
		next, err := change(tx, current)
		if err != nil || next == nil {
			return err
		}
		rt.s.mu.Lock()
		defer rt.s.unlock()
		return rt.s.commitState(tx, rt.inv, current, *next)
	})
}

// gated commits after rereading the task on the line and gating the
// invocation; change runs without the scheduler's mutex.
func (rt *TaskRuntime) gated(ctx context.Context, change func(tx *durable.Tx, current durable.TaskRecord) error) error {
	if err := rt.check(); err != nil {
		return err
	}
	scope := durable.TransactionScope{ConversationID: rt.ConversationID, TaskID: rt.TaskID}
	return rt.s.session.CommitWith(ctx, scope, func(tx *durable.Tx) error {
		s := rt.s
		s.mu.Lock()
		if rt.inv.ended {
			s.unlock()
			return endedError(rt.TaskID)
		}
		if s.closing.Load() {
			s.unlock()
			return closedError(s.session)
		}
		found, ok := s.live[rt.TaskID]
		if !ok {
			s.unlock()
			return fmt.Errorf("Task %d is terminal", rt.TaskID)
		}
		if found.State.Status != durable.TaskRunning {
			s.unlock()
			return fmt.Errorf("Task %d is %s", rt.TaskID, found.State.Status)
		}
		if rt.inv.mode == modeRun && found.AbortRequested {
			s.unlock()
			return fmt.Errorf("Task %d has a durable abort mark", rt.TaskID)
		}
		current := found.Clone()
		s.unlock()
		return change(tx, current)
	})
}

// Memo returns the durable memo name of this task, or nil.
func (rt *TaskRuntime) Memo(ctx context.Context, name string) (json.RawMessage, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	rt.s.mu.Lock()
	defer rt.s.unlock()
	value := rt.s.live[rt.TaskID].Memos[name]
	if value == nil {
		return nil, nil
	}
	return append(json.RawMessage(nil), value...), nil
}

// MemoOr stores candidate as the memo name unless one exists, and returns the
// durable winner.
func (rt *TaskRuntime) MemoOr(ctx context.Context, name string, candidate any) (json.RawMessage, error) {
	raw, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	var winner json.RawMessage
	err = rt.gated(ctx, func(tx *durable.Tx, current durable.TaskRecord) error {
		if existing, ok := current.Memos[name]; ok {
			winner = existing
			return nil
		}
		record := current.Clone()
		if record.Memos == nil {
			record.Memos = map[string]json.RawMessage{}
		}
		record.Memos[name] = raw
		winner = raw
		return tx.SetTask(record)
	})
	return winner, err
}

// GetTask returns the committed task record, or nil.
func (rt *TaskRuntime) GetTask(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	var record *durable.TaskRecord
	err := rt.s.session.ReadOnLine(func() error {
		var err error
		record, err = rt.s.session.Storage().Task(ctx, id)
		return err
	})
	return record, err
}

// WaitForTask waits for the task's terminal record; it fails when the
// invocation ends.
func (rt *TaskRuntime) WaitForTask(ctx context.Context, id durable.TaskID) (durable.TaskRecord, error) {
	if err := rt.check(); err != nil {
		return durable.TaskRecord{}, err
	}
	bound, cancel := rt.bind(ctx)
	defer cancel()
	return rt.s.waitForTask(bound, id)
}

// AbortOwned aborts a task this task owns, as Harness.AbortTask does, and
// returns once it is terminal. It fails for a task another owns, and returns
// at once for a terminal one.
func (rt *TaskRuntime) AbortOwned(ctx context.Context, id durable.TaskID) error {
	if err := rt.check(); err != nil {
		return err
	}
	bound, cancel := rt.bind(ctx)
	defer cancel()
	var record *durable.TaskRecord
	if err := rt.s.session.ReadOnLine(func() error {
		var err error
		record, err = rt.s.session.Storage().Task(bound, id)
		return err
	}); err != nil {
		return err
	}
	if record == nil || record.Owner == nil || *record.Owner != rt.TaskID {
		return fmt.Errorf("Task %d is not owned by task %d", id, rt.TaskID)
	}
	if record.State.Status == durable.TaskTerminal {
		return nil
	}
	if _, err := rt.s.abort(bound, id, true); err != nil {
		return err
	}
	_, err := rt.s.waitForTask(bound, id)
	return err
}

// OwnedTasks returns the live tasks this task owns directly, in ID order:
// also those an earlier invocation created, before a restart. Tasks in
// conversations it owns are not included. The records are copies.
func (rt *TaskRuntime) OwnedTasks(ctx context.Context) ([]durable.TaskRecord, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	var owned []durable.TaskRecord
	err := rt.s.session.ReadOnLine(func() error {
		s := rt.s
		s.mu.Lock()
		defer s.unlock()
		// The step that ends the invocation may have run on the line before
		// this.
		if rt.inv.ended {
			return endedError(rt.TaskID)
		}
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		// Every live task is listed under its parent node, its owner task when
		// it has one.
		if children := s.below[taskKey(rt.TaskID)]; children != nil {
			for _, node := range children.values() {
				if node.isConversation() {
					continue
				}
				if record, ok := s.live[node.task]; ok && record.Owner != nil && *record.Owner == rt.TaskID {
					owned = append(owned, record.Clone())
				}
			}
		}
		sort.Slice(owned, func(i, j int) bool { return owned[i].ID < owned[j].ID })
		return nil
	})
	return owned, err
}

// Outcomes returns the outcomes of terminal tasks, in order; it fails when one
// is missing or not terminal. It is meant for after a wait.
func (rt *TaskRuntime) Outcomes(ctx context.Context, ids []durable.TaskID) ([]durable.TaskOutcome, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	var outcomes []durable.TaskOutcome
	err := rt.s.session.ReadOnLine(func() error {
		for _, id := range ids {
			record, err := rt.s.session.Storage().Task(ctx, id)
			if err != nil {
				return err
			}
			if record == nil || record.State.Status != durable.TaskTerminal || record.State.Outcome == nil {
				return fmt.Errorf("Task %d is not terminal", id)
			}
			outcomes = append(outcomes, *record.State.Outcome)
		}
		return nil
	})
	return outcomes, err
}

// Entry returns a committed entry visible from the task's conversation, or
// nil.
func (rt *TaskRuntime) Entry(ctx context.Context, id durable.EntryID) (*durable.EntryRecord, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	var entry *durable.EntryRecord
	err := rt.s.session.ReadOnLine(func() error {
		found, err := rt.s.session.Storage().VisibleEntry(ctx, rt.ConversationID, id)
		if err != nil || found == nil {
			return err
		}
		e := found.Entry.Clone()
		entry = &e
		return nil
	})
	return entry, err
}

// Context returns the committed raw active transcript and model context of a
// conversation, optionally cut off at the visible entry at.
func (rt *TaskRuntime) Context(ctx context.Context, conversationID durable.ConversationID, at *durable.EntryID) (ContextView, error) {
	if err := rt.check(); err != nil {
		return ContextView{}, err
	}
	return readContext(ctx, rt.s.session, conversationID, at)
}

// Now returns the Harness clock.
func (rt *TaskRuntime) Now() (time.Time, error) {
	if err := rt.check(); err != nil {
		return time.Time{}, err
	}
	return rt.s.now(), nil
}

// Report forwards a non-fatal failure to Options.OnReport.
func (rt *TaskRuntime) Report(err error) error {
	if e := rt.check(); e != nil {
		return e
	}
	rt.s.report(err)
	return nil
}

// Sleep returns once the Harness clock reaches until, rechecking it after
// every timer; it fails when the invocation or ctx is cancelled.
func (rt *TaskRuntime) Sleep(ctx context.Context, until time.Time) error {
	if err := rt.check(); err != nil {
		return err
	}
	for {
		select {
		case <-rt.inv.ctx.Done():
			return context.Cause(rt.inv.ctx)
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}
		remaining := until.Sub(rt.s.now())
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-rt.inv.ctx.Done():
			timer.Stop()
			return context.Cause(rt.inv.ctx)
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		}
	}
}

// Snapshot reads a committed document.
func (rt *TaskRuntime) Snapshot(ctx context.Context, ref durable.DocRef) (map[string]any, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	return rt.s.session.Snapshot(ctx, ref)
}

// SnapshotAsOf reads a committed document as of a visible entry.
func (rt *TaskRuntime) SnapshotAsOf(ctx context.Context, ref durable.DocRef, at durable.EntryID) (map[string]any, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	return rt.s.session.SnapshotAsOf(ctx, ref, at)
}

// WatchDoc watches a document; the watch stops when the invocation ends.
func (rt *TaskRuntime) WatchDoc(ctx context.Context, ref durable.DocRef) (*durable.DocumentWatch, error) {
	if err := rt.check(); err != nil {
		return nil, err
	}
	watch, err := rt.s.session.WatchDoc(ctx, ref)
	if err != nil || watch == nil {
		return watch, err
	}
	s := rt.s
	s.mu.Lock()
	if rt.inv.ended {
		s.unlock()
		watch.Stop()
		return nil, endedError(rt.TaskID)
	}
	rt.inv.watches[watch] = struct{}{}
	s.unlock()
	go func() {
		<-watch.Closed()
		s.mu.Lock()
		delete(rt.inv.watches, watch)
		s.unlock()
	}()
	return watch, nil
}

// EachHook calls invoke with each hook handler of type H that the extensions
// selected by the task's conversation register for its task name, in
// extension order. A failing invoke is reported and the next handler runs;
// once the invocation is cancelled, the failure is returned instead.
func EachHook[H any](ctx context.Context, rt *TaskRuntime, invoke func(handlers H) error) error {
	agent, err := rt.Agent(ctx)
	if err != nil {
		return err
	}
	for _, handlers := range agentHooks(agent, rt.phase.task().Name()) {
		typed, ok := handlers.(H)
		if !ok {
			continue
		}
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = panicError(r)
				}
			}()
			return invoke(typed)
		}()
		if err == nil {
			continue
		}
		if rt.inv.ctx.Err() != nil {
			return err
		}
		rt.s.report(err)
	}
	return nil
}
