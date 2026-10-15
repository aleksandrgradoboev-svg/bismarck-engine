// Shared canonicalizer for the pi-vs-Go differential.
//
// Both sides feed their *serialized wire JSON* through an equivalent
// canonicalizer (this one; go/canon.go is the line-for-line twin) and the
// runner diffs the results. See ../README.md for exactly what is normalized
// and what is deliberately left alone.

const RAW = Symbol("raw-number");

/**
 * Parse JSON while preserving each number's original literal text, so that
 * `1024` vs `1024.0` vs `1e3` stays visible instead of being laundered
 * through a float round-trip.
 */
export function parsePreservingNumbers(text) {
	return JSON.parse(text, function (key, value, context) {
		if (typeof value === "number" && context && typeof context.source === "string") {
			return { [RAW]: context.source };
		}
		if (typeof value === "number") {
			// Fallback for runtimes without source access.
			return { [RAW]: String(value) };
		}
		return value;
	});
}

function isRawNumber(v) {
	return v !== null && typeof v === "object" && Object.getOwnPropertySymbols(v).includes(RAW);
}

// JSON string escaping matched to JSON.stringify (ES2019 well-formed):
// escape the mandatory control set and lone surrogates, and nothing else.
// Notably U+2028/U+2029 are NOT escaped, and <, >, & are NOT escaped.
export function encodeString(s) {
	let out = '"';
	for (let i = 0; i < s.length; i++) {
		const c = s.charCodeAt(i);
		const ch = s[i];
		if (ch === '"') out += '\\"';
		else if (ch === "\\") out += "\\\\";
		else if (ch === "\n") out += "\\n";
		else if (ch === "\r") out += "\\r";
		else if (ch === "\t") out += "\\t";
		else if (ch === "\b") out += "\\b";
		else if (ch === "\f") out += "\\f";
		else if (c < 0x20) out += "\\u" + c.toString(16).padStart(4, "0");
		else if (c >= 0xd800 && c <= 0xdbff) {
			const next = s.charCodeAt(i + 1);
			if (next >= 0xdc00 && next <= 0xdfff) {
				out += ch + s[i + 1];
				i++;
			} else out += "\\u" + c.toString(16).padStart(4, "0"); // lone high surrogate
		} else if (c >= 0xdc00 && c <= 0xdfff) {
			out += "\\u" + c.toString(16).padStart(4, "0"); // lone low surrogate
		} else out += ch;
	}
	return out + '"';
}

/** Canonical body: object keys sorted, arrays untouched, numbers verbatim. */
export function canonicalBody(v, indent = 0) {
	const pad = "  ".repeat(indent);
	const padIn = "  ".repeat(indent + 1);
	if (v === null) return "null";
	if (isRawNumber(v)) return v[RAW];
	if (typeof v === "boolean") return String(v);
	if (typeof v === "string") return encodeString(v);
	if (Array.isArray(v)) {
		if (v.length === 0) return "[]";
		return "[\n" + v.map((e) => padIn + canonicalBody(e, indent + 1)).join(",\n") + "\n" + pad + "]";
	}
	const keys = Object.keys(v).sort();
	if (keys.length === 0) return "{}";
	return (
		"{\n" +
		keys.map((k) => padIn + encodeString(k) + ": " + canonicalBody(v[k], indent + 1)).join(",\n") +
		"\n" +
		pad +
		"}"
	);
}

/**
 * Key-order report: one line per object, `<path>\t<keys in original order>`.
 * Traversal itself is by SORTED keys so both sides walk identical paths even
 * when their orders differ — a pure order difference shows up in the value
 * column, never as a phantom structural difference.
 */
export function orderReport(v, path = "$", lines = []) {
	if (v === null || isRawNumber(v) || typeof v !== "object") return lines;
	if (Array.isArray(v)) {
		v.forEach((e, i) => orderReport(e, `${path}[${i}]`, lines));
		return lines;
	}
	const original = Object.keys(v);
	lines.push(`${path}\t${original.join(",")}`);
	for (const k of [...original].sort()) orderReport(v[k], `${path}.${k}`, lines);
	return lines;
}

export function canonicalize(wireText) {
	const parsed = parsePreservingNumbers(wireText);
	return {
		body: canonicalBody(parsed) + "\n",
		order: orderReport(parsed).join("\n") + "\n",
	};
}
