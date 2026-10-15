#!/usr/bin/env python3
"""Classify each scenario's pi-vs-Go difference into PASS / KNOWN / FAIL / FIXED.

Invoked by run.sh with the scenario files that are in scope:

    classify.py scenarios/a.json scenarios/b.json ...

It reads out/{pi,go}/<name>.{body.json,order.txt}, compares them structurally,
and checks every difference it finds against known-divergences.json.

A difference is only ever excused when the baseline names the SAME scenario,
the SAME JSON path, and the SAME kind of difference. Anything else fails.

Exit codes (see README.md "Exit-code contract"):
    0  every scenario is PASS or KNOWN, and every baseline entry still fires
    1  at least one FAIL
    3  no FAIL, but at least one baseline entry is stale (FIXED)
"""

import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
BASELINE = os.path.join(HERE, "known-divergences.json")

RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
MAGENTA = "\033[35m"
DIM = "\033[2m"
OFF = "\033[0m"


def color(c, s):
    return f"{c}{s}{OFF}" if sys.stdout.isatty() else s


# --- loading ---------------------------------------------------------------
# Numbers keep their ORIGINAL literal text, so 1024 vs 1024.0 vs 1e3 stays a
# visible difference (same rule as the canonicalizers -- see README).

NUM = "\0num"


def _num(text):
    return (NUM, text)


def loads(text):
    return json.loads(text, parse_int=_num, parse_float=_num)


def load(path):
    with open(path) as f:
        return loads(f.read())


def show(v):
    if isinstance(v, tuple) and v and v[0] == NUM:
        return v[1]
    return json.dumps(v, default=lambda x: x[1] if isinstance(x, tuple) else x)


def sortkeys(v):
    """Deep copy with object keys sorted -- equality here means 'same content,
    possibly different key order'."""
    if isinstance(v, dict):
        return {k: sortkeys(v[k]) for k in sorted(v)}
    if isinstance(v, list):
        return [sortkeys(x) for x in v]
    return v


# --- structural diff -------------------------------------------------------


def diff(pi, go, path="$", out=None):
    """Structural difference list. Each entry: {path, kind, pi, go}."""
    if out is None:
        out = []

    if isinstance(pi, dict) and isinstance(go, dict):
        for k in pi:
            if k not in go:
                out.append(
                    {"path": f"{path}.{k}", "kind": "missing-key", "pi": show(pi[k]), "go": "<absent>"}
                )
        for k in go:
            if k not in pi:
                out.append(
                    {"path": f"{path}.{k}", "kind": "extra-key", "pi": "<absent>", "go": show(go[k])}
                )
        for k in pi:
            if k in go:
                diff(pi[k], go[k], f"{path}.{k}", out)
        return out

    if isinstance(pi, list) and isinstance(go, list):
        if len(pi) != len(go):
            out.append(
                {
                    "path": path,
                    "kind": "array-length",
                    "pi": f"{len(pi)} element(s)",
                    "go": f"{len(go)} element(s)",
                }
            )
            return out
        for i, (a, b) in enumerate(zip(pi, go)):
            diff(a, b, f"{path}[{i}]", out)
        return out

    if type(pi) is not type(go) and not (
        isinstance(pi, tuple) and isinstance(go, tuple)
    ):
        out.append({"path": path, "kind": "type", "pi": show(pi), "go": show(go)})
        return out

    if pi != go:
        out.append({"path": path, "kind": classify_scalar(pi, go), "pi": show(pi), "go": show(go)})
    return out


def classify_scalar(pi, go):
    """A string holding JSON (tool-call `arguments`) that differs ONLY in the
    key order of the embedded object is its own kind. Any value difference
    inside it stays a plain `value` difference."""
    if isinstance(pi, str) and isinstance(go, str):
        try:
            a, b = loads(pi), loads(go)
        except (ValueError, TypeError):
            return "value"
        if isinstance(a, (dict, list)) and isinstance(b, (dict, list)):
            if sortkeys(a) == sortkeys(b):
                return "embedded-json-key-order"
    return "value"


# --- declared order-sensitive paths ----------------------------------------


def order_map(path):
    m = {}
    if not os.path.exists(path):
        return m
    with open(path) as f:
        for line in f:
            line = line.rstrip("\n")
            if "\t" not in line:
                continue
            p, keys = line.split("\t", 1)
            m[p] = keys
    return m


def order_diffs(scenario, name):
    """Key ORDER is only compared where pi's order is observable to the model
    or the provider (scenario "orderSensitivePaths"); a JSON object is
    unordered by spec, so a whole-body order compare would be pure noise."""
    out = []
    pi = order_map(f"out/pi/{name}.order.txt")
    go = order_map(f"out/go/{name}.order.txt")
    for p in scenario.get("orderSensitivePaths", []):
        a, b = pi.get(p), go.get(p)
        if a == b:
            continue
        if a is None or b is None:
            kind = "order-path-missing"
        elif sorted(a.split(",")) == sorted(b.split(",")):
            kind = "key-order"
        else:
            kind = "key-set"
        out.append({"path": p, "kind": kind, "pi": a or "<absent>", "go": b or "<absent>"})
    return out


# --- baseline matching -----------------------------------------------------


def path_matches(pattern, actual):
    """Exact match, with an optional `[*]` wildcard for a single array index.
    Prefer exact indices: a wildcard is looser than it looks."""
    if pattern == actual:
        return True
    if "[*]" not in pattern:
        return False
    pa, aa = pattern.split("[*]"), actual
    for i, part in enumerate(pa):
        if i == 0:
            if not aa.startswith(part):
                return False
            aa = aa[len(part) :]
            continue
        if not aa.startswith("["):
            return False
        close = aa.find("]")
        if close < 0 or not aa[1:close].isdigit():
            return False
        aa = aa[close + 1 :]
        if not aa.startswith(part):
            return False
        aa = aa[len(part) :]
    return aa == ""


def load_baseline():
    if not os.path.exists(BASELINE):
        return []
    with open(BASELINE) as f:
        doc = json.load(f)
    return doc.get("divergences", [])


def find_entry(entries, name, d):
    for e in entries:
        for m in e.get("matches", []):
            if m["scenario"] != name or m["kind"] != d["kind"]:
                continue
            if path_matches(m["path"], d["path"]):
                return e, m
    return None, None


# --- main ------------------------------------------------------------------


def _tracked_in(entry):
    """Where a baseline entry says its divergence is written up.

    Accepts either a plain string ("docs/UPSTREAM.md -> '<section>'") or the
    mapping shape {"ledger": ..., "section": ...}. Both appear in the wild
    because this line only runs when an entry goes FIXED, which first happened
    long after the field was introduced.
    """
    t = entry.get("tracked_in")
    if isinstance(t, dict):
        where = t.get("ledger") or "the ledger"
        section = t.get("section")
        return f"{where} -> {section}" if section else where
    return t or "the ledger"


def main(argv):
    entries = load_baseline()
    fired = set()  # (entry id, match index) triples observed this run
    in_scope = set()

    n_pass = n_known = n_fail = 0
    lines = []

    for f in argv:
        with open(f) as fh:
            scenario = json.load(fh)
        name = scenario["name"]
        in_scope.add(name)

        pib, gob = f"out/pi/{name}.body.json", f"out/go/{name}.body.json"
        if not os.path.exists(pib) or not os.path.exists(gob):
            n_fail += 1
            lines.append(
                color(
                    RED,
                    "FAIL  %s  (missing capture: pi=%s go=%s)"
                    % (name, "y" if os.path.exists(pib) else "n", "y" if os.path.exists(gob) else "n"),
                )
            )
            continue

        ds = diff(load(pib), load(gob)) + order_diffs(scenario, name)

        if not ds:
            n_pass += 1
            lines.append(color(GREEN, f"PASS  {name}"))
            continue

        known, unknown = [], []
        for d in ds:
            entry, m = find_entry(entries, name, d)
            if entry is None:
                unknown.append(d)
            else:
                known.append((d, entry))
                fired.add((entry["id"], m["scenario"], m["path"], m["kind"]))

        if unknown:
            n_fail += 1
            lines.append(
                color(RED, "FAIL  %s  (%s)" % (name, ", ".join(sorted({d["kind"] for d in unknown}))))
            )
            for d in unknown:
                lines.append(f"      {d['kind']} @ {d['path']}")
                lines.append(color(DIM, f"        pi (ground truth): {d['pi']}"))
                lines.append(color(DIM, f"        go (port)        : {d['go']}"))
            for d, entry in known:
                lines.append(color(DIM, f"      (also: known {entry['id']} @ {d['path']})"))
        else:
            n_known += 1
            ids = sorted({e["id"] for _, e in known})
            lines.append(color(YELLOW, "KNOWN %s  (tracked debt: %s)" % (name, ", ".join(ids))))
            for d, entry in known:
                lines.append(color(DIM, f"      {d['kind']} @ {d['path']}"))
                lines.append(color(DIM, f"        pi (ground truth): {d['pi']}"))
                lines.append(color(DIM, f"        go (port)        : {d['go']}"))

    # A baseline entry that no longer fires is STALE: someone fixed the
    # divergence and the entry now silently pre-authorizes a future regression
    # at that exact path. Loud, and it fails the run.
    stale = []
    for e in entries:
        for m in e.get("matches", []):
            if m["scenario"] not in in_scope:
                continue
            if (e["id"], m["scenario"], m["path"], m["kind"]) not in fired:
                stale.append((e, m))

    print()
    for line in lines:
        print(line)

    if stale:
        print()
        for e, m in stale:
            print(color(MAGENTA, f"FIXED {m['scenario']}  ({e['id']} @ {m['path']}, {m['kind']})"))
            print(
                color(
                    MAGENTA,
                    "      This divergence is GONE. The baseline entry is now stale and "
                    "re-permits a real regression at that path.",
                )
            )
            print(
                color(
                    MAGENTA,
                    f"      RETIRE it: delete this match from known-divergences.json "
                    f"(entry '{e['id']}'), and close it out in {_tracked_in(e)}.",
                )
            )

    total = n_pass + n_known + n_fail
    print()
    summary = f"{n_pass} PASS, {n_known} KNOWN (tracked debt), {n_fail} FAIL"
    if stale:
        summary += f", {len(stale)} FIXED (stale baseline)"
    summary += f"  [{total} scenario(s)]"

    if n_fail:
        print(color(RED, summary))
        print(
            color(
                RED,
                "pi is ground truth; a difference here is a port bug until proven otherwise.",
            )
        )
        return 1
    if stale:
        print(color(MAGENTA, summary))
        print(color(MAGENTA, "No new divergence, but the baseline is stale — retire the FIXED entries above."))
        return 3
    print(color(GREEN if not n_known else YELLOW, summary))
    if n_known:
        print(
            color(
                YELLOW,
                "KNOWN is not clean: it is accepted, tracked debt. See known-divergences.json.",
            )
        )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
