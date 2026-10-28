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
| `4b994d5e` | repair invalid `\u` escapes in tool-call arguments | `ai/providers/partial_json.go` | to port |
| `87f9a364` | report malformed final tool-call arguments instead of keeping the parsed prefix | `ai/validation.go`, providers | to port; not here: Bedrock, Mistral |
| `34a8bb38` → `5ab7fe74` | images of error tool results for Anthropic: dropped, then kept again (net: keep, through `transform-messages`) | `ai/providers/anthropic.go`, `transform.go` | ported as the net change, with pi's two cases; not here: Bedrock |
| `dc939913` | `off` thinking level on Claude Haiku 5.5 and Opus 5 | `ai/providers/anthropic.go` | ported, with pi's two request cases; the catalog case (which shipped models support `off`) waits for the catalog regeneration |
| `3bc6eeba` | hold Anthropic system updates past dropped assistant messages | `ai/providers/anthropic.go` | ported, with pi's case; the engine's own whitespace-only case (`nativeEmptyAssistant`), whose capture at `9e05370b2` held the 400, is asserted again at `e3bc05e8` |
| `e3bc05e8` | fixed low top-level effort with Anthropic per-message effort | `ai/providers/anthropic.go` | ported. The Anthropic request-body capture is reshot at `e3bc05e8` (`anthropic-native-e3bc05e8.json`); the same node_modules reproduces the old `9e05370b2` capture byte for byte |
| `b271b0a5`, `d4d53612` (v1.2.0 range) | Anthropic mid-conversation tools defined inline (`inline-tools-2026-09-15`, `tool_definition`); one fallback to non-strict `prefer` tools on the provider's strict tool limits | `ai/providers/anthropic.go`, `ai/transcript.go` | ported with this batch: missed by the v1.2.0 triage (section 6). With `d4d53612`, the abort capture is reshot at `e3bc05e8`: an already-aborted signal now ends before `onPayload`, with undici's message |
| `63035734` | thinking before text when a completions chunk carries both | `ai/providers/openai.go` | to port |
| `60ac3bcc` | keep the prompt cache when the thinking level changes on OpenAI GPT-6 (new compat flag) | `ai/providers/openai_responses.go`, `compat.go` | to port |
| `caa6967b` | keep the prompt cache across tool changes on Responses | `openai_responses.go`, `openai.go`, `ai/transcript.go` | to port; not here: Azure, Codex |
| `530015d9` | list only announced removals as still unavailable | `openai_responses.go`, `ai/transcript.go` | to port |
| `4dd2b149` | whitespace-only tool output is empty | `openai.go`, `openai_responses.go`, `google.go` | to port |
| `1692bba6` | `thinkingLevel` for Gemini 4 models | `ai/providers/google.go` | to port |
| `7314819f`, `a7b94a03` | Gemini tool schemas: `const` as `enum`, literal data preserved | `ai/providers/google.go` | to port |
| `91130942` | drop thinking from other models instead of sending it as text | `ai/providers/transform.go` | to port |
| `14c81184` | `responseModel` on pi-messages terminal events | `ai/providers/pi_messages.go` | to port |
| `89c921d8` | harden untrusted URLs, terminal text and streamed data | `pi_messages.go` | to port the pi-messages hunk; durable `json.ts` with section 5; not here: OAuth |
| `f3fa8bd9` | faux cache writes are not input tokens | `ai/providers/faux.go` | to port |
| `7c22a93d`, `cae8d69e` | `totalFromProvider` on usage cost; OpenRouter-reported total cost | `ai/types.go`, `ai/models.go`, `openai.go`, `anthropic.go` | to port; not here: `openrouter-images`, `usage-totals` (host) |
| `2ec55d80` | pi-ai telemetry (`telemetry.ts`, simple options, models) | `ai/` | to port the pi-ai half; the durable and contract halves are ported |
| `adc5fa9f`, `04a93bad` | client identity and provider attribution move into pi-ai, with follow-ups | `ai/providers/pi_user_agent.go`, `attribution.go`, `agent/`, `coding/` | to port; durable hunks with section 5; not here: Bedrock, Mistral, Vertex, Azure, Codex, classifiers, images |
| `f715a15c` | parse streamed tool-call arguments incrementally (`streaming-json.ts`, pi#9265) | `ai/providers/*`, `agent/`, `mcp/streamablehttp.go` | to port last in the core; the chord and durable hunks with section 5 |
| `6f2f164a` | restore active tools on session resume | `coding/session.go` | to port |
| `bd3c9b82` | activate extension tools registered after the branch recorded its loadout | `coding/session.go`, tool loadout | to port |
| `02726086` | recovery omits tool results that belong to the truncated tool call | `coding/session.go` (repair) | to port |
| `8ddc44ec`, `ed76cca6` | write a forked session through one file descriptor; its test | `coding/session_store.go` | to port |
| `807a5c05` | keep session memory in line with the file when a write fails | `coding/session_store.go` | to port |
| `6fcb4867` | no fixed output limits on summary requests | `coding/compaction.go` | to port |
| `adfc1446` | `/compact` instructions apply to the split-turn prefix summary | `coding/compaction.go` | to port |
| `269afc66` | thinking level for branch summarization | `coding/compaction.go` | to port |
| `d7f7e0df` | one provider selection policy for model references | `coding/resolve.go` | to port by hunk (the resolver is partly ported) |
| `984cf664` | `provider/id` references whose provider id contains a slash | `coding/resolve.go` | to port |
| `90d863f7`, `95364a54`, `f049787d` | default models: Xiaomi `mimo-v2.6-pro`, NVIDIA Nemotron 3.5 Super VL, OpenAI `gpt-6.1-sol` | `coding/resolve.go` | to port |
| `e5fe9777` | drop models retired upstream | `coding/resolve.go` | to port the resolver half; catalog at regeneration |
| `fecc19e4` | grep context lines aligned with ripgrep for a lone CR | `coding/tooloperations.go` | to port: the engine has no ripgrep but follows its line splitting |
| `28bea006` | accept Windows Store shell aliases | `internal/proc` | to port |
| `27cc6bfc` | stop macOS shell groups before killing them | `internal/proc` | to port |
| `bdd9f039` | `mcp.json` with a UTF-8 BOM | `coding/mcpext` | to port |
| `ae823bc0` | tool search ranks tools first when the query is their name | `coding/toolsearch` | to port |
| `3daa2028` | codemode cancellation prompt kept bounded | `coding/codemodetool`, `codemode/` | check: may already be in the codemode snapshot (`d2e7d8b7` is later) |
| `885a564b` | queue codemode scripts at the worker limit | `coding/codemodetool` | to port |

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
`f715a15c`, `adc5fa9f`, `04a93bad` and `89c921d8` this is the next durable catch-up, triaged on
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
  table).
