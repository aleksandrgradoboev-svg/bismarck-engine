package telemetry

import (
	"errors"
	"testing"
)

// Ported from pi packages/telemetry/test/telemetry.test.ts (upstream
// 6b461b75b), NOOP_TELEMETRY_CONTEXT half; the "telemetry schemas" half tests
// the unported schema-definition machinery (see the package doc).

// TestNoopContextAdmitsSynchronouslyAndReusesOneSpan mirrors pi "admits
// callbacks synchronously and reuses one inert span": the callback has run by
// the time StartSpan returns, and a child StartSpan hands back the very same
// shared span. pi also asserts Object.isFrozen on the span; the Go noop span
// is a fieldless value, so there is no state to freeze.
func TestNoopContextAdmitsSynchronouslyAndReusesOneSpan(t *testing.T) {
	admitted := false
	var parent, child Span
	err := NoopContext.StartSpan(SpanOptions{Name: "first"}, func(span Span) error {
		admitted = true
		parent = span
		return span.StartSpan(SpanOptions{Name: "child"}, func(childSpan Span) error {
			child = childSpan
			return nil
		})
	})
	if err != nil {
		t.Fatalf("StartSpan = %v, want nil from a callback that succeeded", err)
	}
	if !admitted {
		t.Fatal("the callback must have run by the time StartSpan returns")
	}
	if parent == nil || parent != child {
		t.Fatalf("child span = %#v, want the one shared inert span (parent = %#v)", child, parent)
	}
}

// TestNoopContextPreservesCallbackErrors mirrors pi "preserves synchronous
// and asynchronous rejection values". pi surfaces a sync throw and an async
// rejection identically through the returned promise; Go folds both into the
// returned error, which must come back unchanged — not wrapped — so callers
// can match their own sentinel. A nested failure surfaces through every
// enclosing StartSpan the same way.
func TestNoopContextPreservesCallbackErrors(t *testing.T) {
	sentinel := errors.New("callback failed")
	err := NoopContext.StartSpan(SpanOptions{Name: "sync"}, func(Span) error { return sentinel })
	if err != sentinel {
		t.Fatalf("StartSpan = %v, want the callback's own error unchanged", err)
	}

	err = NoopContext.StartSpan(SpanOptions{Name: "outer"}, func(span Span) error {
		return span.StartSpan(SpanOptions{Name: "inner"}, func(Span) error { return sentinel })
	})
	if err != sentinel {
		t.Fatalf("nested StartSpan = %v, want the inner callback's error unchanged", err)
	}
}

// TestNoopContextIgnoresTelemetryPayloads mirrors pi "does not inspect or
// retain telemetry payloads". pi proves non-inspection with Proxies that
// throw on any read; Go cannot intercept reads, so this locks the observable
// half — every mutator accepts nil and populated payloads without effect —
// while the fieldless noopSpan type is the structural guarantee that nothing
// is retained.
func TestNoopContextIgnoresTelemetryPayloads(t *testing.T) {
	attributes := SpanAttributes{
		"secret": "prompt content",
		"count":  2,
		"tags":   []string{"a", "b"},
		"absent": nil,
	}
	err := NoopContext.StartSpan(SpanOptions{Name: "operation", Attributes: attributes}, func(span Span) error {
		span.AddEvent("event", attributes)
		span.AddEvent("bare", nil)
		span.SetAttributes(attributes)
		span.SetAttributes(nil)
		span.SetStatus(SpanStatus{Code: StatusOK})
		span.SetStatus(SpanStatus{Code: StatusError, Error: &SpanError{Name: "Error", Message: "boom"}})
		span.SetStatus(SpanStatus{})
		return nil
	})
	if err != nil {
		t.Fatalf("a noop span must absorb every payload, got %v", err)
	}
}
