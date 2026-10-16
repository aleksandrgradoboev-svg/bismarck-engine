package durabletest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// The command and watch cases of pi's environment conformance suite.

type collected struct {
	result env.ExecResult
	err    error
	stdout string
	stderr string
}

func (c *envChecker) exec(command env.Command, cwd string) collected {
	c.t.Helper()
	var out collected
	var mu sync.Mutex
	out.result, out.err = c.env.Exec(ctx, command, &env.ExecOptions{Cwd: cwd, OnOutput: func(_ context.Context, text string, info env.OutputInfo) error {
		mu.Lock()
		defer mu.Unlock()
		if info.Stream == env.Stdout {
			out.stdout += text
		} else {
			out.stderr += text
		}
		return nil
	}})
	return out
}

func (c *envChecker) shell(script string, args ...string) env.Command {
	return env.Argv(append(append(append([]string(nil), c.options.Shell...), script), args...)...)
}

func (c *envChecker) execCode(err error, want env.ExecutionErrorCode, what string) {
	c.t.Helper()
	if got := env.ExecCode(err); got != want {
		c.t.Fatalf("%s: error %v (code %q), want code %q", what, err, got, want)
	}
}

var envExecCases = []envCase{
	{name: "argv exec passes arguments to the program without shell parsing", run: func(c *envChecker) {
		hostile := "it's $(touch pwned) `touch pwned` *; touch pwned"
		out := c.exec(c.shell(`printf "%s|%s" "$1" "$2"`, "argv0", hostile, "a b"), "")
		c.must(out.err)
		c.equal(out.result.ExitCode, 0, "exit code")
		c.equal(out.stdout, hostile+"|a b", "stdout")
		exists, err := c.env.Exists(ctx, "pwned")
		c.must(err)
		c.equal(exists, false, "pwned exists")
	}},

	{name: "exec reports the stream of every chunk in both forms", run: func(c *envChecker) {
		script := "printf out; printf err >&2; printf more"
		for _, command := range []env.Command{c.shell(script), env.Script(script)} {
			out := c.exec(command, "")
			c.must(out.err)
			c.equal(out.result.ExitCode, 0, "exit code")
			c.equal(out.stdout, "outmore", "stdout")
			c.equal(out.stderr, "err", "stderr")
		}
	}},

	{name: "argv exec honors cwd and exit codes", run: func(c *envChecker) {
		c.must(c.env.CreateDir(ctx, "sub", true))
		out := c.exec(c.shell("printf x > made.txt; exit 3"), "sub")
		c.must(out.err)
		c.equal(out.result.ExitCode, 3, "exit code")
		text, err := c.env.ReadTextFile(ctx, "sub/made.txt")
		c.must(err)
		c.equal(text, "x", "made.txt")
	}},

	{name: "argv exec reports missing programs and empty argv as spawn errors", run: func(c *envChecker) {
		_, err := c.env.Exec(ctx, env.Argv("pi-durable-conformance-missing-program"), nil)
		c.execCode(err, env.ExecSpawnError, "missing program")
		_, err = c.env.Exec(ctx, env.Argv(), nil)
		c.execCode(err, env.ExecSpawnError, "empty argv")
	}},

	{name: "windowed exec keeps the exact tail and counts what it skips", run: func(c *envChecker) {
		const lines = 2000
		window := &env.OutputWindow{MaxBytes: 200, MaxLines: 5, BytesPerSecond: 1e9}
		var bytes, newlines int64
		var tail strings.Builder
		result, err := c.env.Exec(ctx, c.shell(fmt.Sprintf(`i=0; while [ $i -lt %d ]; do echo line-$i; i=$((i+1)); done`, lines)), &env.ExecOptions{
			Window: window,
			OnOutput: func(_ context.Context, text string, info env.OutputInfo) error {
				if info.Skipped != nil {
					bytes += info.Skipped.Bytes
					newlines += info.Skipped.Newlines
					if after := int64(strings.Count(text, "\n")); int64(len(text)) <= window.MaxBytes && after <= window.MaxLines {
						return fmt.Errorf("a skip is followed by no more than the window")
					}
					tail.Reset()
				}
				bytes += int64(len(text))
				newlines += int64(strings.Count(text, "\n"))
				tail.WriteString(text)
				return nil
			},
		})
		c.must(err)
		c.equal(result.ExitCode, 0, "exit code")
		var expected strings.Builder
		var last strings.Builder
		for i := range lines {
			expected.WriteString(fmt.Sprintf("line-%d\n", i))
			if i >= lines-int(window.MaxLines) {
				last.WriteString(fmt.Sprintf("line-%d\n", i))
			}
		}
		c.equal(bytes, int64(expected.Len()), "bytes")
		c.equal(newlines, int64(lines), "newlines")
		if !strings.HasSuffix(tail.String(), last.String()) {
			c.t.Fatal("the delivered output does not end with the tail")
		}
	}},

	{name: "argv exec distinguishes timeout from abort", run: func(c *envChecker) {
		_, err := c.env.Exec(ctx, c.shell("sleep 2"), &env.ExecOptions{Timeout: 100 * time.Millisecond})
		c.execCode(err, env.ExecTimeout, "timeout")
		cancellable, cancel := context.WithCancel(ctx)
		time.AfterFunc(100*time.Millisecond, cancel)
		_, err = c.env.Exec(cancellable, c.shell("sleep 2"), nil)
		c.execCode(err, env.ExecAborted, "abort")
	}},
}

// changes collects a watcher's changes.
type changes struct {
	mu   sync.Mutex
	list []env.WatchChange
}

func (cs *changes) add(change env.WatchChange) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.list = append(cs.list, change)
}

func (cs *changes) since(from int) []env.WatchChange {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]env.WatchChange(nil), cs.list[from:]...)
}

func (cs *changes) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.list)
}

// covers reports whether a change reports path: an overflow, or a reported
// path at or above it.
func covers(change env.WatchChange, path string) bool {
	if change.Overflow {
		return true
	}
	for _, reported := range change.Paths {
		if path == reported || strings.HasPrefix(path, reported+"/") || strings.HasPrefix(path, reported+`\`) {
			return true
		}
	}
	return false
}

type watchHelpers struct {
	c       *envChecker
	changes *changes
}

func (h watchHelpers) absolute(path string) string {
	h.c.t.Helper()
	abs, err := h.c.env.AbsolutePath(ctx, path)
	h.c.must(err)
	return abs
}

// expectChange waits up to three seconds after change for a change that
// reports path.
func (h watchHelpers) expectChange(path string, change func()) {
	h.c.t.Helper()
	target := h.absolute(path)
	from := h.changes.count()
	change()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, entry := range h.changes.since(from) {
			if covers(entry, target) {
				return
			}
		}
		if time.Now().After(deadline) {
			h.c.t.Fatalf("no change reported %s; got %+v", target, h.changes.since(from))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *envChecker) watching(targets []env.WatchTarget, run func(h watchHelpers)) {
	c.t.Helper()
	cs := &changes{}
	watcher, err := c.env.Watch(ctx, targets, cs.add)
	c.must(err)
	defer watcher.Close(ctx)
	run(watchHelpers{c: c, changes: cs})
}

var envWatchCases = []envCase{
	{name: "watch reports a missing file's creation, changes, replacement and removal", run: func(c *envChecker) {
		c.watching([]env.WatchTarget{{Path: "AGENTS.md"}}, func(h watchHelpers) {
			h.expectChange("AGENTS.md", func() { c.write("AGENTS.md", "one") })
			h.expectChange("AGENTS.md", func() { c.write("AGENTS.md", "two!") })
			// Editors replace a file by renaming a new one over it.
			h.expectChange("AGENTS.md", func() {
				c.write("AGENTS.md.tmp", "three")
				c.must(c.env.RenameFile(ctx, "AGENTS.md.tmp", "AGENTS.md"))
			})
			h.expectChange("AGENTS.md", func() { c.write("AGENTS.md", "four") })
			h.expectChange("AGENTS.md", func() { c.must(c.env.Remove(ctx, "AGENTS.md", env.RemoveOptions{})) })
		})
	}},

	{name: "watch reports a missing target whose ancestors are created", run: func(c *envChecker) {
		c.watching([]env.WatchTarget{{Path: "a/b/c/AGENTS.md"}}, func(h watchHelpers) {
			h.expectChange("a/b/c/AGENTS.md", func() { c.write("a/b/c/AGENTS.md", "x") })
		})
	}},

	{name: "watch follows directories created together with their contents", run: func(c *envChecker) {
		c.must(c.env.CreateDir(ctx, "skills", true))
		c.watching([]env.WatchTarget{{Path: "skills", Recursive: true}}, func(h watchHelpers) {
			h.expectChange("skills/a/b/SKILL.md", func() { c.write("skills/a/b/SKILL.md", "one") })
			h.expectChange("skills/a/b/SKILL.md", func() { c.write("skills/a/b/SKILL.md", "two!") })
			h.expectChange("skills/a/b/c/SKILL.md", func() { c.write("skills/a/b/c/SKILL.md", "deeper") })
		})
	}},

	{name: "watch keeps watching a path whose parent is renamed and recreated", run: func(c *envChecker) {
		c.write("proj/.pi/skills/x.md", "x")
		c.watching([]env.WatchTarget{{Path: "proj/.pi/skills", Recursive: true}}, func(h watchHelpers) {
			h.expectChange("proj/.pi/skills", func() { c.must(c.env.RenameFile(ctx, "proj/.pi", "proj/old")) })
			h.expectChange("proj/.pi/skills/y.md", func() { c.write("proj/.pi/skills/y.md", "y") })
			h.expectChange("proj/.pi/skills/y.md", func() { c.write("proj/.pi/skills/y.md", "yy") })
		})
	}},

	{name: "watch skips excluded entries and reports a rename out of them", run: func(c *envChecker) {
		c.must(c.env.CreateDir(ctx, "skills", true))
		targets := []env.WatchTarget{{Path: "skills", Recursive: true, Exclude: env.WatchExclude{Hidden: true, Names: []string{"node_modules"}}}}
		c.watching(targets, func(h watchHelpers) {
			c.write("skills/node_modules/dep/SKILL.md", "dep")
			c.write("skills/.SKILL.md.tmp", "draft")
			h.expectChange("skills/SKILL.md", func() { c.must(c.env.RenameFile(ctx, "skills/.SKILL.md.tmp", "skills/SKILL.md")) })
			hidden := []string{h.absolute("skills/node_modules"), h.absolute("skills/.SKILL.md.tmp")}
			for _, change := range h.changes.since(0) {
				for _, path := range change.Paths {
					for _, excluded := range hidden {
						if strings.HasPrefix(path, excluded) {
							c.t.Fatalf("excluded %s reported", path)
						}
					}
				}
			}
		})
	}},

	{name: "watch keeps recursive coverage where a non-recursive target overlaps", run: func(c *envChecker) {
		c.write("skills/a/one.md", "one")
		c.watching([]env.WatchTarget{{Path: "skills"}, {Path: "skills", Recursive: true}}, func(h watchHelpers) {
			h.expectChange("skills/a/two.md", func() { c.write("skills/a/two.md", "two") })
		})
	}},

	{name: "watch follows a directory replaced at the same path", run: func(c *envChecker) {
		c.write("skills/a/x.md", "x")
		c.watching([]env.WatchTarget{{Path: "skills", Recursive: true}}, func(h watchHelpers) {
			h.expectChange("skills/a", func() {
				c.must(c.env.RenameFile(ctx, "skills/a", "skills-old"))
				c.must(c.env.CreateDir(ctx, "skills/a", true))
			})
			h.expectChange("skills/a/y.md", func() { c.write("skills/a/y.md", "y") })
			h.expectChange("skills/a/y.md", func() { c.write("skills/a/y.md", "yy") })
		})
	}},

	{name: "watch stops reporting once closed", run: func(c *envChecker) {
		cs := &changes{}
		watcher, err := c.env.Watch(ctx, []env.WatchTarget{{Path: "file.txt"}}, cs.add)
		c.must(err)
		if mode := watcher.Mode(); mode != env.WatchNative && mode != env.WatchPolling {
			c.t.Fatalf("mode %q", mode)
		}
		c.must(watcher.Close(ctx))
		c.must(watcher.Close(ctx))
		c.write("file.txt", "x")
		time.Sleep(300 * time.Millisecond)
		c.equal(cs.count(), 0, "changes after close")
	}},

	{name: "watch reports changes to the file a watched symbolic link points to", symlinks: true, run: func(c *envChecker) {
		c.write("data/real.md", "one")
		c.must(c.env.CreateDir(ctx, "config", true))
		c.must(c.options.Symlink("../data/real.md", "config/AGENTS.md"))
		c.watching([]env.WatchTarget{{Path: "config/AGENTS.md"}}, func(h watchHelpers) {
			h.expectChange("config/AGENTS.md", func() { c.write("data/real.md", "two!") })
		})
	}},
}
