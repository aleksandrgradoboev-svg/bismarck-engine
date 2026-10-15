package coding

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestDecodedTranscriptIsNeverConvertedToAIMessages is a tripwire, not a unit
// test. It asserts that nothing in this module converts a *decoded* protocol
// transcript back into `ai` conversation types.
//
// The port loses the model's tool-call argument key order on the receive side:
// protocol/cbor/decoder.go resolves a CBOR map to map[string]any, and
// coding/transcript.go does the same in parsePartialToolInput and
// cloneJSONValue. That is deliberate — upstream pi has no order-preserving
// decode to port, because a JS object keeps insertion order for free — and it is
// harmless only for as long as a decoded transcript stays a display surface and
// never reaches a model. The day someone wires a decoded remote transcript back
// into a local session, the loss becomes model-visible and silent. This test is
// what makes that day loud. The full reasoning and the failure message live in
// reportDecodedTranscriptConversion below.
//
// Scope and known blind spots, stated rather than implied:
//   - Non-test files only. server/protocol_test.go legitimately names both
//     packages in one function while asserting the ai→protocol direction.
//   - The rule is per function. A conversion split across two functions, where
//     neither one names both packages, is not detected.
//   - Direction matters: ai→protocol (server/protocol.go, the emit side) is the
//     supported direction and must not trip this.
func TestDecodedTranscriptIsNeverConvertedToAIMessages(t *testing.T) {
	root := moduleRoot(t)

	// The two type sets are derived from the packages themselves rather than
	// hardcoded, so a newly added transcript or message type is covered the day
	// it lands. An empty set would make this test vacuous, so an empty set is a
	// failure.
	protocolTypes := exportedTypeNames(t, filepath.Join(root, "protocol"),
		regexp.MustCompile(`Transcript|Snapshot|Content|Progress`))
	aiTypes := exportedTypeNames(t, filepath.Join(root, "ai"),
		regexp.MustCompile(`Message|ToolCall`))
	if len(protocolTypes) == 0 || len(aiTypes) == 0 {
		t.Fatalf("guard cannot run: derived %d protocol decoded-transcript types and %d ai conversation types; "+
			"at least one of each is required. The packages moved or were renamed — repoint the guard at them "+
			"(see %s) instead of leaving it silently vacuous.",
			len(protocolTypes), len(aiTypes), "coding/decodedtranscript_guard_test.go")
	}

	var violations []decodedTranscriptViolation
	fset := token.NewFileSet()
	for _, path := range moduleGoFiles(t, root) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		protocolName, aiName := importNames(t, file, path)
		if protocolName == "" || aiName == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			consumed := consumedProtocolTypes(fn, protocolName, protocolTypes)
			produced := producedAITypes(fn, aiName, aiTypes)
			if len(consumed) == 0 || len(produced) == 0 {
				continue
			}
			violations = append(violations, decodedTranscriptViolation{
				position: fmt.Sprintf("%s:%d", rel, fset.Position(fn.Pos()).Line),
				function: funcName(fn),
				consumes: fmt.Sprintf("%s.%s", protocolName, strings.Join(consumed, ", "+protocolName+".")),
				produces: fmt.Sprintf("%s.%s", aiName, strings.Join(produced, ", "+aiName+".")),
			})
		}
	}

	if len(violations) > 0 {
		t.Fatal(reportDecodedTranscriptConversion(violations))
	}
}

type decodedTranscriptViolation struct {
	position string
	function string
	consumes string
	produces string
}

// reportDecodedTranscriptConversion is the whole point of this file: whoever
// trips the guard has to learn from this message what they broke, why it
// matters, and what their options are.
func reportDecodedTranscriptConversion(violations []decodedTranscriptViolation) string {
	var b strings.Builder
	b.WriteString("a decoded protocol transcript is being converted into ai conversation types:\n\n")
	for _, v := range violations {
		fmt.Fprintf(&b, "    %s  %s\n        consumes %s, produces %s\n", v.position, v.function, v.consumes, v.produces)
	}
	b.WriteString(`
WHY THIS IS GUARDED
    The receive side of this port loses the model's tool-call argument key
    order. A decoded CBOR map becomes a Go map[string]any
    (protocol/cbor/decoder.go, readItem case 5), and so does the streaming
    string form in coding/transcript.go parsePartialToolInput and every clone in
    coding/transcript.go cloneJSONValue. Go maps have no order, so once a
    transcript has been decoded the model's original key order is gone.

    Replay such a transcript to a provider and the model is conditioned on
    literally different text than pi would send — {"depth":1,"path":"/tmp"}
    where pi sends {"path":"/tmp","depth":1} — and the transcript prefix shifts,
    which also costs prompt-cache hits. It is silent: no assertion in this suite
    would notice, which is why the guard is structural.

    This is NOT an oversight and must not be "fixed" casually. Upstream pi has
    no mechanism for preserving decode order at all: its CBOR decoder builds a
    plain JS object key by key and JS objects keep insertion order for free. So
    there is nothing to port, and an order-preserving decode in Go would be
    invented machinery with no upstream counterpart. It is safe today only
    because a decoded transcript never reaches a model: it stays in protocol
    types end to end (coding/remotesession.go -> TranscriptState.Snapshot() /
    .Transcript()) as a display surface for a Go client watching a remote
    session.

HOW TO FIX — pick one, in this order of preference
    1. Do not route decoded transcripts to a model. Keep decoded items in
       protocol types, and build ai messages from the local session state, not
       from frames that arrived over the wire. This is what the code does today.
    2. If a decoded transcript genuinely must be replayed, preserve key order
       FIRST, at all three sites named above, carrying an ordered object the way
       ai.OrderedObject and protocol/cbor.OrderedObject already do on the send
       side. Only once decode is order-preserving may this guard be removed, and
       it must be removed in the same change that removes the need for it — with
       the API decision escalated and recorded, per the repo's "public Go API ->
       escalate, don't ship" rule.
    3. If this is a false positive — the function names both packages but
       performs no protocol->ai conversion — move the ai construction out of the
       function, or narrow the detection in
       coding/decodedtranscript_guard_test.go with a comment saying why. Do not
       widen it into a no-op.

DECISION RECORD
    docs/UPSTREAM.md -> "Rulings", the CBOR decoder order-preservation ruling
    ("Do NOT make the CBOR decoder order-preserving"). The cycle narrative that
    originally carried this record was cut on 2026-09-03; see git history for
    "Drift at last sync check (2026-08-04)".
    Commits b2684ea (provider request bodies) and d2d267e (protocol wire) fixed
    the SEND direction; the receive direction was deliberately left unfixed, and
    this test is what makes leaving it safe.`)
	return b.String()
}

// consumedProtocolTypes reports the decoded-transcript types a function takes
// in: named in its receiver or parameters, or read anywhere in its body.
func consumedProtocolTypes(fn *ast.FuncDecl, pkg string, types map[string]bool) []string {
	found := map[string]bool{}
	collectSelectors(fn.Recv, pkg, types, found)
	if fn.Type != nil {
		collectSelectors(fn.Type.Params, pkg, types, found)
	}
	collectSelectors(fn.Body, pkg, types, found)
	return sortedKeys(found)
}

// producedAITypes reports the ai conversation types a function hands back: named
// in its results (including a nested closure's results) or constructed in its
// body. Merely *reading* an ai value is not producing one — that is what keeps
// the supported ai->protocol emit direction in server/protocol.go green.
func producedAITypes(fn *ast.FuncDecl, pkg string, types map[string]bool) []string {
	found := map[string]bool{}
	if fn.Type != nil {
		collectSelectors(fn.Type.Results, pkg, types, found)
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CompositeLit:
			collectSelectors(typed.Type, pkg, types, found)
		case *ast.ValueSpec:
			collectSelectors(typed.Type, pkg, types, found)
		case *ast.FuncLit:
			if typed.Type != nil {
				collectSelectors(typed.Type.Results, pkg, types, found)
			}
		case *ast.CallExpr:
			// new(ai.ToolCall), make([]ai.Message, 0), ai.ToolCall(v).
			if ident, ok := typed.Fun.(*ast.Ident); ok && (ident.Name == "new" || ident.Name == "make") {
				if len(typed.Args) > 0 {
					collectSelectors(typed.Args[0], pkg, types, found)
				}
				return true
			}
			collectSelectors(typed.Fun, pkg, types, found)
		}
		return true
	})
	return sortedKeys(found)
}

// collectSelectors records every pkg.Name in node whose Name is in types. It is
// generic so that an absent node — a nil *ast.FieldList for a plain function's
// receiver, say — compares equal to its own zero value rather than arriving as a
// non-nil ast.Node holding a nil pointer.
func collectSelectors[N ast.Node](node N, pkg string, types map[string]bool, found map[string]bool) {
	var absent N
	if pkg == "" || any(node) == any(absent) {
		return
	}
	ast.Inspect(node, func(n ast.Node) bool {
		selector, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if ok && ident.Name == pkg && types[selector.Sel.Name] {
			found[selector.Sel.Name] = true
		}
		return true
	})
}

// funcName renders a function or method name for the failure report.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	var receiver strings.Builder
	_ = printer.Fprint(&receiver, token.NewFileSet(), fn.Recv.List[0].Type)
	return fmt.Sprintf("(%s).%s", receiver.String(), fn.Name.Name)
}

// importNames resolves the local names this file uses for the protocol and ai
// packages, empty when the file does not import one. A dot import is rejected
// rather than skipped: it would blind the guard.
func importNames(t *testing.T, file *ast.File, filename string) (protocolName, aiName string) {
	t.Helper()
	const (
		protocolPath = "github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
		aiPath       = "github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	)
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if value != protocolPath && value != aiPath {
			continue
		}
		name := path.Base(value)
		if spec.Name != nil {
			if spec.Name.Name == "." {
				t.Fatalf("%s dot-imports %q, which blinds the decoded-transcript guard: it matches "+
					"qualified selectors. Import it under a name so the guard can see through the file.",
					filename, value)
			}
			name = spec.Name.Name
		}
		if value == protocolPath {
			protocolName = name
		} else {
			aiName = name
		}
	}
	return protocolName, aiName
}

// exportedTypeNames returns the exported type names declared in a package
// directory whose name matches want.
func exportedTypeNames(t *testing.T, dir string, want *regexp.Regexp) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	names := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || !typeSpec.Name.IsExported() {
					continue
				}
				if want.MatchString(typeSpec.Name.Name) {
					names[typeSpec.Name.Name] = true
				}
			}
		}
	}
	return names
}

// moduleGoFiles lists the module's non-test Go files.
func moduleGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "node_modules":
				return filepath.SkipDir
			}
			if strings.HasPrefix(entry.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("guard cannot run: no non-test Go files found under %s, so it would pass vacuously", root)
	}
	return files
}

// moduleRoot walks up from the test's working directory to the go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("guard cannot run: no go.mod found above the test's working directory; "+
				"it needs the module root to scan. Started at %s.", dir)
		}
		dir = parent
	}
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
