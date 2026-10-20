package harness

import (
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// A tool task the scheduler faults or orphans settles its nested calls'
// slots with its own, as pi's settleSchedulerOutcome removes them.
func TestSettleSchedulerOutcomeRemovesTheNestedSlotsOfAFaultedOrOrphanedTool(t *testing.T) {
	for _, outcome := range []durable.TaskOutcome{
		{Status: durable.OutcomeFaulted, Error: &durable.TaskOutcomeError{Message: "broke"}},
		{Status: durable.OutcomeOrphaned, Reason: "task_too_old"},
	} {
		t.Run(outcome.Status, func(t *testing.T) {
			h, root := openChat(t, memory.New(), newChatSetup(t))
			parent, child, other := durable.TaskID(7), durable.TaskID(8), durable.TaskID(9)
			must(t, root.Commit(bg, func(tx *durable.Tx) error {
				live, err := liveDraft(tx, root.ID)
				if err != nil {
					return err
				}
				if err := setDraftField(live, "tools", []ToolSlot{{CallID: "c", Name: "outer", Status: slotRunning, TaskID: &parent}}); err != nil {
					return err
				}
				return setDraftField(live, "nestedTools", []NestedToolSlot{
					{CallID: "c/0", ParentCallID: "c", ParentTaskID: parent, Name: "inner", TaskID: child, Status: slotRunning},
					{CallID: "d/0", ParentCallID: "d", ParentTaskID: 3, Name: "inner", TaskID: other, Status: slotRunning},
				})
			}))
			must(t, root.Commit(bg, func(tx *durable.Tx) error {
				return settleSchedulerOutcome(tx, durable.TaskRecord{ID: parent, Kind: toolKind, ConversationID: root.ID}, outcome)
			}))
			live, err := decodeLiveParts(liveOf(t, h, root))
			must(t, err)
			if len(live.Tools) != 1 || live.Tools[0].Status != slotDone {
				t.Fatalf("tools = %+v", live.Tools)
			}
			if len(live.NestedTools) != 1 || live.NestedTools[0].TaskID != other {
				t.Fatalf("nested tools = %+v, want only the other call's", live.NestedTools)
			}
		})
	}
}
