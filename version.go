// Package engine carries what belongs to the engine as a whole rather than to
// one of its packages.
package engine

import (
	"runtime/debug"
	"strings"
)

// modulePath is this module's path, as go.mod declares it.
const modulePath = "github.com/aleksandrgradoboev-svg/bismarck-engine"

// Version is the engine's version without the leading "v", read from the
// build information: the main module's version when the engine itself is
// built, the dependency's version when another module builds it in. Without
// one — a build outside version control, a test binary — it is "0.0.0", the
// fallback pi uses for VERSION (config.ts).
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "0.0.0"
	}
	return versionFrom(info)
}

// versionFrom picks the module's version out of info.
func versionFrom(info *debug.BuildInfo) string {
	version := "0.0.0"
	for _, dep := range append([]*debug.Module{&info.Main}, info.Deps...) {
		if dep == nil || dep.Path != modulePath {
			continue
		}
		if dep.Replace != nil && dep.Replace.Version != "" {
			dep = dep.Replace
		}
		if dep.Version != "" && dep.Version != "(devel)" {
			version = strings.TrimPrefix(dep.Version, "v")
		}
	}
	return version
}
