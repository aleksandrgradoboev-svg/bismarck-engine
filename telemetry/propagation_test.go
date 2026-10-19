package telemetry_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Ported from pi packages/telemetry/test/propagation.test.ts.

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wire(t *testing.T, carrier telemetry.Carrier) telemetry.Carrier {
	t.Helper()
	raw, err := json.Marshal(carrier)
	must(t, err)
	var out telemetry.Carrier
	must(t, json.Unmarshal(raw, &out))
	return out
}

func spanNamed(t *testing.T, spans []telemetry.RecordedSpan, name string) telemetry.RecordedSpan {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("no span %s", name)
	return telemetry.RecordedSpan{}
}

// pi runs the receiver in another Node process; what it checks is that the
// receiver shares no recorder state, which a second recorder fed only the
// JSON carrier checks as well.
func TestPropagationContinuesAParentInAnotherRecorderWithoutSharedState(t *testing.T) {
	caller := telemetry.NewInMemoryContext()
	var carrier telemetry.Carrier
	must(t, caller.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
		carrier = caller.Inject(span)
		return nil
	}))
	worker := telemetry.NewInMemoryContext()
	must(t, worker.Extract(wire(t, carrier)).StartSpan(telemetry.SpanOptions{Name: "worker"}, func(telemetry.Span) error { return nil }))
	recorded := worker.Spans()
	if len(recorded) != 1 {
		t.Fatalf("spans = %d, want 1", len(recorded))
	}
	if !reflect.DeepEqual(recorded[0].ParentContext, carrier) || recorded[0].ParentID != 0 || reflect.DeepEqual(recorded[0].Context, carrier) {
		t.Fatalf("worker span = %+v, want a remote child of %v", recorded[0], carrier)
	}
}

func TestPropagationIsolatesCrossedConcurrentRemoteParentsAndLinksSharedRoots(t *testing.T) {
	caller := telemetry.NewInMemoryContext()
	provider := telemetry.NewInMemoryContext()
	var mu sync.Mutex
	var activation telemetry.Span
	activated := make(chan struct{})
	finish := make(chan struct{})
	sharedDone := make(chan struct{})
	var startShared sync.Once
	var wg sync.WaitGroup
	// "first" starts the shared activation; "later" runs once it is active.
	run := func(name string, after <-chan struct{}) {
		defer wg.Done()
		if after != nil {
			<-after
		}
		must(t, caller.StartSpan(telemetry.SpanOptions{Name: name}, func(trigger telemetry.Span) error {
			parent := provider.Extract(caller.Inject(trigger))
			return parent.StartSpan(telemetry.SpanOptions{Name: name + ".method"}, func(getter telemetry.Span) error {
				startShared.Do(func() {
					go func() {
						defer close(sharedDone)
						_ = provider.StartSpan(telemetry.SpanOptions{Name: "activation", Links: []telemetry.SpanLink{{Context: getter}}}, func(span telemetry.Span) error {
							mu.Lock()
							activation = span
							mu.Unlock()
							close(activated)
							<-finish
							return nil
						})
					}()
					<-activated
				})
				mu.Lock()
				active := activation
				mu.Unlock()
				if name == "later" {
					getter.AddLink(telemetry.SpanLink{Context: active})
					close(finish)
				}
				<-sharedDone
				return nil
			})
		}))
	}
	firstInside := make(chan struct{})
	wg.Add(2)
	go func() { run("first", nil) }()
	go func() {
		<-activated
		close(firstInside)
		run("later", nil)
	}()
	<-firstInside
	wg.Wait()

	spans := provider.Spans()
	root := spanNamed(t, spans, "activation")
	if root.ParentContext != nil {
		t.Fatalf("activation parent = %v, want a root", root.ParentContext)
	}
	callerSpans := caller.Spans()
	first, later := spanNamed(t, spans, "first.method"), spanNamed(t, spans, "later.method")
	if !reflect.DeepEqual(first.ParentContext, spanNamed(t, callerSpans, "first").Context) || !reflect.DeepEqual(later.ParentContext, spanNamed(t, callerSpans, "later").Context) {
		t.Fatalf("crossed parents: first %v, later %v", first.ParentContext, later.ParentContext)
	}
	if len(root.Links) != 1 || !reflect.DeepEqual(root.Links[0].Context, first.Context) {
		t.Fatalf("activation links = %v, want first.method", root.Links)
	}
	if len(later.Links) != 1 || !reflect.DeepEqual(later.Links[0].Context, root.Context) {
		t.Fatalf("later links = %v, want activation", later.Links)
	}
}

func TestPropagationDetachesCarrierAndLinkSnapshotsAndRetainsSettledTargetIdentities(t *testing.T) {
	c := telemetry.NewInMemoryContext()
	var target telemetry.Span
	must(t, c.StartSpan(telemetry.SpanOptions{Name: "target"}, func(span telemetry.Span) error { target = span; return nil }))
	// Beyond pi: the expected carrier is copied before the mutation, so a
	// shared carrier (both injections the same map) cannot hide the leak.
	original := maps.Clone(c.Inject(target))
	injected := c.Inject(target)
	injected["pi-memory-span"] = "999"
	if !reflect.DeepEqual(c.Inject(target), original) {
		t.Fatal("an injected carrier reaches the recorder")
	}
	must(t, c.StartSpan(telemetry.SpanOptions{Name: "linked", Links: []telemetry.SpanLink{{Context: target, Attributes: telemetry.SpanAttributes{"tags": []string{"original"}}}}}, func(telemetry.Span) error { return nil }))
	spans := c.Spans()
	spans[0].Context["pi-memory-span"] = "123"
	spans[1].Links[0].Context["pi-memory-span"] = "456"
	spans[1].Links[0].Attributes["tags"] = []string{"mutated"}
	want := []telemetry.RecordedLink{{Context: original, Attributes: telemetry.SpanAttributes{"tags": []string{"original"}}}}
	if got := c.Spans()[1].Links; !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

func TestPropagationLinksANewRootToAnExtractedRemoteTarget(t *testing.T) {
	sender, receiver := telemetry.NewInMemoryContext(), telemetry.NewInMemoryContext()
	var carrier telemetry.Carrier
	must(t, sender.StartSpan(telemetry.SpanOptions{Name: "trigger"}, func(span telemetry.Span) error {
		carrier = sender.Inject(span)
		return nil
	}))
	target := receiver.Extract(wire(t, carrier))
	must(t, receiver.StartSpan(telemetry.SpanOptions{Name: "background", Links: []telemetry.SpanLink{{Context: target, Attributes: telemetry.SpanAttributes{"reason": "restart"}}}}, func(span telemetry.Span) error {
		span.AddLink(telemetry.SpanLink{Context: target})
		return nil
	}))
	recorded := receiver.Spans()[0]
	if recorded.ParentContext != nil {
		t.Fatalf("parent = %v, want a root", recorded.ParentContext)
	}
	want := []telemetry.RecordedLink{
		{Context: carrier, Attributes: telemetry.SpanAttributes{"reason": "restart"}},
		{Context: carrier, Attributes: telemetry.SpanAttributes{}},
	}
	if !reflect.DeepEqual(recorded.Links, want) {
		t.Fatalf("links = %v, want %v", recorded.Links, want)
	}
}

// Beyond pi: no pi case checks the local parent of an extracted carrier. A
// carrier of this recorder's own span parents the child under that span's
// ID; another recorder's carrier, and an unknown span ID, give no local
// parent.
func TestPropagationRecordsTheLocalParentOfAnExtractedOwnCarrier(t *testing.T) {
	c, other := telemetry.NewInMemoryContext(), telemetry.NewInMemoryContext()
	var own telemetry.Carrier
	must(t, c.StartSpan(telemetry.SpanOptions{Name: "own"}, func(span telemetry.Span) error {
		own = c.Inject(span)
		return nil
	}))
	var foreign telemetry.Carrier
	must(t, other.StartSpan(telemetry.SpanOptions{Name: "foreign"}, func(span telemetry.Span) error {
		foreign = other.Inject(span)
		return nil
	}))
	unknown := maps.Clone(own)
	unknown["pi-memory-span"] = "99"
	for name, carrier := range map[string]telemetry.Carrier{"local": own, "remote": foreign, "unknown": unknown} {
		must(t, c.Extract(wire(t, carrier)).StartSpan(telemetry.SpanOptions{Name: name}, func(telemetry.Span) error { return nil }))
	}
	spans := c.Spans()
	ownID := spanNamed(t, spans, "own").ID
	if got := spanNamed(t, spans, "local"); got.ParentID != ownID || !reflect.DeepEqual(got.ParentContext, own) {
		t.Fatalf("local child = parent %d %v, want %d %v", got.ParentID, got.ParentContext, ownID, own)
	}
	for _, name := range []string{"remote", "unknown"} {
		if got := spanNamed(t, spans, name); got.ParentID != 0 || got.ParentContext == nil {
			t.Fatalf("%s child = parent %d %v, want a remote parent without a local ID", name, got.ParentID, got.ParentContext)
		}
	}
}

// pi's unreadable carriers are n/a; malformed values of the memory format
// are not.
func TestPropagationRejectsMalformedMemoryCarriersAndNeverInspectsNoopPayloads(t *testing.T) {
	c := telemetry.NewInMemoryContext()
	for _, carrier := range []telemetry.Carrier{
		{"pi-memory-scope": "bad", "pi-memory-span": "1"},
		{"pi-memory-scope": "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f", "pi-memory-span": "0"},
		{"pi-memory-scope": "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f", "pi-memory-span": "9007199254740992"},
		{"pi-memory-scope": "0F0F0F0F-0F0F-4F0F-8F0F-0F0F0F0F0F0F", "pi-memory-span": "1"},
		{"bad": "value"},
	} {
		if got := c.Extract(carrier); got != telemetry.Context(c) {
			t.Fatalf("extract(%v) = %T, want the recorder's root", carrier, got)
		}
	}
	if telemetry.NoopPropagation.Inject(telemetry.NoopContext) != nil {
		t.Fatal("no-op inject returned a carrier")
	}
	if telemetry.NoopPropagation.Extract(telemetry.Carrier{"bad": "value"}) != telemetry.NoopContext {
		t.Fatal("no-op extract is not the no-op context")
	}
	must(t, telemetry.NoopContext.StartSpan(telemetry.SpanOptions{Name: "noop"}, func(span telemetry.Span) error {
		span.AddLink(telemetry.SpanLink{Context: span})
		return nil
	}))
}
