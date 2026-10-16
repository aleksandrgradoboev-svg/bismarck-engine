package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadSkillPathsLoadsDirectoriesAndFilesOnly verifies pi's loadSkills with
// includeDefaults false: a directory is scanned, a .md file is one skill,
// relative paths resolve against cwd, and nothing else is discovered — not
// the user's skill directories under the home directory.
func TestLoadSkillPathsLoadsDirectoriesAndFilesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeSkillFile(t, filepath.Join(home, ".pi", "agent", "skills", "mine", "SKILL.md"), "mine", "home skill")
	cwd := t.TempDir()
	writeSkillFile(t, filepath.Join(cwd, "shared", "nav", "SKILL.md"), "nav", "navigation")
	writeSkillFile(t, filepath.Join(cwd, "shared", "forms", "SKILL.md"), "forms", "forms")
	single := filepath.Join(t.TempDir(), "one", "SKILL.md")
	writeSkillFile(t, single, "one", "a single file")

	skills, diags := LoadSkillPaths(cwd, []string{"shared", " " + single + " "})
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "forms,nav,one" || len(diags) != 0 {
		t.Fatalf("skills = %v, diags = %v", names, diags)
	}
	if s := findSkill(skills, "nav"); s.BaseDir != filepath.Join(cwd, "shared", "nav") {
		t.Fatalf("nav base dir = %q", s.BaseDir)
	}
}

// TestLoadSkillPathsReportsWhatItDrops verifies that no path is dropped
// silently: a missing path and a non-markdown file are warnings, a second
// skill of a name is a collision after them, the first one winning.
func TestLoadSkillPathsReportsWhatItDrops(t *testing.T) {
	cwd := t.TempDir()
	writeSkillFile(t, filepath.Join(cwd, "a", "nav", "SKILL.md"), "nav", "first")
	writeSkillFile(t, filepath.Join(cwd, "b", "nav", "SKILL.md"), "nav", "second")
	if err := os.WriteFile(filepath.Join(cwd, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, diags := LoadSkillPaths(cwd, []string{"a", "b", "missing", "notes.txt", "a"})
	if len(skills) != 1 || skills[0].Description != "first" {
		t.Fatalf("skills = %+v", skills)
	}
	var got []string
	for _, d := range diags {
		got = append(got, d.Type+": "+d.Message+" @ "+filepath.Base(d.Path))
	}
	want := `warning: skill path does not exist @ missing,warning: skill path is not a markdown file @ notes.txt,collision: name "nav" collision @ SKILL.md`
	if strings.Join(got, ",") != want {
		t.Fatalf("diags = %v", got)
	}
	if diags[2].Path != filepath.Join(cwd, "b", "nav", "SKILL.md") {
		t.Fatalf("collision path = %q", diags[2].Path)
	}
}
