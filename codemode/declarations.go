package codemode

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// TypeScript declarations for the script-visible API: pi's declarations.ts.
// Text follows JavaScript's semantics throughout (trim, String(number),
// JSON.stringify, UTF-16 lengths and sort order), via internal/jstext, so the
// model sees what pi's model sees.

// DefaultInputSchemaMaxChars is the largest rendered input type, in UTF-16
// code units, before it becomes `unknown`.
const DefaultInputSchemaMaxChars = 16_000

const (
	indent = "  "
	// maxRefExpansions: local $ref expansions per rendered schema, so shared
	// definitions cannot blow up the output.
	maxRefExpansions = 32
	// maxTagChars: longest property tag; longer ones are left out.
	maxTagChars = 80
)

// tagKeywords are JSON Schema keywords with a number or string value shown as
// `@keyword value` property tags.
var tagKeywords = []string{
	"minimum", "exclusiveMinimum", "maximum", "exclusiveMaximum", "multipleOf",
	"minLength", "maxLength", "pattern", "format", "minItems", "maxItems",
}

// Tag keywords whose values all hold at once, so only the largest (lower) or
// smallest (upper) matters.
var (
	lowerBounds = map[string]bool{"minimum": true, "exclusiveMinimum": true, "minLength": true, "minItems": true}
	upperBounds = map[string]bool{"maximum": true, "exclusiveMaximum": true, "maxLength": true, "maxItems": true}
)

// lineTerminators end a `//` comment.
var lineTerminators = regexp.MustCompile(`[\r\n\x{2028}\x{2029}]`)

var crlfSplit = regexp.MustCompile(`\r?\n`)

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// parseSchema decodes a schema; nil (no schema) and invalid JSON give
// ok false.
func parseSchema(raw json.RawMessage) (any, bool) {
	if raw == nil {
		return nil, false
	}
	v, err := jstext.Parse(raw)
	if err != nil {
		return nil, false
	}
	return v, true
}

// RenderDeclarations renders TypeScript declarations for the script-visible
// API. Tools become members of `declare const tools`, globals `declare
// function` statements, and `ns.member` globals members of `declare const ns`.
// Descriptions become doc comments; schemas become types.
func RenderDeclarations(tools, globals []Tool) string {
	var sections []string
	if len(tools) > 0 {
		members := make([]string, len(tools))
		for i, tool := range tools {
			members[i] = docComment(tool.Description, indent) + indent + RenderToolSignature(tool, 0)
		}
		sections = append(sections, "declare const tools: {\n"+strings.Join(members, "\n")+"\n};")
	}
	var order []string
	namespaces := map[string][]string{}
	for _, g := range globals {
		dot := strings.IndexByte(g.Name, '.')
		if dot == -1 {
			sections = append(sections, renderGlobal("declare function "+g.Name, g, ""))
			continue
		}
		ns := g.Name[:dot]
		if _, seen := namespaces[ns]; !seen {
			order = append(order, ns)
		}
		namespaces[ns] = append(namespaces[ns], renderGlobal(g.Name[dot+1:], g, indent))
	}
	for _, ns := range order {
		sections = append(sections, "declare const "+ns+": {\n"+strings.Join(namespaces[ns], "\n")+"\n};")
	}
	return strings.Join(sections, "\n\n")
}

// RenderToolSignature renders one tool as a member of the `tools` object:
// `name(args: T): Promise<R>;` with the identifier scripts use. Input types
// longer than inputMaxChars (0: DefaultInputSchemaMaxChars) render as unknown.
func RenderToolSignature(tool Tool, inputMaxChars int) string {
	if inputMaxChars == 0 {
		inputMaxChars = DefaultInputSchemaMaxChars
	}
	input := "unknown"
	if schema, ok := parseSchema(tool.InputSchema); ok {
		input = SchemaToType(schema, inputMaxChars)
	}
	return Identifier(tool.Name) + "(args: " + input + "): Promise<" + RenderToolOutputType(tool.OutputSchema) + ">;"
}

// RenderToolSample renders a tool's description followed by its declaration,
// for tool listings.
func RenderToolSample(tool Tool, inputMaxChars int) string {
	declaration := "declare const tools: { " + RenderToolSignature(tool, inputMaxChars) + " };"
	return jstext.Trim(tool.Description) + "\n\ncodemode tool declaration:\n```ts\n" + declaration + "\n```"
}

// RenderToolOutputType is the type a tool call resolves to: the output
// schema's type, and unknown without a schema.
func RenderToolOutputType(schema json.RawMessage) string {
	if s, ok := parseSchema(schema); ok {
		return SchemaToType(s, 0)
	}
	return "unknown"
}

func renderGlobal(head string, g Tool, ind string) string {
	if g.Signature != "" {
		return docComment(g.Description, ind) + ind + head + g.Signature + ";"
	}
	input, output := "unknown", "unknown"
	if s, ok := parseSchema(g.InputSchema); ok {
		input = SchemaToType(s, 0)
	}
	if s, ok := parseSchema(g.OutputSchema); ok {
		output = SchemaToType(s, 0)
	}
	return docComment(g.Description, ind) + ind + head + "(args: " + input + "): Promise<" + output + ">;"
}

func docComment(description, ind string) string {
	text := jstext.Trim(description)
	if text == "" {
		return ""
	}
	lines := crlfSplit.Split(strings.ReplaceAll(text, "*/", `*\/`), -1)
	if len(lines) == 1 {
		return ind + "/** " + lines[0] + " */\n"
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		if line != "" {
			line = " " + line
		}
		out[i] = ind + " *" + line
	}
	return ind + "/**\n" + strings.Join(out, "\n") + "\n" + ind + " */\n"
}

func propertyKey(name string) string {
	if identifierRE.MatchString(name) {
		return name
	}
	return stringify(name)
}

// stringify is JSON.stringify for a value jstext.Parse returned.
func stringify(v any) string {
	s, err := jstext.Stringify(v)
	if err != nil {
		return "unknown"
	}
	return s
}

func union(types []string) string {
	var unique []string
	for _, t := range types {
		if !slices.Contains(unique, t) {
			unique = append(unique, t)
		}
	}
	if slices.Contains(unique, "unknown") {
		return "unknown"
	}
	if len(unique) == 0 {
		return "never"
	}
	return strings.Join(unique, " | ")
}

type schemaContext struct {
	root       any
	resolving  map[string]bool
	expansions int
	tags       bool
}

// SchemaToType converts a JSON Schema (as jstext.Parse returns it) to a
// TypeScript type expression: objects on one line with properties sorted by
// name, or one property per line with `//` comments when a property has a
// description or constraints the type cannot express; Array<T> for arrays.
// Local references resolve against the schema; recursive and remote ones
// render as unknown. A result longer than maxChars (0: no limit) is rendered
// again without the constraint tags, and as unknown if still too long.
func SchemaToType(schema any, maxChars int) string {
	render := func(tags bool) string {
		return toType(schema, &schemaContext{root: schema, resolving: map[string]bool{}, tags: tags})
	}
	t := render(true)
	if maxChars == 0 || utf16Len(t) <= maxChars {
		return t
	}
	if untagged := render(false); utf16Len(untagged) <= maxChars {
		return untagged
	}
	return "unknown"
}

func resolveRef(ref string, root any) (any, bool) {
	if ref != "#" && !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	current := root
	rest := strings.TrimPrefix(strings.TrimPrefix(ref, "#"), "/")
	for _, segment := range strings.Split(rest, "/") {
		if segment == "" {
			continue
		}
		key, err := url.PathUnescape(segment)
		if err != nil {
			return nil, false
		}
		key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = obj[key]; !ok {
			return nil, false
		}
	}
	switch current.(type) {
	case bool, map[string]any:
		return current, true
	}
	return nil, false
}

func toType(schema any, ctx *schemaContext) string {
	switch s := schema.(type) {
	case bool:
		if s {
			return "unknown"
		}
		return "never"
	case map[string]any:
		return objectSchemaType(s, ctx)
	}
	return "unknown"
}

func objectSchemaType(schema map[string]any, ctx *schemaContext) string {
	if ref, ok := schema["$ref"].(string); ok {
		if ctx.resolving[ref] || ctx.expansions >= maxRefExpansions {
			return "unknown"
		}
		target, ok := resolveRef(ref, ctx.root)
		if !ok {
			return "unknown"
		}
		ctx.expansions++
		ctx.resolving[ref] = true
		defer delete(ctx.resolving, ref)
		return toType(target, ctx)
	}

	if c, ok := schema["const"]; ok {
		return stringify(c)
	}
	if enum, ok := schema["enum"].([]any); ok {
		types := make([]string, len(enum))
		for i, v := range enum {
			types[i] = stringify(v)
		}
		return union(types)
	}

	variants, ok := schema["anyOf"].([]any)
	if !ok {
		variants, ok = schema["oneOf"].([]any)
	}
	if ok {
		types := make([]string, len(variants))
		for i, v := range variants {
			types[i] = toType(v, ctx)
		}
		return union(types)
	}
	if allOf, ok := schema["allOf"].([]any); ok {
		var parts []string
		for _, part := range allOf {
			if t := toType(part, ctx); t != "unknown" {
				if strings.Contains(t, " | ") {
					t = "(" + t + ")"
				}
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			return "unknown"
		}
		return strings.Join(parts, " & ")
	}

	typ, hasType := schema["type"]
	if types, ok := typ.([]any); ok {
		out := make([]string, len(types))
		for i, entry := range types {
			copied := make(map[string]any, len(schema))
			for k, v := range schema {
				copied[k] = v
			}
			copied["type"] = entry
			out[i] = toType(copied, ctx)
		}
		return union(out)
	}
	if !hasType {
		for _, key := range []string{"properties", "additionalProperties", "required"} {
			if _, ok := schema[key]; ok {
				return objectType(schema, ctx)
			}
		}
		for _, key := range []string{"items", "prefixItems"} {
			if _, ok := schema[key]; ok {
				return arrayType(schema, ctx)
			}
		}
		return "unknown"
	}
	switch typ {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		return arrayType(schema, ctx)
	case "object":
		return objectType(schema, ctx)
	}
	return "unknown"
}

func arrayType(schema map[string]any, ctx *schemaContext) string {
	items, hasItems := schema["items"]
	if _, isTuple := items.([]any); hasItems && !isTuple {
		return "Array<" + toType(items, ctx) + ">"
	}
	tuple, ok := schema["prefixItems"].([]any)
	if !ok {
		tuple, _ = items.([]any)
	}
	if len(tuple) > 0 {
		out := make([]string, len(tuple))
		for i, item := range tuple {
			out[i] = toType(item, ctx)
		}
		return "[" + strings.Join(out, ", ") + "]"
	}
	return "unknown[]"
}

func descriptionOf(property any) string {
	if obj, ok := property.(map[string]any); ok {
		if d, ok := obj["description"].(string); ok {
			return jstext.Trim(d)
		}
	}
	return ""
}

// constraintTags lists a property's constraints its TypeScript type cannot
// express, as JSDoc-style tags: `@integer @minimum 1 @maximum 100 @default
// 20`. Empty when there are none.
func constraintTags(schema any) string {
	property, ok := schema.(map[string]any)
	if !ok {
		return ""
	}
	// Optional fields such as Pydantic's `int | None` keep their constraints
	// in the non-null branch; it applies together with the property.
	variants, isArray := property["anyOf"].([]any)
	if !isArray {
		variants, isArray = property["oneOf"].([]any)
	}
	var nonNull []any
	if isArray {
		for _, v := range variants {
			if obj, ok := v.(map[string]any); ok && obj["type"] == "null" {
				continue
			}
			nonNull = append(nonNull, v)
		}
	}
	branches := []map[string]any{property}
	if len(nonNull) == 1 {
		if branch, ok := nonNull[0].(map[string]any); ok {
			branches = append(branches, branch)
		}
	}
	// Draft-04 and OpenAPI 3.0 make minimum/maximum exclusive with a boolean
	// exclusiveMinimum/exclusiveMaximum.
	schemas := make([]map[string]any, len(branches))
	for i, b := range branches {
		s := make(map[string]any, len(b))
		for k, v := range b {
			s[k] = v
		}
		for _, pair := range [][2]string{{"minimum", "exclusiveMinimum"}, {"maximum", "exclusiveMaximum"}} {
			if b[pair[1]] == true {
				delete(s, pair[0])
				if v, ok := b[pair[0]]; ok {
					s[pair[1]] = v
				} else {
					delete(s, pair[1])
				}
			}
		}
		schemas[i] = s
	}

	var tags []string
	isInteger := func(t any) bool {
		if t == "integer" {
			return true
		}
		list, ok := t.([]any)
		return ok && slices.Contains(list, any("integer"))
	}
	for _, s := range schemas {
		if isInteger(s["type"]) {
			tags = append(tags, "@integer")
			break
		}
	}
	for _, keyword := range tagKeywords {
		if lowerBounds[keyword] || upperBounds[keyword] {
			var numbers []float64
			for _, s := range schemas {
				if n, ok := s[keyword].(json.Number); ok {
					numbers = append(numbers, jstext.Number(n))
				}
			}
			if len(numbers) == 0 {
				continue
			}
			pick := slices.Max(numbers)
			if upperBounds[keyword] {
				pick = slices.Min(numbers)
			}
			tags = append(tags, "@"+keyword+" "+jstext.NumberToString(pick))
			continue
		}
		// new Set(values): the same string or number once.
		var seenNumbers []float64
		var seenStrings []string
		for _, s := range schemas {
			switch v := s[keyword].(type) {
			case json.Number:
				f := jstext.Number(v)
				if !slices.ContainsFunc(seenNumbers, func(g float64) bool { return g == f || (math.IsNaN(f) && math.IsNaN(g)) }) {
					seenNumbers = append(seenNumbers, f)
					tags = append(tags, "@"+keyword+" "+jstext.NumberToString(f))
				}
			case string:
				if !slices.Contains(seenStrings, v) {
					seenStrings = append(seenStrings, v)
					tags = append(tags, "@"+keyword+" "+v)
				}
			}
		}
	}
	for _, s := range schemas {
		if s["uniqueItems"] == true {
			tags = append(tags, "@uniqueItems")
			break
		}
	}
	// Annotations, unlike constraints, come from the property itself.
	if d, ok := property["default"]; ok && d != nil {
		tags = append(tags, "@default "+stringify(d))
	}
	if property["deprecated"] == true {
		tags = append(tags, "@deprecated")
	}
	var kept []string
	for _, tag := range tags {
		if utf16Len(tag) > maxTagChars {
			continue
		}
		kept = append(kept, lineTerminators.ReplaceAllStringFunc(tag, func(c string) string {
			return fmt.Sprintf(`\u%04x`, []rune(c)[0])
		}))
	}
	return strings.Join(kept, " ")
}

func objectType(schema map[string]any, ctx *schemaContext) string {
	properties, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	if list, ok := schema["required"].([]any); ok {
		for _, name := range list {
			if s, ok := name.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	slices.SortFunc(names, jstext.CompareUTF16)
	members := make([]string, 0, len(names)+1)
	for _, name := range names {
		optional := "?"
		if required[name] {
			optional = ""
		}
		members = append(members, propertyKey(name)+optional+": "+toType(properties[name], ctx)+";")
	}
	additional, hasAdditional := schema["additionalProperties"]
	switch {
	case hasAdditional && additional != false:
		t := "unknown"
		if additional != true {
			t = toType(additional, ctx)
		}
		members = append(members, "[key: string]: "+t+";")
	case !hasAdditional && len(names) == 0:
		members = append(members, "[key: string]: unknown;")
	}
	if len(members) == 0 {
		return "{}"
	}
	comments := make([][]string, len(names))
	anyComment := false
	for i, name := range names {
		var lines []string
		for _, line := range lineTerminators.Split(descriptionOf(properties[name]), -1) {
			if line = jstext.Trim(line); line != "" {
				lines = append(lines, line)
			}
		}
		if ctx.tags {
			if tags := constraintTags(properties[name]); tags != "" {
				lines = append(lines, tags)
			}
		}
		comments[i] = lines
		anyComment = anyComment || len(lines) > 0
	}
	if !anyComment {
		return "{ " + strings.Join(members, " ") + " }"
	}
	lines := []string{"{"}
	for i, comment := range comments {
		for _, line := range comment {
			lines = append(lines, indent+"// "+line)
		}
		lines = append(lines, indent+strings.ReplaceAll(members[i], "\n", "\n"+indent))
	}
	for _, member := range members[len(names):] {
		lines = append(lines, indent+member)
	}
	lines = append(lines, "}")
	return strings.Join(lines, "\n")
}
