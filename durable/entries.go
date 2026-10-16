package durable

import "errors"

// EntryKind names one entry kind; its Is reports whether a record has that
// kind. pi's defineEntry token, without the data type parameter Go cannot
// narrow by.
type EntryKind struct{ Kind string }

// DefineEntry returns the entry kind for kind, which must not be empty.
func DefineEntry(kind string) (EntryKind, error) {
	if kind == "" {
		return EntryKind{}, errors.New("Entry kind must be a non-empty string")
	}
	return EntryKind{Kind: kind}, nil
}

// MustDefineEntry is DefineEntry for package-level kinds, panicking on an
// empty kind.
func MustDefineEntry(kind string) EntryKind {
	k, err := DefineEntry(kind)
	if err != nil {
		panic(err)
	}
	return k
}

// Is reports whether entry is present and has this kind.
func (k EntryKind) Is(entry *EntryRecord) bool { return entry != nil && entry.Kind == k.Kind }

// The built-in entry kinds.
var (
	// UserEntry is user input: Model is [UserMessage]. Written by submissions.
	UserEntry = MustDefineEntry("pi.user")
	// AssistantEntry is a provider result with any stop reason: Model is
	// [AssistantMessage]. Written by generation.
	AssistantEntry = MustDefineEntry("pi.assistant")
	// SystemEntry is a positional prompt and tool change: Model is
	// [SystemMessage] with empty content.
	SystemEntry = MustDefineEntry("pi.system")
	// ToolResultEntry is a tool result: Model is [ToolResultMessage], whose
	// content ends with the rendered diagnostics block; Data holds the
	// structured diagnostics, possibly none. Written by tool tasks, and by
	// generation for calls it did not offer.
	ToolResultEntry = MustDefineEntry("pi.tool-result")
	// ResetEntry starts a new context: always head "self", with Model absent
	// for a plain reset or [UserMessage] carrying the handoff text.
	ResetEntry = MustDefineEntry("pi.reset")
	// CompactionEntry is a compaction summary: Model is [UserMessage] with the
	// wrapped summary, Head the first kept entry.
	CompactionEntry = MustDefineEntry("pi.compaction")
)
