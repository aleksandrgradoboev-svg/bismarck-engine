package providers

import (
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// pi 7f29e7a3: getProviderEnvValue prefers a non-empty scoped override, else the
// OS environment; an empty override falls through.
func TestGetProviderEnvValue(t *testing.T) {
	t.Setenv("PI_TEST_VAR", "os-value")
	if got := getProviderEnvValue("PI_TEST_VAR", map[string]string{"PI_TEST_VAR": "scoped"}); got != "scoped" {
		t.Fatalf("scoped override should win, got %q", got)
	}
	if got := getProviderEnvValue("PI_TEST_VAR", map[string]string{"PI_TEST_VAR": ""}); got != "os-value" {
		t.Fatalf("empty override should fall through to OS env, got %q", got)
	}
	if got := getProviderEnvValue("PI_TEST_VAR", nil); got != "os-value" {
		t.Fatalf("nil env should read OS env, got %q", got)
	}
	if got := getProviderEnvValue("PI_TEST_ABSENT", nil); got != "" {
		t.Fatalf("absent var should be empty, got %q", got)
	}
}

// resolveCacheRetention consults the scoped env for PI_CACHE_RETENTION ahead of
// the OS environment, but an explicit retention always wins.
func TestResolveCacheRetentionScopedEnv(t *testing.T) {
	t.Setenv("PI_CACHE_RETENTION", "") // not "long" in OS env

	if got := resolveCacheRetention("", map[string]string{"PI_CACHE_RETENTION": "long"}); got != ai.CacheLong {
		t.Fatalf("scoped PI_CACHE_RETENTION=long should yield long, got %q", got)
	}
	if got := resolveCacheRetention("", nil); got != ai.CacheShort {
		t.Fatalf("default (no env) should be short, got %q", got)
	}
	// An explicit retention is never overridden by env.
	if got := resolveCacheRetention(ai.CacheNone, map[string]string{"PI_CACHE_RETENTION": "long"}); got != ai.CacheNone {
		t.Fatalf("explicit retention must win over env, got %q", got)
	}
}
