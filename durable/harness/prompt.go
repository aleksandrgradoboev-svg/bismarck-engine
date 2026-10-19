package harness

import (
	"context"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// DocumentReader is committed document reads, which a *durable.Session
// provides.
type DocumentReader interface {
	Snapshot(ctx context.Context, ref durable.DocRef) (map[string]any, error)
	SnapshotAsOf(ctx context.Context, ref durable.DocRef, at durable.EntryID) (map[string]any, error)
}

// PromptInput is the input to system prompt section rendering for one request
// preparation.
type PromptInput struct {
	ConversationID durable.ConversationID
	// Agent is the request's resolution; Agent.Tools are the tools offered in
	// this request.
	Agent *Agent
	// Env is built by the Harness env builder for this preparation; nil
	// without an environment.
	Env env.ExecutionEnv
	// Shown are the sections already in effect after replaying the active
	// transcript.
	Shown map[string]string
	// Read is committed document reads.
	Read DocumentReader
}

// PromptSection is one system prompt section; the agent's sections render in
// order before each request.
type PromptSection struct {
	Key string
	// Render returns the section text, or ok false to omit the section.
	Render func(ctx context.Context, input PromptInput) (text string, ok bool, err error)
	// Untagged sections are not wrapped as "<key>\n...\n</key>".
	Untagged bool
}

// Section builds a tagged prompt section.
func Section(key string, render func(ctx context.Context, input PromptInput) (string, bool, error)) *PromptSection {
	return &PromptSection{Key: key, Render: render}
}

// replaySections returns the sections in effect after replaying system
// messages in order: set in place, nil deletes, re-adding appends.
func replaySections(messages []ai.Message) *ordered[string] {
	shown := newOrdered[string]()
	for _, message := range messages {
		system, ok := systemMessageOf(message)
		if !ok || system.Sections == nil {
			continue
		}
		for _, section := range system.Sections.Entries() {
			if section.Value == nil {
				shown.delete(section.Name)
			} else {
				shown.set(section.Name, *section.Value)
			}
		}
	}
	return shown
}

// renderSections renders the agent's sections in order. An omitted section is
// left out; tagged text is wrapped as "<key>\n...\n</key>". A section that
// fails keeps its shown text, if any, and is reported; failures after ctx is
// cancelled propagate. report gets the failing section, whose extension the
// caller knows.
func renderSections(ctx context.Context, sections []*PromptSection, input PromptInput, shown *ordered[string], report func(error, *PromptSection)) (*ordered[string], error) {
	desired := newOrdered[string]()
	for _, section := range sections {
		text, ok, err := renderSection(ctx, section, input)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			report(err, section)
			if kept, ok := shown.get(section.Key); ok {
				desired.set(section.Key, kept)
			}
			continue
		}
		if !ok {
			continue
		}
		if !section.Untagged {
			text = "<" + section.Key + ">\n" + text + "\n</" + section.Key + ">"
		}
		desired.set(section.Key, text)
	}
	return desired, nil
}

func renderSection(ctx context.Context, section *PromptSection, input PromptInput) (text string, ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return section.Render(ctx, input)
}

// planSystemEntries plans the pi.system entries that make the replayed
// sections and tools of view equal desired and tools in values and order.
//
//   - A head marker with no later pi.system entry in context: one complete
//     baseline that omits every retained earlier pi.system entry, written even
//     when it restates the replayed values.
//   - Otherwise, when a minimal section patch would leave a different order:
//     remove every shown section, then re-add every desired section in order.
//   - Otherwise the minimal patch of changed values and nil removals, or
//     nothing.
//
// Tool changes ride on the last planned entry, or on one entry of their own.
func planSystemEntries(view ContextView, desired *ordered[string], tools []ai.Tool, timestamp int64) []durable.EntryDraft {
	head := view.Head
	if head != nil {
		later := false
		for i := range view.Entries {
			if durable.SystemEntry.Is(&view.Entries[i]) && view.Entries[i].ID > head.ID {
				later = true
				break
			}
		}
		if !later {
			var edits []durable.ContextEdit
			for i := range view.Entries {
				if durable.SystemEntry.Is(&view.Entries[i]) {
					edits = append(edits, durable.ContextEdit{Target: view.Entries[i].ID, Action: durable.ContextEditOmit})
				}
			}
			added := make([]ai.Tool, len(tools))
			for i, tool := range tools {
				added[i] = ai.ToToolDeclaration(tool)
			}
			baseline := systemEntry(fullSections(desired), toolChanges{added: added}, timestamp)
			baseline.Edits = edits
			return []durable.EntryDraft{baseline}
		}
	}
	patches := planSections(replaySections(view.Messages), desired)
	changes := planTools(ai.GetCurrentTools(view.Messages), tools)
	if len(changes.removed) == 0 && len(changes.added) == 0 {
		drafts := make([]durable.EntryDraft, len(patches))
		for i, patch := range patches {
			drafts[i] = systemEntry(patch, toolChanges{}, timestamp)
		}
		return drafts
	}
	if len(patches) == 0 {
		return []durable.EntryDraft{systemEntry(nil, changes, timestamp)}
	}
	drafts := make([]durable.EntryDraft, len(patches))
	for i, patch := range patches {
		if i == len(patches)-1 {
			drafts[i] = systemEntry(patch, changes, timestamp)
		} else {
			drafts[i] = systemEntry(patch, toolChanges{}, timestamp)
		}
	}
	return drafts
}

type toolChanges struct {
	removed []ai.ToolReference
	added   []ai.Tool
}

// planTools returns the tool changes from offered to desired. A changed
// declaration is removed and re-added. Replay keeps retained tools in place
// and appends additions; when that would not yield the desired order, every
// offered tool is removed and every desired tool re-added in order.
func planTools(offered, desired []ai.Tool) toolChanges {
	wanted := map[string]ai.Tool{}
	for _, tool := range desired {
		wanted[tool.Name] = tool
	}
	var kept []ai.Tool
	keptNames := map[string]bool{}
	for _, tool := range offered {
		if next, ok := wanted[tool.Name]; ok && ai.DeclarationsEqual(tool, next) {
			kept = append(kept, tool)
			keptNames[tool.Name] = true
		}
	}
	var added []ai.Tool
	for _, tool := range desired {
		if !keptNames[tool.Name] {
			added = append(added, tool)
		}
	}
	replayed := append(append([]ai.Tool(nil), kept...), added...)
	reorder := false
	for i, tool := range replayed {
		if tool.Name != desired[i].Name {
			reorder = true
			break
		}
	}
	var changes toolChanges
	if reorder {
		for _, tool := range offered {
			changes.removed = append(changes.removed, ai.ToolReference{Name: tool.Name})
		}
		for _, tool := range desired {
			changes.added = append(changes.added, ai.ToToolDeclaration(tool))
		}
		return changes
	}
	for _, tool := range offered {
		if !keptNames[tool.Name] {
			changes.removed = append(changes.removed, ai.ToolReference{Name: tool.Name})
		}
	}
	for _, tool := range added {
		changes.added = append(changes.added, ai.ToToolDeclaration(tool))
	}
	return changes
}

// planSections returns the section patches: none, the minimal patch, or a
// remove-all/re-add-all pair when the order would differ.
func planSections(shown, desired *ordered[string]) []ai.SystemSections {
	var patchedOrder []string
	for _, key := range shown.keys {
		if _, ok := desired.get(key); ok {
			patchedOrder = append(patchedOrder, key)
		}
	}
	for _, key := range desired.keys {
		if _, ok := shown.get(key); !ok {
			patchedOrder = append(patchedOrder, key)
		}
	}
	for i, key := range patchedOrder {
		if key != desired.keys[i] {
			removeAll := ai.SystemSections{}
			for _, key := range shown.keys {
				removeAll = append(removeAll, ai.SystemSection{Name: key})
			}
			return []ai.SystemSections{removeAll, fullSections(desired)}
		}
	}

	patch := ai.SystemSections{}
	for _, key := range shown.keys {
		value, _ := shown.get(key)
		next, ok := desired.get(key)
		switch {
		case !ok:
			patch = append(patch, ai.SystemSection{Name: key})
		case next != value:
			patch = append(patch, ai.SystemSection{Name: key, Value: &next})
		}
	}
	for _, key := range desired.keys {
		if _, ok := shown.get(key); !ok {
			value, _ := desired.get(key)
			patch = append(patch, ai.SystemSection{Name: key, Value: &value})
		}
	}
	if len(patch) == 0 {
		return nil
	}
	return []ai.SystemSections{patch}
}

// fullSections returns every section of desired, in order; never nil.
func fullSections(desired *ordered[string]) ai.SystemSections {
	sections := ai.SystemSections{}
	for _, key := range desired.keys {
		value, _ := desired.get(key)
		sections = append(sections, ai.SystemSection{Name: key, Value: &value})
	}
	return sections
}

func systemEntry(sections ai.SystemSections, tools toolChanges, timestamp int64) durable.EntryDraft {
	message := ai.SystemMessage{Sections: sections, Timestamp: timestamp}
	if len(tools.removed) > 0 {
		message.ToolsRemoved = tools.removed
	}
	if len(tools.added) > 0 {
		message.ToolsAdded = tools.added
	}
	return durable.EntryDraft{Kind: durable.SystemEntry.Kind, Model: []ai.Message{message}}
}

// systemMessageOf returns message as a system message.
func systemMessageOf(message ai.Message) (ai.SystemMessage, bool) {
	switch m := message.(type) {
	case ai.SystemMessage:
		return m, true
	case *ai.SystemMessage:
		if m != nil {
			return *m, true
		}
	}
	return ai.SystemMessage{}, false
}
