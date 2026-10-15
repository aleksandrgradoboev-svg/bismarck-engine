package coding

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Port of upstream test/experimental.test.ts (66335d3a): only the exact value
// "1" enables experimental features.
func TestAreExperimentalFeaturesEnabled(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		want  bool
	}{
		{name: "unset", set: false, want: false},
		{name: "empty", set: true, value: "", want: false},
		{name: "one", set: true, value: "1", want: true},
		{name: "zero", set: true, value: "0", want: false},
		{name: "non-1 value", set: true, value: "true", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("PI_EXPERIMENTAL", tc.value)
			} else {
				// t.Setenv registers the restore, then the variable is removed
				// to exercise the truly-unset case.
				t.Setenv("PI_EXPERIMENTAL", "")
				if err := os.Unsetenv("PI_EXPERIMENTAL"); err != nil {
					t.Fatal(err)
				}
			}
			if got := AreExperimentalFeaturesEnabled(); got != tc.want {
				t.Fatalf("AreExperimentalFeaturesEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Port of builtin-tool-strict-mode.test.ts (upstream fcff255b0, which replaced
// experimental-tool-strict-mode.test.ts): the built-in read/bash/powershell/
// edit/write tools prefer strict constrained sampling by DEFAULT, whatever
// PI_EXPERIMENTAL says, while grep/find/ls stay unconstrained. Strictness is a
// provider-side conversion, so it must never touch the execution schema.
//
// Upstream's two remaining cases have no Go home and are deliberately not
// ported: the wrapToolDefinition opt-out and the extension re-registration case
// are both extension surface (E1).
func TestBuiltInToolsPreferStrictSampling(t *testing.T) {
	strict := map[string]bool{"read": true, "bash": true, "powershell": true, "edit": true, "write": true}
	unconstrained := map[string]bool{"grep": true, "find": true, "ls": true}
	want := &ai.ConstrainedSamplingConfig{
		Type: ai.ConstrainedSamplingJSONSchema, Strict: ai.ConstrainedSamplingPrefer,
	}

	for _, experimental := range []string{"unset", "0", "1"} {
		t.Run("PI_EXPERIMENTAL="+experimental, func(t *testing.T) {
			// t.Setenv registers the restore before the variable is removed to
			// exercise the truly-unset case.
			t.Setenv("PI_EXPERIMENTAL", "")
			if experimental == "unset" {
				if err := os.Unsetenv("PI_EXPERIMENTAL"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("PI_EXPERIMENTAL", experimental)
			}

			tools := CreateAllTools(t.TempDir())
			seen := map[string]bool{}
			for _, tool := range tools {
				seen[tool.Name] = true
				switch {
				case strict[tool.Name]:
					if cs := tool.ConstrainedSampling; cs == nil || *cs != *want {
						t.Errorf("tool %s must prefer strict sampling by default: %#v", tool.Name, cs)
					}
				case unconstrained[tool.Name]:
					if cs := tool.ConstrainedSampling; cs != nil {
						t.Errorf("tool %s must stay unconstrained: %#v", tool.Name, cs)
					}
				default:
					t.Errorf("tool %s is in neither bucket; update this test", tool.Name)
				}
			}
			for name := range strict {
				if !seen[name] {
					t.Errorf("CreateAllTools omitted strict tool %s", name)
				}
			}

			// Strictness is a provider-side conversion, not a change to the
			// execution schema.
			required := func(name string) []string {
				tool, err := CreateTool(name, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				var schema struct {
					Required []string `json:"required"`
				}
				raw, err := json.Marshal(tool.Parameters)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &schema); err != nil {
					t.Fatal(err)
				}
				return schema.Required
			}
			if got := required("read"); len(got) != 1 || got[0] != "path" {
				t.Errorf("read.required = %v, want [path]", got)
			}
			if got := required("bash"); len(got) != 1 || got[0] != "command" {
				t.Errorf("bash.required = %v, want [command]", got)
			}
		})
	}
}
