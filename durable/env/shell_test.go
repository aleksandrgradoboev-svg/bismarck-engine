package env_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/proc"
)

// pi test/env-node.test.ts, its shell cases, and the watch limit case of
// env-node-conformance. Scripts run through bash: Git Bash on Windows.
// n/a: pi's spill backpressure suite (env-node-spill), since the spill is
// written synchronously by the loop that reads the output, and the
// asynchronous taskkill spawn error, which Go reports synchronously.

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := proc.ResolveShell("", func(p string) bool { _, err := os.Stat(p); return err == nil }, ""); err != nil {
		t.Skip("no bash: ", err)
	}
}

func execCode(t *testing.T, err error, want env.ExecutionErrorCode, what string) *env.ExecutionError {
	t.Helper()
	var ee *env.ExecutionError
	if !errors.As(err, &ee) || ee.Code != want {
		t.Fatalf("%s: error %v, want code %q", what, err, want)
	}
	return ee
}

// collect runs a script and gathers its combined output.
func collect(e *env.Local, ctx context.Context, script string, options *env.ExecOptions) (env.ExecResult, string, error) {
	if options == nil {
		options = &env.ExecOptions{}
	}
	var mu sync.Mutex
	var out strings.Builder
	options.OnOutput = func(_ context.Context, text string, _ env.OutputInfo) error {
		mu.Lock()
		defer mu.Unlock()
		out.WriteString(text)
		return nil
	}
	result, err := e.Exec(ctx, env.Script(script), options)
	return result, out.String(), err
}

func TestExecutesInCwdWithEnvOverrides(t *testing.T) {
	requireBash(t)
	root := tempDir(t)
	result, output, err := collect(env.NewLocal(root), ctx,
		`printf '%s' "$NODE_ENV_TEST" > cwd-marker.txt; printf '%s:%s' "$PWD" "$NODE_ENV_TEST"`,
		&env.ExecOptions{Env: map[string]string{"NODE_ENV_TEST": "ok"}})
	noErr(t, err)
	equal(t, result.ExitCode, 0, "exit code")
	equal(t, string(must(os.ReadFile(filepath.Join(root, "cwd-marker.txt")))), "ok", "marker")
	// Git Bash reports $PWD as an MSYS path, not the Windows path.
	if runtime.GOOS != "windows" {
		equal(t, output, must(filepath.EvalSymlinks(root))+":ok", "output")
	}
}

func TestAppliesShellEnvironmentOverrides(t *testing.T) {
	requireBash(t)
	for _, c := range []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{"a missing override preserves the base value", nil, "x:/stale/parent.jsonl"},
		{"an empty override shadows the base value", map[string]string{"PI_SESSION_FILE": ""}, "x:"},
		{"a string override replaces the base value", map[string]string{"PI_SESSION_FILE": "/sessions/current.jsonl"}, "x:/sessions/current.jsonl"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := env.NewLocalWith(tempDir(t), env.LocalOptions{ShellEnv: map[string]string{
				"PI_SESSION_FILE":            "/stale/parent.jsonl",
				"PI_CODING_AGENT":            "true",
				"PI_NODE_ENV_PRESERVED_TEST": "preserved",
			}})
			_, output, err := collect(e, ctx, `printf '%s:%s|%s|%s' "${PI_SESSION_FILE+x}" "${PI_SESSION_FILE-}" "$PI_CODING_AGENT" "$PI_NODE_ENV_PRESERVED_TEST"`,
				&env.ExecOptions{Env: c.overrides})
			noErr(t, err)
			equal(t, output, c.want+"|true|preserved", "output")
		})
	}
}

func TestReplacesRatherThanInheritsTheEnvironment(t *testing.T) {
	requireBash(t)
	t.Setenv("PI_NODE_ENV_INHERITED_TEST", "host")
	e := env.NewLocalWith(tempDir(t), env.LocalOptions{ShellEnv: map[string]string{"PI_NODE_ENV_CONFIGURED_TEST": "configured"}})
	inherit := false
	// Without the inherited environment, bash has no PATH either; printf is
	// a builtin.
	_, output, err := collect(e, ctx, `printf '%s:%s:%s' "${PI_NODE_ENV_INHERITED_TEST-}" "${PI_NODE_ENV_CONFIGURED_TEST-}" "${PI_NODE_ENV_EXPLICIT_TEST-}"`,
		&env.ExecOptions{InheritEnv: &inherit, Env: map[string]string{"PI_NODE_ENV_EXPLICIT_TEST": "explicit"}})
	noErr(t, err)
	equal(t, output, "::explicit", "output")
}

func stringPtr(s string) *string { return &s }

// pi#9490 (upstream 61060ddf): legacy WSL detection must also handle a custom
// Windows directory; an empty SystemRoot falls back to X:\Windows.
func TestSelectsTheCommandTransportForLegacyWSLBashPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a POSIX script")
	}
	for _, c := range []struct {
		shellPath  string
		systemRoot *string
		arg        string
	}{
		{`C:\Windows\System32\bash.exe`, nil, "-s"},
		{`D:\WinNT\System32\bash.exe`, stringPtr(`D:\WinNT`), "-s"},
		{`D:\WinNT\Sysnative\bash.exe`, stringPtr(`D:\WinNT`), "-s"},
		{`D:\WinNT\System32\bash.exe`, stringPtr(""), "-c"},
		{`E:\Windows\System32\bash.exe`, stringPtr(`D:\WinNT`), "-c"},
	} {
		t.Run(c.shellPath, func(t *testing.T) {
			root := tempDir(t)
			// On POSIX the backslashes are part of one file name.
			noErr(t, os.WriteFile(filepath.Join(root, c.shellPath), []byte("#!/bin/sh\nprintf 'args:%s\\n' \"$*\" >&2\nexec /bin/bash \"$@\"\n"), 0o755))
			t.Chdir(root)
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("SystemRoot", "")
			if c.systemRoot == nil {
				noErr(t, os.Unsetenv("SystemRoot"))
			} else {
				t.Setenv("SystemRoot", *c.systemRoot)
			}
			result, output, err := collect(env.NewLocalWith(root, env.LocalOptions{ShellPath: c.shellPath}), ctx, `name='World'; echo "Hello, ${name}!"`, nil)
			noErr(t, err)
			equal(t, result.ExitCode, 0, "exit code")
			if !strings.Contains(output, "Hello, World!") || !strings.Contains(output, "args:"+c.arg) {
				t.Fatalf("output %q, want args:%s", output, c.arg)
			}
		})
	}
}

func TestSettlesWhenADescendantKeepsTheOutputOpen(t *testing.T) {
	requireBash(t)
	root := tempDir(t)
	e := env.NewLocal(root)
	timeout, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, output, err := collect(e, timeout, `sleep 30 & echo $! > child.pid; echo child-exiting`, nil)
	defer func() { _, _ = e.Exec(ctx, env.Script(`kill $(cat child.pid) 2>/dev/null`), nil) }()
	noErr(t, err)
	if !strings.Contains(output, "child-exiting") {
		t.Fatalf("output %q", output)
	}
}

func TestCleanupTerminatesActiveCommands(t *testing.T) {
	requireBash(t)
	e := env.NewLocal(tempDir(t))
	done := make(chan error, 1)
	go func() {
		_, err := e.Exec(ctx, env.Script("touch started; sleep 60"), nil)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !must(e.Exists(ctx, "started")) {
		if time.Now().After(deadline) {
			t.Fatal("the command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.Cleanup(ctx)
	select {
	case err := <-done:
		noErr(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not end the command")
	}
}

func TestStreamsStdoutAndStderr(t *testing.T) {
	requireBash(t)
	result, output, err := collect(env.NewLocal(tempDir(t)), ctx, "printf out; printf err >&2", nil)
	noErr(t, err)
	equal(t, result, env.ExecResult{}, "result")
	if !strings.Contains(output, "out") || !strings.Contains(output, "err") {
		t.Fatalf("output %q", output)
	}
}

func TestDecodesUTF8SplitAcrossProcessChunks(t *testing.T) {
	requireBash(t)
	_, output, err := collect(env.NewLocal(tempDir(t)), ctx, `printf '\360\237'; sleep 0.05; printf '\230\200'`, nil)
	noErr(t, err)
	equal(t, output, "😀", "output")
}

func TestReportsAMissingWorkingDirectoryBeforeSpawning(t *testing.T) {
	_, err := env.NewLocal(filepath.Join(tempDir(t), "missing")).Exec(ctx, env.Script("printf ok"), nil)
	ee := execCode(t, err, env.ExecSpawnError, "missing directory")
	if !strings.Contains(ee.Message, "Working directory does not exist") {
		t.Fatalf("message %q", ee.Message)
	}
}

func TestReturnsNonZeroExitCodesAsResults(t *testing.T) {
	requireBash(t)
	result := must(env.NewLocal(tempDir(t)).Exec(ctx, env.Script("exit 7"), nil))
	equal(t, result, env.ExecResult{ExitCode: 7}, "result")
}

func TestMapsSignalKilledProcessesToANonZeroExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows processes end with exit codes, not signals")
	}
	result := must(env.NewLocal(tempDir(t)).Exec(ctx, env.Script("kill -9 $$"), nil))
	equal(t, result.ExitCode, 128+9, "exit code")
}

func TestReturnsTimeoutsAndRejectsInvalidOnes(t *testing.T) {
	requireBash(t)
	e := env.NewLocal(tempDir(t))
	start := time.Now()
	_, err := e.Exec(ctx, env.Script("sleep 5"), &env.ExecOptions{Timeout: 10 * time.Millisecond})
	execCode(t, err, env.ExecTimeout, "timeout")
	// The timeout kills the command rather than waiting for it.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the timed out command ran for %s", elapsed)
	}
	for _, timeout := range []time.Duration{-time.Second, 2_147_484 * time.Second} {
		_, err := e.Exec(ctx, env.Script("touch spawned"), &env.ExecOptions{Timeout: timeout})
		if ee := execCode(t, err, env.ExecTimeout, fmt.Sprint("timeout ", timeout)); !strings.Contains(ee.Message, "Invalid timeout") {
			t.Fatalf("message %q", ee.Message)
		}
	}
	equal(t, must(e.Exists(ctx, "spawned")), false, "spawned")
}

func TestReturnsCallbackErrors(t *testing.T) {
	requireBash(t)
	_, err := env.NewLocal(tempDir(t)).Exec(ctx, env.Script("printf out"), &env.ExecOptions{
		OnOutput: func(context.Context, string, env.OutputInfo) error { return errors.New("callback failed") },
	})
	ee := execCode(t, err, env.ExecCallbackError, "callback")
	equal(t, ee.Message, "callback failed", "message")
}

func TestReturnsShellUnavailableAndSpawnErrors(t *testing.T) {
	root := tempDir(t)
	_, err := env.NewLocalWith(root, env.LocalOptions{ShellPath: filepath.Join(root, "missing-shell")}).Exec(ctx, env.Script("printf ok"), nil)
	execCode(t, err, env.ExecShellUnavailable, "missing shell")
	shellPath := filepath.Join(root, "not-executable-shell")
	noErr(t, os.WriteFile(shellPath, []byte("not executable"), 0o644))
	_, err = env.NewLocalWith(root, env.LocalOptions{ShellPath: shellPath}).Exec(ctx, env.Script("printf ok"), nil)
	execCode(t, err, env.ExecSpawnError, "not executable shell")
}

func TestReturnsAbortedForCancelledCommands(t *testing.T) {
	requireBash(t)
	e := env.NewLocal(tempDir(t))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := e.Exec(cancelled, env.Script("touch spawned"), nil)
	execCode(t, err, env.ExecAborted, "cancelled before")
	equal(t, must(e.Exists(ctx, "spawned")), false, "spawned")
	running, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = e.Exec(running, env.Script("sleep 5"), nil)
	execCode(t, err, env.ExecAborted, "cancelled while running")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the cancelled command ran for %s", elapsed)
	}
}

// TestWatchComparesRecentSmallFilesByContent: a second write within the
// file system's timestamp granularity can keep size and modification time;
// here the time is set back to make that certain.
func TestWatchComparesRecentSmallFilesByContent(t *testing.T) {
	root := tempDir(t)
	e := env.NewLocalWith(root, env.LocalOptions{Watch: env.WatchOptions{PollInterval: 50 * time.Millisecond}})
	path := filepath.Join(root, "AGENTS.md")
	noErr(t, os.WriteFile(path, []byte("aaaa"), 0o644))
	modified := must(os.Stat(path)).ModTime()
	var mu sync.Mutex
	var reported bool
	watcher := must(e.Watch(ctx, []env.WatchTarget{{Path: "AGENTS.md"}}, func(c env.WatchChange) {
		mu.Lock()
		defer mu.Unlock()
		reported = reported || len(c.Paths) > 0
	}))
	defer watcher.Close(ctx)
	noErr(t, os.WriteFile(path, []byte("bbbb"), 0o644))
	noErr(t, os.Chtimes(path, modified, modified))
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := reported
		mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a same-size rewrite with the same modification time was not reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDoesNotSpillBelowTheThresholds(t *testing.T) {
	requireBash(t)
	result := must(env.NewLocal(tempDir(t)).Exec(ctx, env.Script("printf short"), &env.ExecOptions{Spill: &env.SpillOptions{AfterBytes: 100, AfterLines: 10}}))
	equal(t, result.SpillPath, "", "spill")
}

func spillCleanup(t *testing.T, path string) {
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(path)) })
}

func TestSpillsExactRawBytesWhileStreamingDecodedText(t *testing.T) {
	requireBash(t)
	e := env.NewLocal(tempDir(t))
	result := must(e.Exec(ctx, env.Script(`printf 'f\200\000o'`), &env.ExecOptions{Spill: &env.SpillOptions{AfterBytes: 1, AfterLines: 10}}))
	if result.SpillPath == "" {
		t.Fatal("no spill")
	}
	spillCleanup(t, result.SpillPath)
	equal(t, string(must(e.ReadBinaryFile(ctx, result.SpillPath))), "f\x80\x00o", "spill")
}

func TestReportsTheSpillOfATimedOutCommand(t *testing.T) {
	requireBash(t)
	e := env.NewLocal(tempDir(t))
	_, err := e.Exec(ctx, env.Script("printf 12345678901234567890; sleep 5"), &env.ExecOptions{Timeout: 300 * time.Millisecond, Spill: &env.SpillOptions{AfterBytes: 10, AfterLines: 10}})
	ee := execCode(t, err, env.ExecTimeout, "timeout")
	if ee.SpillPath == "" {
		t.Fatal("no spill")
	}
	spillCleanup(t, ee.SpillPath)
	equal(t, must(e.ReadTextFile(ctx, ee.SpillPath)), "12345678901234567890", "spill")
}

func TestFailsRatherThanLosingARequestedSpill(t *testing.T) {
	requireBash(t)
	root := tempDir(t)
	e := env.NewLocal(root)
	env.SetSpillFile(e, func(context.Context) (string, error) { return filepath.Join(root, "missing", "spill.log"), nil })
	_, err := e.Exec(ctx, env.Script("printf 12345678901234567890"), &env.ExecOptions{Spill: &env.SpillOptions{AfterBytes: 10, AfterLines: 10}})
	ee := execCode(t, err, env.ExecUnknown, "failed spill")
	if !strings.Contains(ee.Message, "Failed to preserve complete shell output") {
		t.Fatalf("message %q", ee.Message)
	}
}

func TestSpillsCompleteLargeOutput(t *testing.T) {
	requireBash(t)
	e := env.NewLocal(tempDir(t))
	const size = 500_000
	result := must(e.Exec(ctx, env.Script(fmt.Sprintf(`head -c %d /dev/zero | tr '\0' x`, size)), &env.ExecOptions{Spill: &env.SpillOptions{AfterBytes: 10, AfterLines: 10}}))
	if result.SpillPath == "" {
		t.Fatal("no spill")
	}
	spillCleanup(t, result.SpillPath)
	equal(t, len(must(e.ReadTextFile(ctx, result.SpillPath))), size, "spill size")
}

func TestStreamsEveryLineAndSpillsThemAll(t *testing.T) {
	requireBash(t)
	e := env.NewLocal(tempDir(t))
	result, output, err := collect(e, ctx, "i=1; while [ $i -le 15000 ]; do echo line-$i; i=$((i+1)); done",
		&env.ExecOptions{Spill: &env.SpillOptions{AfterBytes: 1024 * 1024, AfterLines: 100}})
	noErr(t, err)
	var want strings.Builder
	for i := 1; i <= 15000; i++ {
		fmt.Fprintf(&want, "line-%d\n", i)
	}
	equal(t, output, want.String(), "output")
	if result.SpillPath == "" {
		t.Fatal("no spill")
	}
	spillCleanup(t, result.SpillPath)
	spilled := must(e.ReadTextLines(ctx, result.SpillPath, env.AllLines))
	equal(t, len(spilled), 15000, "spilled lines")
	equal(t, spilled[len(spilled)-1], "line-15000", "last spilled line")
}

func TestWatchRefusesATreeOverTheBudgetAndStopsWhenOneGrowsPastIt(t *testing.T) {
	root := tempDir(t)
	e := env.NewLocalWith(root, env.LocalOptions{Watch: env.WatchOptions{MaxDirectories: 3, PollInterval: 50 * time.Millisecond}})
	noErr(t, e.CreateDir(ctx, "tree/a/b", true))
	noErr(t, e.CreateDir(ctx, "tree/c", true))
	_, err := e.Watch(ctx, []env.WatchTarget{{Path: "tree", Recursive: true}}, func(env.WatchChange) {})
	code(t, err, env.CodeInvalid, "tree over the budget")

	noErr(t, e.Remove(ctx, "tree/c", env.RemoveOptions{Recursive: true}))
	var mu sync.Mutex
	var changes []env.WatchChange
	watcher := must(e.Watch(ctx, []env.WatchTarget{{Path: "tree", Recursive: true}}, func(c env.WatchChange) {
		mu.Lock()
		defer mu.Unlock()
		changes = append(changes, c)
	}))
	defer watcher.Close(ctx)
	noErr(t, e.CreateDir(ctx, "tree/d", true))
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		var last *env.FileError
		if n := len(changes); n > 0 {
			last = changes[n-1].Err
		}
		mu.Unlock()
		if last != nil {
			equal(t, last.Code, env.CodeInvalid, "error code")
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the watcher did not stop with an error")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
