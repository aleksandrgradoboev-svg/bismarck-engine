package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
)

// Ported from pi packages/durable/test/tools.test.ts: the edit, powershell
// and bash cases, with the mutation queue cases that go through edit.

func runWithAPI(ctx context.Context, tool *harness.ToolRegistration, args map[string]any, e env.ExecutionEnv) (harness.ToolExecutionResult, *fakeAPI, error) {
	api := &fakeAPI{env: e}
	result, err := tool.Execute(ctx, args, api)
	return result, api, err
}

func edits(pairs ...string) []any {
	var list []any
	for i := 0; i < len(pairs); i += 2 {
		list = append(list, map[string]any{"oldText": pairs[i], "newText": pairs[i+1]})
	}
	return list
}

func TestEditAppliesDisjointEditsAndReturnsBothDiffFormats(t *testing.T) {
	e := newEnv(t)
	original := "alpha\nbeta\ngamma\ndelta\n"
	mustWrite(t, e, "edit.txt", original)
	result, err := run(context.Background(), NewEditTool(), map[string]any{"path": "edit.txt", "edits": edits("alpha\n", "ALPHA\n", "gamma\n", "GAMMA\n")}, e)
	if err != nil {
		t.Fatal(err)
	}
	details := result.Details.(*EditToolDetails)
	if got := textOutput(result); got != "Successfully replaced 2 block(s) in edit.txt." {
		t.Fatalf("output = %q", got)
	}
	if !strings.Contains(details.Diff, "ALPHA") || !strings.Contains(details.Diff, "GAMMA") {
		t.Fatalf("diff = %q", details.Diff)
	}
	// pi applies the patch with jsdiff; here it is jsdiff's own patch for
	// these texts (internal/diff/testdata, case "pi edit", named edit.txt).
	wantPatch := "--- edit.txt\n+++ edit.txt\n@@ -1,4 +1,4 @@\n-alpha\n+ALPHA\n beta\n-gamma\n+GAMMA\n delta\n"
	if details.Patch != wantPatch {
		t.Fatalf("patch = %q", details.Patch)
	}
	if got := mustRead(t, e, "edit.txt"); got != "ALPHA\nbeta\nGAMMA\ndelta\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditRepairsEditsSentAsAJSONStringASingleObjectOrTopLevelOldTextNewTextWithoutMutatingThem(t *testing.T) {
	prepare := NewEditTool().PrepareArguments
	edit := map[string]any{"oldText": "a", "newText": "b"}
	encoded := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	check := func(input, want map[string]any) {
		t.Helper()
		got, err := prepare(input)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("prepare(%v) = %v, want %v", input, got, want)
		}
	}
	asString := map[string]any{"path": "f", "edits": encoded([]any{edit})}
	check(asString, map[string]any{"path": "f", "edits": []any{edit}})
	if asString["edits"] != encoded([]any{edit}) {
		t.Fatal("arguments mutated")
	}
	check(map[string]any{"path": "f", "edits": encoded(edit)}, map[string]any{"path": "f", "edits": []any{edit}})
	check(map[string]any{"path": "f", "edits": edit}, map[string]any{"path": "f", "edits": []any{edit}})
	check(map[string]any{"path": "f", "edits": []any{edit}, "oldText": "c", "newText": "d"},
		map[string]any{"path": "f", "edits": []any{edit, map[string]any{"oldText": "c", "newText": "d"}}})
	check(map[string]any{"path": "f", "edits": "not json"}, map[string]any{"path": "f", "edits": "not json"})
}

func TestEditMatchesAllEditsAgainstTheOriginalAndRejectsOverlaps(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "edit.txt", "one\ntwo\nthree\n")
	_, err := run(context.Background(), NewEditTool(), map[string]any{"path": "edit.txt", "edits": edits("one\ntwo\n", "ONE\nTWO\n", "two\nthree\n", "TWO\nTHREE\n")}, e)
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("err = %v", err)
	}
	if got := mustRead(t, e, "edit.txt"); got != "one\ntwo\nthree\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditRejectsMissingAndDuplicateTargetText(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "edit.txt", "foo foo foo")
	tool := NewEditTool()
	if _, err := run(context.Background(), tool, map[string]any{"path": "edit.txt", "edits": edits("bar", "baz")}, e); err == nil || !strings.Contains(err.Error(), "Could not find the exact text") {
		t.Fatalf("missing: err = %v", err)
	}
	if _, err := run(context.Background(), tool, map[string]any{"path": "edit.txt", "edits": edits("foo", "bar")}, e); err == nil || !strings.Contains(err.Error(), "Found 3 occurrences") {
		t.Fatalf("duplicate: err = %v", err)
	}
}

// blockingEditEnv holds the first edit's write until finishFirstEditWrite
// closes, then writes it whatever the call's cancellation.
type blockingEditEnv struct {
	*env.Local
	firstEditWriteStarted  chan struct{}
	finishFirstEditWrite   chan struct{}
	firstEditWriteSettled  atomic.Bool
	secondEditWriteStarted atomic.Bool
}

func (e *blockingEditEnv) WriteFile(ctx context.Context, path string, content []byte) error {
	switch string(content) {
	case "ALPHA\nbeta\n":
		close(e.firstEditWriteStarted)
		<-e.finishFirstEditWrite
		err := e.Local.WriteFile(context.Background(), path, content)
		e.firstEditWriteSettled.Store(true)
		return err
	case "ALPHA\nBETA\n", "alpha\nBETA\n":
		e.secondEditWriteStarted.Store(true)
	}
	return e.Local.WriteFile(ctx, path, content)
}

func TestEditKeepsTheMutationQueueLockedUntilAnAbortedEditWriteSettles(t *testing.T) {
	e := &blockingEditEnv{Local: newEnv(t), firstEditWriteStarted: make(chan struct{}), finishFirstEditWrite: make(chan struct{})}
	mustWrite(t, e.Local, "file.txt", "alpha\nbeta\n")
	tool := NewEditTool()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := runAsync(ctx, tool, map[string]any{"path": "file.txt", "edits": edits("alpha", "ALPHA")}, e)
	<-e.firstEditWriteStarted
	cancel()
	second := runAsync(context.Background(), tool, map[string]any{"path": "file.txt", "edits": edits("beta", "BETA")}, e)
	time.Sleep(20 * time.Millisecond)
	if e.secondEditWriteStarted.Load() {
		t.Fatal("second edit wrote while the first was held")
	}
	close(e.finishFirstEditWrite)
	if o := settle(t, first); o.err == nil || o.err.Error() != "Operation aborted" {
		t.Fatalf("first: err = %v", o.err)
	}
	if o := settle(t, second); o.err != nil {
		t.Fatal(o.err)
	}
	if !e.firstEditWriteSettled.Load() {
		t.Fatal("first write did not settle")
	}
	if got := mustRead(t, e.Local, "file.txt"); got != "ALPHA\nBETA\n" {
		t.Fatalf("file = %q", got)
	}
}

// slowReadEnv delays every text read, so concurrent edits overlap unless the
// queue serializes them.
type slowReadEnv struct {
	*env.Local
}

func (e *slowReadEnv) ReadTextFile(ctx context.Context, path string) (string, error) {
	time.Sleep(20 * time.Millisecond)
	return e.Local.ReadTextFile(ctx, path)
}

func runConcurrently(t *testing.T, calls ...func() error) {
	t.Helper()
	errs := make(chan error, len(calls))
	for _, call := range calls {
		go func() { errs <- call() }()
	}
	for range calls {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestEditSerializesConcurrentEditsThroughCanonicalAndSymlinkPaths(t *testing.T) {
	e := &slowReadEnv{Local: newEnv(t)}
	mustWrite(t, e, "target.txt", "alpha\nbeta\ngamma\n")
	symlinkOrSkip(t, "target.txt", filepath.Join(e.Cwd(), "link.txt"))
	tool := NewEditTool()
	runConcurrently(t,
		func() error {
			_, err := run(context.Background(), tool, map[string]any{"path": "target.txt", "edits": edits("alpha", "ALPHA")}, e)
			return err
		},
		func() error {
			_, err := run(context.Background(), tool, map[string]any{"path": "link.txt", "edits": edits("beta", "BETA")}, e)
			return err
		})
	if got := mustRead(t, e, "target.txt"); got != "ALPHA\nBETA\ngamma\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditSerializesEditsOfOneFileAcrossEnvironmentObjectsOfOneFileSystem(t *testing.T) {
	dir := t.TempDir()
	first, second := &slowReadEnv{Local: env.NewLocal(dir)}, &slowReadEnv{Local: env.NewLocal(dir)}
	mustWrite(t, first, "file.txt", "alpha\nbeta\n")
	tool := NewEditTool()
	runConcurrently(t,
		func() error {
			_, err := run(context.Background(), tool, map[string]any{"path": "file.txt", "edits": edits("alpha", "ALPHA")}, first)
			return err
		},
		func() error {
			_, err := run(context.Background(), tool, map[string]any{"path": "file.txt", "edits": edits("beta", "BETA")}, second)
			return err
		})
	if got := mustRead(t, first, "file.txt"); got != "ALPHA\nBETA\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditEditsRegularFilesThroughSymlinks(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "target.txt", "before\n")
	symlinkOrSkip(t, "target.txt", filepath.Join(e.Cwd(), "link.txt"))
	if _, err := run(context.Background(), NewEditTool(), map[string]any{"path": "link.txt", "edits": edits("before", "after")}, e); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, e, "target.txt"); got != "after\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditPreservesBOMAndCRLFLineEndings(t *testing.T) {
	e := newEnv(t)
	const bom = "\xef\xbb\xbf"
	mustWrite(t, e, "edit.txt", bom+"one\r\ntwo\r\n")
	if _, err := run(context.Background(), NewEditTool(), map[string]any{"path": "edit.txt", "edits": edits("two", "TWO")}, e); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(e.Cwd(), "edit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != bom+"one\r\nTWO\r\n" {
		t.Fatalf("file = %q", b)
	}
}

// programsEnv is an environment where only the installed programs exist; it
// records each command and prints out.
type programsEnv struct {
	*env.Local
	installed []string
	out       string
	exitCode  int
	commands  []env.Command
}

func (e *programsEnv) Exec(ctx context.Context, command env.Command, options *env.ExecOptions) (env.ExecResult, error) {
	e.commands = append(e.commands, command)
	if command.Argv == nil || !contains(e.installed, command.Argv[0]) {
		name := ""
		if command.Argv != nil {
			name = command.Argv[0]
		}
		return env.ExecResult{}, &env.ExecutionError{Code: env.ExecSpawnError, Message: "spawn " + name + " ENOENT"}
	}
	if err := options.OnOutput(ctx, e.out, env.OutputInfo{Stream: env.Stdout}); err != nil {
		return env.ExecResult{}, err
	}
	return env.ExecResult{ExitCode: e.exitCode}, nil
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func TestPowerShellRunsTheCommandWithPwshAsOneArgumentForcingUTF8Output(t *testing.T) {
	e := &programsEnv{Local: newEnv(t), installed: []string{"pwsh"}, out: "héllo\n"}
	_, api, err := runWithAPI(context.Background(), NewPowerShellTool(PowerShellToolOptions{}), map[string]any{"command": "Write-Output 'héllo'"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(api.output, ""); got != "héllo\n" {
		t.Fatalf("streamed = %q", got)
	}
	want := []env.Command{env.Argv("pwsh", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		"try { [Console]::OutputEncoding=[System.Text.Encoding]::UTF8 } catch {}\nWrite-Output 'héllo'")}
	if !reflect.DeepEqual(e.commands, want) {
		t.Fatalf("commands = %#v", e.commands)
	}
}

func TestPowerShellFallsBackToWindowsPowerShellAndReportsTheLastStartFailure(t *testing.T) {
	windowsOnly := &programsEnv{Local: newEnv(t), installed: []string{"powershell"}, out: "ok"}
	_, api, err := runWithAPI(context.Background(), NewPowerShellTool(PowerShellToolOptions{CommandPrefix: "$x = 1"}), map[string]any{"command": "$x"}, windowsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(api.output, ""); got != "ok" {
		t.Fatalf("streamed = %q", got)
	}
	if len(windowsOnly.commands) != 2 || windowsOnly.commands[0].Argv[0] != "pwsh" || windowsOnly.commands[1].Argv[0] != "powershell" {
		t.Fatalf("commands = %#v", windowsOnly.commands)
	}
	last := windowsOnly.commands[1].Argv[len(windowsOnly.commands[1].Argv)-1]
	if !regexp.MustCompile(`\n\$x = 1\n\$x$`).MatchString(last) {
		t.Fatalf("script = %q", last)
	}

	none := &programsEnv{Local: newEnv(t)}
	_, err = run(context.Background(), NewPowerShellTool(PowerShellToolOptions{}), map[string]any{"command": "1"}, none)
	if err == nil || err.Error() != "spawn powershell ENOENT" {
		t.Fatalf("err = %v", err)
	}
}

func TestPowerShellAnswersANonzeroExitWithAnErrorResultCarryingTheExitCode(t *testing.T) {
	e := &programsEnv{Local: newEnv(t), installed: []string{"pwsh"}, out: "partial", exitCode: 3}
	result, api, err := runWithAPI(context.Background(), NewPowerShellTool(PowerShellToolOptions{}), map[string]any{"command": "exit 3"}, e)
	if err != nil {
		t.Fatal(err)
	}
	wantDiagnostics := []harness.ToolDiagnostic{{Severity: "error", Code: "exit_code", Message: "Command exited with code 3"}}
	if !result.IsError || result.StructuredOutput != (ShellStructuredOutput{Output: "partial", ExitCode: 3}) || !reflect.DeepEqual(result.Diagnostics, wantDiagnostics) {
		t.Fatalf("result = %+v", result)
	}
	if got := strings.Join(api.output, ""); got != "partial" {
		t.Fatalf("streamed = %q", got)
	}
}

func TestPowerShellRunsRealPowerShellWithUTF8Output(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only, as in pi")
	}
	_, api, err := runWithAPI(context.Background(), NewPowerShellTool(PowerShellToolOptions{}), map[string]any{"command": "Write-Output ('h' + [char]0xe9 + 'llo'); exit 0"}, newEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(strings.Join(api.output, "")); got != "héllo" {
		t.Fatalf("streamed = %q", got)
	}
}

// skippingEnv reports output it omitted and records the window it got.
type skippingEnv struct {
	*env.Local
	skipped  env.OutputSkip
	received *env.OutputWindow
}

func (e *skippingEnv) Exec(ctx context.Context, _ env.Command, options *env.ExecOptions) (env.ExecResult, error) {
	e.received = options.Window
	if err := options.OnOutput(ctx, "tail\n", env.OutputInfo{Stream: env.Stdout, Skipped: &e.skipped}); err != nil {
		return env.ExecResult{}, err
	}
	return env.ExecResult{}, nil
}

// windowAPI keeps a tail window and records each chunk with its skip.
type windowAPI struct {
	*fakeAPI
	window *env.OutputWindow
	calls  []windowCall
}

type windowCall struct {
	chunk string
	skip  *output.Skip
}

func (a *windowAPI) OutputWindow() *env.OutputWindow { return a.window }
func (a *windowAPI) Output(chunk []byte, skip *output.Skip) error {
	a.calls = append(a.calls, windowCall{string(chunk), skip})
	return nil
}

func TestBashPassesTheRetainedWindowToTheEnvironmentAndForwardsWhatItSkipped(t *testing.T) {
	window := &env.OutputWindow{MaxBytes: 4, MaxLines: 1, MinInterval: 100 * time.Millisecond, BytesPerSecond: 1024}
	e := &skippingEnv{Local: newEnv(t), skipped: env.OutputSkip{Bytes: 6, Newlines: 2, EndsWithNewline: true}}
	api := &windowAPI{fakeAPI: &fakeAPI{env: e}, window: window}
	if _, err := NewBashTool(BashToolOptions{}).Execute(context.Background(), map[string]any{"command": "anything"}, api); err != nil {
		t.Fatal(err)
	}
	if e.received != window {
		t.Fatalf("window = %+v", e.received)
	}
	want := []windowCall{{"tail\n", &output.Skip{Bytes: 6, Newlines: 2, EndsWithNewline: true}}}
	if !reflect.DeepEqual(api.calls, want) {
		t.Fatalf("calls = %+v", api.calls)
	}
}

func TestBashStreamsCombinedStdoutAndStderrReturnsNoOutputOfItsOwnAndRepeatsItWithTheExitCode(t *testing.T) {
	result, api, err := runWithAPI(context.Background(), NewBashTool(BashToolOptions{}), map[string]any{"command": "printf out; printf err >&2"}, newEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	streamed := strings.Join(api.output, "")
	if !strings.Contains(streamed, "out") || !strings.Contains(streamed, "err") {
		t.Fatalf("streamed = %q", streamed)
	}
	if result.Output != nil {
		t.Fatalf("output = %#v", result.Output)
	}
	if result.StructuredOutput != (ShellStructuredOutput{Output: streamed, ExitCode: 0}) {
		t.Fatalf("structured = %+v", result.StructuredOutput)
	}
}

func TestBashAnswersANonzeroExitWithAnErrorResultAndFailsOnATimeoutAfterStreamingTheOutput(t *testing.T) {
	e := newEnv(t)
	tool := NewBashTool(BashToolOptions{})
	failed, api, err := runWithAPI(context.Background(), tool, map[string]any{"command": "printf failed; exit 7"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if !failed.IsError || failed.StructuredOutput != (ShellStructuredOutput{Output: "failed", ExitCode: 7}) ||
		len(failed.Diagnostics) != 1 || failed.Diagnostics[0].Code != "exit_code" || failed.Diagnostics[0].Message != "Command exited with code 7" {
		t.Fatalf("result = %+v", failed)
	}
	if got := strings.Join(api.output, ""); got != "failed" {
		t.Fatalf("streamed = %q", got)
	}
	if _, err := run(context.Background(), tool, map[string]any{"command": "sleep 2", "timeout": 0.01}, e); err == nil || err.Error() != "Command timed out after 0.01 seconds" {
		t.Fatalf("timeout: err = %v", err)
	}
}

const truncatedOutputLines = DefaultMaxLines + 1

// timeoutOutputEnv emits output past the line limit, spills it, then times
// out.
type timeoutOutputEnv struct {
	*env.Local
}

func (e *timeoutOutputEnv) Exec(ctx context.Context, _ env.Command, options *env.ExecOptions) (env.ExecResult, error) {
	var b strings.Builder
	for i := 1; i <= truncatedOutputLines; i++ {
		b.WriteString("line-" + itoa(i) + "\n")
	}
	spillPath, err := e.CreateTempFile(ctx, env.TempFileOptions{Prefix: "timeout-", Suffix: ".log"})
	if err != nil {
		return env.ExecResult{}, err
	}
	if err := e.WriteFile(ctx, spillPath, []byte(b.String())); err != nil {
		return env.ExecResult{}, err
	}
	if err := options.OnOutput(ctx, b.String(), env.OutputInfo{Stream: env.Stdout}); err != nil {
		return env.ExecResult{}, err
	}
	return env.ExecResult{}, &env.ExecutionError{Code: env.ExecTimeout, Message: "timeout:" + options.Timeout.String(), SpillPath: spillPath}
}

func itoa(i int) string { return jsNumber(float64(i)) }

var fullOutputPattern = regexp.MustCompile(`^Full output: (.+)$`)

func TestBashReportsTheSpillOfACommandThatTimesOut(t *testing.T) {
	e := &timeoutOutputEnv{Local: newEnv(t)}
	_, api, err := runWithAPI(context.Background(), NewBashTool(BashToolOptions{}), map[string]any{"command": "emit-output-then-time-out", "timeout": 0.05}, e)
	if err == nil || err.Error() != "Command timed out after 0.05 seconds" {
		t.Fatalf("err = %v", err)
	}
	if len(api.diagnostics) == 0 {
		t.Fatal("no spill diagnostic")
	}
	m := fullOutputPattern.FindStringSubmatch(api.diagnostics[0].Message)
	if m == nil {
		t.Fatalf("diagnostic = %q", api.diagnostics[0].Message)
	}
	full := mustRead(t, e, m[1])
	if !strings.Contains(full, "line-1\nline-2") || !strings.Contains(full, "line-"+itoa(DefaultMaxLines)+"\nline-"+itoa(truncatedOutputLines)) {
		t.Fatalf("full output has %d bytes", len(full))
	}
}

func TestBashPreparesCommandCwdAndAnExplicitEnvironmentWithTheCallsAPI(t *testing.T) {
	e := env.NewLocalWith(t.TempDir(), env.LocalOptions{ShellEnv: map[string]string{"PI_BASH_PREPARE_INHERITED": "inherited"}})
	if err := e.CreateDir(context.Background(), "workspace", false); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(e.Cwd(), "workspace")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var receivedEnv env.ExecutionEnv
	var receivedCtx context.Context
	tool := NewBashTool(BashToolOptions{
		CommandPrefix: "prefix=ready",
		Prepare: func(callCtx context.Context, execution *BashExecution, api harness.ToolExecutionAPI) error {
			receivedEnv, receivedCtx = api.Env(), callCtx
			execution.Cwd = workspace
			execution.Env = map[string]string{"PI_BASH_PREPARE_EXPLICIT": "explicit"}
			execution.InheritEnv = false
			execution.Command += "\n: > prepared-cwd\nprintf '%s:%s:%s' \"$prefix\" \"${PI_BASH_PREPARE_INHERITED-}\" \"$PI_BASH_PREPARE_EXPLICIT\""
			// Git Bash on Windows reports $PWD as an MSYS path, so only POSIX
			// compares it.
			if runtime.GOOS != "windows" {
				execution.Command += "\nprintf ':%s' \"$PWD\""
			}
			return nil
		},
	})
	_, api, err := runWithAPI(ctx, tool, map[string]any{"command": ":"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if receivedEnv != env.ExecutionEnv(e) || receivedCtx != ctx {
		t.Fatal("prepare got another environment or context")
	}
	pwd := ""
	if runtime.GOOS != "windows" {
		canonical, err := e.CanonicalPath(context.Background(), workspace)
		if err != nil {
			t.Fatal(err)
		}
		pwd = ":" + canonical
	}
	if got := strings.Join(api.output, ""); got != "ready::explicit"+pwd {
		t.Fatalf("streamed = %q", got)
	}
	if exists, err := e.Exists(context.Background(), filepath.Join(workspace, "prepared-cwd")); err != nil || !exists {
		t.Fatalf("prepared-cwd exists = %v, err = %v", exists, err)
	}
}

func TestBashSupportsCommandPrefixes(t *testing.T) {
	_, api, err := runWithAPI(context.Background(), NewBashTool(BashToolOptions{CommandPrefix: "value=hello"}), map[string]any{"command": "printf $value"}, newEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(api.output, ""); got != "hello" {
		t.Fatalf("streamed = %q", got)
	}
}

func TestBashStreamsEveryByteAndSpillsCompleteOutputBeyondTheDefaultLimits(t *testing.T) {
	e := newEnv(t)
	_, api, err := runWithAPI(context.Background(), NewBashTool(BashToolOptions{}), map[string]any{"command": "i=1; while [ $i -le 3000 ]; do echo line-$i; i=$((i + 1)); done"}, e)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 1; i <= 3000; i++ {
		b.WriteString("line-" + itoa(i) + "\n")
	}
	if got := strings.Join(api.output, ""); got != b.String() {
		t.Fatalf("streamed %d bytes, want %d", len(got), b.Len())
	}
	if len(api.diagnostics) == 0 {
		t.Fatal("no spill diagnostic")
	}
	m := fullOutputPattern.FindStringSubmatch(api.diagnostics[0].Message)
	if m == nil {
		t.Fatalf("diagnostic = %q", api.diagnostics[0].Message)
	}
	if got := mustRead(t, e, m[1]); got != b.String() {
		t.Fatalf("spill has %d bytes", len(got))
	}
}

func TestBashDoesNotSpillOutputWithinTheLimits(t *testing.T) {
	_, api, err := runWithAPI(context.Background(), NewBashTool(BashToolOptions{}), map[string]any{"command": "printf small"}, newEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(api.diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v", api.diagnostics)
	}
}
