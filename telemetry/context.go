package telemetry

import "context"

// pi's chord Context carries the current span as its telemetry member
// (always a span: NOOP_TELEMETRY_CONTEXT when none was set). Go operations
// take a context.Context, so the span travels in it as a value.

type spanKey struct{}

// WithSpan derives a context whose telemetry parent is span, without changing
// cancellation or other values (pi withTelemetry). A nil span is NoopSpan.
func WithSpan(ctx context.Context, span Span) context.Context {
	if span == nil {
		span = NoopSpan
	}
	return context.WithValue(ctx, spanKey{}, span)
}

// SpanFrom returns the context's telemetry span: the one WithSpan set, or
// NoopSpan (pi Context.telemetry).
func SpanFrom(ctx context.Context) Span {
	if ctx != nil {
		if span, ok := ctx.Value(spanKey{}).(Span); ok {
			return span
		}
	}
	return NoopSpan
}

// TraceIn runs fn in a child span of the context's span, with a derived
// context whose span is that child (pi traceIn).
func TraceIn(ctx context.Context, options SpanOptions, fn func(ctx context.Context) error) error {
	return SpanFrom(ctx).StartSpan(options, func(span Span) error {
		return fn(WithSpan(ctx, span))
	})
}
