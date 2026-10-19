// Package telemetrytest is the adapter conformance suite of package telemetry
// (pi packages/telemetry/src/testing, createTelemetryAdapterConformance).
//
// pi's unreadable payloads — proxies that throw when read — have no Go
// counterpart, as reading a Go value cannot fail; the cases built on them
// ("ignores failed attribute calls atomically", "suppresses unreadable
// telemetry payload failures", "ignores failed status calls atomically") are
// n/a, and so are their parts in the other cases. pi's synchronous throw and
// asynchronous rejection fold into one returned error; a panic stands in for
// the throw where the adapter must settle and pass it on.
package telemetrytest

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Fixture is a fresh adapter and its normalized readers, owned by one case
// (pi TelemetryAdapterFixture). Propagation and Metrics are nil when the
// adapter lacks them, and Measurements is required with Metrics.
type Fixture struct {
	Context      telemetry.Context
	Propagation  telemetry.Propagation
	Metrics      telemetry.Metrics
	Measurements func() []telemetry.RecordedMeasurement
	Spans        func() []telemetry.RecordedSpan
}

// Factory creates an isolated fixture for one case; register cleanup with
// t.Cleanup.
type Factory func(t *testing.T) Fixture

type conformanceCase struct {
	group, name string
	run         func(t *testing.T, factory Factory, fixture Fixture)
}

// TestAdapter runs every conformance case against fresh fixtures, as
// subtests named group/case.
func TestAdapter(t *testing.T, factory Factory) {
	for _, c := range cases {
		t.Run(c.group+"/"+c.name, func(t *testing.T) { c.run(t, factory, factory(t)) })
	}
}

func findSpan(t *testing.T, spans []telemetry.RecordedSpan, name string) telemetry.RecordedSpan {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("expected recorded span %s", name)
	return telemetry.RecordedSpan{}
}

func roundtrip(t *testing.T, carrier telemetry.Carrier) telemetry.Carrier {
	t.Helper()
	raw, err := json.Marshal(carrier)
	if err != nil {
		t.Fatal(err)
	}
	var wire telemetry.Carrier
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", what, got, want)
	}
}

func metricsOf(f Fixture) telemetry.Metrics {
	if f.Metrics != nil {
		return f.Metrics
	}
	return telemetry.MetricsOf(f.Context)
}

func propagationOf(f Fixture) telemetry.Propagation {
	if f.Propagation != nil {
		return f.Propagation
	}
	return telemetry.PropagationOf(f.Context)
}

func requireMeasurements(t *testing.T, f Fixture) {
	t.Helper()
	if f.Measurements == nil {
		t.Fatal("metrics fixtures require a normalized measurement reader")
	}
}

var cases = []conformanceCase{
	{"capabilities", "carries adapter capabilities through contexts and spans", func(t *testing.T, _ Factory, f Fixture) {
		metrics, propagation := metricsOf(f), propagationOf(f)
		check := func(context telemetry.Context) {
			t.Helper()
			if telemetry.MetricsOf(context) != metrics || telemetry.PropagationOf(context) != propagation {
				t.Fatalf("context %T does not carry the adapter's capabilities", context)
			}
		}
		check(f.Context)
		var settled telemetry.Span
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "capabilities"}, func(span telemetry.Span) error {
			settled = span
			check(span)
			must(t, span.StartSpan(telemetry.SpanOptions{Name: "capabilities.child"}, func(child telemetry.Span) error { check(child); return nil }))
			if f.Propagation != nil {
				parent := f.Propagation.Extract(f.Propagation.Inject(span))
				check(parent)
				must(t, parent.StartSpan(telemetry.SpanOptions{Name: "capabilities.extracted"}, func(child telemetry.Span) error { check(child); return nil }))
			}
			return nil
		}))
		must(t, settled.StartSpan(telemetry.SpanOptions{Name: "capabilities.inert"}, func(child telemetry.Span) error { check(child); return nil }))
	}},

	{"metrics", "records ordered measurements and optional histogram exemplars", func(t *testing.T, _ Factory, f Fixture) {
		if f.Metrics == nil {
			return
		}
		requireMeasurements(t, f)
		f.Metrics.Counter("bytes", telemetry.InstrumentOptions{Description: "Encoded bytes", Unit: "By"}).Add(12, telemetry.SpanAttributes{"direction": "out"})
		active := f.Metrics.UpDownCounter("active", telemetry.InstrumentOptions{})
		active.Add(1, nil)
		active.Add(-1, nil)
		histogram := f.Metrics.Histogram("duration", telemetry.HistogramOptions{InstrumentOptions: telemetry.InstrumentOptions{Unit: "ms"}, Boundaries: []float64{1, 10}})
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "measured"}, func(span telemetry.Span) error {
			histogram.Record(5, telemetry.SpanAttributes{"method": "read"}, span)
			return nil
		}))
		measurements := f.Measurements()
		var got [][3]any
		for _, m := range measurements {
			got = append(got, [3]any{m.Kind, m.Name, m.Value})
		}
		equal(t, "measurements", got, [][3]any{
			{telemetry.MeasurementCounter, "bytes", 12.0},
			{telemetry.MeasurementUpDownCounter, "active", 1.0},
			{telemetry.MeasurementUpDownCounter, "active", -1.0},
			{telemetry.MeasurementHistogram, "duration", 5.0},
		})
		equal(t, "counter attributes", measurements[0].Attributes, telemetry.SpanAttributes{"direction": "out"})
		equal(t, "counter description", measurements[0].Description, "Encoded bytes")
		equal(t, "counter unit", measurements[0].Unit, "By")
		equal(t, "histogram boundaries", measurements[3].Boundaries, []float64{1, 10})
		equal(t, "exemplar", measurements[3].Context, findSpan(t, f.Spans(), "measured").Context)
	}},

	// pi also changes the options object after creation; Go options are
	// values, so what applies is the boundary slice and the attribute values.
	{"metrics", "copies inputs and suppresses invalid measurements atomically", func(t *testing.T, _ Factory, f Fixture) {
		if f.Metrics == nil {
			return
		}
		requireMeasurements(t, f)
		boundaries := []float64{1, 10}
		histogram := f.Metrics.Histogram("snapshot", telemetry.HistogramOptions{InstrumentOptions: telemetry.InstrumentOptions{Description: "Original"}, Boundaries: boundaries})
		boundaries[1] = 100
		tags := []string{"original"}
		histogram.Record(-1, telemetry.SpanAttributes{"tags": tags}, nil)
		tags[0] = "changed"
		counter := f.Metrics.Counter("invalid", telemetry.InstrumentOptions{})
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
			counter.Add(value, nil)
		}
		f.Metrics.Histogram("bad-boundaries", telemetry.HistogramOptions{Boundaries: []float64{2, 1}}).Record(1, nil, nil)
		measurements := f.Measurements()
		if len(measurements) != 1 {
			t.Fatalf("measurements = %d, want 1: %#v", len(measurements), measurements)
		}
		equal(t, "attributes", measurements[0].Attributes, telemetry.SpanAttributes{"tags": []string{"original"}})
		equal(t, "boundaries", measurements[0].Boundaries, []float64{1, 10})
		equal(t, "description", measurements[0].Description, "Original")
		if measurements[0].Context != nil {
			t.Fatalf("context = %v, want nil", measurements[0].Context)
		}
	}},

	{"links", "records initial and late links without changing parents", func(t *testing.T, _ Factory, f Fixture) {
		var target telemetry.Span
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "target"}, func(span telemetry.Span) error { target = span; return nil }))
		reason := []string{"initial"}
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "linked", Links: []telemetry.SpanLink{{Context: target, Attributes: telemetry.SpanAttributes{"reason": reason}}}}, func(span telemetry.Span) error {
			reason[0] = "mutated"
			span.AddLink(telemetry.SpanLink{Context: target, Attributes: telemetry.SpanAttributes{"reason": "late"}})
			span.AddLink(telemetry.SpanLink{Context: telemetry.NoopContext})
			return nil
		}))
		spans := f.Spans()
		linked := findSpan(t, spans, "linked")
		if linked.ParentID != 0 || linked.ParentContext != nil {
			t.Fatalf("linked parent = %d %v, want a root", linked.ParentID, linked.ParentContext)
		}
		targetContext := findSpan(t, spans, "target").Context
		equal(t, "links", linked.Links, []telemetry.RecordedLink{
			{Context: targetContext, Attributes: telemetry.SpanAttributes{"reason": []string{"initial"}}},
			{Context: targetContext, Attributes: telemetry.SpanAttributes{"reason": "late"}},
		})
	}},

	// pi's malformed links are unreadable objects; in Go a link is malformed
	// when it has no context.
	{"links", "suppresses malformed links and ignores settled sources", func(t *testing.T, _ Factory, f Fixture) {
		var source telemetry.Span
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "link-source"}, func(span telemetry.Span) error {
			source = span
			span.AddLink(telemetry.SpanLink{Attributes: telemetry.SpanAttributes{"partial": true}})
			return nil
		}))
		source.AddLink(telemetry.SpanLink{Context: source})
		equal(t, "links", findSpan(t, f.Spans(), "link-source").Links, []telemetry.RecordedLink{})
	}},

	{"propagation", "roundtrips parent identity through JSON without retaining the carrier", func(t *testing.T, _ Factory, f Fixture) {
		if f.Propagation == nil {
			return
		}
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "sender"}, func(span telemetry.Span) error {
			carrier := f.Propagation.Inject(span)
			if carrier == nil {
				t.Fatal("no carrier for a recorded span")
			}
			wire := roundtrip(t, carrier)
			parent := f.Propagation.Extract(wire)
			for key := range wire {
				wire[key] = "mutated"
			}
			must(t, parent.StartSpan(telemetry.SpanOptions{Name: "receiver"}, func(telemetry.Span) error { return nil }))
			equal(t, "carrier after the roundtrip", f.Propagation.Inject(span), carrier)
			return nil
		}))
		spans := f.Spans()
		equal(t, "receiver parent", findSpan(t, spans, "receiver").ParentContext, findSpan(t, spans, "sender").Context)
	}},

	{"propagation", "unknown carriers produce recorded roots", func(t *testing.T, _ Factory, f Fixture) {
		if f.Propagation == nil {
			return
		}
		if carrier := f.Propagation.Inject(telemetry.NoopContext); carrier != nil {
			t.Fatalf("carrier of the no-op context = %v, want nil", carrier)
		}
		for index, carrier := range []telemetry.Carrier{nil, {}, {"unknown": "format"}, {"bad": "value"}} {
			result := 0
			must(t, f.Propagation.Extract(carrier).StartSpan(telemetry.SpanOptions{Name: "fallback"}, func(telemetry.Span) error { result = 42; return nil }))
			if result != 42 {
				t.Fatalf("carrier %d: callback not admitted", index)
			}
		}
		for _, span := range f.Spans() {
			if span.ParentContext != nil {
				t.Fatalf("span %s has parent %v, want a root", span.Name, span.ParentContext)
			}
		}
	}},

	{"propagation", "propagates across fresh adapters and multiple hops", func(t *testing.T, factory Factory, f Fixture) {
		if f.Propagation == nil {
			return
		}
		receiver, worker := factory(t), factory(t)
		if receiver.Propagation == nil || worker.Propagation == nil {
			t.Fatal("expected propagation on each fixture")
		}
		var senderCarrier, receiverCarrier telemetry.Carrier
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "sender"}, func(span telemetry.Span) error {
			senderCarrier = f.Propagation.Inject(span)
			return nil
		}))
		if senderCarrier == nil {
			t.Fatal("no sender carrier")
		}
		must(t, receiver.Propagation.Extract(roundtrip(t, senderCarrier)).StartSpan(telemetry.SpanOptions{Name: "receiver"}, func(span telemetry.Span) error {
			receiverCarrier = receiver.Propagation.Inject(span)
			return nil
		}))
		if receiverCarrier == nil {
			t.Fatal("no receiver carrier")
		}
		must(t, worker.Propagation.Extract(roundtrip(t, receiverCarrier)).StartSpan(telemetry.SpanOptions{Name: "worker"}, func(telemetry.Span) error { return nil }))
		equal(t, "receiver parent", findSpan(t, receiver.Spans(), "receiver").ParentContext, senderCarrier)
		equal(t, "worker parent", findSpan(t, worker.Spans(), "worker").ParentContext, receiverCarrier)
	}},

	{"callback lifecycle", "admits once synchronously and preserves the result", func(t *testing.T, _ Factory, f Fixture) {
		calls := 0
		result := 0
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "success"}, func(telemetry.Span) error {
			calls++
			result = 42
			return nil
		}))
		if calls != 1 || result != 42 {
			t.Fatalf("calls = %d, result = %d", calls, result)
		}
		span := findSpan(t, f.Spans(), "success")
		equal(t, "status", span.Status, telemetry.SpanStatus{Code: telemetry.StatusOK})
		if !span.Settled {
			t.Fatal("span not settled")
		}
	}},

	{"callback lifecycle", "preserves returned errors and panics", func(t *testing.T, _ Factory, f Fixture) {
		returned := errors.New("returned")
		if err := f.Context.StartSpan(telemetry.SpanOptions{Name: "returned-error"}, func(telemetry.Span) error { return returned }); err != returned {
			t.Fatalf("error = %v, want the callback's own", err)
		}
		panicked := &struct{ kind string }{"panic"}
		func() {
			defer func() {
				if r := recover(); r != panicked {
					t.Fatalf("recovered %v, want the callback's own panic", r)
				}
			}()
			_ = f.Context.StartSpan(telemetry.SpanOptions{Name: "panicked"}, func(telemetry.Span) error { panic(panicked) })
		}()
		spans := f.Spans()
		for _, name := range []string{"returned-error", "panicked"} {
			if got := findSpan(t, spans, name).Status.Code; got != telemetry.StatusError {
				t.Fatalf("%s status = %s, want error", name, got)
			}
		}
	}},

	{"status", "uses last explicit status without automatic overwrite", func(t *testing.T, _ Factory, f Fixture) {
		expected := func(message string) telemetry.SpanStatus {
			return telemetry.SpanStatus{Code: telemetry.StatusError, Error: &telemetry.SpanError{Name: "Expected", Message: message}}
		}
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "last-status"}, func(span telemetry.Span) error {
			span.SetStatus(expected("first"))
			span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
			return nil
		}))
		thrown := errors.New("after explicit status")
		if err := f.Context.StartSpan(telemetry.SpanOptions{Name: "explicit-before-error"}, func(span telemetry.Span) error {
			span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
			return thrown
		}); err != thrown {
			t.Fatalf("error = %v", err)
		}
		rejected := errors.New("after explicit error status")
		if err := f.Context.StartSpan(telemetry.SpanOptions{Name: "explicit-error-before-error"}, func(span telemetry.Span) error {
			span.SetStatus(expected("async failure"))
			return rejected
		}); err != rejected {
			t.Fatalf("error = %v", err)
		}
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "expected-failure"}, func(span telemetry.Span) error {
			span.SetStatus(expected("returned failure"))
			return nil
		}))
		spans := f.Spans()
		equal(t, "last-status", findSpan(t, spans, "last-status").Status, telemetry.SpanStatus{Code: telemetry.StatusOK})
		equal(t, "explicit-before-error", findSpan(t, spans, "explicit-before-error").Status, telemetry.SpanStatus{Code: telemetry.StatusOK})
		equal(t, "explicit-error-before-error", findSpan(t, spans, "explicit-error-before-error").Status, expected("async failure"))
		equal(t, "expected-failure", findSpan(t, spans, "expected-failure").Status, expected("returned failure"))
	}},

	{"recording", "merges attributes and records ordered events", func(t *testing.T, _ Factory, f Fixture) {
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "recording", Attributes: telemetry.SpanAttributes{"start": "value", "overwrite": "start", "ignored": nil}}, func(span telemetry.Span) error {
			span.SetAttributes(telemetry.SpanAttributes{"count": 1, "overwrite": "middle"})
			span.SetAttributes(telemetry.SpanAttributes{"count": nil, "overwrite": "end"})
			span.AddEvent("first", telemetry.SpanAttributes{"index": 1, "ignored": nil})
			span.AddEvent("second", telemetry.SpanAttributes{"index": 2})
			return nil
		}))
		span := findSpan(t, f.Spans(), "recording")
		equal(t, "attributes", span.Attributes, telemetry.SpanAttributes{"start": "value", "overwrite": "end", "count": 1})
		equal(t, "events", span.Events, []telemetry.RecordedEvent{
			{Name: "first", Attributes: telemetry.SpanAttributes{"index": 1}},
			{Name: "second", Attributes: telemetry.SpanAttributes{"index": 2}},
		})
	}},

	{"recording", "makes calls after settlement inert", func(t *testing.T, _ Factory, f Fixture) {
		var settled telemetry.Span
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "settled", Attributes: telemetry.SpanAttributes{"value": "initial"}}, func(span telemetry.Span) error {
			settled = span
			return nil
		}))
		settled.SetAttributes(telemetry.SpanAttributes{"value": "late"})
		settled.AddEvent("late", telemetry.SpanAttributes{"value": true})
		settled.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusError})
		admitted, result := false, 0
		must(t, settled.StartSpan(telemetry.SpanOptions{Name: "late-child"}, func(telemetry.Span) error {
			admitted, result = true, 7
			return nil
		}))
		if !admitted || result != 7 {
			t.Fatal("late child not admitted")
		}
		spans := f.Spans()
		if len(spans) != 1 {
			t.Fatalf("spans = %d, want 1", len(spans))
		}
		equal(t, "attributes", spans[0].Attributes, telemetry.SpanAttributes{"value": "initial"})
		equal(t, "events", spans[0].Events, []telemetry.RecordedEvent{})
		equal(t, "status", spans[0].Status, telemetry.SpanStatus{Code: telemetry.StatusOK})
	}},

	{"parentage", "records nested and concurrent child relationships", func(t *testing.T, _ Factory, f Fixture) {
		release := make(chan struct{})
		must(t, f.Context.StartSpan(telemetry.SpanOptions{Name: "parent"}, func(parent telemetry.Span) error {
			var wg sync.WaitGroup
			started := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = parent.StartSpan(telemetry.SpanOptions{Name: "first-child"}, func(telemetry.Span) error {
					close(started)
					<-release
					return nil
				})
			}()
			<-started
			must(t, parent.StartSpan(telemetry.SpanOptions{Name: "second-child"}, func(telemetry.Span) error { return nil }))
			close(release)
			wg.Wait()
			return nil
		}))
		spans := f.Spans()
		parent, first, second := findSpan(t, spans, "parent"), findSpan(t, spans, "first-child"), findSpan(t, spans, "second-child")
		if parent.ParentID != 0 || first.ParentID != parent.ID || second.ParentID != parent.ID {
			t.Fatalf("parents: %d %d %d (parent %d)", parent.ParentID, first.ParentID, second.ParentID, parent.ID)
		}
		if second.EndSequence == 0 || !(second.EndSequence < first.EndSequence && first.EndSequence < parent.EndSequence) {
			t.Fatalf("end sequences: second %d, first %d, parent %d", second.EndSequence, first.EndSequence, parent.EndSequence)
		}
	}},
}
