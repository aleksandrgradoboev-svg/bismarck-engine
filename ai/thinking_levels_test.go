package ai

import (
	"reflect"
	"testing"
)

// Mirrors pi packages/ai/test/max-thinking.test.ts (upstream fbdd4638): the
// "max" reasoning level is opt-in, gated on an explicit thinkingLevelMap entry
// exactly like "xhigh".

func TestMaxThinkingLevelIsOptIn(t *testing.T) {
	// An ordinary reasoning model (no thinkingLevelMap) exposes neither xhigh nor
	// max, and a requested "max" clamps down to "high".
	model := &Model{Reasoning: true}
	got := GetSupportedThinkingLevels(model)
	want := []ModelThinkingLevel{"off", "minimal", "low", "medium", "high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetSupportedThinkingLevels = %v, want %v", got, want)
	}
	if clamped := ClampThinkingLevel(model, "max"); clamped != "high" {
		t.Fatalf("ClampThinkingLevel(max) = %q, want %q", clamped, "high")
	}
}

func TestThinkingLevelHoleBetweenHighAndMax(t *testing.T) {
	// xhigh explicitly unsupported (null), max supported: the model skips xhigh
	// but still exposes max, and a requested "xhigh" clamps UP to "max".
	maxVal := "max"
	model := &Model{
		Reasoning:        true,
		ThinkingLevelMap: ThinkingLevelMap{"xhigh": nil, "max": &maxVal},
	}
	got := GetSupportedThinkingLevels(model)
	want := []ModelThinkingLevel{"off", "minimal", "low", "medium", "high", "max"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetSupportedThinkingLevels = %v, want %v", got, want)
	}
	if clamped := ClampThinkingLevel(model, "xhigh"); clamped != "max" {
		t.Fatalf("ClampThinkingLevel(xhigh) = %q, want %q", clamped, "max")
	}
}
