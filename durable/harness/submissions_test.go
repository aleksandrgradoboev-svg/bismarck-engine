package harness

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Ported from pi packages/durable/test/harness-submissions.test.ts.

func TestSubmissionsAppendAnIdleWriteAndSettleItDoneWithoutATurn(t *testing.T) {
	h, root := openChat(t, newControlledStorage(), newChatSetup(t))
	submission, err := root.Submit(bg, writeDraft("note", `{"text":"x"}`))
	must(t, err)
	settled, err := submission.Wait(bg)
	must(t, err)
	if settled.Entry == nil {
		t.Fatalf("settled without an entry: %+v", settled)
	}
	want := durable.SubmissionRecord{ID: submission.ID, ConversationID: root.ID, Type: durable.SubmissionTypeWrite, Status: durable.SubmissionDone, Entry: settled.Entry}
	if !reflect.DeepEqual(settled, want) {
		t.Fatalf("settled = %+v, want %+v", settled, want)
	}
	entries := chatEntries(t, root)
	wantEntries := []durable.EntryRecord{{ID: *settled.Entry, ConversationID: root.ID, Kind: "note", Data: json.RawMessage(`{"text":"x"}`)}}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("entries = %+v, want %+v", entries, wantEntries)
	}
	live, err := h.Snapshot(bg, LiveDoc.Conversation(root.ID))
	must(t, err)
	if !reflect.DeepEqual(live, map[string]any{}) {
		t.Fatalf("pi.live = %v, want {}", live)
	}
	var tasks durable.Page[durable.TaskRecord]
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		tasks, err = tx.ScanTasks(durable.TaskQuery{ConversationID: &root.ID}, 10, nil)
		return err
	}))
	if len(tasks.Items) != 0 {
		t.Fatalf("tasks = %+v, want none", tasks.Items)
	}
}

func TestSubmissionsEnableSchedulingWhenACallerSubmitsOrWaits(t *testing.T) {
	setup := newChatSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("answer")})
	h, root := openChat(t, newControlledStorage(), setup)
	// No Resume: submitting asks for progress.
	submission, err := root.Submit(bg, inputDraft("hi"))
	must(t, err)
	settled, err := submission.Wait(bg)
	must(t, err)
	if settled.Status != durable.SubmissionDone || settled.Answer == nil {
		t.Fatalf("status = %s, want done with an answer (%+v)", settled.Status, settled)
	}
	// Beyond pi: the answer is the generation's assistant entry.
	entries := chatEntries(t, root)
	answer := entries[len(entries)-1]
	if answer.ID != *settled.Answer || !durable.AssistantEntry.Is(&answer) || textOf(answer) != "answer" {
		t.Fatalf("answer entry = %+v, want the assistant's \"answer\"", answer)
	}
	must(t, h.Close(bg))

	passive, passiveRoot := openChat(t, newControlledStorage(), newChatSetup(t))
	var taskID durable.TaskID
	must(t, passiveRoot.Commit(bg, func(tx *durable.Tx) error {
		var err error
		taskID, err = CreateTask(tx, GenerationTask, GenerationInput{}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		return err
	}))
	// A committed task alone does not start scheduling; waiting for it does.
	record, err := passive.GetTask(bg, taskID)
	must(t, err)
	if record == nil || record.State.Status != durable.TaskPending {
		t.Fatalf("task before the wait = %+v, want pending", record)
	}
	terminal, err := passive.WaitForTask(bg, taskID)
	must(t, err)
	if terminal.State.Status != durable.TaskTerminal {
		t.Fatalf("task after the wait = %s, want terminal", terminal.State.Status)
	}
}
