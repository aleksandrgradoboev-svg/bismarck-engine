// Package telemetry is the Go port of pi's telemetry tracing contracts
// (@earendil-works/pi-telemetry, added by upstream 04d6447f7 and extracted to
// its own package by 6b461b75b): the Context and Span interfaces a host
// application implements to observe pi's logical operations, and the shared
// no-op context used when it supplies none.
//
// pi's startSpan<T> threads the callback's return value through the returned
// promise; Go interface methods cannot be generic, so StartSpan takes an
// error-returning callback instead — results travel out through closure
// captures, and pi's two failure paths (a synchronous throw and a rejected
// promise, both surfacing as the awaited rejection) fold into the one
// returned error.
//
// Upstream 48909d98 and 2ec55d80 (pi d2e7d8b7) add propagation, span links
// and metrics. pi hangs the optional metrics and propagation capabilities on
// TelemetryContext as properties; here they are the optional interfaces
// MetricsCapable and PropagationCapable, read through MetricsOf and
// PropagationOf, so an adapter without them still implements Context. pi's
// chord Context carries the current span as its telemetry member; in Go the
// span travels in a context.Context (WithSpan, SpanFrom, TraceIn).
//
// Schemas are ported as data (SchemaDefinition and its parts) so that a
// package's vocabulary is a value an adapter can read; the TypeScript
// inference over them (InferStartAttributes, SchemaTelemetrySpan,
// createTypedSpanStarter and friends) is compile-time typing and is not
// ported. createTypedMetrics, which also binds instrument metadata at run
// time, is NewTypedMetrics.
package telemetry

// AttributeValue is a single telemetry attribute value. pi types it as the
// closed union string | number | boolean | readonly arrays of those; Go has
// no union types, so this is `any` under a documented contract: producers
// store string, bool, an int/float kind, or a slice of those, and an
// implementation may drop anything else.
type AttributeValue any

// SpanAttributes names the attribute values on a span or event
// (pi SpanAttributes). A nil map is pi's absent `attributes`, and a nil
// value is pi's undefined entry — implementations treat both as not present.
type SpanAttributes map[string]AttributeValue

// Carrier is opaque, JSON-serializable, adapter-owned propagation data
// (pi TelemetryCarrier). A nil Carrier is pi's undefined carrier.
type Carrier map[string]string

// Propagation is the optional transport capability of an adapter
// (pi TelemetryPropagation): Inject returns the carrier of a context the
// adapter knows, or nil; Extract returns a context that parents spans under
// the carrier's span, or the adapter's root for a carrier it does not
// recognize.
type Propagation interface {
	Inject(context Context) Carrier
	Extract(carrier Carrier) Context
}

// SpanLink links a span to another context without parenting it
// (pi SpanLink).
type SpanLink struct {
	Context    Context
	Attributes SpanAttributes
}

// SpanOptions name a span being started and optionally seed its attributes
// and links (pi SpanOptions).
type SpanOptions struct {
	Name string
	// Attributes seed the span's start attributes; nil starts it bare.
	Attributes SpanAttributes
	// Links are recorded at start; a link whose context the adapter does not
	// know is dropped.
	Links []SpanLink
}

// StatusCode is the terminal disposition a span reports (the `status`
// discriminant of pi SpanStatus).
type StatusCode string

const (
	StatusOK    StatusCode = "ok"
	StatusError StatusCode = "error"
)

// SpanError identifies the failure behind an error status (pi's
// `{ name, message }`): Name classifies the failure the way a JS error name
// does — a Go caller typically uses the error's concrete type — and Message
// is its rendered text.
type SpanError struct {
	Name    string
	Message string
}

// SpanStatus is the terminal status a span reports. pi writes it as the
// union { status: "ok" } | { status: "error"; error? }; Go collapses that
// onto one struct: Error is meaningful only when Code is StatusError, and
// nil is pi's absent `error`.
type SpanStatus struct {
	Code  StatusCode
	Error *SpanError
}

// Context starts spans for whatever telemetry consumer the host application
// wired in (pi TelemetryContext). Every Span is itself a Context, so span
// parenting travels through values of this interface — start a child from
// the Span it belongs under — rather than through a context.Context, which
// StartSpan deliberately does not take: cancellation belongs to the work the
// callback closes over, not to the tracing of it.
type Context interface {
	// StartSpan opens a span, runs callback inside it, and closes the span
	// when the callback returns. The callback's error is returned to the
	// caller unchanged.
	StartSpan(options SpanOptions, callback func(span Span) error) error
}

// MetricsCapable is a Context that carries the adapter's metrics (pi's
// optional TelemetryContext.metrics). An adapter keeps it on child spans and
// extracted parents.
type MetricsCapable interface {
	Metrics() Metrics
}

// PropagationCapable is a Context that carries the adapter's propagation (pi's
// optional TelemetryContext.propagation). An adapter keeps it on child spans
// and extracted parents.
type PropagationCapable interface {
	Propagation() Propagation
}

// MetricsOf returns the metrics a context carries, or NoopMetrics.
func MetricsOf(context Context) Metrics {
	if capable, ok := context.(MetricsCapable); ok {
		if metrics := capable.Metrics(); metrics != nil {
			return metrics
		}
	}
	return NoopMetrics
}

// PropagationOf returns the propagation a context carries, or
// NoopPropagation.
func PropagationOf(context Context) Propagation {
	if capable, ok := context.(PropagationCapable); ok {
		if propagation := capable.Propagation(); propagation != nil {
			return propagation
		}
	}
	return NoopPropagation
}

// Span is a live span: a Context for starting children, plus the mutators
// that record what happened inside it (pi TelemetrySpan).
type Span interface {
	Context
	// AddLink links the span to another context after start.
	AddLink(link SpanLink)
	// AddEvent records a point-in-time event on the span; nil attributes
	// record it bare.
	AddEvent(name string, attributes SpanAttributes)
	// SetAttributes merges attributes onto the span.
	SetAttributes(attributes SpanAttributes)
	// SetStatus sets the span's terminal status.
	SetStatus(status SpanStatus)
}

// noopSpan is the one inert span behind NoopContext (pi's frozen
// noopTelemetrySpan). It is fieldless, so it can neither inspect nor retain
// what it is handed, and every child StartSpan hands back the same shared
// span.
type noopSpan struct{}

func (s noopSpan) StartSpan(_ SpanOptions, callback func(span Span) error) error {
	return callback(s)
}

func (noopSpan) AddLink(SpanLink)                {}
func (noopSpan) AddEvent(string, SpanAttributes) {}
func (noopSpan) SetAttributes(SpanAttributes)    {}
func (noopSpan) SetStatus(SpanStatus)            {}
func (noopSpan) Metrics() Metrics                { return NoopMetrics }
func (noopSpan) Propagation() Propagation        { return NoopPropagation }

// NoopSpan is the shared inert span (pi NOOP_TELEMETRY_CONTEXT, which is a
// TelemetrySpan): it admits every callback synchronously on the caller's
// goroutine, hands it itself, and discards everything else. It carries
// NoopMetrics and NoopPropagation.
var NoopSpan Span = noopSpan{}

// NoopContext is the shared telemetry context used when an application does
// not provide one (pi NOOP_TELEMETRY_CONTEXT); it is NoopSpan.
var NoopContext Context = NoopSpan

type noopPropagation struct{}

func (noopPropagation) Inject(Context) Carrier  { return nil }
func (noopPropagation) Extract(Carrier) Context { return NoopContext }

// NoopPropagation neither inspects nor retains its inputs (pi
// NOOP_TELEMETRY_PROPAGATION): Inject returns nil, Extract NoopContext.
var NoopPropagation Propagation = noopPropagation{}
