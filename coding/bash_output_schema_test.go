package coding

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// The bash output schema is pi's bashOutputSchema (bash.ts, v1.2.0, as
// upstream 6f1072cc shortened it): codemode renders it into the bash
// declaration scripts read, so its descriptions are model-facing text.
func TestBashOutputSchemaMatchesPi(t *testing.T) {
	raw, err := json.Marshal(bashOutputSchema())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]any{
		"output":            {"type": "string", "description": "Combined stdout and stderr, possibly truncated"},
		"truncated":         {"type": "boolean"},
		"full_output_path":  {"type": "string", "description": "Full output, when truncated"},
		"exit_code":         {"type": "number"},
		"wall_time_seconds": {"type": "number"},
	}
	if got.Type != "object" || !reflect.DeepEqual(got.Properties, want) {
		t.Fatalf("schema %s", raw)
	}
	required := slices.Clone(got.Required)
	slices.Sort(required)
	if want := []string{"exit_code", "output", "truncated", "wall_time_seconds"}; !slices.Equal(required, want) {
		t.Fatalf("required %v, want %v", got.Required, want)
	}
}
