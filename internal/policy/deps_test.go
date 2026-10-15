// Package policy holds module-wide invariant tests. It carries no non-test
// source and nothing imports it; it exists so the port's dependency rules are
// enforced by `go test ./...` — which the base gate already runs every cycle —
// rather than by prose in docs/UPSTREAM.md that nothing checks.
package policy

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// allowedModules is the complete set of non-stdlib modules the port's build may
// reach, mapped to why each is here. docs/UPSTREAM.md "E2" makes adding a
// third-party module an owner CONSULT, so an edit to this map should accompany
// a recorded ruling rather than stand on its own.
var allowedModules = map[string]string{
	"golang.org/x/image": "BMP/WebP decoder registration (coding/imageresize.go)",
	"golang.org/x/text":  "collation and Unicode normalization (coding/tools.go, coding/editmatch.go), and JavaScript's toLowerCase (internal/jstext/lower.go)",
	// Owner ruling 2026-10-15, docs/UPSTREAM.md E2: codemode runs pi's QuickJS
	// wasm sandbox on wazero, in the root module.
	"github.com/tetratelabs/wazero": "WebAssembly runtime for the codemode QuickJS sandbox (codemode/quickjs); pure Go, Apache-2.0",
	"golang.org/x/sys":              "reached through wazero (platform memory mapping for its compiler)",
}

// ownModule is skipped by the allowlist check; its own packages are still
// checked for cgo, so a stray `import "C"` in port code fails too.
const ownModule = "github.com/aleksandrgradoboev-svg/bismarck-engine"

// depPackage is one package in the transitive build graph. Module is empty for
// the standard library.
type depPackage struct {
	ImportPath string
	Module     string
	CgoFiles   int
}

// listDeps enumerates every package the module's build reaches.
//
// It forces CGO_ENABLED=1, and that is the whole point rather than an
// incidental detail: with cgo disabled the toolchain excludes cgo files by
// build constraint, so a cgo-requiring dependency reports zero CgoFiles and
// this check would pass while the dependency is very much present. Drivers
// such as mattn/go-sqlite3 additionally ship a build-tagged stub, so even
// `CGO_ENABLED=0 go build ./...` succeeds and produces a binary that fails
// only at run time. Neither a cgo-off build nor a cgo-off file count can see
// what this is looking for.
func listDeps(t *testing.T) []depPackage {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; cannot inspect the dependency graph")
	}

	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("locating go.mod: %v", err)
	}
	root := strings.TrimSpace(string(gomod))
	if root == "" || root == os.DevNull {
		t.Fatal("no go.mod found; this test must run inside the module")
	}

	const format = `{{.ImportPath}}|{{with .Module}}{{.Path}}{{end}}|{{len .CgoFiles}}`
	cmd := exec.Command("go", "list", "-deps", "-f", format, "./...")
	cmd.Dir = filepath.Dir(root)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	var pkgs []depPackage
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 3 {
			t.Fatalf("unexpected go list output line %q", line)
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("parsing CgoFiles count in %q: %v", line, err)
		}
		pkgs = append(pkgs, depPackage{ImportPath: fields[0], Module: fields[1], CgoFiles: n})
	}
	return pkgs
}

// TestBuildDependenciesAreAllowlisted fails when the build reaches a module
// that is not in allowedModules. This is the mechanical form of E2: it catches
// any new third-party dependency, of which a cgo-requiring one is only the
// worst case.
func TestBuildDependenciesAreAllowlisted(t *testing.T) {
	seen := map[string]string{}
	for _, pkg := range listDeps(t) {
		if pkg.Module == "" || pkg.Module == ownModule {
			continue
		}
		if _, ok := allowedModules[pkg.Module]; !ok {
			seen[pkg.Module] = pkg.ImportPath
		}
	}
	if len(seen) == 0 {
		return
	}

	mods := make([]string, 0, len(seen))
	for mod := range seen {
		mods = append(mods, mod)
	}
	sort.Strings(mods)

	var b strings.Builder
	b.WriteString("build reaches modules outside the allowlist:\n")
	for _, mod := range mods {
		b.WriteString("\t" + mod + " (first reached via " + seen[mod] + ")\n")
	}
	b.WriteString("\nTo resolve: adding a third-party module is an owner CONSULT under E2 in\n")
	b.WriteString("docs/UPSTREAM.md — record the ruling (which module, how large, what it buys,\n")
	b.WriteString("whether hand-rolling is credible), then add it to allowedModules here with\n")
	b.WriteString("its reason. If it arrived by accident, drop the import instead.")
	t.Error(b.String())
}

// TestNoCgoInBuild fails when any non-stdlib package in the build graph carries
// cgo files. Staying cgo-free is what keeps the port cross-compiling to every
// target it supports today, including dragonfly, solaris, illumos and aix.
//
// Standard-library packages are exempt because several legitimately use cgo
// when it is enabled; the port's own module is not exempt.
func TestNoCgoInBuild(t *testing.T) {
	var offenders []string
	for _, pkg := range listDeps(t) {
		if pkg.Module != "" && pkg.CgoFiles > 0 {
			offenders = append(offenders, pkg.ImportPath+" ("+strconv.Itoa(pkg.CgoFiles)+" cgo files, module "+pkg.Module+")")
		}
	}
	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	t.Errorf("cgo is present in the build graph:\n\t%s\n\n"+
		"To resolve: cgo breaks cross-compilation and static linking for every consumer of\n"+
		"this SDK, not just this repo. Replace the dependency with a pure-Go equivalent, or\n"+
		"if it is genuinely required, take it to an owner CONSULT under E2 in docs/UPSTREAM.md\n"+
		"and confine it to a submodule so the root module stays cgo-free.",
		strings.Join(offenders, "\n\t"))
}
