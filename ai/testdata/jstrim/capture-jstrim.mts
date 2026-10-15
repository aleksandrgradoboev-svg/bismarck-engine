// Captures how real pi's credential lookups treat a value that is blank only
// under one of the two whitespace vocabularies — U+FEFF (JavaScript trims it,
// Go's strings.TrimSpace does not) and U+0085 (the reverse) — the oracle behind
// jstrim_test.go in this package.
//
//   node --experimental-strip-types capture-jstrim.mts <extraction> <out.json> <sha>
//   e.g. ... capture-jstrim.mts <dir> jstrim-7140838fd.json 7140838fd
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai` from
// the upstream clone) and a node_modules resolving pi-ai's dependencies (the npm
// build 0.85.1's).
//
// `env` is defaultProviderAuthContext().env for a variable holding each value
// (null for undefined). `explicitKey` is compat.ts's hasExplicitApiKey, which
// decides whether options.apiKey stands or the provider's environment key
// replaces it. compat.ts itself cannot be imported from a source extraction
// (it loads generated provider data the repository does not carry), so the
// function's own source is cut out of it and run.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-jstrim.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { defaultProviderAuthContext } = await load("auth/context.ts");
const compatSource = fs.readFileSync(path.join(src, "compat.ts"), "utf8");
const fnSource = compatSource.match(/^function hasExplicitApiKey\([^]*?^}\n/m)?.[0];
if (!fnSource) throw new Error("hasExplicitApiKey not found in compat.ts");
const fnFile = path.join(os.tmpdir(), `pi-jstrim-has-explicit-${process.pid}.mts`);
fs.writeFileSync(fnFile, `export ${fnSource}`);
const { hasExplicitApiKey } = await import(pathToFileURL(fnFile).href);
fs.rmSync(fnFile);

const values = ["\ufeff", "\u0085", "\u00a0", " key "];

const env: Array<{ value: string; result: string | null }> = [];
for (const value of values) {
	process.env.PI_JSTRIM_CAPTURE = value;
	env.push({ value, result: (await defaultProviderAuthContext().env("PI_JSTRIM_CAPTURE")) ?? null });
}
delete process.env.PI_JSTRIM_CAPTURE;

const explicitKey = values.map((apiKey) => ({ apiKey, explicit: hasExplicitApiKey(apiKey) }));

fs.writeFileSync(outFile, `${JSON.stringify({ sha, env, explicitKey }, null, "\t")}\n`);
