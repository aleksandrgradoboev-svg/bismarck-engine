package sqlite

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Tables are the durable schema's table names (pi schema.ts TABLE_NAMES).
// Never change an existing name: a rename adds a field, so historical
// migrations keep rendering their original names.
type Tables struct {
	DurableSchema     string
	DurableMetadata   string
	RecordIDs         string
	Conversations     string
	Entries           string
	Tasks             string
	Submissions       string
	Documents         string
	DocumentRevisions string
}

// Indexes are the durable schema's index names (pi schema.ts INDEX_NAMES).
type Indexes struct {
	ConversationsByOwnerConversation string
	ConversationsByOwnerTask         string
	EntriesByConversation            string
	EntryHeadsByConversation         string
	TasksByStatus                    string
	TasksByConversation              string
	TasksByKind                      string
	TasksByAbortRequested            string
	TasksByBackground                string
	SubmissionsByRequest             string
	SubmissionsByConversation        string
	SubmissionsByStatus              string
	DocumentsByAddress               string
	DocumentsByScope                 string
	DocumentsByScopeKind             string
	DocumentRevisionsByKind          string
}

// Schema is the identifiers every migration and storage query uses.
type Schema struct {
	Tables  Tables
	Indexes Indexes
}

var prefixPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidatePrefix checks a prefix before it becomes part of SQLite
// identifiers.
func ValidatePrefix(prefix string) error {
	if !prefixPattern.MatchString(prefix) {
		return fmt.Errorf("Invalid SQLite schema prefix %s", strconv.Quote(prefix))
	}
	if strings.HasPrefix(strings.ToLower(prefix), "sqlite_") {
		return fmt.Errorf("SQLite schema prefix %s is reserved", strconv.Quote(prefix))
	}
	return nil
}

// NewSchema builds the identifiers under a prefix; "" is no prefix.
func NewSchema(prefix string) (Schema, error) {
	if prefix != "" {
		if err := ValidatePrefix(prefix); err != nil {
			return Schema{}, err
		}
	}
	p := func(name string) string { return prefix + name }
	return Schema{
		Tables: Tables{
			DurableSchema:     p("durable_schema"),
			DurableMetadata:   p("durable_metadata"),
			RecordIDs:         p("record_ids"),
			Conversations:     p("conversations"),
			Entries:           p("entries"),
			Tasks:             p("tasks"),
			Submissions:       p("submissions"),
			Documents:         p("documents"),
			DocumentRevisions: p("document_revisions"),
		},
		Indexes: Indexes{
			ConversationsByOwnerConversation: p("conversations_by_owner_conversation"),
			ConversationsByOwnerTask:         p("conversations_by_owner_task"),
			EntriesByConversation:            p("entries_by_conversation"),
			EntryHeadsByConversation:         p("entry_heads_by_conversation"),
			TasksByStatus:                    p("tasks_by_status"),
			TasksByConversation:              p("tasks_by_conversation"),
			TasksByKind:                      p("tasks_by_kind"),
			TasksByAbortRequested:            p("tasks_by_abort_requested"),
			TasksByBackground:                p("tasks_by_background"),
			SubmissionsByRequest:             p("submissions_by_request"),
			SubmissionsByConversation:        p("submissions_by_conversation"),
			SubmissionsByStatus:              p("submissions_by_status"),
			DocumentsByAddress:               p("documents_by_address"),
			DocumentsByScope:                 p("documents_by_scope"),
			DocumentsByScopeKind:             p("documents_by_scope_kind"),
			DocumentRevisionsByKind:          p("document_revisions_by_kind"),
		},
	}, nil
}

// DefaultSchema is the schema without a prefix.
var DefaultSchema, _ = NewSchema("")
