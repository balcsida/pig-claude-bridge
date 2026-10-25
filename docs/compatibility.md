# Compatibility

What this port is pinned to, and every place where it differs from the TypeScript bridge (pi-claude-bridge at `6aa276a`) with the reason. Each item states the TypeScript behaviour, the port's behaviour and why. Evidence for the behaviours that hold is in [verification.md](verification.md); provenance of each file is in [../UPSTREAM.md](../UPSTREAM.md).

## Pins

| what | pin |
|---|---|
| PiG | 0.5.3 (`pig 0.5.3+1.4.0.74f46864`) |
| PiG Go SDK | `github.com/MichaelKinsy/PiG/extensions/sdk` v0.5.3, Go 1.26 floor |
| Claude Code | 2.1.310 (the port warns when the binary is older) |
| Agent SDK protocol | `@anthropic-ai/claude-agent-sdk` 0.3.310 (Claude Code 2.1.310); [reference](reference/agent-sdk-protocol-0.3.310.md). 0.3.308 to 0.3.310 changes nothing on the wire that the bridge uses; the one new control subtype, survey feedback, is never sent |
| pi-claude-bridge | `6aa276a`, https://github.com/elidickinson/pi-claude-bridge |
| cc-session-io | 0.4.0 (MIT) |
| Pi compatibility surface | pi 1.4.0 (the Pi that PiG 0.5.3 tracks) |
| `@modelcontextprotocol/sdk` | 1.29.0, only as the reference for the JSON-RPC responses the in-process MCP server produces |
| Model catalog | `@earendil-works/pi-ai` 1.2.0, generated into `internal/models/catalog_gen.go` |

## Principal differences (D1 to D7)

**D1. Rate-limit headers are not delivered to other extensions.** TypeScript: `rate_limit_event` frames become `anthropic-ratelimit-unified-*` headers handed to Pi's `after_provider_response` event (and the `onResponse` callback), so footers and quota extensions can read them. Port: the headers go to the debug log only. Reason: PiG's host does not emit `after_provider_response` (a documented host gap) and the config-route `streamSimple` has no `onResponse`. The user-visible notices (rejected with reset time, `allowed_warning` at 5% steps) are kept. To be removed when PiG closes the gap.

**D2. The `claude` binary comes from `PATH` with a version notice.** TypeScript: the Agent SDK launches its bundled binary unless `pathToClaudeCodeExecutable` is set. Port: `provider.pathToClaudeCodeExecutable`, else `claude` on `PATH`; a one-time notice appears when the binary is older than 2.1.310. Reason: downloading the SDK's bundled binary from npm's platform packages would add download and checksum code. A missing binary fails the call with a notice (the TypeScript fell back to the bundled binary).

**D3. Handlers run concurrently.** TypeScript: a single event loop; shared state needed no locking. Port: PiG runs one goroutine per host request, so session map, per-session state, prompt captures, the latest rate-limit event and notices are guarded by mutexes, parked tool calls are channels, abort is context cancellation, and every handler recovers panics. The race detector runs on the whole suite.

**D4. Subagent mark sponsorship is not ported.** TypeScript: `sponsorMarkRebuildForSession` lets a subagent hand a rebuild mark to the module instance that serves the session, because several module instances share one process. Port: PiG subagents are separate processes, so there is nothing to sponsor; the function has no counterpart. The matching test, `unit-session-cwd` "shares the store across module instances", has no Go counterpart either.

**D5. The model list is a static catalog registered in the factory.** TypeScript: the list comes from pi-ai's Anthropic catalog at run time and follows package updates. Port: `scripts/dev/gen-catalog.mjs` generates `internal/models/catalog_gen.go` from `@earendil-works/pi-ai` 1.2.0 and the provider is registered when the extension loads, with nothing registered at `session_start`. Reason: spike S1 showed that registration from `session_start` is too late for `defaultProvider`/`defaultModel` resolution at startup, so the catalog must be known in the factory. A new Claude model needs a regenerated catalog (see [../UPSTREAM.md](../UPSTREAM.md)).

**D6. AskClaude renders as lines.** TypeScript: `askclaude-ui.ts` builds pi-tui components (colour, "(to expand)" affordance). Port: PiG's `ToolDefinition.RenderCall`/`RenderResult` return lines, so the action summary is plain lines without colour, with "… (to expand)" as text, and truncation counts runes.

**D7. Summary attribution by the most recent pending compaction.** TypeScript: `session-cwd.ts` keys the pending summary session by the identity of the request's `AbortSignal`. Port: Go has no such identity, so a single most-recent-pending slot is set at `session_before_compact` / `session_before_tree` and the isolated summary query reads it; it is not cleared by the read (Pi makes several summary calls per operation, compaction makes two) and `forgetSummarySession` clears it only if no other session has been remembered since. With two sessions summarising at once in one process, the later one wins.

## Rulings that changed the plan

- Work in place on branch `feat/native-port`, no worktree: the repo is new and single-purpose.
- Spikes S1 and S3 ran headlessly through PiG's RPC/JSON modes, not the TUI.
- `Host` is passed per call and never stored on `Bridge`; `Deps` carries `AgentDir` and `ConfigDirName`. PiG's `sdk.Context` is per request, so a stored adapter would go stale and race. The fake PiG host of the spec is replaced by the `bridge.Host` interface and a `fakeHost`, because the SDK is confined to `extension.go`.
- `IsolatedStream` is exported from the start.
- The extension directory is `extensions/pig-claude-bridge`: PiG refuses `name_mismatch` when the directory differs from the identity given to `sdk.New`.
- `ProviderConfig` uses the TypeScript bridge's own values `baseUrl "claude-bridge"`, `apiKey "not-used"`, `api "claude-bridge"` (index.ts:2636-2644); they are parity and satisfy PiG's `api`/`baseUrl` requirements seen in S1.
- `Start` dispatches inbound control requests (notably `mcp_message`) while the `initialize` response is outstanding, and drops `control_response` frames whose `request_id` is not one of its own (the CLI echoes every response under `--verbose`). Both found by spike S2.
- `golang.org/x/text/unicode/norm` is the one dependency beyond the PiG SDK: cc-session-io NFC-normalises project paths and the standard library has no NFC; without it session paths of decomposed-Unicode directories (common on macOS) would not match.
- `MapPiToolNameToSdk` returns `(string, error)` and `ConvertPiMessages` gains an `error` result, carrying the TypeScript throw texts verbatim; the provider turns them into a stream error event. `ResolveOrDerive` likewise returns `(*Capture, error)`, `(nil, nil)` for an empty prompt. `ProjectPromptCapture` takes an explicit `debugLogPath` instead of a package-level hook.
- Where the plan contradicted the TypeScript, the TypeScript won: `describeRateLimitFailure(rej, failure)` does no text recognition, `RateLimitInfo` carries `Utilization` and `SurpassedThreshold`, and the notice logic runs at the `rate_limit_event` frame of the consume loop (the TypeScript timing).
- The golden comparison of the conversion checks each record's top-level key sequence exactly (struct order) and compares values canonically (recursively sorted keys, Go escaping). PiG hands the bridge decoded maps, so insertion order inside free-form JSON (tool inputs, results) cannot be preserved; Claude Code does not depend on it. The golden is therefore weaker than "byte for byte" for nested free-form values only.
- The live tests use the real Claude Code login (a scratch `CLAUDE_CONFIG_DIR` has no auth), with a scratch working directory and reads limited to the session files under that directory's project hash. The contract tests live inside the Go module (`internal/ccproto/live_contract_test.go`, build tag `live`) because they import an internal package; the live harness is a Go port under `tests/live/` (own zero-dependency module) with shell scripts under `scripts/live/`.
- A lock held without `defer` on a path the panic recovery depends on, a per-call goroutine on the no-run-signal path, and two leaked descriptors per spawned `claude` (the extension process lives across many turns) were treated as defects, not style, and fixed.

## Other differences, by area

### Config

- `askClaude.enabled: "false"` (a string) disables the tool. The TypeScript treats any non-empty string as truthy.
- A mistyped `askClaude.allowFullMode` denies full mode and a mistyped `askClaude.defaultMode` resolves to `none` (as the TypeScript, which denies on any non-`true` value); other mistyped keys are skipped where the TypeScript passes them through.
- The notice date comes from `time.Now()`; the config file is rewritten only to add the notice key, preserving key order.

### Prompt capture

- Custom prompt sections arriving in one system message are rendered in name order. PiG delivers decoded maps without key order; the TypeScript keeps insertion order. No test pins the two-section case.
- `PlaceCarriedAttachments` returns `(attachments, skipped)`, as the TypeScript object does. The audit-record test appends uuid-less lines directly and the test helper uses a fixed `/work` project path.
- Two Pi-builder-dependent prompt-capture tests became literal-string assertions (the TypeScript tests depended on Pi's prompt builder). `CollectPromptSkills` guards against inheritance cycles by visiting each capture once (`visited` set), listing ancestors first and each skill file once.
- Startup notice is not gated on TUI mode.

### Tool schemas and the MCP server

- Advertised tool schemas serialise `properties` (and combinator-description key lists) in sorted order because `InputSchema` is `map[string]any`; Pi declares them in insertion order. `required` keeps its order.
- Invalid `tools/call` params carry a fixed description instead of the SDK's zod text.

### Session files and logs

- `syncSharedSession` takes `host` and `modelID`.
- The `APPEND` log line prints the advanced cursor (TypeScript prints the pre-advance cursor).
- The `[moduleInstanceId]` bracket is dropped from log lines (the TypeScript `diag/*.mjs` parsers expect it); the deadlock-check regex was adapted. The `imported roles:` debug line and the sanitized-id list are dropped.
- Stderr tail counts runes where the SDK counts UTF-16 units.
- `pending_permission_requests` redelivery is not implemented: it is never sent without `canUseTool`.
- The package is Unix-only (`syscall.WaitStatus`).

### Provider stream and notices

- `Details.executionTime` is an `int64`.
- The "Claude Code returned an error result" replacement is unreachable, as in the TypeScript (the loop stops at the error result). An error result drops the mirror, while a process exit or panic marks rebuild (the TypeScript drops the mirror on exit too).
- Abort lifetime is proven by the live smoke's tool round trip. `reportHeldRateLimit` runs at the start of each call rather than inside the consume loop.
- Prompt-too-long: the CLI exits 1 with empty stderr after the input ends; the bridge surfaces the `is_error` text (`resultErrorText`), not the bare exit.

### Rate limits

- Headers go to the debug log only (D1).
- The reset time renders as en-US `3:04:05 PM` with a plain space, where modern Node's `toLocaleTimeString` uses U+202F.
- The `onResponse`-throw case has no PiG counterpart.

### Isolated queries

- The executable is resolved process-wide. The TypeScript reloads config per summary cwd, so a project-level `pathToClaudeCodeExecutable` cannot differ per session here.
- A missing binary fails the call with a notice (D2).
- Input is ended right after the user write on the isolated path.

### AskClaude

- `enabled: "false"` as above; rendering as lines (D6); rune-based truncation; `Definition(b, cfg, agentDir, host, requestContext)` has the signature the import cycle forces; the deprecation notice date comes from `time.Now()`.
- An aborted call differs as described under the provider stream.

### Protocol client

- There is no Node Agent SDK: the client spawns `claude` with `--permission-mode bypassPermissions` and the SDK's argv, and speaks the newline-delimited JSON control protocol. Argv and the `initialize` request are pinned by a snapshot recorded from the Node SDK 0.3.310 (`scripts/dev/record-sdk-argv.mjs`).
- Between turns no Claude Code process is alive in either bridge: the query ends at the end of a turn and the next turn resumes the session. PiG's `new_session` replaces the extension runner (D70) and the pre-`/new` runner was observed gone after the next prompt.

### Live-test findings

- The live midturn compaction test caught a parity gap: the port passed the bare tag `session_compact` and dropped the event's `reason` and `willRetry`. Fixed: the tag is `session_compact:<reason>:willRetry=<bool>` as in the TypeScript, unit-tested, and the midturn test passed on its rerun. `session_tree` is bare in both.
- The Go port of `int-session-rebuild` lives beside the contract tests (`internal/ccproto/live_rebuild_test.go`), not under `tests/live`. Its first case (open, clear, re-add) is covered by `TestResumeRereadsTheJSONLFromDiskOnEveryCall` because `ccsession` has no Open+Clear primitive.
- `int-attachment-rebuild` and `int-parallel-tool-results` are not RPC tests; their bodies are `-tags live` tests in `internal/ccproto` and `tests/live` runs them.
- The alternative provider for the AskClaude, image and session-resume tests must come from `CLAUDE_BRIDGE_TESTING_ALT_PROVIDER`, `CLAUDE_BRIDGE_TESTING_ALT_MODEL` and that provider's API-key variable, because every live run uses a fresh `PIG_HOME`/`PIG_CODING_AGENT_DIR` (the TypeScript suite used the real agent dir). Without them those checks skip.
- The deadlock-check log pattern, the shutdown test (a third case for the stdin close) and the subagent test (our own agent definition, run through `@tintinweb/pi-subagents`) are adapted as listed in [verification.md](verification.md).

## Pi-only tests that are not ported

The TypeScript suite has 45 `tests/unit-*.mjs` files. Those that test Pi-only wiring with no PiG counterpart are not ported:

- `unit-provider-registration` and `unit-pi-streamfn-inventory` (Pi's `registerProvider` and stream-function wiring; PiG registers the provider through the SDK factory, covered by `extension_test.go` and `pig install --validate-only`).
- `unit-session-cwd` "shares the store across module instances" (D4).
- Four `unit-queued-steer` cases that need a live SDK loopback.
- The `sponsorMarkRebuildForSession` cases (D4).

The remaining unit tests are ported to Go with the same fixtures; where a test reached into `index.ts` through its `__test` seams, `internal/bridge` has an equivalent internal seam.
