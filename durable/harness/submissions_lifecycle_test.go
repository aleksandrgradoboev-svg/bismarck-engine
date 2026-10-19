package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// The rest of pi's harness-submissions.test.ts: busy input, deduplication,
// abort results, waits and close, reopen, settlement, entry kinds.

func TestSubmissionsPlaceIdleInputAndRejectBusyInputWithWhenBusyRejectWithoutWriting(t *testing.T) {
	storage := newControlledStorage()
	setup := newChatSetup(t)
	setup.setNow(func() time.Time { return time.UnixMilli(42) })
	busy, reached := unanswered()
	setup.faux.SetResponses([]providers.FauxResponseStep{busy})
	h, root := openChat(t, storage, setup)
	submission, err := root.Submit(bg, inputDraft("hi"))
	must(t, err)
	awaitSignal(t, reached, "the request")
	record, err := submission.Status(bg)
	must(t, err)
	if record.Status != durable.SubmissionPlaced || record.Entry == nil {
		t.Fatalf("record = %+v, want placed", record)
	}
	var entry *durable.EntryRecord
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		entry, err = tx.Entry(*record.Entry)
		return err
	}))
	if entry == nil || !durable.UserEntry.Is(entry) {
		t.Fatalf("entry = %+v, want pi.user", entry)
	}
	model, err := json.Marshal(entry.Model)
	must(t, err)
	if string(model) != `[{"role":"user","content":"hi","timestamp":42}]` {
		t.Fatalf("model = %s", model)
	}
	run := liveRunOf(t, h, root.ID)
	if run == nil || !reflect.DeepEqual(run.Inputs, []durable.SubmissionID{submission.ID}) {
		t.Fatalf("run = %+v, want inputs [%d]", run, submission.ID)
	}
	task, err := h.GetTask(bg, run.TaskID)
	must(t, err)
	if task == nil || task.Kind != "pi.generation" {
		t.Fatalf("run task = %+v, want pi.generation", task)
	}

	commits := storage.commitCount()
	draft := inputDraft("again")
	draft.Input.WhenBusy = WhenBusyReject
	_, err = root.Submit(bg, draft)
	var busyErr *ConversationBusyError
	if !errors.As(err, &busyErr) || busyErr.ConversationID != root.ID {
		t.Fatalf("err = %v, want ConversationBusyError for %d", err, root.ID)
	}
	if got := storage.commitCount(); got != commits {
		t.Fatalf("commits = %d, want %d", got, commits)
	}
}

func TestSubmissionsDeduplicateRequestIDsPerConversationBeforeAnyWrite(t *testing.T) {
	storage := newControlledStorage()
	setup := newChatSetup(t)
	busy, reached := unanswered()
	setup.faux.SetResponses([]providers.FauxResponseStep{busy})
	h, root := openChat(t, storage, setup)
	draft := inputDraft("hi")
	draft.RequestID = "r1"
	first, err := root.Submit(bg, draft)
	must(t, err)
	awaitSignal(t, reached, "the request")
	commits := storage.commitCount()
	// Deduplication runs before the busy check.
	different := inputDraft("different")
	different.RequestID = "r1"
	again, err := root.Submit(bg, different)
	must(t, err)
	if again.ID != first.ID {
		t.Fatalf("again = %d, want %d", again.ID, first.ID)
	}
	if got := storage.commitCount(); got != commits {
		t.Fatalf("commits = %d, want %d", got, commits)
	}
	write := writeDraft("note", "")
	write.RequestID = "r1"
	if _, err := root.Submit(bg, write); err == nil || err.Error() != "Request r1 already identifies a submission of type input" {
		t.Fatalf("err = %v", err)
	}
	record, err := first.Status(bg)
	must(t, err)
	if record.RequestID != "r1" || record.Status != durable.SubmissionPlaced {
		t.Fatalf("first = %+v", record)
	}

	other, err := h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless}})
	must(t, err)
	written, err := other.Submit(bg, write)
	must(t, err)
	if written.ID == first.ID {
		t.Fatal("another conversation shares the request ID")
	}
	repeated, err := other.Submit(bg, write)
	must(t, err)
	if repeated.ID != written.ID {
		t.Fatalf("repeated = %d, want %d", repeated.ID, written.ID)
	}
}

func TestSubmissionsReportAbortResultsAndLookSubmissionsUpByConversation(t *testing.T) {
	setup := newChatSetup(t)
	release := make(chan struct{})
	setup.faux.SetResponses([]providers.FauxResponseStep{func(ctx context.Context, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		select {
		case <-release:
		case <-ctx.Done():
			return nil
		}
		return providers.FauxAssistantMessage(ai.ContentList{providers.FauxText("answer")}, ai.StopStop)
	}})
	h, root := openChat(t, newControlledStorage(), setup)
	submission, err := root.Submit(bg, inputDraft("hi"))
	must(t, err)
	expectAbort := func(got string, err error, want string) {
		t.Helper()
		must(t, err)
		if got != want {
			t.Fatalf("abort = %s, want %s", got, want)
		}
	}
	got, err := submission.Abort(bg)
	expectAbort(got, err, SubmissionAlreadyPlaced)
	got, err = h.AbortSubmission(bg, submission.ID, root.ID)
	expectAbort(got, err, SubmissionAlreadyPlaced)
	other, err := h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless}})
	must(t, err)
	got, err = h.AbortSubmission(bg, submission.ID, other.ID)
	expectAbort(got, err, SubmissionNotFound)
	got, err = h.AbortSubmission(bg, 999_999, 0)
	expectAbort(got, err, SubmissionNotFound)
	missing, err := h.Submission(bg, 999_999)
	must(t, err)
	if missing != nil {
		t.Fatalf("missing = %+v, want nil", missing)
	}

	close(release)
	_, err = submission.Wait(bg)
	must(t, err)
	got, err = submission.Abort(bg)
	expectAbort(got, err, SubmissionSettled)
	got, err = h.AbortSubmission(bg, submission.ID, 0)
	expectAbort(got, err, SubmissionSettled)
}

func TestSubmissionsCancelOnlyAWaitAndRejectPendingWaitsOnClose(t *testing.T) {
	setup := newChatSetup(t)
	busy, _ := unanswered()
	setup.faux.SetResponses([]providers.FauxResponseStep{busy})
	h, root := openChat(t, newControlledStorage(), setup)
	submission, err := root.Submit(bg, inputDraft("hi"))
	must(t, err)
	waitCtx, cancel := context.WithCancelCause(bg)
	defer cancel(nil)
	cancelled := make(chan error, 1)
	go func() { _, err := submission.Wait(waitCtx); cancelled <- err }()
	pending := make(chan error, 1)
	go func() { _, err := submission.Wait(bg); pending <- err }()
	// An explicit mark instead of pi's synchronous start: both waits are
	// registered.
	eventually(t, func() bool { return submissionWaiters(h, submission.ID) == 2 })
	cancel(errors.New("stop waiting"))
	if err := <-cancelled; err == nil || err.Error() != "stop waiting" {
		t.Fatalf("cancelled wait = %v, want stop waiting", err)
	}
	record, err := submission.Status(bg)
	must(t, err)
	if record.Status != durable.SubmissionPlaced {
		t.Fatalf("status = %s, want placed", record.Status)
	}
	must(t, h.Close(bg))
	if err := <-pending; err == nil || err.Error() != "Harness is closed" {
		t.Fatalf("pending wait = %v, want Harness is closed", err)
	}
}

// heldReads holds Storage.Submission reads while hold is set.
type heldReads struct {
	*controlledStorage
	hold    atomic.Bool
	entered *gate
}

func (s *heldReads) Submission(ctx context.Context, id durable.SubmissionID) (*durable.SubmissionRecord, error) {
	if s.hold.Load() {
		s.entered.enter()
	}
	return s.controlledStorage.Submission(ctx, id)
}

func TestSubmissionsRejectAWaitWhoseSubmissionReadSpansTheStartOfClose(t *testing.T) {
	storage := &heldReads{controlledStorage: newControlledStorage(), entered: newGate()}
	// A test that waits at a gate opens it on the way out.
	defer storage.entered.Release()
	setup := newChatSetup(t)
	busy, _ := unanswered()
	setup.faux.SetResponses([]providers.FauxResponseStep{busy})
	h, root := openChat(t, storage, setup)
	submission, err := root.Submit(bg, inputDraft("hi"))
	must(t, err)
	storage.hold.Store(true)
	waiting := make(chan error, 1)
	go func() { _, err := submission.Wait(bg); waiting <- err }()
	storage.entered.Entered(t)
	closing := make(chan error, 1)
	go func() { closing <- h.Close(bg) }()
	// An explicit mark instead of pi's synchronous close start: close has
	// begun.
	eventually(t, func() bool {
		h.submissions.mu.Lock()
		defer h.submissions.mu.Unlock()
		return h.submissions.closed
	})
	storage.entered.Release()
	if err := <-waiting; err == nil || err.Error() != "Harness is closed" {
		t.Fatalf("wait = %v, want Harness is closed", err)
	}
	must(t, <-closing)
}

// pi reopens SQLite; JSONL in a temporary directory serializes the same
// records (decision 17.10: close/reopen cases on JSONL).
func TestSubmissionsReacquireASubmissionAfterReopenAndSettleItDurably(t *testing.T) {
	directory := t.TempDir()
	setup := newChatSetup(t)
	// The first process never answers; the reopened one does.
	busy, reached := unanswered()
	setup.faux.SetResponses([]providers.FauxResponseStep{busy, fauxText("after reopen")})
	h, root := openChat(t, openJSONL(t, directory), setup)
	draft := inputDraft("hi")
	draft.RequestID = "print"
	submitted, err := root.Submit(bg, draft)
	must(t, err)
	id := submitted.ID
	awaitSignal(t, reached, "the request")
	must(t, h.Close(bg))

	h, _ = openChat(t, openJSONL(t, directory), setup)
	submission, err := h.Submission(bg, id)
	must(t, err)
	record, err := submission.Status(bg)
	must(t, err)
	if record.Status != durable.SubmissionPlaced {
		t.Fatalf("status after reopen = %s, want placed", record.Status)
	}
	must(t, h.Resume())
	settled, err := submission.Wait(bg)
	must(t, err)
	if settled.Status != durable.SubmissionDone || settled.Type != durable.SubmissionTypeInput {
		t.Fatalf("settled = %+v, want a done input", settled)
	}
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, directory), setup)
	submission, err = h.Submission(bg, id)
	must(t, err)
	again, err := submission.Wait(bg)
	must(t, err)
	if !reflect.DeepEqual(again, settled) {
		t.Fatalf("after second reopen = %+v, want %+v", again, settled)
	}
	repeated, err := root.Submit(bg, draft)
	must(t, err)
	if repeated.ID != id {
		t.Fatalf("repeated = %d, want %d", repeated.ID, id)
	}
}

func TestSubmissionsSettleSubmissionsByTheirCurrentRecordInTheTransaction(t *testing.T) {
	h, root := openChat(t, newControlledStorage(), newChatSetup(t))
	var entry durable.EntryID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		appended, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "note"})
		entry = appended.ID
		return err
	}))
	create := func(record durable.SubmissionRecord) durable.SubmissionID {
		t.Helper()
		var id durable.SubmissionID
		must(t, h.Commit(bg, func(tx *durable.Tx) error {
			created, err := tx.CreateSubmission(record)
			id = created.ID
			return err
		}))
		return id
	}
	queued := create(durable.SubmissionRecord{ConversationID: root.ID, Type: durable.SubmissionTypeInput, Status: durable.SubmissionQueued})
	write := create(durable.SubmissionRecord{ConversationID: root.ID, Type: durable.SubmissionTypeWrite, Status: durable.SubmissionQueued})
	answer := durable.SubmissionSettlement{Status: durable.SubmissionDone, Answer: entry}
	settle := func(id durable.SubmissionID, settlement durable.SubmissionSettlement) error {
		return root.Commit(bg, func(tx *durable.Tx) error { return tx.SettleSubmission(id, settlement) })
	}
	for _, id := range []durable.SubmissionID{queued, write} {
		if err := settle(id, answer); err == nil || !strings.Contains(err.Error(), "is not a placed input") {
			t.Fatalf("settling %d = %v, want not a placed input", id, err)
		}
	}
	if err := settle(999_999, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "x"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("settling a missing submission = %v", err)
	}

	// A submission created earlier in the same commit settles; a second
	// settlement leaves the first.
	var placed durable.SubmissionID
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		created, err := tx.CreateSubmission(durable.SubmissionRecord{ConversationID: root.ID, Type: durable.SubmissionTypeInput, Status: durable.SubmissionPlaced, Entry: &entry})
		if err != nil {
			return err
		}
		placed = created.ID
		if err := tx.SettleSubmission(placed, answer); err != nil {
			return err
		}
		return tx.SettleSubmission(placed, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "late"})
	}))
	submission, err := h.Submission(bg, placed)
	must(t, err)
	record, err := submission.Status(bg)
	must(t, err)
	if record.Status != durable.SubmissionDone || record.Entry == nil || *record.Entry != entry || record.Answer == nil || *record.Answer != entry {
		t.Fatalf("record = %+v, want done with entry and answer %d", record, entry)
	}
}

// pi's case types entry data through tokens; a Go EntryKind carries no data
// type (stage 1), so what applies is appending and reading by kind, Is, and
// the built-in kind names. Narrowing a read by kind (tx.entry(Marker, id) is
// undefined for a counter) is n/a.
func TestSubmissionsAppendAndReadEntriesThroughKinds(t *testing.T) {
	counter := durable.MustDefineEntry("app.counter")
	marker := durable.MustDefineEntry("app.marker")
	_, root := openChat(t, newControlledStorage(), newChatSetup(t))
	var appended, markerEntry durable.EntryRecord
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		appended, err = tx.AppendEntry(root.ID, durable.EntryDraft{Kind: counter.Kind, Data: json.RawMessage(`{"n":1}`)})
		return err
	}))
	var data struct{ N int }
	must(t, json.Unmarshal(appended.Data, &data))
	if data.N != 1 {
		t.Fatalf("n = %d, want 1", data.N)
	}
	want := durable.EntryRecord{ID: appended.ID, ConversationID: root.ID, Kind: "app.counter", Data: json.RawMessage(`{"n":1}`)}
	if !reflect.DeepEqual(appended, want) {
		t.Fatalf("counter = %+v, want %+v", appended, want)
	}
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		markerEntry, err = tx.AppendEntry(root.ID, durable.EntryDraft{Kind: marker.Kind})
		return err
	}))
	if markerEntry.Kind != "app.marker" {
		t.Fatalf("marker kind = %s", markerEntry.Kind)
	}
	var read, missing *durable.EntryRecord
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if read, err = tx.Entry(appended.ID); err != nil {
			return err
		}
		missing, err = tx.Entry(999_999)
		return err
	}))
	if read == nil || !reflect.DeepEqual(*read, appended) || missing != nil {
		t.Fatalf("read = %+v, missing = %+v", read, missing)
	}
	if !counter.Is(&appended) || durable.AssistantEntry.Is(&appended) || marker.Is(&appended) {
		t.Fatal("Is does not tell kinds apart")
	}
	if got := []string{durable.UserEntry.Kind, durable.AssistantEntry.Kind}; !reflect.DeepEqual(got, []string{"pi.user", "pi.assistant"}) {
		t.Fatalf("built-in kinds = %v", got)
	}
}

// submissionWaiters counts the registered waits for id.
func submissionWaiters(h *Harness, id durable.SubmissionID) int {
	h.submissions.mu.Lock()
	defer h.submissions.mu.Unlock()
	return len(h.submissions.waiters.sets[id])
}

// liveRunOf reads the committed pi.live run of a conversation.
func liveRunOf(t *testing.T, h *Harness, id durable.ConversationID) *LiveRun {
	t.Helper()
	value, err := h.Snapshot(bg, LiveDoc.Conversation(id))
	must(t, err)
	var live struct {
		Run *LiveRun `json:"run"`
	}
	must(t, fromValue(value, &live))
	return live.Run
}
