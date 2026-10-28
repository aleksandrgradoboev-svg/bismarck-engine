// Package codemodetool is pi's codemode tool (coding-agent
// src/extensions/codemode at v1.2.0): the model writes JavaScript that calls
// other tools, run in the codemode sandbox. This file is the model-facing
// side — the tool's description, the catalog of callable tools it lists, and
// the loadout hook that presents the loadout while the tool is active.
//
// Not ported: the models.* globals (no model registry with classifiers or
// image generation in the engine) and the TUI renderer.
package codemodetool

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// ToolName is the tool's name (pi CODEMODE_TOOL_NAME).
const ToolName = "codemode"

// Mode is how the tool presents tools that are both declared and callable from
// scripts (pi CodemodeMode, the codemode.mode setting).
type Mode string

const (
	// ModeOn: their descriptions say how scripts call them, and the codemode
	// description lists only the callable tools without direct exposure. The
	// default.
	ModeOn Mode = "on"
	// ModeOnly: the codemode description lists every callable tool, and
	// requests leave out the declarations of active direct tools.
	ModeOnly Mode = "only"
)

// DefaultInlineBudget is the default token budget for tool sections in the
// description (pi's codemode.inlineBudget default).
const DefaultInlineBudget = 3000

// charsPerToken estimates the cost of a tool section.
const charsPerToken = 4

// textOutputSchema is what a tool without an output schema resolves to.
var textOutputSchema = json.RawMessage(`{"type":"string"}`)

// Snippet and Guidelines are the tool's system prompt contribution (pi
// codemodeToolSystemPromptContribution).
const Snippet = "Run JavaScript that calls other tools"

// Guidelines are the tool's prompt guidelines.
var Guidelines = []string{
	"Use codemode to batch independent tool calls (Promise.allSettled), chain them, or filter large output, instead of many separate calls.",
}

const descriptionIntro = "Run JavaScript that calls other tools. The input is raw JavaScript (not JSON, no code fence), run as an async function body in a QuickJS sandbox: top-level `await` and `return` work. No Node, file system, network, or timers.\n" +
	"- `await tools.<name>({ ...args })` resolves to a string, or an object if the tool's declaration says so, and rejects with an Error on failure. A tool whose declaration returns an object resolves on failure too when it returned that object; check its fields. Calls still running when the script ends are cancelled.\n" +
	"- Optional first line: `// @options: {\"max_output_tokens\": 10000, \"timeout_ms\": 60000}`"

// describeGlobals is pi's describeGlobals without the models line: the engine
// has no models.* globals.
func describeGlobals() string {
	return strings.Join([]string{
		"Globals:",
		"- `text(value)`, `image(dataUrlOrImageBlock)`, `console.log(...)`, and top-level `return` add output; `exit()` ends the script. With several text items, each starts with a `==> text N/M <==` line, and `console` lines follow the other output in one `<console_output>` block. `image()` also saves the image to a temp file and the result names its path.",
		"- `store(key, value)` and `load(key)` keep JSON values across codemode calls.",
		"- `ALL_TOOLS`, `await searchTools(query, { limit?, namespace? })`, `await describeTool(name)`, `await describeNamespace(name)`: find unlisted tools, such as MCP tools.",
	}, "\n")
}

func schemaJSON(schema *ai.Schema) json.RawMessage {
	if schema == nil {
		return nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	return raw
}

// Declaration is what a script sees of a tool: its description followed by its
// prompt guidelines, which the system prompt only has for declared tools.
// Tools without an output schema resolve to their text output (pi
// toCodemodeDeclaration).
func Declaration(tool agent.AgentTool, guidelines []string) codemode.Tool {
	var bullets []string
	for _, g := range guidelines {
		if g = jstext.Trim(g); g != "" {
			bullets = append(bullets, "- "+g)
		}
	}
	description := tool.Description
	if len(bullets) > 0 {
		description = jstext.Trim(tool.Description) + "\n\n" + strings.Join(bullets, "\n")
	}
	output := schemaJSON(tool.OutputSchema)
	if output == nil {
		output = textOutputSchema
	}
	return codemode.Tool{Name: tool.Name, Description: description, InputSchema: schemaJSON(tool.Parameters), OutputSchema: output}
}

// callableTools are the tools a script may call: every given tool except the
// codemode tool itself (pi getCodemodeCallableTools).
func callableTools(tools []agent.AgentTool) []agent.AgentTool {
	return slices.DeleteFunc(slices.Clone(tools), func(t agent.AgentTool) bool { return t.Name == ToolName })
}

// DescriptionOptions configure Description (pi CodemodeDescriptionOptions,
// without models).
type DescriptionOptions struct {
	// Namespaces are each tool's namespace, by tool name. Tools of one
	// namespace are listed under one heading.
	Namespaces map[string]*coding.ToolNamespace
	// Deferred are tools that are callable but never listed with their
	// declaration (deferred exposure).
	Deferred map[string]bool
	// Guidelines are each tool's prompt guidelines, by tool name, listed after
	// its description.
	Guidelines map[string][]string
	// InlineBudget is the estimated tokens (characters / 4) the tool sections
	// may use; tools that do not fit are left out, like deferred tools. Nil
	// lists every tool that is not deferred.
	InlineBudget *int
}

// renderToolSection is "### `id` (`raw name`)" followed by the tool's
// description and declaration.
func renderToolSection(declaration codemode.Tool) string {
	id := codemode.Identifier(declaration.Name)
	heading := "### `" + id + "`"
	if id != declaration.Name {
		heading = "### `" + id + "` (`" + declaration.Name + "`)"
	}
	return heading + "\n" + jstext.Trim(codemode.RenderToolSample(declaration, 0))
}

type catalogEntry struct {
	name    string
	section string
	cost    int
}

type catalogGroup struct {
	namespace *coding.ToolNamespace
	entries   []catalogEntry
}

// selectCatalog picks the tool sections that fit the budget, like OpenCode's
// catalog: in each round every group (tools without a namespace first, then
// namespaces by name) places its cheapest remaining tool; a group whose next
// tool does not fit drops out while the others continue. Every namespace is
// represented before any namespace is complete.
func selectCatalog(groups []*catalogGroup, budget *int) map[string]bool {
	shown := map[string]bool{}
	if budget == nil {
		for _, g := range groups {
			for _, e := range g.entries {
				shown[e.name] = true
			}
		}
		return shown
	}
	var queues [][]catalogEntry
	for _, g := range groups {
		q := slices.Clone(g.entries)
		slices.SortStableFunc(q, func(a, b catalogEntry) int { return a.cost - b.cost })
		if len(q) > 0 {
			queues = append(queues, q)
		}
	}
	remaining := *budget
	for len(queues) > 0 {
		var next [][]catalogEntry
		for _, q := range queues {
			e := q[0]
			if e.cost > remaining {
				continue
			}
			remaining -= e.cost
			shown[e.name] = true
			if q = q[1:]; len(q) > 0 {
				next = append(next, q)
			}
		}
		queues = next
	}
	return shown
}

// localeCollator approximates JavaScript's localeCompare (ICU root
// collation), as the ls tool does.
var localeCollator = collate.New(language.Und)

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// Description is the model-facing description: the helper list, guidance for
// finding tools that are not listed, and one section per listed tool, grouped
// by namespace (pi createCodemodeDescription). Deferred tools are never listed
// and do not affect the description at all, so it stays the same while tools
// are registered late, for example by MCP servers that connect. Tool sections
// are limited to InlineBudget.
func Description(tools []agent.AgentTool, opts DescriptionOptions) string {
	var declarations []codemode.Tool
	for _, t := range callableTools(tools) {
		if !opts.Deferred[t.Name] {
			declarations = append(declarations, Declaration(t, opts.Guidelines[t.Name]))
		}
	}
	keys := []string{""}
	groups := map[string]*catalogGroup{"": {}}
	for _, d := range declarations {
		namespace := opts.Namespaces[d.Name]
		key := ""
		if namespace != nil {
			key = "ns:" + namespace.Name
		}
		g, ok := groups[key]
		if !ok {
			g = &catalogGroup{namespace: namespace}
			groups[key] = g
			keys = append(keys, key)
		}
		section := renderToolSection(d)
		g.entries = append(g.entries, catalogEntry{name: d.Name, section: section, cost: int(math.Ceil(float64(utf16Len(section)) / charsPerToken))})
	}
	ordered := make([]*catalogGroup, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, groups[k])
	}
	slices.SortStableFunc(ordered, func(a, b *catalogGroup) int {
		switch {
		case a.namespace == nil && b.namespace == nil:
			return 0
		case a.namespace == nil:
			return -1
		case b.namespace == nil:
			return 1
		}
		return localeCollator.CompareString(a.namespace.Name, b.namespace.Name)
	})
	shown := selectCatalog(ordered, opts.InlineBudget)

	sections := []string{descriptionIntro, describeGlobals()}
	if len(declarations) == 0 {
		return strings.Join(sections, "\n\n")
	}
	toolSections := []string{"Nested tools:"}
	for _, g := range ordered {
		var visible []catalogEntry
		for _, e := range g.entries {
			if shown[e.name] {
				visible = append(visible, e)
			}
		}
		if g.namespace != nil {
			// Only tools that did not fit the budget are counted as not
			// listed here.
			listing := ""
			switch {
			case len(visible) == len(g.entries):
			case len(visible) == 0:
				listing = " (tools not listed)"
			default:
				listing = " (some tools not listed)"
			}
			heading := "## " + g.namespace.Name + listing
			if description := jstext.Trim(g.namespace.Description); description != "" {
				heading += "\n" + description
			}
			toolSections = append(toolSections, heading)
		}
		for _, e := range visible {
			toolSections = append(toolSections, e.section)
		}
	}
	sections = append(sections, strings.Join(toolSections, "\n\n"))
	return strings.Join(sections, "\n\n")
}

// describeFields is the field names of an object schema, `{ output,
// exit_code, full_output_path? }`, with the fields of object-typed fields depth
// levels deep, in the schema's property order, and the value of const fields:
// `{ action: "start", id }` (pi f6ee9ce9). ok is false for other schemas.
func describeFields(schema *ai.Schema, depth int) (string, bool) {
	if schema == nil || schema.Type != "object" || schema.Nullable || schema.Properties == nil {
		return "", false
	}
	fields := []string{}
	for _, name := range schema.OrderedProperties() {
		field := name
		if !slices.Contains(schema.Required, name) {
			field += "?"
		}
		property := schema.Properties[name]
		if property != nil && property.HasConst {
			if value, err := jstext.Stringify(property.Const); err == nil {
				field += ": " + value
			}
		} else if depth > 0 {
			if nested, ok := describeFields(property, depth-1); ok {
				field += ": " + nested
			}
		}
		fields = append(fields, field)
	}
	return "{ " + strings.Join(fields, ", ") + " }", true
}

// describeUnionFields is the field names of each variant of an anyOf (or,
// without one, oneOf) whose variants are all objects, joined by " | ".
func describeUnionFields(schema *ai.Schema) (string, bool) {
	if schema == nil {
		return "", false
	}
	variants := schema.AnyOf
	if variants == nil {
		variants = schema.OneOf
	}
	if len(variants) == 0 {
		return "", false
	}
	described := make([]string, len(variants))
	for i, variant := range variants {
		fields, ok := describeFields(variant, 0)
		if !ok {
			return "", false
		}
		described[i] = fields
	}
	return strings.Join(described, " | "), true
}

// describeOutput is what a script call resolves to, in one line: "a string",
// the field names of an object, or of each object in a union, the rendered
// type when it fits on one line, or a pointer to describeTool() for anything
// longer, whose full type would otherwise go with every request (pi
// f6ee9ce9, #11501).
func describeOutput(tool agent.AgentTool) string {
	declaration := Declaration(tool, nil)
	t := codemode.RenderToolOutputType(declaration.OutputSchema)
	if t == "string" {
		return "a string"
	}
	fields, ok := describeFields(tool.OutputSchema, 1)
	if !ok {
		fields, ok = describeUnionFields(tool.OutputSchema)
	}
	if ok {
		return "`" + fields + "`"
	}
	if !strings.Contains(t, "\n") {
		return "`" + t + "`"
	}
	name, _ := jstext.Stringify(tool.Name)
	return "a value; `await describeTool(" + name + ")` shows its type"
}

// describeScriptCall is a declared tool's description followed by how scripts
// call it and what the call resolves to.
func describeScriptCall(tool agent.AgentTool) string {
	return jstext.Trim(tool.Description) + "\n\nCodemode: `tools." + codemode.Identifier(tool.Name) + "(args)` resolves to " + describeOutput(tool) + "."
}

// LoadoutOptions configure PrepareLoadout.
type LoadoutOptions struct {
	// Mode returns the presentation mode; nil or "" means ModeOn.
	Mode func() Mode
	// InlineBudget returns the token budget for tool sections; nil, or a nil
	// result, means DefaultInlineBudget.
	InlineBudget func() *int
}

// PrepareLoadout presents the loadout while the codemode tool is active (pi
// prepareCodemodeLoadout). Listed tools carry their prompt guidelines, which
// the system prompt only has for declared tools. Listing by exposure, not by
// the active set, keeps the codemode description unchanged when tool_search
// loads a tool, so loads do not redeclare codemode.
func PrepareLoadout(loadout coding.ToolLoadout, opts LoadoutOptions) coding.ToolLoadoutChanges {
	mode := ModeOn
	if opts.Mode != nil {
		if m := opts.Mode(); m != "" {
			mode = m
		}
	}
	isDirect := func(t agent.AgentTool) bool { return loadout.Exposure(t.Name) == coding.ToolExposureDirect }
	callable := callableTools(loadout.Callable)
	callableNames := map[string]bool{}
	for _, t := range callable {
		callableNames[t.Name] = true
	}
	descriptions := map[string]string{}
	if mode == ModeOn {
		for _, t := range loadout.Declared {
			if callableNames[t.Name] {
				descriptions[t.Name] = describeScriptCall(t)
			}
		}
	}
	listed := callable
	if mode != ModeOnly {
		listed = slices.DeleteFunc(slices.Clone(callable), isDirect)
	}
	namespaces := map[string]*coding.ToolNamespace{}
	guidelines := map[string][]string{}
	deferred := map[string]bool{}
	for _, t := range listed {
		if ns := loadout.Namespace(t.Name); ns != nil {
			namespaces[t.Name] = ns
		}
		guidelines[t.Name] = loadout.PromptGuidelines(t.Name)
		if loadout.Exposure(t.Name) == coding.ToolExposureDeferred {
			deferred[t.Name] = true
		}
	}
	budget := DefaultInlineBudget
	if opts.InlineBudget != nil {
		if b := opts.InlineBudget(); b != nil {
			budget = *b
		}
	}
	descriptions[ToolName] = Description(listed, DescriptionOptions{
		Namespaces: namespaces, Guidelines: guidelines, Deferred: deferred, InlineBudget: &budget,
	})
	var hidden []string
	if mode == ModeOnly {
		for _, t := range callable {
			if isDirect(t) && slices.ContainsFunc(loadout.Declared, func(d agent.AgentTool) bool { return d.Name == t.Name }) {
				hidden = append(hidden, t.Name)
			}
		}
	}
	return coding.ToolLoadoutChanges{Descriptions: descriptions, HiddenDeclarations: hidden}
}
