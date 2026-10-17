package env

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestFileIDBothDirInfoLayout(t *testing.T) {
	var info fileIDBothDirInfo
	if got := unsafe.Offsetof(info.FileID); got != 96 {
		t.Fatalf("FileID at %d, want 96", got)
	}
	if got := unsafe.Offsetof(info.FileName); got != 104 {
		t.Fatalf("FileName at %d, want 104", got)
	}
}

// The listing names what os.ReadDir names, and gives a directory the file
// index a handle on it gives.
func TestListDirectoryAgreesWithReadDirAndHandles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"b", "a", "сканы", "c.md"} {
		if filepath.Ext(name) == ".md" {
			if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := listDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(entries) {
		t.Fatalf("listed %d entries, os.ReadDir %d", len(listed), len(entries))
	}
	for i, entry := range entries {
		if listed[i].name != entry.Name() {
			t.Fatalf("entry %d: %q, os.ReadDir %q", i, listed[i].name, entry.Name())
		}
		if listed[i].index == 0 {
			t.Fatalf("%s: no file index listed", entry.Name())
		}
		if entry.IsDir() {
			if want := targetIndex(filepath.Join(root, entry.Name())); listed[i].index != want {
				t.Fatalf("%s: listed index %d, handle %d", entry.Name(), listed[i].index, want)
			}
		}
	}
}

func TestListDirectoryKeepsReadDirErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "missing"), file} {
		_, want := os.ReadDir(path)
		_, got := listDirectory(path)
		if want == nil || got == nil {
			t.Fatalf("%s: errors %v and %v, want both", path, got, want)
		}
		if Code(toFileError(got, "")) != Code(toFileError(want, "")) {
			t.Fatalf("%s: code %q, os.ReadDir %q", path, Code(toFileError(got, "")), Code(toFileError(want, "")))
		}
	}
}
