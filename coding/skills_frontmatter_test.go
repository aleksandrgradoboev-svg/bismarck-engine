package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// I11: pi preserves discovery order (skills.ts loadSkills: user dir first,
// then project, insertion order — no name sort).
func TestLoadSkillsPreservesDiscoveryOrder(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	cwd := t.TempDir()

	// User skill sorts after the project skill by name — discovery order must
	// still put it first.
	writeSkill(t, filepath.Join(home, ".pi", "agent", "skills", "z-user-skill"),
		"---\nname: z-user-skill\ndescription: user skill\n---\n")
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "a-project-skill"),
		"---\nname: a-project-skill\ndescription: project skill\n---\n")

	skills, _ := LoadSkillsWithTrust(cwd, true)
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d: %+v", len(skills), skills)
	}
	if skills[0].Name != "z-user-skill" || skills[1].Name != "a-project-skill" {
		t.Fatalf("discovery order not preserved (user dir first): %s, %s", skills[0].Name, skills[1].Name)
	}
}

// I11: a folded block-scalar description (`description: >-`) loads, folded to
// a single line like the YAML parser pi uses.
func TestSkillBlockScalarDescription(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "folded"),
		"---\nname: folded\ndescription: >-\n  Line one of the description\n  continues on line two.\n---\nbody\n")

	skills, _ := LoadSkillsWithTrust(cwd, true)
	if len(skills) != 1 {
		t.Fatalf("folded-description skill did not load: %+v", skills)
	}
	want := "Line one of the description continues on line two."
	if skills[0].Description != want {
		t.Fatalf("folded description wrong:\n got: %q\nwant: %q", skills[0].Description, want)
	}
}

// Literal block scalars keep newlines; clip chomping keeps one trailing \n,
// strip (-) removes it.
func TestParseFrontmatterBlockScalars(t *testing.T) {
	fm, _ := parseFrontmatter("---\nlit: |\n  a\n  b\nstrip: |-\n  a\n  b\nfold: >\n  a\n  b\n---\nbody")
	if got := fm["lit"].value; got != "a\nb\n" {
		t.Fatalf("literal clip wrong: %q", got)
	}
	if got := fm["strip"].value; got != "a\nb" {
		t.Fatalf("literal strip wrong: %q", got)
	}
	if got := fm["fold"].value; got != "a b\n" {
		t.Fatalf("folded clip wrong: %q", got)
	}
}

// Multi-line plain scalars fold across continuation lines.
func TestParseFrontmatterMultilinePlain(t *testing.T) {
	fm, _ := parseFrontmatter("---\ndescription: starts here\n  and continues here\n---\n")
	if got := fm["description"].value; got != "starts here and continues here" {
		t.Fatalf("multi-line plain scalar wrong: %q", got)
	}
}

// I11: disable-model-invocation requires the strict YAML boolean true.
// A quoted "true" is a string and must NOT disable (pi: `=== true` after a
// real YAML parse).
func TestSkillDisableModelInvocationStrictBool(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "plain-true"),
		"---\nname: plain-true\ndescription: d\ndisable-model-invocation: true\n---\n")
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "quoted-true"),
		"---\nname: quoted-true\ndescription: d\ndisable-model-invocation: \"true\"\n---\n")
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "yaml-caps-true"),
		"---\nname: yaml-caps-true\ndescription: d\ndisable-model-invocation: True\n---\n")

	byName := map[string]Skill{}
	loaded, _ := LoadSkillsWithTrust(cwd, true)
	for _, s := range loaded {
		byName[s.Name] = s
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(byName))
	}
	if !byName["plain-true"].DisableModelInvocation {
		t.Fatal("plain true must disable model invocation")
	}
	if byName["quoted-true"].DisableModelInvocation {
		t.Fatal(`quoted "true" is a string, must NOT disable model invocation`)
	}
	if !byName["yaml-caps-true"].DisableModelInvocation {
		t.Fatal("YAML core-schema True must disable model invocation")
	}
}

// I13(c): name/description length validation counts UTF-16 code units
// (JS String.length), not bytes or runes.
func TestSkillValidationLengthsUTF16(t *testing.T) {
	// 513 astral characters = 513 runes but 1026 UTF-16 units > 1024.
	desc := strings.Repeat("\U0001F600", 513)
	errs := validateDescription(desc)
	if len(errs) != 1 || !strings.Contains(errs[0], "description exceeds 1024 characters (1026)") {
		t.Fatalf("description length must count UTF-16 units: %v", errs)
	}
	// 512 astral characters = 1024 units — exactly at the limit, valid.
	if errs := validateDescription(strings.Repeat("\U0001F600", 512)); len(errs) != 0 {
		t.Fatalf("1024 UTF-16 units should be valid: %v", errs)
	}
}

// pi 1355cd36e: a leading UTF-8 BOM comes off BEFORE the "---" test, so a
// BOM-prefixed document still yields its frontmatter, and the returned body no
// longer carries the BOM either. Without the strip, `\ufeff---` fails
// HasPrefix("---") and the whole header is silently returned as body.
func TestParseFrontmatterStripsLeadingBOM(t *testing.T) {
	fm, body := parseFrontmatter("\ufeff---\nname: demo\ndescription: Test\n---\nBody")
	if got := fm["name"].value; got != "demo" {
		t.Fatalf("name = %q, want %q (BOM must not hide the frontmatter)", got, "demo")
	}
	if got := fm["description"].value; got != "Test" {
		t.Fatalf("description = %q, want %q", got, "Test")
	}
	if body != "Body" {
		t.Fatalf("body = %q, want %q", body, "Body")
	}

	// A BOM'd CRLF document \u2014 the shape a Windows editor actually writes \u2014
	// parses. This pins the combination, NOT the order of the two transforms:
	// the strip and the newline normalization commute for every input, so an
	// implementation that nests them the other way is byte-identical here.
	crlfFM, crlfBody := parseFrontmatter("\ufeff---\r\nname: demo\r\ndescription: Test\r\n---\r\nBody")
	if crlfFM["name"].value != "demo" || crlfFM["description"].value != "Test" || crlfBody != "Body" {
		t.Fatalf("BOM+CRLF parsed differently: fm=%v body=%q", crlfFM, crlfBody)
	}

	// A document with no frontmatter still loses its BOM from the body.
	if _, plain := parseFrontmatter("\ufeffjust text"); plain != "just text" {
		t.Fatalf("body = %q, want %q", plain, "just text")
	}
}

// End-to-end: a BOM-prefixed SKILL.md loads with its name and description, so
// the skill reaches the system prompt instead of being dropped for a missing
// description.
func TestSkillWithLeadingBOMLoads(t *testing.T) {
	isolatedHome(t) // HOME *and* USERPROFILE — os.UserHomeDir reads the latter on Windows
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "bom-skill"),
		"\ufeff---\nname: bom-skill\ndescription: Loads despite the byte order mark\n---\nbody\n")

	skills, _ := LoadSkillsWithTrust(cwd, true)
	if len(skills) != 1 {
		t.Fatalf("BOM-prefixed skill did not load: %+v", skills)
	}
	if skills[0].Name != "bom-skill" {
		t.Fatalf("name = %q, want %q", skills[0].Name, "bom-skill")
	}
	if want := "Loads despite the byte order mark"; skills[0].Description != want {
		t.Fatalf("description = %q, want %q", skills[0].Description, want)
	}
	prompt := FormatSkillsForPrompt(skills)
	if !strings.Contains(prompt, "<name>bom-skill</name>") {
		t.Fatalf("skill missing from prompt: %q", prompt)
	}
	if strings.Contains(prompt, "\ufeff") {
		t.Fatalf("skills prompt block carries a BOM: %q", prompt)
	}
}
