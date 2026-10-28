package ai

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// tupleItemsCaptureFile is written by testdata/tuple-items/capture.mts from
// pi's utils/validation.ts at 59ca40cf (typebox 1.3.27).
const tupleItemsCaptureFile = "testdata/tuple-items/tuple-items-59ca40cf.json"

// TestValidateToolArgumentsTupleItemsMatchesPi pins the positional array
// keywords — tuple "items", "prefixItems", "additionalItems" — to what real
// pi's validateToolArguments returns or throws for the same parameters and
// arguments: the coerced arguments, or the whole error message.
func TestValidateToolArgumentsTupleItemsMatchesPi(t *testing.T) {
	raw, err := os.ReadFile(tupleItemsCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/tuple-items/capture.mts)", tupleItemsCaptureFile, err)
	}
	var capture struct {
		Cases map[string]struct {
			Parameters string  `json:"parameters"`
			Arguments  string  `json:"arguments"`
			Result     *string `json:"result"`
			Error      *string `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Cases) == 0 {
		t.Fatal("capture has no cases")
	}
	for name, c := range capture.Cases {
		t.Run(name, func(t *testing.T) {
			var params Schema
			if err := json.Unmarshal([]byte(c.Parameters), &params); err != nil {
				t.Fatalf("parameters: %v", err)
			}
			var call ToolCall
			if err := json.Unmarshal([]byte(`{"id":"call_1","name":"test_tool","arguments":`+c.Arguments+`}`), &call); err != nil {
				t.Fatalf("arguments: %v", err)
			}
			tool := Tool{Name: "test_tool", Description: "A test tool", Parameters: &params}
			got, err := ValidateToolArguments(tool, call)
			switch {
			case c.Error != nil:
				if err == nil {
					t.Fatalf("pi threw, Go returned %v\npi: %s", got, *c.Error)
				}
				if err.Error() != *c.Error {
					t.Fatalf("error mismatch\n got: %s\nwant: %s", err, *c.Error)
				}
			case c.Result != nil:
				if err != nil {
					t.Fatalf("pi returned %s, Go threw: %v", *c.Result, err)
				}
				var want map[string]any
				if err := json.Unmarshal([]byte(*c.Result), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("arguments mismatch\n got: %#v\nwant: %#v", got, want)
				}
			default:
				t.Fatal("case has neither result nor error")
			}
		})
	}
}
