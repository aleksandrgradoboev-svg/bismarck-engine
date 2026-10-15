package ai

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

func TestCatalogLoads(t *testing.T) {
	// pi-ai 0.80.6 pruned all legacy claude-3.x / claude-4-0 models from the
	// anthropic catalog; smoke-test loading against a dated, still-present model.
	m := GetModel("anthropic", "claude-haiku-4-5-20251001")
	if m == nil || m.MaxTokens != 64000 {
		t.Fatalf("haiku-4-5 not loaded: %#v", m)
	}
	if len(GetProviders()) < 10 {
		t.Fatalf("too few providers: %d", len(GetProviders()))
	}
}

// pi's getBuiltinProviders is Object.keys(MODELS), so a catalog key with no
// chat models is still a built-in provider: `typesafe` serves only classifiers
// and appears in MODELS as {} since a328aa89a. GetProviders must list it too;
// otherwise every loop over the providers silently skips it.
func TestGetProvidersListsEveryCatalogKey(t *testing.T) {
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(modelsCatalogJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(maps.Keys(catalog))
	got := GetProviders()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("GetProviders() = %v\nwant the catalog's keys %v", got, want)
	}
}
