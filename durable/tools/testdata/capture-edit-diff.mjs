// Captures what pi's generateDiffString (packages/durable/src/tools/edit-diff.ts)
// returns — the oracle behind TestGenerateDiffStringMatchesPi in package tools.
//
//   node capture-edit-diff.mjs <edit-diff.ts> <jsdiff package dir> <cases.json> <out.json>
//   e.g. git -C <pi> show d2e7d8b7:packages/durable/src/tools/edit-diff.ts > edit-diff.ts
//        node capture-edit-diff.mjs edit-diff.ts ./package \
//          ../../../internal/diff/testdata/jsdiff-8.0.4.json edit-diff-d2e7d8b7.json
//
// edit-diff.ts imports "diff", so it runs from a temporary directory beside a
// copy of jsdiff; node strips its types. The texts are the cases of
// internal/diff's jsdiff capture, at pi's default of 4 context lines.
import { cpSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const [source, jsdiff, casesPath, out] = process.argv.slice(2);
if (!out) {
	console.error("usage: node capture-edit-diff.mjs <edit-diff.ts> <jsdiff package dir> <cases.json> <out.json>");
	process.exit(2);
}
const dir = mkdtempSync(join(tmpdir(), "edit-diff-"));
try {
	cpSync(jsdiff, join(dir, "node_modules", "diff"), { recursive: true });
	cpSync(source, join(dir, "edit-diff.ts"));
	const { generateDiffString } = await import(pathToFileURL(join(dir, "edit-diff.ts")).href);
	const { cases } = JSON.parse(readFileSync(casesPath, "utf8"));
	// Long texts with sparse edits, so context is cut before, between, and
	// after changes.
	let state = 20261021;
	const next = () => {
		state = (state + 0x6d2b79f5) >>> 0;
		let t = state;
		t = Math.imul(t ^ (t >>> 15), t | 1);
		t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
		return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
	};
	for (let i = 0; i < 150; i++) {
		const base = Array.from({ length: 10 + Math.floor(next() * 120) }, (_, k) => `line ${k}`);
		const lines = [...base];
		for (let edits = 1 + Math.floor(next() * 4); edits > 0; edits--) {
			const at = Math.floor(next() * lines.length);
			const kind = next();
			if (kind < 0.4) lines[at] = `changed ${at}`;
			else if (kind < 0.7) lines.splice(at, 0, `added ${at}`);
			else lines.splice(at, 1);
		}
		const end = next() < 0.8 ? "\n" : "";
		cases.push({ name: `long ${i}`, old: base.join("\n") + end, new: lines.join("\n") + end });
	}
	const rows = cases.map((c) => {
		const { diff, firstChangedLine } = generateDiffString(c.old, c.new);
		return { name: c.name, old: c.old, new: c.new, diff, firstChangedLine: firstChangedLine ?? null };
	});
	writeFileSync(out, `${JSON.stringify({ source: "pi d2e7d8b7 packages/durable/src/tools/edit-diff.ts", cases: rows }, null, "\t")}\n`);
	console.log(`${rows.length} cases -> ${out}`);
} finally {
	rmSync(dir, { recursive: true, force: true });
}
