package durable_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/session-tables.test.ts

var workTask = testTask{name: "test.work", version: 1, checkpoint: `{"phase":"start"}`}

var ProgressDoc = durable.MustDefineDoc(durable.DocDef{
	Kind: "test.progress", Version: 1, Scope: durable.ScopeTask,
	Initial: func(any) map[string]any { return map[string]any{"lines": []any{}} },
})

var StepDoc = durable.MustDefineDoc(durable.DocDef{
	Kind: "test.step", Version: 1, Family: true, Scope: durable.ScopeTask,
	Initial: func(any) map[string]any { return map[string]any{"lines": []any{}} },
})

var NotesDoc = durable.MustDefineDoc(durable.DocDef{
	Kind: "test.notes", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf,
	Initial: func(any) map[string]any { return map[string]any{"text": ""} },
})

func terminal(task durable.TaskRecord) durable.TaskRecord {
	return durable.TaskRecord{
		ID: task.ID, ConversationID: task.ConversationID, Kind: task.Kind, Version: task.Version, Input: task.Input,
		Background: task.Background, AbortRequested: task.AbortRequested,
		State: durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: durable.OutcomeCompleted, Result: json.RawMessage(`{"ok":true}`)}},
	}
}

func pushLine(tx *durable.Tx, ref durable.DocRef, line string) {
	_, err := mustDoc(tx, ref).At("lines").Push(line)
	must(err)
}

func createWorkTask(t *testing.T, s *durable.Session, conversationID durable.ConversationID, withDocument bool) durable.TaskID {
	t.Helper()
	id, err := durable.CommitValue(bg, s, func(tx *durable.Tx) (durable.TaskID, error) {
		id, err := tx.CreateTask(workTask, json.RawMessage(`{"path":"a"}`), ownedByConversation(conversationID))
		if err == nil && withDocument {
			pushLine(tx, ProgressDoc.Task(id), "started")
		}
		return id, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func readTask(tx *durable.Tx, id durable.TaskID) durable.TaskRecord {
	task, err := tx.Task(id)
	must(err)
	if task == nil {
		panic(fmt.Sprintf("task %d is missing", id))
	}
	return *task
}

func isReadAfterWrite(err error) bool {
	var target *durable.ReadAfterWriteError
	return errors.As(err, &target)
}

func TestTablesAllowReadsOnlyBeforeTheFirstTableWrite(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	commit(t, ts.session, func(tx *durable.Tx) error {
		if c, err := tx.Conversation(conversationID); err != nil || !reflect.DeepEqual(*c, durable.ConversationRecord{ID: conversationID}) {
			t.Errorf("conversation %v %v", c, err)
		}
		if p, err := tx.ScanConversations(durable.ConversationQuery{}, 1, nil); err != nil || len(p.Items) != 1 || p.Items[0].ID != conversationID {
			t.Errorf("scan conversations %v %v", p, err)
		}
		if p, err := tx.ScanTasks(durable.TaskQuery{ConversationID: &conversationID}, 10, nil); err != nil || len(p.Items) != 0 {
			t.Errorf("scan tasks %v %v", p, err)
		}
		if p, err := tx.ScanEntries(durable.EntryQuery{ConversationID: conversationID}, 10, nil); err != nil || len(p.Items) != 0 {
			t.Errorf("scan entries %v %v", p, err)
		}
		appendEntry(tx, conversationID, "note")
		_, err := tx.Conversation(conversationID)
		if !isReadAfterWrite(err) {
			t.Errorf("conversation after a write: %v", err)
		}
		_, err = tx.Task(1)
		errorContains(t, err, "Tx.task() cannot read tables after the first table write")
		_, err = tx.Entry(1)
		if !isReadAfterWrite(err) {
			t.Errorf("entry after a write: %v", err)
		}
		_, err = tx.ScanConversations(durable.ConversationQuery{}, 10, nil)
		if !isReadAfterWrite(err) {
			t.Errorf("scan conversations after a write: %v", err)
		}
		_, err = tx.ScanEntries(durable.EntryQuery{ConversationID: conversationID}, 10, nil)
		if !isReadAfterWrite(err) {
			t.Errorf("scan entries after a write: %v", err)
		}
		// Document access remains available after table writes.
		return mustDoc(tx, NotesDoc.Conversation(conversationID)).Set("text", "after write")
	})
	expectJSON(t, snapshot(t, ts.session, NotesDoc.Conversation(conversationID)), `{"text":"after write"}`)
}

func TestTablesPassLimitsAndCursorsThroughScans(t *testing.T) {
	ts := openTestSession(t)
	ids := []durable.ConversationID{createConversation(t, ts.session), createConversation(t, ts.session), createConversation(t, ts.session)}
	commit(t, ts.session, func(tx *durable.Tx) error {
		first, err := tx.ScanConversations(durable.ConversationQuery{}, 2, nil)
		must(err)
		if len(first.Items) != 2 || first.Items[0].ID != ids[0] || first.Items[1].ID != ids[1] || first.Next == nil {
			t.Errorf("first page %+v", first)
		}
		second, err := tx.ScanConversations(durable.ConversationQuery{}, 2, first.Next)
		must(err)
		if len(second.Items) != 1 || second.Items[0].ID != ids[2] || second.Next != nil {
			t.Errorf("second page %+v", second)
		}
		return nil
	})
}

func TestTablesSetTaskIsTheFirstTableWrite(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	taskID := createWorkTask(t, ts.session, conversationID, false)
	commit(t, ts.session, func(tx *durable.Tx) error {
		must(tx.SetTask(readTask(tx, taskID)))
		if _, err := tx.Task(taskID); !isReadAfterWrite(err) {
			t.Errorf("task after setTask: %v", err)
		}
		return nil
	})
}

func TestTablesCreateRecordsWithMintedIDs(t *testing.T) {
	ts := openTestSession(t)
	type created struct {
		conversation  durable.ConversationRecord
		first, headed durable.EntryRecord
		task          durable.TaskID
	}
	c, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (created, error) {
		conversation, err := tx.CreateConversation(durable.Ownerless)
		must(err)
		first, err := tx.AppendEntry(conversation.ID, durable.EntryDraft{Kind: "note", Data: json.RawMessage(`"one"`)})
		must(err)
		headed, err := tx.AppendEntry(conversation.ID, durable.EntryDraft{Kind: "summary", HeadSelf: true})
		must(err)
		options := ownedByConversation(conversation.ID)
		options.Background = true
		task, err := tx.CreateTask(workTask, json.RawMessage(`{"path":"x"}`), options)
		return created{conversation, first, headed, task}, err
	})
	must(err)
	ids := map[int64]bool{int64(c.conversation.ID): true, int64(c.first.ID): true, int64(c.headed.ID): true, int64(c.task): true}
	if len(ids) != 4 || c.headed.Head == nil || *c.headed.Head != c.headed.ID {
		t.Fatalf("created = %+v", c)
	}
	if !reflect.DeepEqual(c.first, durable.EntryRecord{ID: c.first.ID, ConversationID: c.conversation.ID, Kind: "note", Data: json.RawMessage(`"one"`)}) {
		t.Fatalf("first = %+v", c.first)
	}
	stored, err := ts.storage.Entry(bg, c.headed.ID)
	must(err)
	if !reflect.DeepEqual(stored.Entry, c.headed) {
		t.Fatalf("stored entry = %+v", stored.Entry)
	}
	task, err := ts.storage.Task(bg, c.task)
	must(err)
	want := durable.TaskRecord{
		ID: c.task, ConversationID: c.conversation.ID, Kind: "test.work", Version: 1, Input: json.RawMessage(`{"path":"x"}`),
		Background: true, State: durable.TaskState{Status: durable.TaskPending, Checkpoint: json.RawMessage(`{"phase":"start"}`)},
	}
	if !reflect.DeepEqual(*task, want) {
		t.Fatalf("task = %+v", *task)
	}
	changes := ts.lastPublication().Changes
	if len(changes) != 4 {
		t.Fatalf("changes = %d", len(changes))
	}
	admitted := ts.storage.lastAdmitted()
	var types []string
	var entries []durable.EntryRecord
	for _, change := range changes {
		found := false
		for _, w := range admitted {
			if reflect.DeepEqual(any(w), any(change)) {
				found = true
			}
		}
		if !found {
			t.Fatalf("change %+v is not an admitted write", change)
		}
		types = append(types, change.ChangeType())
		switch change := change.(type) {
		case durable.ConversationWrite:
			if !reflect.DeepEqual(change.Value, c.conversation) {
				t.Fatalf("conversation = %+v", change.Value)
			}
		case durable.EntryWrite:
			entries = append(entries, change.Value)
		}
	}
	if !reflect.DeepEqual(types, []string{"conversation", "entry", "entry", "task"}) {
		t.Fatalf("types = %v", types)
	}
	if !reflect.DeepEqual(entries, []durable.EntryRecord{c.first, c.headed}) {
		t.Fatalf("entries = %+v", entries)
	}
	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.CreateTask(workTask, json.RawMessage(`{"path":"x"}`), durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		return err
	})
	errorContains(t, err, "requires options.ConversationID")
	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.AppendEntry(12345, durable.EntryDraft{Kind: "note"})
		return err
	})
	errorContains(t, err, "Conversation 12345 does not exist")
}

func TestTablesCreateAConversationWithAStagedTaskOwner(t *testing.T) {
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var supervisorID durable.TaskID
	var child durable.ConversationRecord
	commit(t, ts.session, func(tx *durable.Tx) error {
		options := ownedByConversation(parentID)
		options.Background = true
		var err error
		supervisorID, err = tx.CreateTask(workTask, json.RawMessage(`{"path":"background"}`), options)
		must(err)
		child, err = tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: supervisorID})
		return err
	})
	if child.Owner == nil || *child.Owner != (durable.ConversationOwner{ConversationID: parentID, TaskID: supervisorID}) {
		t.Fatalf("owner = %+v", child.Owner)
	}
	if stored, _ := ts.storage.Conversation(bg, child.ID); !reflect.DeepEqual(*stored, child) {
		t.Fatalf("stored = %+v", stored)
	}
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		supervisor := readTask(tx, supervisorID)
		supervisor.ConversationID = child.ID
		return tx.SetTask(supervisor)
	})
	errorContains(t, err, fmt.Sprintf("Task %d cannot change conversations", supervisorID))
	if task, _ := ts.storage.Task(bg, supervisorID); task.ConversationID != parentID {
		t.Fatal("the task moved")
	}
}

func TestTablesRejectMissingTerminalAndAbortMarkedOwnersAtomically(t *testing.T) {
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	pending := durable.TaskState{Status: durable.TaskPending, Checkpoint: json.RawMessage(`{"phase":"start"}`)}
	record := func(id durable.TaskID, conversationID durable.ConversationID, path string) durable.TaskRecord {
		return durable.TaskRecord{ID: id, ConversationID: conversationID, Kind: workTask.name, Version: workTask.version, Input: json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)), State: pending}
	}
	var moved durable.TaskID
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		var err error
		moved, err = tx.CreateTask(workTask, json.RawMessage(`{"path":"move"}`), ownedByConversation(parentID))
		must(err)
		return tx.SetTask(record(moved, 998, "move"))
	})
	errorContains(t, err, "cannot change conversations")
	if task, _ := ts.storage.Task(bg, moved); task != nil {
		t.Fatal("the staged task was committed")
	}

	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: 999})
		return err
	})
	errorContains(t, err, "Conversation owner task 999 does not exist")

	var rejectedChild durable.ConversationID
	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		supervisorID, err := tx.CreateTask(workTask, json.RawMessage(`{"path":"aborting"}`), ownedByConversation(parentID))
		must(err)
		c, err := tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: supervisorID})
		must(err)
		rejectedChild = c.ID
		aborting := record(supervisorID, parentID, "aborting")
		aborting.AbortRequested = true
		return tx.SetTask(aborting)
	})
	errorContains(t, err, "is abort-marked")
	if rejectedChild == 0 {
		t.Fatal("no child was staged")
	}
	if c, _ := ts.storage.Conversation(bg, rejectedChild); c != nil {
		t.Fatal("the rejected child exists")
	}

	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		supervisorID, err := tx.CreateTask(workTask, json.RawMessage(`{"path":"terminal"}`), ownedByConversation(parentID))
		must(err)
		_, err = tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: supervisorID})
		must(err)
		return tx.SetTask(terminal(record(supervisorID, parentID, "terminal")))
	})
	errorContains(t, err, "is terminal")

	terminalOwner := createWorkTask(t, ts.session, parentID, false)
	commit(t, ts.session, func(tx *durable.Tx) error { return tx.SetTask(terminal(readTask(tx, terminalOwner))) })
	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: terminalOwner})
		return err
	})
	errorContains(t, err, "is terminal")
}

func TestTablesTakeOwnershipOfTableJSONAndRejectNonStrictValues(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	payload := []byte(`{"nested":{"value":1}}`)
	entry, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.EntryRecord, error) {
		created, err := tx.AppendEntry(conversationID, durable.EntryDraft{Kind: "data", Data: payload})
		copy(payload, `{"nested":{"value":2}}`)
		return created, err
	})
	must(err)
	if stored, _ := ts.storage.Entry(bg, entry.ID); string(stored.Entry.Data) != `{"nested":{"value":1}}` {
		t.Fatalf("data = %s", stored.Entry.Data)
	}
	omitted, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.EntryRecord, error) {
		return tx.AppendEntry(conversationID, durable.EntryDraft{Kind: "omitted"})
	})
	must(err)
	stored, _ := ts.storage.Entry(bg, omitted.ID)
	if omitted.Data != nil || stored.Entry.Data != nil || jsonOf(t, stored.Entry) != fmt.Sprintf(`{"id":%d,"conversationId":%d,"kind":"omitted"}`, omitted.ID, conversationID) {
		t.Fatalf("omitted = %s", jsonOf(t, stored.Entry))
	}
	commits := ts.storage.commitCount()
	err = ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.AppendEntry(conversationID, durable.EntryDraft{Kind: "invalid", Data: json.RawMessage("NaN")})
		return err
	})
	errorContains(t, err, "strict JSON")
	if ts.storage.commitCount() != commits {
		t.Fatal("a rejected entry was written")
	}
}

func TestTablesReplaceTaskRecordsCompletely(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	taskID := createWorkTask(t, ts.session, conversationID, false)
	commit(t, ts.session, func(tx *durable.Tx) error {
		task := readTask(tx, taskID)
		return tx.SetTask(durable.TaskRecord{
			ID: task.ID, ConversationID: task.ConversationID, Kind: task.Kind, Version: task.Version, Input: task.Input,
			Background: task.Background, AbortRequested: task.AbortRequested,
			State: durable.TaskState{Status: durable.TaskRunning, Checkpoint: json.RawMessage(`{"phase":"next","step":2}`)},
			Memos: map[string]json.RawMessage{"choice": json.RawMessage(`"b"`)},
		})
	})
	task, _ := ts.storage.Task(bg, taskID)
	if task.State.Status != durable.TaskRunning || string(task.State.Checkpoint) != `{"phase":"next","step":2}` || string(task.Memos["choice"]) != `"b"` {
		t.Fatalf("task = %+v", task)
	}
}

func TestTablesCreateATaskAndItsDocumentInOneTransaction(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	taskID, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.TaskID, error) {
		id, err := tx.CreateTask(workTask, json.RawMessage(`{"path":"a"}`), ownedByConversation(conversationID))
		must(err)
		// Validation uses the candidate task record, not a caller table read.
		pushLine(tx, ProgressDoc.Task(id), "created")
		pushLine(tx, StepDoc.Task(id).Member("one", nil), "step")
		return id, nil
	})
	must(err)
	expectJSON(t, snapshot(t, ts.session, ProgressDoc.Task(taskID)), `{"lines":["created"]}`)
	publication := ts.lastPublication()
	foundTask := false
	for _, c := range publication.Changes {
		if w, ok := c.(durable.TaskWrite); ok && w.Value.ID == taskID {
			foundTask = true
		}
	}
	documents := documentChanges(publication)
	if !foundTask || len(documents) != 2 {
		t.Fatalf("publication = %+v", publication)
	}
	// Task documents take their conversation from the task record.
	for _, d := range documents {
		if d.ConversationID != conversationID {
			t.Fatalf("conversation = %d", d.ConversationID)
		}
	}
	commit(t, ts.session, func(tx *durable.Tx) error {
		_, err := tx.CreateConversation(durable.Ownerless)
		must(err)
		pushLine(tx, ProgressDoc.Task(taskID), "committed task")
		return nil
	})
	if d := documentChanges(ts.lastPublication())[0]; d.ConversationID != conversationID {
		t.Fatalf("conversation = %d", d.ConversationID)
	}
}

func TestTablesRejectTaskDocumentsAfterATerminalCandidate(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	taskID := createWorkTask(t, ts.session, conversationID, true)
	commit(t, ts.session, func(tx *durable.Tx) error {
		task := readTask(tx, taskID)
		progress := mustDoc(tx, ProgressDoc.Task(taskID))
		must(tx.SetTask(terminal(task)))
		_, err := tx.Doc(ProgressDoc.Task(taskID))
		errorContains(t, err, fmt.Sprintf("Task %d is terminal", taskID))
		_, err = tx.Doc(StepDoc.Task(taskID).Member("late", nil))
		errorContains(t, err, fmt.Sprintf("Task %d is terminal", taskID))
		errorContains(t, tx.SetTask(task), "terminal candidate")
		_, err = progress.At("lines").Push("final")
		return err
	})
	err := ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(ProgressDoc.Task(taskID)); return err })
	errorContains(t, err, fmt.Sprintf("Task %d is terminal", taskID))
}

func TestTablesRetireTaskDocumentsAtTerminalSettlement(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	taskID := createWorkTask(t, ts.session, conversationID, true)
	commit(t, ts.session, func(tx *durable.Tx) error {
		pushLine(tx, StepDoc.Task(taskID).Member("committed", nil), "x")
		return nil
	})
	published := ts.publicationCount()
	commit(t, ts.session, func(tx *durable.Tx) error {
		task := readTask(tx, taskID)
		pushLine(tx, StepDoc.Task(taskID).Member("new", nil), "created then retired")
		return tx.SetTask(terminal(task))
	})
	writes := ts.storage.lastCommit()
	create, _ := findWrite[durable.DocumentCreateWrite](writes)
	var retired []durable.DocumentID
	for _, w := range writes {
		if r, ok := w.(durable.DocumentRetireWrite); ok {
			retired = append(retired, r.ID)
		}
	}
	if len(writes) != 5 || countWrites[durable.TaskWrite](writes) != 1 || countWrites[durable.DocumentCreateWrite](writes) != 1 || len(retired) != 3 {
		t.Fatalf("writes = %+v", writes)
	}
	found := false
	for _, id := range retired {
		found = found || id == create.Record.ID
	}
	if !found {
		t.Fatal("the document created in the commit was not retired")
	}
	if ts.publicationCount() != published+1 {
		t.Fatal("not one publication")
	}
	publication := ts.lastPublication()
	tasks := 0
	for _, c := range publication.Changes {
		if _, ok := c.(durable.TaskWrite); ok {
			tasks++
		}
	}
	documents := documentChanges(publication)
	if tasks != 1 || len(documents) != 3 {
		t.Fatalf("publication = %+v", publication)
	}
	for _, d := range documents {
		if d.Value != nil || len(d.Ops) != 0 || d.ConversationID != conversationID {
			t.Fatalf("document = %+v", d)
		}
	}
	if snapshot(t, ts.session, ProgressDoc.Task(taskID)) != nil || snapshot(t, ts.session, StepDoc.Task(taskID).Member("committed", nil)) != nil {
		t.Fatal("a retired task document has a snapshot")
	}
	alive, err := ts.storage.ScanDocuments(bg, durable.DocumentQuery{Scope: durable.TaskScope(taskID), At: durable.Current}, 10, nil)
	must(err)
	if len(alive.Items) != 0 {
		t.Fatalf("alive = %+v", alive.Items)
	}
	err = ts.session.Commit(bg, func(tx *durable.Tx) error { return tx.SetTask(terminal(readTask(tx, taskID))) })
	errorContains(t, err, fmt.Sprintf("Task %d is already terminal", taskID))
}

func TestTablesValidateDocumentOwners(t *testing.T) {
	ts := openTestSession(t)
	err := ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(ProgressDoc.Task(4242)); return err })
	errorContains(t, err, "Task 4242 does not exist")
	err = ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(NotesDoc.Conversation(4242)); return err })
	errorContains(t, err, "Conversation 4242 does not exist")
}
