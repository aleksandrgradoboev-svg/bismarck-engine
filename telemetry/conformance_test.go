package telemetry_test

import (
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry/telemetrytest"
)

// Ported from pi packages/telemetry/test/conformance.test.ts.

func TestInMemoryContextConformance(t *testing.T) {
	telemetrytest.TestAdapter(t, func(*testing.T) telemetrytest.Fixture {
		c := telemetry.NewInMemoryContext()
		return telemetrytest.Fixture{Context: c, Propagation: c, Metrics: c, Measurements: c.Measurements, Spans: c.Spans}
	})
}

func TestInMemoryContextReturnsDetachedSnapshotsWithoutExposingMutableRecordingState(t *testing.T) {
	c := telemetry.NewInMemoryContext()
	var open telemetry.RecordedSpan
	err := c.StartSpan(telemetry.SpanOptions{Name: "snapshot", Attributes: telemetry.SpanAttributes{"tags": []string{"initial"}}}, func(span telemetry.Span) error {
		span.AddEvent("event", telemetry.SpanAttributes{"value": 1})
		open = c.Spans()[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if open.Settled || open.EndSequence != 0 {
		t.Fatalf("open snapshot settled=%v end=%d, want open", open.Settled, open.EndSequence)
	}
	first := c.Spans()[0]
	if !first.Settled || first.EndSequence != 1 {
		t.Fatalf("settled snapshot settled=%v end=%d", first.Settled, first.EndSequence)
	}
	first.Attributes["tags"].([]string)[0] = "mutated"
	first.Events[0].Attributes["value"] = 2
	second := c.Spans()[0]
	if !reflect.DeepEqual(second.Attributes, telemetry.SpanAttributes{"tags": []string{"initial"}}) {
		t.Fatalf("attributes = %v", second.Attributes)
	}
	if !reflect.DeepEqual(second.Events, []telemetry.RecordedEvent{{Name: "event", Attributes: telemetry.SpanAttributes{"value": 1}}}) {
		t.Fatalf("events = %v", second.Events)
	}
}
