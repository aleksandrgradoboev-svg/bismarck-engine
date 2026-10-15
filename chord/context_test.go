package chord

import (
	"context"
	"errors"
	"testing"
)

// Port of packages/chord/test/context.test.ts at 64eeb82a4. Upstream's
// Context is a TypeScript reimplementation of Go's context package, so the
// mirror is context.Context plus typed keys: createContextKey → NewKey,
// withContextValue → WithValue, context.value(key) → key.Value(ctx),
// withAbortSignal/withCancel → context.WithCancel, withoutAbortSignal →
// context.WithoutCancel, awaitWithContext → a select on ctx.Done().

// "provides distinct empty root contexts": a key reads nothing from either
// root, and the roots are Go's own.
func TestKeyReadsNothingFromEmptyRoots(t *testing.T) {
	key := NewKey[string]("value")
	for _, ctx := range []context.Context{context.Background(), context.TODO()} {
		if v, ok := key.Value(ctx); ok || v != "" {
			t.Errorf("%v: Value = %q, %v; want \"\", false", ctx, v, ok)
		}
	}
	if context.Background() == context.TODO() {
		t.Error("roots are not distinct")
	}
}

// "layers typed values without modifying parents"
func TestKeyLayersTypedValuesWithoutModifyingParents(t *testing.T) {
	firstKey := NewKey[string]("first")
	secondKey := NewKey[int]("second")
	first := WithValue(context.Background(), firstKey, "one")
	second := WithValue(first, secondKey, 2)
	replaced := WithValue(second, firstKey, "updated")

	if _, ok := firstKey.Value(context.Background()); ok {
		t.Error("background context gained a value")
	}
	if v, _ := firstKey.Value(first); v != "one" {
		t.Errorf("first.first = %q, want one", v)
	}
	if _, ok := secondKey.Value(first); ok {
		t.Error("first sees a value layered on its child")
	}
	if v, _ := firstKey.Value(second); v != "one" {
		t.Errorf("second.first = %q, want one", v)
	}
	if v, _ := secondKey.Value(second); v != 2 {
		t.Errorf("second.second = %d, want 2", v)
	}
	if v, _ := firstKey.Value(replaced); v != "updated" {
		t.Errorf("replaced.first = %q, want updated", v)
	}
	if v, _ := firstKey.Value(second); v != "one" {
		t.Errorf("second.first after replace = %q, want one", v)
	}
}

// Keys are identities, not names: upstream mints a fresh Symbol per
// createContextKey, so two keys sharing a description never alias.
func TestKeysWithTheSameNameAreDistinct(t *testing.T) {
	a := NewKey[string]("value")
	b := NewKey[string]("value")
	ctx := WithValue(context.Background(), a, "for a")
	if v, ok := b.Value(ctx); ok {
		t.Errorf("b read a's value %q", v)
	}
	if got := a.String(); got != "value" {
		t.Errorf("String() = %q, want the key's name", got)
	}
}

// "inherits parent cancellation and isolates child cancellation"
func TestChildCancellationIsIsolatedAndParentCancellationInherited(t *testing.T) {
	parent, cancelParent := context.WithCancelCause(context.Background())
	defer cancelParent(nil)
	child, cancelChild := context.WithCancelCause(parent)
	sibling, cancelSibling := context.WithCancelCause(parent)
	defer cancelSibling(nil)

	childCause := errors.New("child")
	cancelChild(childCause)
	if child.Err() == nil || context.Cause(child) != childCause {
		t.Errorf("child: err %v cause %v", child.Err(), context.Cause(child))
	}
	if sibling.Err() != nil || parent.Err() != nil {
		t.Error("child cancellation leaked to sibling or parent")
	}

	parentCause := errors.New("parent")
	cancelParent(parentCause)
	if sibling.Err() == nil || context.Cause(sibling) != parentCause {
		t.Errorf("sibling: err %v cause %v", sibling.Err(), context.Cause(sibling))
	}
}

// "masks caller cancellation for mandatory cleanup": WithoutCancel drops the
// deadline but keeps every typed value.
func TestWithoutCancelKeepsTypedValues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	key := NewKey[string]("value")
	ctx = WithValue(ctx, key, "preserved")
	cleanup := context.WithoutCancel(ctx)

	cancel()
	if ctx.Err() == nil {
		t.Error("context not cancelled")
	}
	if cleanup.Err() != nil {
		t.Error("cleanup context cancelled")
	}
	if v, ok := key.Value(cleanup); !ok || v != "preserved" {
		t.Errorf("cleanup value = %q, %v; want preserved, true", v, ok)
	}
}

// "stops waiting when the invocation is cancelled": cancelling the waiter's
// context releases only the waiter; the work still completes on its own.
func TestWaitingStopsWhenTheInvocationIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	work := make(chan string, 1)
	cancellation := errors.New("cancelled")

	cancel(cancellation)
	select {
	case <-ctx.Done():
		if context.Cause(ctx) != cancellation {
			t.Errorf("cause = %v, want %v", context.Cause(ctx), cancellation)
		}
	case v := <-work:
		t.Fatalf("received %q before the work ran", v)
	}

	work <- "completed later"
	if v := <-work; v != "completed later" {
		t.Errorf("work = %q", v)
	}
	done := make(chan string, 1)
	done <- "completed"
	select {
	case <-context.Background().Done():
		t.Fatal("background context is done")
	case v := <-done:
		if v != "completed" {
			t.Errorf("got %q", v)
		}
	}
}
