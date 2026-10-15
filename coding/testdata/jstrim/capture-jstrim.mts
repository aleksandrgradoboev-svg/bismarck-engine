// Captures what real pi's coding agent does with text that is blank, or
// padded, only under one of the two whitespace vocabularies — U+FEFF
// (JavaScript trims it, Go's strings.TrimSpace does not) and U+0085 (the
// reverse) — plus the whitespace the YAML frontmatter parser, the `ignore`
// matcher and JSON.parse each accept on their own terms. The oracle behind
// jstrim_test.go in this package.
//
//   node --experimental-strip-types capture-jstrim.mts <extraction> <out.json> <sha> [<name>=<package dir>...]
//   e.g. ... capture-jstrim.mts <dir> jstrim-7140838fd.json 7140838fd ignore=<copy of ignore 7.0.8>
//
// <extraction> holds packages/{ai,agent,coding-agent,tui} and package-lock.json
// at <sha> (`git archive <sha> packages/ai packages/agent packages/coding-agent
// packages/tui package-lock.json` from the upstream clone), a node_modules
// resolving coding-agent's dependencies, and @earendil-works/{pi-ai,
// pi-agent-core,pi-tui} shims whose src is the extracted packages' src.
//
// The yaml and ignore packages ARE the semantics under test, so each must be
// the exact version <sha>'s package-lock.json resolves for coding-agent: a
// <name>=<package dir> argument routes every import of <name> to that
// directory, and the capture refuses to run unless the package pi actually
// loaded carries the lock's version. The lock's version and integrity are
// recorded under `pins`; that a directory holds the very tarball the integrity
// names is for whoever passes it to establish.
//
// Every fixture is built here and recorded next to its outcome, so the Go test
// replays the same bytes: frontmatter documents, skill trees (paths relative to
// their root), .git files, session files and image mime types.
import fs from "node:fs";
import { registerHooks } from "node:module";
import os from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const [extraction, outFile, sha, ...pinArgs] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error(
		"usage: node --experimental-strip-types capture-jstrim.mts <extraction> <out.json> <sha> [<name>=<package dir>...]",
	);
	process.exit(2);
}

// ---- pinned packages -------------------------------------------------------------

const PINNED = ["ignore", "yaml"];
const lock = JSON.parse(fs.readFileSync(path.join(extraction, "package-lock.json"), "utf8"));
const pins: Record<string, { version: string; integrity: string }> = {};
for (const name of PINNED) {
	const entry = lock.packages[`packages/coding-agent/node_modules/${name}`] ?? lock.packages[`node_modules/${name}`];
	if (!entry?.version || !entry?.integrity) {
		console.error(`${sha}'s package-lock.json resolves no ${name} for coding-agent; drop it from PINNED if pi no longer uses it`);
		process.exit(2);
	}
	pins[name] = { version: entry.version, integrity: entry.integrity };
}

const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "pi-jstrim-pins-"));
process.on("exit", () => fs.rmSync(scratch, { recursive: true, force: true }));
const overrideParents = new Map<string, string>();
for (const arg of pinArgs) {
	const eq = arg.indexOf("=");
	const name = arg.slice(0, eq);
	if (eq <= 0 || !PINNED.includes(name)) {
		console.error(`unknown pin ${JSON.stringify(arg)}: expected <name>=<package dir> with <name> one of ${PINNED.join(", ")}`);
		process.exit(2);
	}
	// Resolving from a file beside node_modules/<name> -> <dir> lands on <dir>'s
	// real path, exactly as a normal install would.
	const parent = path.join(scratch, name);
	fs.mkdirSync(path.join(parent, "node_modules"), { recursive: true });
	fs.symlinkSync(path.resolve(arg.slice(eq + 1)), path.join(parent, "node_modules", name), "dir");
	overrideParents.set(name, pathToFileURL(path.join(parent, "importer.mjs")).href);
}

const loadedRoots = new Map<string, Set<string>>(PINNED.map((name) => [name, new Set<string>()]));
const packageRoot = (name: string, url: string) => {
	for (let dir = path.dirname(fileURLToPath(url)); dir !== path.dirname(dir); dir = path.dirname(dir)) {
		const manifest = path.join(dir, "package.json");
		if (fs.existsSync(manifest) && JSON.parse(fs.readFileSync(manifest, "utf8")).name === name) return dir;
	}
	throw new Error(`no package.json named ${name} above ${url}`);
};
registerHooks({
	resolve(specifier, context, nextResolve) {
		const name = PINNED.find((n) => specifier === n || specifier.startsWith(`${n}/`));
		if (!name) return nextResolve(specifier, context);
		const parentURL = overrideParents.get(name);
		const result = nextResolve(specifier, parentURL ? { ...context, parentURL } : context);
		loadedRoots.get(name)!.add(packageRoot(name, result.url));
		return result;
	},
});

const src = path.join(extraction, "packages/coding-agent/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { parseFrontmatter } = await load("utils/frontmatter.ts");
const { loadSkillsFromDir } = await load("core/skills.ts");
const { findGitPaths } = await load("core/footer-data-provider.ts");
const { findExactModelReferenceMatch } = await load("core/model-resolver.ts");
const { processImage } = await load("utils/image-process.ts");
const { loadEntriesFromFile, SessionManager } = await load("core/session-manager.ts");

for (const name of PINNED) {
	const roots = [...loadedRoots.get(name)!];
	const versions = roots.map((root) => JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8")).version);
	if (roots.length !== 1 || versions[0] !== pins[name].version) {
		console.error(
			`pi loaded ${name} ${JSON.stringify(versions)} from ${JSON.stringify(roots)}, but ${sha}'s lock pins ` +
				`${pins[name].version} (${pins[name].integrity}); pass ${name}=<dir of ${name}@${pins[name].version}>`,
		);
		process.exit(1);
	}
}

const BOM = "\ufeff";
const NEL = "\u0085";
const NBSP = "\u00a0";

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "pi-jstrim-"));
const writeTree = (root: string, files: Record<string, string>) => {
	for (const [rel, content] of Object.entries(files)) {
		const full = path.join(root, rel);
		fs.mkdirSync(path.dirname(full), { recursive: true });
		fs.writeFileSync(full, content);
	}
};

// ---- frontmatter ----------------------------------------------------------------

const frontmatterDocs: Record<string, string> = {
	keyTrailingNbsp: `---\nname${NBSP}: foo\ndescription: d\n---\nbody`,
	keyTrailingNel: `---\nname${NEL}: foo\ndescription: d\n---\nbody`,
	valueTrailingNbsp: `---\nname: foo${NBSP}\ndescription: d\n---\nbody`,
	valueTrailingNel: `---\nname: foo${NEL}\ndescription: d\n---\nbody`,
	valueTrailingBom: `---\nname: foo${BOM}\ndescription: d\n---\nbody`,
	valueTrailingTab: `---\nname: foo\t\ndescription: d\n---\nbody`,
	continuationNbsp: `---\ndescription: a\n  b${NBSP}\n---\nbody`,
	continuationNel: `---\ndescription: a\n  b${NEL}\n---\nbody`,
	bodyNel: `---\nname: foo\n---\n${NEL}body${NEL}`,
	bodyBom: `---\nname: foo\n---\n${BOM}body${BOM}`,
	bodyNbsp: `---\nname: foo\n---\n${NBSP}body${NBSP}`,
	// The yaml lexer drops one U+FEFF from the start of every line it reads
	// before the document begins (blank and comment lines included), and none
	// after.
	streamBomFirstKey: `---\n${BOM}name: foo\ndescription: d\n---\nbody`,
	streamBomAfterBlank: `---\n\n${BOM}name: foo\n---\nbody`,
	streamBomAfterComment: `---\n# c\n${BOM}name: foo\n---\nbody`,
	streamBomEveryLine: `---\n${BOM}\n${BOM}  \n${BOM}name: foo\n---\nbody`,
	streamBomComment: `---\n${BOM}# c: x\nname: foo\n---\nbody`,
	streamBomBlock: `---\n${BOM}description: >-\n  a\n  b\n---\nbody`,
	streamBomTwice: `---\n${BOM}${BOM}name: foo\n---\nbody`,
	documentBom: `---\nname: foo\n${BOM}description: d\n---\nbody`,
	documentBomAfterBlank: `---\nname: foo\n\n${BOM}description: d\n---\nbody`,
};
const frontmatter: Record<string, unknown> = {};
for (const [name, doc] of Object.entries(frontmatterDocs)) {
	try {
		frontmatter[name] = { doc, ...parseFrontmatter(doc) };
	} catch (error) {
		frontmatter[name] = { doc, error: (error as Error).message };
	}
}

// ---- skills: descriptions and ignore rules ----------------------------------------

// pi splits an ignore file on /\r?\n/, so only a CR that is not the one
// before a line's LF stays on the line; the ignore matcher drops a trailing CR
// run and then a trailing run of spaces (and only spaces) unless a backslash
// escapes it.
const skill = (name: string, description: string) => `---\nname: ${name}\ndescription: ${description}\n---\nbody\n`;
const skillTrees: Record<string, Record<string, string>> = {
	whitespace: {
		".gitignore": [
			`${BOM}qux`,
			`foo${NEL}`,
			`bar${NBSP}`,
			`${BOM}#comment`,
			"sp ace ",
			"\tlead",
			"esc\\ 　",
			"dbl\\\\ ",
			"tri\\\\\\ ",
			"tab\t",
			"mix\t ",
			`nbsp${NBSP} `,
			"cr\r\r",
			"ecr\\\r\r",
			"sq \r\r",
			"spcr\r ",
			"anch/ ",
			"neg",
			"\\!neg",
			"/#hash",
			"//slash",
			"lone/\\",
			"two\\\\",
			"",
		].join("\n"),
		"foo/SKILL.md": skill("foo", "d"),
		"bar/SKILL.md": skill("bar", "d"),
		"baz/SKILL.md": skill("baz", "d"),
		"qux/SKILL.md": skill("qux", "d"),
		"#comment/SKILL.md": skill("comment", "d"),
		"sp ace/SKILL.md": skill("space", "d"),
		"\tlead/SKILL.md": skill("lead", "d"),
		"esc /SKILL.md": skill("esc", "d"),
		"dbl\\/SKILL.md": skill("dbl", "d"),
		"dbl\\ /SKILL.md": skill("dblspace", "d"),
		"tri\\/SKILL.md": skill("tri", "d"),
		"tri\\ /SKILL.md": skill("trispace", "d"),
		"tab/SKILL.md": skill("tab", "d"),
		"tab\t/SKILL.md": skill("tabtab", "d"),
		"mix\t/SKILL.md": skill("mix", "d"),
		[`nbsp${NBSP}/SKILL.md`]: skill("nbsp", "d"),
		"cr/SKILL.md": skill("cr", "d"),
		"ecr/SKILL.md": skill("ecr", "d"),
		"ecr\r/SKILL.md": skill("ecrcr", "d"),
		"sq/SKILL.md": skill("sq", "d"),
		"spcr/SKILL.md": skill("spcr", "d"),
		"spcr\r/SKILL.md": skill("spcrcr", "d"),
		"anch/SKILL.md": skill("anch", "d"),
		"nest/anch/SKILL.md": skill("nestanch", "d"),
		"neg/SKILL.md": skill("neg", "d"),
		"#hash/SKILL.md": skill("hash", "d"),
		"slash/SKILL.md": skill("slash", "d"),
		"nest/slash/SKILL.md": skill("nestslash", "d"),
		"lone/\\/SKILL.md": skill("lone", "d"),
		"two\\/SKILL.md": skill("two", "d"),
		"bomdesc.md": skill("bomdesc", `"${BOM}"`),
		"neldesc.md": skill("neldesc", `"${NEL}"`),
		"declared/SKILL.md": skill("declared", `"${BOM}"`),
	},
	// A rule whose body is left empty matches every path: "!" un-ignores
	// everything before it, and "/ \r" ignores everything.
	negateAll: {
		".gitignore": "foo\n!\n",
		"foo/SKILL.md": skill("foo", "d"),
		"bar/SKILL.md": skill("bar", "d"),
	},
	ignoreAll: {
		".gitignore": "/ \r\r\n",
		"foo/SKILL.md": skill("foo", "d"),
		"root.md": skill("root", "d"),
	},
	// A pattern of nothing but spaces is a blank line, not an empty body.
	blank: {
		".gitignore": "/   \n",
		"foo/SKILL.md": skill("foo", "d"),
	},
};
const skills: Record<string, unknown> = {};
for (const [name, tree] of Object.entries(skillTrees)) {
	const root = path.join(tmp, `skills-${name}`);
	writeTree(root, tree);
	const result = loadSkillsFromDir({ dir: root, source: "user" });
	skills[name] = {
		tree,
		names: result.skills.map((s: { name: string }) => s.name).sort(),
		descriptions: Object.fromEntries(
			result.skills.map((s: { name: string; description: string }) => [s.name, s.description]),
		),
		diagnostics: result.diagnostics
			.map((d: { message: string; path: string }) => ({ message: d.message, path: path.relative(root, d.path) }))
			.sort((a: { path: string }, b: { path: string }) => a.path.localeCompare(b.path)),
	};
}

// ---- git paths ---------------------------------------------------------------------

const gitCases: Record<string, Record<string, string>> = {
	// A worktree whose pointer file and commondir carry a BOM.
	bom: {
		"main/.git/HEAD": "ref: refs/heads/main\n",
		"main/.git/worktrees/wt/HEAD": "ref: refs/heads/wt\n",
		"main/.git/worktrees/wt/commondir": `${BOM}../..\n`,
		"wt/.git": `${BOM}gitdir: ../main/.git/worktrees/wt\n`,
	},
	// A pointer whose target keeps a trailing NEL, which no directory has.
	nel: {
		"main/.git/HEAD": "ref: refs/heads/main\n",
		"main/.git/worktrees/wt/HEAD": "ref: refs/heads/wt\n",
		"wt/.git": `gitdir: ../main/.git/worktrees/wt${NEL}`,
	},
};
const gitPaths: Record<string, unknown> = {};
for (const [name, tree] of Object.entries(gitCases)) {
	const root = path.join(tmp, `git-${name}`);
	writeTree(root, tree);
	const found = findGitPaths(path.join(root, "wt"));
	gitPaths[name] = {
		tree,
		result: found
			? { repoDir: path.relative(root, found.repoDir), commonGitDir: path.relative(root, found.commonGitDir) }
			: null,
	};
}

// ---- model references ----------------------------------------------------------------

const models = [
	{ provider: "anthropic", id: "claude-sonnet-4-5", name: "Claude Sonnet 4.5" },
	{ provider: "openai", id: "gpt-4o", name: "GPT-4o" },
];
const modelReferences = [
	`${BOM}anthropic/claude-sonnet-4-5${BOM}`,
	`anthropic/${BOM}claude-sonnet-4-5`,
	`anthropic/${NEL}claude-sonnet-4-5`,
	`${NEL}gpt-4o`,
	`openai / gpt-4o\u3000`,
].map((reference) => {
	const match = findExactModelReferenceMatch(reference, models);
	return { reference, match: match ? `${match.provider}/${match.id}` : null };
});

// ---- image mime types ----------------------------------------------------------------

// A 1x1 opaque PNG.
const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC";
const images = [];
for (const mimeType of [`${BOM}image/png`, `${NEL}image/png`, `image/png${NBSP}`]) {
	const result = await processImage(Buffer.from(pngBase64, "base64"), mimeType, { autoResizeImages: false });
	images.push({ mimeType, ok: result.ok, resultMimeType: result.mimeType, hints: result.hints ?? [] });
}

// ---- session files -------------------------------------------------------------------

const sessionDir = path.join(tmp, "sessions");
fs.mkdirSync(sessionDir);
const cwd = path.join(tmp, "project");
fs.mkdirSync(cwd);
const header = (id: string) =>
	JSON.stringify({ type: "session", version: 3, id, timestamp: "2026-01-01T00:00:00.000Z", cwd });
const message = (id: string, parentId: string | null) =>
	JSON.stringify({
		type: "message",
		id,
		parentId,
		timestamp: "2026-01-01T00:00:01.000Z",
		message: { role: "user", content: id, timestamp: 1 },
	});
// JSON.parse accepts only its own four whitespace characters around a value;
// the blank check in front of it is String.prototype.trim.
const sessionFiles: Record<string, string> = {
	"entries.jsonl": [
		header("entries"),
		`${message("nbsp", null)}${NBSP}`,
		`${message("nel", null)}${NEL}`,
		`${BOM}${message("bom", null)}`,
		`${message("cr", null)}\r`,
		`\t${message("tab", null)} `,
		BOM,
		NEL,
		"",
	].join("\n"),
	"nbsp-header.jsonl": [`${header("nbsp-header")}${NBSP}`, ""].join("\n"),
	"nel-header.jsonl": [`${header("nel-header")}${NEL}`, message("after-nel", null), ""].join("\n"),
	"padded-header.jsonl": [`\t${header("padded-header")}\r`, ""].join("\n"),
	"blank-lead.jsonl": [BOM, NEL, header("blank-lead"), ""].join("\n"),
};
writeTree(sessionDir, sessionFiles);
const entries = loadEntriesFromFile(path.join(sessionDir, "entries.jsonl")).map((e: { id: string }) => e.id);
const listed = await SessionManager.list(cwd, sessionDir);
const sessions = {
	// The headers' cwd, which the Go test replaces with its own.
	cwd,
	files: sessionFiles,
	entries,
	messageCounts: Object.fromEntries(
		listed.map((s: { path: string; messageCount: number }) => [path.basename(s.path), s.messageCount]),
	),
	found: Object.fromEntries(
		["entries", "nbsp-header", "nel-header", "padded-header", "blank-lead"].map((id) => {
			const found = SessionManager.findById(cwd, id, sessionDir);
			return [id, found ? path.basename(found) : null];
		}),
	),
};

fs.rmSync(tmp, { recursive: true, force: true });

const out = { sha, pins, frontmatter, skills, gitPaths, modelReferences, pngBase64, images, sessions };
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
