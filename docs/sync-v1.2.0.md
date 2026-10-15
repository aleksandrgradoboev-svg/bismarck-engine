# Sync with pi: 0.99.2 → v1.2.0

Range: pi `5806068c2..v1.2.0` (`06a46693`, 2026-10-13), 240 non-merge commits. pi has moved on
since; the next sync starts from `06a46693`.

Commits were triaged by the paths they touch and their subjects; each one is decided again from
its diff when it is ported.

## 1. Triage

| Group | Commits | Decision |
|---|---|---|
| Core in scope (`agent`, `ai`, `coding`) | ≈17 | port one by one (section 2) |
| MCP (`packages/mcp`, `extensions/mcp`) | 31 | the engine has no MCP yet: port the v1.2.0 snapshot, not the history (section 3) |
| Releases and "Unreleased" (v1.0.0 … v1.2.0) | 12 | version bumps only — no |
| Changelog audits, documentation | ≈20 | no |
| TUI, CLI, clipboard, terminals, full-screen mode | ≈57 | no: the engine is embedded, the host draws the UI |
| OAuth and catalogs of other providers | ≈25 | no: out of scope |
| Classifiers and image generation | ≈5 | no: the engine has no such subsystem |
| `codemode` | 25 | port as a snapshot, together with MCP (section 3) |
| `durable` (agent harness, subagents) | 68 | port as a snapshot, last (section 4) |
| Removal of `server`/`client`/`protocol` in pi | 15 | no: pi removed them; here they stay, without an upstream |
| `env`, `session-backends/sqlite-node`, examples, lock files | — | no |

## 2. Core

| pi commit | Change | Status |
|---|---|---|
| `27075fe0` | estimate request context at 3.5 characters per token | ported |
| `3874b3e9`, `5b6c792b`, `8b5708db`, `7bd01fb9` | retry "model at capacity", HTTP/2 pending stream cancellation, "server busy", "upstream provider returned an error" | ported |
| `15e7bdc1` | omit thinking from compaction summaries | ported |
| `c30840c2` | keep hidden tools out of prompt rules and the skills hint | ported (core half; the codemode half lands with codemode) |
| `f1b2e77f` | load a symlinked AGENTS.md in nested git worktrees | ported |
| `76dfb88f` | per-thinking-level sampling parameters | ported |
| `bc2d8dc1` | drop mismatched item ids when replaying grammar tool calls | ported |
| `36a686ee`, `f284a246` | record response and tool execution times | ported: `AssistantMessage.DurationMs` (last key) timed by the assistant event stream through hooks on `EventStream`, since Go's stream is the generic type and has no `push`/`end` to override; `ToolResultMessage.DurationMs` between `isError` and `timestamp`, `AgentToolCallOutcome.DurationMs`, and `tool_execution_end`'s, nested calls included. n/a: `lazy.ts` (providers resolve synchronously here), `streamProxy` and the test mocks (no hand-written assistant streams), `durable`, rendering. The response-model capture predates the field, so its comparison drops `durationMs` like `timestamp` and checks that it is set |
| `fe11328b` | per-message prompt-cache usage in the faux provider | ported |
| `ddaa0a03`, `04b97ef0` | `+name`/`-name` and patterns in the tool list; MCP tools with `--tools` | with MCP |
| `e6fb2581` | retry transient Windows EPERM on the auth lock | n/a: no file-based auth storage |
| `a447ba11` | `before_agent_start` for runs started by custom messages | n/a: JavaScript extensions runtime |
| `6a51a39d` | expand `~` in resource setting patterns | n/a: no package manager |
| `7fd478a2` | remove the experimental harness from pi-agent-core | n/a: never ported |

Each port is its own commit, `port(area): … (upstream <hash>)`, with `go test ./...` green after it.

Not from pi but needed by hosts that embed the engine, and modelled on pi's resource loader:
`SessionOptions.NoContextFiles`, `NoSkills` and `SkillPaths` (pi `noContextFiles`, `noSkills`,
`additionalSkillPaths`).

## 3. MCP: the v1.2.0 snapshot

| pi part | Size (without tests) | Taken |
|---|---|---|
| `packages/mcp`: client, transports, OAuth | 18 files, 3.3k lines | all |
| `extensions/mcp`: built-in extension — server manager, `mcp.json`, tool registration | 11 files, 4.3k lines | all but the `/mcp` screen (`ui.ts`) and the `pi mcp …` commands (`cli.ts`), which are TUI |
| `extensions/tool-search`: search among many tools | 2 files, 0.3k lines | all |
| `packages/codemode`: QuickJS sandbox, host protocol, tool declarations | 15 files, 2.7k lines | all but the Cloudflare and remote runtimes and the Node worker launcher |
| `extensions/codemode`: the `execute` tool | 6 files, 1.3k lines | all but the TUI renderer |

`codemode` moved here from section 4: in pi the MCP extension exposes servers through codemode by
default (`exposure: "codemode"`), so MCP without it only works with `exposure: "direct"`.

The sandbox runs `quickjs-wasi` 3.6.2 (MIT; QuickJS-ng inside, MIT) on `tetratelabs/wazero`
(Apache-2.0, pure Go): the first external dependencies of the engine. The wasm is embedded; its C
layer exports `qjs_*` functions and imports one non-WASI function, `env.host_call`, so the
package's JavaScript glue is rewritten in Go. The version is pinned exactly, because pi drives
internal exports (`qjs_is_job_pending`, `qjs_execute_pending_job`).

pi wires extensions through its JavaScript extensions runtime (`core/extensions`), which the
engine does not have; built-in extensions are wired in Go when a session is created, as the
sky-valley port planned (`docs/UPSTREAM.md`, ruling of 2026-09-29).

Order inside MCP: protocol and client (done) → stdio and Streamable HTTP transports (done) →
codemode dependencies → QuickJS glue in Go → `packages/codemode` → the extension (config, server
manager, tools, resources) → tool-search and the codemode extension → OAuth.

## 4. After MCP

1. **Subagents.** In pi v1.2.0 they are an example extension (`examples/extensions/subagent`,
   ≈1.2k lines): agents defined in `.md` files, each subagent a separate `pi` process with its own
   context. Here a subagent is a separate engine session in the same process.
   **Ported** as `coding/subagentext`, from the example at `91130942` (one change since v1.2.0,
   `91014991`, which only touches its terminal rendering): agent discovery, the three modes,
   pi's limits and messages, `details` in pi's shape. Not ported: `renderCall`/`renderResult`
   (terminal rendering) and the sample agents and workflow prompts. Differences: the host builds
   each subagent's session (`Options.NewSession`) and resolves agent models
   (`Options.ResolveModel`); the agent's `tools` narrow the session's active tools; without
   `Options.Confirm` an untrusted project's agents are refused, where pi runs them unasked when it
   has no UI, and `confirmProjectAgents: false` from the model only skips a question that could be
   asked. Pi's regression `8261-subagent-project-trust` is ported in `TestProjectTrust`. Added to
   `coding` for it: `SessionOptions.AppendSystemPrompt` (pi `--append-system-prompt`) and the
   exported `ParseFrontmatter`.
2. **`models.json`** (pi `core/model-config.ts`, `core/model-registry.ts`): custom models and
   providers for `cmd/pi`. Library users can already build an `ai.Model` directly.
3. **`durable`**: 82 files, 25.2k lines, changing almost daily upstream; ported from the newest
   snapshot when this step starts, after measuring what changed since v1.2.0.

## 5. Bookkeeping

- `docs/UPSTREAM.md` keeps sky-valley's journal up to `5806068c2`; this file continues it to
  `06a46693` (v1.2.0).
- pi's model catalog is not regenerated in this sync.
