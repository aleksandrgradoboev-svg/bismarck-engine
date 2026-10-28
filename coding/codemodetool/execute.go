package codemodetool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding/toolsearch"
)

// The executor, ported from pi extensions/codemode/execute.ts (v1.2.0):
// one script runs in the codemode sandbox; nested calls go through the
// session's tool pipeline (Session.ExecuteTool), so validation and the
// before/after hooks apply as for direct calls. Only the script's output
// reaches the model.

const (
	argsPreviewChars  = 200
	errorPreviewChars = 500
	// memoryLimitBytes is the QuickJS heap limit; overruns throw
	// `InternalError: out of memory` inside the script.
	memoryLimitBytes = 256 * 1024 * 1024
	// defaultMaxOutputTokens is the default token budget for script output.
	defaultMaxOutputTokens = 10_000
	outputCharsPerToken    = 4
)

// Session is the session as the codemode tool uses it; *coding.Session fits.
type Session interface {
	CallableTools() []agent.AgentTool
	AllTools() []coding.ToolDefinition
	ExecuteTool(ctx context.Context, parentToolCallID, name string, args map[string]any, onUpdate func(agent.AgentToolResult)) agent.AgentToolCallOutcome
}

// CallStatus is a nested call's status.
type CallStatus string

const (
	CallRunning   CallStatus = "running"
	CallOK        CallStatus = "ok"
	CallError     CallStatus = "error"
	CallCancelled CallStatus = "cancelled"
)

// NestedCall is one nested tool call as the result's details record it (pi
// CodemodeNestedCall).
type NestedCall struct {
	// ID is the nested call's tool call id, <codemode call id>/<n>.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Args is compact JSON of the arguments, truncated for display.
	Args       string     `json:"args"`
	Status     CallStatus `json:"status"`
	DurationMs *float64   `json:"durationMs,omitempty"`
	// Error is the error text, truncated for display.
	Error string `json:"error,omitempty"`
}

// Details are the codemode result's details (pi CodemodeToolDetails).
type Details struct {
	Calls []NestedCall `json:"calls"`
	// FullOutputPath is the temp file with the full text output, when the
	// output was truncated.
	FullOutputPath string `json:"fullOutputPath,omitempty"`
}

// StoreEntryType is the custom entry type holding one script's store()
// writes (pi CODEMODE_STORE_ENTRY_TYPE): {set, delete}.
const StoreEntryType = "codemode-store"

// storeEntryData is a codemode-store entry's data (pi CodemodeStoreEntryData).
type storeEntryData struct {
	Set    map[string]json.RawMessage `json:"set"`
	Delete []string                   `json:"delete"`
}

// EntryLog is the part of a recorded session that keeps store() values
// (*coding.Session has it): the codemode-store entries on its branch and a
// way to append one. ok false means the session is not recorded.
type EntryLog interface {
	CustomEntries(customType string) (data []json.RawMessage, ok bool)
	AppendCustomEntry(customType string, data any) (id string, ok bool)
}

// readStore folds the codemode-store entries on a branch from the root,
// deletions before values in each, skipping malformed data (pi
// readCodemodeStore with isStoreEntryData).
func readStore(entries []json.RawMessage) map[string]any {
	values := map[string]any{}
	for _, raw := range entries {
		set, deleted, ok := parseStoreEntry(raw)
		if !ok {
			continue
		}
		for _, key := range deleted {
			delete(values, key)
		}
		for key, value := range set {
			values[key] = value
		}
	}
	return values
}

// parseStoreEntry is pi isStoreEntryData: an object whose set is a non-null
// object and whose delete is an array of strings. set may be an array, which
// is an object to typeof: Object.entries gives its indices as keys.
func parseStoreEntry(raw json.RawMessage) (set map[string]json.RawMessage, deleted []string, ok bool) {
	var data struct {
		Set    json.RawMessage   `json:"set"`
		Delete []json.RawMessage `json:"delete"`
	}
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &data) != nil || data.Delete == nil {
		return nil, nil, false
	}
	for _, key := range data.Delete {
		var s string
		if len(key) == 0 || key[0] != '"' || json.Unmarshal(key, &s) != nil {
			return nil, nil, false
		}
		deleted = append(deleted, s)
	}
	switch {
	case len(data.Set) > 0 && data.Set[0] == '{':
		if json.Unmarshal(data.Set, &set) != nil {
			return nil, nil, false
		}
	case len(data.Set) > 0 && data.Set[0] == '[':
		var items []json.RawMessage
		if json.Unmarshal(data.Set, &items) != nil {
			return nil, nil, false
		}
		set = make(map[string]json.RawMessage, len(items))
		for i, item := range items {
			set[strconv.Itoa(i)] = item
		}
	default:
		return nil, nil, false
	}
	return set, deleted, true
}

// Store keeps store() values in memory, across calls, for a session that is
// not recorded. pi has no such case — its in-memory session manager keeps
// custom entries all the same — so this stands in for it: one set of values
// for the whole session, without branches. A recorded session keeps the
// values as codemode-store entries instead, and Store is not used.
type Store struct {
	mu     sync.Mutex
	values map[string]json.RawMessage
}

// snapshot is the values load() sees.
func (s *Store) snapshot() map[string]any {
	if s == nil {
		return map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// apply records one script's writes: deletions, then the values set (pi
// readCodemodeStore's fold).
func (s *Store) apply(w codemode.StoreWrites) {
	if s == nil || (len(w.Set) == 0 && len(w.Delete) == 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]json.RawMessage{}
	}
	for _, k := range w.Delete {
		delete(s.values, k)
	}
	for k, v := range w.Set {
		s.values[k] = v
	}
}

// utf16Truncate is pi's truncateText, counting UTF-16 code units as
// JavaScript's length and slice do.
func utf16Truncate(text string, maxChars int) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= maxChars {
		return text
	}
	return string(utf16.Decode(units[:maxChars-3])) + "..."
}

// previewArgs is the compact JSON of a script's arguments, truncated; empty
// for undefined.
func previewArgs(args json.RawMessage) string {
	if args == nil {
		return ""
	}
	return utf16Truncate(string(args), argsPreviewChars)
}

// textOf joins a result's text blocks with newlines.
func textOf(result agent.AgentToolResult) string {
	var parts []string
	for _, c := range result.Content {
		if t, ok := c.(ai.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toScriptValue is the value a script receives for a nested call: a tool that
// declares an output schema resolves to its structured content, also for error
// results that carry one (such as MCP results with isError); any other tool
// resolves to its text content. Other failures reject with the tool's error
// text.
func toScriptValue(tool agent.AgentTool, outcome agent.AgentToolCallOutcome) (any, error) {
	if tool.OutputSchema != nil && outcome.Result.StructuredContent != nil {
		return outcome.Result.StructuredContent, nil
	}
	text := textOf(outcome.Result)
	if outcome.IsError {
		if text == "" {
			text = fmt.Sprintf("Tool %q failed", tool.Name)
		}
		return nil, errors.New(text)
	}
	return text, nil
}

// stringifyIndent is JSON.stringify(value, null, 2).
func stringifyIndent(value any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// scriptArguments turns what a script passed into the arguments of a nested
// call. Undefined and null are no arguments ({}), as pi's nested runner makes
// them. pi hands any other value to the validator as it is; the engine's tool
// calls carry an object, so a value that is not one fails here with the error
// the validator reports — without reaching the tool pipeline, so no hooks run
// and the call is not recorded on the result message.
func scriptArguments(tool agent.AgentTool, raw json.RawMessage) (map[string]any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return v, nil
	}
	if tool.Parameters == nil {
		return nil, fmt.Errorf("Tool %q takes an object of arguments", tool.Name)
	}
	var lines []string
	for _, e := range tool.Parameters.ValidationErrors(value) {
		lines = append(lines, "  - "+e.Path+": "+e.Message)
	}
	if len(lines) == 0 {
		lines = []string{"Unknown validation error"}
	}
	return nil, fmt.Errorf("Validation failed for tool %q:\n%s\n\nReceived arguments:\n%s", tool.Name, strings.Join(lines, "\n"), stringifyIndent(value))
}

// valueText is the script's text() of a returned value: a string as it is,
// any other value as compact JSON.
func valueText(value json.RawMessage) string {
	var s string
	if len(value) > 0 && value[0] == '"' && json.Unmarshal(value, &s) == nil {
		return s
	}
	return string(value)
}

// formatOutput lays the script's output out so the model can tell items
// apart: with more than one text item, each starts with a `==> text N/M <==`
// line; console lines follow all other output in one <console_output> block.
func formatOutput(output []codemode.OutputItem) ai.ContentList {
	total := 0
	for _, item := range output {
		if item.Type == "text" && !item.Console {
			total++
		}
	}
	items := ai.ContentList{}
	var consoleLines []string
	index := 0
	for _, item := range output {
		switch {
		case item.Type == "image":
			items = append(items, ai.ImageContent{Data: item.Data, MimeType: item.MimeType})
		case item.Console:
			consoleLines = append(consoleLines, item.Text)
		default:
			index++
			text := item.Text
			if total > 1 {
				text = fmt.Sprintf("==> text %d/%d <==\n%s", index, total, item.Text)
			}
			items = append(items, ai.TextContent{Text: text})
		}
	}
	if len(consoleLines) > 0 {
		items = append(items, ai.TextContent{Text: "<console_output>\n" + strings.Join(consoleLines, "\n") + "\n</console_output>"})
	}
	return items
}

// joinAdjacentText joins adjacent text items into one, each part starting on
// its own line; the separator check looks at the last non-empty part.
func joinAdjacentText(items ai.ContentList) ai.ContentList {
	joined := ai.ContentList{}
	var parts []string
	lastPart := ""
	flush := func() {
		if len(parts) > 0 {
			joined = append(joined, ai.TextContent{Text: strings.Join(parts, "")})
		}
		parts, lastPart = nil, ""
	}
	for _, item := range items {
		t, ok := item.(ai.TextContent)
		if !ok {
			flush()
			joined = append(joined, item)
			continue
		}
		if lastPart != "" && !strings.HasSuffix(lastPart, "\n") {
			parts = append(parts, "\n")
			lastPart = "\n"
		}
		parts = append(parts, t.Text)
		if t.Text != "" {
			lastPart = t.Text
		}
	}
	flush()
	return joined
}

func formatCallSummary(calls []NestedCall) string {
	if len(calls) == 0 {
		return "No tool calls were made."
	}
	list := make([]string, len(calls))
	for i, c := range calls {
		list[i] = fmt.Sprintf("%s (%s)", c.Name, c.Status)
	}
	return "Tool calls made before the failure (they are not undone): " + strings.Join(list, ", ")
}

func formatError(e *codemode.Error, calls []NestedCall) string {
	var head string
	switch e.Kind {
	case codemode.ErrorScript:
		head = e.Stack
		if head == "" {
			name := e.Name
			if name == "" {
				name = "Error"
			}
			head = name + ": " + e.Message
		}
	case codemode.ErrorTimeout:
		head = "Script timed out: " + e.Message
	case codemode.ErrorAborted:
		head = "Script aborted: " + e.Message
	default:
		head = "Script sandbox failed: " + e.Message
	}
	return head + "\n\n" + formatCallSummary(calls)
}

// imageExtensions are the file extensions of the image types image() accepts.
var imageExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// saveImages saves each image to a temp file and puts a text item with its
// path before it: the model sees the image but has no other way to reach its
// bytes. Images shown more than once are saved once; a failed write becomes
// part of the label, so it does not discard the result of a script whose tool
// calls already ran.
func saveImages(items ai.ContentList) (ai.ContentList, error) {
	labels := map[string]string{}
	out := ai.ContentList{}
	for _, item := range items {
		img, ok := item.(ai.ImageContent)
		if !ok {
			out = append(out, item)
			continue
		}
		label, seen := labels[img.Data]
		if !seen {
			data, _ := base64.StdEncoding.DecodeString(img.Data)
			kind := img.MimeType + ", " + coding.FormatSize(len(data))
			extension, known := imageExtensions[img.MimeType]
			if !known {
				return nil, fmt.Errorf("No file extension for image type %s", img.MimeType)
			}
			if path, err := coding.WriteOutputFile("pi-codemode", extension, data); err == nil {
				label = fmt.Sprintf("[Image saved to %s (%s)]", path, kind)
			} else {
				label = fmt.Sprintf("[Image (%s) could not be saved: %s]", kind, err.Error())
			}
			labels[img.Data] = label
		}
		out = append(out, ai.TextContent{Text: label}, item)
	}
	return out, nil
}

// truncateOutput applies the token budget: when the combined text exceeds it,
// the text items become one item that keeps the start and end of the text,
// and images follow it. The full text is written to a temp file. Lengths and
// cuts count UTF-16 code units, as pi's do.
func truncateOutput(items ai.ContentList, maxTokens int64) (ai.ContentList, string) {
	var texts []string
	var images ai.ContentList
	for _, item := range items {
		if t, ok := item.(ai.TextContent); ok {
			texts = append(texts, t.Text)
		} else {
			images = append(images, item)
		}
	}
	combined := strings.Join(texts, "\n")
	units := utf16.Encode([]rune(combined))
	budget := maxTokens * outputCharsPerToken
	if len(texts) == 0 || int64(len(units)) <= budget {
		return items, ""
	}
	headChars := budget / 2
	tailChars := budget - headChars
	removed := int64(len(units)) - headChars - tailChars
	head := string(utf16.Decode(units[:headChars]))
	tail := ""
	if tailChars > 0 {
		tail = string(utf16.Decode(units[int64(len(units))-tailChars:]))
	}
	ceilTokens := func(n int64) int64 { return int64(math.Ceil(float64(n) / outputCharsPerToken)) }
	text := fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s…%d tokens truncated…%s",
		ceilTokens(int64(len(units))), strings.Count(combined, "\n")+1, head, ceilTokens(removed), tail)
	fullPath := ""
	if path, err := coding.WriteOutputFile("pi-codemode", ".txt", []byte(combined)); err == nil {
		text += "\n\n[Full output: " + path + " (read with offset/limit)]"
		fullPath = path
	} else {
		text += "\n\n[Could not save the full output: " + err.Error() + "]"
	}
	return append(ai.ContentList{ai.TextContent{Text: text}}, images...), fullPath
}

// isNamespaceName reports whether query names the namespace: its name or one
// of its aliases, as written or as the same script identifier.
func isNamespaceName(namespace *coding.ToolNamespace, query string) bool {
	queryID := codemode.Identifier(query)
	for _, name := range append([]string{namespace.Name}, namespace.Aliases...) {
		if name == query || codemode.Identifier(name) == queryID {
			return true
		}
	}
	return false
}

// spreadArgs decodes the arguments of a global called with spread arguments.
func spreadArgs(raw json.RawMessage) []any {
	var args []any
	if raw != nil {
		_ = json.Unmarshal(raw, &args)
	}
	return args
}

func isPositiveInteger(v any) bool {
	n, ok := v.(float64)
	return ok && !math.IsInf(n, 0) && n == math.Trunc(n) && n > 0
}

type searchEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type namespaceInfo struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Tools        []string `json:"tools"`
}

// discoveryGlobals are searchTools(), describeTool() and describeNamespace():
// ranked search and lookup over the script's nested tools and their
// namespaces.
func discoveryGlobals(tools []agent.AgentTool, samples map[string]string, namespaceOf func(string) *coding.ToolNamespace) []codemode.Tool {
	entry := func(name string) searchEntry {
		return searchEntry{Name: codemode.Identifier(name), Description: samples[name]}
	}
	return []codemode.Tool{
		{
			Name:   "searchTools",
			Spread: true,
			Execute: func(_ context.Context, raw json.RawMessage) (any, error) {
				args := spreadArgs(raw)
				var query any
				if len(args) > 0 {
					query = args[0]
				}
				q, ok := query.(string)
				if !ok {
					return nil, errors.New("searchTools() expects a query string")
				}
				var opts map[string]any
				if len(args) > 1 {
					opts, _ = args[1].(map[string]any)
				}
				var limit any = float64(toolsearch.DefaultLimit)
				if v, present := opts["limit"]; present && v != nil {
					limit = v
				}
				if !isPositiveInteger(limit) {
					return nil, errors.New("searchTools() limit must be a positive integer")
				}
				var namespace string
				if v, present := opts["namespace"]; present && v != nil {
					s, isString := v.(string)
					if !isString {
						return nil, errors.New("searchTools() namespace must be a string")
					}
					namespace = s
				}
				var documents []toolsearch.Document
				for _, tool := range tools {
					ns := namespaceOf(tool.Name)
					if namespace != "" && (ns == nil || !isNamespaceName(ns, namespace)) {
						continue
					}
					documents = append(documents, toolsearch.NewDocument(tool, ns))
				}
				matches := toolsearch.Ranker{NamesFirst: true}.Rank(q, documents, int(math.Min(limit.(float64), math.MaxInt32)))
				out := make([]searchEntry, len(matches))
				for i, m := range matches {
					out[i] = entry(m.Name)
				}
				return out, nil
			},
		},
		{
			Name:   "describeTool",
			Spread: true,
			Execute: func(_ context.Context, raw json.RawMessage) (any, error) {
				args := spreadArgs(raw)
				var name any
				if len(args) > 0 {
					name = args[0]
				}
				n, ok := name.(string)
				if !ok {
					return nil, errors.New("describeTool() expects a tool name")
				}
				for _, tool := range tools {
					if tool.Name == n || codemode.Identifier(tool.Name) == n {
						return samples[tool.Name], nil
					}
				}
				return nil, nil
			},
		},
		{
			Name:   "describeNamespace",
			Spread: true,
			Execute: func(_ context.Context, raw json.RawMessage) (any, error) {
				args := spreadArgs(raw)
				var name any
				if len(args) > 0 {
					name = args[0]
				}
				n, ok := name.(string)
				if !ok {
					return nil, errors.New("describeNamespace() expects a namespace name")
				}
				var found *coding.ToolNamespace
				names := []string{}
				for _, tool := range tools {
					ns := namespaceOf(tool.Name)
					if ns == nil || !isNamespaceName(ns, n) {
						continue
					}
					if found == nil {
						found = ns
					}
					names = append(names, codemode.Identifier(tool.Name))
				}
				if found == nil {
					return nil, nil
				}
				return namespaceInfo{Name: found.Name, Description: found.Description, Instructions: found.Instructions, Tools: names}, nil
			},
		},
	}
}

// execute runs one script (pi executeCodemode). Without a session scripts
// cannot call tools.
func execute(ctx context.Context, toolCallID string, params map[string]any, onUpdate agent.ToolUpdateFunc, session Session, store *Store, noticeNames []string) (agent.AgentToolResult, error) {
	startedAt := time.Now()
	source, _ := params["code"].(string)
	parsed, err := codemode.ParseSource(source)
	if err != nil {
		return agent.AgentToolResult{}, err
	}

	var mu sync.Mutex
	var calls []*NestedCall
	var notices []notice
	snapshot := func() Details {
		mu.Lock()
		defer mu.Unlock()
		out := Details{Calls: make([]NestedCall, len(calls))}
		for i, c := range calls {
			out.Calls[i] = *c
		}
		return out
	}
	publish := func() {
		if onUpdate != nil {
			onUpdate(agent.AgentToolResult{Content: ai.ContentList{}, Details: snapshot()})
		}
	}
	// Show the script as running before its first nested call.
	publish()

	var callable []agent.AgentTool
	guidelines := map[string][]string{}
	namespaces := map[string]*coding.ToolNamespace{}
	if session != nil {
		callable = callableTools(session.CallableTools())
		for _, d := range session.AllTools() {
			guidelines[d.Tool.Name] = d.Tool.PromptGuidelines
			namespaces[d.Tool.Name] = d.Namespace
		}
	}
	samples := map[string]string{}
	for _, tool := range callable {
		samples[tool.Name] = codemode.RenderToolSample(Declaration(tool, guidelines[tool.Name]), 0)
	}
	sandboxTools := make([]codemode.Tool, 0, len(callable))
	for _, tool := range callable {
		sandboxTools = append(sandboxTools, codemode.Tool{
			Name:        tool.Name,
			Description: samples[tool.Name],
			Execute: func(callCtx context.Context, raw json.RawMessage) (any, error) {
				record := &NestedCall{ID: toolCallID + "/?", Name: tool.Name, Args: previewArgs(raw), Status: CallRunning}
				mu.Lock()
				calls = append(calls, record)
				mu.Unlock()
				publish()
				callStartedAt := time.Now()
				finish := func(status CallStatus, errText string) {
					mu.Lock()
					duration := float64(time.Since(callStartedAt).Nanoseconds()) / 1e6
					record.DurationMs = &duration
					record.Status = status
					if errText != "" {
						record.Error = utf16Truncate(errText, errorPreviewChars)
					}
					mu.Unlock()
					publish()
				}
				args, err := scriptArguments(tool, raw)
				if err != nil {
					finish(CallError, err.Error())
					return nil, err
				}
				outcome := session.ExecuteTool(callCtx, toolCallID, tool.Name, args, nil)
				found := noticesOf(noticeNames, tool.Name, outcome.Result)
				mu.Lock()
				record.ID = outcome.ToolCall.ID
				notices = append(notices, found...)
				mu.Unlock()
				if outcome.IsError {
					status := CallError
					if callCtx.Err() != nil {
						status = CallCancelled
					}
					text := textOf(outcome.Result)
					if text == "" {
						text = fmt.Sprintf("Tool %q failed", tool.Name)
					}
					finish(status, text)
				} else {
					finish(CallOK, "")
				}
				return toScriptValue(tool, outcome)
			},
		})
	}

	timeout := time.Duration(-1)
	if parsed.Options.TimeoutMs != nil {
		timeout = time.Duration(*parsed.Options.TimeoutMs) * time.Millisecond
		if timeout == 0 {
			// 0 would mean the sandbox default; pi's timeoutMs 0 expires at once.
			timeout = time.Nanosecond
		}
	}
	sandbox, err := codemode.New(codemode.Options{
		Tools:            sandboxTools,
		Globals:          discoveryGlobals(callable, samples, func(name string) *coding.ToolNamespace { return namespaces[name] }),
		Timeout:          timeout,
		MemoryLimitBytes: memoryLimitBytes,
	})
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	// A recorded session keeps the values on its branch (pi reads them from
	// ctx.sessionManager.getBranch()); any other falls back to store.
	entryLog, _ := session.(EntryLog)
	var values map[string]any
	logged := false
	if entryLog != nil {
		var entries []json.RawMessage
		if entries, logged = entryLog.CustomEntries(StoreEntryType); logged {
			values = readStore(entries)
		}
	}
	if !logged {
		values = store.snapshot()
	}
	result, err := func() (codemode.Result, error) {
		defer sandbox.Close()
		return sandbox.Execute(ctx, parsed.Code, codemode.ExecuteOptions{Store: values})
	}()
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	// Calls still marked running were cut off by the script ending, a
	// timeout, or an abort.
	mu.Lock()
	for _, c := range calls {
		if c.Status == CallRunning {
			c.Status = CallCancelled
		}
	}
	mu.Unlock()
	details := snapshot()

	output := append([]codemode.OutputItem(nil), result.Output...)
	if result.OK {
		// Only a successful script's writes are kept, and only when there are
		// any: no entry for a script that changed nothing.
		if writes := result.StoreWrites; logged {
			if len(writes.Set) > 0 || len(writes.Delete) > 0 {
				data := storeEntryData{Set: writes.Set, Delete: writes.Delete}
				if data.Set == nil {
					data.Set = map[string]json.RawMessage{}
				}
				if data.Delete == nil {
					data.Delete = []string{}
				}
				entryLog.AppendCustomEntry(StoreEntryType, data)
			}
		} else {
			store.apply(writes)
		}
		// pi extension: a returned value is appended like text().
		if result.Value != nil {
			output = append(output, codemode.OutputItem{Type: "text", Text: valueText(result.Value)})
		}
	}
	items := formatOutput(output)
	if !result.OK {
		items = append(items, ai.TextContent{Text: "Script error:\n" + formatError(result.Error, details.Calls)})
	}
	maxTokens := int64(defaultMaxOutputTokens)
	if parsed.Options.MaxOutputTokens != nil {
		maxTokens = *parsed.Options.MaxOutputTokens
	}
	truncated, fullPath := truncateOutput(joinAdjacentText(items), maxTokens)
	// After truncation, so no notice is cut, and checked against what the
	// truncated output still shows.
	mu.Lock()
	found := slices.Clone(notices)
	mu.Unlock()
	if block := formatNotices(found, truncated); block != "" {
		truncated = append(truncated, ai.TextContent{Text: block})
	}
	// After truncation, which joins the text items and moves images after
	// them, so each path stays next to its image and is never cut.
	saved, err := saveImages(truncated)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	content := joinAdjacentText(saved)
	status := "Script completed"
	if !result.OK {
		status = "Script failed"
	}
	header := fmt.Sprintf("%s\nWall time %.1f seconds\nOutput:\n", status, time.Since(startedAt).Seconds())
	details.FullOutputPath = fullPath
	return agent.AgentToolResult{
		Content: append(ai.ContentList{ai.TextContent{Text: header}}, content...),
		Details: details,
		IsError: !result.OK,
	}, nil
}

// discoveryGlobalsPattern are the script globals that list or look up tools
// and namespaces, which the script may then call by any name.
var discoveryGlobalsPattern = regexp.MustCompile(`\b(searchTools|describeNamespace|describeTool|ALL_TOOLS)\b`)

var scriptWords = regexp.MustCompile(`\w+`)

// ScriptReach is the tools a script may call (pi getScriptReach, the codemode
// tool's reachesTools): every tool when it uses a discovery global, else the
// words of its source, so tools.mcp__docs__search and
// tools["mcp__docs__" + name] both name the mcp__docs namespace.
func ScriptReach(code string) (all bool, words []string) {
	if discoveryGlobalsPattern.MatchString(code) {
		return true, nil
	}
	return false, scriptWords.FindAllString(code, -1)
}

// Options configure the codemode tool.
type Options struct {
	// Session is where nested calls run; nil means scripts cannot call tools.
	Session Session
	// Store keeps store() values across calls when Session is not a recorded
	// session (EntryLog); nil keeps none. A recorded session keeps them as
	// codemode-store entries on its branch, and Store is not used.
	Store *Store
	// Loadout configures how the tool presents the loadout.
	Loadout LoadoutOptions
	// Notices names top-level fields of nested call results (a warning, a
	// staleness or truncation mark) whose values reach the model even when
	// the script does not print them or its output is truncated: they are
	// appended to the output. Empty — none; see notices.go.
	Notices []string
}

// Definition is the codemode tool (pi createCodemodeToolDefinition plus
// createCodemodeExtension): model-only — scripts must not start other
// scripts — and registered inactive: activate it by name.
func Definition(opts Options) coding.ToolDefinition {
	inactive := false
	notices := noticeFields(opts.Notices)
	tool := agent.AgentTool{
		Name:  ToolName,
		Label: ToolName,
		// Replaced with the declarations of the callable tools when the tool
		// is activated.
		Description:      Description(nil, DescriptionOptions{}),
		Parameters:       ai.Object(ai.Prop("code", ai.String("Raw JavaScript source."))),
		PromptGuidelines: append([]string(nil), Guidelines...),
		// Capable models write the script as raw text instead of a
		// JSON-escaped string.
		ConstrainedSampling: &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingGrammar, Variants: ai.GrammarVariants{OpenAILark: codemode.SourceGrammar}},
		Execute: func(ctx context.Context, toolCallID string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			return execute(ctx, toolCallID, params, onUpdate, opts.Session, opts.Store, notices)
		},
	}
	return coding.ToolDefinition{
		Tool:          tool,
		Exposure:      coding.ToolExposureModelOnly,
		DefaultActive: &inactive,
		PromptSnippet: Snippet,
		Source:        "builtin",
		PrepareLoadout: func(l coding.ToolLoadout) (coding.ToolLoadoutChanges, error) {
			return PrepareLoadout(l, opts.Loadout), nil
		},
		ReachesTools: func(args map[string]any) (bool, []string) {
			code, _ := args["code"].(string)
			return ScriptReach(code)
		},
	}
}
