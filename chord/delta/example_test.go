package delta_test

import (
	"encoding/json"
	"fmt"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// The package doc's "Producing changes" flow, compiled and run, with a
// replica applying the batches in-process as the doc's "Streams" asks:
// immutably, because they share the tracker's revisions.
func ExampleTracker() {
	check := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	tracker := delta.Track(map[string]any{"rows": []any{}, "status": "idle"})
	replica, err := delta.ApplyImmutable[any](nil, []delta.Op{delta.Replace{Value: tracker.Value()}})
	check(err)
	row := map[string]any{"id": 1}

	change, err := tracker.BeginChange()
	check(err)
	state := change.State() // the root draft
	err = state.Set("status", "running")
	check(err)
	_, err = state.At("rows").Push(row)
	check(err)
	prepared, err := change.Prepare() // or change.Abort()
	check(err)
	err = tracker.Adopt(prepared) // commit
	check(err)

	batch, err := json.Marshal(prepared.Ops())
	check(err)
	fmt.Println(string(batch))
	replica, err = delta.ApplyImmutable(replica, prepared.Ops())
	check(err)
	value, err := json.Marshal(replica)
	check(err)
	fmt.Println(string(value))
	// Output:
	// [["s",["status"],"running"],["p",["rows"],0,0,[{"id":1}]]]
	// {"rows":[{"id":1}],"status":"running"}
}
