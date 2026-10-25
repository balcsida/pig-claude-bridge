# Upstream provenance

This extension is a Go port of [pi-claude-bridge](https://github.com/elidickinson/pi-claude-bridge) (TypeScript, MIT). Nothing from the upstream source tree is vendored; the Go code was written against it. Where this port differs on purpose, [docs/compatibility.md](docs/compatibility.md) says so.

## Pins

| what | pin |
|---|---|
| pi-claude-bridge | `6aa276a1498e5376363f5c1f8cf04bfcaee38c93` |
| cc-session-io | 0.4.0 (MIT), the subset the bridge imports |
| `@anthropic-ai/claude-agent-sdk` | 0.3.310 (Claude Code 2.1.310). The protocol client was written from the SDK's public type definitions and live probes ([protocol reference](docs/reference/agent-sdk-protocol-0.3.310.md)); the SDK itself is not vendored or depended on |
| Claude Code | 2.1.310 (`ccproto.MinVersion`) |
| PiG SDK | `github.com/MichaelKinsy/PiG/extensions/sdk` v0.5.3 ([signatures](docs/reference/pig-sdk-signatures-v0.5.3.md)) |
| `@modelcontextprotocol/sdk` | 1.29.0, only as the reference for the JSON-RPC responses the in-process MCP server must produce |
| `@earendil-works/pi-ai` | 1.2.0, source of the generated model catalog |
| `golang.org/x/text` | v0.43.0, for NFC normalisation of project paths |

## File map

Paths are relative to `extensions/pig-claude-bridge/`. TypeScript paths are `src/` files of pi-claude-bridge at `6aa276a`; line ranges are indicative. "cc-session-io" means the files of the 0.4.0 package (`dist/chunk-7JUXR4CP.js`, `dist/index.d.ts`).

| Go file | source | note |
|---|---|---|
| `extension.go` | `index.ts` 2463-2660 (activation, handlers, registration) | PiG SDK wiring, host adapter (`sdkHost`), provider registration in the factory |
| `internal/askclaude/schema.go` | `askclaude-schema.ts` | AskClaude parameters, modes and defaults |
| `internal/askclaude/tool.go` | `index.ts` 2662-2780 and the AskClaude call handler near 1750 | tool definition, remembered/isolated sessions, notices |
| `internal/askclaude/ui.go` | `askclaude-ui.ts` | action summary and tool-name mapping, rendered as lines (D6) |
| `internal/bridge/askclaude.go` | `index.ts` `promptAndWait` (2263) | AskClaude query path |
| `internal/bridge/consume.go` | `index.ts` 1028-1100, 1486-1790 (`consumeQuery`, steering, tool-result delivery, abort, discard) | goroutines and channels instead of the event loop (D3) |
| `internal/bridge/debug.go` | `index.ts` 36-150 (`debug`, `makeCliDebugOptions`, `diagDump`) | |
| `internal/bridge/hooks.go` | `index.ts` 2463-2660 (event handlers) | `session_start`, `before_agent_start`, `agent_start`, `turn_start`, compaction/tree, shutdown |
| `internal/bridge/host.go` | new (Go-only) | `Host` interface and `Deps`, so the PiG SDK stays confined to `extension.go` and the bridge is testable with a fake host |
| `internal/bridge/isolated.go` | `index.ts` 495-600 (`extractIsolatedSummaryPrompt`, `isolatedStreamFn`, `runIsolatedSummary`) | |
| `internal/bridge/notices.go` | `index.ts` 914-1018 (`showStartupNoticeOnce`, `reportLeaks`), 1138-1146 (`effortForReasoning`) | |
| `internal/bridge/provider.go` | `index.ts` 1788-2262 (`streamClaudeAgentSdk`), 414-480 (`turnStart`, prompt extraction) | |
| `internal/bridge/query.go` | `query-state.ts`, `prompt-stream.ts`, `index.ts` 480-494 (`newAssistantOutput`) | |
| `internal/bridge/ratelimit.go` | `rate-limit.ts`, `index.ts` 510-550 | headers go to the debug log only (D1) |
| `internal/bridge/sessioncwd.go` | `session-cwd.ts` | most-recent-pending summary slot (D7) |
| `internal/bridge/state.go` | `index.ts` 187-340 (`SessionState`, `sessionKey`, `markRebuildForSession`, `armStaleContexts`) | `sponsorMarkRebuildForSession` not ported (D4) |
| `internal/bridge/stream.go` | `index.ts` 1101-1216, 1215-1485 (`processStreamEvent`, `processAssistantMessage`, finalisation) | |
| `internal/bridge/sync.go` | `index.ts` 341-414, 600-720 (`syncSharedSession`, `convertAndImportMessages`, `verifyWrittenSession`, `safeRealpath`), 219-237 (`readCarriedAttachments`) | |
| `internal/bridge/toolargs.go` | `index.ts` 914-1018 (`piToolNameFor`, `mapToolArgs`) | |
| `internal/bridge/usage.go` | `usage.ts`, `index.ts` `recordUsage` | |
| `internal/ccproto/args.go` | `index.ts` 53-57 (`CC_CHILD_ENV`), 2030-2096 (query options); the SDK's argv builder (`sdk.mjs`) | argv pinned by `testdata/sdk-argv-snapshot.json` |
| `internal/ccproto/control.go` | the SDK's control protocol (`sdk.mjs`), reference sections 2 to 4 | requests, responses, inbound `mcp_message`, interrupt |
| `internal/ccproto/exec.go` | `index.ts` executable options; the SDK's executable handling | `claude` from `PATH` and version check (D2) |
| `internal/ccproto/messages.go` | the SDK's `SDKUserMessage` type, reference section 4 | |
| `internal/ccproto/testdata/fakeclaude/main.go` | new (Go-only) | a fake `claude` the transport tests spawn; not part of the extension binary |
| `internal/ccproto/transport.go` | the SDK's transport (`sdk.mjs`), reference sections 1.4 and 2 | spawn, NDJSON reader/writer, exit handling, close |
| `internal/ccsession/paths.go` | cc-session-io `chunk-7JUXR4CP.js` 32-56 | project path normalisation and hashing |
| `internal/ccsession/records.go` | cc-session-io `index.d.ts`, `chunk-7JUXR4CP.js` 262-281 (base fields) | struct order is the key order cc-session-io writes |
| `internal/ccsession/repair.go` | cc-session-io `repairWithOrigin` (`chunk-7JUXR4CP.js` 62-121) | |
| `internal/ccsession/session.go` | cc-session-io session create/open/save/delete and `importMessages` | |
| `internal/ccsession/verify.go` | `session-verify.ts` | |
| `internal/config/config.go` | `config.ts` | |
| `internal/config/logpaths.go` | `log-paths.ts` | |
| `internal/convert/agentsmd.go` | `agents-md.ts` | |
| `internal/convert/attachments.go` | `attachments.ts` | |
| `internal/convert/convert.go` | `convert.ts` | golden-tested against `scripts/dev/golden-conversion.mjs` |
| `internal/convert/skills.go` | `skills.ts`; `formatSkillsForPrompt` of `@earendil-works/pi-coding-agent` for the block text | |
| `internal/convert/toolresults.go` | `extract-tool-results.ts` | |
| `internal/convert/transcript.go` | `transcript.ts` | |
| `internal/mcptools/schema.go` | `mcp-server.ts` (root-schema flattening) | |
| `internal/mcptools/server.go` | `mcp-server.ts`; `@modelcontextprotocol/sdk` 1.29.0 for response shapes | |
| `internal/models/catalog_gen.go` | new (Go-only), generated by `scripts/dev/gen-catalog.mjs` from `@earendil-works/pi-ai` 1.2.0 | static catalog (D5); do not edit |
| `internal/models/models.go` | `models.ts` | |
| `internal/promptcapture/capture.go` | `prompt-capture.ts` | |

## Bump procedure

For a new Claude Code / Agent SDK, pi-claude-bridge commit or cc-session-io version:

1. Install the new SDK into a temp directory (not into this repo) and diff its `sdk.d.ts`, the argv builder window and the set of control subtypes against the [protocol reference](docs/reference/agent-sdk-protocol-0.3.310.md). Update the reference where the wire changed.
2. Re-record the argv and `initialize` snapshot: `scripts/dev/record-sdk-argv.mjs` (writes `internal/ccproto/testdata/sdk-argv-snapshot.json`).
3. Regenerate the conversion golden: `scripts/dev/golden-conversion.mjs` (writes `internal/convert/testdata/golden-pi-history-310.jsonl`; needs the pi-claude-bridge clone with `node_modules`).
4. Regenerate the model catalog: `scripts/dev/gen-catalog.mjs` (rewrites `internal/models/catalog_gen.go`).
5. Run the offline suite: `cd extensions/pig-claude-bridge && gofmt -l . && go vet ./... && go test -race ./...`.
6. Run the contract tests against the new `claude` by hand (they spend quota): `go test -race -tags live ./internal/ccproto/` (see [docs/verification.md](docs/verification.md)). A failure names the broken assumption; fix the port, not the test, unless the new behaviour is the intended one.
7. Update the pins here, in [docs/compatibility.md](docs/compatibility.md), `ccproto.MinVersion`, the versions in [docs/verification.md](docs/verification.md), and add a [CHANGELOG.md](CHANGELOG.md) entry.

For a pi-claude-bridge commit, also diff `src/` between the pinned commit and the new one and port the changes file by file using the map above.
