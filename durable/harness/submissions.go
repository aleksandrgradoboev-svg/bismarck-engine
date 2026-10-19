package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Input submission behavior while the conversation is busy.
const (
	// WhenBusySteer places the input at the next boundary, between tool
	// rounds.
	WhenBusySteer = "steer"
	// WhenBusyFollowUp places the input when the run answers; the default.
	WhenBusyFollowUp = "followUp"
	// WhenBusyReject rejects the input with *ConversationBusyError.
	WhenBusyReject = "reject"
)

// SubmissionDraft is a host submission: user input that may start a run
// (Type durable.SubmissionTypeInput, with Input), or a passive entry write
// (durable.SubmissionTypeWrite, with Write).
type SubmissionDraft struct {
	// RequestID deduplicates submissions per conversation: a known one
	// returns its existing submission without writing.
	RequestID string
	Type      string
	Input     *InputDraft
	Write     *durable.EntryDraft
}

// InputDraft is the user input of an input submission.
type InputDraft struct {
	Content UserInput
	// WhenBusy is WhenBusySteer, WhenBusyFollowUp (the default) or
	// WhenBusyReject.
	WhenBusy string
	// Entry places the input as this entry; nil is pi.user without data. The
	// kind must not start with "pi.".
	Entry *InputEntry
}

// ConversationBusyError rejects an input submitted with WhenBusyReject while
// the conversation runs.
type ConversationBusyError struct {
	ConversationID durable.ConversationID
}

func (e *ConversationBusyError) Error() string {
	return fmt.Sprintf("Conversation %d is busy", e.ConversationID)
}

// Submission abort results.
const (
	// SubmissionAborted: the queued submission was withdrawn.
	SubmissionAborted = "aborted"
	// SubmissionAlreadyPlaced: the input is in the transcript; abort its
	// conversation to stop the run.
	SubmissionAlreadyPlaced = "already_placed"
	// SubmissionSettled: it had settled already.
	SubmissionSettled = "settled"
	// SubmissionNotFound: no such submission (in that conversation).
	SubmissionNotFound = "not_found"
)

// submissions is admission, waits, and withdrawal of the durable submissions
// of one Harness.
type submissions struct {
	h       *Harness
	mu      sync.Mutex
	waiters *waiters[durable.SubmissionID, durable.SubmissionRecord]
	closed  bool
}

func newSubmissions(h *Harness) (*submissions, error) {
	s := &submissions{h: h, waiters: newWaiters[durable.SubmissionID, durable.SubmissionRecord]()}
	if _, err := h.Session.ObserveCommits(func(publication durable.CommitPublication, _ context.Context) { s.observe(publication) }); err != nil {
		return nil, err
	}
	if _, err := h.Session.SubscribeClose(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		s.waiters.rejectAll(closedError(h.Session))
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *submissions) observe(publication durable.CommitPublication) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, change := range publication.Changes {
		write, ok := change.(durable.SubmissionWrite)
		if !ok || !isSettled(write.Value) {
			continue
		}
		s.waiters.resolve(write.Value.ID, write.Value.Clone())
	}
}

// submit admits a submission in one commit; see admitSubmission.
//
// The admission runs in a pi.durable.submit span: an input's starts a root
// trace of the adapter, linked to the caller's span, so the run it starts is
// its own trace; a write's is a child of the caller's span. The commit runs
// with the span, so its entry and settlement events land on it.
func (s *submissions) submit(ctx context.Context, conversationID durable.ConversationID, draft SubmissionDraft) (*Submission, error) {
	s.h.tasks.resume()
	caller := telemetry.SpanFrom(ctx)
	var parent telemetry.Context = caller
	if draft.Type == durable.SubmissionTypeInput {
		parent = s.h.telemetry
	}
	var submission *Submission
	err := parent.StartSpan(telemetry.SpanOptions{Name: SpanSubmit, Attributes: telemetry.SpanAttributes{
		"submission.type": draft.Type,
		"conversation.id": int64(conversationID),
	}}, func(span telemetry.Span) error {
		if draft.Type == durable.SubmissionTypeInput {
			span.AddLink(telemetry.SpanLink{Context: caller})
		}
		admitted, err := durable.CommitValue(telemetry.WithSpan(ctx, span), s.h.Session, func(tx *durable.Tx) (admission, error) {
			return admitSubmission(tx, conversationID, draft, s.h.tasks.now().UnixMilli(), s.h.settings(), span)
		})
		if err != nil {
			// Error text may carry the caller's content: the status only.
			span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusError})
			return err
		}
		span.SetAttributes(telemetry.SpanAttributes{"submission.id": int64(admitted.id), "result": admitted.status})
		submission = &Submission{ID: admitted.id, s: s}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return submission, nil
}

// record reads a committed submission on the line.
func (s *submissions) record(ctx context.Context, id durable.SubmissionID) (*durable.SubmissionRecord, error) {
	var record *durable.SubmissionRecord
	err := s.h.ReadOnLine(func() error {
		var err error
		record, err = s.h.Storage().Submission(ctx, id)
		return err
	})
	return record, err
}

// get returns the handle of an existing submission, or nil.
func (s *submissions) get(ctx context.Context, id durable.SubmissionID) (*Submission, error) {
	record, err := s.record(ctx, id)
	if err != nil || record == nil {
		return nil, err
	}
	return &Submission{ID: record.ID, s: s}, nil
}

func (s *submissions) status(ctx context.Context, id durable.SubmissionID) (durable.SubmissionRecord, error) {
	record, err := s.record(ctx, id)
	if err != nil {
		return durable.SubmissionRecord{}, err
	}
	if record == nil {
		return durable.SubmissionRecord{}, fmt.Errorf("Submission %d does not exist", id)
	}
	return *record, nil
}

func (s *submissions) wait(ctx context.Context, id durable.SubmissionID) (durable.SubmissionRecord, error) {
	s.h.tasks.resume()
	var w *waiter[durable.SubmissionRecord]
	var settled *durable.SubmissionRecord
	// Check and register on the line so no settling publication falls between
	// them.
	err := s.h.ReadOnLine(func() error {
		record, err := s.h.Storage().Submission(ctx, id)
		if err != nil {
			return err
		}
		if record == nil {
			return fmt.Errorf("Submission %d does not exist", id)
		}
		if isSettled(*record) {
			settled = record
			return nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		// Close rejects registered waiters at once and may begin during the
		// read.
		if s.closed {
			return closedError(s.h.Session)
		}
		w = s.waiters.add(id)
		return nil
	})
	if err != nil {
		return durable.SubmissionRecord{}, err
	}
	if settled != nil {
		return *settled, nil
	}
	return wait(ctx, &s.mu, s.waiters, id, w)
}

// abort withdraws a queued submission and removes its inbox item; placed
// inputs and settled submissions are reported. A non-zero conversationID
// limits it to that conversation.
func (s *submissions) abort(ctx context.Context, id durable.SubmissionID, conversationID durable.ConversationID) (string, error) {
	return durable.CommitValue(ctx, s.h.Session, func(tx *durable.Tx) (string, error) {
		record, err := tx.Submission(id)
		if err != nil {
			return "", err
		}
		if record == nil || (conversationID != 0 && record.ConversationID != conversationID) {
			return SubmissionNotFound, nil
		}
		switch record.Status {
		case durable.SubmissionQueued:
			if err := tx.SettleSubmission(id, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "aborted"}); err != nil {
				return "", err
			}
			return SubmissionAborted, removeInboxItem(tx, record.ConversationID, id)
		case durable.SubmissionPlaced:
			return SubmissionAlreadyPlaced, nil
		}
		return SubmissionSettled, nil
	})
}

func isSettled(record durable.SubmissionRecord) bool {
	return record.Status == durable.SubmissionDone || record.Status == durable.SubmissionUnanswered
}

// Submission is the handle of one durably admitted submission.
type Submission struct {
	ID durable.SubmissionID
	s  *submissions
}

// Status returns the committed record.
func (sub *Submission) Status(ctx context.Context) (durable.SubmissionRecord, error) {
	return sub.s.status(ctx, sub.ID)
}

// Wait returns the settled record (status done or unanswered) and enables
// scheduling; cancelling ctx cancels only this wait.
func (sub *Submission) Wait(ctx context.Context) (durable.SubmissionRecord, error) {
	return sub.s.wait(ctx, sub.ID)
}

// Abort withdraws a queued submission: SubmissionAborted,
// SubmissionAlreadyPlaced or SubmissionSettled.
func (sub *Submission) Abort(ctx context.Context) (string, error) {
	result, err := sub.s.abort(ctx, sub.ID, 0)
	if err != nil {
		return "", err
	}
	if result == SubmissionNotFound {
		return "", fmt.Errorf("Submission %d does not exist", sub.ID)
	}
	return result, nil
}

// admitSubmission admits a submission inside a commit (spec §6). A known
// request ID returns its existing submission without writing. A display-only
// write (only kind and data) is appended and settled done at once, busy or
// not, as it changes nothing the model sees; only behind a queued reset it
// queues, so it lands after the reset instead of before it in the context the
// reset ends. A busy conversation queues anything else in pi.inbox, or
// rejects WhenBusyReject input with *ConversationBusyError. An idle
// conversation with queued items queues it behind them and runs a final
// boundary. Otherwise idle input places its entry (pi.user, or the input's
// own) and starts a run, and an idle write appends its entry and settles
// done, or unanswered "stale" when its head reaches before the active range.
//
// span is the admission span of a public submission, nil for an internal
// one: its carrier is stored as the submission's trace, and a steer links the
// run it steers.
func admitSubmission(tx *durable.Tx, conversationID durable.ConversationID, draft SubmissionDraft, now int64, settings Settings, span telemetry.Span) (admission, error) {
	switch draft.Type {
	case durable.SubmissionTypeInput:
		if draft.Input == nil {
			return admission{}, errors.New("An input submission needs Input")
		}
		if draft.Input.Entry != nil {
			if err := checkInputEntry(*draft.Input.Entry); err != nil {
				return admission{}, err
			}
		}
	case durable.SubmissionTypeWrite:
		if draft.Write == nil {
			return admission{}, errors.New("A write submission needs Write")
		}
	default:
		return admission{}, fmt.Errorf("Unknown submission type %q", draft.Type)
	}
	if draft.RequestID != "" {
		existing, err := tx.SubmissionByRequest(conversationID, draft.RequestID)
		if err != nil {
			return admission{}, err
		}
		if existing != nil {
			if existing.Type != draft.Type {
				return admission{}, fmt.Errorf("Request %s already identifies a submission of type %s", draft.RequestID, existing.Type)
			}
			status := existing.Status
			if status == durable.SubmissionUnanswered {
				status = durable.SubmissionDone
			}
			return admission{id: existing.ID, status: status}, nil
		}
	}
	var trace telemetry.Carrier
	propagation, hasPropagation := propagationOf(span)
	if span != nil && hasPropagation {
		trace = propagation.Inject(span)
	}
	create := func(record durable.SubmissionRecord) (durable.SubmissionID, error) {
		record.ConversationID, record.RequestID, record.Trace, record.Type = conversationID, draft.RequestID, trace, draft.Type
		created, err := tx.CreateSubmission(record)
		return created.ID, err
	}
	done := func(id durable.SubmissionID, err error) (admission, error) {
		return admission{id: id, status: durable.SubmissionDone}, err
	}
	appendWrite := func() (admission, error) {
		entry, err := tx.AppendEntry(conversationID, *draft.Write)
		if err != nil {
			return admission{}, err
		}
		id := entry.ID
		return done(create(durable.SubmissionRecord{Status: durable.SubmissionDone, Entry: &id}))
	}
	if draft.Type == durable.SubmissionTypeWrite && isDisplayOnly(*draft.Write) {
		inbox, err := inboxDraft(tx, conversationID)
		if err != nil {
			return admission{}, err
		}
		items, err := inboxItems(inbox)
		if err != nil {
			return admission{}, err
		}
		if !hasQueuedReset(items) {
			return appendWrite()
		}
	}
	live, err := liveDraft(tx, conversationID)
	if err != nil {
		return admission{}, err
	}
	run, err := liveRun(live)
	if err != nil {
		return admission{}, err
	}
	busy := run != nil
	// A steer links the run it steers; its work stays in the run's trace.
	if busy && span != nil && hasPropagation && draft.Type == durable.SubmissionTypeInput && draft.Input.WhenBusy == WhenBusySteer {
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return admission{}, err
		}
		if task != nil && task.Trace != nil {
			span.AddLink(telemetry.SpanLink{Context: propagation.Extract(task.Trace)})
		}
	}
	if busy && draft.Type == durable.SubmissionTypeInput && draft.Input.WhenBusy == WhenBusyReject {
		return admission{}, &ConversationBusyError{ConversationID: conversationID}
	}
	// A boundary reads the table, so it is prepared before the first table
	// write; a busy one needs none.
	var b *boundary
	if !busy {
		if b, err = prepareBoundary(tx, conversationID, settings); err != nil {
			return admission{}, err
		}
	}
	queued := busy
	if b != nil {
		items, err := inboxItems(b.inbox)
		if err != nil {
			return admission{}, err
		}
		queued = len(items) > 0
	}
	if queued {
		id, err := create(durable.SubmissionRecord{Status: durable.SubmissionQueued})
		if err != nil {
			return admission{}, err
		}
		item := inboxItem{id: id, mode: modeWrite}
		if draft.Type == durable.SubmissionTypeWrite {
			item.write = *draft.Write
		} else {
			item.mode = modeFollowUp
			if draft.Input.WhenBusy == WhenBusySteer {
				item.mode = modeSteer
			}
			item.content = draft.Input.Content
			if draft.Input.Entry != nil {
				picked := pickInputEntry(*draft.Input.Entry)
				item.input = &picked
			}
		}
		var inbox *delta.Draft
		if b != nil {
			inbox = b.inbox
		} else if inbox, err = inboxDraft(tx, conversationID); err != nil {
			return admission{}, err
		}
		if err := pushInboxItem(inbox, item); err != nil {
			return admission{}, err
		}
		if b == nil {
			return admission{id: id, status: durable.SubmissionQueued}, nil
		}
		placed, err := applyBoundary(tx, b, true, now)
		if err != nil {
			return admission{}, err
		}
		if len(placed.users) > 0 {
			if err := startRun(tx, conversationID, live, placed.users); err != nil {
				return admission{}, err
			}
		}
		// pi: a write is placed or stale at the boundary, both "done"; an input
		// is placed when it is the last user item the boundary selected.
		result := admission{id: id, status: durable.SubmissionQueued}
		if draft.Type == durable.SubmissionTypeWrite {
			result.status = durable.SubmissionDone
		} else if len(placed.users) > 0 && placed.users[len(placed.users)-1] == id {
			result.status = durable.SubmissionPlaced
		}
		return result, nil
	}
	if draft.Type == durable.SubmissionTypeWrite {
		if isStale(b, *draft.Write) {
			return done(create(durable.SubmissionRecord{Status: durable.SubmissionUnanswered, Reason: "stale"}))
		}
		return appendWrite()
	}
	entry, err := appendInput(tx, conversationID, draft.Input.Content, draft.Input.Entry, now)
	if err != nil {
		return admission{}, err
	}
	entryID := entry.ID
	id, err := create(durable.SubmissionRecord{Status: durable.SubmissionPlaced, Entry: &entryID})
	if err != nil {
		return admission{}, err
	}
	return admission{id: id, status: durable.SubmissionPlaced}, startRun(tx, conversationID, live, []durable.SubmissionID{id})
}

// admission is what admitting a submission did: its ID and the result the
// admission span records ("queued", "placed" or "done"; a stale write is
// "done").
type admission struct {
	id     durable.SubmissionID
	status string
}
