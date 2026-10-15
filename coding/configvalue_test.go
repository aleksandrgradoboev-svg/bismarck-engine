package coding

import (
	"strings"
	"testing"
)

// pi resolve-config-value.ts: environment interpolation, escapes, literals.
func TestResolveConfigValueTemplates(t *testing.T) {
	t.Setenv("PI_CFG_TOKEN", "abc")
	for _, tc := range []struct{ in, want string }{
		{"literal", "literal"},
		{"$PI_CFG_TOKEN", "abc"},
		{"Bearer ${PI_CFG_TOKEN}", "Bearer abc"},
		{"a$${PI_CFG_TOKEN}", "a${PI_CFG_TOKEN}"},
		{"$!not a command", "!not a command"},
		{"${not-a-name}", "${not-a-name}"},
		{"cost $5", "cost $5"},
		{"tail $", "tail $"},
		{"${unterminated", "${unterminated"},
	} {
		got, ok := ResolveConfigValue(tc.in, nil)
		if !ok || got != tc.want {
			t.Errorf("ResolveConfigValue(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
		}
	}
	if got, ok := ResolveConfigValue("${PI_CFG_FROM_MAP}", map[string]string{"PI_CFG_FROM_MAP": "m"}); !ok || got != "m" {
		t.Errorf("env map value = %q, %v", got, ok)
	}
}

func TestResolveConfigValueOrErrorNamesMissingVariables(t *testing.T) {
	_, err := ResolveConfigValueOrError("${PI_CFG_MISSING_A}-${PI_CFG_MISSING_B}", `MCP server "x" header "A"`, nil)
	if err == nil || err.Error() != `Failed to resolve MCP server "x" header "A" from environment variables: PI_CFG_MISSING_A, PI_CFG_MISSING_B` {
		t.Fatalf("err = %v", err)
	}
	_, err = ResolveConfigValueOrError("$PI_CFG_MISSING_A", "key", nil)
	if err == nil || !strings.HasSuffix(err.Error(), "from environment variable: PI_CFG_MISSING_A") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveConfigValueRunsCommands(t *testing.T) {
	got, ok := ResolveConfigValue("!echo from-shell", nil)
	if !ok || got != "from-shell" {
		t.Fatalf("command value = %q, %v", got, ok)
	}
	if _, err := ResolveConfigValueOrError("!exit 3", "key", nil); err == nil || !strings.Contains(err.Error(), "from shell command: exit 3") {
		t.Fatalf("failing command err = %v", err)
	}
}
