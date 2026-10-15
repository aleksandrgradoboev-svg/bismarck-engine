package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The host side of an execution: pi's runtime/execution.ts. The host owns
// everything the script must not influence: the tool table, call records,
// output, the deadline, cancellation, and the decoding of what the VM reports.
//
// Lifecycle of one execution: the constructor arms the deadline and the
// cancellation hook and starts the VM; messages from the VM arrive on its
// goroutine, tool calls run on their own goroutines and post results back; the
// first terminal event wins (done, crash, a VM failure, the deadline, an
// abort), finish() cancels pending calls, stops the VM, and publishes the
// result once the VM has stopped. Later events are ignored.

// errBridge marks a payload from the prelude that does not decode. The prelude
// runs in the same VM as the script, so a script that patched built-ins could
// make it send malformed data; the execution then fails as a sandbox error.
type errBridge struct{ msg string }

func (e *errBridge) Error() string { return e.msg }

func bridgeErr(format string, args ...any) error { return &errBridge{fmt.Sprintf(format, args...)} }

// mustJSON marshals without HTML escaping, like JSON.stringify. Inputs are
// plain data built here, so a failure is a bug.
func mustJSON(v any) string {
	s, err := marshalJSON(v)
	if err != nil {
		panic(err)
	}
	return s
}

func marshalJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

func trimEndSpace(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) }

// encodeStore serializes the snapshot for load(); like JSON.stringify,
// a value that marshals to nothing is dropped.
func encodeStore(store map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(store))
	for key, value := range store {
		if value == nil {
			continue
		}
		s, err := marshalJSON(value)
		if err != nil {
			return nil, fmt.Errorf("codemode: store value for %q: %w", key, err)
		}
		out[key] = s
	}
	return out, nil
}

func parseStoreWrites(text string) (StoreWrites, error) {
	writes := StoreWrites{Set: map[string]json.RawMessage{}, Delete: []string{}}
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(text), &entries); err != nil || entries == nil {
		if json.Valid([]byte(text)) {
			return writes, bridgeErr("store writes are not an array")
		}
		return writes, bridgeErr("store writes is not valid JSON")
	}
	for _, raw := range entries {
		var entry []any
		if json.Unmarshal(raw, &entry) != nil || len(entry) < 1 || len(entry) > 2 {
			return writes, bridgeErr("store writes contain a malformed entry")
		}
		key, ok := entry[0].(string)
		if !ok {
			return writes, bridgeErr("store writes contain a malformed entry")
		}
		if len(entry) == 1 {
			writes.Delete = append(writes.Delete, key)
			continue
		}
		value, ok := entry[1].(string)
		if !ok {
			return writes, bridgeErr("store writes contain a malformed entry")
		}
		if !json.Valid([]byte(value)) {
			return writes, bridgeErr("store value for %s is not valid JSON", mustJSON(key))
		}
		writes.Set[key] = json.RawMessage(value)
	}
	return writes, nil
}

func parseScriptError(text string) (*Error, error) {
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return nil, bridgeErr("script error is not valid JSON")
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, bridgeErr("script error is not an object")
	}
	e := &Error{Kind: ErrorScript}
	message, ok := obj["message"].(string)
	if !ok {
		return nil, bridgeErr("script error is malformed")
	}
	e.Message = message
	for field, dst := range map[string]*string{"name": &e.Name, "stack": &e.Stack} {
		if v, present := obj[field]; present {
			s, ok := v.(string)
			if !ok {
				return nil, bridgeErr("script error is malformed")
			}
			*dst = s
		}
	}
	return e, nil
}

type pendingCall struct {
	record    *Call // nil for globals
	startedAt time.Time
	cancel    context.CancelFunc
}

// control are the callbacks of a host-controlled (Start) run.
type control struct {
	onStarted func(fingerprint string)
	onCall    func(HostCall)
	onIdle    func(seq, issued int)
}

type executionOptions struct {
	data    vmStartData
	tools   map[string]Tool
	globals map[string]Tool
	timeout time.Duration // <= 0: none
	ctx     context.Context
	control *control
	// register is called before the VM starts and may refuse the run;
	// release once the VM has stopped.
	register func(*execution) error
	release  func(*execution)
}

// execution is one script run in its own VM.
type execution struct {
	mu        sync.Mutex
	finished  bool
	session   *vmSession
	tools     map[string]Tool
	globals   map[string]Tool
	control   *control
	delivered map[int]bool
	output    []OutputItem
	calls     []*Call
	pending   map[int]*pendingCall
	timer     *time.Timer
	stopAbort func() bool
	result    chan Result
	released  chan struct{}
	release   func(*execution)
	// unsent holds results produced before the VM's session existed.
	unsent []hostMessage
}

func newExecution(o executionOptions) (*execution, error) {
	e := &execution{
		tools:     o.tools,
		globals:   o.globals,
		control:   o.control,
		delivered: map[int]bool{},
		output:    []OutputItem{},
		pending:   map[int]*pendingCall{},
		result:    make(chan Result, 1),
		released:  make(chan struct{}),
		release:   o.release,
	}
	if err := o.register(e); err != nil {
		return nil, err
	}
	e.mu.Lock()
	if o.timeout > 0 {
		timeout := o.timeout
		e.timer = time.AfterFunc(timeout, func() {
			e.finish(&Error{Kind: ErrorTimeout, Message: fmt.Sprintf("Execution timed out after %d ms", timeout.Milliseconds())}, nil, StoreWrites{}, nil)
		})
	}
	e.mu.Unlock()
	if o.ctx != nil {
		if o.ctx.Err() != nil {
			e.abortFromContext(o.ctx)
			return e, nil
		}
		ctx := o.ctx
		e.mu.Lock()
		e.stopAbort = context.AfterFunc(ctx, func() { e.abortFromContext(ctx) })
		e.mu.Unlock()
	}
	session := startVM(o.data, vmEvents{
		message: e.receive,
		failure: func(err *Error) { e.finish(err, nil, StoreWrites{}, nil) },
	})
	e.mu.Lock()
	finished := e.finished
	e.session = session
	for _, m := range e.unsent {
		session.post(m)
	}
	e.unsent = nil
	e.mu.Unlock()
	if finished {
		// Finished while the VM was starting: finish() saw no session to stop.
		go session.stop()
	}
	return e, nil
}

// abortFromContext ends the run with the context's cause, as pi does with
// an AbortSignal's reason.
func (e *execution) abortFromContext(ctx context.Context) {
	message := "Execution aborted"
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		message = cause.Error()
	}
	e.finish(&Error{Kind: ErrorAborted, Message: message}, nil, StoreWrites{}, nil)
}

func (e *execution) abort(message string) {
	e.finish(&Error{Kind: ErrorAborted, Message: message}, nil, StoreWrites{}, nil)
}

// receive handles a message from the VM, on the VM's goroutine. A failure
// here must end the execution rather than escape.
func (e *execution) receive(m vmMessage) {
	if err := e.handleMessage(m); err != nil {
		var be *errBridge
		message := "Sandbox host failed: " + err.Error()
		if errors.As(err, &be) {
			message = "Sandbox bridge broken: " + be.msg + ". The script may have modified built-ins such as a prototype's toJSON."
		}
		e.finish(&Error{Kind: ErrorSandbox, Message: message}, nil, StoreWrites{}, nil)
	}
}

func (e *execution) handleMessage(m vmMessage) error {
	e.mu.Lock()
	if e.finished {
		e.mu.Unlock()
		return nil
	}
	switch m.kind {
	case "output":
		e.output = append(e.output, m.item)
		e.mu.Unlock()
	case "call":
		if _, dup := e.pending[m.id]; dup {
			e.mu.Unlock()
			return bridgeErr("duplicate call id %d", m.id)
		}
		var args json.RawMessage
		if m.args != nil {
			if !json.Valid([]byte(*m.args)) {
				e.mu.Unlock()
				return bridgeErr("call arguments is not valid JSON")
			}
			args = json.RawMessage(*m.args)
		}
		var record *Call
		if m.target == "tool" {
			record = &Call{Name: m.name, Status: CallCancelled}
			e.calls = append(e.calls, record)
		}
		ctx, cancel := context.WithCancel(context.Background())
		e.pending[m.id] = &pendingCall{record: record, startedAt: time.Now(), cancel: cancel}
		control := e.control
		e.mu.Unlock()
		if control != nil {
			control.onCall(HostCall{ID: m.id, Target: m.target, Name: m.name, Args: args})
		} else {
			go e.runCall(ctx, m.id, m.target, m.name, args)
		}
	case "started":
		control := e.control
		e.mu.Unlock()
		if control != nil && control.onStarted != nil {
			control.onStarted(m.fingerprint)
		}
	case "idle":
		control := e.control
		e.mu.Unlock()
		if control != nil && control.onIdle != nil {
			control.onIdle(m.seq, m.issued)
		}
	case "done":
		e.mu.Unlock()
		// Decode everything before finish(), which must not fail once it starts.
		if !m.ok {
			scriptErr, err := parseScriptError(m.errorJSON)
			if err != nil {
				return err
			}
			e.finish(scriptErr, nil, StoreWrites{}, m.during)
			return nil
		}
		var value json.RawMessage
		if m.value != nil {
			if !json.Valid([]byte(*m.value)) {
				return bridgeErr("return value is not valid JSON")
			}
			value = json.RawMessage(*m.value)
		}
		writes, err := parseStoreWrites(m.writes)
		if err != nil {
			return err
		}
		e.finish(nil, value, writes, m.during)
	case "crash":
		e.mu.Unlock()
		e.finish(&Error{Kind: ErrorSandbox, Message: m.message}, nil, StoreWrites{}, nil)
	default:
		e.mu.Unlock()
		return bridgeErr("unknown message from the worker")
	}
	return nil
}

// runCall runs one tool or global for an execute() run and posts its result.
func (e *execution) runCall(ctx context.Context, id int, target, name string, args json.RawMessage) {
	isTool := target == "tool"
	table := e.globals
	if isTool {
		table = e.tools
	}
	reply := hostMessage{id: id}
	status := CallError
	if tool, ok := table[name]; !ok || tool.Execute == nil {
		reply.payload = ptr(fmt.Sprintf("Unknown %s %q", target, name))
	} else if value, err := callTool(ctx, tool, args); err != nil {
		reply.payload = ptr(err.Error())
	} else if value == nil {
		reply.ok, status = true, CallOK
	} else if text, err := marshalJSON(value); err != nil {
		reply.payload = ptr(err.Error())
	} else {
		reply.ok, reply.payload, status = true, &text, CallOK
	}

	e.mu.Lock()
	p, live := e.pending[id]
	// Already cancelled by finish(): the record keeps "cancelled" and the VM
	// is gone or going.
	if !live {
		e.mu.Unlock()
		return
	}
	delete(e.pending, id)
	p.cancel()
	if p.record != nil {
		p.record.Status = status
		p.record.Duration = time.Since(p.startedAt)
	}
	e.postLocked(reply)
	e.mu.Unlock()
}

// postLocked sends a result to the VM, or queues it until the session exists.
// e.mu must be held.
func (e *execution) postLocked(m hostMessage) {
	if e.session == nil {
		e.unsent = append(e.unsent, m)
		return
	}
	e.session.post(m)
}

// callTool runs Execute, turning a panic into an error the script sees, so a
// faulty tool fails one call instead of the host process.
func callTool(ctx context.Context, tool Tool, args json.RawMessage) (value any, err error) {
	defer func() {
		if r := recover(); r != nil {
			value, err = nil, fmt.Errorf("tool %q panicked: %v", tool.Name, r)
		}
	}()
	return tool.Execute(ctx, args)
}

func ptr[T any](v T) *T { return &v }

// deliver answers call id as result number seq (Run.Deliver).
func (e *execution) deliver(seq int, now float64, id int, outcome Outcome) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil
	}
	if !isFinite(now) {
		return fmt.Errorf("The clock value of result %d is not a finite number", seq)
	}
	if seq < 1 || e.delivered[seq] {
		return fmt.Errorf("Result number %d is not a positive integer used once", seq)
	}
	p, ok := e.pending[id]
	if !ok {
		return fmt.Errorf("Call %d is not waiting for a result", id)
	}
	// Serialized before anything is recorded, so a value JSON cannot hold
	// leaves the call waiting for another answer.
	msg := hostMessage{id: id, ok: outcome.OK, seq: seq, now: now, hasNow: true}
	if outcome.OK {
		if outcome.Value != nil {
			text, err := marshalJSON(outcome.Value)
			if err != nil {
				return err
			}
			msg.payload = &text
		}
	} else {
		msg.payload = ptr(outcome.Message)
		if outcome.Properties != nil {
			text, err := marshalJSON(outcome.Properties)
			if err != nil {
				return err
			}
			msg.properties = &text
		}
	}
	e.delivered[seq] = true
	delete(e.pending, id)
	p.cancel()
	if p.record != nil {
		p.record.Status = CallOK
		if !outcome.OK {
			p.record.Status = CallError
		}
		p.record.Duration = time.Since(p.startedAt)
	}
	e.postLocked(msg)
	return nil
}

func isFinite(f float64) bool { return f-f == 0 }

func (e *execution) isFinished() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.finished
}

func (e *execution) finish(err *Error, value json.RawMessage, writes StoreWrites, during *During) {
	e.mu.Lock()
	if e.finished {
		e.mu.Unlock()
		return
	}
	e.finished = true
	if e.timer != nil {
		e.timer.Stop()
	}
	if e.stopAbort != nil {
		e.stopAbort()
	}
	now := time.Now()
	for _, p := range e.pending {
		if p.record != nil {
			p.record.Duration = now.Sub(p.startedAt)
		}
		p.cancel()
	}
	clear(e.pending)
	e.unsent = nil

	calls := make([]Call, len(e.calls))
	for i, c := range e.calls {
		calls[i] = *c
	}
	result := Result{OK: err == nil, Error: err, Output: e.output, Calls: calls}
	if during != nil {
		result.During = &During{Seq: during.Seq, Issued: during.Issued}
	}
	if err == nil {
		result.Value = value
		if writes.Set == nil {
			writes = StoreWrites{Set: map[string]json.RawMessage{}, Delete: []string{}}
		}
		result.StoreWrites = writes
	}
	session := e.session
	e.mu.Unlock()

	// finish may run on the VM's goroutine (a done message), so stopping it,
	// which waits for that goroutine, happens elsewhere.
	go func() {
		if session != nil {
			session.stop()
		}
		e.release(e)
		close(e.released)
		e.result <- result
	}()
}
