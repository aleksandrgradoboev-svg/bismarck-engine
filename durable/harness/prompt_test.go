package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi packages/durable/test/harness-prompt.test.ts.

// desiredOf builds the desired sections from key, value pairs, in order.
func desiredOf(pairs ...string) *ordered[string] {
	desired := newOrdered[string]()
	for i := 0; i < len(pairs); i += 2 {
		desired.set(pairs[i], pairs[i+1])
	}
	return desired
}

func pairsOf(o *ordered[string]) []string {
	var out []string
	for _, key := range o.keys {
		value, _ := o.get(key)
		out = append(out, key, value)
	}
	return out
}

// planned is one planned entry, as JSON: its sections, tool changes, and the
// entries it omits.
type planned struct {
	sections string
	removed  []string
	added    []string
	omit     []durable.EntryID
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	must(t, err)
	return string(raw)
}

// apply plans against the current context, appends the plan, checks that
// replay then yields the desired sections in order and the desired tools, and
// returns what each planned entry carries.
func apply(t *testing.T, conversation *Conversation, tools []ai.Tool, pairs ...string) []planned {
	t.Helper()
	view, err := conversation.Context(bg, nil)
	must(t, err)
	drafts := planSystemEntries(view, desiredOf(pairs...), tools, 7)
	must(t, conversation.Commit(bg, func(tx *durable.Tx) error {
		for _, draft := range drafts {
			if _, err := tx.AppendEntry(conversation.ID, draft); err != nil {
				return err
			}
		}
		return nil
	}))
	after, err := conversation.Context(bg, nil)
	must(t, err)
	if got := pairsOf(replaySections(after.Messages)); !reflect.DeepEqual(got, pairs) && !(len(got) == 0 && len(pairs) == 0) {
		t.Fatalf("replayed sections = %v, want %v", got, pairs)
	}
	declared := make([]ai.Tool, len(tools))
	for i, tool := range tools {
		declared[i] = ai.ToToolDeclaration(tool)
	}
	if got, want := jsonOf(t, ai.GetCurrentTools(after.Messages)), jsonOf(t, declared); got != want && !(len(tools) == 0 && got == "[]") {
		t.Fatalf("offered tools = %s, want %s", got, want)
	}
	out := make([]planned, len(drafts))
	for i, draft := range drafts {
		if draft.Kind != durable.SystemEntry.Kind || len(draft.Model) != 1 {
			t.Fatalf("draft %d is not one system message: %+v", i, draft)
		}
		message, ok := systemMessageOf(draft.Model[0])
		if !ok || message.Timestamp != 7 || len(message.Content) != 0 {
			t.Fatalf("draft %d message = %+v", i, draft.Model[0])
		}
		p := planned{}
		if message.Sections != nil {
			p.sections = jsonOf(t, message.Sections)
		}
		for _, tool := range message.ToolsRemoved {
			p.removed = append(p.removed, tool.Name)
		}
		for _, tool := range message.ToolsAdded {
			p.added = append(p.added, tool.Name)
		}
		for _, edit := range draft.Edits {
			if edit.Action != durable.ContextEditOmit {
				t.Fatalf("edit action = %s", edit.Action)
			}
			p.omit = append(p.omit, edit.Target)
		}
		out[i] = p
	}
	return out
}

func sectionsOnly(sections ...string) []planned {
	out := make([]planned, len(sections))
	for i, s := range sections {
		out[i] = planned{sections: s}
	}
	return out
}

func expectPlanned(t *testing.T, got, want []planned) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planned = %+v, want %+v", got, want)
	}
}

func rootConversation(t *testing.T) *Conversation {
	t.Helper()
	h, _ := openHarness(t, memory.New())
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	return root
}

func lastSystemID(t *testing.T, conversation *Conversation) durable.EntryID {
	t.Helper()
	page, err := conversation.Entries(bg, durable.EntryQuery{}, 100, nil)
	must(t, err)
	for _, entry := range page.Items {
		if entry.Kind == durable.SystemEntry.Kind {
			return entry.ID
		}
	}
	t.Fatal("no system entry")
	return 0
}

// marker appends a head marker selecting head, or itself when head is 0.
func marker(t *testing.T, conversation *Conversation, head durable.EntryID) durable.EntryID {
	t.Helper()
	draft := durable.EntryDraft{Kind: "summary", Model: []ai.Message{user("summary")}}
	if head == 0 {
		draft.HeadSelf = true
	} else {
		draft.Head = &head
	}
	var id durable.EntryID
	must(t, conversation.Commit(bg, func(tx *durable.Tx) error {
		entry, err := tx.AppendEntry(conversation.ID, draft)
		id = entry.ID
		return err
	}))
	return id
}

var promptInput = PromptInput{ConversationID: 1, Agent: &Agent{ThinkingLevel: "off"}, Shown: map[string]string{}}

func TestPromptRendersSectionsInOrderWithTagsOmissionsWrappersAndFailures(t *testing.T) {
	registry := NewRegistry()
	preamble := constSection("preamble", "You are helpful.")
	preamble.Untagged = true
	addSection(t, registry, preamble)
	addSection(t, registry, constSection("cwd", "/repo"))
	addSection(t, registry, Section("skipped", func(context.Context, PromptInput) (string, bool, error) { return "", false, nil }))
	addSection(t, registry, Section("failing", func(context.Context, PromptInput) (string, bool, error) {
		return "", false, errors.New("render failed")
	}))
	// A panic is a throw.
	addSection(t, registry, Section("new-failing", func(context.Context, PromptInput) (string, bool, error) { panic(errors.New("also failed")) }))
	must(t, registry.Install(&Extension{Name: "git", Wraps: []Wrap{
		WrapSection("cwd", func(inner *PromptSection) (*PromptSection, error) {
			wrapped := *inner
			wrapped.Render = func(ctx context.Context, input PromptInput) (string, bool, error) {
				text, ok, err := inner.Render(ctx, input)
				return text + " (git)", ok, err
			}
			return &wrapped, nil
		}),
	}}))
	var reports []error
	shown := desiredOf("failing", "<failing>\nold\n</failing>", "cwd", "stale")
	agent := resolveAgent(nil, registry.Snapshot(), resolveSettings(HarnessSettings{}), func(err error) { t.Fatal(err) })
	desired, err := renderSections(bg, agent.Sections, promptInput, shown, func(err error) { reports = append(reports, err) })
	must(t, err)
	want := []string{"preamble", "You are helpful.", "cwd", "<cwd>\n/repo (git)\n</cwd>", "failing", "<failing>\nold\n</failing>"}
	if got := pairsOf(desired); !reflect.DeepEqual(got, want) {
		t.Fatalf("desired = %q, want %q", got, want)
	}
	if got := names(reports, func(err error) string { return err.Error() }); !reflect.DeepEqual(got, []string{"render failed", "also failed"}) {
		t.Fatalf("reports = %v", got)
	}
}

func TestPromptPropagatesSectionErrorsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(bg)
	cancel()
	failing := Section("a", func(context.Context, PromptInput) (string, bool, error) { return "", false, errors.New("cancelled") })
	_, err := renderSections(ctx, []*PromptSection{failing}, promptInput, newOrdered[string](), func(error) { t.Fatal("reported") })
	if err == nil || err.Error() != "cancelled" {
		t.Fatalf("err = %v, want cancelled", err)
	}
}

func TestPromptReplaysSectionsInPlaceDeletesOnNilAndAppendsReAdditions(t *testing.T) {
	system := func(pairs ...any) ai.SystemMessage {
		sections := ai.SystemSections{}
		for i := 0; i < len(pairs); i += 2 {
			section := ai.SystemSection{Name: pairs[i].(string)}
			if value, ok := pairs[i+1].(string); ok {
				section.Value = &value
			}
			sections = append(sections, section)
		}
		return ai.SystemMessage{Sections: sections, Timestamp: 1}
	}
	shown := replaySections([]ai.Message{
		system("a", "1", "b", "2", "c", "3"),
		user("x"),
		system("b", "20", "a", nil),
		system("a", "10"),
	})
	if got, want := pairsOf(shown), []string{"b", "20", "c", "3", "a", "10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shown = %v, want %v", got, want)
	}
}

func TestPromptEmitsMinimalValuePatchesRemovalsAndAdditions(t *testing.T) {
	c := rootConversation(t)
	expectPlanned(t, apply(t, c, nil, "a", "1", "b", "2", "c", "3"), sectionsOnly(`{"a":"1","b":"2","c":"3"}`))
	expectPlanned(t, apply(t, c, nil, "a", "1", "b", "20", "c", "3", "d", "4"), sectionsOnly(`{"b":"20","d":"4"}`))
	expectPlanned(t, apply(t, c, nil, "a", "1", "c", "3", "d", "4"), sectionsOnly(`{"b":null}`))
	expectPlanned(t, apply(t, c, nil, "a", "1", "c", "3", "d", "4"), nil)
	expectPlanned(t, apply(t, c, nil), sectionsOnly(`{"a":null,"c":null,"d":null}`))
}

func TestPromptRewritesOrderOnlyChangesAndReAdditionsAsTwoEntries(t *testing.T) {
	c := rootConversation(t)
	apply(t, c, nil, "a", "1", "b", "2")
	expectPlanned(t, apply(t, c, nil, "b", "2", "a", "1"), sectionsOnly(`{"a":null,"b":null}`, `{"b":"2","a":"1"}`))
	readded := rootConversation(t)
	apply(t, readded, nil, "a", "1", "b", "2", "c", "3")
	expectPlanned(t, apply(t, readded, nil, "a", "1", "c", "3"), sectionsOnly(`{"b":null}`))
	// Patching would append b after c.
	expectPlanned(t, apply(t, readded, nil, "a", "1", "b", "2", "c", "3"), sectionsOnly(`{"a":null,"c":null}`, `{"a":"1","b":"2","c":"3"}`))
}

func TestPromptRebaselinesAfterAHeadMarkerOmittingRetainedDeltasOnBothSides(t *testing.T) {
	c := rootConversation(t)
	apply(t, c, nil, "a", "1", "b", "2")
	must(t, c.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.AppendEntry(c.ID, durable.EntryDraft{Kind: "pi.user", Model: []ai.Message{user("hi")}})
		return err
	}))
	apply(t, c, nil, "a", "1", "b", "20")
	delta := lastSystemID(t, c)
	// The head keeps the delta but cuts its baseline: replay alone would show
	// only b.
	marker(t, c, delta)
	expectPlanned(t, apply(t, c, nil, "a", "1", "b", "20"), []planned{{sections: `{"a":"1","b":"20"}`, omit: []durable.EntryID{delta}}})
	baselineID := lastSystemID(t, c)
	// A system entry follows the marker now, so later changes are ordinary
	// patches.
	expectPlanned(t, apply(t, c, nil, "a", "1", "b", "21"), sectionsOnly(`{"b":"21"}`))
	after := lastSystemID(t, c)
	// A second marker keeps deltas from both sides of the first one.
	marker(t, c, delta)
	expectPlanned(t, apply(t, c, nil, "a", "1", "b", "21"), []planned{{sections: `{"a":"1","b":"21"}`, omit: []durable.EntryID{delta, baselineID, after}}})
}

func TestPromptWritesACompletePostHeadBaselineEvenWhenReplayMatches(t *testing.T) {
	c := rootConversation(t)
	apply(t, c, nil, "a", "1")
	baselineID := lastSystemID(t, c)
	marker(t, c, baselineID)
	expectPlanned(t, apply(t, c, nil, "a", "1"), []planned{{sections: `{"a":"1"}`, omit: []durable.EntryID{baselineID}}})
	marker(t, c, 0)
	expectPlanned(t, apply(t, c, nil), sectionsOnly(`{}`))
	expectPlanned(t, apply(t, c, nil), nil)
}

func declaration(name, description string) ai.Tool {
	return ai.Tool{Name: name, Description: description, Parameters: objectSchema()}
}

func TestToolLoadoutAddsRemovesReplacesChangedDeclarationsAndRewritesOrder(t *testing.T) {
	c := rootConversation(t)
	a, b, cc := declaration("a", "a"), declaration("b", "b"), declaration("c", "c")
	tools := func(list ...string) []string { return list }
	expectPlanned(t, apply(t, c, []ai.Tool{a, b}), []planned{{added: tools("a", "b")}})
	expectPlanned(t, apply(t, c, []ai.Tool{a, b}), nil)
	expectPlanned(t, apply(t, c, []ai.Tool{a, b, cc}), []planned{{added: tools("c")}})
	expectPlanned(t, apply(t, c, []ai.Tool{a, cc}), []planned{{removed: tools("b")}})
	// A changed declaration at the end is removed and re-added in place.
	c2 := declaration("c", "changed")
	expectPlanned(t, apply(t, c, []ai.Tool{a, c2}), []planned{{removed: tools("c"), added: tools("c")}})
	// A changed declaration in the middle would move to the end, so the whole
	// order is rewritten.
	a2 := declaration("a", "changed")
	expectPlanned(t, apply(t, c, []ai.Tool{a2, c2}), []planned{{removed: tools("a", "c"), added: tools("a", "c")}})
	// Order-only change.
	expectPlanned(t, apply(t, c, []ai.Tool{c2, a2}), []planned{{removed: tools("a", "c"), added: tools("c", "a")}})
	expectPlanned(t, apply(t, c, nil), []planned{{removed: tools("c", "a")}})
}

func TestToolLoadoutRidesOnTheLastSectionEntryAndRedeclaresEveryToolAfterAHeadCut(t *testing.T) {
	c := rootConversation(t)
	a, b := declaration("a", "a"), declaration("b", "b")
	expectPlanned(t, apply(t, c, []ai.Tool{a}, "x", "1", "y", "2"), []planned{{sections: `{"x":"1","y":"2"}`, added: []string{"a"}}})
	// Section order changes need two entries; the tool change rides on the
	// second.
	expectPlanned(t, apply(t, c, []ai.Tool{a, b}, "y", "2", "x", "1"), []planned{
		{sections: `{"x":null,"y":null}`},
		{sections: `{"y":"2","x":"1"}`, added: []string{"b"}},
	})
	marker(t, c, 0)
	expectPlanned(t, apply(t, c, []ai.Tool{a, b}, "y", "2", "x", "1"), []planned{{sections: `{"y":"2","x":"1"}`, added: []string{"a", "b"}}})
}
