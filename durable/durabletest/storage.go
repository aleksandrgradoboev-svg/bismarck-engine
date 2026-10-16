// Package durabletest holds durable's conformance suites (pi
// packages/durable/src/testing): runner-independent checks a Storage
// implementation must pass. In Go the runner is the testing package, so a
// suite is a function that takes a *testing.T, as testing/fstest's are.
package durabletest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// TestStorage runs the storage conformance suite (pi
// testing/storage-conformance.ts). open returns a fresh, empty storage for
// each case; the suite closes nothing it did not open itself, so register
// cleanup in open.
func TestStorage(t *testing.T, open func(t *testing.T) durable.Storage) {
	TestStorageWith(t, StorageOptions{}, open)
}

// StorageOptions are the documented deviations of a backend from pi's
// contract that the suite checks instead of the pi behavior.
type StorageOptions struct {
	// RejectsInvalidUTF8 is a backend that rejects a commit holding a string
	// that is not valid UTF-8, with no durable effect, instead of keeping it
	// losslessly. pi's identities can be lone surrogates, which a JSON file
	// keeps as escapes; a Go string holds them only as invalid UTF-8, which
	// encoding/json replaces, so a JSON-backed Go storage would merge
	// distinct identities on reopen.
	RejectsInvalidUTF8 bool
}

// TestStorageWith runs the storage conformance suite with options.
func TestStorageWith(t *testing.T, options StorageOptions, open func(t *testing.T) durable.Storage) {
	for _, c := range storageCases {
		t.Run(c.name, func(t *testing.T) {
			c.run(&checker{t: t, s: open(t), options: options})
		})
	}
}

var ctx = context.Background()

type checker struct {
	t       *testing.T
	s       durable.Storage
	options StorageOptions
}

func (c *checker) fatalf(format string, args ...any) {
	c.t.Helper()
	c.t.Fatalf(format, args...)
}

func (c *checker) must(err error) {
	c.t.Helper()
	if err != nil {
		c.t.Fatal(err)
	}
}

func (c *checker) equal(got, want any, what string) {
	c.t.Helper()
	if !reflect.DeepEqual(got, want) {
		c.t.Fatalf("%s:\n got  %#v\n want %#v", what, got, want)
	}
}

func (c *checker) fails(err error, includes string) {
	c.t.Helper()
	if err == nil || !strings.Contains(err.Error(), includes) {
		c.t.Fatalf("error %v, want one containing %q", err, includes)
	}
}

func mint[T ~int64](c *checker) T {
	c.t.Helper()
	v, err := c.s.MintID(ctx)
	c.must(err)
	return T(v)
}

func (c *checker) commit(writes ...durable.StorageWrite) durable.Seq {
	c.t.Helper()
	seq, err := c.s.Commit(ctx, writes)
	c.must(err)
	return seq
}

func (c *checker) createRoot() durable.ConversationID {
	c.t.Helper()
	c.commit(durable.ConversationWrite{Value: durable.ConversationRecord{ID: durable.RootConversationID}})
	return durable.RootConversationID
}

func raw(format string, args ...any) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(format, args...))
}

func pendingTask(id durable.TaskID, cid durable.ConversationID) durable.TaskRecord {
	return durable.TaskRecord{
		ID: id, ConversationID: cid, Kind: "test.task", Version: 1,
		Input:      raw(`{"value":%d}`, id),
		State:      durable.TaskState{Status: durable.TaskPending, Checkpoint: raw(`{"phase":"ready"}`)},
		Background: false, AbortRequested: false,
	}
}

func entry(id durable.EntryID, cid durable.ConversationID, kind string) durable.EntryRecord {
	if kind == "" {
		kind = "message"
	}
	return durable.EntryRecord{ID: id, ConversationID: cid, Kind: kind}
}

func p[T any](v T) *T { return &v }

func entryW(e durable.EntryRecord) durable.StorageWrite    { return durable.EntryWrite{Value: e} }
func taskW(r durable.TaskRecord) durable.StorageWrite      { return durable.TaskWrite{Value: r} }
func subW(r durable.SubmissionRecord) durable.StorageWrite { return durable.SubmissionWrite{Value: r} }
func convW(r durable.ConversationRecord) durable.StorageWrite {
	return durable.ConversationWrite{Value: r}
}
func base(version int, value map[string]any) durable.DocumentContent {
	return durable.DocumentContent{Kind: durable.ContentBase, Version: version, Value: value}
}
func deltaContent(version int, ops ...delta.Op) durable.DocumentContent {
	return durable.DocumentContent{Kind: durable.ContentDelta, Version: version, Ops: ops}
}
func set(value any, path ...any) delta.Op { return delta.Set{Path: path_(path), Value: value} }
func path_(segs []any) delta.Path {
	out := delta.Path{}
	for _, s := range segs {
		switch v := s.(type) {
		case string:
			out = append(out, delta.Key(v))
		case int:
			out = append(out, delta.Index(v))
		}
	}
	return out
}

func ids[T any, I ~int64](items []T, idOf func(T) I) []I {
	out := []I{}
	for _, item := range items {
		out = append(out, idOf(item))
	}
	return out
}

func entryIDs(page durable.Page[durable.EntryRecord]) []durable.EntryID {
	return ids(page.Items, func(e durable.EntryRecord) durable.EntryID { return e.ID })
}

func conversationIDs(page durable.Page[durable.ConversationRecord]) []durable.ConversationID {
	return ids(page.Items, func(r durable.ConversationRecord) durable.ConversationID { return r.ID })
}

func taskIDs(page durable.Page[durable.TaskRecord]) []durable.TaskID {
	return ids(page.Items, func(r durable.TaskRecord) durable.TaskID { return r.ID })
}

func documentIDs(page durable.Page[durable.DocumentRecord]) []durable.DocumentID {
	return ids(page.Items, func(r durable.DocumentRecord) durable.DocumentID { return r.ID })
}

// roundTrip passes a cursor through JSON, as a client that stores it does.
func roundTrip(c *checker, cursor durable.Cursor) durable.Cursor {
	c.t.Helper()
	if cursor == nil {
		return nil
	}
	data, err := json.Marshal(cursor)
	c.must(err)
	var out durable.Cursor
	c.must(json.Unmarshal(data, &out))
	return out
}

func (c *checker) document(id durable.DocumentID, at durable.DocumentPoint) *durable.StoredDocument {
	c.t.Helper()
	d, err := c.s.Document(ctx, id, at)
	c.must(err)
	return d
}

func (c *checker) documentMatches(id durable.DocumentID, at durable.DocumentPoint, version int, value map[string]any, deltas int) {
	c.t.Helper()
	d := c.document(id, at)
	if d == nil {
		c.fatalf("document %d at %d is absent", id, at)
	}
	if version >= 0 && d.Version != version {
		c.fatalf("document %d at %d: version %d, want %d", id, at, d.Version, version)
	}
	c.equal(d.Value, value, fmt.Sprintf("document %d at %d", id, at))
	if deltas >= 0 && d.DeltasSinceBase != deltas {
		c.fatalf("document %d at %d: %d deltas since base, want %d", id, at, d.DeltasSinceBase, deltas)
	}
}

type obj = map[string]any
type arr = []any

type storageCase struct {
	name string
	run  func(c *checker)
}

var storageCases = []storageCase{
	{"reserves ID 1 for the immutable root conversation", func(c *checker) {
		if v := mint[durable.ConversationID](c); v != 2 {
			c.fatalf("first minted ID %d, want 2", v)
		}
		root := c.createRoot()
		got, err := c.s.Conversation(ctx, root)
		c.must(err)
		c.equal(got, &durable.ConversationRecord{ID: root}, "root")
		_, err = c.s.Commit(ctx, []durable.StorageWrite{convW(durable.ConversationRecord{ID: root})})
		c.fails(err, fmt.Sprintf("ID %d already belongs to conversation", root))
	}},

	{"commits mixed table writes atomically and rolls all of them back on failure", func(c *checker) {
		root := c.createRoot()
		entryID := mint[durable.EntryID](c)
		taskID := mint[durable.TaskID](c)
		subID := mint[durable.SubmissionID](c)
		task := pendingTask(taskID, root)
		input := durable.SubmissionRecord{ID: subID, ConversationID: root, RequestID: "request-1", Type: durable.SubmissionTypeInput, Status: durable.SubmissionPlaced, Entry: &entryID}
		hello := entry(entryID, root, "user")
		hello.Data = raw(`{"text":"hello"}`)
		initialSeq := c.commit(entryW(hello), taskW(task), subW(input))

		e, err := c.s.Entry(ctx, entryID)
		c.must(err)
		c.equal(e, &durable.StoredEntry{Entry: hello, CommitSeq: initialSeq}, "entry")
		gotTask, err := c.s.Task(ctx, taskID)
		c.must(err)
		c.equal(gotTask, &task, "task")
		gotSub, err := c.s.Submission(ctx, subID)
		c.must(err)
		c.equal(gotSub, &input, "submission")

		transient := mint[durable.EntryID](c)
		running := task
		running.State = durable.TaskState{Status: durable.TaskRunning, Checkpoint: raw(`{"phase":"effect"}`)}
		done := input
		done.Status, done.Answer = durable.SubmissionDone, &transient
		_, err = c.s.Commit(ctx, []durable.StorageWrite{taskW(running), subW(done), entryW(entry(transient, root, "assistant")), convW(durable.ConversationRecord{ID: root})})
		c.fails(err, fmt.Sprintf("ID %d already belongs to conversation", root))

		gotTask, _ = c.s.Task(ctx, taskID)
		c.equal(gotTask, &task, "task after rollback")
		gotSub, _ = c.s.Submission(ctx, subID)
		c.equal(gotSub, &input, "submission after rollback")
		gone, _ := c.s.Entry(ctx, transient)
		c.equal(gone, (*durable.StoredEntry)(nil), "transient entry")
		after := c.commit(entryW(entry(mint[durable.EntryID](c), root, "after-rollback")))
		if after <= initialSeq {
			c.fatalf("sequence %d after a rollback, want more than %d", after, initialSeq)
		}
	}},

	{"detaches retained writes and every returned record", func(c *checker) {
		root := c.createRoot()
		entryID := mint[durable.EntryID](c)
		taskID := mint[durable.TaskID](c)
		subID := mint[durable.SubmissionID](c)
		entryData := raw(`{"nested":[1,2]}`)
		checkpoint := raw(`{"phase":"ready","nested":{"count":1}}`)
		detail := raw(`{"codes":["initial"]}`)
		stored := entry(entryID, root, "note")
		stored.Data = entryData
		task := pendingTask(taskID, root)
		task.State = durable.TaskState{Status: durable.TaskPending, Checkpoint: checkpoint}
		sub := durable.SubmissionRecord{ID: subID, ConversationID: root, Type: durable.SubmissionTypeInput, Status: durable.SubmissionUnanswered, Reason: "failed", Detail: detail}
		c.commit(entryW(stored), taskW(task), subW(sub))

		// Change the caller's bytes after the commit.
		copy(entryData, `{"nested":[9,9]}`)
		copy(checkpoint, `{"phase":"ready","nested":{"count":9}}`)
		copy(detail, `{"codes":["mutated"]}`)
		check := func(when string) {
			c.t.Helper()
			e, _ := c.s.Entry(ctx, entryID)
			c.equal(string(e.Entry.Data), `{"nested":[1,2]}`, "entry data "+when)
			tk, _ := c.s.Task(ctx, taskID)
			c.equal(string(tk.State.Checkpoint), `{"phase":"ready","nested":{"count":1}}`, "checkpoint "+when)
			s, _ := c.s.Submission(ctx, subID)
			c.equal(string(s.Detail), `{"codes":["initial"]}`, "detail "+when)
		}
		check("after the caller's change")

		// Change what reads return.
		e, _ := c.s.Entry(ctx, entryID)
		copy(e.Entry.Data, `{"nested":[7,7]}`)
		tk, _ := c.s.Task(ctx, taskID)
		copy(tk.State.Checkpoint, `{"phase":"ready","nested":{"count":7}}`)
		s, _ := c.s.Submission(ctx, subID)
		copy(s.Detail, `{"codes":["readmut"]}`)
		check("after a reader's change")
	}},

	{"keeps prototype-like JSON keys as data", func(c *checker) {
		root := c.createRoot()
		entryID := mint[durable.EntryID](c)
		e := entry(entryID, root, "note")
		e.Data = raw(`{"__proto__":{"polluted":false},"constructor":{"label":"stored"},"toString":"value"}`)
		c.commit(entryW(e))
		got, _ := c.s.Entry(ctx, entryID)
		var data map[string]any
		c.must(json.Unmarshal(got.Entry.Data, &data))
		c.equal(data, obj{"__proto__": obj{"polluted": false}, "constructor": obj{"label": "stored"}, "toString": "value"}, "data")
	}},

	{"indexes entries committed out of ID order", func(c *checker) {
		root := c.createRoot()
		marker := entry(20, root, "marker")
		marker.Head = p[durable.EntryID](10)
		c.commit(entryW(entry(30, root, "")), entryW(entry(10, root, "")), entryW(marker))
		page, err := c.s.ScanEntries(ctx, durable.EntryQuery{ConversationID: root}, 10, nil)
		c.must(err)
		c.equal(entryIDs(page), []durable.EntryID{30, 20, 10}, "scan")
		m, err := c.s.FindLatestHeadMarker(ctx, root, nil)
		c.must(err)
		c.equal(m.ID, durable.EntryID(20), "marker")
	}},

	{"continues an entry cursor below its last item after a newer commit", func(c *checker) {
		root := c.createRoot()
		oldest, middle, newest := mint[durable.EntryID](c), mint[durable.EntryID](c), mint[durable.EntryID](c)
		c.commit(entryW(entry(oldest, root, "")), entryW(entry(middle, root, "")), entryW(entry(newest, root, "")))
		first, err := c.s.ScanEntries(ctx, durable.EntryQuery{ConversationID: root}, 2, nil)
		c.must(err)
		c.equal(entryIDs(first), []durable.EntryID{newest, middle}, "first page")
		c.commit(entryW(entry(mint[durable.EntryID](c), root, "")))
		second, err := c.s.ScanEntries(ctx, durable.EntryQuery{ConversationID: root}, 2, first.Next)
		c.must(err)
		c.equal(entryIDs(second), []durable.EntryID{oldest}, "second page")
		c.equal(second.Next, durable.Cursor(nil), "second next")
	}},

	{"paginates conversations by opaque cursor in ascending ID order", func(c *checker) {
		root := c.createRoot()
		second, third := mint[durable.ConversationID](c), mint[durable.ConversationID](c)
		c.commit(convW(durable.ConversationRecord{ID: third}), convW(durable.ConversationRecord{ID: second}))
		first, err := c.s.ScanConversations(ctx, durable.ConversationQuery{}, 2, nil)
		c.must(err)
		c.equal(conversationIDs(first), []durable.ConversationID{root, second}, "first page")
		if first.Next == nil {
			c.fatalf("no cursor")
		}
		next, err := c.s.ScanConversations(ctx, durable.ConversationQuery{}, 2, roundTrip(c, first.Next))
		c.must(err)
		c.equal(conversationIDs(next), []durable.ConversationID{third}, "second page")
		c.equal(next.Next, durable.Cursor(nil), "second next")
	}},

	{"filters and pages conversations by durable owner edges", func(c *checker) {
		root := c.createRoot()
		otherOwner := mint[durable.ConversationID](c)
		firstTask, secondTask := mint[durable.TaskID](c), mint[durable.TaskID](c)
		first, second, third := mint[durable.ConversationID](c), mint[durable.ConversationID](c), mint[durable.ConversationID](c)
		c.commit(
			convW(durable.ConversationRecord{ID: otherOwner}),
			convW(durable.ConversationRecord{ID: first, Owner: &durable.ConversationOwner{ConversationID: root, TaskID: firstTask}}),
			convW(durable.ConversationRecord{ID: second, Owner: &durable.ConversationOwner{ConversationID: root, TaskID: secondTask}}),
			convW(durable.ConversationRecord{ID: third, Owner: &durable.ConversationOwner{ConversationID: otherOwner, TaskID: firstTask}}),
		)
		byRoot := durable.ConversationQuery{OwnerConversationID: &root}
		page1, err := c.s.ScanConversations(ctx, byRoot, 1, nil)
		c.must(err)
		c.equal(conversationIDs(page1), []durable.ConversationID{first}, "first page")
		page2, err := c.s.ScanConversations(ctx, byRoot, 1, page1.Next)
		c.must(err)
		c.equal(conversationIDs(page2), []durable.ConversationID{second}, "second page")
		c.equal(page2.Next, durable.Cursor(nil), "second next")
		byTask, err := c.s.ScanConversations(ctx, durable.ConversationQuery{OwnerTaskID: &firstTask}, 10, nil)
		c.must(err)
		c.equal(conversationIDs(byTask), []durable.ConversationID{first, third}, "by task")
		both, err := c.s.ScanConversations(ctx, durable.ConversationQuery{OwnerConversationID: &root, OwnerTaskID: &firstTask}, 10, nil)
		c.must(err)
		c.equal(conversationIDs(both), []durable.ConversationID{first}, "by both")
	}},

	{"scans deep fork history newest-first through every ancestor cap", testDeepForkHistory},

	{"scans tables in either ID order and continues a cursor in its order", func(c *checker) {
		root := c.createRoot()
		convs := []int64{int64(root)}
		var tasks, subs []int64
		for range 3 {
			cid, tid, sid := mint[durable.ConversationID](c), mint[durable.TaskID](c), mint[durable.SubmissionID](c)
			c.commit(convW(durable.ConversationRecord{ID: cid}), taskW(pendingTask(tid, root)),
				subW(durable.SubmissionRecord{ID: sid, ConversationID: root, Type: durable.SubmissionTypeInput, Status: durable.SubmissionQueued}))
			convs, tasks, subs = append(convs, int64(cid)), append(tasks, int64(tid)), append(subs, int64(sid))
		}
		type scan func(order durable.ScanOrder, cursor durable.Cursor) ([]int64, durable.Cursor, error)
		scans := []struct {
			scan scan
			ids  []int64
		}{
			{func(o durable.ScanOrder, cur durable.Cursor) ([]int64, durable.Cursor, error) {
				pg, err := c.s.ScanConversations(ctx, durable.ConversationQuery{Order: o}, 2, cur)
				return ids(pg.Items, func(r durable.ConversationRecord) int64 { return int64(r.ID) }), pg.Next, err
			}, convs},
			{func(o durable.ScanOrder, cur durable.Cursor) ([]int64, durable.Cursor, error) {
				pg, err := c.s.ScanTasks(ctx, durable.TaskQuery{Order: o}, 2, cur)
				return ids(pg.Items, func(r durable.TaskRecord) int64 { return int64(r.ID) }), pg.Next, err
			}, tasks},
			{func(o durable.ScanOrder, cur durable.Cursor) ([]int64, durable.Cursor, error) {
				pg, err := c.s.ScanSubmissions(ctx, durable.SubmissionQuery{Order: o}, 2, cur)
				return ids(pg.Items, func(r durable.SubmissionRecord) int64 { return int64(r.ID) }), pg.Next, err
			}, subs},
		}
		all := func(s scan, order durable.ScanOrder) []int64 {
			found, next, err := s(order, nil)
			c.must(err)
			for next != nil {
				// A cursor carries its order; the query may omit it.
				var more []int64
				more, next, err = s("", roundTrip(c, next))
				c.must(err)
				found = append(found, more...)
			}
			return found
		}
		for _, sc := range scans {
			reversed := slices.Clone(sc.ids)
			slices.Reverse(reversed)
			c.equal(all(sc.scan, ""), sc.ids, "default order")
			c.equal(all(sc.scan, durable.Ascending), sc.ids, "ascending")
			c.equal(all(sc.scan, durable.Descending), reversed, "descending")
			_, next, err := sc.scan(durable.Descending, nil)
			c.must(err)
			got, _, err := sc.scan(durable.Descending, next)
			c.must(err)
			c.equal(got, reversed[2:min(4, len(reversed))], "second descending page") // JS slice(2, 4) clamps
			_, _, err = sc.scan(durable.Ascending, next)
			c.fails(err, "cursor")
		}
	}},

	{"replaces complete task records and pages filtered task scans", func(c *checker) {
		root := c.createRoot()
		firstID, secondID, thirdID := mint[durable.TaskID](c), mint[durable.TaskID](c), mint[durable.TaskID](c)
		first := pendingTask(firstID, root)
		first.Memos = map[string]json.RawMessage{"winner": raw(`"first"`)}
		second := pendingTask(secondID, root)
		second.Background = true
		third := pendingTask(thirdID, root)
		third.AbortRequested = true
		c.commit(taskW(first), taskW(second), taskW(third))

		running := first
		running.State = durable.TaskState{Status: durable.TaskRunning, Checkpoint: raw(`{"phase":"effect","attempt":1}`)}
		running.AbortRequested = true
		c.commit(taskW(running))
		got, _ := c.s.Task(ctx, firstID)
		c.equal(got, &running, "running")
		terminal := durable.TaskRecord{
			ID: firstID, ConversationID: root, Kind: first.Kind, Version: first.Version, Input: first.Input,
			State:          durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: durable.OutcomeCompleted, Result: raw(`{"entryId":99}`)}},
			AbortRequested: true,
		}
		c.commit(taskW(terminal))
		got, _ = c.s.Task(ctx, firstID)
		c.equal(got, &terminal, "terminal")

		pending, err := c.s.ScanTasks(ctx, durable.TaskQuery{Status: durable.TaskPending}, 1, nil)
		c.must(err)
		c.equal(taskIDs(pending), []durable.TaskID{secondID}, "pending page")
		if pending.Next == nil {
			c.fatalf("no cursor")
		}
		more, err := c.s.ScanTasks(ctx, durable.TaskQuery{Status: durable.TaskPending}, 1, pending.Next)
		c.must(err)
		c.equal(taskIDs(more), []durable.TaskID{thirdID}, "next pending page")
		terminals, err := c.s.ScanTasks(ctx, durable.TaskQuery{Status: durable.TaskTerminal, AbortRequested: p(true)}, 10, nil)
		c.must(err)
		c.equal(terminals.Items, []durable.TaskRecord{terminal}, "terminal scan")
		background, err := c.s.ScanTasks(ctx, durable.TaskQuery{Background: p(true)}, 10, nil)
		c.must(err)
		c.equal(taskIDs(background), []durable.TaskID{secondID}, "background scan")
	}},

	{"stores owners and scans waiting and completing tasks by status", func(c *checker) {
		root := c.createRoot()
		ownerID, waitingID, completingID := mint[durable.TaskID](c), mint[durable.TaskID](c), mint[durable.TaskID](c)
		owner := pendingTask(ownerID, root)
		waiting := pendingTask(waitingID, root)
		waiting.Owner = &ownerID
		waiting.State = durable.TaskState{Status: durable.TaskWaiting, Checkpoint: raw(`{"phase":"next"}`), On: []durable.TaskID{ownerID}, Policy: durable.JoinAllSettled}
		waiting.Memos = map[string]json.RawMessage{"kept": raw(`true`)}
		completing := pendingTask(completingID, root)
		completing.Owner = &ownerID
		completing.State = durable.TaskState{Status: durable.TaskCompleting, Outcome: &durable.TaskOutcome{Status: durable.OutcomeFailed, Error: &durable.TaskOutcomeError{Message: "held"}}}
		c.commit(taskW(owner), taskW(waiting), taskW(completing))
		got, _ := c.s.Task(ctx, waitingID)
		c.equal(got, &waiting, "waiting")
		got, _ = c.s.Task(ctx, completingID)
		c.equal(got, &completing, "completing")
		scan := func(status string) []durable.TaskRecord {
			pg, err := c.s.ScanTasks(ctx, durable.TaskQuery{Status: status}, 10, nil)
			c.must(err)
			return pg.Items
		}
		c.equal(scan(durable.TaskWaiting), []durable.TaskRecord{waiting}, "waiting scan")
		c.equal(scan(durable.TaskCompleting), []durable.TaskRecord{completing}, "completing scan")
		c.equal(ids(scan(durable.TaskPending), func(r durable.TaskRecord) durable.TaskID { return r.ID }), []durable.TaskID{ownerID}, "pending scan")
		terminal := completing
		terminal.State = durable.TaskState{Status: durable.TaskTerminal, Outcome: completing.State.Outcome}
		c.commit(taskW(terminal))
		c.equal(len(scan(durable.TaskCompleting)), 0, "completing scan after terminal")
		c.equal(scan(durable.TaskTerminal), []durable.TaskRecord{terminal}, "terminal scan")
	}},

	{"indexes request IDs per conversation and replaces complete submission records", func(c *checker) {
		root := c.createRoot()
		secondConversation := mint[durable.ConversationID](c)
		c.commit(convW(durable.ConversationRecord{ID: secondConversation}))
		firstID, secondID, otherID := mint[durable.SubmissionID](c), mint[durable.SubmissionID](c), mint[durable.SubmissionID](c)
		queued := func(id durable.SubmissionID, cid durable.ConversationID, request string) durable.SubmissionRecord {
			return durable.SubmissionRecord{ID: id, ConversationID: cid, RequestID: request, Type: durable.SubmissionTypeInput, Status: durable.SubmissionQueued}
		}
		first, second, other := queued(firstID, root, "same"), queued(secondID, root, "other"), queued(otherID, secondConversation, "same")
		c.commit(subW(first), subW(second), subW(other))
		byRequest := func(cid durable.ConversationID, request string) *durable.SubmissionRecord {
			r, err := c.s.SubmissionByRequest(ctx, cid, request)
			c.must(err)
			return r
		}
		c.equal(byRequest(root, "same"), &first, "root same")
		c.equal(byRequest(secondConversation, "same"), &other, "second same")

		placed := second
		placed.Status, placed.Entry = durable.SubmissionPlaced, p(mint[durable.EntryID](c))
		c.commit(subW(placed))
		got, _ := c.s.Submission(ctx, secondID)
		c.equal(got, &placed, "placed")
		c.equal(byRequest(root, "other"), &placed, "placed by request")

		scanIDs := func(q durable.SubmissionQuery) []durable.SubmissionID {
			found := []durable.SubmissionID{}
			var cursor durable.Cursor
			for {
				pg, err := c.s.ScanSubmissions(ctx, q, 1, cursor)
				c.must(err)
				found = append(found, ids(pg.Items, func(r durable.SubmissionRecord) durable.SubmissionID { return r.ID })...)
				if cursor = pg.Next; cursor == nil {
					return found
				}
			}
		}
		c.equal(scanIDs(durable.SubmissionQuery{}), []durable.SubmissionID{firstID, secondID, otherID}, "all")
		c.equal(scanIDs(durable.SubmissionQuery{ConversationID: &root}), []durable.SubmissionID{firstID, secondID}, "root")
		// A status change moves the record between status scans.
		c.equal(scanIDs(durable.SubmissionQuery{Status: durable.SubmissionQueued}), []durable.SubmissionID{firstID, otherID}, "queued")
		c.equal(scanIDs(durable.SubmissionQuery{Status: durable.SubmissionPlaced}), []durable.SubmissionID{secondID}, "placed")
		c.equal(scanIDs(durable.SubmissionQuery{ConversationID: &secondConversation, Status: durable.SubmissionQueued}), []durable.SubmissionID{otherID}, "second queued")
		c.equal(scanIDs(durable.SubmissionQuery{ConversationID: &secondConversation, Status: durable.SubmissionPlaced}), []durable.SubmissionID{}, "second placed")
		pg, err := c.s.ScanSubmissions(ctx, durable.SubmissionQuery{Status: durable.SubmissionPlaced}, 10, nil)
		c.must(err)
		c.equal(pg.Items, []durable.SubmissionRecord{placed}, "placed records")
	}},

	{"stores passive write submissions without input-only lifecycle states", func(c *checker) {
		root := c.createRoot()
		doneID, failedID := mint[durable.SubmissionID](c), mint[durable.SubmissionID](c)
		queuedDone := durable.SubmissionRecord{ID: doneID, ConversationID: root, RequestID: "passive-done", Type: durable.SubmissionTypeWrite, Status: durable.SubmissionQueued}
		queuedFailed := durable.SubmissionRecord{ID: failedID, ConversationID: root, RequestID: "passive-failed", Type: durable.SubmissionTypeWrite, Status: durable.SubmissionQueued}
		c.commit(subW(queuedDone), subW(queuedFailed))
		done := queuedDone
		done.Status, done.Entry = durable.SubmissionDone, p(mint[durable.EntryID](c))
		unanswered := queuedFailed
		unanswered.Status, unanswered.Reason, unanswered.Detail = durable.SubmissionUnanswered, "closed", raw(`{"retryable":false}`)
		c.commit(subW(done), subW(unanswered))
		for _, want := range []durable.SubmissionRecord{done, unanswered} {
			got, _ := c.s.Submission(ctx, want.ID)
			c.equal(got, &want, "submission")
			got, _ = c.s.SubmissionByRequest(ctx, root, want.RequestID)
			c.equal(got, &want, "by request")
		}
	}},

	{"reconstructs rewindable documents and preserves half-open incarnations", testRewindableDocuments},
	{"streams long document tails across root replacement deltas", testLongDocumentTails},
	{"copies stored document bases independently and rejects ambiguous sources", testDocumentCopies},

	{"uses bases for version transitions and rejects historical reads of current-only documents", func(c *checker) {
		c.createRoot()
		id := mint[durable.DocumentID](c)
		record := durable.DocumentCreate{ID: id, Kind: "session.settings", Scope: durable.SessionScope()}
		c.commit(durable.DocumentCreateWrite{Record: record, Content: base(1, obj{"count": 1.0})})
		c.commit(durable.DocumentChangeWrite{ID: id, Content: deltaContent(1, set(2.0, "count"))})
		migratedAt := c.commit(durable.DocumentChangeWrite{ID: id, Content: base(2, obj{"count": 3.0})})
		c.documentMatches(id, durable.Current, 2, obj{"count": 3.0}, -1)
		_, err := c.s.Document(ctx, id, migratedAt)
		c.fails(err, "does not retain historical content")
		_, err = c.s.Commit(ctx, []durable.StorageWrite{durable.DocumentChangeWrite{ID: id, Content: deltaContent(1, set(4.0, "count"))}})
		c.fails(err, "version transition requires a base")
		c.documentMatches(id, durable.Current, -1, obj{"count": 3.0}, -1)
		c.commit(durable.DocumentRetireWrite{ID: id})
		c.equal(c.document(id, durable.Current), (*durable.StoredDocument)(nil), "retired")
	}},

	{"indexes logical addresses and exact-scope scans independently", testDocumentAddresses},

	{"keeps document lifecycle failures atomic and gives create-plus-retire an empty lifetime", func(c *checker) {
		root := c.createRoot()
		firstID, secondID := mint[durable.DocumentID](c), mint[durable.DocumentID](c)
		record := durable.DocumentCreate{ID: firstID, Kind: "singleton", Scope: durable.SessionScope()}
		c.commit(durable.DocumentCreateWrite{Record: record, Content: base(1, obj{"value": 1.0})})
		second := record
		second.ID = secondID
		_, err := c.s.Commit(ctx, []durable.StorageWrite{
			durable.DocumentCreateWrite{Record: second, Content: base(1, obj{"value": 2.0})},
			durable.DocumentChangeWrite{ID: firstID, Content: deltaContent(1)},
		})
		c.fails(err, "already has a current incarnation")
		c.documentMatches(firstID, durable.Current, -1, obj{"value": 1.0}, -1)
		c.equal(c.document(secondID, durable.Current), (*durable.StoredDocument)(nil), "rejected second")

		emptyID := mint[durable.DocumentID](c)
		emptyRecord := durable.DocumentCreate{ID: emptyID, Kind: record.Kind, Key: p("empty"), Scope: durable.ConversationScope(root), History: durable.HistoryRewindable, Fork: durable.ForkInitial}
		emptyAt := c.commit(durable.DocumentCreateWrite{Record: emptyRecord, Content: base(1, obj{})}, durable.DocumentRetireWrite{ID: emptyID})
		c.equal(c.document(emptyID, durable.Current), (*durable.StoredDocument)(nil), "empty current")
		c.equal(c.document(emptyID, emptyAt), (*durable.StoredDocument)(nil), "empty at its commit")
		found, err := c.s.FindDocument(ctx, durable.DocumentAddress{Kind: record.Kind, Scope: durable.ConversationScope(root), Key: p("empty")}, emptyAt)
		c.must(err)
		c.equal(found, (*durable.DocumentRecord)(nil), "empty by address")
	}},

	{"rolls back record tables and secondary indexes when a document command fails", func(c *checker) {
		root := c.createRoot()
		taskID, subID, docID := mint[durable.TaskID](c), mint[durable.SubmissionID](c), mint[durable.DocumentID](c)
		task := pendingTask(taskID, root)
		sub := durable.SubmissionRecord{ID: subID, ConversationID: root, RequestID: "atomic", Type: durable.SubmissionTypeInput, Status: durable.SubmissionQueued}
		record := durable.DocumentCreate{ID: docID, Kind: "atomic", Scope: durable.SessionScope()}
		baseline := c.commit(taskW(task), subW(sub), durable.DocumentCreateWrite{Record: record, Content: base(1, obj{"count": 1.0})})

		entryID, conflictID := mint[durable.EntryID](c), mint[durable.DocumentID](c)
		running := task
		running.State = durable.TaskState{Status: durable.TaskRunning, Checkpoint: raw(`{"phase":"effect"}`)}
		failed := sub
		failed.Status, failed.Reason = durable.SubmissionUnanswered, "failed"
		conflict := record
		conflict.ID = conflictID
		_, err := c.s.Commit(ctx, []durable.StorageWrite{taskW(running), subW(failed), entryW(entry(entryID, root, "transient")),
			durable.DocumentCreateWrite{Record: conflict, Content: base(1, obj{"count": 2.0})}})
		c.fails(err, "already has a current incarnation")

		got, _ := c.s.Task(ctx, taskID)
		c.equal(got, &task, "task")
		pending, _ := c.s.ScanTasks(ctx, durable.TaskQuery{Status: durable.TaskPending}, 10, nil)
		c.equal(pending.Items, []durable.TaskRecord{task}, "pending scan")
		bySub, _ := c.s.SubmissionByRequest(ctx, root, "atomic")
		c.equal(bySub, &sub, "submission")
		gone, _ := c.s.Entry(ctx, entryID)
		c.equal(gone, (*durable.StoredEntry)(nil), "entry")
		c.equal(c.document(conflictID, durable.Current), (*durable.StoredDocument)(nil), "conflicting document")
		found, _ := c.s.FindDocument(ctx, durable.DocumentAddress{Kind: record.Kind, Scope: record.Scope}, durable.Current)
		c.equal(found.ID, docID, "address")
		after := c.commit(durable.DocumentChangeWrite{ID: docID, Content: deltaContent(1, set(3.0, "count"))})
		if after <= baseline {
			c.fatalf("sequence %d after a rollback, want more than %d", after, baseline)
		}
	}},

	{"keeps indexed string identities lossless", func(c *checker) {
		root := c.createRoot()
		// pi's identities are lone surrogates; a Go string can hold any bytes,
		// and these two encode the surrogates U+D800 and U+D801 (CESU-8).
		first, second := "\xed\xa0\x80", "\xed\xa0\x81"
		firstTask, secondTask := mint[durable.TaskID](c), mint[durable.TaskID](c)
		firstSub, secondSub := mint[durable.SubmissionID](c), mint[durable.SubmissionID](c)
		firstKind, secondKind := mint[durable.DocumentID](c), mint[durable.DocumentID](c)
		firstKey, secondKey := mint[durable.DocumentID](c), mint[durable.DocumentID](c)
		t1, t2 := pendingTask(firstTask, root), pendingTask(secondTask, root)
		t1.Kind, t2.Kind = first, second
		sub := func(id durable.SubmissionID, request string) durable.StorageWrite {
			return subW(durable.SubmissionRecord{ID: id, ConversationID: root, RequestID: request, Type: durable.SubmissionTypeInput, Status: durable.SubmissionQueued})
		}
		doc := func(id durable.DocumentID, kind string, key *string, identity string) durable.StorageWrite {
			return durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: id, Kind: kind, Key: key, Scope: durable.SessionScope()}, Content: base(1, obj{"identity": identity})}
		}
		writes := []durable.StorageWrite{taskW(t1), taskW(t2), sub(firstSub, first), sub(secondSub, second),
			doc(firstKind, first, nil, "first kind"), doc(secondKind, second, nil, "second kind"),
			doc(firstKey, "family", p(first), "first key"), doc(secondKey, "family", p(second), "second key")}
		if c.options.RejectsInvalidUTF8 {
			for _, w := range writes {
				_, err := c.s.Commit(ctx, []durable.StorageWrite{w})
				c.fails(err, "UTF-8")
			}
			got, err := c.s.Task(ctx, firstTask)
			c.must(err)
			c.equal(got, (*durable.TaskRecord)(nil), "rejected task")
			c.equal(c.document(firstKey, durable.Current), (*durable.StoredDocument)(nil), "rejected document")
			c.commit(taskW(pendingTask(firstTask, root)))
			return
		}
		c.commit(writes...)

		for _, k := range []struct {
			kind string
			id   durable.TaskID
		}{{first, firstTask}, {second, secondTask}} {
			pg, _ := c.s.ScanTasks(ctx, durable.TaskQuery{Kind: k.kind}, 10, nil)
			c.equal(taskIDs(pg), []durable.TaskID{k.id}, "tasks by kind")
			got, _ := c.s.Task(ctx, k.id)
			c.equal(got.Kind, k.kind, "task kind")
		}
		s1, _ := c.s.SubmissionByRequest(ctx, root, first)
		c.equal(s1.RequestID, first, "request ID")
		c.equal(s1.ID, firstSub, "first submission")
		s2, _ := c.s.SubmissionByRequest(ctx, root, second)
		c.equal(s2.ID, secondSub, "second submission")
		find := func(kind string, key *string) durable.DocumentID {
			r, err := c.s.FindDocument(ctx, durable.DocumentAddress{Kind: kind, Scope: durable.SessionScope(), Key: key}, durable.Current)
			c.must(err)
			if r == nil {
				c.fatalf("no document %q %v", kind, key)
			}
			return r.ID
		}
		c.equal(find(first, nil), firstKind, "first kind")
		c.equal(find(second, nil), secondKind, "second kind")
		c.equal(find("family", p(first)), firstKey, "first key")
		c.equal(find("family", p(second)), secondKey, "second key")
		pg, _ := c.s.ScanDocuments(ctx, durable.DocumentQuery{Scope: durable.SessionScope(), At: durable.Current, Kind: first}, 10, nil)
		c.equal(documentIDs(pg), []durable.DocumentID{firstKind}, "documents by kind")
	}},

	{"keeps one global record ID namespace and rejects exhausted ID minting", func(c *checker) {
		root := c.createRoot()
		c.commit(entryW(entry(100, root, "")))
		if v := mint[durable.EntryID](c); v != 101 {
			c.fatalf("minted %d, want 101", v)
		}
		_, err := c.s.Commit(ctx, []durable.StorageWrite{taskW(pendingTask(100, root))})
		c.fails(err, "ID 100 already belongs to entry")
		c.commit(entryW(entry(durable.EntryID(1<<53-1), root, "last-id")))
		for range 2 {
			_, err = c.s.MintID(ctx)
			c.fails(err, "ID space is exhausted")
		}
	}},

	{"rejects every operation after close", func(c *checker) {
		c.createRoot()
		c.must(c.s.Close(ctx))
		_, err := c.s.Conversation(ctx, durable.RootConversationID)
		c.fails(err, "closed")
		_, err = c.s.Commit(ctx, nil)
		c.fails(err, "closed")
		_, err = c.s.MintID(ctx)
		c.fails(err, "closed")
	}},
}

func testDeepForkHistory(c *checker) {
	root := c.createRoot()
	rootFirst, rootForkPoint, rootExcludedSame := mint[durable.EntryID](c), mint[durable.EntryID](c), mint[durable.EntryID](c)
	fork := entry(rootForkPoint, root, "marker")
	fork.Head = &rootFirst
	rootSeq := c.commit(entryW(entry(rootFirst, root, "")), entryW(fork), entryW(entry(rootExcludedSame, root, "")))
	child := mint[durable.ConversationID](c)
	c.commit(convW(durable.ConversationRecord{ID: child, Parent: &durable.ConversationParent{ConversationID: root, At: rootForkPoint}}))
	childForkPoint, childExcluded := mint[durable.EntryID](c), mint[durable.EntryID](c)
	c.commit(entryW(entry(childForkPoint, child, "note")), entryW(entry(childExcluded, child, "")))
	rootExcludedLater := mint[durable.EntryID](c)
	c.commit(entryW(entry(rootExcludedLater, root, "")))
	grandchild := mint[durable.ConversationID](c)
	c.commit(convW(durable.ConversationRecord{ID: grandchild, Parent: &durable.ConversationParent{ConversationID: child, At: childForkPoint}}))
	grandchildHead, grandchildTail := mint[durable.EntryID](c), mint[durable.EntryID](c)
	head := entry(grandchildHead, grandchild, "marker")
	head.Head = &grandchildHead
	grandchildSeq := c.commit(entryW(head), entryW(entry(grandchildTail, grandchild, "")))
	childExcludedLater := mint[durable.EntryID](c)
	c.commit(entryW(entry(childExcludedLater, child, "")))

	scan := func(q durable.EntryQuery, limit int, cursor durable.Cursor) durable.Page[durable.EntryRecord] {
		c.t.Helper()
		pg, err := c.s.ScanEntries(ctx, q, limit, cursor)
		c.must(err)
		return pg
	}
	newest := durable.EntryQuery{ConversationID: grandchild}
	first := scan(newest, 2, nil)
	c.equal(entryIDs(first), []durable.EntryID{grandchildTail, grandchildHead}, "first page")
	second := scan(newest, 2, first.Next)
	c.equal(entryIDs(second), []durable.EntryID{childForkPoint, rootForkPoint}, "second page")
	third := scan(newest, 2, second.Next)
	c.equal(entryIDs(third), []durable.EntryID{rootFirst}, "third page")
	c.equal(third.Next, durable.Cursor(nil), "third next")

	// Oldest first: the root's segment, then each fork's, each capped at the
	// next fork point.
	ascending := durable.EntryQuery{ConversationID: grandchild, Order: durable.Ascending}
	firstUp := scan(ascending, 2, nil)
	c.equal(entryIDs(firstUp), []durable.EntryID{rootFirst, rootForkPoint}, "first ascending page")
	secondUp := scan(ascending, 2, firstUp.Next)
	c.equal(entryIDs(secondUp), []durable.EntryID{childForkPoint, grandchildHead}, "second ascending page")
	thirdUp := scan(newest, 2, secondUp.Next)
	c.equal(entryIDs(thirdUp), []durable.EntryID{grandchildTail}, "third ascending page")
	c.equal(thirdUp.Next, durable.Cursor(nil), "third ascending next")
	bounded := ascending
	bounded.MinEntryID, bounded.MaxEntryID = &rootForkPoint, &childForkPoint
	c.equal(entryIDs(scan(bounded, 10, nil)), []durable.EntryID{rootForkPoint, childForkPoint}, "bounded ascending")
	_, err := c.s.ScanEntries(ctx, durable.EntryQuery{ConversationID: grandchild, Order: durable.Descending}, 2, firstUp.Next)
	c.fails(err, "cursor")

	current, err := c.s.FindLatestHeadMarker(ctx, grandchild, nil)
	c.must(err)
	c.equal([2]durable.EntryID{current.ID, *current.Head}, [2]durable.EntryID{grandchildHead, grandchildHead}, "current marker")
	historical, err := c.s.FindLatestHeadMarker(ctx, grandchild, &childForkPoint)
	c.must(err)
	c.equal([2]durable.EntryID{historical.ID, *historical.Head}, [2]durable.EntryID{rootForkPoint, rootFirst}, "historical marker")
	none, err := c.s.FindLatestHeadMarker(ctx, grandchild, &rootFirst)
	c.must(err)
	c.equal(none, (*durable.EntryRecord)(nil), "marker before every head")

	active := durable.EntryQuery{ConversationID: grandchild, MinEntryID: current.Head}
	activeFirst := scan(active, 1, nil)
	c.equal(entryIDs(activeFirst), []durable.EntryID{grandchildTail}, "active first")
	if activeFirst.Next == nil {
		c.fatalf("no active cursor")
	}
	activeSecond := scan(active, 1, activeFirst.Next)
	c.equal(entryIDs(activeSecond), []durable.EntryID{grandchildHead}, "active second")
	c.equal(activeSecond.Next, durable.Cursor(nil), "active second next")
	c.equal(entryIDs(scan(durable.EntryQuery{ConversationID: grandchild, MinEntryID: historical.Head, MaxEntryID: &childForkPoint}, 10, nil)),
		[]durable.EntryID{childForkPoint, rootForkPoint, rootFirst}, "historical range")

	stored := func(e *durable.StoredEntry, err error) *durable.StoredEntry {
		c.t.Helper()
		c.must(err)
		return e
	}
	c.equal(stored(c.s.Entry(ctx, rootFirst)), &durable.StoredEntry{Entry: entry(rootFirst, root, ""), CommitSeq: rootSeq}, "root first")
	c.equal(stored(c.s.Entry(ctx, rootForkPoint)).CommitSeq, rootSeq, "root fork point seq")
	c.equal(stored(c.s.Entry(ctx, grandchildHead)).CommitSeq, grandchildSeq, "grandchild head seq")
	c.equal(stored(c.s.Entry(ctx, grandchildTail)).CommitSeq, grandchildSeq, "grandchild tail seq")
	c.equal(stored(c.s.Entry(ctx, 999_999)), (*durable.StoredEntry)(nil), "absent entry")

	c.equal(stored(c.s.VisibleEntry(ctx, grandchild, rootFirst)), &durable.StoredEntry{Entry: entry(rootFirst, root, ""), CommitSeq: rootSeq}, "visible root first")
	c.equal(stored(c.s.VisibleEntry(ctx, grandchild, childForkPoint)).Entry.ConversationID, child, "visible child fork point")
	c.equal(stored(c.s.VisibleEntry(ctx, grandchild, grandchildTail)).CommitSeq, grandchildSeq, "visible grandchild tail")
	for _, hidden := range []durable.EntryID{rootExcludedSame, rootExcludedLater, childExcluded, childExcludedLater, 999_999} {
		c.equal(stored(c.s.VisibleEntry(ctx, grandchild, hidden)), (*durable.StoredEntry)(nil), fmt.Sprintf("hidden %d", hidden))
	}
	c.equal(stored(c.s.VisibleEntry(ctx, root, grandchildHead)), (*durable.StoredEntry)(nil), "grandchild entry from root")
	_, err = c.s.VisibleEntry(ctx, 999_999, rootFirst)
	c.fails(err, "Unknown conversation")
	_, err = c.s.ScanEntries(ctx, durable.EntryQuery{ConversationID: 999_999}, 10, nil)
	c.fails(err, "Unknown conversation")
}

func testRewindableDocuments(c *checker) {
	root := c.createRoot()
	firstID := mint[durable.DocumentID](c)
	firstRecord := durable.DocumentCreate{ID: firstID, Kind: "conversation.notes", Scope: durable.ConversationScope(root), History: durable.HistoryRewindable, Fork: durable.ForkAsOf}
	initial := obj{"items": arr{"a"}, "nested": obj{"count": 1.0}}
	createdAt := c.commit(durable.DocumentCreateWrite{Record: firstRecord, Content: base(1, initial)})
	appended := arr{"b"}
	changedAt := c.commit(durable.DocumentChangeWrite{ID: firstID, Content: deltaContent(1,
		delta.Splice{Path: path_([]any{"items"}), Index: 1, Remove: 0, Items: appended},
		set(2.0, "nested", "count"))})

	initial["items"] = append(initial["items"].(arr), "caller mutation")
	appended[0] = "caller mutation"
	c.documentMatches(firstID, createdAt, 1, obj{"items": arr{"a"}, "nested": obj{"count": 1.0}}, 0)
	changed := c.document(firstID, changedAt)
	c.equal(changed.Value, obj{"items": arr{"a", "b"}, "nested": obj{"count": 2.0}}, "changed")
	c.equal(changed.DeltasSinceBase, 1, "changed deltas")
	changed.Value["items"] = append(changed.Value["items"].(arr), "read mutation")
	c.documentMatches(firstID, durable.Current, -1, obj{"items": arr{"a", "b"}, "nested": obj{"count": 2.0}}, -1)

	checkpointAt := c.commit(durable.DocumentChangeWrite{ID: firstID, Content: base(2, obj{"items": arr{"checkpoint"}, "nested": obj{"count": 3.0}})})
	replacedAt := c.commit(durable.DocumentChangeWrite{ID: firstID, Content: deltaContent(2, delta.Replace{Value: obj{"items": arr{"replacement"}, "nested": obj{"count": 4.0}}})})
	c.documentMatches(firstID, changedAt, 1, obj{"items": arr{"a", "b"}, "nested": obj{"count": 2.0}}, -1)
	c.documentMatches(firstID, checkpointAt, 2, obj{"items": arr{"checkpoint"}, "nested": obj{"count": 3.0}}, 0)
	c.documentMatches(firstID, replacedAt, -1, obj{"items": arr{"replacement"}, "nested": obj{"count": 4.0}}, 1)
	c.equal(c.document(firstID, durable.Current).DeltasSinceBase, 1, "current deltas")

	secondID := mint[durable.DocumentID](c)
	secondRecord := firstRecord
	secondRecord.ID = secondID
	retiredAt := c.commit(
		durable.DocumentCreateWrite{Record: secondRecord, Content: base(1, obj{"items": arr{"new"}})},
		durable.DocumentRetireWrite{ID: firstID},
		durable.DocumentChangeWrite{ID: firstID, Content: deltaContent(2, set(true, "retiring"))},
	)
	address := durable.DocumentAddress{Kind: firstRecord.Kind, Scope: firstRecord.Scope}
	found, err := c.s.FindDocument(ctx, address, changedAt)
	c.must(err)
	c.equal(found.ID, firstID, "address at the change")
	found, err = c.s.FindDocument(ctx, address, retiredAt)
	c.must(err)
	c.equal([2]any{found.ID, found.CreatedAt}, [2]any{secondID, retiredAt}, "address at the retirement")
	pg, err := c.s.ScanDocuments(ctx, durable.DocumentQuery{Scope: firstRecord.Scope, At: changedAt}, 10, nil)
	c.must(err)
	c.equal(documentIDs(pg), []durable.DocumentID{firstID}, "scope at the change")
	pg, err = c.s.ScanDocuments(ctx, durable.DocumentQuery{Scope: firstRecord.Scope, At: retiredAt}, 10, nil)
	c.must(err)
	c.equal(documentIDs(pg), []durable.DocumentID{secondID}, "scope at the retirement")
	c.equal(c.document(firstID, retiredAt), (*durable.StoredDocument)(nil), "retired at its retirement")
	c.documentMatches(secondID, durable.Current, -1, obj{"items": arr{"new"}}, -1)
}

func rows(n int, value func(int) float64, stable func(int) string) arr {
	out := make(arr, n)
	for i := range out {
		out[i] = obj{"value": value(i), "stable": stable(i)}
	}
	return out
}

func testLongDocumentTails(c *checker) {
	root := c.createRoot()
	id := mint[durable.DocumentID](c)
	record := durable.DocumentCreate{ID: id, Kind: "conversation.long-tail", Scope: durable.ConversationScope(root), History: durable.HistoryRewindable, Fork: durable.ForkAsOf}
	makeInitial := func() obj {
		return obj{"revision": 0.0, "rows": rows(512, func(i int) float64 { return float64(i) }, func(i int) string { return fmt.Sprintf("row-%d", i) })}
	}
	createdAt := c.commit(durable.DocumentCreateWrite{Record: record, Content: base(1, makeInitial())})
	before := makeInitial()
	beforeAt := createdAt
	for revision := 1; revision <= 24; revision++ {
		index := (revision * 17) % 512
		before["rows"].(arr)[index].(obj)["value"] = float64(-revision)
		before["revision"] = float64(revision)
		beforeAt = c.commit(durable.DocumentChangeWrite{ID: id, Content: deltaContent(1,
			set(float64(-revision), "rows", index, "value"), set(float64(revision), "revision"))})
	}
	makeReplacement := func() obj {
		return obj{"revision": 100.0, "rows": rows(512, func(i int) float64 { return 10_000 + float64(i) }, func(i int) string { return fmt.Sprintf("new-%d", i) })}
	}
	replacement := makeReplacement()
	replacementAt := c.commit(durable.DocumentChangeWrite{ID: id, Content: deltaContent(1, delta.Replace{Value: replacement})})
	replacement["rows"].(arr)[0].(obj)["value"] = -999.0

	current := makeReplacement()
	for revision := 101; revision <= 124; revision++ {
		index := (revision * 19) % 512
		current["rows"].(arr)[index].(obj)["value"] = float64(-revision)
		current["revision"] = float64(revision)
		c.commit(durable.DocumentChangeWrite{ID: id, Content: deltaContent(1,
			set(float64(-revision), "rows", index, "value"), set(float64(revision), "revision"))})
	}
	c.documentMatches(id, createdAt, -1, makeInitial(), -1)
	c.documentMatches(id, beforeAt, -1, before, -1)
	c.documentMatches(id, replacementAt, -1, makeReplacement(), -1)
	read := c.document(id, durable.Current)
	c.equal(read.Value, current, "current")
	read.Value["rows"].(arr)[0].(obj)["value"] = -1_000.0
	c.documentMatches(id, durable.Current, -1, current, -1)
}

func testDocumentCopies(c *checker) {
	root := c.createRoot()
	child, secondChild := mint[durable.ConversationID](c), mint[durable.ConversationID](c)
	c.commit(convW(durable.ConversationRecord{ID: child}), convW(durable.ConversationRecord{ID: secondChild}))
	sourceID := mint[durable.DocumentID](c)
	sourceRecord := durable.DocumentCreate{ID: sourceID, Kind: "copy.source", Scope: durable.ConversationScope(root), History: durable.HistoryRewindable, Fork: durable.ForkAsOf}
	createdAt := c.commit(durable.DocumentCreateWrite{Record: sourceRecord, Content: base(2, obj{"count": 1.0, "rows": arr{obj{"value": "base"}}})})
	c.commit(durable.DocumentChangeWrite{ID: sourceID, Content: deltaContent(2, set(2.0, "count"),
		delta.Splice{Path: path_([]any{"rows"}), Index: 1, Items: arr{obj{"value": "current"}}})})
	historical, currentCopy, retiredCopy := mint[durable.DocumentID](c), mint[durable.DocumentID](c), mint[durable.DocumentID](c)
	childRecord := func(id durable.DocumentID, cid durable.ConversationID) durable.DocumentCreate {
		return durable.DocumentCreate{ID: id, Kind: sourceRecord.Kind, Scope: durable.ConversationScope(cid), History: durable.HistoryRewindable, Fork: durable.ForkAsOf}
	}
	c.commit(
		durable.DocumentCopyWrite{Record: childRecord(historical, child), Source: durable.DocumentCopySource{ID: sourceID, At: createdAt}},
		durable.DocumentCopyWrite{Record: childRecord(currentCopy, secondChild), Source: durable.DocumentCopySource{ID: sourceID, At: durable.Current}},
		durable.DocumentCopyWrite{Record: childRecord(retiredCopy, root), Source: durable.DocumentCopySource{ID: sourceID, At: durable.Current}},
		durable.DocumentRetireWrite{ID: retiredCopy},
	)
	both := obj{"count": 2.0, "rows": arr{obj{"value": "base"}, obj{"value": "current"}}}
	c.documentMatches(historical, durable.Current, 2, obj{"count": 1.0, "rows": arr{obj{"value": "base"}}}, -1)
	c.documentMatches(currentCopy, durable.Current, 2, both, -1)
	c.equal(c.document(retiredCopy, durable.Current), (*durable.StoredDocument)(nil), "retired copy")

	c.commit(durable.DocumentChangeWrite{ID: sourceID, Content: base(2, obj{"count": 99.0, "rows": arr{}})}, durable.DocumentRetireWrite{ID: sourceID})
	c.documentMatches(currentCopy, durable.Current, -1, both, -1)

	latestSource, latestCopy := mint[durable.DocumentID](c), mint[durable.DocumentID](c)
	latestRecord := durable.DocumentCreate{ID: latestSource, Kind: "copy.latest", Scope: durable.ConversationScope(root), History: durable.HistoryLatest, Fork: durable.ForkCurrent}
	c.commit(durable.DocumentCreateWrite{Record: latestRecord, Content: base(4, obj{"retained": "copy"})})
	copyRecord := latestRecord
	copyRecord.ID, copyRecord.Scope = latestCopy, durable.ConversationScope(child)
	c.commit(durable.DocumentCopyWrite{Record: copyRecord, Source: durable.DocumentCopySource{ID: latestSource, At: durable.Current}})
	c.commit(durable.DocumentChangeWrite{ID: latestSource, Content: base(4, obj{"retained": "source-only"})}, durable.DocumentRetireWrite{ID: latestSource})
	c.documentMatches(latestCopy, durable.Current, 4, obj{"retained": "copy"}, -1)

	conflict := mint[durable.DocumentID](c)
	_, err := c.s.Commit(ctx, []durable.StorageWrite{
		durable.DocumentCopyWrite{Record: childRecord(conflict, child), Source: durable.DocumentCopySource{ID: currentCopy, At: durable.Current}},
		durable.DocumentRetireWrite{ID: currentCopy},
	})
	if err == nil {
		c.fatalf("a copy of a document retired in the same batch was accepted")
	}
	// Rejected without effect: the copy and the retirement are both absent.
	c.equal(c.document(conflict, durable.Current), (*durable.StoredDocument)(nil), "conflicting copy")
	c.documentMatches(currentCopy, durable.Current, -1, both, -1)

	mismatch := mint[durable.DocumentID](c)
	mismatchRecord := childRecord(mismatch, child)
	mismatchRecord.Kind = "copy.mismatch"
	_, err = c.s.Commit(ctx, []durable.StorageWrite{durable.DocumentCopyWrite{Record: mismatchRecord, Source: durable.DocumentCopySource{ID: currentCopy, At: durable.Current}}})
	if err == nil {
		c.fatalf("a copy of another kind was accepted")
	}
	c.equal(c.document(mismatch, durable.Current), (*durable.StoredDocument)(nil), "mismatched copy")
}

func testDocumentAddresses(c *checker) {
	root := c.createRoot()
	firstID, secondID, conversationDoc := mint[durable.DocumentID](c), mint[durable.DocumentID](c), mint[durable.DocumentID](c)
	taskID := mint[durable.TaskID](c)
	taskSingleton, taskFamily, taskOther := mint[durable.DocumentID](c), mint[durable.DocumentID](c), mint[durable.DocumentID](c)
	create := func(id durable.DocumentID, kind string, scope durable.DocumentScope, key *string, owner string, history, fork string) durable.StorageWrite {
		return durable.DocumentCreateWrite{
			Record:  durable.DocumentCreate{ID: id, Kind: kind, Scope: scope, Key: key, History: history, Fork: fork},
			Content: base(1, obj{"owner": owner}),
		}
	}
	session, task := durable.SessionScope(), durable.TaskScope(taskID)
	createdAt := c.commit(
		taskW(pendingTask(taskID, root)),
		create(firstID, "cache", session, p("__proto__"), "first", "", ""),
		create(secondID, "cache", session, p("constructor"), "second", "", ""),
		create(conversationDoc, "cache", durable.ConversationScope(root), p("__proto__"), "conversation", durable.HistoryLatest, durable.ForkCurrent),
		create(taskSingleton, "task.cache", task, nil, "singleton", "", ""),
		create(taskFamily, "task.cache", task, p("member"), "family", "", ""),
		create(taskOther, "task.other", task, nil, "other", "", ""),
	)
	find := func(kind string, scope durable.DocumentScope, key *string) durable.DocumentID {
		c.t.Helper()
		r, err := c.s.FindDocument(ctx, durable.DocumentAddress{Kind: kind, Scope: scope, Key: key}, durable.Current)
		c.must(err)
		if r == nil {
			c.fatalf("no document %q", kind)
		}
		return r.ID
	}
	scan := func(q durable.DocumentQuery, limit int, cursor durable.Cursor) durable.Page[durable.DocumentRecord] {
		c.t.Helper()
		pg, err := c.s.ScanDocuments(ctx, q, limit, cursor)
		c.must(err)
		return pg
	}
	c.equal(find("cache", session, p("__proto__")), firstID, "session cache")
	sessionScan := durable.DocumentQuery{Scope: session, At: durable.Current}
	first := scan(sessionScan, 1, nil)
	c.equal(len(first.Items), 1, "first session page size")
	second := scan(sessionScan, 1, first.Next)
	c.equal(append(documentIDs(first), documentIDs(second)...), []durable.DocumentID{firstID, secondID}, "session pages")
	c.equal(documentIDs(scan(durable.DocumentQuery{Scope: durable.ConversationScope(root), At: durable.Current}, 10, nil)), []durable.DocumentID{conversationDoc}, "conversation scope")
	c.equal(find("task.cache", task, nil), taskSingleton, "task singleton")
	c.equal(find("task.cache", task, p("member")), taskFamily, "task family member")
	c.equal(documentIDs(scan(durable.DocumentQuery{Scope: task, At: durable.Current, Kind: "task.cache"}, 10, nil)), []durable.DocumentID{taskSingleton, taskFamily}, "task scope by kind")
	_, err := c.s.Document(ctx, taskSingleton, createdAt)
	c.fails(err, "does not retain historical content")
}
