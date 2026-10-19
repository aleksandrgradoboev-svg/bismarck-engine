package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Next is the next state that replaces a running task's checkpoint.
func Next(checkpoint any) (*durable.TaskState, error) {
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, err
	}
	return &durable.TaskState{Status: durable.TaskRunning, Checkpoint: raw}, nil
}

// Wait is the next state that parks a task, with checkpoint, until every task
// in on is terminal; with durable.JoinFailFast, a failure among tasks the
// waiter owns aborts the others.
func Wait(on []durable.TaskID, policy durable.JoinPolicy, checkpoint any) (*durable.TaskState, error) {
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, err
	}
	return &durable.TaskState{Status: durable.TaskWaiting, Checkpoint: raw, On: append([]durable.TaskID(nil), on...), Policy: policy}, nil
}

// Sleep is the next state that parks a task without an invocation until the
// Harness clock reaches until, resuming at checkpoint.
func Sleep(until time.Time, checkpoint any) (*durable.TaskState, error) {
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, err
	}
	state := durable.SleepingState(raw, until.UnixMilli())
	return &state, nil
}

// Complete is the next state that completes a task with result.
func Complete(result any) (*durable.TaskState, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: durable.OutcomeCompleted, Result: raw}}, nil
}

// Abort is the next state that ends a task as aborted for reason.
func Abort(reason string) *durable.TaskState {
	return &durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: durable.OutcomeAborted, Reason: reason}}
}

// Fail is the next state that ends a task with an expected failure.
func Fail(message string) *durable.TaskState {
	return &durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: durable.OutcomeFailed, Error: &durable.TaskOutcomeError{Message: message}}}
}

// withState replaces a live record's state; memos disappear once an outcome is
// decided.
func withState(record durable.TaskRecord, state durable.TaskState) durable.TaskRecord {
	record = record.Clone()
	record.State = state
	// Memos and the restart count disappear once an outcome is decided.
	if state.Status == durable.TaskTerminal || state.Status == durable.TaskCompleting {
		record.Memos = nil
		record.Restarts = 0
	}
	return record
}

// progressed returns the record without its restart count, as after a state
// the task committed.
func progressed(record durable.TaskRecord) durable.TaskRecord {
	record = record.Clone()
	record.Restarts = 0
	return record
}

// withAbortMark returns the record with an abort mark: "restart" for an
// abandonment; without a reason, a request, which replaces "restart". The
// restart count starts over: it now counts cut-offs of the abort handler.
func withAbortMark(record durable.TaskRecord, reason string) durable.TaskRecord {
	record = record.Clone()
	record.AbortRequested = true
	record.AbortReason = reason
	record.Restarts = 0
	return record
}

// failedOutcome reports whether the record holds or ends with an outcome other
// than completed.
func failedOutcome(record durable.TaskRecord) bool {
	state := record.State
	return (state.Status == durable.TaskCompleting || state.Status == durable.TaskTerminal) &&
		state.Outcome != nil && state.Outcome.Status != durable.OutcomeCompleted
}

// cancellationIntent is a live owner's durable cancellation intent: its abort
// mark, or a held outcome other than completed.
func cancellationIntent(record durable.TaskRecord) bool {
	return record.State.Status != durable.TaskTerminal && (record.AbortRequested || failedOutcome(record))
}

// jsonEqual compares two JSON values structurally; object key order is
// ignored.
func jsonEqual(left, right json.RawMessage) bool {
	var l, r any
	if json.Unmarshal(left, &l) != nil || json.Unmarshal(right, &r) != nil {
		return false
	}
	return reflect.DeepEqual(l, r)
}

// errorMessage is the message of a thrown value.
func errorMessage(err any) string {
	if e, ok := err.(error); ok {
		return e.Error()
	}
	return fmt.Sprint(err)
}

// closedError is what a closed Harness rejects with: a SessionFailedError with
// the cause when it closed because the Session failed.
func closedError(session *durable.Session) error {
	if failure := session.Failure(); failure != nil {
		return &durable.SessionFailedError{Cause: failure}
	}
	return errors.New("Harness is closed")
}

func endedError(id durable.TaskID) error {
	return fmt.Errorf("Task %d invocation has ended", id)
}
