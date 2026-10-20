package durable_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/session-checkpoints-migrations.test.ts

func sessionDoc(def durable.DocDef) *durable.Doc {
	def.Scope = durable.ScopeSession
	if def.Version == 0 {
		def.Version = 1
	}
	// pi's tests write `checkpointWhen: () => false` where no policy is under
	// test.
	if def.CheckpointWhen == nil {
		def.CheckpointWhen = durable.NeverCheckpoint
	}
	return durable.MustDefineDoc(def)
}

func constant(v map[string]any) func(any) map[string]any {
	return func(any) map[string]any { return v }
}

func documentWrites(writes []durable.StorageWrite) []durable.StorageWrite {
	var out []durable.StorageWrite
	for _, w := range writes {
		switch w.(type) {
		case durable.DocumentCreateWrite, durable.DocumentChangeWrite, durable.DocumentRetireWrite:
			out = append(out, w)
		}
	}
	return out
}

func changeKinds(writes []durable.StorageWrite) []string {
	var kinds []string
	for _, w := range writes {
		if c, ok := w.(durable.DocumentChangeWrite); ok {
			kinds = append(kinds, c.Content.Kind)
		}
	}
	return kinds
}

func touch(t *testing.T, s *durable.Session, ref durable.DocRef) {
	t.Helper()
	commit(t, s, func(tx *durable.Tx) error { _, err := tx.Doc(ref); return err })
}

func increment(tx *durable.Tx, ref durable.DocRef, key string) error {
	d := mustDoc(tx, ref)
	v, _ := d.Get(key)
	return d.Set(key, v.(float64)+1)
}

func TestCheckpointPredicateSeesTheChangedValue(t *testing.T) {
	// pi calls checkpointWhen with its definition as the receiver; Go has no
	// receiver, so the predicate closes over what it needs.
	initial := func(any) map[string]any { return map[string]any{"count": 0} }
	doc := sessionDoc(durable.DocDef{
		Kind: "checkpoint.receiver", Initial: initial,
		CheckpointWhen: func(value map[string]any, _ []delta.Op, _ durable.CheckpointInfo) bool {
			return value["count"] == float64(initial(nil)["count"].(int)+2)
		},
	})
	ts := openTestSession(t)
	for count := 0; count <= 2; count++ {
		commit(t, ts.session, func(tx *durable.Tx) error { return mustDoc(tx, doc.Session()).Set("count", count) })
	}
	var kinds []string
	ts.storage.mu.Lock()
	for _, writes := range ts.storage.commits {
		kinds = append(kinds, changeKinds(writes)...)
	}
	ts.storage.mu.Unlock()
	if !reflect.DeepEqual(kinds, []string{"delta", "base"}) {
		t.Fatalf("kinds = %v", kinds)
	}
}

type predicateCall struct {
	value map[string]any
	ops   []delta.Op
}

func TestCheckpointBasesOnlyForNonemptyOrdinaryBatches(t *testing.T) {
	var mu sync.Mutex
	var calls, falseCalls []predicateCall
	baseDoc := sessionDoc(durable.DocDef{
		Kind: "checkpoint.base", Initial: func(any) map[string]any { return map[string]any{"items": []any{}} },
		CheckpointWhen: func(value map[string]any, ops []delta.Op, _ durable.CheckpointInfo) bool {
			mu.Lock()
			calls = append(calls, predicateCall{value, ops})
			mu.Unlock()
			return true
		},
	})
	deltaDoc := sessionDoc(durable.DocDef{
		Kind: "checkpoint.delta", Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		CheckpointWhen: func(value map[string]any, ops []delta.Op, _ durable.CheckpointInfo) bool {
			mu.Lock()
			falseCalls = append(falseCalls, predicateCall{value, ops})
			mu.Unlock()
			return false
		},
	})
	defaultDoc := sessionDoc(durable.DocDef{Kind: "checkpoint.default", Initial: func(any) map[string]any { return map[string]any{"count": 0} }})
	ts := openTestSession(t)
	commit(t, ts.session, func(tx *durable.Tx) error {
		mustDoc(tx, baseDoc.Session())
		mustDoc(tx, deltaDoc.Session())
		mustDoc(tx, defaultDoc.Session())
		return nil
	})
	if len(calls) != 0 || len(falseCalls) != 0 {
		t.Fatal("the predicate ran for creations")
	}
	for _, w := range documentWrites(ts.storage.lastCommit()) {
		if c, ok := w.(durable.DocumentCreateWrite); !ok || c.Content.Kind != durable.ContentBase {
			t.Fatalf("write = %+v", w)
		}
	}
	commit(t, ts.session, func(tx *durable.Tx) error {
		_, err := mustDoc(tx, baseDoc.Session()).At("items").Push("x")
		must(err)
		must(increment(tx, deltaDoc.Session(), "count"))
		return increment(tx, defaultDoc.Session(), "count")
	})
	var changes []durable.DocumentChangeWrite
	for _, w := range ts.storage.lastAdmitted() {
		if c, ok := w.(durable.DocumentChangeWrite); ok {
			changes = append(changes, c)
		}
	}
	if len(changes) != 3 || changes[0].Content.Kind != "base" || changes[1].Content.Kind != "delta" || changes[2].Content.Kind != "delta" {
		t.Fatalf("changes = %+v", changes)
	}
	if len(calls) != 1 || len(falseCalls) != 1 {
		t.Fatalf("calls %d, false calls %d", len(calls), len(falseCalls))
	}
	snap := snapshot(t, ts.session, baseDoc.Session())
	deltaSnap := snapshot(t, ts.session, deltaDoc.Session())
	published := documentChanges(ts.lastPublication())
	if !same(calls[0].value, snap) || !same(calls[0].ops, published[0].Ops) ||
		!same(falseCalls[0].value, deltaSnap) || !same(falseCalls[0].ops, published[1].Ops) {
		t.Fatal("the predicate did not get the exact prepared revision and ops")
	}
	if !same(changes[1].Content.Ops, falseCalls[0].ops) || !same(changes[0].Content.Value, snap) {
		t.Fatal("the writes do not share the prepared revision and ops")
	}
}

func TestCheckpointPassesTheStoredDeltaCountSinceTheNewestBase(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	record := func(info durable.CheckpointInfo) {
		mu.Lock()
		seen = append(seen, info.DeltasSinceBase)
		mu.Unlock()
	}
	v1 := sessionDoc(durable.DocDef{
		Kind: "checkpoint.deltas-since-base", Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		CheckpointWhen: func(_ map[string]any, _ []delta.Op, info durable.CheckpointInfo) bool {
			record(info)
			return info.DeltasSinceBase >= 2
		},
	})
	v2 := sessionDoc(durable.DocDef{
		Kind: "checkpoint.deltas-since-base", Version: 2, Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		Migrate: func(value map[string]any, _ int) map[string]any { return map[string]any{"count": value["count"]} },
		CheckpointWhen: func(_ map[string]any, _ []delta.Op, info durable.CheckpointInfo) bool {
			record(info)
			return false
		},
	})
	ts := openTestSession(t)
	inc := func(doc *durable.Doc) {
		commit(t, ts.session, func(tx *durable.Tx) error { return increment(tx, doc.Session(), "count") })
	}
	touch(t, ts.session, v1.Session())
	inc(v1)
	inc(v1)
	inc(v1)
	must(ts.session.UnloadDocuments())
	inc(v1)
	if !reflect.DeepEqual(seen, []int{0, 1, 2, 0}) {
		t.Fatalf("seen = %v", seen)
	}
	ts.storage.mu.Lock()
	last := ts.storage.commits[len(ts.storage.commits)-4:]
	ts.storage.mu.Unlock()
	var kinds []string
	for _, writes := range last {
		kinds = append(kinds, writes[0].(durable.DocumentChangeWrite).Content.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"delta", "delta", "base", "delta"}) {
		t.Fatalf("kinds = %v", kinds)
	}
	// A required version base resets the count without calling the
	// predicate.
	inc(v2)
	inc(v2)
	if !reflect.DeepEqual(seen, []int{0, 1, 2, 0, 0}) {
		t.Fatalf("seen = %v", seen)
	}
	ts.storage.mu.Lock()
	versionBase := ts.storage.commits[len(ts.storage.commits)-2][0].(durable.DocumentChangeWrite)
	ts.storage.mu.Unlock()
	if versionBase.Content.Kind != durable.ContentBase || versionBase.Content.Version != 2 {
		t.Fatalf("version base = %+v", versionBase)
	}
}

func TestCheckpointSkipsEmptyBatchesButNotStructuralNoOps(t *testing.T) {
	var calls atomic.Int64
	doc := sessionDoc(durable.DocDef{
		Kind: "checkpoint.no-op", Initial: func(any) map[string]any { return map[string]any{"items": []any{"a", "b"}} },
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { calls.Add(1); return false },
	})
	ts := openTestSession(t)
	touch(t, ts.session, doc.Session())
	commits := ts.storage.commitCount()
	commit(t, ts.session, func(tx *durable.Tx) error {
		items := mustDoc(tx, doc.Session()).At("items")
		_, err := items.Push("x")
		must(err)
		_, _, err = items.Pop()
		return err
	})
	if calls.Load() != 0 || ts.storage.commitCount() != commits {
		t.Fatal("an empty batch reached the predicate or Storage")
	}
	commit(t, ts.session, func(tx *durable.Tx) error {
		items := mustDoc(tx, doc.Session()).At("items")
		first, _, err := items.Shift()
		must(err)
		_, err = items.Unshift(first)
		return err
	})
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	if c, ok := ts.storage.lastCommit()[0].(durable.DocumentChangeWrite); !ok || c.Content.Kind != durable.ContentDelta {
		t.Fatalf("write = %+v", ts.storage.lastCommit()[0])
	}
}

func TestCheckpointPanicRollsBackEveryPreparedDocument(t *testing.T) {
	var throwCheckpoint atomic.Bool
	throwCheckpoint.Store(true)
	var firstCalls atomic.Int64
	first := sessionDoc(durable.DocDef{
		Kind: "checkpoint.rollback.first", Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { firstCalls.Add(1); return false },
	})
	second := sessionDoc(durable.DocDef{
		Kind: "checkpoint.rollback.second", Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool {
			if throwCheckpoint.Load() {
				panic(errors.New("checkpoint failed"))
			}
			return false
		},
	})
	ts := openTestSession(t)
	commit(t, ts.session, func(tx *durable.Tx) error { mustDoc(tx, first.Session()); mustDoc(tx, second.Session()); return nil })
	firstSnap, secondSnap := snapshot(t, ts.session, first.Session()), snapshot(t, ts.session, second.Session())
	commits, published := ts.storage.commitCount(), ts.publicationCount()
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		must(mustDoc(tx, first.Session()).Set("count", 1))
		return mustDoc(tx, second.Session()).Set("count", 2)
	})
	errorContains(t, err, "checkpoint failed")
	if firstCalls.Load() != 1 || ts.storage.commitCount() != commits || ts.publicationCount() != published {
		t.Fatal("a failed predicate wrote or published")
	}
	if !same(snapshot(t, ts.session, first.Session()), firstSnap) || !same(snapshot(t, ts.session, second.Session()), secondSnap) {
		t.Fatal("a failed predicate changed state")
	}
	throwCheckpoint.Store(false)
	commit(t, ts.session, func(tx *durable.Tx) error {
		must(mustDoc(tx, first.Session()).Set("count", 3))
		return mustDoc(tx, second.Session()).Set("count", 4)
	})
	expectJSON(t, snapshot(t, ts.session, first.Session()), `{"count":3}`)
	expectJSON(t, snapshot(t, ts.session, second.Session()), `{"count":4}`)
}

func TestCheckpointRepeatedFalseDecisionsReplayTheCompleteTail(t *testing.T) {
	var calls atomic.Int64
	doc := sessionDoc(durable.DocDef{
		Kind: "checkpoint.tail", Initial: func(any) map[string]any { return map[string]any{"values": []any{}} },
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { calls.Add(1); return false },
	})
	ts := openTestSession(t)
	touch(t, ts.session, doc.Session())
	for value := 1; value <= 8; value++ {
		commit(t, ts.session, func(tx *durable.Tx) error { _, err := mustDoc(tx, doc.Session()).At("values").Push(value); return err })
	}
	var kinds []string
	ts.storage.mu.Lock()
	for _, writes := range ts.storage.commits {
		kinds = append(kinds, changeKinds(writes)...)
	}
	ts.storage.mu.Unlock()
	if len(kinds) != 8 || calls.Load() != 8 {
		t.Fatalf("kinds %v, calls %d", kinds, calls.Load())
	}
	for _, k := range kinds {
		if k != durable.ContentDelta {
			t.Fatalf("kinds = %v", kinds)
		}
	}
	must(ts.session.UnloadDocuments())
	expectJSON(t, snapshot(t, ts.session, doc.Session()), `{"values":[1,2,3,4,5,6,7,8]}`)
}

func TestCheckpointKeepsARootReplacementAsADeltaWhenThePredicateIsFalse(t *testing.T) {
	initial := map[string]any{}
	for index := range 4100 {
		initial[fmt.Sprintf("field%d", index)] = 0
	}
	doc := sessionDoc(durable.DocDef{
		Kind: "checkpoint.root-replacement", Initial: constant(initial),
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { return false },
	})
	ts := openTestSession(t)
	touch(t, ts.session, doc.Session())
	commit(t, ts.session, func(tx *durable.Tx) error {
		value := mustDoc(tx, doc.Session())
		for index := range 4100 {
			must(value.Set(fmt.Sprintf("field%d", index), 1))
		}
		return nil
	})
	w, ok := ts.storage.lastCommit()[0].(durable.DocumentChangeWrite)
	if !ok || w.Content.Kind != durable.ContentDelta || len(w.Content.Ops) != 1 {
		t.Fatalf("write = %+v", ts.storage.lastCommit()[0])
	}
	if _, ok := w.Content.Ops[0].(delta.Replace); !ok {
		t.Fatalf("op = %T", w.Content.Ops[0])
	}
	must(ts.session.UnloadDocuments())
	if snapshot(t, ts.session, doc.Session())["field4099"] != 1.0 {
		t.Fatal("the replacement did not persist")
	}
}

func TestCheckpointSelectionRunsBeforeRetirement(t *testing.T) {
	var calls atomic.Int64
	doc := sessionDoc(durable.DocDef{
		Kind: "checkpoint.retire", Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { calls.Add(1); return true },
	})
	ts := openTestSession(t)
	touch(t, ts.session, doc.Session())
	commit(t, ts.session, func(tx *durable.Tx) error {
		must(mustDoc(tx, doc.Session()).Set("count", 1))
		return tx.RetireDoc(doc.Session())
	})
	writes := documentWrites(ts.storage.lastCommit())
	if calls.Load() != 1 || len(writes) != 2 {
		t.Fatalf("calls %d, writes %+v", calls.Load(), writes)
	}
	if c, ok := writes[0].(durable.DocumentChangeWrite); !ok || c.Content.Kind != durable.ContentBase {
		t.Fatalf("write = %+v", writes[0])
	}
	if _, ok := writes[1].(durable.DocumentRetireWrite); !ok {
		t.Fatalf("write = %+v", writes[1])
	}
}

func TestMigrationIsReadOnlyOncePerColdLoad(t *testing.T) {
	old := sessionDoc(durable.DocDef{Kind: "migration.read-only", Initial: func(any) map[string]any { return map[string]any{"count": 2} }})
	var calls atomic.Int64
	var retained map[string]any
	current := sessionDoc(durable.DocDef{
		Kind: "migration.read-only", Version: 3, Initial: func(any) map[string]any { return map[string]any{"count": 0, "labels": []any{}} },
		Migrate: func(value map[string]any, from int) map[string]any {
			if from != 1 {
				panic(fmt.Sprintf("migrated from %d", from))
			}
			calls.Add(1)
			retained = map[string]any{"count": value["count"], "labels": []any{"migrated"}}
			return retained
		},
	})
	ts := openTestSession(t)
	touch(t, ts.session, old.Session())
	must(ts.session.UnloadDocuments())
	commits := ts.storage.commitCount()
	first := snapshot(t, ts.session, current.Session())
	if !same(snapshot(t, ts.session, current.Session()), first) || calls.Load() != 1 || ts.storage.commitCount() != commits {
		t.Fatal("the migration ran twice or wrote")
	}
	retained["count"] = 99
	retained["labels"] = append(retained["labels"].([]any), "mutated")
	expectJSON(t, first, `{"count":2,"labels":["migrated"]}`)
	must(ts.session.UnloadDocuments())
	second := snapshot(t, ts.session, current.Session())
	if same(second, first) || !reflect.DeepEqual(second, first) || calls.Load() != 2 || ts.storage.commitCount() != commits {
		t.Fatal("the cold load did not migrate again without writing")
	}
}

func TestMigrationWritesTheRequiredBaseOnTheFirstTransaction(t *testing.T) {
	old := sessionDoc(durable.DocDef{Kind: "migration.transition", Initial: func(any) map[string]any { return map[string]any{"count": 4} }})
	var checkpoints atomic.Int64
	current := sessionDoc(durable.DocDef{
		Kind: "migration.transition", Version: 3, Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		Migrate: func(value map[string]any, from int) map[string]any {
			return map[string]any{"count": value["count"].(float64) + float64(from) - 1}
		},
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { checkpoints.Add(1); return false },
	})
	ts := openTestSession(t)
	touch(t, ts.session, old.Session())
	must(ts.session.UnloadDocuments())
	// An observer of the older shape.
	watch := watchDoc(t, ts.session, old.Session())
	frames := newRecorder(false)
	must(watch.Start(frames.listener))
	expectJSON(t, snapshot(t, ts.session, current.Session()), `{"count":4}`)
	snap := snapshot(t, ts.session, current.Session())
	// An observer of the new shape: the migration changes nothing it sees.
	currentWatch := watchDoc(t, ts.session, current.Session())
	currentFrames := newRecorder(false)
	must(currentWatch.Start(currentFrames.listener))

	touch(t, ts.session, current.Session())
	base, ok := ts.storage.lastCommit()[0].(durable.DocumentChangeWrite)
	if !ok || base.Content.Kind != durable.ContentBase || base.Content.Version != 3 || jsonOf(t, base.Content.Value) != `{"count":4}` {
		t.Fatalf("write = %+v", ts.storage.lastCommit()[0])
	}
	if checkpoints.Load() != 0 {
		t.Fatal("the predicate ran for a version base")
	}
	// The migration-only base is published, so the older-shape watch
	// receives the new value as a root replacement.
	if changes := documentChanges(ts.lastPublication()); len(changes) != 1 || changes[0].Version != 3 || jsonOf(t, changes[0].Value) != `{"count":4}` || len(changes[0].Ops) != 0 {
		t.Fatalf("changes = %+v", changes)
	}
	eventually(t, func() bool { return len(frames.all()) == 1 })
	settle()
	f := frames.all()
	if len(f) != 1 || jsonOf(t, f[0].value) != `{"count":4}` || jsonOf(t, f[0].ops) != `[["r",{"count":4}]]` || len(currentFrames.all()) != 0 {
		t.Fatalf("frames %+v, current frames %+v", f, currentFrames.all())
	}
	watch.Stop()
	currentWatch.Stop()
	if !same(snapshot(t, ts.session, current.Session()), snap) {
		t.Fatal("the version base replaced the value")
	}
	commit(t, ts.session, func(tx *durable.Tx) error { return mustDoc(tx, current.Session()).Set("count", 7) })
	if c, ok := ts.storage.lastCommit()[0].(durable.DocumentChangeWrite); !ok || c.Content.Kind != durable.ContentDelta || c.Content.Version != 3 {
		t.Fatalf("write = %+v", ts.storage.lastCommit()[0])
	}
	if checkpoints.Load() != 1 {
		t.Fatalf("checkpoints = %d", checkpoints.Load())
	}
	must(ts.session.UnloadDocuments())
	expectJSON(t, snapshot(t, ts.session, current.Session()), `{"count":7}`)
}

func TestMigrationRollsBackWithTheCallbackAndCoalescesLaterEdits(t *testing.T) {
	old := sessionDoc(durable.DocDef{Kind: "migration.rollback", Initial: func(any) map[string]any { return map[string]any{"count": 1} }})
	var migrations atomic.Int64
	current := sessionDoc(durable.DocDef{
		Kind: "migration.rollback", Version: 2, Initial: func(any) map[string]any { return map[string]any{"count": 0, "migrated": false} },
		Migrate: func(value map[string]any, _ int) map[string]any {
			migrations.Add(1)
			return map[string]any{"count": value["count"], "migrated": true}
		},
	})
	ts := openTestSession(t)
	touch(t, ts.session, old.Session())
	must(ts.session.UnloadDocuments())
	commits := ts.storage.commitCount()
	err := ts.session.Commit(bg, func(tx *durable.Tx) error {
		must(mustDoc(tx, current.Session()).Set("count", 8))
		return errors.New("rollback")
	})
	errorContains(t, err, "rollback")
	if ts.storage.commitCount() != commits || migrations.Load() != 1 {
		t.Fatal("the rolled-back migration wrote or ran twice")
	}
	expectJSON(t, snapshot(t, ts.session, current.Session()), `{"count":1,"migrated":true}`)
	commit(t, ts.session, func(tx *durable.Tx) error {
		value := mustDoc(tx, current.Session())
		must(value.Set("count", 9))
		return value.Set("migrated", false)
	})
	writes := documentWrites(ts.storage.lastCommit())
	if len(writes) != 1 {
		t.Fatalf("writes = %+v", writes)
	}
	if c, ok := writes[0].(durable.DocumentChangeWrite); !ok || c.Content.Kind != durable.ContentBase || c.Content.Version != 2 || jsonOf(t, c.Content.Value) != `{"count":9,"migrated":false}` {
		t.Fatalf("write = %+v", writes[0])
	}
	published := documentChanges(ts.lastPublication())[0]
	admitted := ts.storage.lastAdmitted()[0].(durable.DocumentChangeWrite)
	if !same(published.Value, snapshot(t, ts.session, current.Session())) || !same(published.Value, admitted.Content.Value) || len(published.Ops) == 0 || migrations.Load() != 1 {
		t.Fatal("the coalesced base is not the published revision")
	}
}

func TestMigrationResultsAreStrictCheckedAndTheSessionStaysUsable(t *testing.T) {
	old := sessionDoc(durable.DocDef{Kind: "migration.invalid", Initial: func(any) map[string]any { return map[string]any{"count": 1} }})
	invalid := sessionDoc(durable.DocDef{
		Kind: "migration.invalid", Version: 2, Initial: func(any) map[string]any { return map[string]any{} },
		Migrate: func(map[string]any, int) map[string]any { return map[string]any{"invalid": context.Background()} },
	})
	other := sessionDoc(durable.DocDef{Kind: "migration.invalid.other", Initial: func(any) map[string]any { return map[string]any{"ok": true} }})
	ts := openTestSession(t)
	touch(t, ts.session, old.Session())
	must(ts.session.UnloadDocuments())
	commits := ts.storage.commitCount()
	_, err := ts.session.Snapshot(bg, invalid.Session())
	errorContains(t, err, "strict JSON")
	err = ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(invalid.Session()); return err })
	errorContains(t, err, "strict JSON")
	if ts.storage.commitCount() != commits {
		t.Fatal("an invalid migration wrote")
	}
	touch(t, ts.session, other.Session())
	expectJSON(t, snapshot(t, ts.session, other.Session()), `{"ok":true}`)
}

func TestMigrationRejectsNewerVersionsAndOlderOnesWithoutMigration(t *testing.T) {
	v2 := sessionDoc(durable.DocDef{Kind: "migration.compatibility", Version: 2, Initial: func(any) map[string]any { return map[string]any{"count": 2} }})
	v1 := sessionDoc(durable.DocDef{Kind: "migration.compatibility", Initial: func(any) map[string]any { return map[string]any{"count": 1} }})
	v3 := sessionDoc(durable.DocDef{Kind: "migration.compatibility", Version: 3, Initial: func(any) map[string]any { return map[string]any{"count": 3} }})
	ts := openTestSession(t)
	touch(t, ts.session, v2.Session())
	must(ts.session.UnloadDocuments())
	commits := ts.storage.commitCount()
	for doc, want := range map[*durable.Doc]string{v1: "newer version 2 than 1", v3: "requires migration from version 2"} {
		_, err := ts.session.Snapshot(bg, doc.Session())
		errorContains(t, err, want)
		err = ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(doc.Session()); return err })
		errorContains(t, err, want)
	}
	if ts.storage.commitCount() != commits {
		t.Fatal("a rejected access wrote")
	}
}

func TestMigrationBaseBeforeRetirementSkipsThePredicate(t *testing.T) {
	old := sessionDoc(durable.DocDef{Kind: "migration.retire", Initial: func(any) map[string]any { return map[string]any{"count": 1} }})
	current := sessionDoc(durable.DocDef{
		Kind: "migration.retire", Version: 2, Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		Migrate:        func(value map[string]any, _ int) map[string]any { return map[string]any{"count": value["count"]} },
		CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { panic("must not run") },
	})
	ts := openTestSession(t)
	touch(t, ts.session, old.Session())
	must(ts.session.UnloadDocuments())
	commit(t, ts.session, func(tx *durable.Tx) error {
		mustDoc(tx, current.Session())
		return tx.RetireDoc(current.Session())
	})
	writes := documentWrites(ts.storage.lastCommit())
	if len(writes) != 2 {
		t.Fatalf("writes = %+v", writes)
	}
	if c, ok := writes[0].(durable.DocumentChangeWrite); !ok || c.Content.Kind != durable.ContentBase || c.Content.Version != 2 || jsonOf(t, c.Content.Value) != `{"count":1}` {
		t.Fatalf("write = %+v", writes[0])
	}
	if _, ok := writes[1].(durable.DocumentRetireWrite); !ok {
		t.Fatalf("write = %+v", writes[1])
	}
}

func TestMigrationLeavesUnaccessedDocumentsUntouched(t *testing.T) {
	firstV1 := sessionDoc(durable.DocDef{Kind: "migration.lazy.first", Initial: func(any) map[string]any { return map[string]any{"count": 1} }})
	secondV1 := sessionDoc(durable.DocDef{Kind: "migration.lazy.second", Initial: func(any) map[string]any { return map[string]any{"count": 2} }})
	var secondMigrations atomic.Int64
	firstV2 := sessionDoc(durable.DocDef{
		Kind: "migration.lazy.first", Version: 2, Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		Migrate: func(value map[string]any, _ int) map[string]any { return map[string]any{"count": value["count"]} },
	})
	sessionDoc(durable.DocDef{
		Kind: "migration.lazy.second", Version: 2, Initial: func(any) map[string]any { return map[string]any{"count": 0} },
		Migrate: func(value map[string]any, _ int) map[string]any {
			secondMigrations.Add(1)
			return map[string]any{"count": value["count"]}
		},
	})
	ts := openTestSession(t)
	commit(t, ts.session, func(tx *durable.Tx) error {
		mustDoc(tx, firstV1.Session())
		mustDoc(tx, secondV1.Session())
		return nil
	})
	must(ts.session.UnloadDocuments())
	commits := ts.storage.commitCount()
	expectJSON(t, snapshot(t, ts.session, firstV2.Session()), `{"count":1}`)
	if ts.storage.commitCount() != commits || secondMigrations.Load() != 0 {
		t.Fatal("an unaccessed document was migrated or written")
	}
	record, err := ts.storage.FindDocument(bg, durable.DocumentAddress{Kind: "migration.lazy.second", Scope: durable.SessionScope()}, durable.Current)
	must(err)
	if stored, _ := ts.storage.Document(bg, record.ID, durable.Current); stored.Version != 1 {
		t.Fatal("the unaccessed document's version changed")
	}
}

func TestHistoryMigratesCurrentAndHistoricalValuesAndFollowsForks(t *testing.T) {
	rewindable := func(kind string, version int, family bool, initial func(any) map[string]any, migrate func(map[string]any, int) map[string]any) *durable.Doc {
		return durable.MustDefineDoc(durable.DocDef{
			CheckpointWhen: durable.NeverCheckpoint,
			Kind:           kind, Version: version, Family: family, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf,
			Initial: initial, Migrate: migrate,
		})
	}
	v1 := rewindable("history.migration", 1, false, func(any) map[string]any { return map[string]any{"count": 0} }, nil)
	family := rewindable("history.family", 1, true, func(seed any) map[string]any { return map[string]any{"seed": seed, "count": 0} }, nil)
	var mu sync.Mutex
	var migrations []int
	v3 := rewindable("history.migration", 3, false, func(any) map[string]any { return map[string]any{"count": 0, "version": 3} },
		func(value map[string]any, from int) map[string]any {
			mu.Lock()
			migrations = append(migrations, from)
			mu.Unlock()
			return map[string]any{"count": value["count"], "version": 3}
		})
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	var firstEntry, secondEntry, thirdEntry durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		firstEntry = appendEntry(tx, conversationID, "first")
		must(mustDoc(tx, v1.Conversation(conversationID)).Set("count", 1))
		return mustDoc(tx, family.Conversation(conversationID).Member("member", "seed")).Set("count", 1)
	})
	commit(t, ts.session, func(tx *durable.Tx) error {
		secondEntry = appendEntry(tx, conversationID, "second")
		return mustDoc(tx, v1.Conversation(conversationID)).Set("count", 2)
	})
	must(ts.session.UnloadDocuments())
	commits := ts.storage.commitCount()
	expectJSON(t, snapshot(t, ts.session, v3.Conversation(conversationID)), `{"count":2,"version":3}`)
	if !reflect.DeepEqual(migrations, []int{1}) || ts.storage.commitCount() != commits {
		t.Fatalf("migrations = %v", migrations)
	}
	commit(t, ts.session, func(tx *durable.Tx) error {
		thirdEntry = appendEntry(tx, conversationID, "third")
		_, err := tx.Doc(v3.Conversation(conversationID))
		return err
	})
	if c, ok := findWrite[durable.DocumentChangeWrite](ts.storage.lastCommit()); !ok || c.Content.Kind != durable.ContentBase || c.Content.Version != 3 {
		t.Fatalf("writes = %+v", ts.storage.lastCommit())
	}
	asOf := func(doc durable.DocRef, conversation durable.ConversationID, at durable.EntryID) map[string]any {
		_ = conversation
		v, err := ts.session.SnapshotAsOf(bg, doc, at)
		must(err)
		return v
	}
	expectJSON(t, asOf(v3.Conversation(conversationID), conversationID, firstEntry), `{"count":1,"version":3}`)
	expectJSON(t, asOf(v3.Conversation(conversationID), conversationID, secondEntry), `{"count":2,"version":3}`)
	expectJSON(t, asOf(v3.Conversation(conversationID), conversationID, thirdEntry), `{"count":2,"version":3}`)
	if !reflect.DeepEqual(migrations, []int{1, 1, 1}) {
		t.Fatalf("migrations = %v", migrations)
	}
	expectJSON(t, asOf(family.Conversation(conversationID).Member("member", nil), conversationID, firstEntry), `{"count":1,"seed":"seed"}`)
	childID := fork(t, ts.session, conversationID, secondEntry)
	expectJSON(t, asOf(v3.Conversation(childID), childID, firstEntry), `{"count":1,"version":3}`)
	expectJSON(t, asOf(v3.Conversation(childID), childID, secondEntry), `{"count":2,"version":3}`)
	expectJSON(t, asOf(family.Conversation(childID).Member("member", nil), childID, firstEntry), `{"count":1,"seed":"seed"}`)
	_, err := ts.session.SnapshotAsOf(bg, v3.Conversation(childID), thirdEntry)
	errorContains(t, err, fmt.Sprintf("Entry %d is not visible", thirdEntry))
}

func TestHistorySelectsTheIncarnationAliveAtTheEntryCommit(t *testing.T) {
	doc := valueDoc("history.incarnation", durable.HistoryRewindable, durable.ForkAsOf, "initial")
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	ref := doc.Conversation(conversationID)
	beforeCreation, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.EntryID, error) {
		return appendEntry(tx, conversationID, "before"), nil
	})
	must(err)
	var createdAt, retiredAt, recreatedAt durable.EntryID
	commit(t, ts.session, func(tx *durable.Tx) error {
		createdAt = appendEntry(tx, conversationID, "create")
		setDocValue(tx, ref, "old")
		return nil
	})
	commit(t, ts.session, func(tx *durable.Tx) error {
		retiredAt = appendEntry(tx, conversationID, "retire")
		return tx.RetireDoc(ref)
	})
	commit(t, ts.session, func(tx *durable.Tx) error {
		recreatedAt = appendEntry(tx, conversationID, "recreate")
		setDocValue(tx, ref, "new")
		return nil
	})
	for at, want := range map[durable.EntryID]string{beforeCreation: "null", createdAt: `{"value":"old"}`, retiredAt: "null", recreatedAt: `{"value":"new"}`} {
		v, err := ts.session.SnapshotAsOf(bg, ref, at)
		must(err)
		if v == nil {
			if want != "null" {
				t.Fatalf("at %d: absent, want %s", at, want)
			}
			continue
		}
		expectJSON(t, v, want)
	}
	must(ts.session.Close(bg))
	_, err = ts.session.SnapshotAsOf(bg, ref, recreatedAt)
	errorContains(t, err, "closed")
}

var (
	cacheV1 = sessionDoc(durable.DocDef{Kind: "cache.versioned", Initial: func(any) map[string]any { return map[string]any{"name": "first"} }})
	cacheV2 = sessionDoc(durable.DocDef{
		Kind: "cache.versioned", Version: 2, Initial: func(any) map[string]any { return map[string]any{"names": []any{}} },
		Migrate: func(value map[string]any, _ int) map[string]any { return map[string]any{"names": []any{value["name"]}} },
	})
)

func pushName(tx *durable.Tx, name string) error {
	_, err := mustDoc(tx, cacheV2.Session()).At("names").Push(name)
	return err
}

func TestCacheMigratesADocumentCachedByAnOlderDefinition(t *testing.T) {
	ts := openTestSession(t)
	touch(t, ts.session, cacheV1.Session())
	expectJSON(t, snapshot(t, ts.session, cacheV1.Session()), `{"name":"first"}`)
	// Reloaded extension code reads the still-cached document with a newer
	// definition.
	expectJSON(t, snapshot(t, ts.session, cacheV2.Session()), `{"names":["first"]}`)
	commit(t, ts.session, func(tx *durable.Tx) error { return pushName(tx, "second") })
	w, ok := documentWrites(ts.storage.lastCommit())[0].(durable.DocumentChangeWrite)
	if !ok || w.Content.Version != 2 || w.Content.Kind != durable.ContentBase || jsonOf(t, w.Content.Value) != `{"names":["first","second"]}` {
		t.Fatalf("write = %+v", documentWrites(ts.storage.lastCommit())[0])
	}
	_, err := ts.session.Snapshot(bg, cacheV1.Session())
	errorContains(t, err, "newer version 2")
}

func TestCacheSendsOlderShapeObserversARootReplacement(t *testing.T) {
	ts := openTestSession(t)
	touch(t, ts.session, cacheV1.Session())
	state := documentState(t, ts.session, cacheV1.Session())
	defer state.Dispose()
	watch := watchDoc(t, ts.session, cacheV1.Session())
	frames := newRecorder(false)
	must(watch.Start(frames.listener))
	commit(t, ts.session, func(tx *durable.Tx) error { return pushName(tx, "second") })
	eventually(t, func() bool { return len(frames.all()) == 1 })
	eventually(t, func() bool { return jsonOf(t, state.Value()) == `{"names":["first","second"]}` })
	if jsonOf(t, watch.Value()) != `{"names":["first","second"]}` || jsonOf(t, frames.all()[0].ops) != `[["r",{"names":["first","second"]}]]` {
		t.Fatalf("watch %v, frames %+v", watch.Value(), frames.all())
	}
	watch.Stop()
}

func TestCacheServesAnOlderDefinitionFromStorageAfterAnInMemoryMigration(t *testing.T) {
	ts := openTestSession(t)
	touch(t, ts.session, cacheV1.Session())
	must(ts.session.UnloadDocuments())
	expectJSON(t, snapshot(t, ts.session, cacheV2.Session()), `{"names":["first"]}`)
	expectJSON(t, snapshot(t, ts.session, cacheV1.Session()), `{"name":"first"}`)
	commit(t, ts.session, func(tx *durable.Tx) error { return mustDoc(tx, cacheV1.Session()).Set("name", "renamed") })
	expectJSON(t, snapshot(t, ts.session, cacheV2.Session()), `{"names":["renamed"]}`)
}
