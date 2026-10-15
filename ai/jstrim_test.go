package ai

import (
	"encoding/json"
	"os"
	"testing"
)

// pi's credential lookups call a value blank when String.prototype.trim leaves
// nothing, which strips U+FEFF and keeps U+0085 — the reverse of
// strings.TrimSpace. The expectations are pi's own, captured under node at
// 7140838fd by testdata/jstrim/capture-jstrim.mts.

type jstrimCredentialCapture struct {
	Env []struct {
		Value  string  `json:"value"`
		Result *string `json:"result"`
	} `json:"env"`
	ExplicitKey []struct {
		APIKey   string `json:"apiKey"`
		Explicit bool   `json:"explicit"`
	} `json:"explicitKey"`
}

func loadJSTrimCredentialCapture(t *testing.T) jstrimCredentialCapture {
	t.Helper()
	const file = "testdata/jstrim/jstrim-7140838fd.json"
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var c jstrimCredentialCapture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("%s: %v; rerun capture-jstrim.mts", file, err)
	}
	if len(c.Env) == 0 || len(c.ExplicitKey) == 0 {
		t.Fatalf("%s carries no cases; rerun capture-jstrim.mts", file)
	}
	return c
}

func TestJSTrimDefaultAuthContextEnv(t *testing.T) {
	const name = "PI_JSTRIM_CAPTURE"
	for _, tc := range loadJSTrimCredentialCapture(t).Env {
		t.Setenv(name, tc.Value)
		want := ""
		if tc.Result != nil {
			want = *tc.Result
		}
		if got := DefaultProviderAuthContext().Env(name); got != want {
			t.Errorf("Env(%+q) = %+q, pi = %+q", tc.Value, got, want)
		}
	}
}

// hasExplicitAPIKey decides whether a request's own key stands or the
// provider's environment key replaces it.
func TestJSTrimExplicitAPIKey(t *testing.T) {
	for _, tc := range loadJSTrimCredentialCapture(t).ExplicitKey {
		if got := hasExplicitAPIKey(tc.APIKey); got != tc.Explicit {
			t.Errorf("hasExplicitAPIKey(%+q) = %v, pi = %v", tc.APIKey, got, tc.Explicit)
		}
	}
}
