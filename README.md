# bismarck-engine

The agent engine of **Bismarck**: a Go implementation of [pi](https://github.com/earendil-works/pi)
(Mario Zechner, MIT) — pi's unified LLM API, agent loop and coding agent, with pi's extensions for
MCP and agents — embedded as a library, with no Node.js runtime.

## Origin

- The code started on 2026-10-15 from [sky-valley/pi](https://github.com/sky-valley/pi), a Go port
  of pi (MIT), at commit `1067024e` (pi 0.99.2, pi source `5806068c2`), tagged `sky-valley-1067024`.
  Its history is kept as the record of where the code came from; its copyright stands in
  [LICENSE](LICENSE).
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
| [`coding`](coding/) | `@earendil-works/pi-coding-agent` | Built-in tools (`read`, `write`, `edit`, `bash`, `powershell`, `ls`, `find`, `grep`), the system prompt builder (AGENTS.md/CLAUDE.md context files, Agent Skills), the session runner, compaction, JSONL session persistence. |
| [`server`](server/), [`client`](client/), [`protocol`](protocol/) | pi-server, pi-client (removed upstream) | Transport-neutral session server and client over a CBOR wire format. |
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

CI runs `gofmt`, `go vet` and `go test` on Linux and Windows. The module's dependencies are
limited to the standard library and `golang.org/x/{image,text}`; `internal/policy` enforces it.

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

The built-in providers register themselves when `coding` is imported. Using `ai` alone, blank-import
them: `import _ "github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"`.

## Differences from pi

- Provider attribution headers (`HTTP-Referer: https://pi.dev`, `X-OpenRouter-Title`,
  `x-opencode-*`) are sent only when `PI_TELEMETRY` is truthy; pi sends them by default.
- The grep tool reports model-facing paths with forward slashes on every OS.
- Project-local resources under `<cwd>/.pi` load only with `SessionOptions.TrustProject`, pi's
  answer for a host with no UI to ask the user.
- Not ported: the interactive TUI and its host layer, the JavaScript extensions runtime (pi's
  built-in extensions are wired in Go instead), and the wire APIs that would need a third-party
  dependency (AWS Bedrock SigV4 and eventstream, OpenAI Codex WebSocket with zstd).

## License

MIT. See [LICENSE](LICENSE): this project's notice, sky-valley's and pi's.
