package env_test

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// A directory recreated at the same path within one tick of the file
// system clock gets the old directory's creation and modification times;
// on a fast machine that happens without help. Here the times are set back
// to make it certain.

func directoryTimes(t *testing.T, path string) (created, modified syscall.Filetime) {
	t.Helper()
	d := must(os.Lstat(path)).Sys().(*syscall.Win32FileAttributeData)
	return d.CreationTime, d.LastWriteTime
}

func setDirectoryTimes(t *testing.T, path string, created, modified syscall.Filetime) {
	t.Helper()
	name := must(syscall.UTF16PtrFromString(path))
	h, err := syscall.CreateFile(name, syscall.FILE_WRITE_ATTRIBUTES,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	noErr(t, err)
	defer syscall.CloseHandle(h)
	noErr(t, syscall.SetFileTime(h, &created, nil, &modified))
}

// replaceWithSameTimes renames the directory at path away and creates
// another there with the old one's times.
func replaceWithSameTimes(t *testing.T, path, away string) {
	t.Helper()
	created, modified := directoryTimes(t, path)
	noErr(t, os.Rename(path, away))
	noErr(t, os.Mkdir(path, 0o755))
	setDirectoryTimes(t, path, created, modified)
	gotCreated, gotModified := directoryTimes(t, path)
	if gotCreated != created || gotModified != modified {
		t.Fatal("the new directory's times were not set back")
	}
}

// watchReports watches targets and waits for path to be reported after
// change runs. The first poll comes long after change, so the watcher
// only ever sees the replacement finished.
func watchReports(t *testing.T, root string, targets []env.WatchTarget, path string, change func()) {
	t.Helper()
	e := env.NewLocalWith(root, env.LocalOptions{Watch: env.WatchOptions{PollInterval: 500 * time.Millisecond}})
	var mu sync.Mutex
	var reported [][]string
	watcher := must(e.Watch(ctx, targets, func(c env.WatchChange) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, c.Paths)
	}))
	defer watcher.Close(ctx)
	change()
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := slices.Clone(reported)
		mu.Unlock()
		for _, paths := range got {
			if slices.Contains(paths, path) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no change reported for %s; got %v", path, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatchTellsADirectoryReplacedWithTheSameTimes(t *testing.T) {
	root := tempDir(t)
	a := filepath.Join(root, "skills", "a")
	noErr(t, os.MkdirAll(a, 0o755))
	noErr(t, os.WriteFile(filepath.Join(a, "x.md"), []byte("x"), 0o644))
	watchReports(t, root, []env.WatchTarget{{Path: "skills", Recursive: true}}, a, func() {
		replaceWithSameTimes(t, a, filepath.Join(root, "skills-old"))
	})
}

func TestWatchTellsATargetDirectoryReplacedWithTheSameTimes(t *testing.T) {
	root := tempDir(t)
	skills := filepath.Join(root, "skills")
	noErr(t, os.Mkdir(skills, 0o755))
	watchReports(t, root, []env.WatchTarget{{Path: "skills"}}, skills, func() {
		replaceWithSameTimes(t, skills, filepath.Join(root, "skills-old"))
	})
}
