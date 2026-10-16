package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Task is an executable durable state machine definition, registered by Name
// in a registry: pi's erased AnyTask. Input and checkpoints cross it as JSON;
// DefineTask builds one with typed values.
type Task interface {
	durable.TaskDefinition
	// Phases returns the handler of every checkpoint phase, by phase name.
	Phases() map[string]PhaseHandler
	// Abort runs in a fresh invocation after an abort mark and must commit a
	// terminal outcome.
	Abort(ctx context.Context, task *durable.TaskRecord, runtime *TaskRuntime) error
	// HasMigration reports whether Migrate can convert older records.
	HasMigration() bool
	// Migrate converts a record stored by an older supported version; it runs
	// at reservation, and only when HasMigration.
	Migrate(input, checkpoint json.RawMessage, fromVersion int) (migratedInput, migratedCheckpoint json.RawMessage, err error)
}

// PhaseHandler runs one checkpoint phase of a running task. It must commit a
// changed checkpoint or a terminal outcome through the runtime; returning
// without durable progress faults the task.
type PhaseHandler func(ctx context.Context, task *durable.TaskRecord, runtime *TaskRuntime) error

// Running is a running task with its input and checkpoint decoded, as a typed
// phase handler receives it.
type Running[I, S any] struct {
	Record     *durable.TaskRecord
	Input      I
	Checkpoint S
}

// TaskSpec is a typed task definition: I the input, S the checkpoint, which
// encodes to a JSON object with a string "phase" naming one of Phases, and R
// the result.
type TaskSpec[I, S, R any] struct {
	// Name is the registered task kind persisted in TaskRecord.Kind.
	Name string
	// Version is the definition version persisted with live input and
	// checkpoints; positive.
	Version int
	// Initial is the first durable checkpoint of a newly created task.
	Initial func(input I) S
	// Phases is the exhaustive phase map.
	Phases map[string]func(ctx context.Context, task *Running[I, S], runtime *TaskRuntime) error
	// Abort runs in a fresh invocation after an abort mark and must commit a
	// terminal outcome.
	Abort func(ctx context.Context, task *Running[I, S], runtime *TaskRuntime) error
	// Migrate, when set, converts a record stored by an older version.
	Migrate func(input, checkpoint json.RawMessage, fromVersion int) (I, S, error)
}

// DefineTask builds a Task from a typed spec, decoding input and checkpoints
// before each handler runs.
func DefineTask[I, S, R any](spec TaskSpec[I, S, R]) (Task, error) {
	if spec.Name == "" {
		return nil, errors.New("Task name must be a non-empty string")
	}
	if spec.Version <= 0 {
		return nil, fmt.Errorf("Task %s version must be a positive integer", spec.Name)
	}
	if spec.Initial == nil || spec.Abort == nil {
		return nil, fmt.Errorf("Task %s needs Initial and Abort", spec.Name)
	}
	if len(spec.Phases) == 0 {
		return nil, fmt.Errorf("Task %s has no phases", spec.Name)
	}
	t := &typedTask[I, S, R]{spec: spec, phases: map[string]PhaseHandler{}}
	for phase, handler := range spec.Phases {
		if handler == nil {
			return nil, fmt.Errorf("Task %s phase %s has no handler", spec.Name, phase)
		}
		t.phases[phase] = func(ctx context.Context, record *durable.TaskRecord, runtime *TaskRuntime) error {
			running, err := decodeRunning[I, S](record)
			if err != nil {
				return err
			}
			return handler(ctx, running, runtime)
		}
	}
	return t, nil
}

// MustDefineTask is DefineTask for package-level tasks, panicking on an
// invalid spec.
func MustDefineTask[I, S, R any](spec TaskSpec[I, S, R]) Task {
	t, err := DefineTask(spec)
	if err != nil {
		panic(err)
	}
	return t
}

type typedTask[I, S, R any] struct {
	spec   TaskSpec[I, S, R]
	phases map[string]PhaseHandler
}

func (t *typedTask[I, S, R]) Name() string                    { return t.spec.Name }
func (t *typedTask[I, S, R]) Version() int                    { return t.spec.Version }
func (t *typedTask[I, S, R]) Phases() map[string]PhaseHandler { return t.phases }

func (t *typedTask[I, S, R]) Initial(input json.RawMessage) (json.RawMessage, error) {
	var in I
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("Task %s input: %w", t.spec.Name, err)
	}
	checkpoint, err := json.Marshal(t.spec.Initial(in))
	if err != nil {
		return nil, fmt.Errorf("Task %s checkpoint: %w", t.spec.Name, err)
	}
	phase, err := CheckpointPhase(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("Task %s checkpoint: %w", t.spec.Name, err)
	}
	if _, ok := t.phases[phase]; !ok {
		return nil, fmt.Errorf("Task %s has no phase %s", t.spec.Name, phase)
	}
	return checkpoint, nil
}

func (t *typedTask[I, S, R]) Abort(ctx context.Context, record *durable.TaskRecord, runtime *TaskRuntime) error {
	running, err := decodeRunning[I, S](record)
	if err != nil {
		return err
	}
	return t.spec.Abort(ctx, running, runtime)
}

func (t *typedTask[I, S, R]) HasMigration() bool { return t.spec.Migrate != nil }

func (t *typedTask[I, S, R]) Migrate(input, checkpoint json.RawMessage, fromVersion int) (json.RawMessage, json.RawMessage, error) {
	if t.spec.Migrate == nil {
		return nil, nil, fmt.Errorf("Task %s has no migration", t.spec.Name)
	}
	in, state, err := t.spec.Migrate(input, checkpoint, fromVersion)
	if err != nil {
		return nil, nil, err
	}
	migratedInput, err := json.Marshal(in)
	if err != nil {
		return nil, nil, err
	}
	migratedCheckpoint, err := json.Marshal(state)
	if err != nil {
		return nil, nil, err
	}
	return migratedInput, migratedCheckpoint, nil
}

func decodeRunning[I, S any](record *durable.TaskRecord) (*Running[I, S], error) {
	running := &Running[I, S]{Record: record}
	if err := json.Unmarshal(record.Input, &running.Input); err != nil {
		return nil, fmt.Errorf("Task %s input: %w", record.Kind, err)
	}
	if err := json.Unmarshal(record.State.Checkpoint, &running.Checkpoint); err != nil {
		return nil, fmt.Errorf("Task %s checkpoint: %w", record.Kind, err)
	}
	return running, nil
}

// CheckpointPhase returns the "phase" of a checkpoint, which must be a JSON
// object with a string phase.
func CheckpointPhase(checkpoint json.RawMessage) (string, error) {
	var head struct {
		Phase *string `json:"phase"`
	}
	if err := json.Unmarshal(checkpoint, &head); err != nil || head.Phase == nil {
		return "", errors.New("a checkpoint must be a JSON object with a string phase")
	}
	return *head.Phase, nil
}

// CreateTask creates a task with a typed input in tx: tx.CreateTask with the
// input encoded.
func CreateTask[I any](tx *durable.Tx, task Task, input I, options durable.TaskOptions) (durable.TaskID, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return 0, fmt.Errorf("Task %s input: %w", task.Name(), err)
	}
	return tx.CreateTask(task, raw, options)
}
