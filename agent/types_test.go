package agent

import (
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// TestAgentToolForwardsConstrainedSampling: upstream 24bace27 threads
// `constrainedSampling` through tool-definition-wrapper.ts, which in TypeScript
// reaches AgentTool for free because `AgentTool<T> extends Tool<T>`. Go has no
// such inheritance, and asAITool is the ONLY path from a tool to the provider
// request — so without an explicit field the whole constrained-sampling feature
// is implemented in the ai layer but unreachable from agent/ and coding/.
func TestAgentToolForwardsConstrainedSampling(t *testing.T) {
	cfg := &ai.ConstrainedSamplingConfig{
		Type:   ai.ConstrainedSamplingJSONSchema,
		Strict: ai.ConstrainedSamplingRequire,
	}
	got := AgentTool{Name: "read", Description: "d", ConstrainedSampling: cfg}.asAITool()
	if got.ConstrainedSampling == nil {
		t.Fatal("asAITool dropped ConstrainedSampling; the ai-layer feature is unreachable")
	}
	if got.ConstrainedSampling.Type != ai.ConstrainedSamplingJSONSchema ||
		got.ConstrainedSampling.Strict != ai.ConstrainedSamplingRequire {
		t.Errorf("config mangled in transit: %+v", got.ConstrainedSampling)
	}
	// A tool that asks for nothing must still carry nothing.
	if plain := (AgentTool{Name: "x"}).asAITool(); plain.ConstrainedSampling != nil {
		t.Errorf("unset ConstrainedSampling should stay nil, got %+v", plain.ConstrainedSampling)
	}
}
