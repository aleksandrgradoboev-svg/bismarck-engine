package telemetry

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"sync"
)

// RecordedEvent is one point-in-time event captured on a recorded span
// (pi RecordedTelemetryEvent).
type RecordedEvent struct {
	Name       string
	Attributes SpanAttributes
}

// RecordedLink is a link captured on a recorded span: the linked context's
// carrier and the link attributes (pi RecordedTelemetryLink).
type RecordedLink struct {
	Context    Carrier
	Attributes SpanAttributes
}

// RecordedSpan is a detached snapshot of one span an InMemoryContext observed
// (pi RecordedTelemetrySpan).
type RecordedSpan struct {
	// ID is assigned in span-start order, from 1.
	ID int
	// ParentID is the enclosing span's ID, or 0 for a root span and for a
	// parent owned by another recorder. pi models this as `number | null`;
	// IDs start at 1, so 0 is unambiguous here.
	ParentID int
	// Context is the span's own carrier; ParentContext its parent's, or nil
	// for a root (pi null).
	Context       Carrier
	ParentContext Carrier
	Links         []RecordedLink
	Name          string
	// Attributes are the start attributes merged with every SetAttributes
	// call the span accepted.
	Attributes SpanAttributes
	Events     []RecordedEvent
	Status     SpanStatus
	Settled    bool
	// EndSequence orders settlement across spans, from 1, and is 0 while the
	// span is still open. pi omits the key entirely in that case.
	EndSequence int
}

// Measurement kinds of RecordedMeasurement.
const (
	MeasurementCounter       = "counter"
	MeasurementUpDownCounter = "upDownCounter"
	MeasurementHistogram     = "histogram"
)

// RecordedMeasurement is one measurement, unaggregated (pi
// RecordedTelemetryMeasurement). Context is the exemplar span's carrier, or
// nil (pi null).
type RecordedMeasurement struct {
	Kind        string
	Name        string
	Description string
	Unit        string
	Boundaries  []float64
	Value       float64
	Attributes  SpanAttributes
	Context     Carrier
}

// InMemoryContext is the backend-neutral reference adapter that records spans
// and measurements in process memory (pi InMemoryTelemetryContext). It is a
// Context, and its own Metrics and Propagation. Create a fresh one to isolate
// tests or independent recording scopes.
//
// Unlike pi's single-threaded original this is safe for concurrent use: Go
// callers routinely start sibling spans from several goroutines, so the
// recorder takes a mutex around its state. The lock is never held across a
// callback — a callback starting a child span would otherwise deadlock.
//
// The usable zero value is deliberate: span ids and end-sequences are derived
// from state the recorder already keeps rather than from counters a constructor
// has to seed, and the scope that carriers name is drawn at first use.
type InMemoryContext struct {
	mu    sync.Mutex
	scope string
	spans []*recordedSpan
	// settledCount is the number of spans settled so far; the next one to
	// settle takes settledCount+1 as its end-sequence.
	settledCount int
	measurements []RecordedMeasurement
}

// NewInMemoryContext returns an empty recorder. The zero value is equally
// usable; this exists for symmetry with the rest of the package.
func NewInMemoryContext() *InMemoryContext { return &InMemoryContext{} }

// Carrier keys of the in-memory adapter.
const (
	memoryScopeKey = "pi-memory-scope"
	memorySpanKey  = "pi-memory-span"
)

var (
	memoryScopePattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	memorySpanPattern  = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, the bound pi's
// extract puts on a span ID.
const maxSafeInteger = 1<<53 - 1

// recordedSpan is the mutable state behind a live span
// (pi MutableRecordedTelemetrySpan). carrier and parentCarrier are fixed at
// creation and read without the lock.
type recordedSpan struct {
	id             int
	parentID       int
	carrier        Carrier
	parentCarrier  Carrier
	links          []RecordedLink
	name           string
	attributes     SpanAttributes
	events         []RecordedEvent
	status         SpanStatus
	explicitStatus bool
	settled        bool
	endSequence    int
}

// memoryParent is what a child needs of its parent (pi InMemoryParent): the
// parent's carrier, its local ID when this recorder knows it, and its live
// state when it is a span here.
type memoryParent struct {
	carrier Carrier
	localID int
	span    *recordedSpan
}

// carrierHolder is a context whose identity this adapter knows: a recorded
// span or an extracted parent, settled or not (pi's contextCarriers WeakMap,
// which is shared by every recorder).
type carrierHolder interface {
	memoryCarrier() Carrier
}

func newMemoryScope() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (c *InMemoryContext) scopeLocked() string {
	if c.scope == "" {
		c.scope = newMemoryScope()
	}
	return c.scope
}

// StartSpan opens a root span on this recorder.
func (c *InMemoryContext) StartSpan(options SpanOptions, callback func(span Span) error) error {
	return c.startSpan(nil, options, callback)
}

// Metrics returns the recorder itself.
func (c *InMemoryContext) Metrics() Metrics { return c }

// Propagation returns the recorder itself.
func (c *InMemoryContext) Propagation() Propagation { return c }

// Inject returns a detached carrier for a span or extracted parent this
// adapter knows, settled or not; otherwise nil.
func (c *InMemoryContext) Inject(context Context) Carrier {
	if holder, ok := context.(carrierHolder); ok {
		return holder.memoryCarrier()
	}
	return nil
}

// Extract returns a parent for a carrier of this adapter's format. An
// unrecognized or malformed carrier falls back to this recorder's root.
func (c *InMemoryContext) Extract(carrier Carrier) Context {
	scope, hasScope := carrier[memoryScopeKey]
	span, hasSpan := carrier[memorySpanKey]
	if !hasScope || !hasSpan || !memoryScopePattern.MatchString(scope) || !memorySpanPattern.MatchString(span) {
		return c
	}
	id, err := strconv.ParseInt(span, 10, 64)
	if err != nil || id > maxSafeInteger {
		return c
	}
	parent := &extractedParent{ctx: c, carrier: Carrier{memoryScopeKey: scope, memorySpanKey: span}}
	c.mu.Lock()
	if scope == c.scope && int(id) <= len(c.spans) {
		parent.localID = int(id)
	}
	c.mu.Unlock()
	return parent
}

// Spans returns detached snapshots in span-start order. Mutating the result
// cannot reach the recorder's state.
func (c *InMemoryContext) Spans() []RecordedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RecordedSpan, 0, len(c.spans))
	for _, s := range c.spans {
		events := make([]RecordedEvent, 0, len(s.events))
		for _, e := range s.events {
			events = append(events, RecordedEvent{Name: e.Name, Attributes: copyAttributes(e.Attributes)})
		}
		links := make([]RecordedLink, 0, len(s.links))
		for _, l := range s.links {
			links = append(links, RecordedLink{Context: copyCarrier(l.Context), Attributes: copyAttributes(l.Attributes)})
		}
		out = append(out, RecordedSpan{
			ID:            s.id,
			ParentID:      s.parentID,
			Context:       copyCarrier(s.carrier),
			ParentContext: copyCarrier(s.parentCarrier),
			Links:         links,
			Name:          s.name,
			Attributes:    copyAttributes(s.attributes),
			Events:        events,
			Status:        copyStatus(s.status),
			Settled:       s.settled,
			EndSequence:   s.endSequence,
		})
	}
	return out
}

// Measurements returns detached measurements in recording order, without
// aggregation (pi getMetrics; Metrics is the capability here).
func (c *InMemoryContext) Measurements() []RecordedMeasurement {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RecordedMeasurement, 0, len(c.measurements))
	for _, m := range c.measurements {
		m.Boundaries = copyBoundaries(m.Boundaries)
		m.Attributes = copyAttributes(m.Attributes)
		m.Context = copyCarrier(m.Context)
		out = append(out, m)
	}
	return out
}

func (c *InMemoryContext) startSpan(parent *memoryParent, options SpanOptions, callback func(span Span) error) error {
	// pi: a child started under an already-settled parent is not recorded; it
	// runs on an inert span that keeps the adapter's capabilities. The check
	// and the append share one critical section -- splitting them lets the
	// parent settle in between and a child still be recorded under it.
	c.mu.Lock()
	if parent != nil && parent.span != nil && parent.span.settled {
		c.mu.Unlock()
		return c.startInert(callback)
	}
	id := len(c.spans) + 1
	span := &recordedSpan{
		id:         id,
		carrier:    Carrier{memoryScopeKey: c.scopeLocked(), memorySpanKey: strconv.Itoa(id)},
		links:      []RecordedLink{},
		name:       options.Name,
		attributes: copyAttributes(options.Attributes),
		status:     SpanStatus{Code: StatusOK},
	}
	for _, link := range options.Links {
		if recorded, ok := copyLink(link); ok {
			span.links = append(span.links, recorded)
		}
	}
	if parent != nil {
		span.parentID = parent.localID
		span.parentCarrier = copyCarrier(parent.carrier)
	}
	c.spans = append(c.spans, span)
	c.mu.Unlock()

	live := &inMemorySpan{ctx: c, span: span}

	// pi settles the span on a synchronous throw and rethrows. Go's analog is
	// a panic: settle it as a failure, then let it keep unwinding. settle is
	// idempotent, so the normal path below is unaffected.
	defer func() {
		if r := recover(); r != nil {
			c.settle(span, true, panicError{value: r})
			panic(r)
		}
	}()

	err := callback(live)
	c.settle(span, err != nil, err)
	return err
}

// startInert runs callback on an inert span (pi startInertSpan): nothing is
// recorded, children are inert too, and the capabilities are this adapter's.
func (c *InMemoryContext) startInert(callback func(span Span) error) error {
	return callback(inertSpan{ctx: c})
}

// settle closes a span exactly once, stamping the end-sequence that orders
// settlement across spans (pi settleSpan).
func (c *InMemoryContext) settle(span *recordedSpan, failed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if span.settled {
		return
	}
	if failed && !span.explicitStatus {
		span.status = automaticErrorStatus(err)
	}
	span.settled = true
	c.settledCount++
	span.endSequence = c.settledCount
}

// Counter returns a counter recording into this adapter; it drops negative
// and non-finite values.
func (c *InMemoryContext) Counter(name string, options InstrumentOptions) Counter {
	return memoryAdder{record: c.recorder(MeasurementCounter, name, options.Description, options.Unit, nil, false)}
}

// UpDownCounter returns an up-down counter recording into this adapter; it
// drops non-finite values.
func (c *InMemoryContext) UpDownCounter(name string, options InstrumentOptions) UpDownCounter {
	return memoryAdder{record: c.recorder(MeasurementUpDownCounter, name, options.Description, options.Unit, nil, false)}
}

// Histogram returns a histogram recording into this adapter. Boundaries that
// are not finite and strictly increasing make the instrument inert.
func (c *InMemoryContext) Histogram(name string, options HistogramOptions) Histogram {
	return memoryRecorder{record: c.recorder(MeasurementHistogram, name, options.Description, options.Unit, options.Boundaries, options.Boundaries != nil)}
}

type measure func(value float64, attributes SpanAttributes, context Context)

// recorder returns the measuring function of one instrument (pi
// createMeasurementRecorder). Metadata is captured now, so later changes to
// the caller's options do not reach it; invalid boundaries make it inert.
func (c *InMemoryContext) recorder(kind, name, description, unit string, boundaries []float64, hasBoundaries bool) measure {
	var captured []float64
	if hasBoundaries {
		for i, value := range boundaries {
			if math.IsNaN(value) || math.IsInf(value, 0) || (i > 0 && value <= boundaries[i-1]) {
				return func(float64, SpanAttributes, Context) {}
			}
		}
		captured = append([]float64{}, boundaries...)
	}
	return func(value float64, attributes SpanAttributes, context Context) {
		if math.IsNaN(value) || math.IsInf(value, 0) || (kind == MeasurementCounter && value < 0) {
			return
		}
		measurement := RecordedMeasurement{
			Kind:        kind,
			Name:        name,
			Description: description,
			Unit:        unit,
			Boundaries:  copyBoundaries(captured),
			Value:       value,
			Attributes:  copyAttributes(attributes),
		}
		if holder, ok := context.(carrierHolder); ok {
			measurement.Context = holder.memoryCarrier()
		}
		c.mu.Lock()
		c.measurements = append(c.measurements, measurement)
		c.mu.Unlock()
	}
}

type memoryAdder struct{ record measure }

func (a memoryAdder) Add(value float64, attributes SpanAttributes) { a.record(value, attributes, nil) }

type memoryRecorder struct{ record measure }

func (r memoryRecorder) Record(value float64, attributes SpanAttributes, context Context) {
	r.record(value, attributes, context)
}

// panicError carries a recovered panic into the status derivation so a panic
// is distinguishable from an ordinary returned error, which %T alone cannot do
// (both collapse to *errors.errorString for most stdlib errors).
type panicError struct{ value any }

func (e panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

// automaticErrorStatus derives the status of a span that failed without one
// being set explicitly. pi reads a JS Error's name and message; Go's nearest
// equivalent of `error.name` is the error's concrete type, which carries real
// signal only for typed errors.
func automaticErrorStatus(err error) SpanStatus {
	// Unreachable today -- both callers pass a non-nil error -- but kept so a
	// future caller cannot turn a nil into a nil-deref inside err.Error().
	if err == nil {
		return SpanStatus{Code: StatusError}
	}
	var pe panicError
	if errors.As(err, &pe) {
		return SpanStatus{
			Code:  StatusError,
			Error: &SpanError{Name: "panic", Message: fmt.Sprint(pe.value)},
		}
	}
	return SpanStatus{
		Code:  StatusError,
		Error: &SpanError{Name: fmt.Sprintf("%T", err), Message: err.Error()},
	}
}

// inMemorySpan is the live Span handed to a callback.
type inMemorySpan struct {
	ctx  *InMemoryContext
	span *recordedSpan
}

func (s *inMemorySpan) StartSpan(options SpanOptions, callback func(span Span) error) error {
	return s.ctx.startSpan(&memoryParent{carrier: s.span.carrier, localID: s.span.id, span: s.span}, options, callback)
}

func (s *inMemorySpan) Metrics() Metrics         { return s.ctx }
func (s *inMemorySpan) Propagation() Propagation { return s.ctx }
func (s *inMemorySpan) memoryCarrier() Carrier   { return copyCarrier(s.span.carrier) }

// Recording is passive: every mutator below is a no-op once the span settled,
// matching pi's guards.

func (s *inMemorySpan) AddLink(link SpanLink) {
	recorded, ok := copyLink(link)
	if !ok {
		return
	}
	s.ctx.mu.Lock()
	defer s.ctx.mu.Unlock()
	if s.span.settled {
		return
	}
	s.span.links = append(s.span.links, recorded)
}

func (s *inMemorySpan) AddEvent(name string, attributes SpanAttributes) {
	s.ctx.mu.Lock()
	defer s.ctx.mu.Unlock()
	if s.span.settled {
		return
	}
	s.span.events = append(s.span.events, RecordedEvent{Name: name, Attributes: copyAttributes(attributes)})
}

func (s *inMemorySpan) SetAttributes(attributes SpanAttributes) {
	s.ctx.mu.Lock()
	defer s.ctx.mu.Unlock()
	if s.span.settled {
		return
	}
	s.span.attributes = mergeAttributes(s.span.attributes, attributes)
}

func (s *inMemorySpan) SetStatus(status SpanStatus) {
	s.ctx.mu.Lock()
	defer s.ctx.mu.Unlock()
	if s.span.settled {
		return
	}
	s.span.status = copyStatus(status)
	s.span.explicitStatus = true
}

// inertSpan records nothing and keeps the adapter's capabilities; it is not
// a context the adapter can inject.
type inertSpan struct{ ctx *InMemoryContext }

func (s inertSpan) StartSpan(_ SpanOptions, callback func(span Span) error) error {
	return s.ctx.startInert(callback)
}

func (inertSpan) AddLink(SpanLink)                {}
func (inertSpan) AddEvent(string, SpanAttributes) {}
func (inertSpan) SetAttributes(SpanAttributes)    {}
func (inertSpan) SetStatus(SpanStatus)            {}
func (s inertSpan) Metrics() Metrics              { return s.ctx }
func (s inertSpan) Propagation() Propagation      { return s.ctx }

// extractedParent is a context Extract returned: children are recorded under
// its carrier, and under its local span when this recorder started it.
type extractedParent struct {
	ctx     *InMemoryContext
	carrier Carrier
	localID int
}

func (p *extractedParent) StartSpan(options SpanOptions, callback func(span Span) error) error {
	return p.ctx.startSpan(&memoryParent{carrier: p.carrier, localID: p.localID}, options, callback)
}

func (p *extractedParent) Metrics() Metrics         { return p.ctx }
func (p *extractedParent) Propagation() Propagation { return p.ctx }
func (p *extractedParent) memoryCarrier() Carrier   { return copyCarrier(p.carrier) }

// copyLink records a link to a context the adapter knows; any other link is
// dropped (pi copyLink).
func copyLink(link SpanLink) (RecordedLink, bool) {
	holder, ok := link.Context.(carrierHolder)
	if !ok {
		return RecordedLink{}, false
	}
	return RecordedLink{Context: holder.memoryCarrier(), Attributes: copyAttributes(link.Attributes)}, true
}

func copyCarrier(carrier Carrier) Carrier {
	if carrier == nil {
		return nil
	}
	copied := make(Carrier, len(carrier))
	for key, value := range carrier {
		copied[key] = value
	}
	return copied
}

func copyBoundaries(boundaries []float64) []float64 {
	if boundaries == nil {
		return nil
	}
	return append([]float64{}, boundaries...)
}

// copyAttributes detaches a caller's attribute map. Per the SpanAttributes
// contract a nil value is pi's `undefined` entry, so it is dropped rather
// than recorded.
func copyAttributes(attributes SpanAttributes) SpanAttributes {
	copied := SpanAttributes{}
	for name, value := range attributes {
		if value == nil {
			continue
		}
		copied[name] = copyAttributeValue(value)
	}
	return copied
}

func mergeAttributes(current, attributes SpanAttributes) SpanAttributes {
	merged := copyAttributes(current)
	for name, value := range attributes {
		if value == nil {
			continue
		}
		merged[name] = copyAttributeValue(value)
	}
	return merged
}

// copyAttributeValue clones slice values so a caller mutating the slice it
// passed cannot reach into recorded state (pi's `[...value]`). Scalars are
// already values in Go and pass through.
func copyAttributeValue(value AttributeValue) AttributeValue {
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice || rv.IsNil() {
		return value
	}
	clone := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
	reflect.Copy(clone, rv)
	return clone.Interface()
}

func copyStatus(status SpanStatus) SpanStatus {
	if status.Code != StatusError {
		return SpanStatus{Code: StatusOK}
	}
	if status.Error == nil {
		return SpanStatus{Code: StatusError}
	}
	return SpanStatus{
		Code:  StatusError,
		Error: &SpanError{Name: status.Error.Name, Message: status.Error.Message},
	}
}
