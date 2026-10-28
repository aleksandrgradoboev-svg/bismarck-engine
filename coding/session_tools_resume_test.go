package coding

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// The resume cases of pi's default-tools-setting.test.ts (6f2f164a, #10785;
// bd3c9b82, #11062 and #11075): a session on the default tools takes the
// loadout its branch records, with +name/-name applied; extension tools the
// branch never mentioned are activated; tools removed before a compaction stay
// removed; /tree navigation restores the target branch's loadout.

// resumeBranch writes a session file of entries, each the child of the one
// before, and loads it as a tree.
type resumeBranch struct {
	t     *testing.T
	lines []string
	last  string
}

func newResumeBranch(t *testing.T) *resumeBranch {
	return &resumeBranch{t: t, lines: []string{`{"type":"session","version":3,"id":"0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`}}
}

func (b *resumeBranch) append(entry map[string]any) string {
	id := fmt.Sprintf("e%d", len(b.lines))
	entry["id"], entry["timestamp"] = id, "2026-01-01T00:00:01.000Z"
	if b.last == "" {
		entry["parentId"] = nil
	} else {
		entry["parentId"] = b.last
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		b.t.Fatal(err)
	}
	b.lines = append(b.lines, string(raw))
	b.last = id
	return id
}

// recordLoadoutChange is pi's: a system message that adds and removes tools.
func (b *resumeBranch) recordLoadoutChange(added []string, removed ...string) string {
	message := map[string]any{"role": "system", "content": []any{}, "timestamp": 1}
	if len(added) > 0 {
		var tools []any
		for _, name := range added {
			tools = append(tools, map[string]any{"name": name, "description": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
		}
		message["toolsAdded"] = tools
	}
	if len(removed) > 0 {
		var refs []any
		for _, name := range removed {
			refs = append(refs, map[string]any{"name": name})
		}
		message["toolsRemoved"] = refs
	}
	return b.append(map[string]any{"type": "message", "message": message})
}

func (b *resumeBranch) user(text string) string {
	return b.append(map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}, "timestamp": 2}})
}

func (b *resumeBranch) compaction(firstKept string) string {
	return b.append(map[string]any{"type": "compaction", "summary": "summary", "firstKeptEntryId": firstKept, "tokensBefore": 1000})
}

func (b *resumeBranch) tree() *SessionTree {
	tree, err := LoadSessionTree(writeSessionLines(b.t, b.lines))
	if err != nil {
		b.t.Fatal(err)
	}
	return tree
}

// createChangedSession is pi's: it recorded read, grep and active_tool, then
// activated inactive_tool and deactivated active_tool.
func createChangedSession(t *testing.T) *resumeBranch {
	b := newResumeBranch(t)
	b.recordLoadoutChange([]string{"read", "grep", "active_tool"})
	b.recordLoadoutChange([]string{"inactive_tool"}, "active_tool")
	return b
}

// resumeWithExtensionTools is pi's resume: the extension registers
// inactive_tool (defaultActive false) and active_tool, and then the session
// resumes the branch — in the engine, extension tools register through
// RegisterTools, so a host registers them before LoadBranch.
func resumeWithExtensionTools(t *testing.T, tree *SessionTree, opts SessionOptions) *Session {
	t.Helper()
	sess := registrySession(t, opts)
	off := false
	sess.RegisterTools(
		ToolDefinition{Tool: echoTool("inactive_tool"), DefaultActive: &off},
		ToolDefinition{Tool: echoTool("active_tool")},
	)
	sess.LoadBranch(tree.BuildProjection())
	return sess
}

func sortedActive(s *Session) []string {
	return slices.Sorted(slices.Values(s.ActiveToolNames()))
}

func TestResumeRestoresTheLoadoutTheTranscriptRecords(t *testing.T) {
	sess := resumeWithExtensionTools(t, createChangedSession(t).tree(), SessionOptions{})
	if got := sortedActive(sess); !slices.Equal(got, []string{"grep", "inactive_tool", "read"}) {
		t.Fatalf("active %v", got)
	}
}

func TestResumeActivatesExtensionToolsTheTranscriptNeverMentioned(t *testing.T) {
	// Built-in tools and tools registered inactive stay inactive.
	b := newResumeBranch(t)
	b.recordLoadoutChange([]string{"read", "grep"})
	sess := resumeWithExtensionTools(t, b.tree(), SessionOptions{})
	if got := sortedActive(sess); !slices.Equal(got, []string{"active_tool", "grep", "read"}) {
		t.Fatalf("active %v", got)
	}
}

func TestResumeKeepsToolsRemovedBeforeACompactionInactive(t *testing.T) {
	b := createChangedSession(t)
	kept := b.user("hi")
	b.compaction(kept)
	tree := b.tree()
	for _, m := range tree.BuildProjection().Messages {
		if _, ok := m.(ai.SystemMessage); ok {
			t.Fatal("the compacted context still holds a system message; the case needs it gone")
		}
	}
	sess := resumeWithExtensionTools(t, tree, SessionOptions{})
	if got := sortedActive(sess); !slices.Equal(got, []string{"grep", "inactive_tool", "read"}) {
		t.Fatalf("active %v", got)
	}
	// Beyond pi's suite: /tree navigation reads the whole branch too.
	sess.SetActiveTools([]string{"bash"})
	sess.RestoreToolsFromBranch(tree.BuildProjection())
	if got := sortedActive(sess); !slices.Equal(got, []string{"grep", "inactive_tool", "read"}) {
		t.Fatalf("after navigation %v", got)
	}
}

// Beyond pi's suite: an extension tool the branch added is mentioned, so it
// is not appended a second time as a new tool, which a -name entry, removing
// the first occurrence only, would leave active.
func TestResumeRemovesARecordedExtensionToolWithAModifier(t *testing.T) {
	b := newResumeBranch(t)
	b.recordLoadoutChange([]string{"read", "active_tool"})
	sess := resumeWithExtensionTools(t, b.tree(), SessionOptions{ToolNames: []string{"-active_tool"}})
	if got := sortedActive(sess); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("active %v", got)
	}
}

// Beyond pi's suite: a custom tool that replaces a built-in keeps the
// built-in's name in pi's _baseToolDefinitions, so it follows the default
// selection rather than being activated as a new extension tool.
func TestResumeKeepsAReplacedBuiltInTheBranchNeverMentionedInactive(t *testing.T) {
	b := newResumeBranch(t)
	b.recordLoadoutChange([]string{"read"})
	sess := registrySession(t, SessionOptions{CustomTools: []agent.AgentTool{echoTool("grep")}})
	sess.LoadBranch(b.tree().BuildProjection())
	if got := sortedActive(sess); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("active %v", got)
	}
}

func TestTreeNavigationRestoresTheTargetBranchsLoadout(t *testing.T) {
	// The target branch never mentions active_tool, so it is active there. It
	// removed grep.
	b := newResumeBranch(t)
	b.recordLoadoutChange([]string{"read", "grep"})
	b.recordLoadoutChange(nil, "grep")
	target := b.user("first")
	b.recordLoadoutChange([]string{"grep"}, "active_tool")
	tree := b.tree()
	sess := resumeWithExtensionTools(t, tree, SessionOptions{})
	if got := sortedActive(sess); !slices.Equal(got, []string{"grep", "read"}) {
		t.Fatalf("resumed %v", got)
	}
	sess.RestoreToolsFromBranch(tree.BuildProjection(target))
	if got := sortedActive(sess); !slices.Equal(got, []string{"active_tool", "read"}) {
		t.Fatalf("after navigation %v", got)
	}
}

func TestTreeNavigationKeepsRegisteredToolsOutsideTheAllowlistInactive(t *testing.T) {
	// An allowlist without mcp__ entries keeps MCP tools registered but
	// inactive.
	b := newResumeBranch(t)
	b.recordLoadoutChange([]string{"read"})
	target := b.user("first")
	tree := b.tree()
	sess := registrySession(t, SessionOptions{ToolNames: []string{"read"}, CustomTools: []agent.AgentTool{echoTool("mcp__server__lookup")}})
	sess.LoadBranch(tree.BuildProjection())
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("resumed %v", got)
	}
	sess.RestoreToolsFromBranch(tree.BuildProjection(target))
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("after navigation %v", got)
	}
	if !slices.Contains(registeredNames(sess), "mcp__server__lookup") {
		t.Fatal("MCP tool not registered")
	}
}

func TestResumeDoesNotRestoreExcludedTools(t *testing.T) {
	sess := resumeWithExtensionTools(t, createChangedSession(t).tree(), SessionOptions{ExcludeTools: []string{"inactive_tool"}})
	if got := sortedActive(sess); !slices.Equal(got, []string{"grep", "read"}) {
		t.Fatalf("active %v", got)
	}
	// pi: excluded tools never register, so they are not kept pending.
	if slices.Contains(sess.PendingToolNames(), "inactive_tool") {
		t.Fatalf("pending %v", sess.PendingToolNames())
	}
}

func TestResumeUsesExplicitToolOptionsInsteadOfTheRecordedLoadout(t *testing.T) {
	allowlisted := resumeWithExtensionTools(t, createChangedSession(t).tree(), SessionOptions{ToolNames: []string{"read", "write"}})
	if got := allowlisted.ActiveToolNames(); !slices.Equal(got, []string{"read", "write"}) {
		t.Fatalf("allowlisted %v", got)
	}
	builtinless := resumeWithExtensionTools(t, createChangedSession(t).tree(), SessionOptions{NoTools: NoToolsBuiltin})
	if got := builtinless.ActiveToolNames(); !slices.Equal(got, []string{"active_tool"}) {
		t.Fatalf("builtinless %v", got)
	}
}

func TestResumeAppliesToolModifiersToTheRecordedLoadout(t *testing.T) {
	sess := resumeWithExtensionTools(t, createChangedSession(t).tree(), SessionOptions{ToolNames: []string{"-grep", "+write"}})
	if got := sortedActive(sess); !slices.Equal(got, []string{"inactive_tool", "read", "write"}) {
		t.Fatalf("active %v", got)
	}
}

func TestResumeUsesTheDefaultToolsWhenTheTranscriptRecordsNoLoadout(t *testing.T) {
	b := newResumeBranch(t)
	b.user("hi")
	sess := resumeWithExtensionTools(t, b.tree(), SessionOptions{})
	if got := sortedActive(sess); !slices.Equal(got, []string{"active_tool", "bash", "edit", "read", "write"}) {
		t.Fatalf("active %v", got)
	}
}
