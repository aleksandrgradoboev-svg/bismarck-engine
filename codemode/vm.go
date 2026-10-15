package codemode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/codemode/quickjs"
)

// The VM side of an execution: pi's runtime/vm.ts. It drives one QuickJS VM
// through one script on its own goroutine and talks to the host only through
// vmMessage and hostMessage, the Go form of pi's protocol.ts. Tool arguments,
// results and values cross as JSON text.

// vmStartData is everything a VM needs to run one script (pi's VmStartData).
type vmStartData struct {
	code    string
	tools   []vmTool
	globals []vmGlobal
	// memoryLimit caps the QuickJS heap; 0 means none.
	memoryLimit uint32
	// store is the snapshot for load(): key to JSON text.
	store map[string]string
	// deterministic: UTC, a virtual clock from startedAt (ms since the epoch)
	// that moves only with delivered results, randomness from seed, no
	// WeakRef, results applied in seq order, started/idle messages.
	deterministic *deterministicStart
}

type vmTool struct {
	Name        string `json:"name"`
	JSName      string `json:"jsName"`
	Description string `json:"description"`
}

type vmGlobal struct {
	Name   string `json:"name"`
	Spread bool   `json:"spread"`
}

type deterministicStart struct {
	startedAt float64
	seed      uint32
}

// vmMessage is a message from the VM to the host (pi's WorkerToHostMessage).
type vmMessage struct {
	kind string // "call", "output", "done", "crash", "started", "idle"

	// call
	id     int
	target string // "tool" or "global"
	name   string
	args   *string

	// output
	item OutputItem

	// done: ok with value (nil: undefined) and writes (JSON array of [key,
	// json] and [key]), or not ok with errorJSON ({name?, message, stack?}).
	ok        bool
	value     *string
	writes    string
	errorJSON string
	during    *During

	// crash
	message string
	// started
	fingerprint string
	// idle
	seq, issued int
}

// hostMessage is a call's result from the host to the VM (pi's
// HostToWorkerMessage). payload is the JSON result when ok (nil: undefined),
// otherwise the error message. seq, now and properties are deterministic
// mode only.
type hostMessage struct {
	id         int
	ok         bool
	payload    *string
	seq        int
	now        float64
	hasNow     bool
	properties *string
}

// deterministicVersion is bumped whenever this file or the prelude changes
// what a deterministic script observes beyond what the fingerprint's other
// parts already capture. It is pi's DETERMINISTIC_VERSION, kept equal while
// the two implementations agree.
const deterministicVersion = 1

// jobsPerPoll is how many promise jobs run between interrupt polls while the
// job queue drains: a script such as `while (true) await null` runs only a
// few opcodes per job, so QuickJS's own opcode-counting poll would rarely fire.
const jobsPerPoll = 50

// mulberry32 is pi's PRNG for deterministic randomness: one 32-bit draw per call.
func mulberry32(seed uint32) func() uint32 {
	state := seed
	return func() uint32 {
		state += 0x6d2b79f5
		v := (state ^ state>>15) * (1 | state)
		v = (v + (v^v>>7)*(61|v)) ^ v
		return v ^ v>>14
	}
}

// virtualClock is the deterministic VM's clock: seed-derived while the
// context is created, then virtual time in ms.
type virtualClock struct {
	creating  bool
	startedAt float64
	now       float64
	seed      uint32
}

// read answers a WASI clock read in nanoseconds. QuickJS seeds Math.random()
// from the first realtime read, in microseconds, turning 0 into 1, so seed s
// reads s + 1.
func (c *virtualClock) read(clockID uint32) uint64 {
	switch {
	case c.creating && clockID == quickjs.ClockRealtime:
		return (uint64(c.seed) + 1) * 1000
	case c.creating:
		return 0
	case clockID == quickjs.ClockRealtime:
		return uint64(jsRound(c.now)) * 1_000_000
	default:
		return uint64(jsRound(c.now-c.startedAt)) * 1_000_000
	}
}

// jsRound is JavaScript's Math.round: halves round toward +Infinity.
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// vmEvents are the callbacks a running VM reports through. They are called on
// the VM's goroutine, never concurrently, and must not block on the VM.
type vmEvents struct {
	message func(vmMessage)
	// failure: the VM could not start or stopped outside the bridge.
	failure func(*Error)
}

// vmSession is the host's handle on a running VM (pi's VmChannel).
type vmSession struct {
	cancel  context.CancelFunc
	stopped atomic.Bool
	done    chan struct{}

	mu     sync.Mutex
	inbox  []hostMessage
	notify chan struct{}
}

// post queues a result for the VM. It never blocks.
func (s *vmSession) post(m hostMessage) {
	s.mu.Lock()
	s.inbox = append(s.inbox, m)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// stop stops the VM and waits for its goroutine to exit. Canceling the context
// aborts even a script spinning in wasm; the stopped flag also interrupts it at
// QuickJS's next poll.
func (s *vmSession) stop() {
	s.stopped.Store(true)
	s.cancel()
	<-s.done
}

// startVM starts a VM for one script on its own goroutine.
func startVM(data vmStartData, events vmEvents) *vmSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &vmSession{cancel: cancel, done: make(chan struct{}), notify: make(chan struct{}, 1)}
	go func() {
		defer close(s.done)
		defer cancel()
		runVM(ctx, s, data, events)
	}()
	return s
}

func crashMessage(err error) vmMessage {
	var exc *quickjs.Exception
	if errors.As(err, &exc) {
		return vmMessage{kind: "crash", message: exc.Error()}
	}
	return vmMessage{kind: "crash", message: "Error: " + err.Error()}
}

// describeException is the JSON a script error crosses as when the script
// fails to compile: {name, message, stack}, stack headed "Name: message".
func describeException(e *quickjs.Exception) string {
	head := e.Name
	if e.Message != "" {
		head = e.Name + ": " + e.Message
	}
	stack := head
	if trimmed := trimEndSpace(e.Stack); trimmed != "" {
		stack = head + "\n" + trimmed
	}
	return mustJSON(map[string]string{"name": e.Name, "message": e.Message, "stack": stack})
}

func runVM(ctx context.Context, s *vmSession, data vmStartData, events vmEvents) {
	post := events.message
	det := data.deterministic
	var clock *virtualClock
	opts := quickjs.Options{
		MemoryLimit: data.memoryLimit,
		Interrupt:   s.stopped.Load,
	}
	if det != nil {
		clock = &virtualClock{creating: true, startedAt: det.startedAt, now: det.startedAt, seed: det.seed}
		random := mulberry32(det.seed ^ 0x9e3779b9)
		opts.Clock = clock.read
		opts.Random = func(p []byte) {
			for i := 0; i < len(p); i += 4 {
				v := random()
				for b := 0; b < 4 && i+b < len(p); b++ {
					p[i+b] = byte(v >> (8 * b))
				}
			}
		}
		opts.TimezoneOffset = func(int64) int32 { return 0 }
		opts.Intrinsics = quickjs.IntrinsicsAll &^ quickjs.IntrinsicWeakRef
	}
	vm, err := quickjs.New(ctx, opts)
	if err != nil {
		if ctx.Err() == nil {
			events.failure(&Error{Kind: ErrorSandbox, Message: "Failed to start QuickJS: " + err.Error()})
		}
		return
	}
	defer vm.Close()
	if clock != nil {
		clock.creating = false
	}

	r := &vmRun{vm: vm, post: post, clock: clock, s: s, early: map[int]hostMessage{}, nextSeq: 1}
	if err := r.start(data); err != nil {
		post(crashMessage(err))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
		}
		s.mu.Lock()
		batch := s.inbox
		s.inbox = nil
		s.mu.Unlock()
		for _, m := range batch {
			if ctx.Err() != nil {
				return
			}
			r.deliver(m)
		}
	}
}

// vmRun is one VM's state while it runs the script.
type vmRun struct {
	vm    *quickjs.VM
	post  func(vmMessage)
	clock *virtualClock
	s     *vmSession

	api, settle, stalled quickjs.Value
	// issued counts calls the script made, tools and globals; reported with idle.
	issued int
	// deterministic: the next result to apply, results that arrived early, and
	// the result being processed (0 during the first run), reported with done.
	nextSeq int
	early   map[int]hostMessage
	current int
}

func (r *vmRun) start(data vmStartData) error {
	vm := r.vm
	bridge, err := vm.NewFunction("bridge", r.bridge)
	if err != nil {
		return err
	}
	defer bridge.Free()

	prelude, err := vm.Eval(preludeSource, "codemode-prelude.js")
	if err != nil {
		return err
	}
	defer prelude.Free()
	args := []quickjs.Value{bridge}
	defer func() {
		for _, a := range args[1:] {
			a.Free()
		}
	}()
	for _, text := range []string{mustJSON(data.tools), mustJSON(data.globals), mustJSON(data.store)} {
		v, err := vm.NewString(text)
		if err != nil {
			return err
		}
		args = append(args, v)
	}
	if r.api, err = vm.Call(prelude, vm.Undefined(), args...); err != nil {
		return err
	}
	if r.settle, err = r.api.Get("settle"); err != nil {
		return err
	}
	if r.stalled, err = r.api.Get("stalled"); err != nil {
		return err
	}
	run, err := r.api.Get("run")
	if err != nil {
		return err
	}
	defer run.Free()

	if r.clock != nil {
		heap := "none"
		if data.memoryLimit != 0 {
			heap = fmt.Sprint(data.memoryLimit)
		}
		r.post(vmMessage{kind: "started", fingerprint: fmt.Sprintf(
			"quickjs-wasi %s, quickjs %s, prelude %s, deterministic %d, stack %d, heap %s, budget none",
			quickjs.Version, vm.QuickJSVersion(), preludeHash, deterministicVersion, quickjs.MaxStackSize, heap)})
	}

	// The prefix shares the first line with the script, so reported line
	// numbers match the script as written.
	fn, err := vm.Eval("(async (tools, console) => {"+data.code+"\n})", "codemode.js")
	if err != nil {
		var exc *quickjs.Exception
		if errors.As(err, &exc) {
			r.post(vmMessage{kind: "done", ok: false, errorJSON: describeException(exc)})
			return nil
		}
		return err
	}
	defer fn.Free()
	res, err := vm.Call(run, r.api, fn)
	if err != nil {
		return err
	}
	res.Free()
	if err := r.drain(); err != nil {
		return err
	}
	if r.clock != nil {
		r.post(vmMessage{kind: "idle", seq: 0, issued: r.issued})
	}
	return nil
}

// bridge is called from the prelude with primitives only.
func (r *vmRun) bridge(_ quickjs.Value, args []quickjs.Value) (quickjs.Value, error) {
	arg := func(i int) quickjs.Value {
		if i < len(args) {
			return args[i]
		}
		return r.vm.Undefined()
	}
	optString := func(v quickjs.Value) *string {
		if v.IsUndefined() {
			return nil
		}
		s := v.String()
		return &s
	}
	switch kind := arg(0).String(); kind {
	case "call", "global":
		r.issued++
		target := "tool"
		if kind == "global" {
			target = "global"
		}
		r.post(vmMessage{kind: "call", id: int(arg(1).Float64()), target: target, name: arg(2).String(), args: optString(arg(3))})
	case "output":
		var item OutputItem
		switch arg(1).String() {
		case "image":
			item = OutputItem{Type: "image", Data: arg(2).String(), MimeType: arg(3).String()}
		case "console":
			item = OutputItem{Type: "text", Text: arg(2).String(), Console: true}
		default:
			item = OutputItem{Type: "text", Text: arg(2).String()}
		}
		r.post(vmMessage{kind: "output", item: item})
	case "done":
		// The host stops the VM on done, so the idle of this drain never
		// arrives: report its numbers here.
		var during *During
		if r.clock != nil {
			during = &During{Seq: r.current, Issued: r.issued}
		}
		if arg(1).Bool() {
			r.post(vmMessage{kind: "done", ok: true, value: optString(arg(2)), writes: arg(3).String(), during: during})
		} else {
			r.post(vmMessage{kind: "done", ok: false, errorJSON: arg(2).String(), during: during})
		}
	}
	return r.vm.Undefined(), nil
}

// drain runs queued jobs, then fails a script that waits on nothing that can
// ever resume it.
func (r *vmRun) drain() error {
	for jobs := 1; ; jobs++ {
		pending, err := r.vm.IsJobPending()
		if err != nil {
			return err
		}
		if !pending {
			break
		}
		// A job only fails for uncatchable errors, such as an interrupt.
		if err := r.vm.ExecutePendingJob(); err != nil {
			return errors.New("Promise job failed")
		}
		if jobs%jobsPerPoll == 0 && r.s.stopped.Load() {
			return errors.New("Execution interrupted")
		}
	}
	res, err := r.vm.Call(r.stalled, r.api)
	if err != nil {
		return err
	}
	res.Free()
	return nil
}

// apply hands the VM one result and runs the script until it waits again.
func (r *vmRun) apply(m hostMessage) error {
	vm := r.vm
	id, err := vm.NewNumber(float64(m.id))
	if err != nil {
		return err
	}
	defer id.Free()
	str := func(p *string) (quickjs.Value, error) {
		if p == nil {
			return vm.Undefined(), nil
		}
		return vm.NewString(*p)
	}
	payload, err := str(m.payload)
	if err != nil {
		return err
	}
	defer payload.Free()
	props, err := str(m.properties)
	if err != nil {
		return err
	}
	defer props.Free()
	res, err := vm.Call(r.settle, r.api, id, vm.Bool(m.ok), payload, props)
	if err != nil {
		return err
	}
	res.Free()
	return r.drain()
}

// deliver applies a result; in deterministic mode strictly in seq order,
// buffering early ones, with the clock at the result's now and never moving
// backwards.
func (r *vmRun) deliver(m hostMessage) {
	if r.clock == nil {
		if err := r.apply(m); err != nil {
			r.post(crashMessage(err))
		}
		return
	}
	if m.seq < r.nextSeq {
		r.post(crashMessage(fmt.Errorf("Result %d has sequence number %d, expected %d or later, once", m.id, m.seq, r.nextSeq)))
		return
	}
	if _, dup := r.early[m.seq]; dup {
		r.post(crashMessage(fmt.Errorf("Result %d has sequence number %d, expected %d or later, once", m.id, m.seq, r.nextSeq)))
		return
	}
	r.early[m.seq] = m
	for next, ok := r.early[r.nextSeq]; ok; next, ok = r.early[r.nextSeq] {
		delete(r.early, r.nextSeq)
		if next.hasNow && !math.IsInf(next.now, 0) && !math.IsNaN(next.now) {
			r.clock.now = math.Max(r.clock.now, next.now)
		}
		r.current = r.nextSeq
		if err := r.apply(next); err != nil {
			r.post(crashMessage(err))
			return
		}
		r.post(vmMessage{kind: "idle", seq: r.nextSeq, issued: r.issued})
		r.nextSeq++
	}
}
