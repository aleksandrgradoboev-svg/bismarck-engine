package durable_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/session-watches.test.ts

var WatchDoc = durable.MustDefineDoc(durable.DocDef{
	CheckpointWhen: durable.NeverCheckpoint,
	Kind:           "watch.state", Version: 1, Scope: durable.ScopeSession,
	Initial: func(any) map[string]any {
		return map[string]any{"value": 0, "items": []any{"a", "b"}, "retained": map[string]any{"label": "stable"}}
	},
})

func createWatchState(t *testing.T) *testSession {
	t.Helper()
	ts := openTestSession(t)
	commit(t, ts.session, func(tx *durable.Tx) error { _, err := tx.Doc(WatchDoc.Session()); return err })
	return ts
}

func watchDoc(t *testing.T, s *durable.Session, ref durable.DocRef) *durable.DocumentWatch {
	t.Helper()
	w, err := s.WatchDoc(bg, ref)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func closedWith(t *testing.T, w *durable.DocumentWatch) durable.WatchEnd {
	t.Helper()
	select {
	case <-w.Closed():
		return w.End()
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not close")
		return durable.WatchEnd{}
	}
}

type watchDelivery struct {
	value map[string]any
	ops   []delta.Op
	ctx   context.Context
}

// recorder collects deliveries; hold blocks the first one until released.
type recorder struct {
	mu         sync.Mutex
	deliveries []watchDelivery
	active     int
	maxActive  int
	entered    chan struct{}
	release    chan struct{}
	enterOnce  sync.Once
}

func newRecorder(hold bool) *recorder {
	r := &recorder{entered: make(chan struct{})}
	if hold {
		r.release = make(chan struct{})
	}
	return r
}

func (r *recorder) listener(ctx context.Context, value map[string]any, ops []delta.Op) error {
	r.mu.Lock()
	r.active++
	r.maxActive = max(r.maxActive, r.active)
	r.deliveries = append(r.deliveries, watchDelivery{value, ops, ctx})
	first := len(r.deliveries) == 1
	r.mu.Unlock()
	if first {
		r.enterOnce.Do(func() { close(r.entered) })
		if r.release != nil {
			<-r.release
		}
	}
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return nil
}

func (r *recorder) all() []watchDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]watchDelivery(nil), r.deliveries...)
}

func (r *recorder) values() []any {
	var out []any
	for _, d := range r.all() {
		out = append(out, valueOf(d.value))
	}
	return out
}

func (r *recorder) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-r.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
}

func TestWatchNeverCreatesAnAbsentDocument(t *testing.T) {
	ts := openTestSession(t)
	commits := ts.storage.commitCount()
	if watchDoc(t, ts.session, WatchDoc.Session()) != nil || ts.storage.commitCount() != commits || ts.storage.mints() != 0 {
		t.Fatal("watching an absent document created it")
	}
}

func TestWatchKeepsTheAcquisitionRevisionUntilStartAndDeliversExactFrames(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	initial := watch.Value()
	setValue(t, ts.session, ref, 1)
	firstPublished := documentChanges(ts.lastPublication())[0]
	setValue(t, ts.session, ref, 2)
	secondPublished := documentChanges(ts.lastPublication())[0]
	settle()
	if !same(watch.Value(), initial) {
		t.Fatal("the watch advanced before start")
	}
	// The first delivery holds until released: an inline delivery would keep
	// Start from returning. Pi checks right after start, which its microtask
	// delivery makes exact; a Go delivery goroutine may already have run.
	r := newRecorder(true)
	var mismatch bool
	started := make(chan error, 1)
	go func() {
		started <- watch.Start(func(ctx context.Context, value map[string]any, ops []delta.Op) error {
			if !same(watch.Value(), value) {
				mismatch = true
			}
			return r.listener(ctx, value, ops)
		})
	}()
	select {
	case err := <-started:
		must(err)
	case <-time.After(5 * time.Second):
		close(r.release)
		t.Fatal("start delivered inline")
	}
	close(r.release)
	eventually(t, func() bool { return len(r.all()) == 2 })
	d := r.all()
	if !reflect.DeepEqual(r.values(), []any{1.0, 2.0}) || mismatch {
		t.Fatalf("values %v", r.values())
	}
	if !same(d[0].value, firstPublished.Value) || !same(d[0].ops, firstPublished.Ops) ||
		!same(d[1].value, secondPublished.Value) || !same(d[1].ops, secondPublished.Ops) {
		t.Fatal("the delivered frames are not the published ones")
	}
	if valueOf(initial) != 0.0 {
		t.Fatal("the acquisition revision changed")
	}
	watch.Stop()
}

func TestWatchSerializesCallbacksAndBuffersFramesWhileOneIsInFlight(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	r := newRecorder(true)
	must(watch.Start(r.listener))
	setValue(t, ts.session, ref, 1)
	r.waitEntered(t)
	for value := 2; value <= 20; value++ {
		setValue(t, ts.session, ref, value)
	}
	if !reflect.DeepEqual(r.values(), []any{1.0}) {
		t.Fatalf("values %v", r.values())
	}
	close(r.release)
	eventually(t, func() bool { return len(r.all()) == 20 })
	var want []any
	for value := 1; value <= 20; value++ {
		want = append(want, float64(value))
	}
	if r.maxActive != 1 || !reflect.DeepEqual(r.values(), want) {
		t.Fatalf("max active %d, values %v", r.maxActive, r.values())
	}
	watch.Stop()
}

func TestWatchListenerMayCommit(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	completed := make(chan struct{})
	var values []any
	must(watch.Start(func(_ context.Context, value map[string]any, _ []delta.Op) error {
		values = append(values, valueOf(value))
		if valueOf(value) == 1.0 {
			return ts.session.Commit(bg, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("value", 2) })
		}
		close(completed)
		return nil
	}))
	setValue(t, ts.session, ref, 1)
	<-completed
	if !reflect.DeepEqual(values, []any{1.0, 2.0}) {
		t.Fatalf("values %v", values)
	}
	watch.Stop()
}

func TestWatchCollapses101PendingCommitsToOneReplacement(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	for value := 1; value <= 101; value++ {
		setValue(t, ts.session, ref, value)
	}
	r := newRecorder(false)
	must(watch.Start(r.listener))
	eventually(t, func() bool { return len(r.all()) == 1 })
	settle()
	d := r.all()
	if len(d) != 1 || valueOf(d[0].value) != 101.0 || !reflect.DeepEqual(d[0].ops, []delta.Op{delta.Replace{Value: watch.Value()}}) {
		t.Fatalf("deliveries %v", d)
	}
	watch.Stop()
}

func TestWatchNeverFoldsTheInFlightFrameIntoAnOverflowReset(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	r := newRecorder(true)
	must(watch.Start(r.listener))
	setValue(t, ts.session, ref, 1)
	r.waitEntered(t)
	for value := 2; value <= 102; value++ {
		setValue(t, ts.session, ref, value)
	}
	close(r.release)
	eventually(t, func() bool { return len(r.all()) == 2 })
	settle()
	d := r.all()
	if len(d) != 2 || valueOf(d[0].value) != 1.0 || valueOf(d[1].value) != 102.0 ||
		!reflect.DeepEqual(d[1].ops, []delta.Op{delta.Replace{Value: watch.Value()}}) {
		t.Fatalf("deliveries %v", d)
	}
	watch.Stop()
}

func TestWatchFoldsRetirementIntoAnOverflowResetAndCloses(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	for value := 1; value <= 100; value++ {
		setValue(t, ts.session, ref, value)
	}
	commit(t, ts.session, func(tx *durable.Tx) error { return tx.RetireDoc(ref) })
	r := newRecorder(false)
	must(watch.Start(r.listener))
	if end := closedWith(t, watch); end != (durable.WatchEnd{Reason: durable.WatchRetired}) {
		t.Fatalf("end %+v", end)
	}
	d := r.all()
	if len(d) != 1 || d[0].value != nil || !reflect.DeepEqual(d[0].ops, []delta.Op{delta.Replace{Value: nil}}) {
		t.Fatalf("deliveries %v", d)
	}
}

func TestWatchDeliversStructuralNoOpCommits(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	initial := watch.Value()
	commit(t, ts.session, func(tx *durable.Tx) error {
		items := mustDoc(tx, ref).At("items")
		first, _, err := items.Shift()
		must(err)
		_, err = items.Unshift(first)
		return err
	})
	newest := snapshot(t, ts.session, ref)
	if same(newest, initial) || !reflect.DeepEqual(newest, initial) {
		t.Fatal("the no-op did not produce an equal new revision")
	}
	r := newRecorder(false)
	must(watch.Start(r.listener))
	eventually(t, func() bool { return len(r.all()) == 1 })
	settle()
	d := r.all()
	if len(d) != 1 || !same(d[0].value, newest) || len(d[0].ops) == 0 {
		t.Fatalf("deliveries %v", d)
	}
	watch.Stop()
}

type watchKey struct{}

func TestWatchKeepsCommitContextValuesWithoutProducerCancellation(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	parent, cancel := context.WithCancel(bg)
	commitCtx := context.WithValue(parent, watchKey{}, "newest-commit")
	r := newRecorder(true)
	must(watch.Start(r.listener))
	must(ts.session.Commit(commitCtx, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("value", 1) }))
	r.waitEntered(t)
	delivered := r.all()[0].ctx
	if delivered.Value(watchKey{}) != "newest-commit" || delivered.Done() != nil {
		t.Fatal("the delivery context lost a value or kept cancellation")
	}
	cancel()
	if end := watch.Stop(); end != (durable.WatchEnd{Reason: durable.WatchStopped}) {
		t.Fatalf("end %+v", end)
	}
	if delivered.Err() != nil {
		t.Fatal("the delivery context was cancelled")
	}
	close(r.release)
}

func TestWatchKeepsEarlierRevisionsStable(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	initial := watch.Value()
	r := newRecorder(false)
	must(watch.Start(r.listener))
	setValue(t, ts.session, ref, 1)
	eventually(t, func() bool { return len(r.all()) == 1 })
	delivered := r.all()[0].value
	if !same(delivered, watch.Value()) || same(delivered, initial) || !same(delivered["retained"], initial["retained"]) || valueOf(initial) != 0.0 {
		t.Fatal("revisions are not stable and shared")
	}
	watch.Stop()
}

func TestWatchDeliversRetirementAndDoesNotFollowRecreation(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	oldWatch := watchDoc(t, ts.session, ref)
	r := newRecorder(false)
	must(oldWatch.Start(r.listener))
	commit(t, ts.session, func(tx *durable.Tx) error {
		must(tx.RetireDoc(ref))
		return mustDoc(tx, ref).Set("value", 10)
	})
	if end := closedWith(t, oldWatch); end.Reason != durable.WatchRetired {
		t.Fatalf("end %+v", end)
	}
	if d := r.all(); len(d) != 1 || d[0].value != nil || oldWatch.Value() != nil {
		t.Fatalf("deliveries %v", d)
	}
	replacement := watchDoc(t, ts.session, ref)
	if valueOf(replacement.Value()) != 10.0 {
		t.Fatal("the replacement watch is not hydrated")
	}
	setValue(t, ts.session, ref, 11)
	settle()
	if oldWatch.Value() != nil {
		t.Fatal("the retired watch followed the replacement")
	}
	replacement.Stop()
}

func TestWatchSessionCloseDiscardsRetirementBufferedBeforeStart(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	baseline := watch.Value()
	commit(t, ts.session, func(tx *durable.Tx) error { return tx.RetireDoc(ref) })
	must(ts.session.Close(bg))
	if end := closedWith(t, watch); end.Reason != durable.WatchSessionClosed {
		t.Fatalf("end %+v", end)
	}
	if !same(watch.Value(), baseline) {
		t.Fatal("the buffered retirement was applied")
	}
	errorContains(t, watch.Start(func(context.Context, map[string]any, []delta.Op) error { return nil }), "stopped")
}

func TestWatchSessionCloseDiscardsRetirementBehindAnInFlightCallback(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	r := newRecorder(true)
	must(watch.Start(r.listener))
	setValue(t, ts.session, ref, 1)
	r.waitEntered(t)
	commit(t, ts.session, func(tx *durable.Tx) error { return tx.RetireDoc(ref) })
	must(ts.session.Close(bg))
	if end := closedWith(t, watch); end.Reason != durable.WatchSessionClosed {
		t.Fatalf("end %+v", end)
	}
	close(r.release)
	settle()
	if !reflect.DeepEqual(r.values(), []any{1.0}) {
		t.Fatalf("values %v", r.values())
	}
}

func TestWatchStopIsIdempotentAndStartIsSingleUse(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	noop := func(context.Context, map[string]any, []delta.Op) error { return nil }
	started := watchDoc(t, ts.session, ref)
	must(started.Start(noop))
	errorContains(t, started.Start(noop), "already started")
	first, second := started.Stop(), started.Stop()
	if first != second || first != (durable.WatchEnd{Reason: durable.WatchStopped}) {
		t.Fatalf("ends %+v %+v", first, second)
	}
	stopped := watchDoc(t, ts.session, ref)
	stopped.Stop()
	errorContains(t, stopped.Start(noop), "stopped")
}

func TestWatchCancelsAcquisitionWithoutLeakingAWatch(t *testing.T) {
	ts := createWatchState(t)
	must(ts.session.UnloadDocuments())
	g := ts.storage.holdFindDocument()
	ctx, cancel := context.WithCancelCause(bg)
	acquisition := async(func() error { _, err := ts.session.WatchDoc(ctx, WatchDoc.Session()); return err })
	g.Entered(t)
	cancel(errors.New("cancel acquisition"))
	g.Release()
	errorContains(t, wait(t, acquisition), "cancel acquisition")
	if _, _, attached, _ := ts.session.DocumentCacheSizes(); attached != 0 {
		t.Fatalf("attached = %d", attached)
	}
	must(ts.session.Close(bg))
}

func TestWatchCancellationStopsFutureDeliveryWithoutAbortingTheCallback(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	ctx, cancel := context.WithCancel(bg)
	watch, err := ts.session.WatchDoc(ctx, ref)
	must(err)
	r := newRecorder(true)
	must(watch.Start(r.listener))
	setValue(t, ts.session, ref, 1)
	r.waitEntered(t)
	cancel()
	if end := closedWith(t, watch); end.Reason != durable.WatchCancelled {
		t.Fatalf("end %+v", end)
	}
	if r.all()[0].ctx.Err() != nil {
		t.Fatal("the callback was cancelled")
	}
	close(r.release)
}

func TestWatchSessionCloseStopsFutureDeliveryWithoutJoiningTheCallback(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	r := newRecorder(true)
	must(watch.Start(r.listener))
	setValue(t, ts.session, ref, 1)
	r.waitEntered(t)
	must(ts.session.Close(bg))
	if r.all()[0].ctx.Err() != nil {
		t.Fatal("the callback was cancelled")
	}
	if end := closedWith(t, watch); end.Reason != durable.WatchSessionClosed {
		t.Fatalf("end %+v", end)
	}
	close(r.release)
}

func TestWatchListenerFailureEndsOnlyTheAffectedWatch(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	failed := watchDoc(t, ts.session, ref)
	healthy := watchDoc(t, ts.session, ref)
	must(failed.Start(func(context.Context, map[string]any, []delta.Op) error { return errors.New("listener failed") }))
	r := newRecorder(false)
	must(healthy.Start(r.listener))
	setValue(t, ts.session, ref, 1)
	end := closedWith(t, failed)
	if end.Reason != durable.WatchListenerError || end.Err == nil || end.Err.Error() != "listener failed" {
		t.Fatalf("end %+v", end)
	}
	eventually(t, func() bool { return len(r.all()) == 1 })
	healthy.Stop()
}

var (
	watchMigrationOld = durable.MustDefineDoc(durable.DocDef{
		CheckpointWhen: durable.NeverCheckpoint,
		Kind:           "watch.migration", Version: 1, Scope: durable.ScopeSession,
		Initial: func(any) map[string]any { return map[string]any{"value": 3} },
	})
	watchMigrationCurrent = durable.MustDefineDoc(durable.DocDef{
		CheckpointWhen: durable.NeverCheckpoint,
		Kind:           "watch.migration", Version: 2, Scope: durable.ScopeSession,
		Initial: func(any) map[string]any { return map[string]any{"value": 0, "migrated": false} },
		Migrate: func(value map[string]any, _ int) map[string]any {
			return map[string]any{"value": value["value"], "migrated": true}
		},
	})
)

func TestWatchHydratesMigrationWithoutWritingAndObservesTheLaterEdit(t *testing.T) {
	ts := openTestSession(t)
	commit(t, ts.session, func(tx *durable.Tx) error { _, err := tx.Doc(watchMigrationOld.Session()); return err })
	must(ts.session.UnloadDocuments())
	commits := ts.storage.commitCount()
	ref := watchMigrationCurrent.Session()
	watch := watchDoc(t, ts.session, ref)
	if jsonOf(t, watch.Value()) != `{"migrated":true,"value":3}` || ts.storage.commitCount() != commits {
		t.Fatalf("value %v", watch.Value())
	}
	r := newRecorder(false)
	must(watch.Start(r.listener))
	commit(t, ts.session, func(tx *durable.Tx) error { _, err := tx.Doc(ref); return err })
	settle()
	if len(r.all()) != 0 {
		t.Fatal("the version base was delivered")
	}
	setValue(t, ts.session, ref, 4)
	eventually(t, func() bool { return len(r.all()) == 1 })
	if !reflect.DeepEqual(r.values(), []any{4.0}) {
		t.Fatalf("values %v", r.values())
	}
	watch.Stop()
}

func TestWatchDeliveredOperationsReplayFromTheAcquisitionRevision(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	replica := watch.Value()
	var mismatch error
	done := make(chan struct{})
	must(watch.Start(func(_ context.Context, value map[string]any, ops []delta.Op) error {
		next, err := delta.ApplyImmutable(replica, ops)
		if err == nil && !reflect.DeepEqual(next, value) {
			err = errors.New("the replica differs")
		}
		replica, mismatch = next, err
		close(done)
		return nil
	}))
	commit(t, ts.session, func(tx *durable.Tx) error {
		state := mustDoc(tx, ref)
		must(state.Set("value", 3))
		_, err := state.At("items").Splice(0, 1)
		return err
	})
	<-done
	if mismatch != nil || !reflect.DeepEqual(replica, watch.Value()) {
		t.Fatalf("replay: %v", mismatch)
	}
	watch.Stop()
}

func TestWatchContinuesAfterTheTrackerCacheUnloads(t *testing.T) {
	ts := createWatchState(t)
	ref := WatchDoc.Session()
	watch := watchDoc(t, ts.session, ref)
	baseline := watch.Value()
	reads := ts.storage.documentReads()
	must(ts.session.UnloadDocuments())
	reloaded := snapshot(t, ts.session, ref)
	if !reflect.DeepEqual(reloaded, baseline) || same(reloaded, baseline) || ts.storage.documentReads() <= reads {
		t.Fatal("the reload is not a fresh equal value")
	}
	must(watch.Start(func(context.Context, map[string]any, []delta.Op) error { return nil }))
	setValue(t, ts.session, ref, 7)
	eventually(t, func() bool { return valueOf(watch.Value()) == 7.0 })
	watch.Stop()
}
