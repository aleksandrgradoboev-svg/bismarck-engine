package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillFile(t *testing.T, path, name, desc string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\n"
	if name != "" {
		body += "name: " + name + "\n"
	}
	if desc != "" {
		body += "description: " + desc + "\n"
	}
	body += "---\n# body\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// findSkill returns the loaded skill with the given name (or nil).
func findSkill(skills []Skill, name string) *Skill {
	for i := range skills {
		if skills[i].Name == name {
			return &skills[i]
		}
	}
	return nil
}

// TestLoadSkillsRootMarkdownChild verifies a direct .md child of the skills root
// is loaded as a skill (pi loadSkillsFromDir: root .md files are skills), in
// addition to SKILL.md-rooted subdirectory skills.
func TestLoadSkillsRootMarkdownChild(t *testing.T) {
	root := t.TempDir()
	// Direct .md child of the root.
	writeSkillFile(t, filepath.Join(root, "quick-tip.md"), "quick-tip", "A root-level markdown skill")
	// A subdirectory skill via SKILL.md.
	writeSkillFile(t, filepath.Join(root, "deep", "SKILL.md"), "deep-skill", "A nested skill")

	skills, _ := loadSkillsFromDir(root)
	if findSkill(skills, "quick-tip") == nil {
		t.Fatalf("root .md skill not loaded: %+v", skills)
	}
	if findSkill(skills, "deep-skill") == nil {
		t.Fatalf("nested SKILL.md skill not loaded: %+v", skills)
	}
}

// TestLoadSkillsSkillRootStopsRecursion verifies a dir with SKILL.md is a skill
// root and its non-SKILL .md children / subdirs are NOT additionally loaded.
func TestLoadSkillsSkillRootStopsRecursion(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "myskill")
	writeSkillFile(t, filepath.Join(skillDir, "SKILL.md"), "myskill", "The skill")
	// Extra .md and nested SKILL.md inside the skill root must be ignored.
	writeSkillFile(t, filepath.Join(skillDir, "extra.md"), "extra", "Should not load")
	writeSkillFile(t, filepath.Join(skillDir, "sub", "SKILL.md"), "subskill", "Should not load")

	skills, _ := loadSkillsFromDir(skillDir)
	if len(skills) != 1 || skills[0].Name != "myskill" {
		t.Fatalf("skill-root recursion not stopped: %+v", skills)
	}
}

// TestLoadSkillsHonorsIgnoreFiles verifies .gitignore/.ignore/.fdignore exclude
// matching skill files/dirs.
func TestLoadSkillsHonorsIgnoreFiles(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "keep.md"), "keep", "Kept skill")
	writeSkillFile(t, filepath.Join(root, "drop.md"), "drop", "Ignored skill")
	writeSkillFile(t, filepath.Join(root, "secret", "SKILL.md"), "secret-skill", "Ignored dir skill")

	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("drop.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".fdignore"), []byte("secret/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, _ := loadSkillsFromDir(root)
	if findSkill(skills, "keep") == nil {
		t.Fatalf("kept skill missing: %+v", skills)
	}
	if findSkill(skills, "drop") != nil {
		t.Fatalf(".gitignore'd skill should be excluded: %+v", skills)
	}
	if findSkill(skills, "secret-skill") != nil {
		t.Fatalf(".fdignore'd dir skill should be excluded: %+v", skills)
	}
}

// TestLoadSkillsSkipsNodeModules verifies node_modules is never scanned.
func TestLoadSkillsSkipsNodeModules(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "node_modules", "pkg", "SKILL.md"), "dep-skill", "Should be skipped")
	writeSkillFile(t, filepath.Join(root, "real", "SKILL.md"), "real-skill", "Kept")

	skills, _ := loadSkillsFromDir(root)
	if findSkill(skills, "dep-skill") != nil {
		t.Fatalf("node_modules skill should be skipped: %+v", skills)
	}
	if findSkill(skills, "real-skill") == nil {
		t.Fatalf("real skill missing: %+v", skills)
	}
}

// TestSkillNameValidationDiagnostics verifies invalid names emit warning
// diagnostics but still load (description present), and a long description emits
// a warning. A missing description drops the skill entirely.
func TestSkillNameValidationDiagnostics(t *testing.T) {
	root := t.TempDir()
	// Invalid name (uppercase + leading hyphen + consecutive hyphens) but valid desc.
	writeSkillFile(t, filepath.Join(root, "Bad", "SKILL.md"), "-Bad--Name", "ok desc")
	// Over-long description.
	longDesc := strings.Repeat("x", maxSkillDescriptionLength+5)
	writeSkillFile(t, filepath.Join(root, "long", "SKILL.md"), "long-skill", longDesc)
	// Missing description: dropped, no skill.
	writeSkillFile(t, filepath.Join(root, "nodesc", "SKILL.md"), "nodesc-skill", "")

	skills, diags := loadSkillsFromDir(root)

	// Invalid-name skill still loads.
	if findSkill(skills, "-Bad--Name") == nil {
		t.Fatalf("skill with invalid name should still load (desc present): %+v", skills)
	}
	// Missing-description skill is dropped.
	if findSkill(skills, "nodesc-skill") != nil {
		t.Fatalf("skill without description must not load")
	}

	msgs := strings.Join(diagMessages(diags), "|")
	for _, want := range []string{
		"name contains invalid characters",
		"name must not start or end with a hyphen",
		"name must not contain consecutive hyphens",
	} {
		if !strings.Contains(msgs, want) {
			t.Fatalf("missing name diagnostic %q in: %s", want, msgs)
		}
	}
	if !strings.Contains(msgs, "description exceeds 1024 characters") {
		t.Fatalf("missing long-description diagnostic in: %s", msgs)
	}
	if !strings.Contains(msgs, "description is required") {
		t.Fatalf("missing required-description diagnostic in: %s", msgs)
	}
}

func diagMessages(diags []SkillDiagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Message)
	}
	return out
}

// pi 8c2529dae: markdown files not declared as skills (basename other than
// SKILL.md) with no usable description are silently skipped — no skill, no
// diagnostics. A stray README.md in a skills root must not warn. SKILL.md
// files keep the full diagnostics, including "description is required".
func TestLoadSkillsUndeclaredRootMdSilentSkip(t *testing.T) {
	root := t.TempDir()
	// A README with no frontmatter at all.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Just a readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A root .md with frontmatter but no description.
	writeSkillFile(t, filepath.Join(root, "notes.md"), "notes", "")
	// A declared skill without description still warns and is dropped.
	writeSkillFile(t, filepath.Join(root, "nodesc", "SKILL.md"), "nodesc-skill", "")

	skills, diags := loadSkillsFromDir(root)
	if len(skills) != 0 {
		t.Fatalf("no skill should load: %+v", skills)
	}
	for _, d := range diags {
		if filepath.Base(d.Path) != "SKILL.md" {
			t.Fatalf("undeclared .md must not produce diagnostics, got: %+v", d)
		}
	}
	if !strings.Contains(strings.Join(diagMessages(diags), "|"), "description is required") {
		t.Fatalf("SKILL.md must keep the required-description diagnostic: %+v", diags)
	}
}

// pi 8c2529dae type guards: frontmatter values that the YAML core schema types
// as non-strings (plain null/bool/int/float scalars) are not descriptions or
// names. Quoted forms stay strings; a non-string name falls back to the
// directory name.
func TestSkillFrontmatterNonStringScalars(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("boolean-desc/SKILL.md", "---\ndescription: true\n---\n# b\n")
	write("number-desc.md", "---\ndescription: 42\n---\n# n\n")
	write("quoted-desc/SKILL.md", "---\ndescription: \"true\"\n---\n# q\n")
	write("num-name/SKILL.md", "---\nname: 123\ndescription: a skill\n---\n# s\n")

	skills, diags := loadSkillsFromDir(root)

	// Plain `description: true` is not a string: declared skill drops + warns.
	if findSkill(skills, "boolean-desc") != nil {
		t.Fatalf("plain-boolean description must not load: %+v", skills)
	}
	// Undeclared .md with a plain-number description: silent skip.
	for _, d := range diags {
		if filepath.Base(d.Path) != "SKILL.md" {
			t.Fatalf("number-desc.md must skip silently, got: %+v", d)
		}
	}
	// Quoted "true" IS a string description.
	q := findSkill(skills, "quoted-desc")
	if q == nil || q.Description != "true" {
		t.Fatalf("quoted description must load as the string \"true\": %+v", skills)
	}
	// Plain-number name falls back to the parent directory name.
	if findSkill(skills, "num-name") == nil || findSkill(skills, "123") != nil {
		t.Fatalf("non-string name must fall back to the directory name: %+v", skills)
	}
}

// TestValidateNameAccepts verifies a well-formed name produces no errors.
func TestValidateNameAccepts(t *testing.T) {
	if errs := validateName("good-skill-1"); len(errs) != 0 {
		t.Fatalf("valid name rejected: %v", errs)
	}
	if errs := validateName(strings.Repeat("a", maxSkillNameLength+1)); len(errs) == 0 {
		t.Fatalf("over-long name should error")
	}
}

// TestLoadSkillsAgentsModeIsTheMirrorOfPiMode pins pi's SkillDiscoveryMode
// (upstream 5e11f6586): the AGENTS-convention directories load NESTED markdown
// files and ignore root-level ones, exactly inverting pi-mode discovery.
// SKILL.md keeps stopping recursion in both.
func TestLoadSkillsAgentsModeIsTheMirrorOfPiMode(t *testing.T) {
	build := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		writeSkillFile(t, filepath.Join(root, "root-note.md"), "root-note", "A root-level markdown file")
		writeSkillFile(t, filepath.Join(root, "nested", "helper.md"), "nested-helper", "A nested markdown file")
		writeSkillFile(t, filepath.Join(root, "declared", "SKILL.md"), "declared-skill", "A declared skill")
		writeSkillFile(t, filepath.Join(root, "declared", "extra.md"), "declared-extra", "Must not load")
		return root
	}

	piSkills, _ := loadSkillsFromDirMode(build(t), skillModePi)
	if findSkill(piSkills, "root-note") == nil {
		t.Fatalf("pi mode must load root-level .md: %+v", piSkills)
	}
	if findSkill(piSkills, "nested-helper") != nil {
		t.Fatalf("pi mode must not load nested .md: %+v", piSkills)
	}

	agentsSkills, _ := loadSkillsFromDirMode(build(t), skillModeAgents)
	if findSkill(agentsSkills, "root-note") != nil {
		t.Fatalf("agents mode must not load root-level .md: %+v", agentsSkills)
	}
	if findSkill(agentsSkills, "nested-helper") == nil {
		t.Fatalf("agents mode must load nested .md: %+v", agentsSkills)
	}

	// Shared in both modes: SKILL.md declares a root and stops recursion there.
	for name, skills := range map[string][]Skill{"pi": piSkills, "agents": agentsSkills} {
		if findSkill(skills, "declared-skill") == nil {
			t.Fatalf("%s mode must load a declared SKILL.md: %+v", name, skills)
		}
		if findSkill(skills, "declared-extra") != nil {
			t.Fatalf("%s mode must not recurse past a SKILL.md root: %+v", name, skills)
		}
	}
}

// TestLoadSkillsDiscoversUserAgentsDir verifies ~/.agents/skills participates in
// discovery, in agents mode, and that it cannot displace a name an existing pi
// directory already resolved (names dedup first-wins, and it is scanned last).
func TestLoadSkillsDiscoversUserAgentsDir(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("USERPROFILE", home) // windows home lookup

	agentsRoot := filepath.Join(home, ".agents", "skills")
	writeSkillFile(t, filepath.Join(agentsRoot, "nested", "from-agents.md"), "from-agents", "Nested agents-dir skill")
	writeSkillFile(t, filepath.Join(agentsRoot, "top-level.md"), "agents-root-note", "Root-level, must not load")
	writeSkillFile(t, filepath.Join(agentsRoot, "shared", "SKILL.md"), "shared-name", "The agents-dir one")

	// The pi user directory defines the same name and is scanned first.
	writeSkillFile(t, filepath.Join(AgentDir(), "skills", "shared", "SKILL.md"), "shared-name", "The pi one")

	cwd := t.TempDir()
	skills, _ := LoadSkillsWithDiagnostics(cwd)

	if findSkill(skills, "from-agents") == nil {
		t.Fatalf("~/.agents/skills nested skill not discovered: %+v", skills)
	}
	if findSkill(skills, "agents-root-note") != nil {
		t.Fatalf("~/.agents/skills root-level .md must not load: %+v", skills)
	}
	shared := findSkill(skills, "shared-name")
	if shared == nil || shared.Description != "The pi one" {
		t.Fatalf("the pi directory must keep the name it resolved first: %+v", shared)
	}
}
