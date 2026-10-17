package env

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// WatchMode is how a FileWatcher learns of changes.
type WatchMode string

const (
	// WatchNative: changes are reported within about two seconds.
	WatchNative WatchMode = "native"
	// WatchPolling: the environment compares snapshots; a change undone
	// between two snapshots can be missed.
	WatchPolling WatchMode = "polling"
)

// WatchTarget is a file or directory to watch. It may be missing; creating
// it is a change.
type WatchTarget struct {
	Path string
	// Recursive watches everything below a directory, not only its
	// entries. Symbolic links below it are not followed.
	Recursive bool
	// Exclude names entries below Path that are neither watched nor
	// reported.
	Exclude WatchExclude
}

// WatchExclude excludes entries below a watched directory.
type WatchExclude struct {
	// Hidden excludes names starting with ".".
	Hidden bool
	Names  []string
}

// WatchChange is what changed: Paths (something at or below each may have
// changed; calls may be spurious, a change is never missed while the
// watcher is healthy), Overflow (coverage was uncertain for a while: rescan
// everything watched), or Err (the watcher stopped; no calls follow).
type WatchChange struct {
	Paths    []string
	Overflow bool
	Err      *FileError
}

// FileWatcher reports changes until it is closed.
type FileWatcher interface {
	Mode() WatchMode
	// Close stops watching; no onChange call starts after it returns.
	// Idempotent.
	Close(ctx context.Context) error
}

// WatchOptions configure how Local watches. Local only polls: Go's
// standard library has no native file watching, and on Windows native
// watchers keep directories open, which blocks renaming their parents.
type WatchOptions struct {
	// PollInterval is the time between snapshots; 0 is 2 s.
	PollInterval time.Duration
	// MaxDirectories is the most directories one watcher covers; 0 is
	// 10,000.
	MaxDirectories int
}

const (
	defaultPollInterval   = 2 * time.Second
	defaultMaxDirectories = 10_000
	// Recently modified small files are also compared by content: a second
	// write within the file system's timestamp granularity can keep size and
	// modification time.
	hashMaxBytes = 256 * 1024
	hashRecent   = 5 * time.Second
)

type entryKind string

const (
	entryFile      entryKind = "file"
	entryDirectory entryKind = "directory"
	entrySymlink   entryKind = "symlink"
	entryOther     entryKind = "other"
)

// watchEntry is what a snapshot remembers of one path. Directories and
// ancestors are compared by identity only.
type watchEntry struct {
	kind  entryKind
	id    fileID
	size  int64
	mtime int64
	hash  [sha256.Size]byte
	// hashed is set when hash is.
	hashed bool
}

func kindOf(info fs.FileInfo) entryKind {
	switch {
	case info.Mode().IsRegular():
		return entryFile
	case info.IsDir():
		return entryDirectory
	case info.Mode()&fs.ModeSymlink != 0:
		return entrySymlink
	}
	return entryOther
}

func entryOf(info fs.FileInfo) watchEntry {
	e := watchEntry{kind: kindOf(info), id: identity(info)}
	if e.kind != entryDirectory {
		e.size, e.mtime = info.Size(), info.ModTime().UnixNano()
	}
	return e
}

type resolvedTarget struct {
	path      string
	recursive bool
	hidden    bool
	names     map[string]bool
}

func (t resolvedTarget) excluded(name string) bool {
	return (t.hidden && strings.HasPrefix(name, ".")) || t.names[name]
}

// ancestorsOf are path's ancestors, nearest first, up to the root.
func ancestorsOf(path string) []string {
	var out []string
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		out = append(out, current)
		if filepath.Dir(current) == current {
			return out
		}
	}
}

// isWithin reports whether path is ancestor or below it.
func isWithin(path, ancestor string) bool {
	if path == ancestor {
		return true
	}
	if !strings.HasSuffix(ancestor, string(filepath.Separator)) {
		ancestor += string(filepath.Separator)
	}
	return strings.HasPrefix(path, ancestor)
}

var errBudget = errors.New("watch budget exceeded")

// pollWatcher watches by snapshots (pi NodeFileWatcher in polling mode):
// changes are the difference between snapshots, so a replaced file, a
// renamed or recreated ancestor, or a directory created with its contents
// never depends on what an operating system reports.
type pollWatcher struct {
	targets        []resolvedTarget
	onChange       func(WatchChange)
	interval       time.Duration
	maxDirectories int
	snapshot       map[string]watchEntry

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// Watch implements FileSystem.
func (l *Local) Watch(ctx context.Context, targets []WatchTarget, onChange func(WatchChange)) (FileWatcher, error) {
	if err := aborted(ctx, ""); err != nil {
		return nil, err
	}
	w := &pollWatcher{
		onChange:       onChange,
		interval:       l.watch.PollInterval,
		maxDirectories: l.watch.MaxDirectories,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	if w.interval <= 0 {
		w.interval = defaultPollInterval
	}
	if w.maxDirectories <= 0 {
		w.maxDirectories = defaultMaxDirectories
	}
	for _, t := range targets {
		names := map[string]bool{}
		for _, n := range t.Exclude.Names {
			names[n] = true
		}
		w.targets = append(w.targets, resolvedTarget{path: l.resolve(t.Path), recursive: t.Recursive, hidden: t.Exclude.Hidden, names: names})
	}
	snapshot, err := w.scan()
	if err != nil {
		if errors.Is(err, errBudget) {
			return nil, newFileError(CodeInvalid, err.Error(), "")
		}
		return nil, toFileError(err, "")
	}
	w.snapshot = snapshot
	if err := aborted(ctx, ""); err != nil {
		return nil, err
	}
	go w.poll()
	return w, nil
}

func (w *pollWatcher) Mode() WatchMode { return WatchPolling }

func (w *pollWatcher) Close(ctx context.Context) error {
	w.closeOnce.Do(func() { close(w.stop) })
	<-w.done
	return nil
}

func (w *pollWatcher) poll() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
		}
		next, err := w.scan()
		select {
		case <-w.stop:
			return // closed during the scan
		default:
		}
		if err != nil {
			code := CodeInvalid
			if errors.Is(err, fs.ErrPermission) {
				code = CodePermissionDenied
			}
			message := err.Error()
			if errors.Is(err, errBudget) {
				message = fmt.Sprintf("Watched paths exceed %d directories", w.maxDirectories)
			}
			w.deliver(WatchChange{Err: &FileError{Code: code, Message: message, Cause: err}})
			return
		}
		changed := w.diff(w.snapshot, next)
		w.snapshot = next
		if len(changed) > 0 {
			w.deliver(WatchChange{Paths: changed})
		}
	}
}

// deliver calls onChange; a panicking callback must not stop watching.
func (w *pollWatcher) deliver(change WatchChange) {
	defer func() { _ = recover() }()
	w.onChange(change)
}

func (w *pollWatcher) diff(previous, next map[string]watchEntry) []string {
	changed := map[string]bool{}
	for path, entry := range next {
		if before, ok := previous[path]; !ok || before != entry {
			changed[w.reported(path)] = true
		}
	}
	for path := range previous {
		if _, ok := next[path]; !ok {
			changed[w.reported(path)] = true
		}
	}
	out := make([]string, 0, len(changed))
	for path := range changed {
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// reported maps a changed ancestor, which moved every target below it, to
// those targets.
func (w *pollWatcher) reported(path string) string {
	for _, t := range w.targets {
		if isWithin(path, t.path) {
			return path
		}
	}
	for _, t := range w.targets {
		if isWithin(t.path, path) {
			return t.path
		}
	}
	return path
}

func isDenied(err error) bool { return errors.Is(err, fs.ErrPermission) }

func (w *pollWatcher) scan() (map[string]watchEntry, error) {
	snapshot := map[string]watchEntry{}
	// Directories are counted once but traversed once per target:
	// overlapping targets differ in recursion and exclusions.
	counted := map[string]bool{}
	traversed := map[string]bool{}
	// Kinds of listed entries, not following links.
	listed := map[string]entryKind{}
	count := func(path string) error {
		counted[path] = true
		if len(counted) > w.maxDirectories {
			return errBudget
		}
		return nil
	}
	// fileIndex identifies a directory where its identity needs it (Windows);
	// zero is unknown.
	record := func(path string, info fs.FileInfo, fileIndex uint64) {
		e := entryOf(info)
		if e.kind == entryDirectory {
			e.id = withIndex(e.id, fileIndex)
		}
		if e.kind == entryFile && info.Size() <= hashMaxBytes && time.Since(info.ModTime()) < hashRecent {
			if content, err := os.ReadFile(path); err == nil {
				e.hash, e.hashed = sha256.Sum256(content), true
			}
		}
		snapshot[path] = e
	}
	var scanDirectory func(index int, t resolvedTarget, directory string) error
	scanDirectory = func(index int, t resolvedTarget, directory string) error {
		key := fmt.Sprintf("%d\x00%s", index, directory)
		if traversed[key] {
			return nil
		}
		traversed[key] = true
		entries, err := listDirectory(directory)
		if err != nil {
			// The watched directory itself must be readable; below it,
			// unreadable directories are skipped.
			if directory == t.path && isDenied(err) {
				return err
			}
			if errors.Is(err, fs.ErrNotExist) || isDenied(err) || Code(toFileError(err, "")) == CodeNotDirectory {
				return nil
			}
			return err
		}
		for _, entry := range entries {
			name := entry.name
			if t.excluded(name) {
				continue
			}
			path := filepath.Join(directory, name)
			kind, ok := listed[path]
			if !ok {
				info, err := os.Lstat(path)
				if err != nil {
					continue
				}
				kind = kindOf(info)
				listed[path] = kind
				// A target's own entry (following links) wins over its
				// listing by another target.
				if _, recorded := snapshot[path]; !recorded {
					record(path, info, entry.index)
				}
			}
			if t.recursive && kind == entryDirectory {
				if err := count(path); err != nil {
					return err
				}
				if err := scanDirectory(index, t, path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for index, t := range w.targets {
		for _, ancestor := range ancestorsOf(t.path) {
			if _, ok := snapshot[ancestor]; ok {
				continue
			}
			// Identity only: an ancestor's own timestamps change with every
			// unrelated sibling.
			if info, err := os.Lstat(ancestor); err == nil {
				snapshot[ancestor] = watchEntry{kind: kindOf(info), id: ancestorIdentity(info)}
			}
		}
		// The target may be a symbolic link to what is watched; follow it.
		// A missing target is watched for its creation; one that cannot be
		// reached for lack of permission fails.
		info, err := os.Stat(t.path)
		if err != nil {
			if isDenied(err) {
				return nil, err
			}
			continue
		}
		var fileIndex uint64
		if info.IsDir() {
			fileIndex = targetIndex(t.path)
		}
		record(t.path, info, fileIndex)
		if info.IsDir() {
			if err := count(t.path); err != nil {
				return nil, err
			}
			if err := scanDirectory(index, t, t.path); err != nil {
				return nil, err
			}
		}
	}
	return snapshot, nil
}
