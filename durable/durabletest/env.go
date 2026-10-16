package durabletest

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// EnvOptions configure the environment conformance suite.
type EnvOptions struct {
	// Shell is the argv prefix that runs a script, such as {"sh", "-c"} (the
	// default); on Windows, Git Bash's path and "-c".
	Shell []string
	// Symlink makes link a symbolic link to target, both relative to the
	// environment's working directory; nil skips the cases that need links.
	// pi's suite makes links through the environment's shell, which on
	// Windows copies instead of linking unless native links are enabled.
	Symlink func(target, link string) error
}

// TestEnv runs the environment conformance suite (pi
// testing/env-conformance.ts). open returns an environment whose working
// directory is a fresh, empty, writable directory. pi's listFiles cases are
// not here: no environment of the engine implements it yet.
func TestEnv(t *testing.T, options EnvOptions, open func(t *testing.T) env.ExecutionEnv) {
	if options.Shell == nil {
		options.Shell = []string{"sh", "-c"}
	}
	for _, c := range slices.Concat(envCases, envExecCases, envWatchCases) {
		if c.symlinks && options.Symlink == nil {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			c.run(&envChecker{t: t, env: open(t), options: options})
		})
	}
}

type envChecker struct {
	t       *testing.T
	env     env.ExecutionEnv
	options EnvOptions
}

func (c *envChecker) must(err error) {
	c.t.Helper()
	if err != nil {
		c.t.Fatal(err)
	}
}

func (c *envChecker) code(err error, want env.FileErrorCode, what string) {
	c.t.Helper()
	if got := env.Code(err); got != want {
		c.t.Fatalf("%s: error %v (code %q), want code %q", what, err, got, want)
	}
}

func (c *envChecker) equal(got, want any, what string) {
	c.t.Helper()
	equalValues(c.t, got, want, what)
}

func equalValues(t *testing.T, got, want any, what string) {
	t.Helper()
	if g, ok := got.([]byte); ok {
		if !bytes.Equal(g, want.([]byte)) {
			t.Fatalf("%s:\n got  %q\n want %q", what, g, want)
		}
		return
	}
	if got != want {
		t.Fatalf("%s:\n got  %#v\n want %#v", what, got, want)
	}
}

func (c *envChecker) write(path, content string) {
	c.t.Helper()
	c.must(c.env.WriteFile(ctx, path, []byte(content)))
}

func cancelledContext() context.Context {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	return cancelled
}

func (c *envChecker) readAll(path string, maxEntries int) (pages [][]env.FileInfo, done bool) {
	c.t.Helper()
	reader, err := c.env.OpenDirReader(ctx, path)
	c.must(err)
	defer reader.Close()
	for range 1000 {
		entries, done, err := reader.Next(ctx, maxEntries)
		c.must(err)
		pages = append(pages, entries)
		if done {
			return pages, true
		}
	}
	return pages, false
}

type envCase struct {
	name     string
	symlinks bool
	run      func(c *envChecker)
}

var envCases = []envCase{
	{name: "binary reader reads byte ranges of the opened file", run: func(c *envChecker) {
		c.write("data.txt", "hello world")
		reader, err := c.env.OpenBinaryReader(ctx, "data.txt", env.BinaryReaderOptions{})
		c.must(err)
		info, err := reader.Info(ctx)
		c.must(err)
		c.equal(info.Name, "data.txt", "name")
		c.equal(info.Kind, env.KindFile, "kind")
		c.equal(info.Size, int64(11), "size")
		read := func(offset, length int64) []byte {
			c.t.Helper()
			b, err := reader.Read(ctx, offset, length)
			c.must(err)
			return b
		}
		c.equal(string(read(0, 5)), "hello", "read 0 5")
		c.equal(string(read(6, 100)), "world", "read 6 100")
		c.equal(len(read(11, 4)), 0, "read at the end")
		c.equal(len(read(50, 1)), 0, "read past the end")
		c.equal(len(read(3, 0)), 0, "empty read")
		_, err = reader.Read(ctx, -1, 1)
		c.code(err, env.CodeInvalid, "negative offset")
		_, err = reader.Read(ctx, 0, -1)
		c.code(err, env.CodeInvalid, "negative length")
		_, err = reader.Read(cancelledContext(), 0, 1)
		c.code(err, env.CodeAborted, "cancelled read")
		c.must(reader.Close())
		c.must(reader.Close())
		_, err = reader.Read(ctx, 0, 1)
		c.code(err, env.CodeInvalid, "read after close")
		_, err = reader.Info(ctx)
		c.code(err, env.CodeInvalid, "info after close")
	}},

	{name: "binary reader scans lines like decoding the whole file", run: func(c *envChecker) {
		// A byte-order mark, an invalid sequence before a newline, an empty
		// line, a later U+FEFF, and no final newline.
		data := []byte{0xef, 0xbb, 0xbf, 0x61, 0x0a, 0xe2, 0x82, 0x0a, 0x0a, 0xef, 0xbb, 0xbf, 0x62, 0x0a, 0xc3, 0xa9}
		c.must(c.env.WriteFile(ctx, "lines.txt", data))
		lines := strings.Split(jstext.DecodeText(data), "\n")
		reader, err := c.env.OpenBinaryReader(ctx, "lines.txt", env.BinaryReaderOptions{})
		c.must(err)
		defer reader.Close()
		for _, r := range []env.LineRange{{Start: 0}, {Start: 0, End: 1}, {Start: 1, End: 3}, {Start: 2, End: 3}, {Start: 3}, {Start: 4, End: 9}} {
			scan, err := reader.ScanLines(ctx, r)
			c.must(err)
			end := int64(len(lines))
			if r.End != 0 {
				end = min(r.End, end)
			}
			selected := strings.Join(lines[min(r.Start, int64(len(lines))):end], "\n")
			c.equal(scan.Newlines, int64(len(lines)-1), "newlines")
			c.equal(decodeRange(data, scan.Start, scan.End), selected, "selection")
			c.equal(scan.SelectedBytes, int64(len(selected)), "selected bytes")
			c.equal(decodeRange(data, scan.Start, scan.FirstLineEnd), lines[r.Start], "first line")
			c.equal(scan.FirstLineBytes, int64(len(lines[r.Start])), "first line bytes")
		}
		scan, err := reader.ScanLines(ctx, env.LineRange{Start: 9})
		c.must(err)
		size := int64(len(data))
		c.equal(scan.Start, size, "start past the last line")
		c.equal(scan.End, size, "end past the last line")
		c.equal(scan.SelectedBytes, int64(0), "selected bytes past the last line")
		_, err = reader.ScanLines(ctx, env.LineRange{Start: 2, End: 2})
		c.code(err, env.CodeInvalid, "empty range")
	}},

	{name: "binary reader keeps reading the file it opened after a rename", run: func(c *envChecker) {
		c.write("a.txt", "one")
		reader, err := c.env.OpenBinaryReader(ctx, "a.txt", env.BinaryReaderOptions{})
		c.must(err)
		defer reader.Close()
		c.must(c.env.RenameFile(ctx, "a.txt", "b.txt"))
		c.write("a.txt", "two")
		b, err := reader.Read(ctx, 0, 10)
		c.must(err)
		c.equal(string(b), "one", "content")
	}},

	{name: "binary reader refuses directories, missing files and aborted opens", run: func(c *envChecker) {
		c.must(c.env.CreateDir(ctx, "dir", true))
		c.write("file.txt", "x")
		_, err := c.env.OpenBinaryReader(ctx, "dir", env.BinaryReaderOptions{})
		c.code(err, env.CodeIsDirectory, "directory")
		_, err = c.env.OpenBinaryReader(ctx, "missing.txt", env.BinaryReaderOptions{})
		c.code(err, env.CodeNotFound, "missing file")
		_, err = c.env.OpenBinaryReader(cancelledContext(), "file.txt", env.BinaryReaderOptions{})
		c.code(err, env.CodeAborted, "cancelled open")
	}},

	{name: "directory reader pages every entry exactly once", run: func(c *envChecker) {
		names := []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}
		for _, name := range names {
			c.write(name, name)
		}
		c.must(c.env.CreateDir(ctx, "sub", true))
		pages, done := c.readAll(".", 2)
		if !done {
			c.t.Fatal("the directory reader did not reach the end")
		}
		var entries []env.FileInfo
		for _, page := range pages {
			if len(page) > 2 {
				c.t.Fatalf("page of %d entries, want at most 2", len(page))
			}
			entries = append(entries, page...)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name)
			switch e.Name {
			case "sub":
				c.equal(e.Kind, env.KindDirectory, "sub kind")
			case "a.txt":
				c.equal(e.Kind, env.KindFile, "a.txt kind")
				c.equal(e.Size, int64(5), "a.txt size")
			}
		}
		slices.Sort(got)
		c.equal(strings.Join(got, ","), strings.Join(append(names, "sub"), ","), "entries")
	}},

	{name: "directory reader reports the end and refuses use after close", run: func(c *envChecker) {
		c.must(c.env.CreateDir(ctx, "empty", true))
		reader, err := c.env.OpenDirReader(ctx, "empty")
		c.must(err)
		for range 2 {
			entries, done, err := reader.Next(ctx, 10)
			c.must(err)
			c.equal(len(entries), 0, "entries")
			c.equal(done, true, "done")
		}
		_, _, err = reader.Next(ctx, 0)
		c.code(err, env.CodeInvalid, "zero entries")
		_, _, err = reader.Next(cancelledContext(), 1)
		c.code(err, env.CodeAborted, "cancelled next")
		c.must(reader.Close())
		c.must(reader.Close())
		_, _, err = reader.Next(ctx, 1)
		c.code(err, env.CodeInvalid, "next after close")
	}},

	{name: "directory reader refuses missing paths and files", run: func(c *envChecker) {
		c.write("file.txt", "x")
		_, err := c.env.OpenDirReader(ctx, "missing")
		c.code(err, env.CodeNotFound, "missing directory")
		_, err = c.env.OpenDirReader(ctx, "file.txt")
		c.code(err, env.CodeNotDirectory, "file")
		_, err = c.env.OpenDirReader(cancelledContext(), ".")
		c.code(err, env.CodeAborted, "cancelled open")
	}},

	{name: "directory reader skips entries removed during enumeration", run: func(c *envChecker) {
		c.must(c.env.CreateDir(ctx, "dir", true))
		for _, name := range []string{"x", "y", "z"} {
			c.write("dir/"+name, name)
		}
		reader, err := c.env.OpenDirReader(ctx, "dir")
		c.must(err)
		defer reader.Close()
		for _, name := range []string{"x", "y", "z"} {
			c.must(c.env.Remove(ctx, "dir/"+name, env.RemoveOptions{}))
		}
		var entries []env.FileInfo
		for range 10 {
			page, done, err := reader.Next(ctx, 10)
			c.must(err)
			entries = append(entries, page...)
			if done {
				break
			}
		}
		c.equal(len(entries), 0, "entries")
	}},

	{name: "binary reader follows symlinks unless noFollow refuses the final one", symlinks: true, run: func(c *envChecker) {
		c.write("target.txt", "target")
		c.must(c.env.CreateDir(ctx, "sub", true))
		c.write("sub/inner.txt", "inner")
		c.must(c.options.Symlink("target.txt", "link.txt"))
		c.must(c.options.Symlink("sub", "dirlink"))

		followed, err := c.env.OpenBinaryReader(ctx, "link.txt", env.BinaryReaderOptions{})
		c.must(err)
		b, err := followed.Read(ctx, 0, 10)
		c.must(err)
		c.equal(string(b), "target", "followed link")
		c.must(followed.Close())

		_, err = c.env.OpenBinaryReader(ctx, "link.txt", env.BinaryReaderOptions{NoFollow: true})
		c.code(err, env.CodeInvalid, "noFollow link")

		// Only the final component is refused; earlier linked directories
		// still resolve.
		inner, err := c.env.OpenBinaryReader(ctx, "dirlink/inner.txt", env.BinaryReaderOptions{NoFollow: true})
		c.must(err)
		b, err = inner.Read(ctx, 0, 10)
		c.must(err)
		c.equal(string(b), "inner", "file below a linked directory")
		c.must(inner.Close())
	}},
}

// decodeRange is a TextDecoder of data[from:to], ignoring a byte-order mark
// unless the range starts the file.
func decodeRange(data []byte, from, to int64) string {
	d := jstext.TextDecoder{IgnoreBOM: from > 0}
	return d.Decode(data[from:to]) + d.Flush()
}
