package telemetry_test

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Ported from pi packages/telemetry/test/metrics.test.ts. pi's compile-time
// checks of names, kinds and measurement attributes are n/a: Go has no such
// typing. What applies is the run-time binding of metadata, the rejected
// duplicate, and the recorder's handling of measurements.

var trafficSchema = telemetry.SchemaDefinition{
	Version: 1,
	Spans:   map[string]telemetry.SpanDefinition{},
	Metrics: map[string]telemetry.MetricDefinition{
		"network.bytes": {
			Kind: telemetry.MetricCounter, Description: "Encoded bytes", Unit: "By",
			Attributes: map[string]telemetry.AttributeDefinition{
				"direction": {Type: telemetry.AttributeString, Values: []any{"in", "out"}, Required: true, Description: "Direction", Cardinality: telemetry.CardinalityLow},
			},
		},
		"network.active": {Kind: telemetry.MetricUpDownCounter, Description: "Open links", Attributes: map[string]telemetry.AttributeDefinition{}},
	},
}

var latencySchema = telemetry.SchemaDefinition{
	Version: 1,
	Spans:   map[string]telemetry.SpanDefinition{},
	Metrics: map[string]telemetry.MetricDefinition{
		"call.duration": {
			Kind: telemetry.MetricHistogram, Description: "Call duration", Unit: "ms", Boundaries: []float64{1, 10, 100},
			Attributes: map[string]telemetry.AttributeDefinition{"method": {Type: telemetry.AttributeString, Description: "Method"}},
		},
	},
}

func TestMetricsBindSchemaNamesAndMetadata(t *testing.T) {
	adapter := telemetry.NewInMemoryContext()
	metrics, err := telemetry.NewTypedMetrics(adapter, trafficSchema, latencySchema)
	must(t, err)
	metrics.Counter("network.bytes").Add(128, telemetry.SpanAttributes{"direction": "out"})
	metrics.UpDownCounter("network.active").Add(1, nil)
	metrics.UpDownCounter("network.active").Add(-1, nil)
	must(t, adapter.StartSpan(telemetry.SpanOptions{Name: "request"}, func(span telemetry.Span) error {
		metrics.Histogram("call.duration").Record(12, telemetry.SpanAttributes{"method": "read"}, span)
		metrics.Histogram("call.duration").Record(13, nil, nil)
		metrics.Histogram("call.duration").Record(14, nil, span)
		return nil
	}))
	measurements := adapter.Measurements()
	counter := measurements[0]
	if counter.Kind != telemetry.MeasurementCounter || counter.Name != "network.bytes" || counter.Description != "Encoded bytes" || counter.Unit != "By" || counter.Value != 128 ||
		!reflect.DeepEqual(counter.Attributes, telemetry.SpanAttributes{"direction": "out"}) {
		t.Fatalf("counter = %+v", counter)
	}
	request := adapter.Spans()[0].Context
	if m := measurements[3]; m.Kind != telemetry.MeasurementHistogram || !reflect.DeepEqual(m.Boundaries, []float64{1, 10, 100}) || !reflect.DeepEqual(m.Context, request) {
		t.Fatalf("histogram = %+v", m)
	}
	if measurements[4].Context != nil || !reflect.DeepEqual(measurements[5].Context, request) {
		t.Fatalf("exemplars = %v, %v", measurements[4].Context, measurements[5].Context)
	}
	// Beyond the compile-time check: a duplicate name is an error at run time.
	if _, err := telemetry.NewTypedMetrics(adapter, trafficSchema, trafficSchema); err == nil || !strings.Contains(err.Error(), "more than one schema") {
		t.Fatalf("duplicate schemas = %v, want an error", err)
	}
}

func TestMetricsReturnDetachedMeasurementSnapshotsIncludingExemplarsAndBoundaries(t *testing.T) {
	c := telemetry.NewInMemoryContext()
	histogram := c.Histogram("duration", telemetry.HistogramOptions{Boundaries: []float64{1, 2}})
	must(t, c.StartSpan(telemetry.SpanOptions{Name: "request"}, func(span telemetry.Span) error {
		histogram.Record(1, telemetry.SpanAttributes{"tags": []string{"original"}}, span)
		return nil
	}))
	measurement := c.Measurements()[0]
	measurement.Boundaries[0] = 3
	measurement.Attributes["tags"] = []string{"mutated"}
	measurement.Context["pi-memory-span"] = "999"
	again := c.Measurements()[0]
	if !reflect.DeepEqual(again.Boundaries, []float64{1, 2}) || !reflect.DeepEqual(again.Attributes, telemetry.SpanAttributes{"tags": []string{"original"}}) || !reflect.DeepEqual(again.Context, c.Spans()[0].Context) {
		t.Fatalf("measurement = %+v", again)
	}
}

// pi's unreadable options and attributes are n/a.
func TestMetricsIgnoreMalformedBoundariesAndNonfiniteValues(t *testing.T) {
	c := telemetry.NewInMemoryContext()
	for _, boundaries := range [][]float64{{1, 1}, {2, 1}, {math.NaN()}, {math.Inf(1)}} {
		c.Histogram("bad", telemetry.HistogramOptions{Boundaries: boundaries}).Record(1, nil, nil)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c.Counter("bad", telemetry.InstrumentOptions{}).Add(value, nil)
		c.UpDownCounter("bad", telemetry.InstrumentOptions{}).Add(value, nil)
		c.Histogram("bad", telemetry.HistogramOptions{}).Record(value, nil, nil)
	}
	c.Counter("bad", telemetry.InstrumentOptions{}).Add(-1, nil)
	if got := c.Measurements(); len(got) != 0 {
		t.Fatalf("measurements = %+v, want none", got)
	}
	c.Histogram("valid", telemetry.HistogramOptions{}).Record(-1, telemetry.SpanAttributes{}, telemetry.NoopContext)
	if got := c.Measurements(); len(got) != 1 || got[0].Context != nil {
		t.Fatalf("measurements = %+v, want one without exemplar", got)
	}
}

// pi also checks the instruments are frozen; Go values of an unexported,
// fieldless type cannot be changed.
func TestMetricsShareNoopInstrumentsWithoutInspectingArguments(t *testing.T) {
	counter := telemetry.NoopMetrics.Counter("bytes", telemetry.InstrumentOptions{Unit: "By"})
	if counter != telemetry.NoopMetrics.Counter("other", telemetry.InstrumentOptions{}) {
		t.Fatal("no-op counters are not shared")
	}
	counter.Add(1, telemetry.SpanAttributes{"secret": "value"})
	telemetry.NoopMetrics.UpDownCounter("active", telemetry.InstrumentOptions{}).Add(-1, nil)
	telemetry.NoopMetrics.Histogram("duration", telemetry.HistogramOptions{}).Record(1, nil, telemetry.NoopContext)
}
