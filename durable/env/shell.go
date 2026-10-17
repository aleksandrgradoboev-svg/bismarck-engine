package env

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/proc"
)

// ExecutionErrorCode classifies an ExecutionError.
type ExecutionErrorCode string

const (
	ExecAborted          ExecutionErrorCode = "aborted"
	ExecTimeout          ExecutionErrorCode = "timeout"
	ExecShellUnavailable ExecutionErrorCode = "shell_unavailable"
	ExecSpawnError       ExecutionErrorCode = "spawn_error"
	ExecCallbackError    ExecutionErrorCode = "callback_error"
	ExecUnknown          ExecutionErrorCode = "unknown"
)

// ExecutionError is a command that could not run or was cut short. A
// command that ran and failed is an ExecResult with its exit code.
type ExecutionError struct {
	Code    ExecutionErrorCode
	Message string
	// SpillPath is the spill of a command that timed out or was aborted
	// after its output crossed the spill thresholds.
	SpillPath string
	Cause     error
}

func (e *ExecutionError) Error() string { return e.Message }
func (e *ExecutionError) Unwrap() error { return e.Cause }

// ExecCode is the code of err's ExecutionError, or "" when it holds none.
func ExecCode(err error) ExecutionErrorCode {
	var ee *ExecutionError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ""
}

// Command is what Exec runs: Script through the environment's shell, or
// Argv's program directly with the rest as its arguments, unparsed.
type Command struct {
	Script string
	Argv   []string
	// argv marks Argv as given, so an empty one is an error rather than a
	// script.
	argv bool
}

// Script runs a command through the environment's shell.
func Script(script string) Command { return Command{Script: script} }

// Argv runs a program directly with its arguments.
func Argv(args ...string) Command { return Command{Argv: args, argv: true} }

// OutputStream names the stream a chunk of output came from.
type OutputStream string

const (
	Stdout OutputStream = "stdout"
	Stderr OutputStream = "stderr"
)

// OutputSkip is output an environment omitted, measured on the decoded text
// OnOutput would have received (the shape of chord/output's OutputSkip).
type OutputSkip struct {
	Bytes           int64
	Newlines        int64
	EndsWithNewline bool
}

// OutputInfo describes one chunk of output.
type OutputInfo struct {
	Stream OutputStream
	// Skipped is output omitted immediately before this chunk, only with a
	// Window; the chunk then holds more than the window, so the omitted text
	// can never be in the kept tail.
	Skipped *OutputSkip
}

// OutputWindow is the tail of the combined output a caller keeps and how
// often it samples it, so an environment that sends output over a slow link
// may omit what the caller would drop anyway. Local delivers everything.
type OutputWindow struct {
	MaxBytes       int64
	MaxLines       int64
	MinInterval    time.Duration
	BytesPerSecond float64
}

// SpillOptions spill the complete output to a temporary file once it
// exceeds either threshold.
type SpillOptions struct {
	AfterBytes int64
	// AfterLines counts complete or partial lines.
	AfterLines int64
}

// ExecOptions control one command. The zero value runs in the
// environment's working directory with the inherited environment, no
// timeout and no output callback.
type ExecOptions struct {
	Cwd string
	Env map[string]string
	// InheritEnv nil inherits the environment, as pi's default does.
	InheritEnv *bool
	// Timeout 0 is none; a negative one is invalid.
	Timeout time.Duration
	// OnOutput receives every decoded chunk of stdout and stderr as it
	// arrives, each stream decoded separately. An error it returns kills
	// the command and fails Exec with "callback_error".
	OnOutput func(ctx context.Context, text string, info OutputInfo) error
	Spill    *SpillOptions
	Window   *OutputWindow
}

// ExecResult is a command that ran.
type ExecResult struct {
	ExitCode int
	// SpillPath holds the complete raw output when it crossed the spill
	// thresholds.
	SpillPath string
}

// Shell is the process half of an environment.
type Shell interface {
	// Exec runs a command. Cancelling ctx or a timeout kills only this
	// command's processes.
	Exec(ctx context.Context, command Command, options *ExecOptions) (ExecResult, error)
	// Cleanup kills every command the environment still runs, for its
	// owner's shutdown, never for one request.
	Cleanup(ctx context.Context)
}

// ExecutionEnv is files and commands.
type ExecutionEnv interface {
	FileSystem
	Shell
}

var _ ExecutionEnv = (*Local)(nil)

const (
	maxTimeout = math.MaxInt32 * time.Millisecond
	// exitStdioGrace is how long output may stay idle after the process
	// exited while a descendant still holds its stdio, before Exec settles.
	exitStdioGrace = 100 * time.Millisecond
	readChunk      = 64 * 1024
)

func resolveTimeout(timeout time.Duration) error {
	switch {
	case timeout < 0:
		return &ExecutionError{Code: ExecTimeout, Message: "Invalid timeout: must be a finite number of seconds"}
	case timeout > maxTimeout:
		return &ExecutionError{Code: ExecTimeout, Message: fmt.Sprintf("Invalid timeout: maximum is %s seconds", jstext.NumberToString(maxTimeout.Seconds()))}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// environ is pi's getShellEnv.
func (l *Local) environ(options *ExecOptions) []string {
	if options.InheritEnv != nil && !*options.InheritEnv {
		var env []string
		for k, v := range options.Env {
			env = append(env, k+"="+v)
		}
		return env
	}
	env := os.Environ()
	for k, v := range l.shellEnv {
		env = append(env, k+"="+v)
	}
	for k, v := range options.Env {
		env = append(env, k+"="+v)
	}
	return env
}

type outputChunk struct {
	stream OutputStream
	data   []byte
}

// Exec implements Shell.
func (l *Local) Exec(ctx context.Context, command Command, options *ExecOptions) (ExecResult, error) {
	if options == nil {
		options = &ExecOptions{}
	}
	if ctx.Err() != nil {
		return ExecResult{}, &ExecutionError{Code: ExecAborted, Message: "aborted", Cause: ctx.Err()}
	}
	if err := resolveTimeout(options.Timeout); err != nil {
		return ExecResult{}, err
	}
	cwd := l.cwd
	if options.Cwd != "" {
		cwd = l.resolve(options.Cwd)
	}
	var program string
	var args []string
	var stdin string
	useStdin := false
	if command.argv || command.Argv != nil {
		if len(command.Argv) == 0 {
			return ExecResult{}, &ExecutionError{Code: ExecSpawnError, Message: "Empty argv: no program to run"}
		}
		program, args = command.Argv[0], command.Argv[1:]
	} else {
		shell, err := proc.ResolveShell(l.shellPath, fileExists, "Configure an explicit shellPath")
		if err != nil {
			return ExecResult{}, &ExecutionError{Code: ExecShellUnavailable, Message: err.Error(), Cause: err}
		}
		program, args = shell.Path, append(append([]string(nil), shell.Args...), command.Script)
		if shell.Stdin {
			args, stdin, useStdin = shell.Args, command.Script, true
		}
	}
	if _, err := os.Stat(cwd); err != nil {
		return ExecResult{}, &ExecutionError{Code: ExecSpawnError, Message: "Working directory does not exist: " + cwd + "\nCannot execute bash commands.", Cause: err}
	}

	cmd := exec.Command(program, args...)
	cmd.Dir = cwd
	cmd.Env = l.environ(options)
	if useStdin {
		cmd.Stdin = strings.NewReader(stdin)
	}
	proc.SetProcessGroup(cmd)
	// Pipes of our own rather than StdoutPipe: Wait closes those once the
	// process exits, and a descendant holding the stdio would lose output.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return ExecResult{}, &ExecutionError{Code: ExecSpawnError, Message: err.Error(), Cause: err}
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return ExecResult{}, &ExecutionError{Code: ExecSpawnError, Message: err.Error(), Cause: err}
	}
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	// Started and tracked in one step, as pi's spawn and its add to the
	// active set run in one turn: a Cleanup in between would miss a command
	// that is already running.
	l.mu.Lock()
	startErr := cmd.Start()
	if startErr == nil {
		l.trackLocked(cmd, true)
	}
	l.mu.Unlock()
	stdoutW.Close()
	stderrW.Close()
	if startErr != nil {
		stdoutR.Close()
		stderrR.Close()
		return ExecResult{}, &ExecutionError{Code: ExecSpawnError, Message: startErr.Error(), Cause: startErr}
	}
	defer l.track(cmd, false)

	return l.run(ctx, cmd, stdoutR, stderrR, options)
}

func (l *Local) track(cmd *exec.Cmd, active bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trackLocked(cmd, active)
}

func (l *Local) trackLocked(cmd *exec.Cmd, active bool) {
	if l.active == nil {
		l.active = map[*exec.Cmd]bool{}
	}
	if active {
		l.active[cmd] = true
	} else {
		delete(l.active, cmd)
	}
}

// Cleanup kills every command still running.
func (l *Local) Cleanup(ctx context.Context) {
	l.mu.Lock()
	cmds := make([]*exec.Cmd, 0, len(l.active))
	for cmd := range l.active {
		cmds = append(cmds, cmd)
	}
	l.active = nil
	l.mu.Unlock()
	for _, cmd := range cmds {
		_ = proc.KillProcessTree(cmd)
	}
}

// run streams the output of a started command until it settles.
func (l *Local) run(ctx context.Context, cmd *exec.Cmd, stdoutR, stderrR *os.File, options *ExecOptions) (ExecResult, error) {
	chunks := make(chan outputChunk, 16)
	readerDone := make(chan struct{}, 2)
	read := func(stream OutputStream, r *os.File) {
		defer func() { readerDone <- struct{}{} }()
		for {
			buf := make([]byte, readChunk)
			n, err := r.Read(buf)
			if n > 0 {
				chunks <- outputChunk{stream, buf[:n]}
			}
			if err != nil {
				return
			}
		}
	}
	go read(Stdout, stdoutR)
	go read(Stderr, stderrR)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var timer <-chan time.Time
	if options.Timeout > 0 {
		t := time.NewTimer(options.Timeout)
		defer t.Stop()
		timer = t.C
	}
	kill := func() { _ = proc.KillProcessTree(cmd) }

	decoders := map[OutputStream]*jstext.TextDecoder{Stdout: {}, Stderr: {}}
	var callbackErr, spillErr error
	timedOut, aborted := false, false
	emit := func(text string, stream OutputStream) {
		if text == "" || options.OnOutput == nil || callbackErr != nil {
			return
		}
		if err := options.OnOutput(ctx, text, OutputInfo{Stream: stream}); err != nil {
			callbackErr = &ExecutionError{Code: ExecCallbackError, Message: err.Error(), Cause: err}
			kill()
		}
	}

	// Spill: output seen before the spill starts counts against the
	// thresholds and is kept for its prefix.
	var spillPath string
	var spillFile *os.File
	var prefix [][]byte
	var seenBytes, seenNewlines int64
	failSpill := func(err error) {
		if spillErr == nil {
			spillErr = &ExecutionError{Code: ExecUnknown, Message: "Failed to preserve complete shell output: " + err.Error(), Cause: err}
			kill()
		}
	}
	writeSpill := func(data []byte) {
		if spillErr != nil || spillFile == nil {
			return
		}
		if _, err := spillFile.Write(data); err != nil {
			failSpill(err)
		}
	}
	startSpill := func() {
		path, err := l.createSpill(ctx)
		if err == nil {
			spillPath = path
			spillFile, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		}
		if err != nil {
			failSpill(err)
		}
	}
	feed := func(c outputChunk) {
		emit(decoders[c.stream].Decode(c.data), c.stream)
		spill := options.Spill
		if spill == nil || len(c.data) == 0 || spillErr != nil {
			return
		}
		if spillFile != nil {
			writeSpill(c.data)
			return
		}
		seenBytes += int64(len(c.data))
		seenNewlines += int64(bytes.Count(c.data, []byte{'\n'}))
		lines := seenNewlines
		if c.data[len(c.data)-1] != '\n' {
			lines++
		}
		if seenBytes <= spill.AfterBytes && lines <= spill.AfterLines {
			prefix = append(prefix, c.data)
			return
		}
		startSpill()
		for _, p := range prefix {
			writeSpill(p)
		}
		prefix = nil
		writeSpill(c.data)
	}

	readersLeft := 2
	var exitErr error
	exitedSeen := false
	var idle <-chan time.Time
	var idleTimer *time.Timer
	armIdle := func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
		idleTimer = time.NewTimer(exitStdioGrace)
		idle = idleTimer.C
	}
	done := ctx.Done()
loop:
	for readersLeft > 0 || !exitedSeen {
		select {
		case c := <-chunks:
			feed(c)
			if exitedSeen {
				armIdle()
			}
		case <-readerDone:
			readersLeft--
		case exitErr = <-exited:
			exitedSeen = true
			if readersLeft > 0 {
				armIdle()
			}
		case <-idle:
			break loop // a descendant holds the stdio open; settle without it
		case <-timer:
			timedOut = true
			timer = nil
			kill()
		case <-done:
			aborted = true
			done = nil
			kill()
		}
	}
	if idleTimer != nil {
		idleTimer.Stop()
	}
	stdoutR.Close()
	stderrR.Close()
	// Output already read before the pipes closed.
	for drained := false; !drained; {
		select {
		case c := <-chunks:
			feed(c)
		default:
			drained = true
		}
	}
	if spillFile != nil {
		if err := spillFile.Close(); err != nil {
			failSpill(err)
		}
	}
	emit(decoders[Stdout].Flush(), Stdout)
	emit(decoders[Stderr].Flush(), Stderr)

	if callbackErr != nil {
		return ExecResult{}, callbackErr
	}
	if timedOut || aborted {
		e := &ExecutionError{Code: ExecAborted, Message: "aborted", SpillPath: spillPath, Cause: ctx.Err()}
		if timedOut {
			e = &ExecutionError{Code: ExecTimeout, Message: "timeout:" + jstext.NumberToString(options.Timeout.Seconds()), SpillPath: spillPath}
		}
		return ExecResult{}, e
	}
	if spillErr != nil {
		return ExecResult{}, spillErr
	}
	var ee *exec.ExitError
	if exitErr != nil && !errors.As(exitErr, &ee) {
		return ExecResult{}, &ExecutionError{Code: ExecSpawnError, Message: exitErr.Error(), Cause: exitErr}
	}
	return ExecResult{ExitCode: exitCode(cmd.ProcessState), SpillPath: spillPath}, nil
}

// exitCode is pi's `code ?? (signal ? 128 + signals[signal] : 1)`: a
// process killed by a signal, the OOM killer say, has no exit code of its
// own and is not mistaken for a clean exit.
func exitCode(state *os.ProcessState) int {
	if state == nil {
		return 1
	}
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	if signal, ok := proc.ExitSignalNumber(state); ok {
		return 128 + signal
	}
	return 1
}

// createSpill is the spill file of one command; tests replace it.
func (l *Local) createSpill(ctx context.Context) (string, error) {
	if l.spillFile != nil {
		return l.spillFile(ctx)
	}
	return l.CreateTempFile(ctx, TempFileOptions{Prefix: "pi-output-", Suffix: ".log"})
}
