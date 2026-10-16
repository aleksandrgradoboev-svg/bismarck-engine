package harness

import (
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/uuidv7"
)

func always(map[string]any, []delta.Op, durable.CheckpointInfo) bool { return true }

// LiveDoc is the built-in live document: the generation and tool calls that
// are running. Its operations arrive with the live stage of the port.
var LiveDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:    "pi.live",
	Version: 1,
	Scope:   durable.ScopeConversation,
	History: "latest",
	Fork:    "initial",
	Initial: func(any) map[string]any { return map[string]any{} },
	// A complete base whenever nothing runs: no generation and no running
	// tool slot. That holds while idle, in the commit handing a generation
	// over to its tool round, and between tools, so the delta chain spans at
	// most one generation or the overlapping execution of one round's tools.
	CheckpointWhen: func(value map[string]any, _ []delta.Op, _ durable.CheckpointInfo) bool {
		if _, running := value["generation"]; running {
			return false
		}
		return !anySlotRunning(value["tools"]) && !anySlotRunning(value["nestedTools"])
	},
})

func anySlotRunning(slots any) bool {
	list, _ := slots.([]any)
	for _, slot := range list {
		if fields, ok := slot.(map[string]any); ok && fields["status"] == "running" {
			return true
		}
	}
	return false
}

// InboxDoc is the built-in inbox: queued submissions. Its operations arrive
// with the submissions stage of the port.
var InboxDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:    "pi.inbox",
	Version: 1,
	Scope:   durable.ScopeConversation,
	History: "latest",
	Fork:    "initial",
	Initial: func(any) map[string]any { return map[string]any{"items": []any{}} },
	CheckpointWhen: func(value map[string]any, _ []delta.Op, _ durable.CheckpointInfo) bool {
		items, _ := value["items"].([]any)
		return len(items) == 0
	},
})

// UsageState is the ledger of one conversation's own spend: its entries, and
// compaction summarization attempts, which have none.
type UsageState struct {
	// Models holds assistant entries and summarization attempts, keyed
	// "provider/modelId".
	Models map[string]ai.Usage `json:"models"`
	// Tools holds tool results, keyed by tool name; their usage has no model
	// identity.
	Tools map[string]ai.Usage `json:"tools"`
}

// UsageDoc is the built-in usage ledger of a conversation.
var UsageDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:    "pi.usage",
	Version: 1,
	Scope:   durable.ScopeConversation,
	History: "latest",
	Fork:    "initial",
	Initial: func(any) map[string]any {
		return map[string]any{"models": map[string]any{}, "tools": map[string]any{}}
	},
	CheckpointWhen: always,
})

// addUsage adds every counter of usage to total; optional counters are added
// once either side reports them (Go keeps them as zero when unreported).
func addUsage(total *ai.Usage, usage ai.Usage) {
	total.Input += usage.Input
	total.Output += usage.Output
	total.CacheRead += usage.CacheRead
	total.CacheWrite += usage.CacheWrite
	total.TotalTokens += usage.TotalTokens
	total.CacheWrite1h += usage.CacheWrite1h
	total.Reasoning += usage.Reasoning
	total.Cost.Input += usage.Cost.Input
	total.Cost.Output += usage.Cost.Output
	total.Cost.CacheRead += usage.Cost.CacheRead
	total.Cost.CacheWrite += usage.Cost.CacheWrite
	total.Cost.Total += usage.Cost.Total
}

// addUsageState adds every bucket of state into sum.
func addUsageState(sum *UsageState, state UsageState) {
	for _, bucket := range []struct{ into, from map[string]ai.Usage }{
		{sum.Models, state.Models},
		{sum.Tools, state.Tools},
	} {
		for key, usage := range bucket.from {
			total := bucket.into[key]
			addUsage(&total, usage)
			bucket.into[key] = total
		}
	}
}

// ProviderState is the stable provider-facing identity of one conversation.
type ProviderState struct {
	SessionID string `json:"sessionId"`
}

// ProviderDoc is the built-in provider state; every fork starts with a fresh
// identity instead of copying its parent.
var ProviderDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:    "pi.provider",
	Version: 1,
	Scope:   durable.ScopeConversation,
	History: "latest",
	Fork:    "initial",
	// pi's initial() throws when no id can be minted, failing the creating
	// commit; a Go initial cannot fail, so it panics, which the Session's hook
	// guard turns into the same failed operation.
	Initial:        func(any) map[string]any { return map[string]any{"sessionId": uuidv7.Must()} },
	CheckpointWhen: always,
})
