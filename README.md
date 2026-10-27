<p align="center"><img src="docs/img/portrait.png" alt="Bismarck" width="220"></p>

# bismarck-engine

The agent engine of **Bismarck**: a Go implementation of [pi](https://github.com/earendil-works/pi)
(Mario Zechner, MIT) — pi's unified LLM API, agent loop and coding agent, with pi's extensions for
MCP and agents — embedded as a library, with no Node.js runtime.

## Why

- **An agent inside your Go program.** One `go get`, no Node.js process beside it, no external
  workflow server: the agent loop, the providers, the coding tools and MCP are packages you import.
- **Agents that survive a restart** (`durable`, experimental). Conversations, tasks and tool calls
  are committed to memory, JSONL or SQLite storage (no cgo); after a crash the harness resumes
  where it stopped. A tool interrupted mid-call is retried only if it is marked safe to replay,
  and otherwise reported as "interrupted and may have partially run" — the engine does not guess.
- **MCP and code mode.** MCP servers from a shared `mcp.json`, and a QuickJS sandbox in which the
  model writes JavaScript that calls the tools.
- **pi as the reference.** Behaviour follows pi commit by commit, differences are written down, and
  every test of a new mechanism is checked by turning the mechanism off (see
  [CONTRIBUTING.md](CONTRIBUTING.md)).

Contributions, test runs on your platform and bug reports are welcome — start with
[CONTRIBUTING.md](CONTRIBUTING.md) or an issue labelled `good first issue`.

## Origin

- The code started on 2026-10-15 from [sky-valley/pi](https://github.com/sky-valley/pi), a Go port
  of pi (MIT), at commit `1067024e7cce0028602e638bbf6676c6d2883cda` (pi 0.99.2, pi source
  `5806068c2`); its copyright stands in [LICENSE](LICENSE). The history up to v0.1.0 is squashed
  into one commit; [docs/UPSTREAM.md](docs/UPSTREAM.md) records the base and the sync ledger.
- This is an independent project, not a fork. **The source of changes is pi itself.** New pi
  commits are ported here directly; the sky-valley port is a reference while it is maintained,
  never a dependency.
- Scope tracked from pi: `agent` (pi-agent-core), `ai` (core and the OpenAI-compatible provider),
  `coding` (sessions, tools, compaction), and pi's extensions for MCP and agents.
- pi removed its `server`, `client` and `protocol` packages on 2026-10-12 (`7472cda0`); the Go
  versions here no longer have an upstream.
- Each sync is planned in `docs/sync-<version>.md`; commits that port pi say so:
  `port(area): … (upstream <pi commit>)`.

## Layout

| Package | Mirrors (pi) | What it is |
|---|---|---|
| [`ai`](ai/) | `@earendil-works/pi-ai` | Unified LLM API: message/content/tool/model types, the channel-based `EventStream`, JSON-Schema tool validation, model catalog and cost, provider registry. |
| [`ai/providers`](ai/providers/) | `pi-ai` providers | Providers over `net/http` SSE: faux (deterministic test double), Anthropic Messages, OpenAI Chat Completions, OpenAI Responses, Google Gemini. |
| [`agent`](agent/) | `@earendil-works/pi-agent-core` | The agent runtime: `AgentLoop`, the stateful `Agent` with tool calling, `BeforeToolCall`/`AfterToolCall` hooks, steering and follow-up queues, sequential and parallel tool execution. |
| [`coding`](coding/) | `@earendil-works/pi-coding-agent` | Built-in tools (`read`, `write`, `edit`, `bash`, `powershell`, `ls`, `find`, `grep`), the system prompt builder (AGENTS.md/CLAUDE.md context files, Agent Skills), the session runner with its tool registry (exposures, loadout hooks, nested tool calls), compaction, JSONL session persistence. |
| [`mcp`](mcp/) | `@earendil-works/pi-mcp` | MCP client: JSON-RPC, stdio and Streamable HTTP transports, an in-memory transport for tests. |
| [`durable`](durable/), [`durable/storage/memory`](durable/storage/memory/), [`durable/storage/jsonl`](durable/storage/jsonl/), [`durable/storage/sqlite`](durable/storage/sqlite/) (+ the [`modernc`](durable/storage/sqlite/modernc/) adapter module), [`durable/env`](durable/env/), [`durable/harness`](durable/harness/), [`durable/tools`](durable/tools/), [`durable/images`](durable/images/), [`durable/toolsearch`](durable/toolsearch/), [`durable/codemode`](durable/codemode/), [`durable/durabletest`](durable/durabletest/) | `@earendil-works/pi-durable` (**experimental**) | Durable sessions: records, transactions, documents and forks over the atomic `Storage` contract; memory, JSONL and SQLite storage; the execution environment (files, commands, watching); the task harness — scheduler, recovery, submissions and inbox, generation with retry, deferred polling and compaction, tool rounds and nested tool calls, the conversation view and agent events, telemetry; coding tools on the execution environment, images, `tool_search` and code mode. Ported up to pi `73f8de80` except code mode's `models.*` and the browser IndexedDB storage. Nothing else in the engine uses these packages yet, and their API may still change. |
| [`chord`](chord/), [`chord/delta`](chord/delta/), [`chord/output`](chord/output/) | `@earendil-works/chord` | Strict JSON, service identities and wire types, replicated state; JSON change tracking and operations; bounded output streams and paced progress commits. |
| [`coding/mcpext`](coding/mcpext/) | `pi-coding-agent` extensions/mcp | MCP servers from `mcp.json`, their tools with exposures (`direct`, `codemode`, `deferred`, `hidden`), resource tools, and `Bind`, which wires a server manager into a session. |
| [`codemode`](codemode/) | `@earendil-works/pi-codemode` | The script sandbox: QuickJS (quickjs-ng, the `quickjs-wasi` build pi uses) under [wazero](https://github.com/tetratelabs/wazero), TypeScript declarations of the callable tools. |
| [`coding/codemodetool`](coding/codemodetool/) | `pi-coding-agent` extensions/codemode | The `codemode` tool: the model writes JavaScript that calls other tools; its description lists them within a token budget. |
| [`coding/toolsearch`](coding/toolsearch/) | `pi-coding-agent` extensions/tool-search | The `tool_search` tool and the BM25 ranker scripts' `searchTools()` shares. |
| [`coding/subagentext`](coding/subagentext/) | `pi-coding-agent` examples/extensions/subagent | The `subagent` tool: tasks delegated to agents defined in `.md` files, one at a time, in parallel or as a chain, each in a session of its own built by the host. |
| [`cmd/pi`](cmd/pi/) | `pi` CLI | Print mode, interactive REPL, session resume, `pi models`, `pi sessions`. |

[`difftest/`](difftest/) is the differential harness: for identical inputs it captures the HTTP
request body that real pi builds and the one this code builds, and diffs them. pi is ground
truth. It is a separate Go module and is not part of the library.

## Build and test

```bash
go build ./...
go test ./...          # live-API tests skip without keys
go build -o pi ./cmd/pi
```

CI runs `gofmt`, `go vet` and `go test` on Linux and Windows, `durable/codemode` and
`durable/harness` again with one processor, and the race detector over the concurrent parts
(durable, codemode, MCP, subagents, nested tool calls, the tool registry, telemetry) on Linux. The
module's dependencies are the standard library, `golang.org/x/{image,text}` and
`github.com/tetratelabs/wazero` (Apache-2.0, pure Go, for codemode); `internal/policy` enforces the
list. The SQLite driver for `durable` is a separate module,
[`durable/storage/sqlite/modernc`](durable/storage/sqlite/modernc/) (`modernc.org/sqlite`, no cgo).
How changes are checked is in [CONTRIBUTING.md](CONTRIBUTING.md).

## Using the library

```go
import (
    "github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
    "github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

// An OpenAI-compatible endpoint (a LiteLLM gateway, for example) needs no catalog entry.
model := &ai.Model{
    ID: "my-model", Api: ai.APIOpenAICompletions, Provider: "litellm",
    BaseURL: "https://gateway.example/v1", ContextWindow: 128000, MaxTokens: 8000,
}
sess := coding.NewSession(coding.SessionOptions{
    Model:          model,
    APIKey:         os.Getenv("LLM_API_KEY"),
    SystemPrompt:   "…",
    NoTools:        coding.NoToolsBuiltin, // or ToolNames: []string{"read"}
    CustomTools:    []agent.AgentTool{myTool},
    NoContextFiles: true,                  // a server must not pick up AGENTS.md from its directories
    NoSkills:       true,                  // nor skills from the home directory
    SkillPaths:     []string{"/srv/skills"},
    Compaction:     &coding.DefaultCompactionSettings,
})
res, err := sess.Run(ctx, "question")
// res.Text, res.ToolCalls, res.Messages, res.StopReason, res.Usage
```

`sess.Subscribe` streams events; mid-session `SetModel`, `SetThinkingLevel`, `Steer`,
`FollowUp`, `Continue` and `Abort` are available, and sessions persist and resume through
`coding.StartSession` / `coding.LoadSessionTree`. A runnable example is in
[`examples/sdk`](examples/sdk/main.go).

MCP servers come from an `mcp.json` in the `mcpServers` format other MCP clients share. Register
the codemode and tool_search tools first — the servers' tools are reached through codemode by
default — then bind a server manager to the session:

```go
loaded := mcpext.LoadConfig("mcp.json", cwd, false)
manager := mcpext.NewManager(mcpext.ManagerOptions{Config: loaded, Cwd: cwd})
sess.RegisterTools(
    codemodetool.Definition(codemodetool.Options{Session: sess, Store: &codemodetool.Store{}}),
    toolsearch.Definition(sess),
)
unbind := mcpext.Bind(sess, manager, mcpext.BindOptions{Warn: log.Println})
manager.Start()
defer func() { unbind(); manager.Close() }()
```

`Bind` registers the servers' tools as they connect, activates codemode or tool_search when the
servers' exposures need them, lists the servers in the `mcp_servers` prompt section, and makes calls
wait for the servers they need.

The built-in providers register themselves when `coding` is imported. Using `ai` alone, blank-import
them: `import _ "github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"`.

## Differences from pi

- Provider attribution headers (`HTTP-Referer: https://pi.dev`, `X-OpenRouter-Title`,
  `x-opencode-*`) are sent only when `PI_TELEMETRY` is truthy; pi sends them by default.
- The grep tool reports model-facing paths with forward slashes on every OS.
- Project-local resources under `<cwd>/.pi` load only with `SessionOptions.TrustProject`, pi's
  answer for a host with no UI to ask the user.
- MCP calls wait for their servers in a session tool gate that runs before the tool executes
  (`Session.AddToolGate`), not in a `tool_call` hook: hooks run under the agent loop's tool lock,
  and a wait there would stall the rest of a parallel batch.
- codemode's `store()` keeps its values in memory for the session; pi writes them to the session
  file per branch.
- codemode can carry tool notices (`codemodetool.Options.Notices`, not in pi, off by default): the
  host names top-level fields of nested call results — a warning, a staleness or truncation mark —
  and their values are appended to the script's output when the script did not print them or
  truncation cut them out.
- Not ported: the interactive TUI and its host layer, the JavaScript extensions runtime (pi's
  built-in extensions are wired in Go instead), codemode's `models.*` script globals (classifiers
  and image generation), MCP OAuth sign-in, and the wire APIs that would need a further
  third-party dependency (AWS Bedrock SigV4 and eventstream, OpenAI Codex WebSocket with zstd).

## License

MIT. See [LICENSE](LICENSE): this project's notice, sky-valley's and pi's.
