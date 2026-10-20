package harness

import (
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Beyond pi: a context read whose bounds were captured on the line derives
// its entries off it even when Close seals the line meanwhile; pi's
// continuation reaches Storage ahead of the close on the line, here Close
// waits for it.
func TestContextReadFinishesItsDerivationWhenCloseSealsTheLineMeanwhile(t *testing.T) {
	o := openRoot(t, nil, openOptions{storage: newControlledStorage()})
	// An entry, so the derivation reads Storage: an empty conversation has no
	// bounds to derive from.
	must(t, o.h.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "note"})
		return err
	}))
	entered, release := make(chan struct{}), make(chan struct{})
	var enter, open sync.Once
	unblock := func() { open.Do(func() { close(release) }) }
	hook := func() {
		enter.Do(func() {
			close(entered)
			<-release
		})
	}
	beforeContextDerive.Store(&hook)
	t.Cleanup(func() { unblock(); beforeContextDerive.Store(nil) })
	read := async(func() (ContextView, error) { return o.root.Context(bg, nil) })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not reach its derivation")
	}
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	// Long enough for Close to reach Storage, were it not waiting.
	time.Sleep(50 * time.Millisecond)
	unblock()
	if _, err := await(t, read); err != nil {
		t.Fatalf("context = %v", err)
	}
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
}
