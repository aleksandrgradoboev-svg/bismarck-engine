# Sync with pi: v1.2.0 → v1.4.0 and after

Range: pi `06a46693..59ca40cf` (v1.2.0, 2026-10-13 → main, 2026-10-27; v1.4.0 is `4a080252`,
2026-10-22), 238 non-merge commits. The durable family (`durable`, `chord`, `codemode`,
`telemetry`) is already ported to `73f8de80` (`sync-v1.2.0.md`, section 4), so for it the range
starts there.

Commits were triaged by the paths they touch and their subjects; each one is decided again from
its diff when it is ported. A commit that touches several groups is listed under the first group
in the table order, and its other hunks are noted in its row.

## 1. Triage

| Group | Commits | Decision |
|---|---|---|
| Core in scope (`agent`, `ai`, `coding`, `mcp`, extensions `mcp`/`tool-search`/`codemode`) | 55 | port (section 2) |
| Core, host or no Go home | 24 | no (section 3) |
| Core, already ported | 1 | `61060ddf` (catch-up to `73f8de80`) |
| Catalogs and provider definitions (OpenCode, Kimi Coding, Xiaomi, Codex, MiniMax, GPT-6.1 Terra…) | 12 | at the next catalog regeneration, not in this sync (section 4) |
| Tests that only follow catalog changes | 8 | with the catalog |
| `durable`, `chord` after `73f8de80` | 24 | next durable catch-up, as a snapshot (section 5) |
| `durable` family up to `73f8de80` | 39 | done |
| TUI, HTML export, clipboard | 18 | no: the engine is embedded, the host draws the UI |
| npm package manager | 3 | no: host |
| Releases, "Unreleased", dependency bumps | 8 | version bumps only — no |
| Changelogs, documentation | 20 | no |
| `env`, `evals`, examples, Nix, CI, contributor gate | 25 | no |
| Bedrock shims (`531340f1`) | 1 | no: out of scope |

## 2. Core to port

Suggested order: small batches by file first, the coupled changes (client identity, incremental
tool-call arguments) last. "Not here" marks hunks of the commit that stay out.

| pi commit | Change | Engine home | Status |
|---|---|---|---|
| `7efebff1`, `550f1f3e`, `0fbdbd3f`, `698a2d23`, `29057ece`, `6e491cfa`, `cedefc53` | retry: gateway "Unable to forward upstream stream" and more gateway failures, "service is temporarily unavailable", node-fetch "request to … failed", Codex CLI disconnects relayed by proxies, status codes only as standalone numbers; ChatGPT Business usage limit (also in `openai-responses.ts`) | `ai/retry_classify.go`, `ai/providers/openai_responses.go` | ported: the alternatives translate `\S`, `+`, groups with `\|` and the edge lookarounds to RE2 with JavaScript's semantics (RE2 has no lookaround; a stand-in consumes the character, which tests the same at an alternative's edge only, and anything else still refuses to build); the reference is reshot with node at `59ca40cf` (`classify-59ca40cf.json`, 188 rows), with rows where JavaScript and RE2 disagree on `\S` and the lookarounds; `RegExp.prototype.source` escapes `/`. The workspace-limit hint has no test in pi; one is added |
| `73f8de80` | HTTP 413 is request-size overflow for any provider (Copilot "failed to parse request") | `ai/overflow.go` | ported, with pi's Copilot case through the Responses error path; `\s` is JavaScript's and the "i" covers only "API error", so no Unicode fold reaches `\w` (checked against node). pi's coding-agent regression (`regressions/copilot-request-size-overflow`) has its counterpart in the durable harness, whose generation owns overflow compaction here: a Copilot 413 compacts and retries |
| `4b994d5e` | repair invalid `\u` escapes in tool-call arguments | `ai/providers/json.go` | ported; the streaming-json capture is reshot at `e3bc05e8` (one case moves) |
| `87f9a364` | report malformed final tool-call arguments instead of keeping the parsed prefix | `ai/validation.go`, `ai/types.go`, `json.go`, providers | ported: `ToolCall.ArgumentsError`, with V8's JSON.parse message (`jstext.JSONSyntaxError`); the openai-stream capture is reshot at `e3bc05e8` and now records `argumentsError`, as does the deletedIndex row of the Anthropic SSE capture (reshot at `e3bc05e8`, calibrated at `49681e1b7`). pi's `parseToolCallArguments`, validation and Responses cases are transliterated. Not here: Bedrock, Mistral |
| `34a8bb38` → `5ab7fe74` | images of error tool results for Anthropic: dropped, then kept again (net: keep, through `transform-messages`) | `ai/providers/anthropic.go`, `transform.go` | ported as the net change, with pi's two cases; not here: Bedrock |
| `dc939913` | `off` thinking level on Claude Haiku 5.5 and Opus 5 | `ai/providers/anthropic.go` | ported, with pi's two request cases; the catalog case (which shipped models support `off`) waits for the catalog regeneration |
| `3bc6eeba` | hold Anthropic system updates past dropped assistant messages | `ai/providers/anthropic.go` | ported, with pi's case; the engine's own whitespace-only case (`nativeEmptyAssistant`), whose capture at `9e05370b2` held the 400, is asserted again at `e3bc05e8` |
| `e3bc05e8` | fixed low top-level effort with Anthropic per-message effort | `ai/providers/anthropic.go` | ported. The Anthropic request-body capture is reshot at `e3bc05e8` (`anthropic-native-e3bc05e8.json`); the same node_modules reproduces the old `9e05370b2` capture byte for byte |
| `b271b0a5`, `d4d53612` (v1.2.0 range) | Anthropic mid-conversation tools defined inline (`inline-tools-2026-09-15`, `tool_definition`); one fallback to non-strict `prefer` tools on the provider's strict tool limits | `ai/providers/anthropic.go`, `ai/transcript.go` | ported with this batch: missed by the v1.2.0 triage (section 6). With `d4d53612`, the abort capture is reshot at `e3bc05e8`: an already-aborted signal now ends before `onPayload`, with undici's message |
| `63035734` | thinking before text when a completions chunk carries both | `ai/providers/openai.go` | ported: reasoning, then reasoning_details, then content, then tool_calls; the two engine tests that pinned the old order are turned around, with pi's two cases |
| `60ac3bcc` | keep the prompt cache when the thinking level changes on OpenAI GPT-6 (new compat flag) | `ai/providers/openai_responses.go` | ported (`supportsMidConvoEffort` in the Responses compat; the engine has no compat schema file), with pi's four cases; the catalog half (which models set the flag) at regeneration |
| `caa6967b` | keep the prompt cache across tool changes on Responses | `openai_responses.go`, `openai.go`, `ai/transcript.go` | ported with `530015d9` as the net change (`TranscriptToolSupport`, `FoldedSystemMessages`). The Responses body capture is reshot at `e3bc05e8` (`responses-midconvo-e3bc05e8.json`; the old one reproduces byte for byte first); the completions capture at `e3bc05e8` has the same bytes as at `95fbc0499`, and the tools-only capture's bodies are unchanged, so it stays at `95fbc0499` (its faux entry can no longer be taken: `fe11328b` removed `serializeContext`). pi's OpenAI and Kimi cases are transliterated; the Kimi Coding ones read the catalog and wait for it. Not here: Azure, Codex |
| `530015d9` | list only announced removals as still unavailable | `openai_responses.go`, `ai/transcript.go` | ported with `caa6967b` |
| `4dd2b149` | whitespace-only tool output is empty | `openai.go`, `openai_responses.go`, `google.go` | ported (JavaScript's `trim`), with pi's cases for the three transports |
| `1692bba6` | `thinkingLevel` for Gemini 4 models | `ai/providers/google.go` | ported. The thinking-config capture is reshot at `e3bc05e8` (the old `16235fd93` one reproduces byte for byte first); its case "gemini 4 uses budgets" becomes "uses levels", with pi's `gemini-4-argon`, a two-digit major and a path prefix |
| `7314819f`, `a7b94a03` | Gemini tool schemas: `const` as `enum`, literal data preserved | `ai/providers/google.go`, `ai/schema.go` | ported as the net change, on the schema's JSON in pi's key order. Both commits come after `e3bc05e8`, so the new capture (`google-tools/const-as-enum-a7b94a03.json`, pi's cases plus edges, compared byte for byte) is taken at `a7b94a03`. It found a bug of the engine's own: `ai.Schema` dropped the tuple form of `items`; it now travels as written, as the other unrepresentable keywords do |
| `91130942` | drop thinking from other models instead of sending it as text | `ai/providers/transform.go` | ported, with pi's two cases; the jstrim capture is reshot at `e3bc05e8`, its transform case moved onto the model's own turn, where the trim still shows. The response-model capture is reshot at `e3bc05e8` against the 1.4.0 build (calibrated at `d86654abb`): its replay into another model now drops the thinking. The 1.4.0 catalog's claude-opus-5 lost its `"off": null` (the catalog half of `dc939913`), so the extraction gets it back until the catalog is regenerated |
| `14c81184` | `responseModel` on pi-messages terminal events | `ai/providers/pi_messages.go` | ported |
| `89c921d8` | harden untrusted URLs, terminal text and streamed data | `pi_messages.go` | ported the pi-messages hunk (a `contentIndex` that is not the next or an existing block fails the stream, with V8's TypeErrors where pi's check throws one); both pi-messages captures are reshot at `89c921d8`. The engine's hole and property-key machinery in the converter is unreachable now; durable `json.ts` with section 5; not here: OAuth |
| `f3fa8bd9` | faux cache writes are not input tokens | `ai/providers/faux.go` | ported; its `Math.max(0, …)` floor on cache writes is kept but unreachable (the common prefix is never longer than the prompt) |
| `7c22a93d`, `cae8d69e` | `totalFromProvider` on usage cost; OpenRouter-reported total cost | `ai/types.go`, `ai/models.go`, `openai.go`, `anthropic.go`, `openrouter_cost.go` | ported (`ai.OptionalBool`, so an unpriced usage leaves the key out and a priced one writes `false`, as pi's), with pi's `openrouter-cost` cases; the Anthropic SSE, 1-hour cache-write and response-model captures are reshot at `e3bc05e8` and gain only that `false` where nothing else changed. The `usage-totals` hunk is the engine's too, not the host's as first triaged: `combineUsage` (`coding/session_nested.go`) marks a sum provider-reported only when both parts are, with pi's case, and the nested-call recorder now keeps its first usage and combines the rest as pi's does, so it also stops dropping `cacheWrite1h` and `reasoning`; not here: `openrouter-images`, the footer's `addUsageToTotals` (host) |
| `2ec55d80` | pi-ai telemetry (`telemetry.ts`, simple options, models) | `ai/telemetry.go`, `ai/models_runtime.go`, `ai/types.go` | ported the pi-ai half (the durable and contract halves were ported with `d2e7d8b7`): `TelemetrySchema` (pi `AI_TELEMETRY_SCHEMA`, as data), and Models `Stream`, `StreamSimple` and `StreamDeferred` open `pi.ai.request` or `pi.ai.poll` under the caller's span, setup failures included, with the first-chunk and block events, end attributes, the bounded `provider_error` status and the four metrics; events reach the caller before the observer sees them, and the result once, after the stream ends. `ProviderRequestOptions.TelemetryContext` is renamed `Telemetry` and takes a `telemetry.Span`, as pi's, with no alias; durable's generation and compaction requests now get a `pi.ai.request` under their request phase (the half of pi's content-free case that does not wait for slice 2, and durable's metric-ID check filtered to durable metrics as pi's is). Without a parent span, or under `NoopSpan`, the provider stream is returned as it is. The instrument cache keeps the last adapter only (Go has no weak map over interface values), and the forwarding stream does not time the response again, so a traced result equals an untraced one. The error message is cut at 500 UTF-16 units, as pi's, and a surrogate pair at the cut is dropped whole. pi's 21 cases (17 of telemetry, 4 of lazyStream), adapted where Go cannot express them (reasoning on non-simple options, a throwing arguments getter, a stream without `result`), plus two for the cut. Sabotage: 12 of 13, the survivor equivalent (clearing a finished tool block's counter) |
| `adc5fa9f`, `04a93bad` | client identity and provider attribution move into pi-ai, with follow-ups | `ai/identity.go`, `ai/providers/attribution.go`, `ai/models_runtime.go`, `agent/`, `durable/harness`, `coding/identity.go` | ported as the net change. `ai.ClientIdentity` (User-Agent mode, `ClientAttribution` or `NoAttribution` for pi's `null`, version; pi's plain JSON form), `MergeClientIdentity`, `FormatUserAgent` (`pi/<version> (<platform>; go/<go version>; <arch>)`, Node's platform and arch names, the engine's version), `AttributionHeaders` (OpenRouter, NVIDIA with the name's first letter upper-cased, OpenCode), `OpenCodeSessionHeaders`, `IdentityHeaders`, `PIAttribution`, and `WithClientIdentity`, which calls the same method of the wrapped Models and keeps a `MutableModels` mutable. `ProviderRequestOptions.Identity`, `CreateModelsOptions.Identity`; Models merges its identity under the request's and adds `x-opencode-session` for any OpenCode target below the auth and request headers. Every adapter opens its header merge with the identity's headers, but for the Anthropic OAuth branch, which sends Claude Code's identity instead, so a consumer `User-Agent` of either spelling now beats claude-cli there (pi-messages too: its fixed headers now join one record, so a consumer header of any spelling replaces one and a marker deletes it); the `PI_TELEMETRY` gate, the Cloudflare `pi-coding-agent` User-Agent and the runtime user agent with the OS release are gone (`ai/providers/pi_user_agent*.go`). `agent.AgentOptions.Identity` / `Agent.Identity` over `DefaultAgentIdentity`, `harness.Options.Identity` over `DefaultHarnessIdentity` (the Harness's Models wrapped, a `ModelsFunc` result at each call), and `coding.ClientIdentity` (detailed, engine version, `PIAttribution` while `PI_TELEMETRY` is truthy, `null` otherwise), sent on every session request unless `SessionOptions.Identity`, added for a host, names another. **DIVERGENCE, decided 28.10.2026:** pi's agent-core and durable defaults attribute to pi (`{ attribution: PI_ATTRIBUTION }`); here both defaults are empty, and the coding agent attributes only on a truthy `PI_TELEMETRY`, the opt-in the engine had: an embedding product does not announce itself as pi unless its owner asks. pi's cases: 7 of identity (the version-file case n/a: the version is the engine's build information), its 4 request cases on a real adapter, the agent and durable identity cases with the empty default, durable's two Models-identity cases as pi adapted them, and 3 of the coding agent's 5 (the two OpenCode cases check pi's ModelRuntime, which the session does not use; the adapters' are covered). Not here: Bedrock, Mistral, Vertex, Azure, Codex, classifiers, images, login `agentName` (net unchanged) |
| `f715a15c` | parse streamed tool-call arguments incrementally (`streaming-json.ts`, pi#9265) | `ai/providers/*`, `agent/`, `mcp/streamablehttp.go` | to port last in the core; the chord and durable hunks with section 5 |
| `6f2f164a` | restore active tools on session resume | `coding/session.go` | ported: `LoadBranch` restores the loadout the branch records when the session is on the default tools (no `ToolNames`, or only `+name`/`-name` entries, which apply to it; pi `usesDefaultTools`), with pi's resume cases. A host registers its extension tools before `LoadBranch`, as pi's are registered by the time its constructor restores |
| `bd3c9b82` | activate extension tools registered after the branch recorded its loadout | `coding/session.go`, `coding/toolregistry.go` | ported: the loadout is read from every system message on the branch (`BranchProjection.Branch`, new), those before a compaction too, and extension tools it never mentioned are activated; `RestoreToolsFromTranscript()` becomes `RestoreToolsFromBranch(p)`, for `/tree`. pi's cases |
| `02726086` | recovery omits tool results that belong to the truncated tool call | — | n/a for now: `coding` has no overflow or length-stop recovery (its compaction triggers on its own estimate, never on a provider's refusal: `UPSTREAM.md` row 16), so nothing omits an attempt (pi `_omitRecoveryAttempt`); it goes with that recovery when it is ported. `durable/harness` recovers from an overflow by compacting and keeps the attempt |
| `8ddc44ec`, `ed76cca6` | write a forked session through one file descriptor; its test | — | n/a: the engine has no `SessionManager.forkFrom`, which copies a session from another project into a new file; `SessionRecorder.ForkFrom` only moves the parent of the next entry |
| `807a5c05` | keep session memory in line with the file when a write fails | `coding/session_store.go` | ported: a failed write or create is no longer ignored; the entry is not kept (its id, the leaf, the held-back entry, the conversation flag), the append returns an empty id and `SessionRecorder.Err` reports the error, where pi rethrows it. pi's two cases, with the failures injected: a recorder writes through a file it holds open, which a read-only mode does not stop |
| `6fcb4867` | no fixed output limits on summary requests | `coding/compaction.go` | ported: summary requests set no `maxTokens`, so the provider sizes the output from the model's limit fitted to the context (`SimpleMaxTokensDefault`); `ReserveTokens` only decides when compaction starts. The requests and resume captures are reshot against the 1.4.0 build, after the 0.87.1 build reproduced them (requests byte for byte, resume but for its fresh ids and timestamps); only `maxTokens` moved |
| `adfc1446` | `/compact` instructions apply to the split-turn prefix summary | — | n/a: the engine's compaction takes no custom instructions (pi `compact`'s `customInstructions`, the `/compact <text>` argument), so there is nothing to append to either summary |
| `269afc66` | thinking level for branch summarization | — | n/a: the engine has no branch summarization (`generateBranchSummary`); it reads branch summary entries only |
| `d7f7e0df` | one provider selection policy for model references | `coding/resolve.go` | ported: `resolveModelReference` and `resolveCliModel` as pi has them, the latter public as `ResolveCliModel` (`--provider`, `--thinking`, `--api-key`, and `HasAuth` for pi's `hasConfiguredAuth`). `ResolveModelPattern` counts a provider as authenticated when the environment holds its key (`EnvHasAuth`), as pi's CLI does, so a bare id on several providers resolves to the one with a key and is otherwise an error; `cmd/pi` goes through it. pi's reference table, run in both catalog orders, and its `resolveCliModel` cases. Not here: model scopes (`--models`/`enabledModels`), `findInitialModel`, credential printing |
| `984cf664` | `provider/id` references whose provider id contains a slash | `coding/resolve.go` | ported, with pi's two cases (in the reference table) |
| `90d863f7`, `95364a54`, `f049787d` | default models: Xiaomi `mimo-v2.6-pro`, NVIDIA Nemotron 3.5 Super VL, OpenAI `gpt-6.1-sol` | `coding/resolve.go` | ported but NVIDIA's, whose model the embedded catalog lacks: it waits for the catalog regeneration, as does pi's `azure` key (`a37306d4`, the catalog's `azure-openai-responses` renamed). The defaults capture is reshot at `f049787d` (`default-models-f049787d.json`, src mode) after `5806068c2` reproduced the old one byte for byte; the test names both pending entries |
| `e5fe9777` | drop models retired upstream | `coding/resolve.go` | ported the resolver half (GitHub Copilot `gpt-6.1-sol`); catalog at regeneration |
| `fecc19e4` | grep context lines aligned with ripgrep for a lone CR | `coding/tools.go` | ported: lines end at LF only, a lone CR stays in its line (the match runs on it, as ripgrep's does, checked against rg 15.0.0) and is dropped from the display; pi's case plus a match across the CR |
| `28bea006` | accept Windows Store shell aliases | — | n/a: the bug is Node's `existsSync` (nodejs/node#36790); Go's `os.Stat` accepts the aliases, checked on all 82 App Execution Aliases of a Windows 11 machine with Go 1.26 |
| `27cc6bfc` | stop macOS shell groups before killing them | `internal/proc` | ported as `KillShellProcessTree`, which the coding tools use: on macOS SIGSTOP to the group before SIGKILL. `durable/env` keeps `KillProcessTree`, as pi's durable environment does not stop first. pi's five signal-order cases, run under Linux (WSL) |
| `bdd9f039` | `mcp.json` with a UTF-8 BOM | `coding/mcpext` | ported the reading half, with pi's case; the engine does not edit `mcp.json` |
| `ae823bc0` | tool search ranks tools first when the query is their name | `internal/search` | ported as `Ranker.NamesFirst`, which `tool_search` and codemode's `searchTools` set; the durable searches keep plain BM25, as pi's `packages/durable/src/search.ts` does. pi's case |
| `3daa2028` | codemode cancellation prompt kept bounded | — | n/a: its changes are to worker-backed VMs (Bun's `terminate()` behind a non-interruptible built-in, #10748); the engine's VM runs on a goroutine, and a timeout ends at 200–318 ms for a 200 ms limit with a catastrophic regex, a 2M-element sort, a 50M split and a 3M-object `JSON.stringify` (measured), so nothing stalls the result |
| `885a564b` | queue codemode scripts at the worker limit | — | n/a: the engine's codemode has no workers and no worker limit (`PI_CODEMODE_MAX_WORKERS`), so a script never waits for one |

## 3. Core, not ported

| pi commit | Change | Reason |
|---|---|---|
| `cea25743` | session share destination | n/a: settings and slash commands (host) |
| `9fe3f371` | empty quoted prompt template arguments | n/a: no prompt templates |
| `b3997979` | remove the unused assistant-message frame API | n/a: never ported |
| `22dc6c26` | `pi-ai/models` in the extension loader | n/a: JavaScript extensions runtime |
| `c60de42e`, `36be9c61` | `pi.exec` spawn errors; UTF-8 across chunks | n/a: JavaScript extensions API |
| `8c4e8738` | grep runs ripgrep with `--no-config` | n/a: no ripgrep |
| `c8ec752c` | skip fd/rg downloads on unsupported architectures | n/a: no tool downloads |
| `ea25697e`, `6dfb5467` | managed install markers with a BOM; start when a package fails to install | n/a: npm package manager |
| `7f225f25` | image input for System One classifiers | n/a: no classifier subsystem |
| `842c7317` | Bedrock strict tool limit fallback | n/a: Bedrock; the Anthropic hunks only move the limit messages into a shared helper |
| `8efd314f` | generate the models.json reference from schemas | n/a: documentation generator |
| `9ffd27cf` | MCP OAuth accepts only http and https URLs | n/a: the engine's MCP has no OAuth |
| `91014991`, `a4f67eb6`, `38847563`, `64627838`, `7151099d`, `8c03fef4`, `606af866`, `7853186d`, `a6a7bac8`, `c1810bf7` | rendering, dialogs, selectors, device-code confirmation | n/a: host UI |

## 4. Catalogs

`6374c2ba`, `5712b134`, `d2e7d8b7`, `fdce48eb`, `363ad78b`, `81d6759f`, `1de2d7ad`, and the provider
definitions `195120a9`, `177e2ad7` (OpenCode endpoints), `b06c1f2c` → `6a1f799b` → `a3f265b1`
(Kimi Coding on Chat Completions, net) change model data only. The catalog is not regenerated in
this sync, as in v1.2.0; the catalog halves of `dc939913` and `e5fe9777` and the test follow-ups
`381a2d8e`, `568c2a82`, `dfb03c32`, `99fe1489`, `e86b0547`, `18e56a06`, `482c1381`, `fbc456d8` wait for
it.

## 5. Durable family after `73f8de80`

24 commits, about +3.7k/−0.8k lines of source: replicated SQLite and storage commit log (added and
partly reverted), `IndexedDbStorage` (n/a, as decided on 20.10), read-only Harness, `Watch` /
`Watchable` / `NativeWatchable` in chord, `appendText`, `PendingOps`, deferred storage of throttled
commits, publish at adoption, `runCounted`, and fixes (`38ff9917` runs after tool handoffs,
`d5b3de22` requested thinking level on assistant messages). Together with the durable hunks of
`f715a15c` and `89c921d8` this is the next durable catch-up, triaged on
its own.

## 6. Bookkeeping

- The groups come from sorting `git log --name-only` by path prefix, then checked by hand against
  the engine's packages; the script is not in the repository yet (task `bismarck-engine-sync`).
- The next sync starts from `59ca40cf`.
- **Missed in v1.2.0.** The v1.2.0 range (`5806068c2..06a46693`) has two commits to
  `anthropic-messages.ts`, `b271b0a5` and `d4d53612`, and its triage lists neither; nothing in the
  engine or `UPSTREAM.md` had them. They surfaced when the Anthropic capture was reshot at
  `e3bc05e8` and nine of its bodies differed, and are ported with the Anthropic batch (section 2).
  The rest of that range is not re-checked yet: a triage by path prefix can drop a core commit, so
  the v1.2.0 list wants the same check per core file (`git log <range> -- <file>` against the
  table). A second one surfaced with the resolver batch: `a37306d4` (Azure Foundry Chat
  Completions) renames the `azure-openai-responses` default to `azure` in `model-resolver.ts`, and
  neither sync lists it; its default waits for the catalog with the provider rename.
