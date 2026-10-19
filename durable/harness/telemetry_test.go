package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Ported from pi packages/durable/test/harness-telemetry.test.ts (snapshot
// d2e7d8b7). Waiting for their slices: the cases that run tools — the
// content-free submit and phase spans with nested tool parents, the steer
// link (without a postTools boundary a steer starts a run of its own), an
// input submitted inside a tool's turn — slice 2; the two compaction cases
// slice 3; the codemode replay stage 5. n/a: the async listener case, as a Go
// commit listener cannot return a promise.

func trackedSetup(t *testing.T) (*chatSetup, *telemetry.InMemoryContext) {
	t.Helper()
	setup := newChatSetup(t)
	recorder := telemetry.NewInMemoryContext()
	setup.telemetry = recorder
	return setup, recorder
}

func installTask(t *testing.T, setup *chatSetup, task Task) {
	t.Helper()
	must(t, setup.registry.Install(&Extension{Name: "task:" + task.Name(), Tasks: []Task{task}}))
}

func spansNamed(recorder *telemetry.InMemoryContext, name string) []telemetry.RecordedSpan {
	var out []telemetry.RecordedSpan
	for _, span := range recorder.Spans() {
		if span.Name == name {
			out = append(out, span)
		}
	}
	return out
}

func allSettled(t *testing.T, recorders ...*telemetry.InMemoryContext) {
	t.Helper()
	eventually(t, func() bool {
		for _, recorder := range recorders {
			for _, span := range recorder.Spans() {
				if !span.Settled {
					return false
				}
			}
		}
		return true
	})
}

func measurementsNamed(recorder *telemetry.InMemoryContext, name string) []telemetry.RecordedMeasurement {
	var out []telemetry.RecordedMeasurement
	for _, m := range recorder.Measurements() {
		if m.Name == name {
			out = append(out, m)
		}
	}
	return out
}

func reportTypes(recorder *telemetry.InMemoryContext) []telemetry.SpanAttributes {
	out := []telemetry.SpanAttributes{}
	for _, m := range measurementsNamed(recorder, MetricReports) {
		out = append(out, m.Attributes)
	}
	return out
}

func mustNotContain(t *testing.T, what string, value any, secrets ...string) {
	t.Helper()
	raw, err := json.Marshal(value)
	must(t, err)
	for _, secret := range secrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s carries %q: %s", what, secret, raw)
		}
	}
}

func equalValues(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", what, got, want)
	}
}

// countingStorage counts Submission reads.
type countingStorage struct {
	durable.Storage
	submissionReads atomic.Int32
}

func (s *countingStorage) Submission(ctx context.Context, id durable.SubmissionID) (*durable.SubmissionRecord, error) {
	s.submissionReads.Add(1)
	return s.Storage.Submission(ctx, id)
}

// pi also spies that the adapter root is never injected and no undefined
// carrier extracted; here a span's propagation is the adapter's own, so what
// applies is the stored-record reads and the results.
func TestTelemetryReturnsAdmissionResultsWithoutRereadingRecords(t *testing.T) {
	setup, recorder := trackedSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	storage := &countingStorage{Storage: memory.New()}
	_, root := openChat(t, storage, setup)
	reads := storage.submissionReads.Load()
	firstSubmission := submit(t, root, inputDraft("first"))
	awaitSignal(t, reached, "the first request")
	draft := inputDraft("second")
	draft.RequestID = "queued"
	second := submit(t, root, draft)
	ignored := inputDraft("ignored")
	ignored.RequestID = "queued"
	duplicate := submit(t, root, ignored)
	if duplicate.ID != second.ID {
		t.Fatalf("duplicate = %d, want %d", duplicate.ID, second.ID)
	}
	if got := storage.submissionReads.Load(); got != reads {
		t.Fatalf("admission read %d stored submissions", got-reads)
	}
	var results []any
	for _, span := range spansNamed(recorder, SpanSubmit) {
		results = append(results, span.Attributes["result"])
	}
	equalValues(t, "results", results, []any{"placed", "queued", "queued"})
	release()
	waitFor(t, firstSubmission)
	waitFor(t, second)
}

func TestTelemetryInheritsOmittedTaskTracesAndStoresNoCarrierForAnExplicitRoot(t *testing.T) {
	setup, recorder := trackedSetup(t)
	task := oneStep(t, "test.trace", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	installTask(t, setup, task)
	h, root := openChat(t, memory.New(), setup)
	var ids []durable.TaskID
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "creator"}, func(span telemetry.Span) error {
		return root.Commit(telemetry.WithSpan(bg, span), func(tx *durable.Tx) error {
			// pi's omitted and undefined trace are both a nil Trace in Go.
			for _, options := range []durable.TaskOptions{{}, {TraceRoot: true}} {
				options.Ownership = durable.TaskOwnership{Kind: durable.OwnershipConversation}
				id, err := CreateTask[any](tx, task, nil, options)
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return nil
		})
	}))
	for _, id := range ids {
		_, err := h.WaitForTask(bg, id)
		must(t, err)
	}
	allSettled(t, recorder)
	creator := recorder.Spans()[0].Context
	var traces []telemetry.Carrier
	for _, id := range ids {
		traces = append(traces, getTask(t, h, id).Trace)
	}
	equalValues(t, "traces", traces, []telemetry.Carrier{creator, nil})
	// The two tasks run concurrently here, so their phases are matched by
	// task, not by start order as in pi.
	parents := map[any]telemetry.Carrier{}
	for _, span := range spansNamed(recorder, SpanPhase) {
		parents[span.Attributes["task.id"]] = span.ParentContext
	}
	equalValues(t, "phase parents", parents, map[any]telemetry.Carrier{int64(ids[0]): creator, int64(ids[1]): nil})
}

type countState struct {
	Phase string `json:"phase"`
	Count int    `json:"count"`
}

func TestTelemetryRecordsCheckpointsOnlyWhenTheRunningPhaseChanges(t *testing.T) {
	setup, recorder := trackedSetup(t)
	next := func(phase string, count int) func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
		return func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
			return Next(countState{phase, count})
		}
	}
	task, err := DefineTask(TaskSpec[any, countState, any]{
		Name:    "test.checkpoint",
		Version: 1,
		Initial: func(any) countState { return countState{Phase: "call"} },
		Phases: map[string]func(context.Context, *Running[any, countState], *TaskRuntime) error{
			"call": func(ctx context.Context, _ *Running[any, countState], rt *TaskRuntime) error {
				for _, step := range []func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error){next("call", 1), next("call", 2), next("finish", 2)} {
					if err := rt.Commit(ctx, step); err != nil {
						return err
					}
				}
				return nil
			},
			"finish": func(ctx context.Context, _ *Running[any, countState], rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(ctx context.Context, _ *Running[any, countState], rt *TaskRuntime) error {
			return rt.Commit(ctx, abortWith("test"))
		},
	})
	must(t, err)
	installTask(t, setup, task)
	h, root := openChat(t, memory.New(), setup)
	id := start(t, root, task, false)
	_, err = h.WaitForTask(bg, id)
	must(t, err)
	allSettled(t, recorder)
	var checkpoints []telemetry.RecordedEvent
	for _, span := range recorder.Spans() {
		for _, event := range span.Events {
			if event.Name == EventCheckpoint {
				checkpoints = append(checkpoints, event)
			}
		}
	}
	equalValues(t, "checkpoints", checkpoints, []telemetry.RecordedEvent{{Name: EventCheckpoint, Attributes: telemetry.SpanAttributes{"phase": "finish"}}})
}

func TestTelemetryStartsEachQueuedInputsRunInItsAdmissionTraceAndLinksInputRootsToTheirCaller(t *testing.T) {
	setup, recorder := trackedSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	h, root := openChat(t, memory.New(), setup)
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(caller telemetry.Span) error {
		ctx := telemetry.WithSpan(bg, caller)
		one, err := root.Submit(ctx, inputDraft("first"))
		must(t, err)
		awaitSignal(t, reached, "the first request")
		two, err := root.Submit(ctx, inputDraft("second"))
		must(t, err)
		if got := submissionStatus(t, two).Status; got != durable.SubmissionQueued {
			t.Fatalf("second = %s, want queued", got)
		}
		release()
		waitFor(t, one)
		waitFor(t, two)
		return nil
	}))
	must(t, h.WaitForIdle(bg))
	allSettled(t, recorder)
	caller := spansNamed(recorder, "caller")[0]
	submits := spansNamed(recorder, SpanSubmit)
	if len(submits) != 2 {
		t.Fatalf("submit spans = %d, want 2", len(submits))
	}
	for _, s := range submits {
		if s.ParentID != 0 || s.ParentContext != nil {
			t.Fatalf("submit parent = %d %v, want a root", s.ParentID, s.ParentContext)
		}
		equalValues(t, "submit links", s.Links, []telemetry.RecordedLink{{Context: caller.Context, Attributes: telemetry.SpanAttributes{}}})
		found := false
		for _, phase := range spansNamed(recorder, SpanPhase) {
			if phase.Attributes["phase"] == "prepare" && reflect.DeepEqual(phase.ParentContext, s.Context) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no prepare phase under submit %v", s.Context)
		}
	}
	if reflect.DeepEqual(submits[0].Context, submits[1].Context) {
		t.Fatal("both runs share one trace")
	}
}

func TestTelemetryLinksTheOtherQueuedInputsToTheFirstInvocationOfTheirSharedRun(t *testing.T) {
	setup, recorder := trackedSetup(t)
	setup.setSettings(func(s *HarnessSettings) { s.FollowUpMode = QueueAll })
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("shared")})
	h, root := openChat(t, memory.New(), setup)
	one := submit(t, root, inputDraft("first"))
	awaitSignal(t, reached, "the first request")
	two := submit(t, root, inputDraft("second"))
	three := submit(t, root, inputDraft("third"))
	secondTrace, thirdTrace := submissionStatus(t, two).Trace, submissionStatus(t, three).Trace
	release()
	for _, s := range []*Submission{one, two, three} {
		waitFor(t, s)
	}
	must(t, h.WaitForIdle(bg))
	allSettled(t, recorder)
	var tasks durable.Page[durable.TaskRecord]
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		tasks, err = tx.ScanTasks(durable.TaskQuery{Kind: GenerationTask.Name()}, 10, nil)
		return err
	}))
	if len(tasks.Items) < 2 {
		t.Fatalf("generations = %d, want 2", len(tasks.Items))
	}
	shared := tasks.Items[1]
	equalValues(t, "shared run trace", shared.Trace, secondTrace)
	equalValues(t, "shared run links", shared.Links, []telemetry.Carrier{thirdTrace})
	var phases []any
	for _, span := range spansNamed(recorder, SpanPhase) {
		if span.Attributes["task.id"] != int64(shared.ID) {
			continue
		}
		phases = append(phases, span.Attributes["phase"])
		equalValues(t, "phase links", span.Links, []telemetry.RecordedLink{{Context: thirdTrace, Attributes: telemetry.SpanAttributes{}}})
	}
	equalValues(t, "shared run phases", phases, []any{"prepare", "request"})
}

func TestTelemetryKeepsAPublicWriteAdmissionOpenThroughCommitAsAChildOfItsCaller(t *testing.T) {
	setup, recorder := trackedSetup(t)
	storage := newControlledStorage()
	_, root := openChat(t, storage, setup)
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(caller telemetry.Span) error {
		held := storage.holdCommits()
		writing := async(func() (*Submission, error) {
			return root.Submit(telemetry.WithSpan(bg, caller), writeDraft("app.note", `"private-note"`))
		})
		held.Entered(t)
		inFlight := spansNamed(recorder, SpanSubmit)[0]
		if inFlight.Settled || len(inFlight.Events) != 0 {
			t.Fatalf("in-flight submit = %+v, want open without events", inFlight)
		}
		storage.releaseHold(held)
		submission, err := await(t, writing)
		must(t, err)
		record := submissionStatus(t, submission)
		span := spansNamed(recorder, SpanSubmit)[0]
		if !span.Settled || !reflect.DeepEqual(span.ParentContext, recorder.Spans()[0].Context) || span.Attributes["result"] != "done" {
			t.Fatalf("submit = %+v, want a settled child of the caller with result done", span)
		}
		equalValues(t, "submission trace", record.Trace, span.Context)
		var names []string
		for _, event := range span.Events {
			names = append(names, event.Name)
		}
		equalValues(t, "events", names, []string{EventEntry, EventSubmission})
		return nil
	}))
}

func TestTelemetryMarksOnlyTheFirstInvocationAfterReopenResumedRetainingItsStoredParent(t *testing.T) {
	setup, recorder := trackedSetup(t)
	storage := newControlledStorage()
	var recover atomic.Bool
	reached := make(chan struct{})
	var reachedOnce sync.Once
	task, err := DefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.resume",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "hold"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"hold": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				if !recover.Load() {
					reachedOnce.Do(func() { close(reached) })
					<-ctx.Done()
				}
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Next(stepState{Phase: "finish"})
				})
			},
			"finish": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
			return rt.Commit(ctx, abortWith("test"))
		},
	})
	must(t, err)
	installTask(t, setup, task)
	h, root := openChat(t, storage, setup)
	var id durable.TaskID
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "creator"}, func(span telemetry.Span) error {
		return root.Commit(telemetry.WithSpan(bg, span), func(tx *durable.Tx) error {
			var err error
			id, err = CreateTask[any](tx, task, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
			return err
		})
	}))
	must(t, h.Resume())
	awaitSignal(t, reached, "the hold phase")
	before := getTask(t, h, id)
	must(t, h.Close(bg))
	recover.Store(true)
	reopened := telemetry.NewInMemoryContext()
	setup.telemetry = reopened
	h, _ = openChat(t, storage.reopen(), setup)
	_, err = h.WaitForTask(bg, id)
	must(t, err)
	allSettled(t, reopened)
	phases := append(spansNamed(recorder, SpanPhase), spansNamed(reopened, SpanPhase)...)
	var resumed []any
	for _, phase := range phases {
		resumed = append(resumed, phase.Attributes["resumed"])
		equalValues(t, "phase parent", phase.ParentContext, before.Trace)
	}
	equalValues(t, "resumed", resumed, []any{false, true, false})
	if phases[0].Attributes["outcome"] != "aborted" || phases[0].Status != (telemetry.SpanStatus{Code: telemetry.StatusOK}) {
		t.Fatalf("interrupted phase = %+v, want aborted with status ok", phases[0])
	}
	must(t, h.Close(bg))
	var active []float64
	for _, m := range append(measurementsNamed(recorder, MetricTasksActive), measurementsNamed(reopened, MetricTasksActive)...) {
		if m.Attributes["task"] == "test.resume" {
			active = append(active, m.Value)
		}
	}
	equalValues(t, "active tasks", active, []float64{1, -1, 1, -1})
}

func TestTelemetryEmitsEntryAndSettlementEventsOnlyAfterSuccessfulStorageCommits(t *testing.T) {
	setup, recorder := trackedSetup(t)
	storage := newControlledStorage()
	_, root := openChat(t, storage, setup)
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
		ctx := telemetry.WithSpan(bg, span)
		rollback := errors.New("rollback")
		if err := root.Commit(ctx, func(tx *durable.Tx) error {
			if _, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "app.rolled-back", Data: json.RawMessage(`"private-rolled-back"`)}); err != nil {
				return err
			}
			return rollback
		}); !errors.Is(err, rollback) {
			t.Fatalf("err = %v, want the rollback", err)
		}
		if events := recorder.Spans()[0].Events; len(events) != 0 {
			t.Fatalf("events after a rollback = %v", events)
		}
		held := storage.holdCommits()
		var entry durable.EntryRecord
		writing := async(func() (struct{}, error) {
			return struct{}{}, root.Commit(ctx, func(tx *durable.Tx) error {
				var err error
				entry, err = tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "app.note", Data: json.RawMessage(`"private-content"`)})
				return err
			})
		})
		held.Entered(t)
		if events := recorder.Spans()[0].Events; len(events) != 0 {
			t.Fatalf("events while held = %v", events)
		}
		storage.releaseHold(held)
		_, err := await(t, writing)
		must(t, err)
		var created durable.SubmissionRecord
		must(t, root.Commit(ctx, func(tx *durable.Tx) error {
			var err error
			created, err = tx.CreateSubmission(durable.SubmissionRecord{ConversationID: root.ID, Type: durable.SubmissionTypeWrite, Status: durable.SubmissionDone, Entry: &entry.ID})
			return err
		}))
		equalValues(t, "events", recorder.Spans()[0].Events, []telemetry.RecordedEvent{
			{Name: EventEntry, Attributes: telemetry.SpanAttributes{"entry.id": int64(entry.ID), "entry.kind": "app.note"}},
			{Name: EventSubmission, Attributes: telemetry.SpanAttributes{"submission.id": int64(created.ID), "status": durable.SubmissionDone}},
		})
		return nil
	}))
}

func TestTelemetryRecordsAFixedCategoryAndABoundedDiagnostic(t *testing.T) {
	for _, status := range []string{durable.OutcomeFaulted, durable.OutcomeFailed, durable.OutcomeOrphaned} {
		t.Run(status, func(t *testing.T) {
			setup, recorder := trackedSetup(t)
			message := strings.Repeat("diagnostic ", 80)
			task := oneStep(t, "test.fault", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				switch status {
				case durable.OutcomeFaulted:
					return errors.New(message)
				case durable.OutcomeOrphaned:
					return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
						return &durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: status, Reason: message}}, nil
					})
				}
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return &durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: status, Error: &durable.TaskOutcomeError{Message: message, Detail: json.RawMessage(`"private-error-detail"`)}}}, nil
				})
			}, nil)
			installTask(t, setup, task)
			h, root := openChat(t, memory.New(), setup)
			var id durable.TaskID
			must(t, root.Commit(bg, func(tx *durable.Tx) error {
				var err error
				id, err = CreateTask[any](tx, task, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}, Trace: telemetry.Carrier{"unknown": "carrier"}})
				return err
			}))
			record, err := h.WaitForTask(bg, id)
			must(t, err)
			if record.State.Outcome == nil || record.State.Outcome.Status != status {
				t.Fatalf("outcome = %+v, want %s", record.State.Outcome, status)
			}
			allSettled(t, recorder)
			span := spansNamed(recorder, SpanPhase)[0]
			if span.ParentID != 0 || span.Attributes["outcome"] != "faulted" {
				t.Fatalf("phase = %+v, want a root with outcome faulted", span)
			}
			equalValues(t, "status", span.Status, telemetry.SpanStatus{Code: telemetry.StatusError, Error: &telemetry.SpanError{Name: "task_" + status, Message: message[:500]}})
			if status == durable.OutcomeFaulted {
				want := telemetry.RecordedEvent{Name: EventReport, Attributes: telemetry.SpanAttributes{"type": "task_faulted", "reason": "threw", "extension": "task:test.fault", "task.id": int64(id)}}
				found := false
				for _, event := range span.Events {
					found = found || reflect.DeepEqual(event, want)
				}
				if !found {
					t.Fatalf("events = %v, want %v", span.Events, want)
				}
			}
			mustNotContain(t, "phase span", span, "private-error-detail")
			mustNotContain(t, "phase attributes and events", []any{span.Attributes, span.Events}, "diagnostic")
		})
	}
}

func TestTelemetryKeepsBothInterruptedRunsAndExplicitAbortHandlersAtStatusOK(t *testing.T) {
	setup, recorder := trackedSetup(t)
	reached := make(chan struct{})
	var once sync.Once
	task := oneStep(t, "test.abort", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		once.Do(func() { close(reached) })
		<-ctx.Done()
		return ctx.Err()
	}, func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, abortWith("private-abort-reason"))
	})
	installTask(t, setup, task)
	h, root := openChat(t, memory.New(), setup)
	id := start(t, root, task, false)
	must(t, h.Resume())
	awaitSignal(t, reached, "the hold phase")
	_, err := h.AbortTask(bg, id)
	must(t, err)
	_, err = h.WaitForTask(bg, id)
	must(t, err)
	allSettled(t, recorder)
	phases := spansNamed(recorder, SpanPhase)
	var kinds []any
	for _, span := range phases {
		kinds = append(kinds, span.Attributes["kind"])
		if span.Attributes["outcome"] != "aborted" || span.Status != (telemetry.SpanStatus{Code: telemetry.StatusOK}) {
			t.Fatalf("phase = %+v, want aborted with status ok", span)
		}
	}
	equalValues(t, "kinds", kinds, []any{"run", "abort"})
	mustNotContain(t, "phases", phases, "private-abort-reason")
}

func TestTelemetryMirrorsRuntimeReportsAsFixedFieldPhaseEventsAndTypeOnlyCounters(t *testing.T) {
	setup, recorder := trackedSetup(t)
	payload := errors.New("private-report-payload")
	task := oneStep(t, "test.report", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		must(t, rt.Report(payload))
		must(t, rt.Report(payload, ReportFrom{Extension: "reporting-extension"}))
		must(t, rt.Report(payload, ReportFrom{Callback: durable.CallbackModels}))
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	installTask(t, setup, task)
	h, root := openChat(t, memory.New(), setup)
	id := start(t, root, task, false)
	_, err := h.WaitForTask(bg, id)
	must(t, err)
	allSettled(t, recorder)
	span := spansNamed(recorder, SpanPhase)[0]
	equalValues(t, "events", span.Events, []telemetry.RecordedEvent{
		{Name: EventReport, Attributes: telemetry.SpanAttributes{"type": "task_error", "extension": "task:test.report", "task.id": int64(id)}},
		{Name: EventReport, Attributes: telemetry.SpanAttributes{"type": "extension_error", "extension": "reporting-extension", "task.id": int64(id)}},
		{Name: EventReport, Attributes: telemetry.SpanAttributes{"type": "host_error", "task.id": int64(id)}},
	})
	equalValues(t, "reports", setup.reports.all(), []error{payload, payload, payload})
	if len(spansNamed(recorder, EventReport)) != 0 {
		t.Fatal("a report started a span")
	}
	var counted []telemetry.SpanAttributes
	for _, m := range measurementsNamed(recorder, MetricReports) {
		if m.Value != 1 {
			t.Fatalf("report count = %v, want 1", m.Value)
		}
		counted = append(counted, m.Attributes)
	}
	equalValues(t, "counted", counted, []telemetry.SpanAttributes{{"type": "task_error"}, {"type": "extension_error"}, {"type": "host_error"}})
	mustNotContain(t, "spans", recorder.Spans(), "private-report-payload")
}

func TestTelemetryReportsHookFailuresOnThePhaseAndClockFailuresWithoutAPhaseContext(t *testing.T) {
	setup, recorder := trackedSetup(t)
	hookErr := errors.New("private-hook-error")
	must(t, setup.registry.Install(&Extension{Name: "reporting-hooks", Hooks: []HookRegistration{Hook(GenerationTask, &GenerationHooks{
		BeforeRequest: func(context.Context, []ai.Message, *TaskRuntime) ([]ai.Message, error) { return nil, hookErr },
	})}}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("answer")})
	h, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("input")))
	must(t, h.WaitForIdle(bg))
	allSettled(t, recorder)
	var request telemetry.RecordedSpan
	for _, span := range spansNamed(recorder, SpanPhase) {
		if span.Attributes["phase"] == "request" {
			request = span
		}
	}
	want := telemetry.RecordedEvent{Name: EventReport, Attributes: telemetry.SpanAttributes{"type": "extension_error", "extension": "reporting-hooks", "task.id": request.Attributes["task.id"]}}
	found := false
	for _, event := range request.Events {
		found = found || reflect.DeepEqual(event, want)
	}
	if !found {
		t.Fatalf("request events = %v, want %v", request.Events, want)
	}

	clockErr := errors.New("private-clock-error")
	task := oneStep(t, "test.clock", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		setup.setNow(func() time.Time { panic(clockErr) })
		_, _ = rt.Now()
		setup.setNow(time.Now)
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	installTask(t, setup, task)
	id := start(t, root, task, false)
	_, err := h.WaitForTask(bg, id)
	must(t, err)
	allSettled(t, recorder)
	for _, span := range spansNamed(recorder, SpanPhase) {
		if span.Attributes["task.name"] == "test.clock" && len(span.Events) != 0 {
			t.Fatalf("clock phase events = %v, want none", span.Events)
		}
	}
	equalValues(t, "counted", reportTypes(recorder), []telemetry.SpanAttributes{{"type": "extension_error"}, {"type": "host_error"}})
	equalValues(t, "reports", setup.reports.all(), []error{hookErr, clockErr})
	mustNotContain(t, "spans", recorder.Spans(), "private-hook-error", "private-clock-error")
}

func TestTelemetryCountsAnOutsideSpanSessionFailureWithoutStartingASpan(t *testing.T) {
	setup, recorder := trackedSetup(t)
	storage := newControlledStorage()
	h, root := openChat(t, storage, setup)
	failure := errors.New("private-storage-error")
	storage.failNextCommit(failure)
	if err := root.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "app.failed"})
		return err
	}); !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the storage error", err)
	}
	awaitSignal(t, h.Closed(), "the Session close")
	equalValues(t, "reports", setup.reports.all(), []error{failure})
	if spans := recorder.Spans(); len(spans) != 0 {
		t.Fatalf("spans = %v, want none", spans)
	}
	mustNotContain(t, "measurements", recorder.Measurements(), "private-storage-error")
	equalValues(t, "counted", reportTypes(recorder), []telemetry.SpanAttributes{{"type": "session_failed"}})
}

func TestTelemetryCountsReportsRaisedWithANoopSpanWithoutCreatingARoot(t *testing.T) {
	setup, recorder := trackedSetup(t)
	h, root := openChat(t, memory.New(), setup)
	failure := errors.New("private-noop-report")
	unsubscribe, err := h.SubscribeCommits(func(durable.CommitPublication, context.Context) { panic(failure) })
	must(t, err)
	defer unsubscribe()
	appendPlainNote(t, root)
	equalValues(t, "reports", setup.reports.all(), []error{failure})
	if spans := recorder.Spans(); len(spans) != 0 {
		t.Fatalf("spans = %v, want none", spans)
	}
	equalValues(t, "counted", reportTypes(recorder), []telemetry.SpanAttributes{{"type": "listener_error"}})
}

func TestTelemetryAttachesSynchronousCommitListenerReportsToTheActiveSubmissionSpan(t *testing.T) {
	setup, recorder := trackedSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("answer")})
	h, root := openChat(t, memory.New(), setup)
	failure := errors.New("private-listener-error")
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if entry, ok := change.(durable.EntryWrite); ok && entry.Value.Kind == durable.UserEntry.Kind {
				panic(failure)
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	waitFor(t, submit(t, root, inputDraft("input")))
	must(t, h.WaitForIdle(bg))
	allSettled(t, recorder)
	equalValues(t, "reports", setup.reports.all(), []error{failure})
	span := spansNamed(recorder, SpanSubmit)[0]
	want := telemetry.RecordedEvent{Name: EventReport, Attributes: telemetry.SpanAttributes{"type": "listener_error"}}
	found := false
	for _, event := range span.Events {
		found = found || reflect.DeepEqual(event, want)
	}
	if !found {
		t.Fatalf("submit events = %v, want %v", span.Events, want)
	}
	mustNotContain(t, "spans", recorder.Spans(), "private-listener-error")
}

// Beyond pi: the stored trace and links of tasks and submissions survive a
// serializing Storage (JSONL), as pi's records keep them in their JSON.
func TestTelemetryTracesSurviveAReopenedJSONLStorage(t *testing.T) {
	directory := t.TempDir()
	setup, recorder := trackedSetup(t)
	task := oneStep(t, "test.stored", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	installTask(t, setup, task)
	h, root := openChat(t, openJSONL(t, directory), setup)
	trace := telemetry.Carrier{"pi-memory-scope": "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f", "pi-memory-span": "7"}
	link := telemetry.Carrier{"pi-memory-scope": "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f", "pi-memory-span": "8"}
	var id durable.TaskID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask[any](tx, task, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}, Trace: trace, Links: []telemetry.Carrier{link}})
		return err
	}))
	// A write's span is a child of its caller's: without one it is no-op.
	var write *Submission
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
		var err error
		write, err = root.Submit(telemetry.WithSpan(bg, span), writeDraft("app.note", ""))
		return err
	}))
	submitted := submissionStatus(t, write).Trace
	if submitted == nil {
		t.Fatal("the write stored no trace")
	}
	must(t, h.Close(bg))

	h, _ = openChat(t, openJSONL(t, directory), setup)
	record := getTask(t, h, id)
	equalValues(t, "task trace", record.Trace, trace)
	equalValues(t, "task links", record.Links, []telemetry.Carrier{link})
	reopened, err := h.Submission(bg, write.ID)
	must(t, err)
	equalValues(t, "submission trace", submissionStatus(t, reopened).Trace, submitted)
}

// The cases below are beyond pi: each covers a mechanism no case of pi's
// checks (sabotage of the durable telemetry slice).

func generationRecords(t *testing.T, h *Harness) []durable.TaskRecord {
	t.Helper()
	var tasks durable.Page[durable.TaskRecord]
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		tasks, err = tx.ScanTasks(durable.TaskQuery{Kind: GenerationTask.Name()}, 10, nil)
		return err
	}))
	return tasks.Items
}

// A run whose first input has no trace (an internal admission, as pi's
// compaction makes) starts at the adapter root, not under the span of the
// commit that places it — here the answering phase of the run before.
func TestTelemetryStartsARunAtTheRootWhenItsFirstInputHasNoTrace(t *testing.T) {
	setup, recorder := trackedSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	h, root := openChat(t, memory.New(), setup)
	one := submit(t, root, inputDraft("first"))
	awaitSignal(t, reached, "the first request")
	var internal durable.SubmissionID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		admitted, err := admitSubmission(tx, root.ID, inputDraft("internal"), 0, h.settings(), nil)
		internal = admitted.id
		return err
	}))
	release()
	waitFor(t, one)
	queued, err := h.Submission(bg, internal)
	must(t, err)
	waitFor(t, queued)
	must(t, h.WaitForIdle(bg))
	allSettled(t, recorder)
	generations := generationRecords(t, h)
	if len(generations) != 2 {
		t.Fatalf("generations = %d, want 2", len(generations))
	}
	if trace := generations[1].Trace; trace != nil {
		t.Fatalf("the internal input's run stored trace %v, want none", trace)
	}
	for _, span := range spansNamed(recorder, SpanPhase) {
		if span.Attributes["task.id"] == int64(generations[1].ID) && span.ParentContext != nil {
			t.Fatalf("phase %v of the internal run has parent %v, want a root", span.Attributes["phase"], span.ParentContext)
		}
	}
}

// An idle admission behind queued items runs a boundary: the input is
// "placed" when the boundary selects it last, otherwise "queued".
func TestTelemetryReportsPlacedOrQueuedForAnAdmissionBehindABoundary(t *testing.T) {
	for _, mode := range []QueueMode{QueueOneAtATime, QueueAll} {
		t.Run(string(mode), func(t *testing.T) {
			setup, recorder := trackedSetup(t)
			setup.setSettings(func(s *HarnessSettings) { s.FollowUpMode = mode })
			failing, reached, release := gatedStep(fauxError("invalid request"))
			setup.faux.SetResponses([]providers.FauxResponseStep{failing, fauxText("for f"), fauxText("for g")})
			h, root := openChat(t, memory.New(), setup)
			input := submit(t, root, inputDraft("a"))
			awaitSignal(t, reached, "the failing request")
			f := submit(t, root, inputDraft("f"))
			release()
			waitFor(t, input)
			must(t, h.WaitForIdle(bg))
			g := submit(t, root, inputDraft("g"))
			want := "queued"
			if mode == QueueAll {
				want = "placed"
			}
			submits := spansNamed(recorder, SpanSubmit)
			if got := submits[len(submits)-1].Attributes["result"]; got != want {
				t.Fatalf("result of g = %v, want %s", got, want)
			}
			waitFor(t, f)
			waitFor(t, g)
		})
	}
}

// A repeated request ID reports its submission's status; unanswered is
// reported done, as a stale write is.
func TestTelemetryReportsADuplicatesUnansweredSubmissionDone(t *testing.T) {
	setup, recorder := trackedSetup(t)
	_, root := openChat(t, memory.New(), setup)
	old := appendPlainNote(t, root)
	reset(t, root, nil)
	draft := headWrite("summary", old.ID)
	draft.RequestID = "stale"
	// A write's span is a child of its caller's.
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
		ctx := telemetry.WithSpan(bg, span)
		first, err := root.Submit(ctx, draft)
		must(t, err)
		expectUnanswered(t, "stale", waitFor(t, first), "stale")
		_, err = root.Submit(ctx, draft)
		return err
	}))
	var results []any
	for _, span := range spansNamed(recorder, SpanSubmit) {
		results = append(results, span.Attributes["result"])
	}
	equalValues(t, "results", results, []any{"done", "done"})
}

// A queued submission is not settled: its admission commit emits no
// settlement event.
func TestTelemetryEmitsNoSettlementEventForAQueuedSubmission(t *testing.T) {
	setup, recorder := trackedSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	_, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
		_, err := root.Submit(telemetry.WithSpan(bg, span), queuedNote("w"))
		return err
	}))
	var write telemetry.RecordedSpan
	for _, span := range spansNamed(recorder, SpanSubmit) {
		if span.Attributes["submission.type"] == durable.SubmissionTypeWrite {
			write = span
		}
	}
	if write.Attributes["result"] != "queued" {
		t.Fatalf("write result = %v, want queued", write.Attributes["result"])
	}
	for _, event := range write.Events {
		if event.Name == EventSubmission {
			t.Fatalf("a queued write emitted %v", event)
		}
	}
	release()
	waitFor(t, input)
}

// A task's creation-time links belong to its first invocation: after a
// reopen, the next invocation's phases carry none.
func TestTelemetryLinksATasksCreationLinksToItsFirstInvocationOnly(t *testing.T) {
	setup, recorder := trackedSetup(t)
	storage := newControlledStorage()
	var recover atomic.Bool
	reached := make(chan struct{})
	var once sync.Once
	task := oneStep(t, "test.links", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		if !recover.Load() {
			once.Do(func() { close(reached) })
			<-ctx.Done()
			return ctx.Err()
		}
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	installTask(t, setup, task)
	h, root := openChat(t, storage, setup)
	var target telemetry.Carrier
	must(t, recorder.StartSpan(telemetry.SpanOptions{Name: "target"}, func(span telemetry.Span) error {
		target = recorder.Inject(span)
		return nil
	}))
	var id durable.TaskID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask[any](tx, task, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}, Links: []telemetry.Carrier{target}})
		return err
	}))
	must(t, h.Resume())
	awaitSignal(t, reached, "the first invocation")
	must(t, h.Close(bg))
	recover.Store(true)
	h, _ = openChat(t, storage.reopen(), setup)
	_, err := h.WaitForTask(bg, id)
	must(t, err)
	allSettled(t, recorder)
	var links [][]telemetry.RecordedLink
	for _, span := range spansNamed(recorder, SpanPhase) {
		links = append(links, span.Links)
	}
	equalValues(t, "phase links", links, [][]telemetry.RecordedLink{{{Context: target, Attributes: telemetry.SpanAttributes{}}}, {}})
}

// Every phase records one duration, with fixed fields only (pi checks this
// in its content-free case, which waits for the tool slice).
func TestTelemetryRecordsOneDurationPerPhaseWithoutIDs(t *testing.T) {
	setup, recorder := trackedSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("answer")})
	h, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("input")))
	must(t, h.WaitForIdle(bg))
	allSettled(t, recorder)
	phases := spansNamed(recorder, SpanPhase)
	durations := measurementsNamed(recorder, MetricPhaseDuration)
	if len(phases) == 0 || len(durations) != len(phases) {
		t.Fatalf("durations = %d, phases = %d", len(durations), len(phases))
	}
	for _, m := range recorder.Measurements() {
		for key := range m.Attributes {
			if strings.Contains(key, "id") {
				t.Fatalf("metric %s carries %s", m.Name, key)
			}
		}
	}
}

// TelemetrySchema marshals as pi's DURABLE_TELEMETRY_SCHEMA does (spot
// checks; the definition is data, so a key or value a schema reader needs is
// what may not drift).
func TestTelemetrySchemaDeclaresDurablesVocabulary(t *testing.T) {
	raw, err := json.Marshal(TelemetrySchema)
	must(t, err)
	for _, want := range []string{
		`"pi.durable.submit"`, `"pi.durable.phase"`, `"pi.durable.reports"`, `"pi.durable.phase.duration"`,
		`"pi.durable.tasks.active"`, `"kind":"upDownCounter"`, `"unit":"ms"`, `"cardinality":"high"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("schema lacks %s", want)
		}
	}
	if _, err := telemetry.NewTypedMetrics(telemetry.NoopMetrics, TelemetrySchema); err != nil {
		t.Fatal(err)
	}
}
