package durable_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/session-states.test.ts

var StateDoc = durable.MustDefineDoc(durable.DocDef{
	CheckpointWhen: durable.NeverCheckpoint,
	Kind:           "state.state", Version: 1, Scope: durable.ScopeSession,
	Initial: func(any) map[string]any {
		return map[string]any{"value": 0, "retained": map[string]any{"label": "stable"}}
	},
})

var StateFamilyDoc = durable.MustDefineDoc(durable.DocDef{
	CheckpointWhen: durable.NeverCheckpoint,
	Kind:           "state.family", Version: 1, Family: true, Scope: durable.ScopeSession,
	Initial: func(seed any) map[string]any { return map[string]any{"value": len(seed.(string))} },
})

func createState(t *testing.T) *testSession {
	t.Helper()
	ts := openTestSession(t)
	commit(t, ts.session, func(tx *durable.Tx) error { _, err := tx.Doc(StateDoc.Session()); return err })
	return ts
}

func setValue(t *testing.T, s *durable.Session, ref durable.DocRef, value int) {
	t.Helper()
	commit(t, s, func(tx *durable.Tx) error { return mustDoc(tx, ref).Set("value", value) })
}

func documentState(t *testing.T, s *durable.Session, ref durable.DocRef) durable.DocumentState {
	t.Helper()
	state, err := s.DocumentState(bg, ref)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

type stateDelivery struct {
	value    map[string]any
	sequence int
}

// recordDeliveries subscribes and records every delivery.
func recordDeliveries(state durable.DocumentState) func() []stateDelivery {
	var mu sync.Mutex
	var deliveries []stateDelivery
	state.Subscribe(func(value map[string]any, _ context.Context, d chord.ReplicatedStateDelivery) <-chan error {
		mu.Lock()
		deliveries = append(deliveries, stateDelivery{value, d.Sequence})
		mu.Unlock()
		return nil
	})
	return func() []stateDelivery {
		mu.Lock()
		defer mu.Unlock()
		return append([]stateDelivery(nil), deliveries...)
	}
}

func valueOf(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m["value"]
}

func TestStateNeverCreatesAnAbsentDocument(t *testing.T) {
	ts := openTestSession(t)
	commits := ts.storage.commitCount()
	if documentState(t, ts.session, StateDoc.Session()) != nil || documentState(t, ts.session, StateFamilyDoc.Session().Member("missing", nil)) != nil {
		t.Fatal("an absent document has a state")
	}
	if ts.storage.commitCount() != commits || ts.storage.mints() != 0 {
		t.Fatal("acquiring a state wrote")
	}
}

func TestStateIsHydratedWithContiguousDeliveries(t *testing.T) {
	ts := createState(t)
	ref := StateDoc.Session()
	baseline := snapshot(t, ts.session, ref)
	state := documentState(t, ts.session, ref)
	defer state.Dispose()
	deliveries := recordDeliveries(state)
	if !same(state.Value(), baseline) {
		t.Fatal("the state is not hydrated with the committed value")
	}
	setValue(t, ts.session, ref, 1)
	setValue(t, ts.session, ref, 2)
	eventually(t, func() bool { return len(deliveries()) == 3 })
	var sequences, values []any
	for _, d := range deliveries() {
		sequences = append(sequences, d.sequence)
		values = append(values, valueOf(d.value))
	}
	if !reflect.DeepEqual(sequences, []any{0, 1, 2}) || !reflect.DeepEqual(values, []any{0.0, 1.0, 2.0}) {
		t.Fatalf("sequences %v, values %v", sequences, values)
	}
	if !same(state.Value(), snapshot(t, ts.session, ref)) {
		t.Fatal("the state is not the committed value")
	}
}

func TestStatesOfOneIncarnationAreIndependent(t *testing.T) {
	ts := createState(t)
	ref := StateDoc.Session()
	first := documentState(t, ts.session, ref)
	second := documentState(t, ts.session, ref)
	if first == second {
		t.Fatal("one state for two acquisitions")
	}
	setValue(t, ts.session, ref, 1)
	eventually(t, func() bool { return valueOf(first.Value()) == 1.0 && valueOf(second.Value()) == 1.0 })
	first.Dispose()
	setValue(t, ts.session, ref, 2)
	eventually(t, func() bool { return valueOf(second.Value()) == 2.0 })
	if valueOf(first.Value()) != 1.0 {
		t.Fatal("a disposed state advanced")
	}
	second.Dispose()
}

func TestStateSharesExactCommittedValues(t *testing.T) {
	ts := createState(t)
	ref := StateDoc.Session()
	state := documentState(t, ts.session, ref)
	defer state.Dispose()
	setValue(t, ts.session, ref, 4)
	published := documentChanges(ts.lastPublication())[0]
	eventually(t, func() bool { return same(state.Value(), published.Value) })
	if !same(state.Value()["retained"], snapshot(t, ts.session, ref)["retained"]) {
		t.Fatal("the state copies the committed value")
	}
}

func TestStateCapturesALateBaselineWithoutRedelivery(t *testing.T) {
	ts := createState(t)
	ref := StateDoc.Session()
	setValue(t, ts.session, ref, 1)
	state := documentState(t, ts.session, ref)
	defer state.Dispose()
	deliveries := recordDeliveries(state)
	settle()
	if valueOf(state.Value()) != 1.0 || len(deliveries()) != 1 || deliveries()[0].sequence != 0 {
		t.Fatalf("value %v, deliveries %v", state.Value(), deliveries())
	}
}

func TestStatePublishesRetirementAndNeverFollowsAReplacement(t *testing.T) {
	ts := createState(t)
	ref := StateDoc.Session()
	oldState := documentState(t, ts.session, ref)
	defer oldState.Dispose()
	commit(t, ts.session, func(tx *durable.Tx) error {
		must(tx.RetireDoc(ref))
		return mustDoc(tx, ref).Set("value", 10)
	})
	eventually(t, func() bool { return oldState.Value() == nil })
	replacement := documentState(t, ts.session, ref)
	defer replacement.Dispose()
	if valueOf(replacement.Value()) != 10.0 {
		t.Fatal("the replacement state is not hydrated")
	}
	setValue(t, ts.session, ref, 11)
	eventually(t, func() bool { return valueOf(replacement.Value()) == 11.0 })
	if oldState.Value() != nil {
		t.Fatal("the retired state followed the replacement")
	}
}

func TestStateColdLoadsADefinitionFreeForkCopy(t *testing.T) {
	copied := durable.MustDefineDoc(durable.DocDef{
		CheckpointWhen: durable.NeverCheckpoint,
		Kind:           "state.copied", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryLatest, Fork: durable.ForkCurrent,
		Initial: func(any) map[string]any { return map[string]any{"value": 0} },
	})
	ts := openTestSession(t)
	parentID := createConversation(t, ts.session)
	entry, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.EntryRecord, error) {
		created, err := tx.AppendEntry(parentID, durable.EntryDraft{Kind: "point"})
		must(err)
		return created, mustDoc(tx, copied.Conversation(parentID)).Set("value", 7)
	})
	must(err)
	childID, err := durable.CommitValue(bg, ts.session, func(tx *durable.Tx) (durable.ConversationID, error) {
		c, err := tx.ForkConversation(parentID, entry.ID, durable.Ownerless)
		return c.ID, err
	})
	must(err)
	reads := ts.storage.documentReads()
	state := documentState(t, ts.session, copied.Conversation(childID))
	defer state.Dispose()
	if jsonOf(t, state.Value()) != `{"value":7}` || ts.storage.documentReads() <= reads {
		t.Fatalf("value %v, reads %d→%d", state.Value(), reads, ts.storage.documentReads())
	}
}

var (
	migrationOld = durable.MustDefineDoc(durable.DocDef{
		CheckpointWhen: durable.NeverCheckpoint,
		Kind:           "state.migration", Version: 1, Scope: durable.ScopeSession,
		Initial: func(any) map[string]any { return map[string]any{"value": 3} },
	})
	migrationCurrent = durable.MustDefineDoc(durable.DocDef{
		CheckpointWhen: durable.NeverCheckpoint,
		Kind:           "state.migration", Version: 2, Scope: durable.ScopeSession,
		Initial: func(any) map[string]any { return map[string]any{"value": 0, "migrated": false} },
		Migrate: func(value map[string]any, _ int) map[string]any {
			return map[string]any{"value": value["value"], "migrated": true}
		},
	})
)

func TestStateHydratesAMigratedTrackerWithoutWriting(t *testing.T) {
	ts := openTestSession(t)
	commit(t, ts.session, func(tx *durable.Tx) error { _, err := tx.Doc(migrationOld.Session()); return err })
	must(ts.session.UnloadDocuments())
	commits := ts.storage.commitCount()
	ref := migrationCurrent.Session()
	state := documentState(t, ts.session, ref)
	defer state.Dispose()
	if jsonOf(t, state.Value()) != `{"migrated":true,"value":3}` || ts.storage.commitCount() != commits {
		t.Fatalf("value %v", state.Value())
	}
	baseline := state.Value()
	// An access with the new version stores its base, which changes nothing
	// for an observer of that version.
	commit(t, ts.session, func(tx *durable.Tx) error { _, err := tx.Doc(ref); return err })
	settle()
	if ts.storage.commitCount() != commits+1 || !same(state.Value(), baseline) {
		t.Fatal("the version base reached the state")
	}
	setValue(t, ts.session, ref, 4)
	eventually(t, func() bool { return jsonOf(t, state.Value()) == `{"migrated":true,"value":4}` })
}

func TestStateContinuesAfterTheTrackerCacheUnloads(t *testing.T) {
	ts := createState(t)
	ref := StateDoc.Session()
	state := documentState(t, ts.session, ref)
	defer state.Dispose()
	baseline := state.Value()
	reads := ts.storage.documentReads()
	must(ts.session.UnloadDocuments())
	reloaded := snapshot(t, ts.session, ref)
	if !reflect.DeepEqual(reloaded, baseline) || same(reloaded, baseline) || ts.storage.documentReads() <= reads {
		t.Fatal("the reload is not a fresh equal value")
	}
	setValue(t, ts.session, ref, 6)
	eventually(t, func() bool { return valueOf(state.Value()) == 6.0 })
}
