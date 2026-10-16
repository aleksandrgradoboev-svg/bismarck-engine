package harness

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
)

// sectionKey is the shape of a prompt section key.
var sectionKey = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// builtinTasks are the task definitions every registry holds; they are not an
// extension and cannot be removed or replaced. The generation, tool and
// compaction tasks join them with the stages of the port that bring them.
var builtinTasks []Task

// Extension is a named bundle of code, installed in a registry and selected by
// conversations by name.
type Extension struct {
	Name     string
	Tools    []*ToolRegistration
	Sections []*PromptSection
	Hooks    []HookRegistration
	// Wraps apply where this extension is selected, in order.
	Wraps []Wrap
	// Tasks are resolved by name for every task, whichever conversations
	// select this extension.
	Tasks []Task
}

// HookRegistration is hook handlers for the tasks named Task. Handlers is the
// hook struct of that task, such as the generation hooks, with the hooks it
// does not set left nil.
type HookRegistration struct {
	Task     string
	Handlers any
}

// Hook registers handlers for tasks with task's name.
func Hook(task Task, handlers any) HookRegistration {
	return HookRegistration{Task: task.Name(), Handlers: handlers}
}

// Wrap targets a tool name or a section key; built by WrapTool and
// WrapSection. A wrapper returns the replacement, keeps the name, and is pure.
type Wrap struct {
	tool    string
	section string
	wrapT   func(*ToolRegistration) (*ToolRegistration, error)
	wrapS   func(*PromptSection) (*PromptSection, error)
}

// WrapTool wraps the tool named name wherever the wrapping extension is
// selected.
func WrapTool(name string, wrapper func(tool *ToolRegistration) (*ToolRegistration, error)) Wrap {
	return Wrap{tool: name, wrapT: wrapper}
}

// WrapSection wraps the section key wherever the wrapping extension is
// selected.
func WrapSection(key string, wrapper func(section *PromptSection) (*PromptSection, error)) Wrap {
	return Wrap{section: key, wrapS: wrapper}
}

// ExtensionTool is one installed tool with its extension.
type ExtensionTool struct {
	Extension *Extension
	Tool      *ToolRegistration
}

// ExtensionSection is one installed section with its extension.
type ExtensionSection struct {
	Extension *Extension
	Section   *PromptSection
}

// RegistrySnapshot is the immutable view of one published registry state.
type RegistrySnapshot struct {
	extensions []*Extension
	byName     map[string]*Extension
	tasks      []Task
	taskByName map[string]Task
}

func newSnapshot(extensions []*Extension) (*RegistrySnapshot, error) {
	s := &RegistrySnapshot{
		extensions: extensions,
		byName:     make(map[string]*Extension, len(extensions)),
		taskByName: map[string]Task{},
	}
	for _, extension := range extensions {
		s.byName[extension.Name] = extension
	}
	for _, task := range builtinTasks {
		s.tasks = append(s.tasks, task)
		s.taskByName[task.Name()] = task
	}
	for _, extension := range extensions {
		for _, task := range extension.Tasks {
			name := task.Name()
			if _, ok := s.taskByName[name]; ok {
				return nil, fmt.Errorf("Task %s of extension %s is already installed", name, extension.Name)
			}
			s.tasks = append(s.tasks, task)
			s.taskByName[name] = task
		}
	}
	return s, nil
}

// Installed returns the installed extensions in install order.
func (s *RegistrySnapshot) Installed() []*Extension {
	return append([]*Extension(nil), s.extensions...)
}

// Extension returns the installed extension named name, or nil.
func (s *RegistrySnapshot) Extension(name string) *Extension { return s.byName[name] }

// Tools returns every installed tool with its extension, in install order.
// Names may repeat across extensions.
func (s *RegistrySnapshot) Tools() []ExtensionTool {
	var tools []ExtensionTool
	for _, extension := range s.extensions {
		for _, tool := range extension.Tools {
			tools = append(tools, ExtensionTool{Extension: extension, Tool: tool})
		}
	}
	return tools
}

// Sections returns every installed section with its extension, in install
// order.
func (s *RegistrySnapshot) Sections() []ExtensionSection {
	var sections []ExtensionSection
	for _, extension := range s.extensions {
		for _, section := range extension.Sections {
			sections = append(sections, ExtensionSection{Extension: extension, Section: section})
		}
	}
	return sections
}

// Tasks returns the built-in and installed task definitions.
func (s *RegistrySnapshot) Tasks() []Task { return append([]Task(nil), s.tasks...) }

// Task returns the task definition named name, or nil.
func (s *RegistrySnapshot) Task(name string) Task { return s.taskByName[name] }

// RegistryReader is the read side of a registry a Harness consumes.
type RegistryReader interface {
	// Snapshot returns the immutable view of the whole current registry.
	Snapshot() *RegistrySnapshot
	// Subscribe calls listener synchronously after every publication; the
	// returned function unsubscribes.
	Subscribe(listener func()) (unsubscribe func())
}

// Registry is an application-owned registry of extensions.
type Registry interface {
	RegistryReader
	// Install installs extension, or replaces the installed extension with its
	// name in place, and publishes at once. An invalid result publishes
	// nothing.
	Install(extension *Extension) error
	// Uninstall removes the installed extension named like extension,
	// whichever object it is. A later install appends.
	Uninstall(extension *Extension)
}

// NewRegistry creates a registry holding only the built-in tasks.
func NewRegistry() Registry {
	current, _ := newSnapshot(nil)
	return &registry{current: current, listeners: map[int]func(){}}
}

type registry struct {
	mu        sync.Mutex
	current   *RegistrySnapshot
	listeners map[int]func()
	order     []int
	nextID    int
}

func (r *registry) Snapshot() *RegistrySnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

func (r *registry) Subscribe(listener func()) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextID
	r.nextID++
	r.listeners[id] = listener
	r.order = append(r.order, id)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.listeners, id)
	}
}

func (r *registry) Install(extension *Extension) error {
	if err := validateExtension(extension); err != nil {
		return err
	}
	r.mu.Lock()
	current := r.current.extensions
	next := make([]*Extension, 0, len(current)+1)
	replaced := false
	for _, installed := range current {
		if installed.Name == extension.Name {
			next = append(next, extension)
			replaced = true
		} else {
			next = append(next, installed)
		}
	}
	if !replaced {
		next = append(next, extension)
	}
	return r.publishLocked(next)
}

func (r *registry) Uninstall(extension *Extension) {
	r.mu.Lock()
	current := r.current.extensions
	next := make([]*Extension, 0, len(current))
	for _, installed := range current {
		if installed.Name != extension.Name {
			next = append(next, installed)
		}
	}
	if len(next) == len(current) {
		r.mu.Unlock()
		return
	}
	_ = r.publishLocked(next)
}

// publishLocked builds and validates the next state, which fails on a task
// name collision, then publishes it and calls the listeners after unlocking.
func (r *registry) publishLocked(extensions []*Extension) error {
	next, err := newSnapshot(extensions)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	r.current = next
	var listeners []func()
	live := r.order[:0]
	for _, id := range r.order {
		if listener, ok := r.listeners[id]; ok {
			listeners = append(listeners, listener)
			live = append(live, id)
		}
	}
	r.order = live
	r.mu.Unlock()
	for _, listener := range listeners {
		listener()
	}
	return nil
}

// validateExtension checks unique tool names and section keys within one
// extension, and valid, unreserved section keys.
func validateExtension(extension *Extension) error {
	if extension == nil || extension.Name == "" {
		return errors.New("An extension needs a name")
	}
	tools := map[string]bool{}
	for _, tool := range extension.Tools {
		if tools[tool.Name] {
			return fmt.Errorf("Extension %s has two tools named %s", extension.Name, tool.Name)
		}
		tools[tool.Name] = true
	}
	sections := map[string]bool{}
	for _, section := range extension.Sections {
		key := section.Key
		if !sectionKey.MatchString(key) {
			return fmt.Errorf("Section key %s must match %s", strconv.Quote(key), "/"+sectionKey.String()+"/")
		}
		if key == InstructionsKey {
			return fmt.Errorf("Section key %s is reserved for the agent's instructions", key)
		}
		if sections[key] {
			return fmt.Errorf("Extension %s has two sections with key %s", extension.Name, key)
		}
		sections[key] = true
	}
	return nil
}
