// Captures what real pi's validateToolArguments does with the array keywords
// that address positions — tuple "items" (an array of schemas), "prefixItems"
// and "additionalItems" — the oracle behind
// TestValidateToolArgumentsTupleItemsMatchesPi in package ai.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> tuple-items-59ca40cf.json 59ca40cf
//
// <extraction> holds `git archive <sha> packages/ai package-lock.json` from the
// upstream clone and a node_modules with typebox, the one runtime dependency
// of utils/validation.ts (types.ts is a type import, which
// --experimental-strip-types erases): `npm install --ignore-scripts
// typebox@<version packages/ai/package.json pins>`. The script refuses to
// write unless the installed typebox has the version AND integrity the sha's
// package-lock.json locks; both, with the node version, are recorded as
// `source`.
//
// Each case's parameters and arguments are JSON text, parsed with JSON.parse,
// so pi and the Go test start from the same objects. A plain JSON schema has
// no TypeBox Kind, so Value.Convert leaves it alone and every coercion comes
// from pi's own coerceWithJsonSchema; validation and its messages are
// TypeBox's compiled validator. `result` is JSON.stringify of the returned
// arguments, `error` the thrown message, received arguments included.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}

const lock = JSON.parse(fs.readFileSync(path.join(extraction, "package-lock.json"), "utf8"));
const installed = JSON.parse(fs.readFileSync(path.join(extraction, "node_modules/.package-lock.json"), "utf8"));
const locked = lock.packages?.["node_modules/typebox"];
const got = installed.packages?.["node_modules/typebox"];
if (!locked || !got || locked.version !== got.version || locked.integrity !== got.integrity) {
	console.error(`typebox mismatch: lockfile ${locked?.version} ${locked?.integrity}, installed ${got?.version} ${got?.integrity}`);
	process.exit(1);
}

const { validateToolArguments } = await import(
	pathToFileURL(path.join(extraction, "packages/ai/src/utils/validation.ts")).href
);

// p wraps an array schema as the one property of the tool's parameters.
const p = (array: object) => JSON.stringify({ type: "object", properties: { p: { type: "array", ...array } } });
const tuple = { items: [{ type: "number" }, { type: "string" }, { type: "boolean" }] };

const cases: Record<string, { parameters: string; arguments: string }> = {
	// Tuple items: every position is coerced and validated by its own schema;
	// positions past the tuple and a shorter array are not constrained.
	"tuple/coerces-by-position": { parameters: p(tuple), arguments: '{"p":["1",2,"true","extra"]}' },
	"tuple/errors-by-position": { parameters: p(tuple), arguments: '{"p":["x",{},3]}' },
	"tuple/shorter-array": { parameters: p(tuple), arguments: '{"p":["1"]}' },
	"tuple/empty-array": { parameters: p(tuple), arguments: '{"p":[]}' },
	// A boolean schema at a position: true accepts anything, false nothing,
	// and coercion skips both.
	"tuple/boolean-positions": { parameters: p({ items: [true, false] }), arguments: '{"p":["1","2"]}' },
	// An element that is not a schema makes TypeBox ignore "items" (and with it
	// "additionalItems"), while pi's coercion still walks the positions.
	"tuple/non-schema-element": {
		parameters: p({ items: [{ type: "number" }, 5], additionalItems: false }),
		arguments: '{"p":["1","x","y"]}',
	},
	"tuple/empty-items": { parameters: p({ items: [], additionalItems: false }), arguments: '{"p":[1]}' },
	// additionalItems: checked past the tuple only, never coerced, and TypeBox
	// stops at the first additional item that fails.
	"additionalItems/false": { parameters: p({ ...tuple, additionalItems: false }), arguments: '{"p":[1,"a",true,"x"]}' },
	"additionalItems/false-within-tuple": {
		parameters: p({ ...tuple, additionalItems: false }),
		arguments: '{"p":[1,"a",true]}',
	},
	"additionalItems/schema-not-coerced": {
		parameters: p({ ...tuple, additionalItems: { type: "number" } }),
		arguments: '{"p":[1,"a",true,"7",8]}',
	},
	"additionalItems/stops-at-first": {
		parameters: p({ ...tuple, additionalItems: { type: "number" } }),
		arguments: '{"p":[1,"a",true,"x","y"]}',
	},
	"additionalItems/ignored-for-single-items": {
		parameters: p({ items: { type: "number" }, additionalItems: false }),
		arguments: '{"p":[1,2]}',
	},
	"additionalItems/ignored-without-items": { parameters: p({ additionalItems: false }), arguments: '{"p":[1,2]}' },
	// additionalItems errors come before items errors (TypeBox's keyword order).
	"order/additionalItems-before-items": {
		parameters: p({ items: [{ type: "number" }], additionalItems: { type: "number" } }),
		arguments: '{"p":["x","y"]}',
	},
	// maxItems, then minItems, then prefixItems.
	"order/max-min-prefix": {
		parameters: p({ prefixItems: [{ type: "number" }], minItems: 3, maxItems: 1 }),
		arguments: '{"p":["x","y"]}',
	},
	// prefixItems: validated by position, never coerced by pi.
	"prefixItems/not-coerced": {
		parameters: p({ prefixItems: [{ type: "number" }, { type: "string" }] }),
		arguments: '{"p":["1",2]}',
	},
	"prefixItems/valid": { parameters: p({ prefixItems: [{ type: "number" }, { type: "string" }] }), arguments: '{"p":[1,"a",null]}' },
	// A single-schema "items" next to prefixItems validates from the end of the
	// prefix on, but pi's coercion applies it to every position.
	"prefixItems/items-validates-after-prefix": {
		parameters: p({ prefixItems: [{ type: "string" }], items: { type: "number" } }),
		arguments: '{"p":["a","b"]}',
	},
	"prefixItems/items-coerces-every-position": {
		parameters: p({ prefixItems: [{ type: "number" }], items: { type: "string" } }),
		arguments: '{"p":[1,2]}',
	},
	"prefixItems/items-false": { parameters: p({ prefixItems: [{ type: "number" }], items: false }), arguments: '{"p":[1,2]}' },
	"prefixItems/items-true": { parameters: p({ prefixItems: [{ type: "number" }], items: true }), arguments: '{"p":[1,"x"]}' },
	// A prefixItems element that is not a schema voids it, and "items" then
	// starts at position 0.
	"prefixItems/non-schema-element": {
		parameters: p({ prefixItems: [5], items: { type: "number" } }),
		arguments: '{"p":["x"]}',
	},
	// Tuple and prefixItems together: both constrain their positions.
	"prefixItems/with-tuple": {
		parameters: p({ prefixItems: [{ type: "string" }], items: [{ type: "number" }] }),
		arguments: '{"p":["1"]}',
	},
	// Nested schemas at a position: coercion and null-as-omission reach inside.
	"nested/coerces-inside-position": {
		parameters: p({ items: [{ type: "object", properties: { n: { type: "integer" } } }] }),
		arguments: '{"p":[{"n":"3"}]}',
	},
	"nested/null-as-omission": {
		parameters: p({
			items: [{ type: "object", properties: { a: { type: "string" }, b: { type: ["string", "null"] } } }, { type: "string" }],
		}),
		arguments: '{"p":[{"a":null,"b":null},null]}',
	},
	"nested/error-path": {
		parameters: p({ items: [{ type: "number" }, { type: "object", properties: { n: { type: "integer" } } }] }),
		arguments: '{"p":[1,{"n":"x"}]}',
	},
	// A union branch with a tuple: the value is coerced against the branch it
	// then validates under.
	"union/anyOf-tuple-branch": {
		parameters: JSON.stringify({
			type: "object",
			properties: { p: { anyOf: [{ type: "array", items: [{ type: "number" }] }, { type: "string" }] } },
		}),
		arguments: '{"p":["1"]}',
	},
};

const out: Record<string, unknown> = {
	sha,
	source: { typebox: { version: got.version, integrity: got.integrity }, node: process.version },
};
const results: Record<string, unknown> = {};
for (const [name, c] of Object.entries(cases)) {
	const tool = { name: "test_tool", description: "A test tool", parameters: JSON.parse(c.parameters) };
	const toolCall = { type: "toolCall", id: "call_1", name: "test_tool", arguments: JSON.parse(c.arguments) };
	try {
		results[name] = { ...c, result: JSON.stringify(validateToolArguments(tool, toolCall)) };
	} catch (error) {
		results[name] = { ...c, error: (error as Error).message };
	}
}
out.cases = results;
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
console.log(`wrote ${Object.keys(cases).length} cases to ${outFile}`);
