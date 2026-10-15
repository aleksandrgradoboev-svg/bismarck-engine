// Captures pi's codemode description and loadout presentation for the tools
// in tools.json — the oracle behind TestDescriptionMatchesPi and
// TestPrepareLoadoutMatchesPi.
//
//   node capture.mjs <pi checkout> <out.json>
//   e.g. git -C pi-upstream archive v1.2.0 packages/codemode packages/coding-agent/src/extensions/codemode | tar -x -C /tmp/pi
//        node capture.mjs /tmp/pi pi-v1.2.0.json
//
// extensions/codemode/tool.ts imports pi's settings, config and typebox, so it
// is not loaded as is: the functions from TEXT_OUTPUT_SCHEMA up to the script
// reach helpers are cut out of it, with the three settings it reads replaced by
// their v1.2.0 defaults, and run against pi-codemode's real declarations.ts.
// Node 24 strips the TypeScript types.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const [piDir, outFile] = process.argv.slice(2);
if (!piDir || !outFile) {
	console.error("usage: node capture.mjs <pi checkout> <out.json>");
	process.exit(2);
}
const toolTs = fs.readFileSync(path.join(piDir, "packages/coding-agent/src/extensions/codemode/tool.ts"), "utf8");
const start = toolTs.indexOf("const TEXT_OUTPUT_SCHEMA");
const end = toolTs.indexOf("/** Script globals that list");
if (start < 0 || end < 0) throw new Error("tool.ts markers not found");
let body = toolTs.slice(start, end);
const replace = (pattern, value) => {
	if (!pattern.test(body)) throw new Error(`tool.ts: ${pattern} not found`);
	body = body.replace(pattern, value);
};
replace(/export const codemodeSchema = Type\.Object\([\s\S]*?\n\}\);\n/, "");
replace(/export const CODEMODE_DOCS_PATH = .*\n/, 'export const CODEMODE_DOCS_PATH = "codemode.md";\n');
replace(/export const DEFAULT_CODEMODE_INLINE_BUDGET = .*\n/, "export const DEFAULT_CODEMODE_INLINE_BUDGET = 3000;\n");
replace(/SETTINGS_DEFAULTS\.codemode\.mode/, '"on"');
const declarations = pathToFileURL(path.resolve(piDir, "packages/codemode/src/declarations.ts")).href;
const harness = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "codemode-capture-")), "tool-slice.ts");
fs.writeFileSync(
	harness,
	`import { renderToolOutputType, renderToolSample, toCodemodeIdentifier } from ${JSON.stringify(declarations)};\n` +
		`const CODEMODE_TOOL_NAME = "codemode";\n${body}\nexport { prepareCodemodeLoadout };\n`,
);
const { createCodemodeDescription, prepareCodemodeLoadout } = await import(pathToFileURL(harness).href);

const here = path.dirname(fileURLToPath(import.meta.url));
const fixture = JSON.parse(fs.readFileSync(path.join(here, "tools.json"), "utf8"));
const byName = new Map(fixture.tools.map((tool) => [tool.name, tool]));
const tools = (names) => names.map((name) => byName.get(name));
const namespaces = new Map(Object.entries(fixture.namespaces));
// pi normalizes guidelines when it registers a tool (_normalizePromptGuidelines).
const normalize = (list) => [...new Set((list ?? []).map((g) => g.trim()).filter((g) => g.length > 0))];
const guidelines = new Map(Object.keys(fixture.guidelines).map((name) => [name, normalize(fixture.guidelines[name])]));

const descriptions = {};
for (const c of fixture.descriptionCases) {
	descriptions[c.name] = createCodemodeDescription(tools(c.tools), {
		namespaces,
		...(c.deferred ? { deferred: new Set(c.deferred) } : {}),
		...(c.guidelines ? { guidelines } : {}),
		...(c.budget === undefined ? {} : { inlineBudget: c.budget }),
	});
}
const loadouts = {};
for (const c of fixture.loadoutCases) {
	const loadout = {
		declared: tools(c.declared),
		callable: tools(c.callable),
		registered: fixture.tools,
		getExposure: (name) => fixture.exposures[name] ?? "direct",
		getNamespace: (name) => namespaces.get(name),
		getPromptGuidelines: (name) => guidelines.get(name) ?? [],
	};
	loadouts[c.name] = prepareCodemodeLoadout(loadout, {
		getMode: () => c.mode,
		...(c.budget === undefined ? {} : { getInlineBudget: () => c.budget }),
	});
}
fs.writeFileSync(
	outFile,
	`${JSON.stringify({ source: "pi v1.2.0 extensions/codemode/tool.ts", descriptions, loadouts }, null, 2)}\n`,
);
console.log(`wrote ${outFile}`);
