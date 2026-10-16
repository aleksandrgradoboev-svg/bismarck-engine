package harness

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Harness test support (pi packages/durable/test/harness-support.ts).

var bg = context.Background()

// recordingStorage is memory storage that records the write types of every
// commit and survives Close, so a reopen reads its records again, as a fresh
// process would read a database.
type recordingStorage struct {
	*memory.Storage
	mu      sync.Mutex
	commits [][]string
}

func newRecordingStorage() *recordingStorage { return &recordingStorage{Storage: memory.New()} }

func (s *recordingStorage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	seq, err := s.Storage.Commit(ctx, writes)
	if err == nil {
		types := make([]string, len(writes))
		for i, write := range writes {
			types[i] = write.WriteType()
		}
		s.mu.Lock()
		s.commits = append(s.commits, types)
		s.mu.Unlock()
	}
	return seq, err
}

func (s *recordingStorage) Close(context.Context) error { return nil }

func (s *recordingStorage) commitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commits)
}

func (s *recordingStorage) commit(i int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits[i]
}

func testTool(name string) *ToolRegistration {
	return &ToolRegistration{
		Tool: ai.Tool{Name: name, Description: name + " tool", Parameters: objectSchema()},
		Execute: func(context.Context, map[string]any, *ToolExecutionAPI) (ToolExecutionResult, error) {
			return ToolExecutionResult{Output: ai.ContentList{}}, nil
		},
	}
}

func objectSchema() *ai.Schema {
	var schema ai.Schema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{}}`), &schema); err != nil {
		panic(err)
	}
	return &schema
}

// openHarness opens a Harness over storage with a fresh registry holding the
// named tools; it closes the Harness when the test ends.
func openHarness(t *testing.T, storage durable.Storage, toolNames ...string) (*Harness, Registry) {
	t.Helper()
	registry := NewRegistry()
	if len(toolNames) > 0 {
		tools := make([]*ToolRegistration, len(toolNames))
		for i, name := range toolNames {
			tools[i] = testTool(name)
		}
		must(t, registry.Install(&Extension{Name: "tools", Tools: tools}))
	}
	h, err := Open(bg, storage, Options{Registry: registry})
	must(t, err)
	t.Cleanup(func() { _ = h.Close(bg) })
	return h, registry
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func user(text string) ai.UserMessage {
	return ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, Timestamp: 1}
}

// addTool installs a one-tool extension named after the tool.
func addTool(t *testing.T, registry Registry, tool *ToolRegistration) {
	t.Helper()
	must(t, registry.Install(&Extension{Name: "tool:" + tool.Name, Tools: []*ToolRegistration{tool}}))
}

// addSection installs a one-section extension named after the section.
func addSection(t *testing.T, registry Registry, section *PromptSection) {
	t.Helper()
	must(t, registry.Install(&Extension{Name: "section:" + section.Key, Sections: []*PromptSection{section}}))
}

func names[T, K any](items []T, name func(T) K) []K {
	out := make([]K, len(items))
	for i, item := range items {
		out[i] = name(item)
	}
	return out
}

func toolNames(tools []*ToolRegistration) []string { return names(tools, toolName) }

func extensionNames(extensions []*Extension) []string { return names(extensions, extensionName) }

func ptr[T any](v T) *T { return &v }
