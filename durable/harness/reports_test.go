package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi test/harness-reports.test.ts. Its third case, sections, hooks and the
// environment builder inside pi.generation, comes with generation.

func TestReportsSayWhichTaskReportedAFailureOrFaulted(t *testing.T) {
	faulty := oneStep(t, "test.faulty", func(_ context.Context, _ *stepTask, rt *TaskRuntime) error {
		must(t, rt.Report(errors.New("noted")))
		return errors.New("broken")
	}, func(context.Context, *TaskRuntime) error { return nil })
	o := openRoot(t, []Task{faulty}, openOptions{})
	id := start(t, o.root, faulty, false)
	must(t, o.h.Resume())
	if got := waitDone(t, o.h, id).State.Outcome.Status; got != durable.OutcomeFaulted {
		t.Fatalf("outcome = %s", got)
	}
	eventually(t, func() bool { return len(o.reports.all()) == 2 })
	// The task's own failures name the extension that installed it.
	task := durable.ReportTask{ID: id, Kind: "test.faulty", ConversationID: o.root.ID}
	want := []struct {
		message string
		typ     durable.ReportType
		reason  string
	}{{"noted", durable.ReportTaskError, ""}, {"broken", durable.ReportTaskFaulted, "threw"}}
	errs, infos := o.reports.all(), o.reports.allInfos()
	for i, w := range want {
		info := infos[i]
		if errs[i].Error() != w.message || info.Type != w.typ || info.Task == nil || *info.Task != task || info.Extension != "tasks" || info.Reason != w.reason {
			t.Fatalf("report %d = %q %+v (task %+v), want %q %s of %+v from tasks", i, errs[i], info, info.Task, w.message, w.typ, task)
		}
	}
}

func TestReportsNameTheExtensionWhoseDefinitionRanAlsoAfterAnotherOneReplacedIt(t *testing.T) {
	gate, started := make(chan struct{}), make(chan struct{})
	definition := func(name string) Task {
		return oneStep(t, "test.replaced", func(context.Context, *stepTask, *TaskRuntime) error {
			if name == "old" {
				close(started)
			}
			<-gate
			return errors.New("broken in " + name)
		}, func(context.Context, *TaskRuntime) error { return nil })
	}
	old := definition("old")
	o := openRoot(t, nil, openOptions{})
	oldExtension := &Extension{Name: "old-extension", Tasks: []Task{old}}
	must(t, o.registry.Install(oldExtension))
	id := start(t, o.root, old, false)
	must(t, o.h.Resume())
	<-started
	o.registry.Uninstall(oldExtension)
	must(t, o.registry.Install(&Extension{Name: "new-extension", Tasks: []Task{definition("new")}}))
	close(gate)
	waitDone(t, o.h, id)
	faulted := func() (durable.ReportInfo, string, bool) {
		errs, infos := o.reports.all(), o.reports.allInfos()
		for i, info := range infos {
			if info.Type == durable.ReportTaskFaulted {
				return info, errs[i].Error(), true
			}
		}
		return durable.ReportInfo{}, "", false
	}
	eventually(t, func() bool { _, _, ok := faulted(); return ok })
	if info, message, _ := faulted(); message != "broken in old" || info.Extension != "old-extension" {
		t.Fatalf("faulted report = %q %+v", message, info)
	}
}

func TestReportsTellExtensionListenerWakeAndTimerFailuresFromAFailedSession(t *testing.T) {
	type sleeperState struct {
		Phase string `json:"phase"`
	}
	sleeper, err := DefineTask(TaskSpec[any, sleeperState, any]{
		Name:    "test.sleeper",
		Version: 1,
		Initial: func(any) sleeperState { return sleeperState{Phase: "run"} },
		Phases: map[string]func(context.Context, *Running[any, sleeperState], *TaskRuntime) error{
			"run": func(ctx context.Context, _ *Running[any, sleeperState], rt *TaskRuntime) error {
				// The host's SetTimer panics: reported, and armed with
				// time.AfterFunc instead.
				rt.SetTimer(func() {}, 10*time.Millisecond)
				now, err := rt.Now()
				if err != nil {
					return err
				}
				return rt.SleepUntil(ctx, now.Add(time.Minute), sleeperState{Phase: "done"})
			},
			"done": func(ctx context.Context, _ *Running[any, sleeperState], rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *Running[any, sleeperState], *TaskRuntime) error { return nil },
	})
	must(t, err)
	storage := newControlledStorage()
	o := openRoot(t, []Task{sleeper}, openOptions{
		storage:  storage,
		onWake:   func(Wake) { panic(errors.New("alarm")) },
		setTimer: func(func(), time.Duration, bool) func() { panic(errors.New("no timers")) },
	})
	echo := testTool("echo")
	must(t, o.registry.Install(&Extension{Name: "tools", Tools: []*ToolRegistration{echo}}))
	must(t, o.registry.Install(&Extension{Name: "wraps", Wraps: []Wrap{WrapTool("echo", func(tool *ToolRegistration) (*ToolRegistration, error) {
		renamed := *tool
		renamed.Name = "renamed"
		return &renamed, nil
	})}}))
	_, err = o.h.SubscribeCommits(func(durable.CommitPublication, context.Context) { panic(errors.New("listener")) })
	must(t, err)
	_, err = o.root.Agent(bg)
	must(t, err)
	start(t, o.root, sleeper, false)
	must(t, o.h.Resume())
	eventually(t, func() bool {
		_, alarm := o.reports.infoOf("alarm")
		_, timers := o.reports.infoOf("no timers")
		return alarm && timers
	})
	expect := func(message string, want durable.ReportInfo) {
		t.Helper()
		info, ok := o.reports.infoOf(message)
		if !ok || info.Task != nil || want.Task != nil {
			t.Fatalf("report %q = %+v (found %v)", message, info, ok)
		}
		if info != want {
			t.Fatalf("report %q = %+v, want %+v", message, info, want)
		}
	}
	expect("Wrapper renamed echo to renamed", durable.ReportInfo{Type: durable.ReportExtensionError, ConversationID: o.root.ID, Extension: "wraps"})
	expect("listener", durable.ListenerError)
	expect("alarm", durable.HostError(durable.CallbackOnWake, nil))
	expect("no timers", durable.HostError(durable.CallbackSetTimer, nil))

	storage.failNextCommit(errors.New("disk gone"))
	_ = o.root.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipOwnerless})
		return err
	})
	select {
	case <-o.h.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the Session did not close")
	}
	expect("disk gone", durable.ReportInfo{Type: durable.ReportSessionFailed})
}

// Beyond pi, which reports from a source through tools and hooks through
// generation: a task's report names whose code failed, and a failing hook
// names its extension.
func TestReportsNameTheSourceATaskGivesAndTheExtensionOfAFailingHook(t *testing.T) {
	type hookHandlers struct{ fail func() error }
	hooked := oneStep(t, "test.hooked", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		must(t, rt.Report(errors.New("from an extension"), ReportFrom{Extension: "helper"}))
		must(t, rt.Report(errors.New("from the host"), ReportFrom{Callback: durable.CallbackModels}))
		if err := EachHook(ctx, rt, func(h *hookHandlers) error { return h.fail() }); err != nil {
			return err
		}
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{hooked}, openOptions{})
	must(t, o.registry.Install(&Extension{Name: "hooks", Hooks: []HookRegistration{Hook(hooked, &hookHandlers{fail: func() error { return errors.New("hook") }})}}))
	id := start(t, o.root, hooked, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, id)
	task := durable.ReportTask{ID: id, Kind: "test.hooked", ConversationID: o.root.ID}
	for message, want := range map[string]durable.ReportInfo{
		"from an extension": {Type: durable.ReportExtensionError, Extension: "helper"},
		"from the host":     {Type: durable.ReportHostError, Callback: durable.CallbackModels},
		"hook":              {Type: durable.ReportExtensionError, Extension: "hooks"},
	} {
		info, ok := o.reports.infoOf(message)
		if !ok || info.Task == nil || *info.Task != task {
			t.Fatalf("report %q = %+v (found %v), want it about %+v", message, info, ok, task)
		}
		info.Task = nil
		if info != want {
			t.Fatalf("report %q = %+v, want %+v", message, info, want)
		}
	}
}
