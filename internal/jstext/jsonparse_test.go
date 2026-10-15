package jstext

import (
	"encoding/json"
	"os"
	"testing"
)

// TestJSONSyntaxErrorMatchesNode compares JSONSyntaxError with node's
// JSON.parse over testdata/json-parse-errors-node.json, captured by
// testdata/capture-json-parse-errors.mjs: the exact SyntaxError message for
// every input JSON.parse rejects, and nil for every input it accepts.
func TestJSONSyntaxErrorMatchesNode(t *testing.T) {
	data, err := os.ReadFile("testdata/json-parse-errors-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Node string       `json:"node"`
		Rows [][2]*string `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) < 2000 {
		t.Fatalf("only %d rows in testdata/json-parse-errors-node.json", len(capture.Rows))
	}
	failed := 0
	for _, row := range capture.Rows {
		input, want := *row[0], row[1]
		got := JSONSyntaxError(input)
		switch {
		case want == nil && got != nil:
			t.Errorf("JSONSyntaxError(%q) = %q, node %s accepts it", input, got, capture.Node)
		case want != nil && (got == nil || got.Error() != *want):
			t.Errorf("JSONSyntaxError(%q) = %v\nnode %s:   %q", input, got, capture.Node, *want)
		default:
			continue
		}
		if failed++; failed == 20 {
			t.Fatal("too many mismatches")
		}
	}
}
