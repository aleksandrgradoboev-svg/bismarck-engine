package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// Cases beyond pi's tests: each covers a path no pi case reaches.

// changingEnv opens readers whose file looks modified at every Info, as a
// file rewritten in place while it is read.
type changingEnv struct {
	*env.Local
}

type changingReader struct {
	env.BinaryReader
	calls int
}

func (e *changingEnv) OpenBinaryReader(ctx context.Context, path string, options env.BinaryReaderOptions) (env.BinaryReader, error) {
	reader, err := e.Local.OpenBinaryReader(ctx, path, options)
	if err != nil {
		return nil, err
	}
	return &changingReader{BinaryReader: reader}, nil
}

func (r *changingReader) Info(ctx context.Context) (env.FileInfo, error) {
	info, err := r.BinaryReader.Info(ctx)
	r.calls++
	info.ModTime = info.ModTime.Add(time.Duration(r.calls) * time.Second)
	return info, err
}

func TestReadFailsWhenTheFileChangesDuringBothPasses(t *testing.T) {
	local := newEnv(t)
	mustWrite(t, local, "busy.txt", "one\ntwo\n")
	_, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "busy.txt"}, &changingEnv{Local: local})
	if err == nil || err.Error() != "busy.txt changed while it was read" {
		t.Fatalf("err = %v", err)
	}
}

func TestReadNamesNoLineForAFractionalStartAtALineLongerThanTheByteLimit(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "long.txt", "a\n"+strings.Repeat("é", 40_000)+"\n")
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "long.txt", "offset": 2.5}, e)
	if err != nil {
		t.Fatal(err)
	}
	// Like allLines[1.5] in pi, a fractional start line names no line: the
	// shown start is empty.
	if len(result.Output) != 0 || result.Output == nil {
		t.Fatalf("output = %#v, want an empty list", result.Output)
	}
	want := "Line 2.5 is 0B, exceeds the 50.0KB limit; showing its first 0B. Use bash: sed -n '2.5p' long.txt | tail -c +1"
	if got := diagnosticText(result); got != want {
		t.Fatalf("diagnostics = %q", got)
	}
}

func TestReadDropsALeadingAtFromThePath(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "notes.txt", "hello")
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "@notes.txt"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := textOutput(result); got != "hello" {
		t.Fatalf("output = %q", got)
	}
}

func TestReadFindsAFileNamedWithATypographicApostrophe(t *testing.T) {
	e := newEnv(t)
	mustWrite(t, e, "it’s.txt", "curly")
	result, err := run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "it's.txt"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := textOutput(result); got != "curly" {
		t.Fatalf("output = %q", got)
	}
}

// cancelAfterWriteEnv cancels the call once its write has succeeded.
type cancelAfterWriteEnv struct {
	*env.Local
	cancel context.CancelFunc
}

func (e *cancelAfterWriteEnv) WriteFile(ctx context.Context, path string, content []byte) error {
	err := e.Local.WriteFile(ctx, path, content)
	e.cancel()
	return err
}

func TestWriteFailsWhenCancelledRightAfterAWriteThatSucceeded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &cancelAfterWriteEnv{Local: newEnv(t), cancel: cancel}
	_, err := run(ctx, NewWriteTool(), map[string]any{"path": "file.txt", "content": "data"}, e)
	if err != errAborted {
		t.Fatalf("err = %v, want %v", err, errAborted)
	}
	if got := mustRead(t, e.Local, "file.txt"); got != "data" {
		t.Fatalf("file = %q", got)
	}
}
