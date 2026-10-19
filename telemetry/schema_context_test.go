package telemetry_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Ported from pi packages/telemetry/test/telemetry.test.ts. The inferred
// attribute types and createTypedSpanStarter are compile-time typing (n/a);
// what applies is that a definition is serializable data.

func TestSchemaDefinitionsAreSerializableData(t *testing.T) {
	schema := telemetry.SchemaDefinition{
		Version: 1,
		Spans: map[string]telemetry.SpanDefinition{
			"operation": {
				Description: "Test operation",
				Parents:     telemetry.ParentDefinition{Kind: telemetry.ParentsAny},
				StartAttributes: map[string]telemetry.AttributeDefinition{
					"kind": {Type: telemetry.AttributeString, Required: true, Values: []any{"read", "write"}, Description: "Kind"},
				},
				EndAttributes: map[string]telemetry.AttributeDefinition{},
				Events: map[string]telemetry.EventDefinition{
					"result": {Description: "Result", Attributes: map[string]telemetry.AttributeDefinition{
						"outcome": {Type: telemetry.AttributeString, Required: true, Values: []any{"ok", "error"}, Description: "Outcome"},
					}},
				},
				Status: telemetry.StatusDefinition{Default: "ok", ErrorWhen: "The operation fails"},
			},
		},
	}
	raw, err := json.Marshal(schema)
	must(t, err)
	want := `{"version":1,"spans":{"operation":{"description":"Test operation","parents":{"kind":"any"},` +
		`"startAttributes":{"kind":{"type":"string","required":true,"description":"Kind","values":["read","write"]}},` +
		`"endAttributes":{},"events":{"result":{"description":"Result","attributes":{"outcome":{"type":"string","required":true,` +
		`"description":"Outcome","values":["ok","error"]}}}},"status":{"default":"ok","errorWhen":"The operation fails"}}}}`
	if string(raw) != want {
		t.Fatalf("schema JSON =\n%s\nwant\n%s", raw, want)
	}
	var back telemetry.SchemaDefinition
	must(t, json.Unmarshal(raw, &back))
	if !reflect.DeepEqual(back, schema) {
		t.Fatalf("roundtrip = %+v", back)
	}
}

func TestNoopSpanAdmitsCallbacksSynchronouslyAndReusesOneInertSpan(t *testing.T) {
	admitted := false
	var first telemetry.Span
	must(t, telemetry.NoopContext.StartSpan(telemetry.SpanOptions{Name: "first"}, func(span telemetry.Span) error {
		admitted = true
		first = span
		return span.StartSpan(telemetry.SpanOptions{Name: "child"}, func(child telemetry.Span) error {
			if child != span {
				t.Fatal("the no-op child is another span")
			}
			return nil
		})
	}))
	if !admitted || first != telemetry.NoopSpan || telemetry.NoopContext != telemetry.Context(telemetry.NoopSpan) {
		t.Fatal("the no-op context is not the shared inert span")
	}
	if telemetry.MetricsOf(telemetry.NoopContext) != telemetry.NoopMetrics || telemetry.PropagationOf(first) != telemetry.NoopPropagation {
		t.Fatal("the no-op span does not carry the no-op capabilities")
	}
	returned := errors.New("returned")
	if err := telemetry.NoopContext.StartSpan(telemetry.SpanOptions{Name: "error"}, func(telemetry.Span) error { return returned }); err != returned {
		t.Fatalf("error = %v, want the callback's own", err)
	}
}

// An adapter without the optional capabilities still implements Context;
// MetricsOf and PropagationOf fall back to the no-op ones.
type bareContext struct{}

func (bareContext) StartSpan(_ telemetry.SpanOptions, callback func(telemetry.Span) error) error {
	return callback(telemetry.NoopSpan)
}

func TestCapabilitiesFallBackToNoopForAnAdapterWithoutThem(t *testing.T) {
	if telemetry.MetricsOf(bareContext{}) != telemetry.NoopMetrics || telemetry.PropagationOf(bareContext{}) != telemetry.NoopPropagation {
		t.Fatal("a bare adapter does not fall back to the no-op capabilities")
	}
	c := telemetry.NewInMemoryContext()
	if telemetry.MetricsOf(c) != telemetry.Metrics(c) || telemetry.PropagationOf(c) != telemetry.Propagation(c) {
		t.Fatal("the in-memory adapter does not carry itself")
	}
}

// pi chord/context: Context.telemetry is always a span; withTelemetry and
// traceIn.
func TestContextCarriesTheSpanAndTraceInParentsTheChild(t *testing.T) {
	if telemetry.SpanFrom(context.Background()) != telemetry.NoopSpan || telemetry.SpanFrom(nil) != telemetry.NoopSpan { //nolint:staticcheck // nil is part of the contract
		t.Fatal("an empty context has no no-op span")
	}
	if telemetry.SpanFrom(telemetry.WithSpan(context.Background(), nil)) != telemetry.NoopSpan {
		t.Fatal("WithSpan(nil) is not the no-op span")
	}
	c := telemetry.NewInMemoryContext()
	type key struct{}
	base := context.WithValue(context.Background(), key{}, "kept")
	must(t, c.StartSpan(telemetry.SpanOptions{Name: "parent"}, func(span telemetry.Span) error {
		ctx := telemetry.WithSpan(base, span)
		if telemetry.SpanFrom(ctx) != span || ctx.Value(key{}) != "kept" {
			t.Fatal("WithSpan lost the span or a value")
		}
		return telemetry.TraceIn(ctx, telemetry.SpanOptions{Name: "child"}, func(ctx context.Context) error {
			return telemetry.TraceIn(ctx, telemetry.SpanOptions{Name: "grandchild"}, func(context.Context) error { return nil })
		})
	}))
	spans := c.Spans()
	parent, child, grandchild := spanNamed(t, spans, "parent"), spanNamed(t, spans, "child"), spanNamed(t, spans, "grandchild")
	if child.ParentID != parent.ID || grandchild.ParentID != child.ID {
		t.Fatalf("parents: child %d (want %d), grandchild %d (want %d)", child.ParentID, parent.ID, grandchild.ParentID, child.ID)
	}
	cancelled, cancel := context.WithCancel(telemetry.WithSpan(context.Background(), telemetry.NoopSpan))
	cancel()
	if err := telemetry.TraceIn(cancelled, telemetry.SpanOptions{Name: "x"}, func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("TraceIn changed cancellation: %v", err)
	}
}
