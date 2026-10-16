package sqlite

import (
	"strings"
	"testing"
)

// TestIndexedStringMatchesJSONStringify: indexed columns hold pi's
// JSON.stringify of a string, so a database pi wrote is searched by the
// same bytes. The expectations were taken from Node 24's JSON.stringify;
// encoding/json differs on <, >, & and U+2028/U+2029.
func TestIndexedStringMatchesJSONStringify(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a<b>&c", `"a<b>&c"`},
		{"  ", "\"  \""},
		{"\x00\x01\x1f\x7f\u0080", `"\u0000\u0001\u001f` + "\x7f\u0080" + `"`},
		{"tab\there\nnl\r\b\f\v", `"tab\there\nnl\r\b\f\u000b"`},
		{`q"b\s/`, `"q\"b\\s/"`},
		{"кречет \U0001F985", "\"кречет \U0001F985\""},
		{"", `""`},
	} {
		if got := indexedString(c.in); got != c.want {
			t.Errorf("indexedString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidatesSchemaPrefixes(t *testing.T) {
	for _, prefix := range []string{"pi_", "_x", "A9"} {
		if err := ValidatePrefix(prefix); err != nil {
			t.Errorf("ValidatePrefix(%q): %v", prefix, err)
		}
	}
	for prefix, want := range map[string]string{
		"bad-prefix": `Invalid SQLite schema prefix "bad-prefix"`,
		"9x":         "Invalid SQLite schema prefix",
		"SQLite_x":   `SQLite schema prefix "SQLite_x" is reserved`,
	} {
		if err := ValidatePrefix(prefix); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidatePrefix(%q) = %v, want %q", prefix, err, want)
		}
	}
	schema, err := NewSchema("pi_")
	if err != nil || schema.Tables.Tasks != "pi_tasks" || schema.Indexes.DocumentRevisionsByKind != "pi_document_revisions_by_kind" {
		t.Errorf("NewSchema(pi_) = %+v, %v", schema, err)
	}
}
