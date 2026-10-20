// Captures what jsdiff 8.0.4 returns for diffLines and createTwoFilesPatch —
// the oracle behind TestDiffLinesMatchesJSDiff and
// TestCreateTwoFilesPatchMatchesJSDiff in package diff.
//
//   node capture.mjs <jsdiff package dir> <out.json>
//   e.g. npm pack diff@8.0.4 && tar xzf diff-8.0.4.tgz
//        node capture.mjs ./package jsdiff-8.0.4.json
//
// The texts come from a seeded generator, so a regen with the same jsdiff
// writes the same file: lines from a small alphabet (so lines repeat and the
// algorithm has choices to make), CRLF and lone CR, texts with and without a
// final newline, empty texts, and edits of a base text (insert, delete,
// replace, move) as well as unrelated pairs.
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const [dir, out] = process.argv.slice(2);
if (!dir || !out) {
	console.error("usage: node capture.mjs <jsdiff package dir> <out.json>");
	process.exit(2);
}
const jsdiff = await import(pathToFileURL(join(dir, "libesm", "index.js")).href);
const version = JSON.parse(await (await import("node:fs/promises")).readFile(join(dir, "package.json"), "utf8")).version;

function random(seed) {
	let state = seed >>> 0;
	return () => {
		state = (state + 0x6d2b79f5) >>> 0;
		let t = state;
		t = Math.imul(t ^ (t >>> 15), t | 1);
		t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
		return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
	};
}

const WORDS = ["a", "b", "c", "alpha", "beta", "", "  x", "é", "🙂", "}", "{"];
const ENDINGS = ["\n", "\n", "\n", "\n", "\r\n", "\r"];

function text(next, lines) {
	const parts = [];
	for (let i = 0; i < lines.length; i++) {
		parts.push(lines[i]);
		if (i < lines.length - 1 || next() < 0.7) parts.push(ENDINGS[Math.floor(next() * ENDINGS.length)]);
	}
	return parts.join("");
}

function randomLines(next, max) {
	return Array.from({ length: Math.floor(next() * max) }, () => WORDS[Math.floor(next() * WORDS.length)]);
}

function edited(next, base) {
	const lines = [...base];
	const ops = 1 + Math.floor(next() * 4);
	for (let op = 0; op < ops; op++) {
		const at = Math.floor(next() * (lines.length + 1));
		const kind = next();
		if (kind < 0.3) lines.splice(at, 0, ...randomLines(next, 4));
		else if (kind < 0.6) lines.splice(at, 1 + Math.floor(next() * 3));
		else if (kind < 0.9) lines.splice(at, 1, WORDS[Math.floor(next() * WORDS.length)]);
		else lines.push(...lines.splice(at, 2));
	}
	return lines;
}

const cases = [
	// pi's tools.test "applies disjoint edits and returns both diff formats".
	{ name: "pi edit", old: "alpha\nbeta\ngamma\ndelta\n", new: "ALPHA\nbeta\nGAMMA\ndelta\n", context: 4 },
	{ name: "both empty", old: "", new: "", context: 4 },
	{ name: "from empty", old: "", new: "a\nb", context: 4 },
	{ name: "to empty", old: "a\nb\n", new: "", context: 4 },
	{ name: "newline added at end", old: "a\nb", new: "a\nb\n", context: 4 },
	{ name: "far apart", old: Array.from({ length: 30 }, (_, i) => `l${i}\n`).join(""), new: Array.from({ length: 30 }, (_, i) => (i === 3 || i === 25 ? `L${i}\n` : `l${i}\n`)).join(""), context: 4 },
];
const next = random(20261020);
for (let i = 0; i < 400; i++) {
	const base = randomLines(next, 14);
	const other = next() < 0.8 ? edited(next, base) : randomLines(next, 14);
	const context = [0, 1, 2, 4, 4, 4][Math.floor(next() * 6)];
	cases.push({ name: `random ${i}`, old: text(next, base), new: text(next, other), context });
}

for (const c of cases) {
	c.changes = jsdiff.diffLines(c.old, c.new).map((p) => [p.count, !!p.added, !!p.removed, p.value]);
	c.patch = jsdiff.createTwoFilesPatch("f.txt", "f.txt", c.old, c.new, undefined, undefined, {
		context: c.context,
		headerOptions: jsdiff.FILE_HEADERS_ONLY,
	});
}
writeFileSync(out, `${JSON.stringify({ jsdiff: version, cases }, null, "\t")}\n`);
console.log(`${cases.length} cases from jsdiff ${version} -> ${out}`);
