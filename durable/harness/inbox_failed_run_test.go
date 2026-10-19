package harness

import (
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-failed-run-inbox.test.ts.

type failedRun struct {
	h      *Harness
	root   *Conversation
	second *Submission
}

// failWithQueuedInput fails a run with an input queued behind it, under
// policy ("" is the default).
func failWithQueuedInput(t *testing.T, policy AfterFailedRun) failedRun {
	t.Helper()
	setup := newChatSetup(t)
	setup.setSettings(func(s *HarnessSettings) { s.AfterFailedRun = policy })
	failing, reached, release := gatedStep(fauxError("Invalid request"))
	setup.faux.SetResponses([]providers.FauxResponseStep{failing, fauxText("second answer"), fauxText("third answer")})
	h, root := openChat(t, memory.New(), setup)
	first, err := root.Submit(bg, inputDraft("Analyze this report"))
	must(t, err)
	awaitSignal(t, reached, "the failing request")
	draft := inputDraft("Email me the summary")
	draft.Input.WhenBusy = WhenBusyFollowUp
	second, err := root.Submit(bg, draft)
	must(t, err)
	release()
	settled, err := first.Wait(bg)
	must(t, err)
	if settled.Status != durable.SubmissionUnanswered || settled.Reason != "model_error" {
		t.Fatalf("first = %+v, want unanswered model_error", settled)
	}
	return failedRun{h: h, root: root, second: second}
}

func TestFailedRunInboxKeepsQueuedInputsByDefault(t *testing.T) {
	run := failWithQueuedInput(t, "")
	must(t, run.h.WaitForIdle(bg))
	record, err := run.second.Status(bg)
	must(t, err)
	if record.Status != durable.SubmissionQueued {
		t.Fatalf("second = %s, want queued", record.Status)
	}
	// The next input runs the queue, as before.
	third, err := run.root.Submit(bg, inputDraft("Anything else?"))
	must(t, err)
	for name, submission := range map[string]*Submission{"second": run.second, "third": third} {
		settled, err := submission.Wait(bg)
		must(t, err)
		if settled.Status != durable.SubmissionDone {
			t.Fatalf("%s = %+v, want done", name, settled)
		}
	}
}

func TestFailedRunInboxStartsTheNextRunWithContinue(t *testing.T) {
	run := failWithQueuedInput(t, AfterFailedRunContinue)
	settled, err := run.second.Wait(bg)
	must(t, err)
	if settled.Status != durable.SubmissionDone || settled.Type != durable.SubmissionTypeInput || settled.Answer == nil {
		t.Fatalf("second = %+v, want a done input", settled)
	}
	if got := textOf(entryByID(t, run.root, *settled.Answer)); got != "second answer" {
		t.Fatalf("answer = %q, want \"second answer\"", got)
	}
}

func TestFailedRunInboxPlacesQueuedWritesAndKeepsThemWithWithdraw(t *testing.T) {
	for _, policy := range []AfterFailedRun{AfterFailedRunContinue, AfterFailedRunWithdraw} {
		t.Run(string(policy), func(t *testing.T) {
			setup := newChatSetup(t)
			setup.setSettings(func(s *HarnessSettings) { s.AfterFailedRun = policy })
			failing, reached, release := gatedStep(fauxError("Invalid request"))
			setup.faux.SetResponses([]providers.FauxResponseStep{failing, fauxText("second answer")})
			h, root := openChat(t, memory.New(), setup)
			first, err := root.Submit(bg, inputDraft("first"))
			must(t, err)
			awaitSignal(t, reached, "the failing request")
			note := ai.NewUserText("a note the model sees", 1)
			write, err := root.Submit(bg, SubmissionDraft{Type: durable.SubmissionTypeWrite, Write: &durable.EntryDraft{Kind: "test.note", Model: []ai.Message{note}}})
			must(t, err)
			record, err := write.Status(bg)
			must(t, err)
			if record.Status != durable.SubmissionQueued {
				t.Fatalf("write before the failure = %s, want queued", record.Status)
			}
			release()
			_, err = first.Wait(bg)
			must(t, err)
			must(t, h.WaitForIdle(bg))
			// continue places the write at the failed run's boundary; withdraw
			// withdraws inputs only.
			want := durable.SubmissionDone
			if policy == AfterFailedRunWithdraw {
				want = durable.SubmissionQueued
			}
			record, err = write.Status(bg)
			must(t, err)
			if record.Status != want {
				t.Fatalf("write = %s, want %s", record.Status, want)
			}
		})
	}
}

func TestFailedRunInboxSettlesUnansweredWithRunFailedWithWithdraw(t *testing.T) {
	run := failWithQueuedInput(t, AfterFailedRunWithdraw)
	settled, err := run.second.Wait(bg)
	must(t, err)
	if settled.Status != durable.SubmissionUnanswered || settled.Reason != "run_failed" {
		t.Fatalf("second = %+v, want unanswered run_failed", settled)
	}
}
