package harness

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// propagationOf is the propagation an adapter carries, if it has one; pi
// tests `propagation !== undefined`, which MetricsOf's no-op fallback would
// hide.
func propagationOf(c telemetry.Context) (telemetry.Propagation, bool) {
	if capable, ok := c.(telemetry.PropagationCapable); ok {
		if propagation := capable.Propagation(); propagation != nil {
			return propagation, true
		}
	}
	return nil, false
}

// maxStatusMessage caps a phase's error status message (pi slice(0, 500),
// counted in runes here rather than UTF-16 units).
const maxStatusMessage = 500

// phase runs one phase invocation, its handler and the decision commit after
// it, in a pi.durable.phase span (pi #phase). The span's parent is the task's
// stored trace, or the adapter root; a first invocation links the task's
// creation-time links. The outcome, status and duration are read from the
// task's record as the invocation left it.
func (s *scheduler) phase(inv *invocation, task *durable.TaskRecord, run func(ctx context.Context) *durable.TaskRecord) *durable.TaskRecord {
	parent := s.telemetry
	if propagation, ok := propagationOf(s.telemetry); ok && task.Trace != nil {
		parent = propagation.Extract(task.Trace)
	}
	var checkpoint struct {
		Phase   string `json:"phase"`
		Attempt *int64 `json:"attempt"`
	}
	_ = json.Unmarshal(task.State.Checkpoint, &checkpoint)
	s.mu.Lock()
	resumed := s.resumed.has(task.ID)
	s.resumed.delete(task.ID)
	links := inv.links
	s.unlock()
	attributes := telemetry.SpanAttributes{
		"task.id":   int64(task.ID),
		"task.name": task.Kind,
		"phase":     checkpoint.Phase,
		"kind":      inv.mode,
		"resumed":   resumed,
	}
	if checkpoint.Attempt != nil {
		attributes["attempt"] = *checkpoint.Attempt
	}
	var result *durable.TaskRecord
	_ = parent.StartSpan(telemetry.SpanOptions{Name: SpanPhase, Attributes: attributes}, func(span telemetry.Span) error {
		if propagation, ok := propagationOf(span); ok {
			for _, carrier := range links {
				span.AddLink(telemetry.SpanLink{Context: propagation.Extract(carrier)})
			}
		}
		inv.span.Store(&spanRef{span: span})
		defer inv.span.Store(nil)
		started := time.Now()
		result = run(telemetry.WithSpan(inv.ctx, span))

		s.mu.Lock()
		record, fault, ended := inv.record, inv.fault, inv.ended
		s.unlock()
		outcome, terminal := phaseOutcome(record, fault, ended || s.closing.Load())
		span.SetAttributes(telemetry.SpanAttributes{"outcome": outcome})
		if terminal != nil && (terminal.Status == durable.OutcomeFaulted || terminal.Status == durable.OutcomeFailed || terminal.Status == durable.OutcomeOrphaned) {
			message := terminal.Reason
			if terminal.Status != durable.OutcomeOrphaned && terminal.Error != nil {
				message = terminal.Error.Message
			}
			if runes := []rune(message); len(runes) > maxStatusMessage {
				message = string(runes[:maxStatusMessage])
			}
			span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusError, Error: &telemetry.SpanError{Name: "task_" + terminal.Status, Message: message}})
		} else if outcome == "aborted" {
			span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
		}
		elapsed := float64(time.Since(started)) / float64(time.Millisecond)
		s.phaseDuration.Record(elapsed, telemetry.SpanAttributes{"task": task.Kind, "phase": checkpoint.Phase, "outcome": outcome}, span)
		return nil
	})
	return result
}

// phaseOutcome is a phase's outcome attribute: done, aborted or faulted for a
// decided outcome, waiting or sleeping, aborted when the invocation ended
// without one, else next=<phase>.
func phaseOutcome(record durable.TaskRecord, fault *durable.TaskOutcome, ended bool) (string, *durable.TaskOutcome) {
	terminal := fault
	if terminal == nil && (record.State.Status == durable.TaskTerminal || record.State.Status == durable.TaskCompleting) {
		terminal = record.State.Outcome
	}
	switch {
	case terminal != nil:
		switch terminal.Status {
		case durable.OutcomeCompleted:
			return "done", terminal
		case durable.OutcomeAborted:
			return "aborted", terminal
		}
		return "faulted", terminal
	case record.State.Status == durable.TaskWaiting || record.State.Status == durable.TaskSleeping:
		return record.State.Status, nil
	case ended:
		return "aborted", nil
	}
	name, _ := CheckpointPhase(record.State.Checkpoint)
	return "next=" + name, nil
}
