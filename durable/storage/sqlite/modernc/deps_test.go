package modernc_test

import (
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The dependency rules of this module, as internal/policy holds the root
// module's: modernc.org/sqlite is the one third-party driver the owner
// admitted (ruling 2026-10-16, stage 2b of docs/sync-v1.2.0.md), with the
// modules it builds with. Licenses were checked against the module sources:
// BSD-3-Clause and MIT, SQLite itself public domain; the MPL-2.0 module in
// its module graph is not built.
var allowedModules = map[string]string{
	"modernc.org/sqlite":                                "the SQLite driver, transpiled from C; BSD-3-Clause, SQLite public domain",
	"modernc.org/libc":                                  "the C runtime the driver runs on; BSD-3-Clause (with musl, MIT)",
	"modernc.org/mathutil":                              "reached through modernc.org/libc; BSD-3-Clause",
	"modernc.org/memory":                                "reached through modernc.org/libc; BSD-3-Clause",
	"github.com/dustin/go-humanize":                     "reached through modernc.org/libc; MIT",
	"github.com/google/uuid":                            "reached through modernc.org/libc; BSD-3-Clause",
	"github.com/remyoudompheng/bigfft":                  "reached through modernc.org/mathutil; BSD-3-Clause",
	"github.com/mattn/go-isatty":                        "reached through modernc.org/libc on Windows and macOS; MIT",
	"github.com/ncruces/go-strftime":                    "reached through modernc.org/libc on Windows and macOS; MIT",
	"golang.org/x/sys":                                  "reached through modernc.org/libc and the engine; BSD-3-Clause",
	"golang.org/x/text":                                 "the engine's own dependency",
	"golang.org/x/image":                                "the engine's own dependency",
	"github.com/tetratelabs/wazero":                     "the engine's own dependency",
	"github.com/aleksandrgradoboev-svg/bismarck-engine": "the engine",
}

const ownModule = "github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/sqlite/modernc"

type depPackage struct {
	importPath string
	module     string
	cgoFiles   int
}

// listDeps enumerates the packages the module's build reaches on goos, with
// cgo enabled, so a cgo dependency cannot hide behind a build constraint.
func listDeps(t *testing.T, goos string) []depPackage {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; cannot inspect the dependency graph")
	}
	cmd := exec.Command("go", "list", "-deps", "-f", `{{.ImportPath}}|{{with .Module}}{{.Path}}{{end}}|{{len .CgoFiles}}`, "./...")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOOS="+goos)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps (%s): %v", goos, err)
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
		pkgs = append(pkgs, depPackage{importPath: fields[0], module: fields[1], cgoFiles: n})
	}
	return pkgs
}

var targets = []string{"linux", "windows", "darwin"}

func TestBuildDependenciesAreAllowlisted(t *testing.T) {
	for _, goos := range targets {
		seen := map[string]string{}
		for _, pkg := range listDeps(t, goos) {
			if pkg.module == "" || pkg.module == ownModule {
				continue
			}
			if _, ok := allowedModules[pkg.module]; !ok {
				seen[pkg.module] = pkg.importPath
			}
		}
		var mods []string
		for mod := range seen {
			mods = append(mods, mod+" (first reached via "+seen[mod]+")")
		}
		sort.Strings(mods)
		if len(mods) > 0 {
			t.Errorf("%s build reaches modules outside the allowlist:\n\t%s\n\nAdding a module is an owner ruling: check its license against its source first.",
				goos, strings.Join(mods, "\n\t"))
		}
	}
}

func TestNoCgoInBuild(t *testing.T) {
	for _, goos := range targets {
		var offenders []string
		for _, pkg := range listDeps(t, goos) {
			if pkg.module != "" && pkg.cgoFiles > 0 {
				offenders = append(offenders, pkg.importPath+" ("+strconv.Itoa(pkg.cgoFiles)+" cgo files)")
			}
		}
		sort.Strings(offenders)
		if len(offenders) > 0 {
			t.Errorf("cgo is present in the %s build graph:\n\t%s", goos, strings.Join(offenders, "\n\t"))
		}
	}
}
