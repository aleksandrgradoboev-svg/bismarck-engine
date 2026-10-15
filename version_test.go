package engine

import (
	"runtime/debug"
	"testing"
)

func TestVersionFrom(t *testing.T) {
	cases := []struct {
		name string
		info debug.BuildInfo
		want string
	}{
		{"main module", debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.1.1"}}, "0.1.1"},
		{"devel", debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, "0.0.0"},
		{"dependency", debug.BuildInfo{
			Main: debug.Module{Path: "example.com/app", Version: "v2.0.0"},
			Deps: []*debug.Module{{Path: "example.com/other", Version: "v9.9.9"}, {Path: modulePath, Version: "v0.1.0"}},
		}, "0.1.0"},
		{"pseudo-version", debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.1.1-0.20261015120000-abcdef123456+dirty"}},
			"0.1.1-0.20261015120000-abcdef123456+dirty"},
		{"replaced by a version", debug.BuildInfo{
			Main: debug.Module{Path: "example.com/app"},
			Deps: []*debug.Module{{Path: modulePath, Version: "v0.1.0", Replace: &debug.Module{Path: "example.com/fork", Version: "v0.1.5"}}},
		}, "0.1.5"},
		{"replaced by a directory", debug.BuildInfo{
			Main: debug.Module{Path: "example.com/app"},
			Deps: []*debug.Module{{Path: modulePath, Version: "v0.1.0", Replace: &debug.Module{Path: "../bismarck-engine"}}},
		}, "0.1.0"},
		{"absent", debug.BuildInfo{Main: debug.Module{Path: "example.com/app", Version: "v2.0.0"}}, "0.0.0"},
		{"nil dependency", debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}, Deps: []*debug.Module{nil}}, "0.0.0"},
	}
	for _, c := range cases {
		if got := versionFrom(&c.info); got != c.want {
			t.Errorf("%s: versionFrom = %q, want %q", c.name, got, c.want)
		}
	}
}

// A test binary carries no version of its own, so Version falls back.
func TestVersionInTestBinary(t *testing.T) {
	if got := Version(); got != "0.0.0" {
		t.Fatalf("Version() = %q in a test binary, want the fallback 0.0.0", got)
	}
}
