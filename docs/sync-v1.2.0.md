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
   has no UI, and the tool has no `confirmProjectAgents` parameter: in pi the model can skip the
   question with it, here only the user approves. The tool's result carries what its subagents
   spent, failed runs included, as `Usage`, which pi's example leaves out; and a run's
   `RunResult.Usage` counts tool results' usage, as pi's `getSessionStats` counts `toolResult`
   usage, so a host's quota sees subagents and codemode's nested calls. Pi's regression `8261-subagent-project-trust` is ported in `TestProjectTrust`. Added to
   `coding` for it: `SessionOptions.AppendSystemPrompt` (pi `--append-system-prompt`) and the
   exported `ParseFrontmatter`.
2. **`models.json`** (pi `core/model-config.ts`, `core/model-registry.ts`): custom models and
   providers for `cmd/pi`. Library users can already build an `ai.Model` directly.
3. **`durable`**: 82 files, 25.2k lines, changing almost daily upstream; ported from the newest
   snapshot when this step starts, after measuring what changed since v1.2.0. Measured on 2026-10-16
   at `d05a73fb`: `--shortstat v1.2.0..HEAD -- packages/durable/src` changed or removed 565 of the
   snapshot's 25,159 lines (2.2%). The port follows that snapshot in stages.

   **Stage 0, chord prerequisites** (at `d05a73fb`): `chord/output` (pi `chord/src/output.ts`:
   `BoundOutput`, `OutputBuffer`, `Progress`) and replicated state in `chord` (pi `state.ts`:
   `NewReplicatedState`, `AttachReplicatedState`), with pi's tests (`output`, `output-skip`,
   `replicated-state`, `replicated-state-delivery`). Go-only: pi's overloaded `replicatedState` is
   two constructors; a listener that goes on in the background returns a channel where pi's returns
   a promise, and a panic is its throw; failures nothing receives go to `chord.ReportError`, which
   logs, where pi rethrows them in a microtask; text arrives as UTF-8, its malformed bytes read as the
   WHATWG decoder reads them (pi's lone surrogates encode the same way), and byte chunks decode with
   a WHATWG decoder rather than Go's `utf8`, which replaces malformed input differently. Context,
   `BACKGROUND_CONTEXT` and the abort-signal helpers are Go's `context`. A change callback that panics
   leaves the state usable, as pi's `finally` does; a test beyond pi's pins it.

   **Stage 1a, records and storage**: `durable` (pi `types.ts`, its records, queries and the
   `Storage` contract; `errors.ts`; `storage/scan.ts`), `durable/storage/memory` (pi
   `storage/memory.ts`) and `durable/durabletest.TestStorage` (pi `testing/storage-conformance.ts`,
   every case, run by `go test`; the memory storage passes it, as pi's `memory-storage.test.ts`
   checks). Go-only: JSON payloads (entry data, task input, checkpoints, results, memos, details) are
   `json.RawMessage`, which keeps every shape and pi's key order through storage; document values are
   chord values. Copies are explicit, not through JSON: Go's JSON replaces malformed UTF-8, which would
   merge two distinct string identities, the Go form of pi's lone-surrogate case; the memory storage's
   address keys quote with `strconv.Quote` for the same reason. `Storage.entry`'s two overloads are
   `Entry` and `VisibleEntry`; `"current"` is the `Current` point, 0. The task runtime, task
   definitions, `Tx` and the Session come with the stages that use them.

   **Stage 1b-1, the Session kernel**: `durable` (pi `documents.ts`, `session/forks.ts`,
   `session/observation.ts`, `session/transaction.ts`, `session/session.ts`, and the Session half of
   `types.ts`), with pi's tests `session-definitions`, `session-documents`, `session-states` and
   `session-watches`. The rest of the session suites (`forks`, `tables`, `checkpoints-migrations`,
   `document-retention`) is stage 1b-2; the retention cases that need the harness wait for stage 4.
   Go-only: a definition is one `Doc` whose semantics `DefineDoc` checks, since Go has no overloads to
   type them, and an address is a `DocRef` built explicitly (`Session()`, `Conversation(id)`,
   `Task(id)`, `.Member(key, seed)`), whose misuse is the error of the call that uses it; values are
   `map[string]any`, nil for a retired incarnation. `Tx` operations block, and a callback may call
   them from several goroutines: each stretch between two Storage calls runs under the
   transaction's mutex, as pi's single thread runs the stretch between two awaits, and settlement
   counts the operations still running, rejects the commit and waits for them. The mutation line is
   a FIFO admitted and joined in one step. Watch and document-state listeners run on goroutines
   started while frames wait, where pi queues a microtask; a watch listener returns an error where
   pi's rejects. `withoutAbortSignal` is `context.WithoutCancel`; eviction timers are
   `time.AfterFunc`. The harness's overrides of `report`, `conversationCreated` and `beforeClose` are
   `SessionHooks`. Typed entry tokens (`defineEntry`) come with the harness. n/a: pi's type-only
   assertions, and `Object.isFrozen`. Tests beyond pi's: definition semantics, misbuilt references
   on every call, and no observer left attached by a cancelled watch acquisition. Where pi's tests
   order calls by their synchronous parts, the Go tests order them explicitly (a held Storage call,
   the transaction's pending count, its seal).

   **Stage 1b-2, the rest of the session suites**: `session-forks`, `session-tables`,
   `session-checkpoints-migrations` and `session-document-retention`. The kernel changes they
   required: a definition hook (`Initial`, `Migrate`, `CheckpointWhen`) that panics rejects the
   operation and leaves the Session usable, as pi's throw does; the cache's clock and retention are
   guarded as pi guards them (a panicking clock counts as `time.Now`, a panicking retention as the
   default); a retention of 100 years or more never expires, pi's `Infinity`, since a `Duration` ends
   after 292 years and a clock can pass that; and a sweep that falls due while a job holds the line
   runs at the end of that job, still on the line, where pi queues it in a microtask, so the next
   call cannot read a document that should have left. The retention cases that need the harness (a
   settings change, the same events with and without eviction) wait for stage 4. n/a: timers that
   cannot be unreferenced, non-numeric retentions (a negative one keeps the default), the suite-wide
   retention override, and pi's `this` in `checkpointWhen`. The test storage stands for a closed
   database without discarding its records, so `reopen` can read them, as pi's does.

   **Stage 2a, JSONL storage**: `durable/storage/jsonl` (pi `storage/jsonl/storage.ts`, and
   `storage/jsonl/node.ts` as `OpenDir`) with pi's `jsonl-storage` suite, and the storage
   conformance suite run on it directly and across a reopen after every commit. The storage writes
   through `jsonl.FS`, the nine operations of pi's env `FileSystem` it uses, with `OSFS` on the
   local file system; stage 3's environment can satisfy it. The line formats are pi's, so the files
   read the same. Deviation: pi's strings are UTF-16 and its files keep a lone surrogate as an
   escape, while a Go string holds one only as invalid UTF-8, which `encoding/json` replaces, so
   reading back would merge distinct identities. The storage therefore rejects a commit holding a
   string, map key or raw payload that is not valid UTF-8, before any I/O and without poisoning
   itself, and refuses a line holding a lone surrogate escape as corruption rather than read it
   lossily; `durabletest.StorageOptions.RejectsInvalidUTF8` makes the conformance suite check that
   rejection in place of the lossless case. Recovery writes kept sidecar lines back as read, where
   pi re-serializes them. n/a: `storage-runtime-boundary` (Go's package graph keeps the portable
   packages free of the local adapter).

   **Stage 2b, SQLite storage**: `durable/storage/sqlite` (pi `storage/sqlite`: `schema.ts`,
   `migrations.ts`, `storage.ts`) and the module `durable/storage/sqlite/modernc` (pi
   `storage/sqlite/node.ts`). The storage runs on a `database/sql` database, which takes the place
   of pi's `SqliteDatabase` facade, so the root module needs no SQLite driver. The file adapter is
   its own module on `modernc.org/sqlite`, a SQLite without cgo (owner ruling 2026-10-16; licenses
   checked against the module sources: BSD-3-Clause and MIT, SQLite public domain, the MPL-2.0
   module of its graph not built; the module's own allowlist test checks the Linux, Windows and
   macOS build graphs, which differ). It configures the connection as pi's adapter does (WAL,
   NORMAL synchronous, a 5 s busy timeout, BEGIN IMMEDIATE, one connection serializing operations,
   the WAL truncated on close). The schema and migrations are pi's byte for byte, and indexed
   strings are encoded as `JSON.stringify` encodes them (checked against Node), so a database
   either side wrote is searched by the same bytes. Ported: the conformance suite directly and
   across a reopen after every commit, `sqlite-storage`, `sqlite-migrations`, and of
   `sqlite-facade` the cases that apply to `database/sql` (serialized transactions, idempotent
   close, reads admitted before close finish, IDs adopted only after a successful commit, a
   document read from one committed state). n/a: the facade's own operation queue, statement
   cache, stale transaction handles and rollback-failure error (`database/sql` owns them), and
   `cloudflare.ts`. Deviation: the UTF-8 rejection of stage 2a, for the same reason; the check
   moves to `durable/internal/utf8check`. The adapter module requires the engine through a
   `replace` to this checkout; a release pins the engine's tag instead.

   **Stage 3a, the environment's files**: `durable/env` (pi `env/index.ts`, `decode.ts`,
   `line-scan.ts`, and the file half of `node.ts` as `Local`) with `durabletest.TestEnv` (pi
   `testing/env-conformance.ts`, its file cases), pi's `env-line-scan` suite, the file and text line
   reader cases of `env-node`, and the reader cases of `env-node-conformance`. `durable/env` is new
   beside `coding.ExecutionEnv`, which ports the env of pi's agent harness that upstream removed
   (`7fd478a2`); moving the coding tools onto `durable/env` is later work. pi's `Result` is
   `(T, error)` with a `*FileError` whose `Code` is pi's; `fileInfo` is `Stat`, since a Go method
   cannot share its result type's name. Decoding uses `internal/jstext.TextDecoder`, which gains
   `IgnoreBOM` for ranges that start mid-file; pi's `StreamDecoder` is its default. `JsonlStorage`
   now writes through `env.FileSystem`, as pi's does, and `jsonl.FS` is gone; the indexed strings
   of `SqliteStorage` use `jstext.Stringify`. Added: a dir reader case whose entries vanish after
   the reader read them, since Go reads a directory lazily and pi's case never reaches that branch.
   n/a: the cases that replace `FileHandle` methods with spies (a read cancelled while pending, a
   failing sync); `listFiles`, which pi's local environment does not implement either; and
   `env-truncate`, which tests `truncate.ts`, a tools utility for stage 5. The shell and watching
   are stage 3b.

   **Stage 3b, commands and watching**: the shell half of `node.ts` (`Local.Exec`: a script through
   pi's bash resolution or an argv run directly, cwd, environment overrides, timeout and abort
   killing the process tree, the output callback decoded per stream, the spill, `Cleanup`), and
   `node-watch.ts` in its polling mode (`Local.Watch`), with the rest of the conformance suite
   (command and watch cases), the shell cases of `env-node` and its watch limit case. pi's shell
   resolution, process groups, tree killing and signal reading move from `coding` to
   `internal/proc`, which both runners use; `coding` keeps thin wrappers under its old names. A
   command is `env.Script` or `env.Argv`, pi's string or array. Output is read from pipes of the
   environment's own, not `StdoutPipe`, which `Wait` closes at exit: as in pi, a descendant holding
   the stdio delays settling until the output is idle for 100 ms. `Local` ignores `ExecOptions.Window`,
   as pi's local environment does. `Local` only polls (owner ruling 2026-10-16): Go's standard
   library has no native watching, and pi itself polls on Windows. A file's identity is its device
   and inode; on Windows, where the file index needs an open handle and an open directory handle
   blocks renaming its parents, it is the creation time, with a directory's modification time.
   Added: checks that a timeout and an abort return promptly (the pi cases pass even without
   killing) and a case for a same-size rewrite that keeps its modification time (the content hash
   pi's polling uses). n/a: `env-node-spill`, since the spill is written synchronously by the loop
   that reads the output, so there is no spill backpressure; and the asynchronous taskkill spawn
   error, which Go reports synchronously.

   **Stage 4a, the harness frame**: `durable/harness` (pi `harness/`): `registry.ts`, `define.ts`,
   `agent.ts`, the settings, `prompt.ts`, `context.ts` without its kept reads, `usage.ts`,
   `provider.ts`, and `harness.ts` with the conversation handle's `agent`, `configure`, `commit`,
   `context`, `entries` and `fork`; the built-in documents `pi.live` and `pi.inbox` are defined,
   their operations come with stage 4c. pi's entry kinds (`entries.ts`) join `durable`. Tests: the
   `harness-registry`, `harness-prompt` and `harness-conversations` suites, and the
   `harness-context` cases that derive a context from committed entries (the extended and kept
   reads come with generation, stage 4d). The harness is a `durable.Session` with the creation
   hook through `SessionOptions`, not a subclass. Tasks are erased to JSON (`Task`, with phases,
   abort and migration) and typed by `DefineTask[I, S, R]`; hooks are a struct of functions per
   task. `AgentChange` fields are `Change[T]`, set or cleared, since Go has no `undefined` beside
   `null`; selections are `Selection[T]`, exact or an edit. Wrappers return an error, and a panic
   in a wrapper or a section is its throw. A tool's extra application fields (pi's generic
   `Tool`) have no Go form; the registry holds `*ToolRegistration`. pi compares the baseline
   system message by object identity; Go messages are values, so each contributed message carries
   its entry, index and whether an edit contributed it. The built-in tasks join the registry with
   their stages, so `NewRegistry()` holds none yet. The process-wide UUIDv7 generator moves from
   `coding` to `internal/uuidv7`, which `pi.provider` uses too. Added: a case for callers, model
   tools and deferred tools in resolution (pi covers them through nested calls and `tool_search`).
   The reopen case uses memory storage that survives close, not node's SQLite. n/a:
   `spec-usage`, a compile check of the TypeScript examples in pi's spec.

   **Stage 4b-1, the task scheduler**: `scheduler.ts` whole (reservation, the step and its
   precedence rules, phase and abort invocations, waits and the wait-for cycle check, the
   ownership tree with abort cascades, held outcomes and finalization, idle waits, inspection) and
   the task runtime, with the Harness's `Resume`, `AbortTask`, `WaitForTask`, `WaitForIdle` and
   `Inspect` and a conversation's `Abort` and `WaitForIdle`. One mutex guards the scheduler; under
   it nothing joins the Session line and no task or host code runs, so a runtime commit checks
   the task under it, runs its change without it, and takes it again to stage the state.
   `queueMicrotask` is a goroutine behind a coalescing flag. The task's signal is its handler's
   context. A failed Storage call inside a commit callback seals the scheduler on that goroutine,
   so the seal sets an atomic flag there and rejects waits on a goroutine of its own, which join
   waits for. A close seals the Session's admission a moment before its listeners run, which pi
   never lets anything see; a scheduler commit refused as closed in between is the close, not a
   failure, so `durable` exports `ErrSessionClosed`. `Task` gains `HasMigration`. `pi.live` cleanup
   of scheduler outcomes and the withdrawal of queued inputs are no-ops until stage 4c; the
   runtime's `context` reads without the kept range until generation, and its `conversation`
   handle comes with submissions. Tests: `harness-tasks`, `harness-cancellation-barrier`, and the
   cases of `harness-inspect`, `harness-lifecycle` and `harness-storage-failure` that need no
   submissions, generation, tools, views or task graph (those come with their stages). pi orders
   line jobs by synchronous calls; where a case depends on that order, the Go case waits for the
   holding commit to reach storage and queues the next jobs with short pauses. One pi case aborts
   a task twice before its abort invocation starts, which a microtask guarantees in pi; the Go case
   holds the abort handler instead. n/a: the cases of rejected promises, thenables and another
   realm's promise.

   **Catch-up to pi `1402973a`, the sleeping state** (`f75b3240`, `ec1be23e`, as of `1402973a`):
   a task commits `{status: "sleeping", checkpoint, until}`, or calls `TaskRuntime.SleepUntil`, and
   holds no invocation until the Harness clock reaches `until`; `TaskRuntime.Sleep` is gone, as
   pi's `sleep` is. `TaskState.Until` is epoch milliseconds, a pointer since 0 is a valid time;
   the API takes `time.Time`. `Options.SetTimer` arms every timer (`durable.SetTimer`, with
   `NativeTimer` and `HostTimers`), `Options.OnWake` replaces the sleep timer, and
   `Harness.Wake` returns `Wake{Active, At}`, a zero `At` for no sleep. A Go timer never keeps the
   process alive, so foreground and background timers differ only in that a host may decline a
   background one; a declined or panicking foreground timer falls back to `time.AfterFunc` and is
   reported once. pi calls `onWake` and the host's `setTimer` from a microtask on its one thread;
   here they are host code, which never runs under the scheduler's mutex, so a delivery computes
   the wake under it and arms the timer or calls `OnWake` under a second mutex that orders
   deliveries. Timers an ended invocation leaves are disarmed once the mutex is released. Without
   `OnWake`, a delivery reads only the sleeping tasks (pi's `wake()` walks every live task): the
   full walk on every kick timed out the 20,000-task ownership chains. `wake()` counts a reconcile
   pass with work as active, as in `1402973a`. SQLite migration 2 rebuilds the task table with
   pi's statements byte for byte. Tests: `harness-sleep` whole, the sleeping case of the storage
   conformance suite and the version 1 to 2 migration; pi's NaN and Infinity sleeps have no
   `time.Time` form, so the Go case tries -1 and a sleeping state without `until`. Added: a graph
   first observed while a task sleeps (pi's graph case observes it before), and a host that
   declines every foreground timer reported once across two separate sleeps (pi's two sleeps share
   one deadline). The restart count, typed reports and the close write-back follow in their own
   changes.

## 5. Bookkeeping

- `docs/UPSTREAM.md` keeps sky-valley's journal up to `5806068c2`; this file continues it to
  `06a46693` (v1.2.0).
- pi's model catalog is not regenerated in this sync.
- The session server stack is removed, as pi removed it in `7472cda0` (2026-10-12, before v1.2.0):
  the `server`, `client` and `protocol` packages and `coding/remotesession.go`, `coding/transcript.go`
  with their tests (about 13k lines of code and 15k of tests). Nothing else in the engine used them;
  `go mod tidy` drops no dependency. A host that needs a transport builds its own.
