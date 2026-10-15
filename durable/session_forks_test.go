package durable_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/session-forks.test.ts

func valueDoc(kind, history, fork, initial string) *durable.Doc {
	return durable.MustDefineDoc(durable.DocDef{
		Kind: kind, Version: 1, Scope: durable.ScopeConversation, History: history, Fork: fork,
		Initial: func(any) map[string]any { return map[string]any{"value": initial} },
	})
}

func valueFamily(kind, history, fork string) *durable.Doc {
	return durable.MustDefineDoc(durable.DocDef{
		Kind: kind, Version: 1, Family: true, Scope: durable.ScopeConversation, History: history, Fork: fork,
		Initial: func(seed any) map[string]any { return map[string]any{"value": seed} },
	})
}

func setDocValue(tx *durable.Tx, ref durable.DocRef, value any) {
	must(mustDoc(tx, ref).Set("value", value))
}

func expectJSON(t *testing.T, got any, want string) {
	t.Helper()
	if g := jsonOf(t, got); g != want {
		t.Fatalf("got %s, want %s", g, want)
	}
}

func TestForkCopiesAsOfAndCurrentDocumentsAndLeavesInitialOnesAbsent(t *testing.T) {
	asOf := valueDoc("fork.policies.as-of", durable.HistoryRewindable, durable.ForkAsOf, "as-initial")
	current := valueDoc("fork.policies.current", durable.HistoryLatest, durable.ForkCurrent, "current-initial")
	initial := valueDoc("fork.policies.initial", durable.HistoryLatest, durable.ForkInitial, "fresh")
	asOfFamily := valueFamily("fork.policies.as-of-family", durable.HistoryRewindable, durable.ForkAsOf)
	currentFamily := valueFamily("fork.policies.current-family", durable.HistoryLatest, durable.ForkCurrent)
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "fork-point")
		setDocValue(tx, asOf.Conversation(parentID), "as-at-fork")
		setDocValue(tx, current.Conversation(parentID), "current-at-fork")
		setDocValue(tx, initial.Conversation(parentID), "parent-only")
		setDocValue(tx, asOfFamily.Conversation(parentID).Member("a", "unused"), "family-as-a")
		setDocValue(tx, asOfFamily.Conversation(parentID).Member("b", "unused"), "family-as-b")
		setDocValue(tx, currentFamily.Conversation(parentID).Member("a", "unused"), "family-current-a")
		return nil
	})
	commit(t, ts.session, func(tx *durable.Tx) error {
		setDocValue(tx, asOf.Conversation(parentID), "as-after-fork")
		setDocValue(tx, current.Conversation(parentID), "current-when-copied")
		setDocValue(tx, asOfFamily.Conversation(parentID).Member("a", "unused"), "family-as-after")
		setDocValue(tx, currentFamily.Conversation(parentID).Member("a", "unused"), "family-current-when-copied")
		return nil
	})
	reads := ts.storage.documentReads()
	child := fork(t, ts.session, parentID, forkAt)
	if ts.storage.documentReads() != reads {
		t.Fatal("the fork read document content")
	}
	expectJSON(t, snapshot(t, ts.session, asOf.Conversation(child)), `{"value":"as-at-fork"}`)
	expectJSON(t, snapshot(t, ts.session, current.Conversation(child)), `{"value":"current-when-copied"}`)
	if snapshot(t, ts.session, initial.Conversation(child)) != nil {
		t.Fatal("an initial-policy document was copied")
	}
	expectJSON(t, snapshot(t, ts.session, asOfFamily.Conversation(child).Member("a", nil)), `{"value":"family-as-a"}`)
	expectJSON(t, snapshot(t, ts.session, asOfFamily.Conversation(child).Member("b", nil)), `{"value":"family-as-b"}`)
	expectJSON(t, snapshot(t, ts.session, currentFamily.Conversation(child).Member("a", nil)), `{"value":"family-current-when-copied"}`)

	var copies []durable.DocumentCopyWrite
	for _, w := range ts.storage.lastAdmitted() {
		if c, ok := w.(durable.DocumentCopyWrite); ok {
			copies = append(copies, c)
		}
	}
	if len(copies) != 5 {
		t.Fatalf("copies = %d", len(copies))
	}
	for _, c := range copies {
		if c.Record.Scope != durable.ConversationScope(child) {
			t.Fatalf("copy scope = %+v", c.Record.Scope)
		}
	}
	publication := ts.lastPublication()
	copied := documentCopyChanges(publication)
	if len(copied) != 5 {
		t.Fatalf("copy changes = %d", len(copied))
	}
	for _, change := range copied {
		if change.Record.CreatedAt != publication.Seq || change.ConversationID != child {
			t.Fatalf("change = %+v", change)
		}
		for _, c := range copies {
			if c.Record.ID == change.Record.ID && c.Source != change.Source {
				t.Fatalf("source %+v, want %+v", change.Source, c.Source)
			}
		}
	}
	parentRecord, err := ts.storage.FindDocument(bg, durable.DocumentAddress{Kind: "fork.policies.as-of", Scope: durable.ConversationScope(parentID)}, durable.Current)
	must(err)
	childRecord, err := ts.storage.FindDocument(bg, durable.DocumentAddress{Kind: "fork.policies.as-of", Scope: durable.ConversationScope(child)}, durable.Current)
	must(err)
	if childRecord.ID == parentRecord.ID {
		t.Fatal("the copy shares the source's incarnation")
	}
	commit(t, ts.session, func(tx *durable.Tx) error { setDocValue(tx, asOf.Conversation(child), "child-independent"); return nil })
	expectJSON(t, snapshot(t, ts.session, asOf.Conversation(parentID)), `{"value":"as-after-fork"}`)
	expectJSON(t, snapshot(t, ts.session, asOf.Conversation(child)), `{"value":"child-independent"}`)
	commit(t, ts.session, func(tx *durable.Tx) error { setDocValue(tx, initial.Conversation(child), "child-created"); return nil })
	expectJSON(t, snapshot(t, ts.session, initial.Conversation(child)), `{"value":"child-created"}`)
}

func TestForkUsesTheFinalStateOfTheForkEntryCommit(t *testing.T) {
	doc := valueDoc("fork.same-commit", durable.HistoryRewindable, durable.ForkAsOf, "initial")
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt, excluded durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "included")
		setDocValue(tx, doc.Conversation(parentID), "final-state-of-commit")
		excluded = appendEntry(tx, parentID, "excluded")
		return nil
	})
	child := fork(t, ts.session, parentID, forkAt)
	expectJSON(t, snapshot(t, ts.session, doc.Conversation(child)), `{"value":"final-state-of-commit"}`)
	visible, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.Page[durable.EntryRecord], error) {
		return tx.ScanEntries(durable.EntryQuery{ConversationID: child}, 10, nil)
	})
	must(err)
	ids := map[durable.EntryID]bool{}
	for _, e := range visible.Items {
		ids[e.ID] = true
	}
	if !ids[forkAt] || ids[excluded] {
		t.Fatalf("visible = %v", ids)
	}
}

func TestForkSelectsTheEntryOwnerForAsOfAndTheParentForCurrent(t *testing.T) {
	asOf := valueDoc("fork.ancestry.as-of", durable.HistoryRewindable, durable.ForkAsOf, "initial")
	current := valueDoc("fork.ancestry.current", durable.HistoryLatest, durable.ForkCurrent, "initial")
	ts := openTestSession(t)
	rootID := createConversation(t, ts.session)
	var inherited durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		inherited = appendEntry(tx, rootID, "root")
		setDocValue(tx, asOf.Conversation(rootID), "root-at-entry")
		setDocValue(tx, current.Conversation(rootID), "root-current")
		return nil
	})
	parent := fork(t, ts.session, rootID, inherited)
	var parentEntry durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		parentEntry = appendEntry(tx, parent, "parent")
		setDocValue(tx, asOf.Conversation(parent), "parent-at-own-entry")
		setDocValue(tx, current.Conversation(parent), "parent-current")
		return nil
	})
	inheritedFork := fork(t, ts.session, parent, inherited)
	expectJSON(t, snapshot(t, ts.session, asOf.Conversation(inheritedFork)), `{"value":"root-at-entry"}`)
	expectJSON(t, snapshot(t, ts.session, current.Conversation(inheritedFork)), `{"value":"parent-current"}`)
	ownEntryFork := fork(t, ts.session, parent, parentEntry)
	expectJSON(t, snapshot(t, ts.session, asOf.Conversation(ownEntryFork)), `{"value":"parent-at-own-entry"}`)
	expectJSON(t, snapshot(t, ts.session, current.Conversation(ownEntryFork)), `{"value":"parent-current"}`)
}

func TestForkCopiesTheStoredValueAndVersionWithoutMigrating(t *testing.T) {
	v1 := durable.MustDefineDoc(durable.DocDef{
		Kind: "fork.stored-version", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf,
		Initial: func(any) map[string]any { return map[string]any{"count": 1} },
	})
	var migrations atomic.Int64
	v3 := durable.MustDefineDoc(durable.DocDef{
		Kind: "fork.stored-version", Version: 3, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf,
		Initial: func(any) map[string]any { return map[string]any{"count": 0, "migrated": false} },
		Migrate: func(value map[string]any, from int) map[string]any {
			if from != 1 {
				panic(fmt.Sprintf("migrated from %d", from))
			}
			migrations.Add(1)
			return map[string]any{"count": value["count"], "migrated": true}
		},
	})
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "point")
		_, err := tx.Doc(v1.Conversation(parentID))
		return err
	})
	must(ts.session.UnloadDocuments())
	expectJSON(t, snapshot(t, ts.session, v3.Conversation(parentID)), `{"count":1,"migrated":true}`)
	if migrations.Load() != 1 {
		t.Fatalf("migrations = %d", migrations.Load())
	}
	child := fork(t, ts.session, parentID, forkAt)
	if migrations.Load() != 1 {
		t.Fatal("the fork migrated")
	}
	record, err := ts.storage.FindDocument(bg, durable.DocumentAddress{Kind: "fork.stored-version", Scope: durable.ConversationScope(child)}, durable.Current)
	must(err)
	stored, err := ts.storage.Document(bg, record.ID, durable.Current)
	must(err)
	if stored.Version != 1 || jsonOf(t, stored.Value) != `{"count":1}` {
		t.Fatalf("stored = %+v", stored)
	}
	expectJSON(t, snapshot(t, ts.session, v3.Conversation(child)), `{"count":1,"migrated":true}`)
	if migrations.Load() != 2 {
		t.Fatalf("migrations = %d", migrations.Load())
	}
}

func TestForkCoalescesAMigrationAndAnOverrideIntoTheCopiedBase(t *testing.T) {
	v1 := durable.MustDefineDoc(durable.DocDef{
		Kind: "fork.override", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf,
		Initial: func(any) map[string]any { return map[string]any{"count": 2} },
	})
	var checkpoints atomic.Int64
	v2 := durable.MustDefineDoc(durable.DocDef{
		Kind: "fork.override", Version: 2, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf,
		Initial: func(any) map[string]any { return map[string]any{"count": 0, "migrated": false} },
		Migrate: func(value map[string]any, _ int) map[string]any {
			return map[string]any{"count": value["count"], "migrated": true}
		},
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { checkpoints.Add(1); return false },
	})
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "point")
		_, err := tx.Doc(v1.Conversation(parentID))
		return err
	})
	reads := ts.storage.documentReads()
	child, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.ConversationID, error) {
		created, err := tx.ForkConversation(parentID, forkAt, durable.Ownerless)
		must(err)
		return created.ID, mustDoc(tx, v2.Conversation(created.ID)).Set("count", 9)
	})
	must(err)
	if ts.storage.documentReads() != reads+1 {
		t.Fatalf("reads %d→%d", reads, ts.storage.documentReads())
	}
	creates := 0
	for _, w := range ts.storage.lastAdmitted() {
		if c, ok := w.(durable.DocumentCreateWrite); ok {
			creates++
			if c.Content.Kind != durable.ContentBase || c.Content.Version != 2 || jsonOf(t, c.Content.Value) != `{"count":9,"migrated":true}` {
				t.Fatalf("content = %+v", c.Content)
			}
		}
	}
	if creates != 1 || checkpoints.Load() != 0 {
		t.Fatalf("creates %d, checkpoints %d", creates, checkpoints.Load())
	}
	if changes := documentChanges(ts.lastPublication()); len(changes) != 1 || changes[0].Version != 2 {
		t.Fatalf("changes = %+v", changes)
	}
	expectJSON(t, snapshot(t, ts.session, v2.Conversation(child)), `{"count":9,"migrated":true}`)
	parentRecord, err := ts.storage.FindDocument(bg, durable.DocumentAddress{Kind: "fork.override", Scope: durable.ConversationScope(parentID)}, durable.Current)
	must(err)
	if stored, _ := ts.storage.Document(bg, parentRecord.ID, durable.Current); stored.Version != 1 {
		t.Fatal("the source's version changed")
	}
}

func TestForkCopiesTheIncarnationAliveAtTheForkPoint(t *testing.T) {
	doc := valueDoc("fork.incarnations", durable.HistoryRewindable, durable.ForkAsOf, "initial")
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var oldAt, retiredAt, newAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		oldAt = appendEntry(tx, parentID, "old")
		setDocValue(tx, doc.Conversation(parentID), "old")
		return nil
	})
	commit(t, ts.session, func(tx *durable.Tx) error {
		retiredAt = appendEntry(tx, parentID, "retired")
		return tx.RetireDoc(doc.Conversation(parentID))
	})
	commit(t, ts.session, func(tx *durable.Tx) error {
		newAt = appendEntry(tx, parentID, "new")
		setDocValue(tx, doc.Conversation(parentID), "new")
		return nil
	})
	oldChild := fork(t, ts.session, parentID, oldAt)
	emptyChild := fork(t, ts.session, parentID, retiredAt)
	newChild := fork(t, ts.session, parentID, newAt)
	expectJSON(t, snapshot(t, ts.session, doc.Conversation(oldChild)), `{"value":"old"}`)
	if snapshot(t, ts.session, doc.Conversation(emptyChild)) != nil {
		t.Fatal("a retired incarnation was copied")
	}
	expectJSON(t, snapshot(t, ts.session, doc.Conversation(newChild)), `{"value":"new"}`)
}

func TestForkRejectsAnInvisibleForkPointBeforeAdmission(t *testing.T) {
	ts := openTestSession(t)
	rootID := createConversation(t, ts.session)
	var visible, hidden durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		visible = appendEntry(tx, rootID, "visible")
		hidden = appendEntry(tx, rootID, "hidden")
		return nil
	})
	parent := fork(t, ts.session, rootID, visible)
	commits, published := ts.storage.commitCount(), ts.publicationCount()
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.ForkConversation(parent, hidden, durable.Ownerless)
		return err
	})
	errorContains(t, err, fmt.Sprintf("Entry %d is not visible", hidden))
	if ts.storage.commitCount() != commits || ts.publicationCount() != published {
		t.Fatal("a rejected fork wrote or published")
	}
	independent := createConversation(t, ts.session)
	if record, _ := ts.storage.Conversation(bg, independent); record == nil || record.ID != independent {
		t.Fatal("the Session is not usable")
	}
}

func TestForkRejectsDuplicateSelectionsForOneChildAddress(t *testing.T) {
	asOf := valueDoc("fork.duplicate-policy", durable.HistoryRewindable, durable.ForkAsOf, "as-of")
	current := valueDoc("fork.duplicate-policy", durable.HistoryLatest, durable.ForkCurrent, "current")
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var oldAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		oldAt = appendEntry(tx, parentID, "old")
		_, err := tx.Doc(asOf.Conversation(parentID))
		return err
	})
	commit(t, ts.session, func(tx *durable.Tx) error { return tx.RetireDoc(asOf.Conversation(parentID)) })
	commit(t, ts.session, func(tx *durable.Tx) error { _, err := tx.Doc(current.Conversation(parentID)); return err })
	commits := ts.storage.commitCount()
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.ForkConversation(parentID, oldAt, durable.Ownerless)
		return err
	})
	errorContains(t, err, "Fork selects multiple source documents")
	if ts.storage.commitCount() != commits {
		t.Fatal("a rejected fork wrote")
	}
	createConversation(t, ts.session)
}

func TestForkRejectsSourceWritesInTheForkTransaction(t *testing.T) {
	current := valueDoc("fork.same-transaction-current", durable.HistoryLatest, durable.ForkCurrent, "committed")
	asOf := valueDoc("fork.same-transaction-as-of", durable.HistoryRewindable, durable.ForkAsOf, "committed")
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "point")
		mustDoc(tx, current.Conversation(parentID))
		mustDoc(tx, asOf.Conversation(parentID))
		return nil
	})
	commits := ts.storage.commitCount()
	forkTx := func(tx *durable.Tx) { _, err := tx.ForkConversation(parentID, forkAt, durable.Ownerless); must(err) }
	for name, change := range map[string]func(tx *durable.Tx){
		"current before the fork": func(tx *durable.Tx) { setDocValue(tx, current.Conversation(parentID), "before-fork"); forkTx(tx) },
		"current after the fork":  func(tx *durable.Tx) { forkTx(tx); setDocValue(tx, current.Conversation(parentID), "after-fork") },
		"as-of before the fork":   func(tx *durable.Tx) { setDocValue(tx, asOf.Conversation(parentID), "as-of-write"); forkTx(tx) },
	} {
		err := ts.session.Commit(bg, func(tx *durable.Tx) error { change(tx); return nil })
		if err == nil {
			t.Fatalf("%s: committed", name)
		}
		errorContains(t, err, "Cannot change fork source document")
	}
	if ts.storage.commitCount() != commits {
		t.Fatal("a rejected fork wrote")
	}
	expectJSON(t, snapshot(t, ts.session, current.Conversation(parentID)), `{"value":"committed"}`)
	child := fork(t, ts.session, parentID, forkAt)
	expectJSON(t, snapshot(t, ts.session, current.Conversation(child)), `{"value":"committed"}`)
}

func TestForkRollsCopiedBasesBackWhenAssemblyFails(t *testing.T) {
	copied := valueDoc("fork.rollback.copied", durable.HistoryRewindable, durable.ForkAsOf, "copied")
	var failCheckpoint atomic.Bool
	failCheckpoint.Store(true)
	failure := durable.MustDefineDoc(durable.DocDef{
		Kind: "fork.rollback.failure", Version: 1, Scope: durable.ScopeSession,
		Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool {
			if failCheckpoint.Load() {
				panic(errors.New("checkpoint failed"))
			}
			return false
		},
	})
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "point")
		mustDoc(tx, copied.Conversation(parentID))
		mustDoc(tx, failure.Session())
		return nil
	})
	commits, published := ts.storage.commitCount(), ts.publicationCount()
	var childID durable.ConversationID
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		c, err := tx.ForkConversation(parentID, forkAt, durable.Ownerless)
		must(err)
		childID = c.ID
		return mustDoc(tx, failure.Session()).Set("count", 1)
	})
	errorContains(t, err, "checkpoint failed")
	if ts.storage.commitCount() != commits || ts.publicationCount() != published {
		t.Fatal("a failed assembly wrote or published")
	}
	if record, _ := ts.storage.Conversation(bg, childID); record != nil {
		t.Fatal("the rejected child exists")
	}
	failCheckpoint.Store(false)
	commit(t, ts.session, func(tx *durable.Tx) error { return mustDoc(tx, failure.Session()).Set("count", 2) })
	expectJSON(t, snapshot(t, ts.session, failure.Session()), `{"count":2}`)
	expectJSON(t, snapshot(t, ts.session, copied.Conversation(parentID)), `{"value":"copied"}`)
}

func TestForkRejectedByStorageFailsTheSessionWithoutEffect(t *testing.T) {
	doc := valueDoc("fork.storage-rejected", durable.HistoryRewindable, durable.ForkAsOf, "source")
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "point")
		mustDoc(tx, doc.Conversation(parentID))
		return nil
	})
	rejection := errors.New("copy rejected")
	ts.storage.failNextCommit(rejection)
	var rejectedChild durable.ConversationID
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		c, err := tx.ForkConversation(parentID, forkAt, durable.Ownerless)
		rejectedChild = c.ID
		return err
	})
	if !errors.Is(err, rejection) {
		t.Fatalf("error = %v", err)
	}
	err = ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.CreateConversation(durable.Ownerless); return err })
	var failed *durable.SessionFailedError
	if !errors.As(err, &failed) || failed.Cause != rejection {
		t.Fatalf("error = %v", err)
	}
	// The failed Session closed its Storage; reopened, it holds nothing of
	// the batch.
	<-ts.session.Closed()
	if end := ts.session.End(); end.Reason != durable.SessionFailedReason || end.Err != rejection {
		t.Fatalf("end = %+v", end)
	}
	if record, _ := ts.storage.reopen().Conversation(bg, rejectedChild); record != nil {
		t.Fatal("the rejected child exists")
	}
}

func TestForkRetiresACopiedDocumentAndRecreatesTheAddress(t *testing.T) {
	doc := valueDoc("fork.retire-recreate", durable.HistoryRewindable, durable.ForkAsOf, "fresh")
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "point")
		setDocValue(tx, doc.Conversation(parentID), "copied")
		return nil
	})
	child, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.ConversationID, error) {
		c, err := tx.ForkConversation(parentID, forkAt, durable.Ownerless)
		must(err)
		must(tx.RetireDoc(doc.Conversation(c.ID)))
		setDocValue(tx, doc.Conversation(c.ID), "replacement")
		return c.ID, nil
	})
	must(err)
	writes := ts.storage.lastCommit()
	if countWrites[durable.DocumentCopyWrite](writes) != 1 || countWrites[durable.DocumentCreateWrite](writes) != 1 || countWrites[durable.DocumentRetireWrite](writes) != 1 {
		t.Fatalf("writes = %+v", writes)
	}
	expectJSON(t, snapshot(t, ts.session, doc.Conversation(child)), `{"value":"replacement"}`)
}

func TestForkCopiesOnlyConversationDocuments(t *testing.T) {
	copied := valueDoc("fork.scope.conversation", durable.HistoryLatest, durable.ForkCurrent, "conversation")
	sessionOnly := durable.MustDefineDoc(durable.DocDef{
		Kind: "fork.scope.session", Version: 1, Scope: durable.ScopeSession,
		Initial: func(any) map[string]any { return map[string]any{"value": "session"} },
	})
	taskOnly := durable.MustDefineDoc(durable.DocDef{
		Kind: "fork.scope.task", Version: 1, Scope: durable.ScopeTask,
		Initial: func(any) map[string]any { return map[string]any{"value": "task"} },
	})
	work := testTask{name: "fork.scope.work", version: 1, checkpoint: `{"phase":"start"}`}
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	var forkAt durable.EntryID
	var taskID durable.TaskID
	commit(t, ts.session, func(tx *durable.Tx) error {
		forkAt = appendEntry(tx, parentID, "point")
		var err error
		taskID, err = tx.CreateTask(work, []byte("null"), ownedByConversation(parentID))
		must(err)
		mustDoc(tx, copied.Conversation(parentID))
		mustDoc(tx, sessionOnly.Session())
		mustDoc(tx, taskOnly.Task(taskID))
		return nil
	})
	child := fork(t, ts.session, parentID, forkAt)
	var kinds []string
	for _, w := range ts.storage.lastCommit() {
		if c, ok := w.(durable.DocumentCopyWrite); ok {
			kinds = append(kinds, c.Record.Kind)
		}
	}
	changes := documentCopyChanges(ts.lastPublication())
	if len(kinds) != 1 || kinds[0] != "fork.scope.conversation" || len(changes) != 1 || changes[0].Record.Kind != "fork.scope.conversation" {
		t.Fatalf("copies %v, changes %+v", kinds, changes)
	}
	expectJSON(t, snapshot(t, ts.session, copied.Conversation(child)), `{"value":"conversation"}`)
	expectJSON(t, snapshot(t, ts.session, sessionOnly.Session()), `{"value":"session"}`)
	expectJSON(t, snapshot(t, ts.session, taskOnly.Task(taskID)), `{"value":"task"}`)
}

func TestForkCopiesEveryFamilyMemberAcrossScanPages(t *testing.T) {
	family := durable.MustDefineDoc(durable.DocDef{
		Kind: "fork.pagination", Version: 1, Family: true, Scope: durable.ScopeConversation, History: durable.HistoryLatest, Fork: durable.ForkCurrent,
		Initial: func(seed any) map[string]any { return map[string]any{"value": seed} },
	})
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	forkAt, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.EntryID, error) {
		return appendEntry(tx, parentID, "point"), nil
	})
	must(err)
	commit(t, ts.session, func(tx *durable.Tx) error {
		var wg sync.WaitGroup
		for index := range 260 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				mustDoc(tx, family.Conversation(parentID).Member(fmt.Sprintf("member-%d", index), index))
			}()
		}
		wg.Wait()
		return nil
	})
	child := fork(t, ts.session, parentID, forkAt)
	if n := countWrites[durable.DocumentCopyWrite](ts.storage.lastCommit()); n != 260 {
		t.Fatalf("copies = %d", n)
	}
	expectJSON(t, snapshot(t, ts.session, family.Conversation(child).Member("member-0", nil)), `{"value":0}`)
	expectJSON(t, snapshot(t, ts.session, family.Conversation(child).Member("member-259", nil)), `{"value":259}`)
}
