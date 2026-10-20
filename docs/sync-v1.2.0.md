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

   **Catch-up to pi `1402973a`, typed reports** (`91b33cab`, with the report sources of
   `a30c2a81`, `440a5fcd`, `5ba1cbaf`): `OnReport` and `SessionHooks.Report` take a
   `durable.ReportInfo`. pi's discriminated union is one flat struct whose `Type` decides which of
   `Task`, `Extension`, `ConversationID`, `Reason` and `Callback` are set; its JSON keys are pi's.
   Task reports name the extension that installed the definition, captured at reservation; a
   phase's failure is reported as `task_faulted` once durable. `TaskRuntime.Report` takes an
   optional `ReportFrom` for pi's `from`. `agentHooks` pairs handlers with their extension, and
   `renderSections` hands the failing section to its caller, as in pi; `sectionExtension` and
   `toolExtension` come with generation and tools, which use them. `task_outdated` carries its
   cause in `Reason`. Tests: `harness-reports` without its generation case, which comes with
   generation; the typed checks pi added to two handover cases and to agent hooks. Added: a
   task's report sources and a failing hook's extension (pi covers them through tools and
   generation), and both the Session's and the scheduler's timer failures in the timer case,
   whose first report alone hid the scheduler's. n/a: an installer among several extensions that
   list one task, which the registry refuses.

   **Catch-up to pi `1402973a`, the restart guard** (`a30c2a81`, `440a5fcd`, `5ba1cbaf`): open
   counts every surviving `running` task as cut off (`TaskRecord.Restarts`, the last field, as pi
   appends the key when it first counts one); a task at `HarnessSettings.RestartLimit` (default 5,
   negative never parks, pi's `Infinity`) is parked, blocked with `restart_limit` in `Inspect`,
   the task graph and `Wake`, until `Harness.RetryTask`. A changed state starts the count over, the
   same checkpoint committed again does not; an abort mark starts it over too, and an abort handler
   cut off the limit's times ends `faulted` with detail `{"reason":"restart_limit"}`. Close writes
   the tasks it stopped back as `pending`, so only a crash counts: pi's protected `writeAtClose` is
   passed to `SessionHooks.BeforeClose`, which alone may call it. The task graph stays in
   `durable`; `SessionHooks.TaskGraphBlocking` gives it pi's `blocked` and `subscribeBlocked`,
   and it derives reasons again on the line when they change without a commit. The restart limit
   is read under the scheduler's mutex, as the registry snapshot is; a panicking settings callback
   is reported as `host_error` and the default applies. Go's open delivers its first wake
   synchronously before the recovery pass: pi's microtask runs before that pass's commit can fail,
   while here the pass and a delivery goroutine would race, and a host told nothing arms no
   recovery wakeup. Tests: `harness-blocked` whole, the close cases pi changed in `harness-tasks`.
   Added, where pi's cases pass without the mechanism: `RetryTask` on a paused Harness (pi's case
   has scheduling running), a registry change reaching the graph without a commit (pi's case runs
   the task, whose commit updates it), a lowered limit parking a task at the next pass, and an
   abort-requested task without its definition shown as not blocked. Not covered by a test: the
   synchronous first wake, whose race the scheduler decides, and `writeAtClose`'s failed-Session
   check, which the Storage guard repeats.

   **Durable slice 1, submissions** (snapshot `1402973a`; `submissions.ts`, `inbox.ts`, the run
   control of `live.ts`, the core of `generation.ts`, `usage.ts`'s `recordUsage`, `provider.ts`'s
   `ensureProviderSessionId`, `json.ts`'s `assignJson`): `Conversation.Submit`, `Harness.Submission`
   and `Harness.AbortSubmission`, with one `SubmissionDraft` whose `Type` picks `Input` or `Write`,
   a `*Submission` handle and `*ConversationBusyError`. `UserInput` keeps pi's string form of
   content (`content: "hi"`). The inbox comes whole, since admission uses nearly all of it; an
   inbox write keeps pi's entry draft shape, `head` as `"self"` or an ID and present-but-empty
   `model`/`edits` counting for display-only. The scheduler's outcome cleanup and input
   withdrawal hooks are no longer empty. `GenerationTask` joins the built-in tasks in `init`, as
   it refers to itself through the run it continues; its checkpoint is pi's union as one struct
   with `phase`. Ported: prepare (system entries, cutoff), request (partial throttle, provider
   session ID, `beforeRequest`), classification, the final answer with `onYield` and the final
   boundary, `no_model` and `model_error` failures with `afterFailedRun`, abort, and the cleanup of
   a faulted or orphaned run. The retry, poll and tools phases, deferred responses, threshold and
   overflow compaction fault with "not ported yet" and come with slices 2 and 3; pi-ai's
   `isContextOverflow` is not in `ai` yet, so until compaction an overflow is an ordinary model
   error, as in pi with compaction disabled. `Settings` gains `Stream` (pi's
   `ConversationStreamOptions`, `deferred` as `StreamDeferral`) and `AfterFailedRun`.
   `providers.FauxResponseStep` takes the request's `ctx` first, as pi's step gets
   `options.signal`; a step that returns nil fails the stream with the cause, as a rejected step
   does, and a deferred redemption runs with the fetch's `ctx`. Tests: `harness-submissions`
   whole; the reopen case uses JSONL, as other reopen cases do; the typed-entry case keeps what
   applies to a Go `EntryKind`, which has no data type (reading narrowed by kind is n/a). Added:
   the answer of the scheduling case is checked to be the generation's assistant entry. Waits that
   pi starts synchronously are marked explicitly: registered waits, close begun. Not covered here:
   the display-only fast path (its outcome is reached the long way too), withdrawal of a queued
   submission, the outcome cleanup, and `Wait` enabling scheduling, which `Submit` does as well;
   the inbox, generation and failed-run slices cover them.

   **Durable slice 1, inbox tests** (snapshot `1402973a`): `harness-failed-run-inbox` whole and
   `harness-inbox` without the cases that run tools — steers at the postTools boundary, a reset
   after tools, a handoff from a tool (two cases), `steeringMode: "all"`, a terminating round, and
   the two usage cases with tool usage (one also retries) — which come with slice 2, and "usage
   across reopen", whose partial converted after reopen is generation recovery (slice 1 (d)).
   Added for them: `Conversation.Reset(ctx, handoff *string)`, pi's `reset`; nil is no handoff
   and an empty string an empty message, as pi tells `undefined` from `""`. Fixed: the final
   answer read the queue modes before its commit, not on the Session line as pi does, so a mode
   changed while the boundary's commit waited behind another did not apply (caught by "reads
   queue modes when the final boundary's commit runs on the Session line"). The reopen case uses
   JSONL, as other reopen cases do. Adapted: a Go map has no prototype, so the prototype-key usage
   case keeps that each name has its own exact total, and key order is n/a. Unlike pi, where a
   submission wait resolves in a microtask after every commit listener, the Session wakes the
   wait first (`ObserveCommits` runs before host listeners), so the cases that read a host
   listener's record after a wait wait for the record itself. Added beyond pi: a head write placed
   earlier in the same boundary advances the active range, so a later write behind it is stale —
   no case of pi's checks it. Sabotage: 25 of 30 mutations caught; the order the boundary removes
   items in is not a mechanism, as Chord normalizes the adopted splices. Expected and still
   uncovered: the postTools boundary (slice 2), data-only writes while busy
   (`harness-input-entries`) and the outcome cleanup (slice 1 (d)).

   **Telemetry contract** (snapshot moves to `d2e7d8b7`, 2026-10-19; pi `48909d98` and the
   contract part of `2ec55d80`): `telemetry` gains propagation (`Carrier`, `Propagation`), span
   links (`SpanLink`, `SpanOptions.Links`, `Span.AddLink`), metrics (`Counter`, `UpDownCounter`,
   `Histogram` with an exemplar context, `Metrics`, `NoopMetrics`), schema data
   (`SchemaDefinition` and its parts, marshalling as pi's objects) and `NewTypedMetrics` for
   `createTypedMetrics`. pi's optional `metrics` and `propagation` properties are the optional
   interfaces `MetricsCapable` and `PropagationCapable`, read through `MetricsOf` and
   `PropagationOf`, so an adapter without them still implements `Context`. `NoopContext` is now
   `NoopSpan`, carrying the no-op capabilities. pi's chord `Context.telemetry` ("always a span")
   is a `context.Context` value: `WithSpan`, `SpanFrom` (`NoopSpan` when unset) and `TraceIn`.
   `InMemoryContext` records carriers (`pi-memory-scope`/`pi-memory-span`), parent carriers,
   links and measurements (`Measurements`, pi's `getMetrics`); a child of a settled span runs on
   an inert span that keeps the adapter's capabilities. The adapter conformance suite is
   `telemetry/telemetrytest` (`TestAdapter`). Not ported: the TypeScript inference over schemas
   and `createTypedSpanStarter` (compile-time typing), and every case built on unreadable
   payloads (proxies that throw on read; a Go value cannot). Adapted: the "another Node process"
   propagation case feeds the JSON carrier to a second recorder, as what it checks is that no
   recorder state is shared; a returned error and a panic stand in for pi's rejection and throw.
   Strengthened: the detached-carrier case compares with a copy taken before the mutation — in
   pi's form a carrier shared between injections compares equal to itself. Added beyond pi: the
   local parent ID of an extracted carrier of the recorder's own span, which no case of pi's
   checks. Sabotage 30 of 30. CI runs `./telemetry/...` under the race detector.

   **Durable telemetry** (snapshot `d2e7d8b7`, the durable part of `2ec55d80`): `harness.Options.Telemetry`
   and `TelemetrySchema` (pi `DURABLE_TELEMETRY_SCHEMA`, as data). Task and submission records
   carry `Trace` (and a task `Links`) inside their JSON, so Storage needs no migration; JSONL keeps
   them across a reopen. `TaskOptions` gains `Trace`, `TraceRoot` (pi's `"root"`) and `Links`; a
   nil trace inherits the carrier of the commit's span. pi's internal `submissionIds` is
   `TaskOptions.TraceSubmissions`: run creation resolves the inputs' traces inside `CreateTask`,
   from the commit's own submissions or Storage, past the table-read rule, as pi does inside
   `createTask` — outside the Tx the reads are refused after the commit's first write. The Session
   emits `pi.durable.entry` and `pi.durable.submission` on the commit's span after a successful
   commit, listened to or not; `SessionHooks.Report` takes the operation's context, and
   `Session.FailIn` reports a failure with one. `pi.durable.submit` wraps a public admission (an
   input's span a root of the adapter linked to the caller, a write's a child of the caller's;
   the admission result, the submission's trace, a steer's link to the run it steers);
   `pi.durable.phase` wraps each phase handler with the decision commit after it, its parent the
   task's trace, with the first invocation's links, `resumed`, the outcome, a `task_<status>`
   error status capped at 500 runes (pi: UTF-16 units) and the duration. The current phase span is
   held apart from the invocation's context, which other goroutines read, and handlers get a
   derived context; measurements decided under the scheduler's mutex are recorded after it is
   released, as no host code runs under it. Reports go to the report counter by type and, with
   their operation's context, to its span as fixed-field events. The provider request gets the
   phase span. Ported: 17 of pi's 24 cases (the three statuses of the fixed-category case
   counted). Waiting: the content-free case with nested tool parents, the steer link and the input
   submitted inside a tool's turn (slice 2), the two compaction cases (slice 3), the codemode
   replay (stage 5). n/a: the async listener case, as a Go commit listener returns no promise.
   Adapted: the trace-inheritance case matches phases by task, as its two tasks run concurrently;
   the admission case checks Storage reads and results, as a span's propagation is the adapter's
   own and cannot be spied on through the root. Added: traces surviving a reopened JSONL Storage.
   Added beyond pi, for mechanisms no case of pi's checks: a traceless first input starting a
   root run, the placed or queued result behind a boundary, a duplicate's unanswered result
   reported done, no settlement event for a queued submission, creation links on the first
   invocation only, and one duration per phase without IDs (pi checks this in its content-free
   case, which waits for slice 2). Sabotage 38 of 45. Expected or equivalent: the steer link
   (slice 2), the provider request span (pi-ai's request telemetry is not ported), events only
   with listeners (a Harness always listens), copying an immutable trace in `Clone`, the explicit
   ok of an aborted phase (its callback never fails). Unchecked, as in pi: the context of a
   Session failure inside a step and of an outdated report.

   **Durable slice 1, live and view** (snapshot `d2e7d8b7`; `view.ts`): the conversation view
   (spec §9.3) — `Conversation.ViewState` (a `durable.DocumentState`) and `Conversation.Watch` (a
   `durable.DocumentWatch`), whose value is the chord object `{conversation, entries, docs}`, read
   with `DecodeConversationView`. One mount per conversation, built on the Session line by its first
   observer and dropped with its last, advanced from commit publications: entries appended, cut at
   a head marker to the entries from its head, documents (`pi.agent`, `pi.live`, `pi.inbox`,
   `pi.provider`, `pi.usage`) set whole when mounted anew and otherwise by their own operations
   under `["docs", kind]`, documents first, then entries. `ApplyImmutable` keeps the untouched parts
   of a revision shared, as pi's case checks by reference. pi's `publication` observer and
   `conversationViews` are kept internal for the event adapters of slice 1 (d). `live.ts`'s run
   control came with slice 1 (b); its tool and compaction parts come with slices 2 and 3. Tests:
   `harness-view` whole (14), `harness-live-deltas` without tools (2 of 11; the 9 that drive a tool
   wait for slice 2). Adapted: frames are delivered off the commit, so the cases wait for the
   frames they count, then a little longer for a frame too many. Added beyond pi: a document
   recreated in one commit mounted whole, a keyed member of a mounted kind (another definition of
   the kind can be a family) ignored, another conversation's documents ignored, document
   operations before entry operations, a view state cancelled while it waits for the line.
   Sabotage 15 of 21. Duplicate safeguards: the incarnation check (a retirement is published
   before the recreation and unmounts first) and the closed check in attach (the line refuses
   after close). Unreachable through the API: a panicking observer. Waiting: the publication
   observer (slice 1 (d)). Unchecked: a root replacement of a document, and a document's
   definition version changing under a mount.

   **Durable slice 1, the rest of generation** (snapshot `d2e7d8b7`; `generation.ts`): the `retry`
   phase (a durable backoff: the error answer is kept, `pi.live.generation.retry` shows when the
   next attempt starts, an early wake sleeps again, the next attempt prepares anew and so reads the
   settings again), the `poll` phase for a deferred response (`pi.live.generation.deferred`, a
   still deferred result polls strictly later), and the abort of a polling generation, which
   cancels the deferred response first and reports a failed cancellation as the host's `models`
   callback. The tools phase and compaction stay "not ported yet" until slices 2 and 3. Tests:
   `harness-generation` whole (21), `harness-generation-recovery` whole (8, on a JSONL directory
   instead of pi's SQLite file, as in tasks recovery; the "earlier version" case rewrites the task
   record in the closed Storage instead of SQL), the third `harness-reports` case (sections, hooks
   and the environment builder inside `pi.generation`), and the `harness-inbox` usage case across
   reopen held back from slice 1 (c). The faux provider of the chat setup now forwards deferred
   fetches and cancellations, as pi's does. Adapted: the faulting final message holds a NaN cost
   instead of a function (Go marshals both to an error); pi's request signal is the request
   context, so the stream-options case does not check it. Differences: a provider handle's
   `PollAfterMs` of 0 means "not said" in `ai` (pi: 0 polls at once), so it waits the default
   5 s. Fixed on the way: `HarnessSettings.RestartLimit` of 0 resolved to 0, and a limit of 0
   parked every task (`Restarts >= 0`); pi takes the default below one restart, and so does Go
   now (negative stays "never", pi's Infinity). Added beyond pi, for mechanisms no case of pi's
   checks: a poll stored as a running phase by an earlier version sleeps again instead of
   fetching, and a still deferred result polls strictly later than the poll that returned it
   (made certain by a clock stepped back during the poll; pi's case catches it only when two
   polls share a millisecond). Sabotage 14 of 17. Equivalent: the retry phase's attempt in
   `pi.live` (the request phase writes it again at once, in pi as well). Unchecked, as in pi:
   the default poll wait and the backoff's cap.

   **Durable slice 1, agent events** (snapshot `d2e7d8b7`; `events.ts`, `observation.ts`): pi's
   `CommittedWatch<T>` is generic, with an optional `replace()` for the value an overflow delivers;
   Go's becomes `durable.CommittedWatch[T]` with `CommittedWatchOptions{Replace, Retires}`, and
   `DocumentWatch` and `WatchListener` are its type aliases for documents, so their API is
   unchanged. `Conversation.WatchEvents` (experimental, spec §9.4) is pi's `watchEvents`: a
   snapshot at attachment, then one batch per commit that causes events, in the spec's order,
   translated from the view's internal publication hook; an overflow replaces the undelivered
   batches with a snapshot of the newest view. `AgentEvent` and `MessageChange` are interfaces
   over concrete types whose JSON carries `"type"` first, as pi's unions (one flat struct does not
   fit: `message` is a message in `message_start` and a string in `task_failed`). Whether a
   built-in document changed is told by the identity of its immutable value (the mount shares
   untouched parts), as pi compares by reference; a deep comparison would drop events for a write
   of an equal value. The tool events (`tool_execution_*`) and compaction events, and the
   snapshot's `tools`, `nestedTools`, `compactions` and `compactionFailure`, come with slices 2
   and 3, with the `pi.live` slots they read. Tests: `harness-events` without the cases that run a
   tool (9 of 14, the mixed case without its tool part; the 5 wait for slice 2);
   `harness-input-entries` without the two that hold a tool (9 of 11); its untyped-caller case
   checks how the entry is queued and placed, since `InputEntry` has no other fields, and its
   non-string kind cannot be written in Go. Adapted: batches are delivered off the commit, so the
   cases wait until no batch arrives for 50 ms. Added beyond pi: a commit while a partial is in
   flight that leaves it unchanged sends no `message_update`. Sabotage 17 of 23. Waiting for
   slice 2: a turn ending at a completing hold (a generation holds its outcome only while it owns live tool
   tasks), and the thinking delta. Duplicate: the cancellation check after attaching (attach
   checks it on the line). Unchecked, as in pi: `task_failed`, the generations held at
   attachment, and a block sent whole in the same batch as its deltas. The slice 1 (c)
   sabotage's display-only fast path and outcome cleanup are now caught.

   **Fix: close writes back a run that ended while closing** (found by a Windows CI flake of
   `TestCloseStopsWithoutOutcomesAndStartsNoFreshPhaseOrAbortInvocation`, "commits 5 -> 5"). pi's
   `close()` marks closing, seals and collects its invocations in one synchronous step; Go's seal
   runs on its own goroutine and `join` collected the invocations only once it was done, so a run
   that returned in between was neither stopped nor written back pending, and the next open
   counted a restart for it. The scheduler now remembers the invocations that end after closing
   began, and `join` writes back those with the ones it stops. A 50 ms delay before `join`
   collects reproduced the CI failure 10 of 10 on the old code and passes 10 of 10 on the new.

   **Catch-up to pi `73f8de80`** (two commits after `d2e7d8b7`; W-009 on 20.10: 1241 / 25 159 =
   4.9%). `5c1736f8`: `checkpointWhen` is required on every document definition, with no
   default. pi enforces that by type only; `DefineDoc` refuses a definition without
   `CheckpointWhen`, and `durable.NeverCheckpoint` is pi's `() => false`. The test definitions
   get it where pi's tests do; the semantics table too, so its definitions are still refused for
   their own reasons. pi's `codemode-checkpoints` cases wait for codemode (stage 5) and the
   nested-call document (slice 2). `61060ddf` (pi#9490) changes both command runners, which share
   `internal/proc`: the legacy WSL launcher is matched under `SystemRoot`, the taskkill path falls
   back to `C:\Windows` for an empty `SystemRoot` too, and the coding powershell tool finds
   PowerShell under `ProgramFiles` and `SystemRoot`. `WinNormalize`/`WinJoin` stand in for node's
   `path.win32` on any OS (expectations checked against node). Tests: the definition case,
   `windows-shell-config`, `powershell-config`, `env-node`'s transport table (POSIX only, as in
   pi) and the taskkill regression. n/a: the coding agent's `docs/windows.md` and the
   tools-manager's `WINDIR` fallback (not ported). Added beyond pi: a `SystemRoot` that is not
   drive-absolute matches nothing, not even its own relative join. Sabotage 11 of 11.

   **Fix: a cancelled caller of `TaskRuntime.Agent` fails even when the resolution is done**
   (found by a Windows CI flake of `TestTaskFailsAnAgentWaitWhoseCallerIsCancelled...` on PR #56;
   ported wrong since stage 4b-1). pi's `awaitWithContext` checks `signal.aborted` first; Go's
   `select` over a closed `done` and a cancelled context picks either. A probe that resolves first
   and then waits with a cancelled caller failed 25 of 50 before and 0 of 50 after; it stays as a
   case beyond pi.

   **Fix: the sleep timer's deadline is computed when the wake is scheduled** (found by a Linux
   `-race` CI hang of `TestGenerationPollsADeferredResponseUntilItIsReady` on PR #56; ported wrong
   since D1). pi computes the wake in a microtask, before the pass the same change starts, so a
   sleep the wake finds due has its pass still ahead. Here delivery and pass run on goroutines: a
   delivery late enough to find a 1 ms sleep due skipped it, after the pass had found it not due,
   and nothing armed or ran it again. `scheduleWakeLocked` (and open, which delivers directly) now
   compute the deadline under `mu`; the delivery arms what was computed. A case beyond pi holds
   `wakeMu` while the pass runs and the clock passes the deadline: 3 of 3 failures before, 0 of 20
   after.

   **Durable slice 2a, the tool round** (snapshot `d2e7d8b7`; `tool.ts`, the tools phase of
   `generation.ts`, the slots of `live.ts`). The built-in `pi.tool` task, version 2 (a version 1
   record migrates to a model-issued call): its call phase resolves the tool among the tools the
   model is offered, repairs and validates the arguments, runs `BeforeTool` (the first block wins;
   an error blocks with its text), validates again, records intent, executes, runs `AfterTool`, and
   settles a `pi.tool-result` entry; its abort settles from the slot's durable output. The
   generation's tools phase starts the round in the tool-calling answer's commit (an unoffered call
   is answered there; a sequential round starts one call at a time), waits for the tool tasks it
   owns, and applies their controls (`addTools`, `terminate`, the last `handoff`); aborting it answers
   the calls never started. `ToolExecutionAPI` is an interface, so a wrapper embeds it and replaces
   members, as pi's wrappers spread the api; `Output` takes bytes and an `output.Skip` (an
   environment's `OutputSkip` converts field by field); `Details` blocks until a progress commit
   holds the value. Running output, details and diagnostics commit to the slot field by field,
   paced by the Harness clock: `chord/output` exports `Clock` and `NewProgressWithClock`. Structured
   output of a model-issued call is checked with `ai.Schema.ValidationErrors` and only reported.
   The execute phase (recovery after intent), nested calls and the tool events come with 2b and 2c
   and fault as "not ported yet" until then. Fixed on the way, in this slice's own code: `handOver`
   and the run's added inputs wrote `run` whole instead of `run.taskId` and an append to
   `run.inputs`; a slot's first progress commit wrote `output: ""` when nothing was output (pi's
   `slot.output ?? ""`). Tests: `harness-tools` without the two coding-tools cases (37; stage 5),
   the two `harness-callers` cases without nested calls (the three that probe `executeTool` come
   with 2c), `harness-host-inputs` (4), the tool cases of `harness-live-deltas` (9). pi's
   `models: Models | (() => Models)` is two options: `Models` and `ModelsFunc`, which wins when set and
   is read at every access (each request, each `Models()` of a runtime or tool API), as pi's
   `currentModels`; sabotage of it 3 of 3. Adapted: a parallel
   round's starts are checked as a set, since goroutines start in any order; explicit null details
   are `json.RawMessage("null")`, as Go has no undefined, and `ai.ToolResultMessage` then omits them;
   the order in which `Details` calls settle is checked per call; the undefined-control-keys case
   keeps its `addTools` half; the environment case uses temporary directories instead of `/tmp` and
   `/`. Sabotage 28 of 34. Waiting: the tool resolved among the callable tools for a nested call (2c),
   `AbandonOnRestart` for a non-replay-safe tool's child tasks (2c), and the aborted results of calls
   never started (2b). Equivalent: controls of a call that did not complete (no reachable result
   carries them). Unchecked, as in pi: an API call after the call settled, and rewriting an equal
   `modelTools`.

   **Durable slice 2b, recovery and tool events** (snapshot `d2e7d8b7`; `tool.ts`, `events.ts`). The
   tool task's `execute` phase, reached only by recovery after intent, reruns only when the stored
   and the current replay policy both say `safe`, after one commit clears the slot's progress;
   otherwise the call fails as interrupted, answered with the slot's durable output. Empty
   arguments are omitted from the stored intent and rerun as `{}`. Agent events gain
   `tool_execution_start` (the intent's arguments), `tool_execution_update` and
   `tool_execution_end` (with the result entry, directly before its message), and the snapshot
   gains `tools`. `ToolEventCall` is embedded in each tool event; its parent fields and the nested
   `result` of an end come with 2c. An update's output is one `ToolOutputChange` that encodes as
   `{set}` or `{trimStart?, append?}`; details and diagnostics a safe rerun cleared are sent as `null`
   and `[]`, marked by `DetailsChanged` and `DiagnosticsChanged`, as Go has no undefined. pi's `===`
   on slot parts is `sameValue`, now for any value: objects and arrays by identity, other values by
   equality. Tests: `harness-tools-recovery` without the real bash case (8; stage 5), the six tool
   cases of `harness-events` (one extends the ported usage-only case), the eight tool cases of
   `harness-inbox` and the two of `harness-input-entries`; their code came with 2a. The snapshot case
   checks its empty `tools` again, as pi's does; slice 1 (d2) had dropped that check. Beyond pi,
   from the sabotage run: an interrupted call fails (pi checks only its answer), empty arguments
   rerun as `{}`, a call a sequential round never started ends with its result entry, scalar
   details compare by value, and the JSON of an output change. Sabotage 18 of 18.

   **Durable slice 2c-1, nested tool calls** (snapshot `d2e7d8b7`; `tool.ts`, the nested slots of
   `live.ts`). `ToolExecutionAPI.ExecuteTool` runs a tool among those the call may call as a nested
   call in its own tool task, owned by the caller; one commit creates the task, its slot in
   `pi.live` `nestedTools` and its `NestedCallDoc` member (a task-scoped family of the caller, keyed
   by the call's key or position); a key already made returns that call when it names the same tool
   and arguments, compared without regard to key order. The first admission of an attempt commits the
   `pi.tool.madeNestedCalls` memo. A nested call settles into its caller's document, records its
   usage, and leaves a summary in its done slot; every settling call first aborts the nested calls it
   left running, all marked before any is awaited, and removes the slots below it. Programs get
   structured output: a schema tool's validated value, else the tool's bounded output (one text as a
   string, one image as itself, nothing as `""`, else the list); broken structured output is an error
   result for them and only a report for the model's calls; a schema tool's error text becomes a
   `tool_error` diagnostic. Forms: `NestedToolExecutionResult` carries its structured output and
   details as `json.RawMessage`, decoded by the program into its own type, so a stored null is
   `null`; `NestedToolSlot` and `ToolSlot` embed `SlotProgress`. A nested call's input stores its call
   as pi does, a tool call block with its `"type"` (`ai.ToolCall` alone has none). Cancelling the
   caller's context stops only its wait. `sliceUTF16` cuts the summary's error text in UTF-16 units,
   leaving a surrogate pair whole. Tests: `harness-structured-output` without the bash case (stage 5)
   and without the events half of the stored-result case, the three `harness-callers` cases that probe
   `executeTool`, and `harness-nested-tools` without the three cases that read nested calls' tool
   events (432, 861, 968) and the event halves of three others: those come with 2c-2, as does
   `harness-nested-tools-restart`. Adapted: values read back from a document list their keys in Go's
   order, so JSON compares by meaning; an empty explicit key is the call's position, as Go has no
   undefined key; a repair drops a property instead of setting it undefined; goroutines admit nested
   calls in no fixed order where pi's admissions are synchronous, so the abort-order case admits its
   two calls in sequence, and the admission-during-return case holds its admission at
   `beforeNestedAdmission`, a hook only tests set. Tests whose tools wait on the test open their
   latches at cleanup, so a failure ends instead of hanging Close. Beyond pi: a call made after its
   caller settled is refused. Sabotage 26 of 28: `abandonOnRestart` comes with the restart cases of
   2c-2; unchecked, as in pi: removing nested slots transitively, which matters only when a nested
   call left its own children's slots behind.

   **Durable slice 2c-2, nested calls' events and restarts** (snapshot `d2e7d8b7`; `events.ts`). A
   nested call's `tool_execution_start` carries the arguments of its slot, its updates read
   `pi.live` `nestedTools` (`toolUpdate` now takes the list and the shared `SlotProgress`), and its
   `tool_execution_end` carries `Result`, the result its commit stored in the caller's
   `NestedCallDoc`, read from the publication since the document may retire next; nested ends come
   children first, after the model-issued calls' and before the entries. Nested calls' events fill
   `ParentToolCallID` and `ParentTaskID`; the snapshot carries `nestedTools`. Tests: the three
   `harness-nested-tools` cases that read nested calls' tool events, the event halves of the
   abandoned-call, nested-of-nested and arguments cases and of `harness-structured-output`'s
   stored-result case, and all of `harness-nested-tools-restart` on JSONL; their mechanisms
   (abandonment on restart, its cascade, a missing definition) were already there. Adapted: cases
   whose caller makes two default-keyed calls hold the second until the first took its key, through
   `beforeNestedAdmission`, since pi's synchronous admissions keep call order and goroutines do not.
   Beyond pi: a nested call's later output is an append on its own slot's path (pi's nested updates
   are all a slot's first output, a set, which a wrong path also yields). Sabotage 8 of 12, with
   `abandonOnRestart` caught by the restart cases; unchecked, as in pi, and not reached by any flow
   found: a nested slot that vanishes unfinished, children-first order within one commit (nested
   calls end in commits of their own), a nested slot created done, and dropping another
   conversation's nested results.

   **Fix of slice 2c-1: a faulted or orphaned tool settles its nested slots.** pi's
   `settleSchedulerOutcome` removes the nested slots below a tool task the scheduler faults or
   orphans; the port marked only the tool's own slot done, so its nested calls' slots outlived it in
   `pi.live`. Found reading pi for slice 3a. A case for both outcomes fails on the old code.

   **Durable slice 3a, the compaction task** (snapshot `d2e7d8b7`; `compaction.ts`, the compaction
   parts of `live.ts`, `events.ts` and `harness.ts`, and pi-ai `utils/overflow.ts`). The
   `pi.compaction` task (select, summarize, retry, abort), range selection, serialization, placing a
   summary (appended for a blocking compaction, admitted as a write for a conversation-owned one),
   `CompactionHooks.BeforeCompact`, `Conversation.Compact`, `pi.live` `compactions` and
   `compactionFailure`, the scheduler's cleanup of a faulted or orphaned compaction,
   `compaction_start` and `compaction_end` with the snapshot's `compactions`, per-model
   `ModelOverrides`. In `ai`: `IsContextOverflow` (a `contextWindow` of 0 is pi's absent window),
   `IsRecoverableLength`, `OverflowPatterns`; `EstimateMessageTokens` and `CalculateContextTokens` are
   exported. Go forms: `CompactionDecision{Decline, Summary}` for pi's `{decline}`/`{summary}`, a nil
   decision asking the next hook; `CompactionResult` with nil fields for absent ones;
   `Compact(ctx, instructions *string)`, nil apart from `""`; `Settings.CompactionOverrides` beside
   the comparable `CompactionPolicy`. A summarize phase whose first kept entry is missing from the
   context at its tail faults, where pi's `findIndex` would summarize all but the last entry. Tests:
   every case of `harness-compaction` that needs no generation-side compaction (65, including the
   manual cases of the later sections) and the 20 of pi-ai `overflow.test.ts`; `context-overflow`
   calls live providers and is not ported. The events snapshot case now checks its parts, as pi's
   `toMatchObject` does. Beyond pi: the summarizer reads the context at the pinned tail (an edit the
   hook commits after selection is not summarized); a steer between a call and its result is
   summarized after the result; an answer with text and a tool call is no summary; a stop whose input
   exceeds the window, and both usage checks without a window. Sabotage 54 of 59: `compactionFailure`
   (kept on success, missing on failure and from the snapshot) and `compaction_end`'s error on another
   task's end come with the background cases of slice 3b; a hook's panic is equivalent to its error.

   **Fix of slice 1 (d) test: the queued view acquisition waits for Close.** The case that closes the
   Harness while a view acquisition waits for the Session line released the line right after starting
   Close; when Close had not yet closed the views, the acquisition attached and returned no error
   (CI, Linux `-race`). It now waits for the views to close first. A delay before Close reproduces the
   CI failure on the old case every time and passes on the new one.

   **Durable slice 3b, compaction in generation** (snapshot `d2e7d8b7`; the compaction parts of
   `generation.ts`, `estimateContext` in `compaction.ts`). Preparation starts a background threshold
   compaction in its commit while `pi.live` lists none, or creates a blocking one owned by the
   generation and waits for it (`allSettled`), then prepares again; prepare checks the waited
   compaction once: an overflow one without a summary entry fails `model_error` with the overflow's
   text, a threshold one that did not complete fails with "Compaction failed: …" or "Compaction was
   aborted". Classification waits for an overflow compaction (`IsContextOverflow` without a window,
   as pi calls it) when the request's model policy is enabled and range selection finds a cut; an
   overflow is never retried. The not-ported faults are gone. Go form: pi finds the measured answer
   in the context by object identity; `ContextView` keeps each message's origin (unexported, nil in a
   hand-built view, which then estimates every message, as pi's `lastIndexOf` miss does). Tests: every
   remaining case of `harness-compaction`, the manual blocking-and-manual case missed in 3a, and the
   two compaction cases of `harness-telemetry`. Beyond pi: no compaction task at all without a cut,
   for a threshold and for an overflow; one summary request for two overflows (with a budget that
   leaves a cut the second time); `estimateContext` skips an answer without usage and counts planned
   messages, and the threshold is strict; no `generation` in `pi.live` while an overflow compaction
   runs; another compaction's end carries no recorded failure. Sabotage 32 of 35, and slice 3a's
   remaining misses now caught (58 of 59): equivalent, as read, a background threshold at
   `backgroundTokens` 0 (it equals the blocking one, checked first) and the overflow context read
   past the cutoff (only display-only entries can land meanwhile); unchecked, as in pi, the orphaned
   compaction's text.

   **Durable stage 5a-1, read and write on an environment** (snapshot `d2e7d8b7`;
   `packages/durable/src/tools` and `truncate.ts`). New package `durable/tools`: `read`, `write`,
   `truncate` with `TruncateHeadOf`, the per-file mutation queue, tool paths, and the
   `ImageProcessor` contract (a processor itself comes with 5b). The format sniffing of the coding
   read tool moves unchanged to `internal/imagemime`, which both packages use; it adds `DetectOf`
   (the header and the PNG chunk headers of a file only) and `Dimensions`. `truncate` is not shared:
   pi's coding agent and durable choose `truncatedBy` differently (durable reports bytes when only a
   trailing newline exceeds them at the line cap), so a shared one would change `coding`. The queue
   is not shared either: durable keys a file by environment ID and the environment's canonical path
   and runs calls in the order their keys resolve; coding keys the OS path. Go forms: offsets and
   limits are JSON numbers with JS arithmetic, fractions and unsafe integers included, and messages
   render numbers as JS does; an empty output is an empty `ContentList`, not nil (nil keeps the
   streamed output); `ImageProcessor.Prepare` returns nil for an image it cannot prepare. Tests: the
   read, write and queue-through-write cases of `tools.test` (14; the symlinked directory and the
   backslash name run on Linux only), `env-truncate` (7; the runtime-without-Buffer case is n/a), and
   `tools-read-differential` (2, without NaN, which JSON arguments cannot carry; the reference does
   not reuse the tool's index and number helpers). Beyond pi: a file that changes during both passes
   fails; a fractional start at an over-long line names no line; a leading `@` is dropped; a read
   path finds the typographic-apostrophe variant; a cancellation right after a successful write
   fails the call. Sabotage 18 of 21: the non-inline format and the dimension check wait for the
   image cases of 5b; the canonical key of a new file under a symlinked directory is caught on Linux
   only.

   **Durable stage 5a-2, edit and shell tools** (snapshot `d2e7d8b7`; `edit.ts`, `edit-diff.ts`,
   `bash.ts`, `index.ts`). The edit matching of the coding tool moves unchanged to
   `internal/editmatch` (pi's coding agent and durable share this core of `edit-diff.ts`), with the
   line-ending helpers; `coding` keeps thin wrappers. `internal/diff` ports the part of jsdiff 8.0.4
   (BSD-3-Clause, license alongside) that edit calls: `diffLines` with its Myers variant and
   diagonal pruning, and `createTwoFilesPatch` with file headers only; callbacks, timeouts,
   `maxEditLength` and the ignore options are not ported. `durable/tools` adds `edit` (argument
   repair, BOM and CRLF preserved, a display diff and a unified patch in details), `bash` and
   `powershell` on the environment's exec (prepare, command prefix, timeouts in seconds, spill
   diagnostic, structured output with the retained tail), and `NewCodingTools`, a fresh
   `coding-tools` extension per call in place of pi's shared `CodingTools` value. Tests: the edit,
   powershell and bash cases of `tools.test` (21; real PowerShell on Windows only, the symlink cases
   on Linux only); pi asserts the patch with jsdiff's `applyPatch`, here it equals jsdiff's own patch.
   The harness cases that run these tools (`harness-tools` coding tools 2, `harness-tools-recovery`
   real bash 1, `harness-structured-output` bash 1) live in `durable/tools`, as the harness tests
   cannot import the tools, through the Harness's exported API. Oracles: `internal/diff/testdata/
   capture.mjs` runs jsdiff on 406 seeded cases (changes and patches equal byte for byte);
   `durable/tools/testdata/capture-edit-diff.mjs` runs pi's own `edit-diff.ts` under node on those
   and 150 long texts (display diffs and first changed lines equal). Beyond pi: the display diff
   oracle; a zero timeout and one above the maximum are rejected; a cancelled call fails with the
   environment's error, an environment's own abort with "Command aborted"; structured output says
   when earlier output was dropped; the extension's name and tools. Sabotage 33 of 35: equivalent,
   the diagonal pruning (a speed optimization; no difference on the oracle); the symlink kind in
   edit is caught on Linux only (`Stat` is `Lstat`).

   **Durable stage 5b, images and tool search** (snapshot `d2e7d8b7`; `src/images`, `search.ts`,
   `src/tool-search`). `durable/images` is the image processor (`NewImages`) in pure Go: the standard
   decoders with `x/image`'s BMP and WebP, the standard PNG and JPEG encoders. Its decisions are
   pi's `prepare`, which differ from the coding pipeline's: pass-through only for an upright inline
   image within the limits (base64 `<=` the limit), scale `min(1, maxW/w, maxH/h)` rounded, PNG then
   JPEG 80/70/55/40 (JPEG first for a JPEG source), then both sides times 3/4 until 1×1. EXIF is
   durable's own reader (`exif.ts`: unsigned sizes, a WebP EXIF chunk taken without checking its
   size). The pixel work — orientation and a bilinear downscale — moves unchanged from `coding` to
   `internal/imaging`; Photon resamples with Lanczos3, so tests check sizes, formats, limits and
   block colours rather than bytes. pi prepares in a worker thread so as not to block node's event
   loop; Go needs none: `Prepare` runs on the calling goroutine, is safe for concurrent use and
   stops between encodings on a cancelled ctx. The BM25 ranking of `coding/toolsearch` moves
   unchanged to `internal/search` (pi's `search.ts` is a copy of the coding agent's), and
   `durable/toolsearch` is the `tool_search` tool and a fresh `tool-search` extension per call. Test
   support for packages on the Harness: `durable/internal/chattest` (chat through the exported API)
   and `durable/internal/imagetest` (pi's image fixtures, encoded by the standard library). Tests:
   `tools-images` — read without a processor, of a changing image, and `imageDimensions` in
   `durable/tools` (10), the processor, EXIF and worker cases as goroutine cases in `durable/images`
   (13 + 8 orientations); n/a the event-loop worker case, the two `toBase64` cases and
   `images-wasm-module`; `tool-search` (6). Beyond pi: an image whose base64 is exactly the limit
   passes through; rounding of the scaled side; shrinking by 3/4; shrinking on once one side is 1;
   cancellation; the lower JPEG qualities before shrinking; tool_search's query and limit checks,
   callers and replay, no control without matches, the first description line, its search text and
   the extension name. Sabotage 27 of 27, including the two image branches of read missed in 5a-1.

   Under the race detector, `durable/images` took 80–132 s on CI (113 s locally), 84 s of it in
   the two noisy 2000×2000 cases (the 3/4 shrink and the tight half of the PNG re-encoding). They
   test sizes and limits, not concurrency, so the race step skips them (`raceDetector`, as harness
   does for its depth tests); the test step without it runs them; locally 35 s under race.

   **Durable stage 5c, code mode** (snapshot `d2e7d8b7`; `src/codemode`: `codemode.ts`,
   `output.ts`, `describe.ts`). `durable/codemode` is the replay-safe code mode tool on the
   `codemode` package's deterministic `Sandbox.Start` (start time, seed, fingerprint, `OnCall`,
   `OnIdle`, `Deliver`, `During`), with pi's documents (`pi.codemode.run`, `pi.codemode.journal`,
   `pi.codemode.store`) and its replay: journal before delivery, a call launched only once the count
   of calls issued before it is committed, and the divergence checks (module version, fingerprint,
   deadline, counts, call targets and arguments, a replay that ends where the live run went on).
   `models.*` is not ported (the engine's model access has no classifier or image generation): the
   tool is pi's with `models: false`, and `codemode-models.test` (7) is n/a. Go forms:
   `Options.Sandbox` is an interface `*codemode.Sandbox` satisfies; VM callbacks change state under
   one mutex and leave commits, nested calls and deliveries to goroutines; results delivered
   together go to the VM last first, so it processes the batch in one turn and acknowledges it
   once, as pi's VM does with results of one microtask turn; result files are joined with the
   environment's `JoinPath`. `Draft.Get` returns a draft for an object member, so stored values are
   read with `At(...).Snapshot()`. Tests: `codemode.test` (41; restarts reopen JSONL; one case made
   deterministic, as Go runs nested tasks concurrently), `codemode-describe.test` (5), and the code
   mode replay case of `harness-telemetry`; `durable/internal/chattest` gains a telemetry adapter.
   Beyond pi: the fingerprint is recorded on the first run; a global where the journal has a tool
   call diverges; `searchTools()` rejects a limit that is not a positive integer; text items are
   numbered from two; an image shown twice is saved once; tools without a namespace that do not
   fit say so; a global made after a result does not run while that count's commit is held (a
   global runs in this process without a commit of its own, so the FIFO commit line, which puts a
   tool call's admission behind the count, does not hide a launch that skips the wait). Sabotage 38
   of 40: equivalent, a delivery after a failed journal commit (the failure aborts the VM first)
   and the first read of the output directory (the second commit reads it again, leaving an empty
   temp directory, as in pi).
   Fixed after CI on the PR (with one processor, `-cpu 1`, three cases failed or hung): covered
   calls started one goroutine each, in no order, so a later call could be admitted first and a
   call made just before the script ended could never start; pi starts a covered batch
   synchronously in call order. Now one launcher starts them in call order, each once the one before
   has started: a tool call once admitted (`NestedCallOptions.OnAdmitted`, new), a global once it
   executes. And `pump`, re-run after each commit, committed counts while the VM was still
   processing a delivered batch, a few at a time; counts alone now wait for the VM's idle on the
   last delivered result, one commit per batch as in pi. "Results that finish together" has no
   tick in Go: the case holds the first result's commit until all 50 globals return, so the rest
   arrive during it and go in one commit. CI runs `./durable/codemode/...` with `-cpu 1` too, which
   showed two more cases depending on timing: "replays results in the order they arrived" released
   call 1 once call 2's tool returned, while 2's result still travelled through its nested task (1
   is now released by a global the script calls on 2's result), and the restart timeout case had
   300 ms of real time to reach its hold (the Harness's clock now moves past a 60 s deadline while
   the call is interrupted; `chattest.Chat.Now`, new).
   Sabotage of the fix 6 of 7; counts committed mid-batch are caught only now and then (2 of 20
   runs with four processors): the window is a few statements between delivery and the re-run, and
   a deterministic case needs a test-only hook (open).
   **Fix: task graph case racing the late child** (stage 4b, found by CI on main). "Follows every
   live task through its statuses…" compared the observed graph with a rebuild from Storage once the
   parent was completing, while the scheduler could still be starting the late child on its own
   pass: the rebuild saw it running, the observed graph pending. In pi the comparison happens in the
   same turn; here the case waits until the late child runs, after which nothing commits until it
   is released. Shown by sabotage: a 5 ms delay before the scheduler's reservation commit fails the
   old case 20 of 20 with CI's message; the new one passes 20 of 20, also with a further 20 ms
   between reading the observed graph and rebuilding it.
   **Fix: structured cycle case reading the reviewer's ID before the test stored it** (stage 4b,
   found by CI). "Rejects a cycle of a hold and two waits…" stored the reviewer's ID when `start`
   returned, while the reviewer could already run, wait and release the worker, which then waited
   on task 0 ("Task 0 does not exist"). The reviewer now records its own ID before it waits. Shown
   by sabotage: 50 ms between `start` returning and the test storing the ID fails the old case 5 of
   5 with CI's message; the new one passes 20 of 20.
   **Fix: compaction cases queueing answers after a reopen resumed** (slice 3, found by CI). Six
   cases reopened the chat (`reopenCompaction`) and only then queued the faux model's
   summary or agent answer; the resumed compaction could ask first, get "No scripted summary
   response" and fail. In pi the queue is filled in the same turn, before the resumed work runs.
   `reopenCompaction` now takes the answers and queues them before `Resume`; a failed compaction
   reports its error text. Shown by sabotage: a 50 ms delay after `Resume` fails the old cases with
   CI's message ("compaction 20 = … failed …, want completed", 3 of 3); the new ones pass 5 of 5.

## 5. Bookkeeping

- `docs/UPSTREAM.md` keeps sky-valley's journal up to `5806068c2`; this file continues it to
  `06a46693` (v1.2.0).
- pi's model catalog is not regenerated in this sync.
- The session server stack is removed, as pi removed it in `7472cda0` (2026-10-12, before v1.2.0):
  the `server`, `client` and `protocol` packages and `coding/remotesession.go`, `coding/transcript.go`
  with their tests (about 13k lines of code and 15k of tests). Nothing else in the engine used them;
  `go mod tidy` drops no dependency. A host that needs a transport builds its own.
