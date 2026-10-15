package codemode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// Ported from pi v1.2.0 packages/codemode/test/declarations.test.ts.

func sch(t *testing.T, text string) any {
	t.Helper()
	v, err := jstext.Parse([]byte(text))
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return v
}

func checkType(t *testing.T, schema string, maxChars int, want string) {
	t.Helper()
	if got := SchemaToType(sch(t, schema), maxChars); got != want {
		t.Errorf("schema %s\ngot:  %q\nwant: %q", schema, got, want)
	}
}

func lines(l ...string) string { return strings.Join(l, "\n") }

func TestSchemaPrimitivesLiteralsUnions(t *testing.T) {
	for schema, want := range map[string]string{
		`{"type":"string"}`:                               "string",
		`{"type":"integer"}`:                              "number",
		`{"type":["string","null"]}`:                      "string | null",
		`{"const":"a"}`:                                   `"a"`,
		`{"enum":["a",1,null]}`:                           `"a" | 1 | null`,
		`{"anyOf":[{"type":"string"},{"type":"number"}]}`: "string | number",
		`{"anyOf":[{"type":"string"},{}]}`:                "unknown",
		`{"allOf":[{"anyOf":[{"type":"string"},{"type":"number"}]},{"const":1}]}`: "(string | number) & 1",
		`{"$ref":"#/defs/x"}`: "unknown",
		`true`:                "unknown",
		`false`:               "never",
	} {
		checkType(t, schema, 0, want)
	}
}

func TestSchemaObjects(t *testing.T) {
	checkType(t, `{"type":"object","properties":{"city":{"type":"string"},"max-lines":{"type":"number"}},"required":["city"],"additionalProperties":false}`, 0,
		`{ city: string; "max-lines"?: number; }`)
	checkType(t, `{"type":"object","additionalProperties":{"type":"number"}}`, 0, "{ [key: string]: number; }")
	checkType(t, `{"type":"object"}`, 0, "{ [key: string]: unknown; }")
	checkType(t, `{"type":"object","properties":{},"additionalProperties":false}`, 0, "{}")
}

func TestSchemaDescriptions(t *testing.T) {
	checkType(t, `{"type":"object","properties":{"weather":{"type":"array","description":"look up weather for a given list of locations",
		"items":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}},"required":["weather"]}`, 0,
		"{\n  // look up weather for a given list of locations\n  weather: Array<{ location: string; }>;\n}")
	checkType(t, `{"type":"object","properties":{"outer":{"type":"object","description":"Outer","properties":{"inner":{"type":"string","description":"Inner"}}}}}`, 0,
		"{\n  // Outer\n  outer?: {\n    // Inner\n    inner?: string;\n  };\n}")
}

func TestSchemaConstraintTags(t *testing.T) {
	checkType(t, `{"type":"object","properties":{
		"timeoutMs":{"type":"integer","minimum":0,"maximum":30000,"default":0,"description":"Wait in milliseconds."},
		"id":{"type":"string","pattern":"^[a-z]+$","maxLength":8},
		"tags":{"type":"array","items":{"type":"string"},"maxItems":5,"uniqueItems":true,"deprecated":true},
		"name":{"type":"string"}},"required":["id"]}`, 0, lines(
		"{",
		"  // @maxLength 8 @pattern ^[a-z]+$",
		"  id: string;",
		"  name?: string;",
		"  // @maxItems 5 @uniqueItems @deprecated",
		"  tags?: Array<string>;",
		"  // Wait in milliseconds.",
		"  // @integer @minimum 0 @maximum 30000 @default 0",
		"  timeoutMs?: number;",
		"}"))
	// Pydantic's `limit: int | None = Field(None, ge=1, le=100)`.
	checkType(t, `{"type":"object","properties":{"limit":{"anyOf":[{"type":"integer","minimum":1,"maximum":100},{"type":"null"}],"default":null,"title":"Limit"}}}`, 0,
		"{\n  // @integer @minimum 1 @maximum 100\n  limit?: number | null;\n}")
	checkType(t, `{"type":"object","properties":{"n":{"anyOf":[{"type":"integer","minimum":10,"maximum":50,"format":"int32"},{"type":"null"}],"minimum":0,"maximum":20,"format":"port"}}}`, 0,
		"{\n  // @integer @minimum 10 @maximum 20 @format port @format int32\n  n?: number | null;\n}")
	checkType(t, `{"type":"object","properties":{
		"n":{"type":"integer","minimum":0,"exclusiveMinimum":true,"maximum":10,"exclusiveMaximum":false},
		"m":{"anyOf":[{"type":"number","maximum":5,"exclusiveMaximum":true},{"type":"null"}],"maximum":8}}}`, 0, lines(
		"{",
		"  // @maximum 8 @exclusiveMaximum 5",
		"  m?: number | null;",
		"  // @integer @exclusiveMinimum 0 @maximum 10",
		"  n?: number;",
		"}"))
}

func TestSchemaLongTagsAndLineTerminators(t *testing.T) {
	checkType(t, fmt.Sprintf(`{"type":"object","properties":{"config":{"type":"object","default":{"text":"%s"}}}}`, strings.Repeat("x", 100)), 0,
		"{ config?: { [key: string]: unknown; }; }")
	// JSON escapes in the schema; the expected text spells them out, since a
	// comment line cannot hold a line terminator.
	u := func(hex string) string { return `\` + "u" + hex }
	checkType(t, `{"type":"object","properties":{"sep":{"type":"string","pattern":"^[`+u("000d")+u("000a")+u("2028")+u("2029")+`]$","default":"`+u("2028")+`","description":"a`+u("2028")+`b"}}}`, 0, lines(
		"{",
		"  // a",
		"  // b",
		"  // @pattern ^["+u("000d")+u("000a")+u("2028")+u("2029")+"]$ @default \""+u("2028")+"\"",
		"  sep?: string;",
		"}"))
}

func TestSchemaBudget(t *testing.T) {
	schema := `{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}`
	checkType(t, schema, 0, "{\n  // @integer @minimum 1\n  n?: number;\n}")
	checkType(t, schema, 20, "{ n?: number; }")
	checkType(t, schema, 10, "unknown")
	var fields []string
	for i := range 50 {
		fields = append(fields, fmt.Sprintf(`"field%d":{"type":"string"}`, i))
	}
	big := `{"type":"object","properties":{` + strings.Join(fields, ",") + `}}`
	checkType(t, big, 100, "unknown")
	if got := SchemaToType(sch(t, big), 0); !strings.Contains(got, "field49?: string;") {
		t.Errorf("got %s", got)
	}
}

func TestSchemaReferences(t *testing.T) {
	checkType(t, `{"type":"object","properties":{"item":{"$ref":"#/$defs/Item"},"legacy":{"$ref":"#/definitions/Legacy"},"remote":{"$ref":"https://example.com/schema.json"}},
		"required":["item"],"$defs":{"Item":{"type":"object","properties":{"id":{"type":"string"},"parent":{"$ref":"#/$defs/Item"}},"required":["id"]}},
		"definitions":{"Legacy":{"enum":["a","b"]}}}`, 0,
		`{ item: { id: string; parent?: unknown; }; legacy?: "a" | "b"; remote?: unknown; }`)
}

func TestSchemaArrays(t *testing.T) {
	checkType(t, `{"type":"array","items":{"type":"string"}}`, 0, "Array<string>")
	checkType(t, `{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}]}`, 0, "[string, number]")
	checkType(t, `{"type":"array"}`, 0, "unknown[]")
}

func TestToolDeclarations(t *testing.T) {
	got := RenderToolSignature(Tool{
		Name:         "hidden-dynamic-tool",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
	}, 0)
	if got != "hidden_dynamic_tool(args: { city: string; }): Promise<{ ok: boolean; }>;" {
		t.Errorf("got %s", got)
	}
	if got := RenderToolSignature(Tool{Name: "free"}, 0); got != "free(args: unknown): Promise<unknown>;" {
		t.Errorf("got %s", got)
	}
	got = RenderToolSignature(Tool{
		Name:         "plain",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{}}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"array","items":{"type":"object"}},"isError":{"type":"boolean"},"_meta":{"type":"object"}},"required":["content"]}`),
	}, 0)
	if want := "plain(args: { [key: string]: unknown; }): Promise<{ _meta?: { [key: string]: unknown; }; content: Array<{ [key: string]: unknown; }>; isError?: boolean; }>;"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	got = RenderToolSample(Tool{Name: "foo", Description: "bar", InputSchema: json.RawMessage(`{"type":"string"}`)}, 0)
	if got != "bar\n\ncodemode tool declaration:\n```ts\ndeclare const tools: { foo(args: string): Promise<unknown>; };\n```" {
		t.Errorf("got %q", got)
	}
}

func TestRenderDeclarations(t *testing.T) {
	got := RenderDeclarations([]Tool{
		{Name: "read", Description: "Read a file.\nSecond line.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			OutputSchema: json.RawMessage(`{"type":"string"}`)},
		{Name: "remote-api"},
	}, []Tool{{Name: "attach", Description: "Attach it.", InputSchema: json.RawMessage(`{"type":"string"}`)}})
	want := lines(
		"declare const tools: {",
		"  /**",
		"   * Read a file.",
		"   * Second line.",
		"   */",
		"  read(args: { path: string; }): Promise<string>;",
		"  remote_api(args: unknown): Promise<unknown>;",
		"};",
		"",
		"/** Attach it. */",
		"declare function attach(args: string): Promise<unknown>;")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	got = RenderDeclarations(nil, []Tool{
		{Name: "models.list", Description: "List models.", Signature: "(type: string): Promise<string[]>"},
		{Name: "models.get", InputSchema: json.RawMessage(`{"type":"string"}`)},
		{Name: "plain", Signature: "(): void"},
	})
	want = lines(
		"declare function plain(): void;",
		"",
		"declare const models: {",
		"  /** List models. */",
		"  list(type: string): Promise<string[]>;",
		"  get(args: string): Promise<unknown>;",
		"};")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := RenderDeclarations([]Tool{{Name: "x", Description: "a */ b"}}, nil); !strings.Contains(got, `/** a *\/ b */`) {
		t.Errorf("got %s", got)
	}
}
