package env_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/durabletest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// pi test/env-node.test.ts (its file and text line reader cases) and
// test/env-node-conformance.test.ts. The cases that replace FileHandle
// methods with spies (a read cancelled while it is pending, a failing sync)
// have no Go form: an *os.File's methods cannot be replaced.

var ctx = context.Background()

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equal[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s:\n got  %#v\n want %#v", what, got, want)
	}
}

func code(t *testing.T, err error, want env.FileErrorCode, what string) {
	t.Helper()
	if got := env.Code(err); got != want {
		t.Fatalf("%s: error %v (code %q), want %q", what, err, got, want)
	}
}

func fileError(t *testing.T, err error) *env.FileError {
	t.Helper()
	var fe *env.FileError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v (%T), want a *FileError", err, err)
	}
	return fe
}

// symlinks reports whether this machine lets the test make symbolic links;
// Windows needs developer mode or a privilege.
func symlinks(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	return os.Symlink("target", filepath.Join(dir, "probe")) == nil
}

// TestLocalConformance runs the suite, with its symbolic link cases where
// this machine lets the test make links.
func TestLocalConformance(t *testing.T) {
	var cwd string
	options := durabletest.EnvOptions{}
	if symlinks(t) {
		options.Symlink = func(target, link string) error { return os.Symlink(target, filepath.Join(cwd, link)) }
	} else {
		t.Log("symbolic link cases skipped: this machine does not let the test make links")
	}
	durabletest.TestEnv(t, options, func(t *testing.T) env.FileSystem {
		cwd = t.TempDir()
		return env.NewLocal(cwd)
	})
}

func TestReadsRangesSpanningSeveralInternalChunks(t *testing.T) {
	cwd := t.TempDir()
	data := make([]byte, 5*1024*1024/2)
	for i := range data {
		data[i] = byte((i * 31) % 251)
	}
	noErr(t, os.WriteFile(filepath.Join(cwd, "big.bin"), data, 0o644))
	reader := must(env.NewLocal(cwd).OpenBinaryReader(ctx, "big.bin", env.BinaryReaderOptions{}))
	defer reader.Close()
	all := must(reader.Read(ctx, 0, int64(len(data)+10)))
	if !bytes.Equal(all, data) {
		t.Fatalf("read %d bytes that differ from the file", len(all))
	}
	middle := must(reader.Read(ctx, 1024*1024-3, 7))
	if !bytes.Equal(middle, data[1024*1024-3:1024*1024+4]) {
		t.Fatalf("middle %v", middle)
	}
}

func TestRefusesAFIFOWithoutWaitingForAWriter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no FIFOs")
	}
	cwd := t.TempDir()
	if err := exec.Command("mkfifo", filepath.Join(cwd, "pipe")).Run(); err != nil {
		t.Skip("mkfifo: ", err)
	}
	_, err := env.NewLocal(cwd).OpenBinaryReader(ctx, "pipe", env.BinaryReaderOptions{})
	code(t, err, env.CodeInvalid, "FIFO")
}

func TestReadsWritesListsAndRemoves(t *testing.T) {
	root := t.TempDir()
	e := env.NewLocal(root)
	equal(t, must(e.AbsolutePath(ctx, "nested/child")), filepath.Join(root, "nested/child"), "absolute path")
	equal(t, must(e.JoinPath(ctx, []string{root, "nested", "child"})), filepath.Join(root, "nested", "child"), "joined path")
	noErr(t, e.CreateDir(ctx, "nested/child", true))
	noErr(t, e.WriteFile(ctx, "nested/child/file.txt", []byte("hel")))
	noErr(t, e.AppendFile(ctx, "nested/child/file.txt", []byte("lo")))
	equal(t, must(e.ReadTextFile(ctx, "nested/child/file.txt")), "hello", "text")
	equal(t, strings.Join(must(e.ReadTextLines(ctx, "nested/child/file.txt", 1)), "|"), "hello", "lines")
	equal(t, string(must(e.ReadBinaryFile(ctx, "nested/child/file.txt"))), "hello", "bytes")

	entries := must(e.ListDir(ctx, "nested/child"))
	equal(t, len(entries), 1, "entries")
	equal(t, entries[0].Name, "file.txt", "name")
	equal(t, entries[0].Path, filepath.Join(root, "nested/child/file.txt"), "path")
	equal(t, entries[0].Kind, env.KindFile, "kind")
	equal(t, entries[0].Size, int64(5), "size")
	if entries[0].ModTime.IsZero() {
		t.Fatal("no modification time")
	}

	equal(t, must(e.Exists(ctx, "nested/child/file.txt")), true, "exists")
	noErr(t, e.Remove(ctx, "nested/child/file.txt", env.RemoveOptions{}))
	equal(t, must(e.Exists(ctx, "nested/child/file.txt")), false, "exists after remove")
}

func TestExpandsHomeRelativePathsAndFileURLs(t *testing.T) {
	root := t.TempDir()
	e := env.NewLocal(root)
	home := must(os.UserHomeDir())
	equal(t, must(e.AbsolutePath(ctx, "~/pi-node-env-test")), filepath.Join(home, "pi-node-env-test"), "home path")
	path := filepath.Join(root, "file with spaces.txt")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	if runtime.GOOS == "windows" {
		u.Path = "/" + u.Path
	}
	equal(t, must(e.AbsolutePath(ctx, u.String())), path, "file URL")
}

func TestStatsFilesDirectoriesAndSymlinksWithoutFollowing(t *testing.T) {
	if !symlinks(t) {
		t.Skip("this machine does not let the test make symbolic links")
	}
	root := t.TempDir()
	e := env.NewLocal(root)
	noErr(t, e.CreateDir(ctx, "dir", true))
	noErr(t, e.WriteFile(ctx, "dir/file.txt", []byte("hello")))
	noErr(t, os.Symlink(filepath.Join(root, "dir/file.txt"), filepath.Join(root, "file-link")))
	noErr(t, os.Symlink(filepath.Join(root, "dir"), filepath.Join(root, "dir-link")))
	for _, c := range []struct {
		path string
		kind env.FileKind
	}{{"dir", env.KindDirectory}, {"dir/file.txt", env.KindFile}, {"file-link", env.KindSymlink}, {"dir-link", env.KindSymlink}} {
		info := must(e.Stat(ctx, c.path))
		equal(t, info.Name, filepath.Base(c.path), c.path+" name")
		equal(t, info.Path, filepath.Join(root, c.path), c.path+" path")
		equal(t, info.Kind, c.kind, c.path+" kind")
	}
	equal(t, must(e.Stat(ctx, "dir/file.txt")).Size, int64(5), "file size")
	equal(t, must(e.CanonicalPath(ctx, "file-link")), must(filepath.EvalSymlinks(filepath.Join(root, "dir/file.txt"))), "canonical path")

	var listed []string
	for _, entry := range must(e.ListDir(ctx, ".")) {
		listed = append(listed, entry.Name+":"+string(entry.Kind))
	}
	slices.Sort(listed)
	equal(t, strings.Join(listed, ","), "dir-link:symlink,dir:directory,file-link:symlink", "listing")
}

func TestStopsReadingTextLinesAtTheLimit(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	noErr(t, e.WriteFile(ctx, "file.txt", []byte("one\ntwo\nthree")))
	equal(t, strings.Join(must(e.ReadTextLines(ctx, "file.txt", 1)), "|"), "one", "one line")
	equal(t, strings.Join(must(e.ReadTextLines(ctx, "file.txt", env.AllLines)), "|"), "one|two|three", "all lines")
	equal(t, len(must(e.ReadTextLines(ctx, "file.txt", 0))), 0, "no lines")
}

func TestReportsMissingPathsAndNonDirectories(t *testing.T) {
	root := t.TempDir()
	e := env.NewLocal(root)
	_, err := e.Stat(ctx, "missing.txt")
	fe := fileError(t, err)
	equal(t, fe.Code, env.CodeNotFound, "code")
	equal(t, fe.Path, filepath.Join(root, "missing.txt"), "path")
	equal(t, must(e.Exists(ctx, "missing.txt")), false, "exists")

	noErr(t, e.WriteFile(ctx, "file.txt", []byte("hello")))
	_, err = e.ListDir(ctx, "file.txt")
	code(t, err, env.CodeNotDirectory, "listing a file")
}

func TestAppendsToNewFilesAndCreatesParents(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	noErr(t, e.AppendFile(ctx, "new/nested/file.txt", []byte("a")))
	noErr(t, e.AppendFile(ctx, "new/nested/file.txt", []byte("b")))
	equal(t, must(e.ReadTextFile(ctx, "new/nested/file.txt")), "ab", "text")
}

func TestRenamesAndReplacesTheDestination(t *testing.T) {
	root := t.TempDir()
	e := env.NewLocal(root)
	noErr(t, e.WriteFile(ctx, "source.txt", []byte("new")))
	noErr(t, e.WriteFile(ctx, "destination.txt", []byte("old")))
	noErr(t, e.RenameFile(ctx, "source.txt", "destination.txt"))
	equal(t, must(e.Exists(ctx, "source.txt")), false, "source exists")
	equal(t, must(e.ReadTextFile(ctx, "destination.txt")), "new", "destination")

	noErr(t, e.WriteFile(ctx, "destination.txt", []byte("unchanged")))
	fe := fileError(t, e.RenameFile(ctx, "missing-source.txt", "destination.txt"))
	equal(t, fe.Code, env.CodeNotFound, "code")
	equal(t, fe.Path, filepath.Join(root, "missing-source.txt"), "path")
	equal(t, must(e.ReadTextFile(ctx, "destination.txt")), "unchanged", "destination after a failed rename")
}

func TestCreatesTemporaryDirectoriesAndFiles(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	dir := must(e.CreateTempDir(ctx, "node-env-test-"))
	t.Cleanup(func() { os.RemoveAll(dir) })
	if !strings.HasPrefix(filepath.Base(dir), "node-env-test-") {
		t.Fatalf("temporary directory %s", dir)
	}
	file := must(e.CreateTempFile(ctx, env.TempFileOptions{Prefix: "prefix-", Suffix: ".txt"}))
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(file)) })
	if !strings.HasSuffix(file, ".txt") || !strings.HasPrefix(filepath.Base(file), "prefix-") {
		t.Fatalf("temporary file %s", file)
	}
	equal(t, must(e.ReadTextFile(ctx, file)), "", "temporary file content")
}

func TestHonorsNonRecursiveCreateAndRemoveOptions(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	code(t, e.CreateDir(ctx, "missing/child", false), env.CodeNotFound, "non-recursive create")
	noErr(t, e.WriteFile(ctx, "dir/child/file.txt", []byte("hello")))
	if err := e.Remove(ctx, "dir", env.RemoveOptions{}); err == nil {
		t.Fatal("a directory was removed without Recursive")
	}
	noErr(t, e.Remove(ctx, "dir", env.RemoveOptions{Recursive: true}))
	equal(t, must(e.Exists(ctx, "dir")), false, "exists after a recursive remove")
	code(t, e.Remove(ctx, "missing", env.RemoveOptions{}), env.CodeNotFound, "missing without Force")
	noErr(t, e.Remove(ctx, "missing", env.RemoveOptions{Force: true}))
}

func TestCancelledFileOperationsHaveNoEffect(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	noErr(t, e.WriteFile(ctx, "file.txt", []byte("hello")))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	errs := map[string]error{}
	_, errs["ReadTextFile"] = e.ReadTextFile(cancelled, "file.txt")
	_, errs["ReadTextLines"] = e.ReadTextLines(cancelled, "file.txt", env.AllLines)
	_, errs["ReadBinaryFile"] = e.ReadBinaryFile(cancelled, "file.txt")
	_, errs["OpenTextLineReader"] = e.OpenTextLineReader(cancelled, "file.txt")
	errs["WriteFile"] = e.WriteFile(cancelled, "other.txt", []byte("hello"))
	errs["AppendFile"] = e.AppendFile(cancelled, "file.txt", []byte(" world"))
	errs["TruncateFile"] = e.TruncateFile(cancelled, "file.txt", 1)
	errs["FlushFile"] = e.FlushFile(cancelled, "file.txt")
	errs["RenameFile"] = e.RenameFile(cancelled, "file.txt", "renamed.txt")
	_, errs["Stat"] = e.Stat(cancelled, "file.txt")
	_, errs["ListDir"] = e.ListDir(cancelled, ".")
	_, errs["CanonicalPath"] = e.CanonicalPath(cancelled, "file.txt")
	_, errs["Exists"] = e.Exists(cancelled, "file.txt")
	errs["CreateDir"] = e.CreateDir(cancelled, "dir", true)
	errs["Remove"] = e.Remove(cancelled, "file.txt", env.RemoveOptions{})
	_, errs["CreateTempDir"] = e.CreateTempDir(cancelled, "")
	_, errs["CreateTempFile"] = e.CreateTempFile(cancelled, env.TempFileOptions{})
	for name, err := range errs {
		code(t, err, env.CodeAborted, name)
	}
	equal(t, must(e.ReadTextFile(ctx, "file.txt")), "hello", "content")
	listed := must(e.ListDir(ctx, "."))
	equal(t, len(listed), 1, "entries")
	equal(t, listed[0].Name, "file.txt", "entry")
}

func TestTruncatesAndExtendsToExactSizes(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	noErr(t, e.WriteFile(ctx, "file.bin", []byte{0x61, 0xc3, 0xa9, 0x0a, 0x62, 0x0a}))
	// Truncation is byte-exact even when it splits a UTF-8 sequence.
	noErr(t, e.TruncateFile(ctx, "file.bin", 2))
	equal(t, string(must(e.ReadBinaryFile(ctx, "file.bin"))), "\x61\xc3", "cut")
	noErr(t, e.TruncateFile(ctx, "file.bin", 4))
	equal(t, string(must(e.ReadBinaryFile(ctx, "file.bin"))), "\x61\xc3\x00\x00", "extended")
	noErr(t, e.TruncateFile(ctx, "file.bin", 0))
	equal(t, must(e.Stat(ctx, "file.bin")).Size, int64(0), "emptied")
}

func TestRejectsInvalidTruncationsAndNeverCreatesFiles(t *testing.T) {
	root := t.TempDir()
	e := env.NewLocal(root)
	noErr(t, e.WriteFile(ctx, "file.txt", []byte("hello")))
	for _, size := range []int64{-1, 1<<53 + 1} {
		fe := fileError(t, e.TruncateFile(ctx, "file.txt", size))
		equal(t, fe.Code, env.CodeInvalid, "code")
		equal(t, fe.Path, filepath.Join(root, "file.txt"), "path")
	}
	equal(t, must(e.ReadTextFile(ctx, "file.txt")), "hello", "content")
	fe := fileError(t, e.TruncateFile(ctx, "missing.txt", 0))
	equal(t, fe.Code, env.CodeNotFound, "missing code")
	equal(t, fe.Path, filepath.Join(root, "missing.txt"), "missing path")
	equal(t, must(e.Exists(ctx, "missing.txt")), false, "missing exists")
}

func TestFlushesFilesAndRefusesMissingPathsAndDirectories(t *testing.T) {
	root := t.TempDir()
	e := env.NewLocal(root)
	noErr(t, e.WriteFile(ctx, "file.txt", []byte("durable")))
	noErr(t, e.FlushFile(ctx, "file.txt"))
	equal(t, must(e.ReadTextFile(ctx, "file.txt")), "durable", "content")
	fe := fileError(t, e.FlushFile(ctx, "missing.txt"))
	equal(t, fe.Code, env.CodeNotFound, "missing code")
	equal(t, fe.Path, filepath.Join(root, "missing.txt"), "missing path")
	equal(t, must(e.Exists(ctx, "missing.txt")), false, "missing exists")
	noErr(t, e.CreateDir(ctx, "dir", true))
	code(t, e.FlushFile(ctx, "dir"), env.CodeIsDirectory, "directory")
}

// TestDirReaderSkipsEntriesRemovedAfterTheyWereRead: the conformance case
// removes entries before the first page, which a lazily reading directory
// never enumerates. Here the first page makes the reader read the
// directory, so the later entries are enumerated, and vanish before their
// metadata is read.
func TestDirReaderSkipsEntriesRemovedAfterTheyWereRead(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	names := []string{"a", "b", "c", "d", "e"}
	for _, name := range names {
		noErr(t, e.WriteFile(ctx, "dir/"+name, []byte(name)))
	}
	reader := must(e.OpenDirReader(ctx, "dir"))
	defer reader.Close()
	first, done, err := reader.Next(ctx, 1)
	noErr(t, err)
	equal(t, len(first), 1, "first page")
	equal(t, done, false, "done after the first page")
	for _, name := range names {
		noErr(t, e.Remove(ctx, "dir/"+name, env.RemoveOptions{Force: true}))
	}
	var rest []env.FileInfo
	for range 10 {
		page, done, err := reader.Next(ctx, 10)
		noErr(t, err)
		rest = append(rest, page...)
		if done {
			break
		}
	}
	equal(t, len(rest), 0, "entries after removal")
}

func TestCleanupIsBestEffort(t *testing.T) {
	env.NewLocal(t.TempDir()).Cleanup(ctx)
}

func readLines(t *testing.T, r env.TextLineReader) []env.TextLine {
	t.Helper()
	var lines []env.TextLine
	for {
		line, err := r.ReadLine(ctx)
		if errors.Is(err, io.EOF) {
			return lines
		}
		noErr(t, err)
		lines = append(lines, line)
	}
}

func TestTextLinesReportTerminationAndKeepCarriageReturns(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	noErr(t, e.WriteFile(ctx, "lines.txt", []byte("one\r\n\ntwo\npartial")))
	r := must(e.OpenTextLineReader(ctx, "lines.txt"))
	defer r.Close()
	got := readLines(t, r)
	want := []env.TextLine{{Text: "one\r", Terminated: true}, {Text: "", Terminated: true}, {Text: "two", Terminated: true}, {Text: "partial"}}
	if !slices.Equal(got, want) {
		t.Fatalf("lines %#v", got)
	}
	_, err := r.ReadLine(ctx)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read after the end: %v", err)
	}
}

func TestTextLinesOfAnEmptyFileAndALoneNewline(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	noErr(t, e.WriteFile(ctx, "empty.txt", nil))
	noErr(t, e.WriteFile(ctx, "newline.txt", []byte("\n")))
	empty := must(e.OpenTextLineReader(ctx, "empty.txt"))
	equal(t, len(readLines(t, empty)), 0, "empty file lines")
	empty.Close()
	newline := must(e.OpenTextLineReader(ctx, "newline.txt"))
	got := readLines(t, newline)
	newline.Close()
	if !slices.Equal(got, []env.TextLine{{Text: "", Terminated: true}}) {
		t.Fatalf("lines %#v", got)
	}
}

func TestTextLinesDecodeCharactersSplitAcrossChunks(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	// The reader reads 64 KiB chunks; a four-byte character crosses the first
	// boundary.
	first := strings.Repeat("a", 64*1024-2) + "😀tail"
	second := strings.Repeat("é", 100_000)
	noErr(t, e.WriteFile(ctx, "large.txt", []byte(first+"\n"+second)))
	r := must(e.OpenTextLineReader(ctx, "large.txt"))
	defer r.Close()
	got := readLines(t, r)
	if !slices.Equal(got, []env.TextLine{{Text: first, Terminated: true}, {Text: second}}) {
		t.Fatalf("%d lines, first %d bytes", len(got), len(got[0].Text))
	}
}

func TestTextLinesDropOnlyALeadingByteOrderMark(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	noErr(t, e.WriteFile(ctx, "bom.txt", []byte("\xef\xbb\xbfa\n\xef\xbb\xbfb\xff")))
	r := must(e.OpenTextLineReader(ctx, "bom.txt"))
	defer r.Close()
	got := readLines(t, r)
	if !slices.Equal(got, []env.TextLine{{Text: "a", Terminated: true}, {Text: "\ufeffb\ufffd"}}) {
		t.Fatalf("lines %#v", got)
	}
}

func TestACancelledReadConsumesNothing(t *testing.T) {
	e := env.NewLocal(t.TempDir())
	noErr(t, e.WriteFile(ctx, "lines.txt", []byte("one\ntwo\n")))
	r := must(e.OpenTextLineReader(ctx, "lines.txt"))
	defer r.Close()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := r.ReadLine(cancelled)
	code(t, err, env.CodeAborted, "cancelled read")
	got := readLines(t, r)
	if !slices.Equal(got, []env.TextLine{{Text: "one", Terminated: true}, {Text: "two", Terminated: true}}) {
		t.Fatalf("lines %#v", got)
	}
}

func TestTextLineReaderRefusesReadsAfterClose(t *testing.T) {
	root := t.TempDir()
	e := env.NewLocal(root)
	noErr(t, e.WriteFile(ctx, "lines.txt", []byte("one\n")))
	r := must(e.OpenTextLineReader(ctx, "lines.txt"))
	noErr(t, r.Close())
	noErr(t, r.Close())
	_, err := r.ReadLine(ctx)
	fe := fileError(t, err)
	equal(t, fe.Code, env.CodeInvalid, "code")
	equal(t, fe.Path, filepath.Join(root, "lines.txt"), "path")

	_, err = e.OpenTextLineReader(ctx, "missing.txt")
	fe = fileError(t, err)
	equal(t, fe.Code, env.CodeNotFound, "missing code")
	equal(t, fe.Path, filepath.Join(root, "missing.txt"), "missing path")
}
