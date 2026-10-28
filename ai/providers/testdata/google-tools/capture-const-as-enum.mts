// Captures the parametersJsonSchema real pi's google convertTools sends for the
// const cases of packages/ai/test/google-shared-convert-tools.test.ts — the
// oracle behind TestGoogleParametersJSONSchemaConstAsEnumMatchesPi.
//
//   node --experimental-strip-types capture-const-as-enum.mts <extraction> <out.json> <sha>
//   e.g. ... capture-const-as-enum.mts <dir> const-as-enum-a7b94a03.json a7b94a03
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone) and a node_modules resolving pi-ai's dependencies at the
// versions the sha pins. Each case's parameters are JSON text, parsed with
// JSON.parse, so pi and the Go test start from the same object; the output is
// JSON.stringify of the declaration's parametersJsonSchema, which pins the key
// order as well (pi's own tests compare it the same way for the name maps).
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-const-as-enum.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const { convertTools } = await import(pathToFileURL(path.join(src, "api/google-shared.ts")).href);

const cases: Record<string, string> = {
	// "rewrites string and number const values to single-value enums"
	rewrites: JSON.stringify({
		type: "object",
		properties: {
			origin: { type: "string", const: "zq-src-7" },
			mode: { anyOf: [{ type: "string", const: "a" }, { type: "string", const: "b" }] },
			level: { oneOf: [{ const: 1 }, { const: 2 }] },
			flag: { type: "boolean", const: true },
			list: { type: "array", items: { const: "x" } },
			const: { type: "string" },
		},
		required: ["origin"],
	}),
	// "rewrites only schema-bearing keywords"
	schemaBearingOnly: JSON.stringify({
		type: "object",
		additionalProperties: { const: "x" },
		unevaluatedProperties: false,
		propertyNames: { const: "name" },
		allOf: [{ if: { const: "x" }, then: { const: "y" }, else: { not: { const: "z" } } }],
		properties: {
			list: {
				type: "array",
				items: [{ const: 1 }],
				prefixItems: [{ const: 2 }],
				additionalItems: { const: 3 },
				contains: { const: 4 },
				unevaluatedItems: { const: 5 },
			},
		},
		dependencies: { list: ["other"] },
	}),
	// Edge cases of this port: an existing enum keeps the const, const null and
	// an object const stay, and the enum lands after every other keyword.
	enumAlreadyPresent: '{"type":"object","properties":{"a":{"const":"x","enum":["x","y"]},"b":{"const":null},"c":{"const":{"k":1}},"d":{"const":"x","description":"after"}}}',
};
// "preserves literal data under %s"
for (const key of ["default", "examples", "enum", "const", "x-custom"]) {
	const literal = { const: "literal-data", properties: { nested: { const: 1 } } };
	cases[`literal/${key}`] = JSON.stringify({
		type: "object",
		properties: { value: { type: "object", [key]: key === "enum" || key === "examples" ? [literal] : literal } },
	});
}
// "preserves names while rewriting schemas in %s"
for (const key of ["properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"]) {
	cases[`names/${key}`] = `{"type":"object","${key}":{"__proto__":{"const":"x"},"const":{"const":"y"},"properties":{"const":"z"}}}`;
}

const out: Record<string, unknown> = { sha };
for (const [name, text] of Object.entries(cases)) {
	const decl = convertTools([{ name: "test_tool", description: "A test tool", parameters: JSON.parse(text) }])?.[0]
		?.functionDeclarations?.[0];
	out[name] = { parameters: text, parametersJsonSchema: JSON.stringify(decl?.parametersJsonSchema) };
}
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`wrote ${Object.keys(cases).length} cases to ${outFile}`);
