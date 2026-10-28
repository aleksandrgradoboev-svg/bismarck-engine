// Captures how real pi's StreamingJsonParser reads documents streamed in
// pieces — the oracle behind TestStreamingJSONMatchesPi in package ai.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> capture-f715a15c.json f715a15c
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone). No node_modules is needed: streaming-json.ts imports only
// types and parseToolCallArguments, which the parser itself never calls; the
// capture loads the module's own text with those two imports replaced.
//
// The documents are pi's streaming-json.test.ts generator (seeds 11 and 12, 40 each,
// spaced and compact), its malformed list, and a few more: invalid escapes,
// array-index keys, a duplicate key, number edges and nesting past 32 levels.
// Pieces are whole code points, as a Go string holds them. Every step records
// the changes Append returned; the snapshot after each piece is recorded for
// pieces of 7 code points, and only at the end for pieces of 1, which keeps
// the file small.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, out, sha] = process.argv.slice(2);
const source = fs
	.readFileSync(path.join(extraction!, "packages/ai/src/utils/streaming-json.ts"), "utf8")
	.replace('import type { JsonValue, ToolCall } from "../types.ts";', "type JsonValue = any; type ToolCall = any;")
	.replace('import { parseToolCallArguments } from "./json-parse.ts";', 'const parseToolCallArguments = (): never => { throw new Error("not captured"); };');
const module = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "streaming-json-")), "streaming-json.ts");
fs.writeFileSync(module, source);
const { StreamingJsonParser } = await import(pathToFileURL(module).href);

function random(seed: number): () => number {
	let state = seed;
	return () => {
		state = (state * 1103515245 + 12345) & 0x7fffffff;
		return state / 0x7fffffff;
	};
}
type Random = () => number;
const pick = <T>(rand: Random, items: readonly T[]): T => items[Math.floor(rand() * items.length)]!;
const STRING_PIECES = ["a", "b", "x y", " ", "\n", "\t", "\\", '"', "/", "é", "😀", "\u0001", "\u001f", " ", "{", "]", ","];
function randomString(rand: Random): string {
	let text = "";
	const length = Math.floor(rand() * 6);
	for (let index = 0; index < length; index++) text += pick(rand, STRING_PIECES);
	return text;
}
function randomScalar(rand: Random): any {
	return pick(rand, [
		() => randomString(rand),
		() => Math.floor(rand() * 2000) - 1000,
		() => rand() * 100,
		() => 1e21,
		() => -5e-8,
		() => -0,
		() => 0,
		() => true,
		() => false,
		() => null,
	])();
}
function randomValue(rand: Random, depth: number): any {
	const roll = rand();
	if (depth > 3 || roll < 0.3) return randomScalar(rand);
	if (roll < 0.65) {
		const members: { [key: string]: any } = {};
		const count = Math.floor(rand() * 4);
		for (let index = 0; index < count; index++) members[randomString(rand)] = randomValue(rand, depth + 1);
		return members;
	}
	return Array.from({ length: Math.floor(rand() * 4) }, () => randomValue(rand, depth + 1));
}
function render(value: any, rand: Random, spaced: boolean): string {
	const space = () => (spaced ? pick(rand, ["", "", " ", "\n  ", "\t", "\r\n"]) : "");
	if (Array.isArray(value)) {
		return `[${space()}${value.map((item) => render(item, rand, spaced)).join(`${space()},${space()}`)}${space()}]`;
	}
	if (value !== null && typeof value === "object") {
		const members = Object.entries(value).map(
			([key, item]) => `${JSON.stringify(key)}${space()}:${space()}${render(item, rand, spaced)}`,
		);
		return `{${space()}${members.join(`${space()},${space()}`)}${space()}}`;
	}
	return Object.is(value, -0) ? "-0" : JSON.stringify(value);
}
function randomDocuments(seed: number, count: number, spaced: boolean): string[] {
	const rand = random(seed);
	return Array.from({ length: count }, () =>
		render(rand() < 0.8 ? { path: randomString(rand), args: randomValue(rand, 0) } : randomValue(rand, 0), rand, spaced),
	);
}

const MALFORMED = [
	'{"a":1 "b":2}',
	'{"a" 1, "b": 2}',
	"{a:1, 'b': 2}",
	'{"a":1,,"b":2,}',
	"[1 2, , 3,]",
	'{"a":[1,2}, "b":3]',
	"]]]}}}{",
	'{"a":NaN,"b":Infinity,"c":-Infinity,"d":undefined,"e":1}',
	'{"a":01,"b":1.,"c":.5,"d":+1,"e":0x10,"f":1e}',
	'{"a":"x"} trailing garbage {"b":2}',
	'{"a": tru, "b": nulll, "c": falsey}',
	'{"a":{"b":{"c":',
	'{"a\\": 1}',
	'"unterminated \\',
	'// comment\n{"a":1}',
	'\u0000{\u0001"a"\u0002:\u00031}',
	'{"a":"x"}}}]]]',
	String.raw`{"path":"a.tex","content":"\usepackage{x}","mode":"w"}`,
	String.raw`{"a":"xé\n\\\"\q \u12 \uZZZZ"}`,
	'{"b":1,"2":2,"1":1,"a":{"10":1,"9":2}}',
	'{"a":1,"b":2,"a":"three"}',
	`{"n":-1.25e+4,"m":2.5E-,"o":12.,"p":1e400,"q":-0}`,
	`${'{"a":'.repeat(40)}"deep"${"}".repeat(40)}`,
];

const documents = [...randomDocuments(11, 40, true), ...randomDocuments(12, 40, false), ...MALFORMED];
const cases = [];
for (const text of documents) {
	const points = [...text];
	for (const size of [1, 7]) {
		const parser = new StreamingJsonParser();
		const steps = [];
		for (let index = 0; index < points.length; index += size) {
			const piece = points.slice(index, index + size).join("");
			const changes = parser.append(piece).map((change) =>
				change.kind === "set"
					? { kind: "set", path: change.path, value: JSON.stringify(change.value) }
					: { kind: "append", path: change.path, text: change.text },
			);
			const snapshot = parser.snapshot();
			const last = index + size >= points.length;
			const step: any = { piece, changes };
			if (size === 7 || last) Object.assign(step, { has: snapshot !== undefined, snapshot: snapshot === undefined ? "" : JSON.stringify(snapshot) });
			steps.push(step);
		}
		cases.push({ text, size, steps });
	}
}
// The copy credit: a large array kept open lags in the snapshots; how many
// items each snapshot shows pins the credit rules.
const lagText = JSON.stringify({ items: Array.from({ length: 3000 }, (_, index) => index) });
const lagParser = new StreamingJsonParser();
const lag: number[] = [];
for (let index = 0; index < lagText.length; index += 16) {
	lagParser.append(lagText.slice(index, index + 16));
	lag.push((lagParser.snapshot()?.items ?? []).length);
}

fs.writeFileSync(out!, `${JSON.stringify({ source: `pi packages/ai/src/utils/streaming-json.ts at ${sha}, node ${process.version}`, lag: { text: lagText, piece: 16, items: lag }, cases })}
`);
console.log(cases.length, "cases");
