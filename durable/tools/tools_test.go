package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/imagemime"
)

// Ported from pi packages/durable/test/tools.test.ts: the cases of read,
// write and the mutation queue through write. The edit, bash and powershell
// cases, and the queue cases that go through edit, come with those tools.

// fakeAPI is a minimal execution API: the environment, collected output and
// diagnostics, and nothing durable. Any other method panics through the nil
// embedded interface.
type fakeAPI struct {
	harness.ToolExecutionAPI
	env         env.ExecutionEnv
	output      []string
	diagnostics []harness.ToolDiagnostic
}

func (a *fakeAPI) Env() env.ExecutionEnv { return a.env }
func (a *fakeAPI) Output(chunk []byte, _ *output.Skip) error {
	a.output = append(a.output, string(chunk))
	return nil
}
func (a *fakeAPI) OutputWindow() *env.OutputWindow { return nil }
func (a *fakeAPI) RetainedOutput() (string, bool, error) {
	return strings.Join(a.output, ""), false, nil
}
func (a *fakeAPI) Diagnostic(d harness.ToolDiagnostic) error {
	a.diagnostics = append(a.diagnostics, d)
	return nil
}
func (a *fakeAPI) Details(context.Context, any) error { return nil }

// Agent has no model: read treats it as one that sees images.
func (a *fakeAPI) Agent(context.Context) (*harness.Agent, error) { return &harness.Agent{}, nil }
func (a *fakeAPI) Models() ai.Models                             { return nil }

// run executes tool through a fakeAPI; a nil e is no environment.
func run(ctx context.Context, tool *harness.ToolRegistration, args map[string]any, e env.ExecutionEnv) (harness.ToolExecutionResult, error) {
	return tool.Execute(ctx, args, &fakeAPI{env: e})
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newEnv(t *testing.T) *env.Local { return env.NewLocal(t.TempDir()) }

func textOutput(result harness.ToolExecutionResult) string {
	var parts []string
	for _, part := range result.Output {
		if text, ok := part.(ai.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func diagnosticText(result harness.ToolExecutionResult) string {
	var parts []string
	for _, d := range result.Diagnostics {
		parts = append(parts, d.Message)
	}
	return strings.Join(parts, "\n")
}

func mustWrite(t *testing.T, e env.ExecutionEnv, path, content string) {
	t.Helper()
	if err := e.WriteFile(context.Background(), path, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, e env.ExecutionEnv, path string) string {
	t.Helper()
	text, err := e.ReadTextFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func numberedLines(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("Line %d", i+1)
	}
	return strings.Join(lines, "\n")
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestToolsFailWithAnOrdinaryErrorWhenNoEnvironmentIsConfigured(t *testing.T) {
	_, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "No execution environment") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadDetectsTheCompleteGIFSignatures(t *testing.T) {
	for _, signature := range []string{"GIF87a", "GIF89a"} {
		if got := imagemime.Detect([]byte(signature)); got != "image/gif" {
			t.Errorf("Detect(%q) = %q", signature, got)
		}
	}
}

func TestReadReadsTextWithOffsetsAndLimitsAndReportsContinuationAsADiagnostic(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "test.txt", numberedLines(100))
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "test.txt", "offset": 41.0, "limit": 20.0}, e)
	if err != nil {
		t.Fatal(err)
	}
	out := textOutput(result)
	if strings.Contains(out, "Line 40") || !strings.Contains(out, "Line 41") || !strings.Contains(out, "Line 60") ||
		strings.Contains(out, "Line 61") || strings.Contains(out, "more lines") {
		t.Fatalf("output = %q", out)
	}
	if got := diagnosticText(result); got != "40 more lines in file. Use offset=61 to continue." {
		t.Fatalf("diagnostics = %q", got)
	}
}

func TestReadTruncatesLargeTextByLineCount(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "large.txt", numberedLines(2500))
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "large.txt"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := diagnosticText(result); got != "Showing lines 1-2000 of 2500. Use offset=2001 to continue." {
		t.Fatalf("diagnostics = %q", got)
	}
	if result.Diagnostics[0].Code != "truncated" {
		t.Fatalf("code = %q", result.Diagnostics[0].Code)
	}
	tr := result.Details.(*ReadToolDetails).Truncation
	if !tr.Truncated || tr.TruncatedBy != TruncatedByLines || tr.TotalLines != 2500 || tr.OutputLines != 2000 {
		t.Fatalf("truncation = %+v", tr)
	}
}

func TestReadDoesNotCountATrailingNewlineAsAnExtraLineAtTheTruncationLimit(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "exact.txt", strings.Repeat("x\n", 2000))
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "exact.txt"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if result.Details != nil || len(result.Diagnostics) != 0 || result.Diagnostics == nil {
		t.Fatalf("details = %v, diagnostics = %#v", result.Details, result.Diagnostics)
	}
}

func TestReadShowsTheStartOfALineLongerThanTheByteLimit(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "long.txt", strings.Repeat("é", 40_000)+"\nnext\n")
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "long.txt"}, e)
	if err != nil {
		t.Fatal(err)
	}
	// Two-byte characters: the cut lands on a character boundary at or below
	// the limit.
	if got := textOutput(result); got != strings.Repeat("é", 25_600) {
		t.Fatalf("output has %d bytes", len(got))
	}
	want := "Line 1 is 78.1KB, exceeds the 50.0KB limit; showing its first 50.0KB. Use bash: sed -n '1p' long.txt | tail -c +51201"
	if got := diagnosticText(result); got != want {
		t.Fatalf("diagnostics = %q", got)
	}
	tr := result.Details.(*ReadToolDetails).Truncation
	if !tr.Truncated || !tr.FirstLineExceedsLimit || tr.OutputBytes != 51_200 || tr.OutputLines != 1 {
		t.Fatalf("truncation = %+v", tr)
	}
	if strings.Contains(mustJSON(t, result.Details), "content") {
		t.Fatal("details carry the content")
	}
}

func TestReadRejectsOffsetsBeyondTheFile(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "short.txt", "one\ntwo\nthree")
	_, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "short.txt", "offset": 100.0}, e)
	if err == nil || !strings.Contains(err.Error(), "Offset 100 is beyond end of file (3 lines total)") {
		t.Fatalf("err = %v", err)
	}
}

// growingEnv appends to a log at every read, as a busy writer would.
type growingEnv struct {
	*env.Local
	logPath string
}

type growingReader struct {
	env.BinaryReader
	logPath string
}

func (e *growingEnv) OpenBinaryReader(ctx context.Context, path string, options env.BinaryReaderOptions) (env.BinaryReader, error) {
	reader, err := e.Local.OpenBinaryReader(ctx, path, options)
	if err != nil {
		return nil, err
	}
	return &growingReader{BinaryReader: reader, logPath: e.logPath}, nil
}

func (r *growingReader) Read(ctx context.Context, offset, length int64) ([]byte, error) {
	f, err := os.OpenFile(r.logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString("more\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return r.BinaryReader.Read(ctx, offset, length)
}

func TestReadReadsALogThatGrowsWhileItIsRead(t *testing.T) {
	local := newEnv(t)
	mustWrite(t, local, "app.log", "one\ntwo\n")
	growing := &growingEnv{Local: local, logPath: filepath.Join(local.Cwd(), "app.log")}
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "app.log", "limit": 2.0}, growing)
	if err != nil {
		t.Fatal(err)
	}
	if got := textOutput(result); got != "one\ntwo" {
		t.Fatalf("output = %q", got)
	}
}

func TestReadReturnsImagesByContentAsOneImageBlock(t *testing.T) {
	e := newEnv(t)
	data := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAX+XDSwAAAABJRU5ErkJggg=="
	bytes, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.WriteFile(context.Background(), "image.txt", bytes); err != nil {
		t.Fatal(err)
	}
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "image.txt"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) != 1 || result.Output[0] != (ai.ImageContent{Data: data, MimeType: "image/png"}) {
		t.Fatalf("output = %#v", result.Output)
	}
	if result.IsError {
		t.Fatal("isError")
	}
	if got := diagnosticText(result); got != "Read image file [image/png]." {
		t.Fatalf("diagnostics = %q", got)
	}
}

func TestWriteWritesFilesAndCreatesParentDirectories(t *testing.T) {
	e := newEnv(t)
	result, err := run(context.Background(), NewWriteTool(), map[string]any{"path": "nested/dir/file.txt", "content": "hello"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := textOutput(result); got != "Successfully wrote to nested/dir/file.txt" {
		t.Fatalf("output = %q", got)
	}
	if got := mustRead(t, e, "nested/dir/file.txt"); got != "hello" {
		t.Fatalf("file = %q", got)
	}
}

// blockingWriteEnv holds the write of "first\n" until finishFirstWrite closes
// and records whether the write of "second\n" started.
type blockingWriteEnv struct {
	*env.Local
	id                 string
	firstWriteStarted  chan struct{}
	finishFirstWrite   chan struct{}
	secondWriteStarted atomic.Bool
}

func newBlockingWriteEnv(dir, id string) *blockingWriteEnv {
	return &blockingWriteEnv{Local: env.NewLocal(dir), id: id, firstWriteStarted: make(chan struct{}), finishFirstWrite: make(chan struct{})}
}

func (e *blockingWriteEnv) ID() string {
	if e.id != "" {
		return e.id
	}
	return e.Local.ID()
}

func (e *blockingWriteEnv) WriteFile(ctx context.Context, path string, content []byte) error {
	switch string(content) {
	case "first\n":
		close(e.firstWriteStarted)
		<-e.finishFirstWrite
	case "second\n":
		e.secondWriteStarted.Store(true)
	}
	return e.Local.WriteFile(ctx, path, content)
}

type outcome struct {
	result harness.ToolExecutionResult
	err    error
}

func runAsync(ctx context.Context, tool *harness.ToolRegistration, args map[string]any, e env.ExecutionEnv) <-chan outcome {
	done := make(chan outcome, 1)
	go func() {
		result, err := run(ctx, tool, args, e)
		done <- outcome{result, err}
	}()
	return done
}

// settle waits for a call, failing the test after a generous timeout rather
// than hanging it.
func settle(t *testing.T, done <-chan outcome) outcome {
	t.Helper()
	select {
	case o := <-done:
		return o
	case <-time.After(30 * time.Second):
		t.Fatal("call did not settle")
		return outcome{}
	}
}

func TestWriteKeepsTheMutationQueueLockedUntilAnAbortedWriteSettles(t *testing.T) {
	e := newBlockingWriteEnv(t.TempDir(), "")
	tool := NewWriteTool()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := runAsync(ctx, tool, map[string]any{"path": "file.txt", "content": "first\n"}, e)
	<-e.firstWriteStarted
	cancel()
	second := runAsync(context.Background(), tool, map[string]any{"path": "file.txt", "content": "second\n"}, e)
	time.Sleep(20 * time.Millisecond)
	if e.secondWriteStarted.Load() {
		t.Fatal("second write started while the first was held")
	}
	close(e.finishFirstWrite)
	if o := settle(t, first); o.err == nil {
		t.Fatal("aborted write succeeded")
	}
	if o := settle(t, second); o.err != nil {
		t.Fatal(o.err)
	}
	if got := mustRead(t, e, "file.txt"); got != "second\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestWriteSerializesANewFileCreatedThroughASymlinkedDirectoryWithItsCanonicalPath(t *testing.T) {
	e := newBlockingWriteEnv(t.TempDir(), "")
	if err := os.Mkdir(filepath.Join(e.Cwd(), "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(e.Cwd(), "real"), filepath.Join(e.Cwd(), "link"))
	tool := NewWriteTool()
	first := runAsync(context.Background(), tool, map[string]any{"path": "link/new.txt", "content": "first\n"}, e)
	<-e.firstWriteStarted
	second := runAsync(context.Background(), tool, map[string]any{"path": "real/new.txt", "content": "second\n"}, e)
	time.Sleep(20 * time.Millisecond)
	if e.secondWriteStarted.Load() {
		t.Fatal("second write started while the first was held")
	}
	close(e.finishFirstWrite)
	for _, done := range []<-chan outcome{first, second} {
		if o := settle(t, done); o.err != nil {
			t.Fatal(o.err)
		}
	}
	if got := mustRead(t, e, "real/new.txt"); got != "second\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestMutationQueueKeysAMissingFileWhoseNameContainsABackslashLikeTheCreatedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a backslash separates paths on Windows")
	}
	e := newEnv(t)
	ctx := context.Background()
	created, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := withFileMutationQueue(ctx, e, `a\b.txt`, func() (struct{}, error) {
			if err := e.WriteFile(ctx, `a\b.txt`, []byte("first\n")); err != nil {
				return struct{}{}, err
			}
			close(created)
			<-release
			return struct{}{}, nil
		})
		first <- err
	}()
	<-created
	var entered atomic.Bool
	second := make(chan error, 1)
	go func() {
		_, err := withFileMutationQueue(ctx, e, `a\b.txt`, func() (struct{}, error) {
			entered.Store(true)
			return struct{}{}, nil
		})
		second <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if entered.Load() {
		t.Fatal("second mutation entered while the first held the file")
	}
	close(release)
	if err := errors.Join(<-first, <-second); err != nil {
		t.Fatal(err)
	}
	if !entered.Load() {
		t.Fatal("second mutation never entered")
	}
}

func TestWriteDoesNotSerializeTheSamePathOnDifferentFileSystems(t *testing.T) {
	dir := t.TempDir()
	local := newBlockingWriteEnv(dir, "")
	other := newBlockingWriteEnv(dir, "other")
	tool := NewWriteTool()
	blocked := runAsync(context.Background(), tool, map[string]any{"path": "file.txt", "content": "first\n"}, local)
	<-local.firstWriteStarted
	if o := settle(t, runAsync(context.Background(), tool, map[string]any{"path": "file.txt", "content": "second\n"}, other)); o.err != nil {
		t.Fatal(o.err)
	}
	if !other.secondWriteStarted.Load() {
		t.Fatal("the other file system's write did not run")
	}
	close(local.finishFirstWrite)
	if o := settle(t, blocked); o.err != nil {
		t.Fatal(o.err)
	}
}
