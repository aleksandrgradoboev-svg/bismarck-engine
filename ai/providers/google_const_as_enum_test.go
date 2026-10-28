package providers

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Gemini ignores `const` in parametersJsonSchema but honors `enum`, so string
// and number consts go as single-value enums, rewritten only inside
// schema-bearing keywords (pi 7314819f, a7b94a03, #11308). The cases are pi's
// from google-shared-convert-tools.test.ts plus this port's edges; pi's
// parametersJsonSchema for each is captured under node at a7b94a03 by
// testdata/google-tools/capture-const-as-enum.mts as JSON.stringify writes it,
// so the key order is compared too.

const googleConstAsEnumCaptureFile = "testdata/google-tools/const-as-enum-a7b94a03.json"

func TestGoogleParametersJSONSchemaConstAsEnumMatchesPi(t *testing.T) {
	data, err := os.ReadFile(googleConstAsEnumCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture map[string]json.RawMessage
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range capture {
		if name != "sha" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) != 14 {
		t.Fatalf("%d cases in the capture, want 14", len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			var c struct {
				Parameters           string `json:"parameters"`
				ParametersJSONSchema string `json:"parametersJsonSchema"`
			}
			if err := json.Unmarshal(capture[name], &c); err != nil {
				t.Fatal(err)
			}
			var parameters ai.Schema
			if err := json.Unmarshal([]byte(c.Parameters), &parameters); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(&parameters)
			tools, err := googleTools([]ai.Tool{{Name: "test_tool", Description: "A test tool", Parameters: &parameters}}, false, false)
			if err != nil {
				t.Fatal(err)
			}
			decl := tools[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
			got, err := json.Marshal(decl["parametersJsonSchema"])
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.ParametersJSONSchema {
				t.Fatalf("parametersJsonSchema\n got %s\nwant %s", got, c.ParametersJSONSchema)
			}
			// The tool's own schema is left as it was.
			if after, _ := json.Marshal(&parameters); string(after) != string(before) {
				t.Fatalf("the tool's schema changed:\n%s\nthen\n%s", before, after)
			}
		})
	}
}
