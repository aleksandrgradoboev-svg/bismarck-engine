package durable_test

import (
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/session-definitions.test.ts. pi's overloads are
// checked by the type checker, which Go does not have for one Doc type: what
// they reject is rejected here when the definition is made or when a
// misbuilt reference is used.

func stateInitial(any) map[string]any { return map[string]any{"value": 0} }

func TestDefinitionValidatesPersistedVersionSemantics(t *testing.T) {
	_, err := durable.DefineDoc(durable.DocDef{Kind: "k", Version: 0, Scope: durable.ScopeSession, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint})
	errorContains(t, err, "positive integer")
	d, err := durable.DefineDoc(durable.DocDef{Kind: "", Version: 1, Scope: durable.ScopeSession, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint})
	if err != nil || d.Definition().Kind != "" {
		t.Fatalf("empty kind: %v", err)
	}
}

func TestDefinitionValidatesDocumentSemantics(t *testing.T) {
	for _, def := range []durable.DocDef{
		{Kind: "k", Version: 1, Scope: "global", Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint},
		{Kind: "k", Version: 1, Scope: durable.ScopeSession, History: durable.HistoryLatest, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint},
		{Kind: "k", Version: 1, Scope: durable.ScopeTask, Fork: durable.ForkCurrent, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint},
		{Kind: "k", Version: 1, Scope: durable.ScopeConversation, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint},
		// Latest-only history keeps nothing to fork as of.
		{Kind: "k", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryLatest, Fork: durable.ForkAsOf, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint},
		{Kind: "k", Version: 1, Scope: durable.ScopeSession, CheckpointWhen: durable.NeverCheckpoint},
	} {
		if _, err := durable.DefineDoc(def); err == nil {
			t.Errorf("accepted %+v", def)
		}
	}
	for _, def := range []durable.DocDef{
		{Kind: "k", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryLatest, Fork: durable.ForkInitial, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint},
		{Kind: "k", Version: 1, Scope: durable.ScopeConversation, History: durable.HistoryRewindable, Fork: durable.ForkAsOf, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint},
		{Kind: "k", Version: 1, Scope: durable.ScopeTask, Family: true, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint},
	} {
		if _, err := durable.DefineDoc(def); err != nil {
			t.Errorf("rejected %+v: %v", def, err)
		}
	}
}

// pi 5c1736f8 requires checkpointWhen by type (@ts-expect-error); Go checks it
// when the definition is made, for documents and families alike.
func TestDefinitionRequiresAnExplicitCheckpointPolicyForDocumentsAndFamilies(t *testing.T) {
	for _, family := range []bool{false, true} {
		_, err := durable.DefineDoc(durable.DocDef{Kind: "t.missing-checkpoint", Version: 1, Family: family, Scope: durable.ScopeSession, Initial: stateInitial})
		errorContains(t, err, "requires a checkpoint policy")
	}
	if durable.NeverCheckpoint(map[string]any{}, nil, durable.CheckpointInfo{DeltasSinceBase: 1000}) {
		t.Fatal("NeverCheckpoint asked for a base")
	}
}

func TestDefinitionReferencesRejectTheWrongOwnerOrKey(t *testing.T) {
	ts := openTestSession(t)
	conversationID := createConversation(t, ts.session)
	sessionFamily := durable.MustDefineDoc(durable.DocDef{Kind: "t.session-family", Version: 1, Family: true, Scope: durable.ScopeSession, Initial: stateInitial, CheckpointWhen: durable.NeverCheckpoint})
	for _, c := range []struct {
		name string
		ref  durable.DocRef
		want string
	}{
		{"a Session document takes no owner", CounterDoc.Conversation(conversationID), "session-scoped"},
		{"a conversation document requires a conversation", LiveDoc.Session(), "conversation-scoped"},
		{"a family requires a member key", sessionFamily.Session(), "requires a member key"},
		{"a singleton has no members", CounterDoc.Session().Member("k", 1), "not a family"},
		{"an empty reference", durable.DocRef{}, "no definition"},
	} {
		err := ts.session.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(c.ref); return err })
		errorContains(t, err, c.want)
		err = ts.session.Commit(bg, func(tx *durable.Tx) error { return tx.RetireDoc(c.ref) })
		errorContains(t, err, c.want)
		_, err = ts.session.Snapshot(bg, c.ref)
		errorContains(t, err, c.want)
		_, err = ts.session.WatchDoc(bg, c.ref)
		errorContains(t, err, c.want)
		_, err = ts.session.DocumentState(bg, c.ref)
		errorContains(t, err, c.want)
	}
	_, err := ts.session.SnapshotAsOf(bg, CounterDoc.Session(), 1)
	errorContains(t, err, "requires a conversation document")
}
