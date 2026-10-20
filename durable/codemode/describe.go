package codemode

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	sandbox "github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// What the model reads about code mode (pi durable/src/codemode/describe.ts):
// the tool's fixed description, and the codemode prompt section, which lists
// the tools scripts can call as TypeScript declarations, grouped by namespace
// and limited to a token budget. Tools that do not fit are found with
// searchTools(). The section renders from the conversation's callable tools,
// so it changes with them; the tool's description never does.

// Namespace is a group of tools named by a common prefix, such as an MCP
// server's "mcp__github__".
type Namespace struct {
	Name         string
	Description  string
	Instructions string
	// Prefix: tool names starting with it belong to the namespace; the
	// longest matching prefix wins.
	Prefix string
}

// DefaultInlineBudget is the default token budget of the tool declarations in
// the codemode section, as in the coding agent.
const DefaultInlineBudget = 3000

// charsPerToken estimates the cost of a tool section.
const charsPerToken = 4

var textOutputSchema = json.RawMessage(`{"type":"string"}`)

const descriptionIntro = "Run JavaScript that calls other tools. The input is raw JavaScript (not JSON, no code fence), run as an async function body in a QuickJS sandbox: top-level `await` and `return` work. No Node, file system, network, or timers.\n" +
	"- `await tools.<name>({ ...args })` resolves to a string, or an object if the tool's declaration says so, and rejects with an Error on failure. A tool whose declaration returns an object resolves on failure too when it returned that object; check its fields. Calls still running when the script ends are cancelled.\n" +
	"- The script survives a restart: it resumes where it was, without running finished calls again.\n" +
	"- Optional first line: `// @options: {\"max_output_tokens\": 10000, \"timeout_ms\": 60000}`"

// description is the code mode tool's description: the script environment
// and its globals, the same for every conversation (pi
// codemodeDescription(false): the engine has no models.* globals).
func description() string {
	globals := strings.Join([]string{
		"Globals:",
		"- `text(value)`, `image(dataUrlOrImageBlock)`, `console.log(...)`, and top-level `return` add output; `exit()` ends the script. With several text items, each starts with a `==> text N/M <==` line, and `console` lines follow the other output in one `<console_output>` block. `image()` also saves the image to a temp file and the result names its path.",
		"- `store(key, value)` and `load(key)` keep JSON values across codemode calls.",
		"- `ALL_TOOLS`, `await searchTools(query, { limit?, namespace? })`, `await describeTool(name)`, `await describeNamespace(name)`: find tools the `codemode` section does not list.",
	}, "\n")
	return strings.Join([]string{
		descriptionIntro,
		globals,
		"The `codemode` section of the system prompt lists the tools scripts can call, by namespace, as far as space allows.",
	}, "\n\n")
}

func schemaJSON(schema any) json.RawMessage {
	raw, err := json.Marshal(schema)
	if err != nil || string(raw) == "null" {
		return nil
	}
	return raw
}

// declarationOf is what a script sees of a tool: its declaration. Tools
// without an output schema resolve to their text output.
func declarationOf(tool *harness.ToolRegistration) sandbox.Tool {
	declaration := sandbox.Tool{Name: tool.Name, Description: tool.Description, InputSchema: schemaJSON(tool.Parameters), OutputSchema: textOutputSchema}
	if tool.StructuredOutputSchema != nil {
		declaration.OutputSchema = schemaJSON(tool.StructuredOutputSchema)
	}
	return declaration
}

// sampleOf is a tool's description and TypeScript declaration.
func sampleOf(tool *harness.ToolRegistration) string {
	return sandbox.RenderToolSample(declarationOf(tool), 0)
}

// namespaceOf is the namespace of a tool: the one with the longest prefix its
// name starts with.
func namespaceOf(name string, namespaces []Namespace) *Namespace {
	var found *Namespace
	for i := range namespaces {
		namespace := &namespaces[i]
		if strings.HasPrefix(name, namespace.Prefix) && (found == nil || len(namespace.Prefix) > len(found.Prefix)) {
			found = namespace
		}
	}
	return found
}

// renderToolSection is "### `id` (`raw name`)" followed by the tool's
// description and declaration.
func renderToolSection(tool *harness.ToolRegistration) string {
	id := sandbox.Identifier(tool.Name)
	heading := "### `" + id + "`"
	if id != tool.Name {
		heading = "### `" + id + "` (`" + tool.Name + "`)"
	}
	return heading + "\n" + jstext.Trim(sampleOf(tool))
}

type catalogEntry struct {
	name    string
	section string
	cost    int
}

type catalogGroup struct {
	namespace *Namespace
	entries   []catalogEntry
}

// selectCatalog picks the tool sections that fit the budget: in each round
// every group (tools without a namespace first, then namespaces by name)
// places its cheapest remaining tool; a group whose next tool does not fit
// drops out while the others continue. Every namespace is represented before
// any namespace is complete.
func selectCatalog(groups []*catalogGroup, budget int) map[string]bool {
	var queues [][]catalogEntry
	for _, group := range groups {
		queue := slices.Clone(group.entries)
		slices.SortStableFunc(queue, func(a, b catalogEntry) int { return a.cost - b.cost })
		if len(queue) > 0 {
			queues = append(queues, queue)
		}
	}
	shown := map[string]bool{}
	remaining := budget
	for len(queues) > 0 {
		var next [][]catalogEntry
		for _, queue := range queues {
			entry := queue[0]
			if entry.cost > remaining {
				continue
			}
			remaining -= entry.cost
			shown[entry.name] = true
			if queue = queue[1:]; len(queue) > 0 {
				next = append(next, queue)
			}
		}
		queues = next
	}
	return shown
}

// localeCollator approximates JavaScript's localeCompare.
var localeCollator = collate.New(language.Und)

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// section is the codemode section: the tools scripts can call, grouped by
// namespace, with their declarations as far as the budget allows; ok false
// without callable tools.
func section(tools []*harness.ToolRegistration, namespaces []Namespace, budget int) (string, bool) {
	if len(tools) == 0 {
		return "", false
	}
	keys := []string{""}
	groups := map[string]*catalogGroup{"": {}}
	for _, tool := range tools {
		namespace := namespaceOf(tool.Name, namespaces)
		key := ""
		if namespace != nil {
			key = "ns:" + namespace.Name
		}
		group, ok := groups[key]
		if !ok {
			group = &catalogGroup{namespace: namespace}
			groups[key] = group
			keys = append(keys, key)
		}
		text := renderToolSection(tool)
		group.entries = append(group.entries, catalogEntry{name: tool.Name, section: text, cost: int(math.Ceil(float64(utf16Len(text)) / charsPerToken))})
	}
	ordered := make([]*catalogGroup, 0, len(keys))
	for _, key := range keys {
		ordered = append(ordered, groups[key])
	}
	slices.SortStableFunc(ordered, func(a, b *catalogGroup) int {
		switch {
		case a.namespace == nil:
			return -1
		case b.namespace == nil:
			return 1
		}
		return localeCollator.CompareString(a.namespace.Name, b.namespace.Name)
	})
	shown := selectCatalog(ordered, budget)

	sections := []string{"Tools scripts can call (`await tools.<name>(args)`):"}
	for _, group := range ordered {
		if len(group.entries) == 0 {
			continue
		}
		var visible []catalogEntry
		for _, entry := range group.entries {
			if shown[entry.name] {
				visible = append(visible, entry)
			}
		}
		if group.namespace != nil {
			listing := ""
			switch {
			case len(visible) == len(group.entries):
			case len(visible) == 0:
				listing = " (tools not listed)"
			default:
				listing = " (some tools not listed)"
			}
			heading := "## " + group.namespace.Name + listing
			if description := jstext.Trim(group.namespace.Description); description != "" {
				heading += "\n" + description
			}
			sections = append(sections, heading)
		} else if len(visible) < len(group.entries) {
			sections = append(sections, "Some tools are not listed: find them with `searchTools()`.")
		}
		for _, entry := range visible {
			sections = append(sections, entry.section)
		}
	}
	return strings.Join(sections, "\n\n"), true
}
