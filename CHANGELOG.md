# Changelog

## Unreleased

- ci: the release workflow no longer passes an `NPM_TOKEN` secret to `npm publish`. The package publishes through npm trusted publishing (OIDC); the secret was never set, so the variable came through empty.

## 0.5.0 - 2026-11-09

Catches up with pi-claude-bridge 0.11.0 (acbe45b). Every behaviour listed here is the TypeScript's unless marked port-only; docs/compatibility.md lists what is deliberately different.

- feat(bridge): session sync compares message content instead of counting messages (pi-claude-bridge #222). An unchanged history reuses the Claude Code session; a rewind, edit, abort, failed turn or restart forks or trims it at the last valid cut and imports only what follows; a full import happens only when the model, the system prompt append or the cwd changed. Live queries record which pi messages Claude Code's file covers and commit that on completion, failure, abort or discard, so prompt cache and earlier thinking survive what used to be a rebuild.
- feat(bridge): the session pointer is persisted under `<agentDir>/claude-bridge/sessions/` (user-only, atomic), so a restart or `/reload` forks the earlier Claude Code session instead of re-sending the conversation. A `/fork` or clone inherits its parent's pointer. Mirrors survive switching sessions and shutdown.
- feat(bridge): a parked query is closed, its progress kept, when the same session sends a fresh prompt, when its history was edited under it (a content check before every tool-result delivery), or at `session_shutdown` (pi-claude-bridge #210); tool results route only to their own pi session's query (#111); tool calls take the arguments Claude Code converted (#242).
- fix(convert): a tool result with no output becomes the placeholder Claude Code substitutes live, `is_error` is written only when true (#227), and a tool result Claude Code already recorded (an oversized output saved to a file, an image) is reused verbatim in a rewrite (#207).
- fix(provider): Claude Code loads user settings only (`settingSources: ["user"]`), so a repo's `.claude/settings.json` and `settings.local.json` never run their hooks, `apiKeyHelper` or `env` (#198). The project `.pig/claude-bridge.json` is no longer read: a cloned repo could point the bridge at its own Claude Code executable or turn on metered billing.
- feat(provider): pi's per-request provider env (a credential's `env`, or the caller's) applies to the Claude Code child; an empty value unsets a variable, and `CLAUDE_CONFIG_DIR`, `HOME` and `USERPROFILE` are refused.
- fix(bridge): the rate-limit notice says when requests are billed to Extra Usage (#201, #213); the non-streaming fallback keeps later blocks of the same message (87f67d1); `bash` runs with a 120 s default timeout under claude-bridge models, applied in the `tool_call` hook so pi's history keeps the arguments Claude Code sent (#203).
- refactor(convert): the dropped-content counters and the builtin-name path for unserved tools are gone with upstream; the golden conversion is regenerated from upstream at acbe45b. `internal/digest` is pinned to the TypeScript's hashes by tests/fixtures/message-digests.json.
- docs: AskClaude stays in this package, off by default (upstream moved it to `pi-claude-bridge-askclaude`); `CLAUDE_BRIDGE_TRACE_DIR` is not ported.

## 0.4.0 - 2026-11-09

- build: the PiG Go SDK pin moves from v0.5.3 to v0.7.0 (PiG 0.7.0); no API the bridge uses changed. Verified against Claude Code 2.1.319 (`ccproto.MinVersion` stays 2.1.310).
- feat(ccproto): Claude Code runs with feature-flag evaluation off (`CLAUDE_CODE_DISABLE_FEATURE_FLAGS=1`): every GrowthBook flag and experiment (`tengu_*`) takes its built-in default and the assignments cached in `~/.claude.json` are not read. `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, which the bridge already set, implies this in 2.1.319; the explicit variable pins it, and `TestDisableFeatureFlagsTurnsFlagEvaluationOff` fails if a Claude Code release stops honouring it. Server-side changes to a model are outside the client's reach.
- fix(ccproto): ported from pi-claude-bridge 0.11.0: Claude Code's "The user hasn't heard from you in a while" reminder is off (`CLAUDE_CODE_SILENT_TURN_REMINDER=0`), MCP tool descriptions are cut at 16384 chars instead of 4096 (`CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH`), and a resumed session that ends at a tool result no longer gets a "Continue from where you left off." message ahead of PiG's prompt (`CLAUDE_CODE_RESUME_INTERRUPTED_TURN_MAX_AGE_MS=1`, pi-claude-bridge #145).
- fix(ccproto): when PiG itself runs inside a Claude Code session, that session's markers (`CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`, `CLAUDE_ENV_FILE`, the SDK entrypoint and version, and the rest) are dropped from the child environment (pi-claude-bridge #198).
- fix(provider): Claude Code runs with its default output style (`outputStyle: "default"` in `--settings`), so a user's `outputStyle` in `~/.claude/settings.json` no longer competes with PiG's system prompt (pi-claude-bridge #220).
- fix(provider): thinking off is honoured: with no reasoning level the provider and the isolated summary pass `--thinking=disabled` on reasoning models, except where the model's `thinkingLevelMap` marks `off` unsupported (pi-claude-bridge #152).

## 0.3.0 - 2026-11-01

- feat(provider): when `provider.plan` is unset, the plan is detected from `claude auth status --json` at load, so a Max, Ultra, Team or Enterprise subscription gets Opus 4.6 at 1M without configuration and the startup notice no longer asks.
- fix(models): Opus 4.6 at 1M is enabled on the `team` and `enterprise` plans (Standard and Premium seats alike), as Claude Code's docs say it is included there.
- feat(provider): `/claude-fast [on|off]` toggles Claude Code's fast mode for the session, and `provider.fastMode` turns it on by default; the bridge passes `fastMode: true` in Claude Code's `--settings`, which an SDK session needs (`sdk_opt_in_required` otherwise).
- feat(bridge): the footer status shows the plan from session start and `fast` while fast mode is on, ahead of the usage limits (`Claude Max · fast · 5h 6% ↻3h · 7d 76% ↻8h`).

## 0.2.1 - 2026-10-30

- chore(release): republish 0.2.0's changes; the `v0.2.0` tag predates the release workflow, so 0.2.0 never reached npm.

## 0.2.0 - 2026-10-28

- feat(bridge): the footer status bar shows the Claude plan's usage limits, including per-model ones such as Fable, and an unused rate-limit reset (`Claude 5h 6% ↻3h · 7d 76% ↻8h · Fable 99% ↻8h · reset available`). Each `rate_limit_event` asks the live CLI for its `get_usage` rows and falls back to the event's windows.
- docs(readme): install from npm (`pig install npm:pig-claude-bridge`), with the update command and a `main` alternative.
- build: the package has a `description`, which PiG's package catalog requires.
- ci: a signed `v*.*.*` tag on `main` publishes the package to npm with provenance and creates the GitHub release.

## 0.1.0 - 2026-10-26

First release candidate: a native Go port of [pi-claude-bridge](https://github.com/elidickinson/pi-claude-bridge) at commit 6aa276a for PiG 0.5.3, with Claude Code 2.1.310 and cc-session-io 0.4.0 as the reference. Every difference is listed in docs/compatibility.md.

- feat(provider): the `claude-bridge` provider serves Claude models from a static catalog registered when the extension loads, with 1M-context eligibility, `[1m]` suffixes and the `plan`, `longContextExtraUsage` and `forceTwoHundredK` settings.
- feat(ccproto): own client for Claude Code's stdio protocol (spawn, argv, environment, `initialize`, inbound `mcp_message`, interrupt, close), with the argv and `initialize` request pinned to a snapshot recorded from Agent SDK 0.3.310.
- feat(ccsession): Go port of the cc-session-io subset the bridge uses: project path hashing, records, chained writes, tool-pairing repair, import with attachments and verification of written sessions.
- feat(convert): conversion of PiG history to Claude Code session records, tool-result extraction, carried attachments, skills block and project context, checked against a golden file.
- feat(mcptools): in-process MCP server over `mcp_message` that serves PiG's tools to Claude Code, with root-schema flattening for `anyOf`/`oneOf`/`allOf` tools.
- feat(promptcapture): system prompt recording and rejection rules with the TypeScript error texts, so a foreign prompt is refused instead of sent.
- feat(bridge): per-session state and sync decisions (clean start, reuse, rebuild), query lifecycle, parked tool calls, steering, abort, stream mapping, usage, rate-limit notices and isolated queries for compaction and branch summaries.
- feat(askclaude): the opt-in AskClaude tool with read, full and none modes, isolated and remembered sessions, rendered as lines.
- feat(config): `claude-bridge.json` in PiG's agent directory and project config directory, with the TypeScript keys and defaults, `CLAUDE_BRIDGE_DEBUG` and `CLAUDE_BRIDGE_DEBUG_PATH`.
- fix(bridge): tag a compaction rebuild with the reason and retry state (`session_compact:<reason>:willRetry=<bool>`), as the TypeScript does.
- fix: final review wave: an abort now reaches Claude Code's start-up and `claude --version` is bounded; `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` is sent to Claude Code as the TypeScript does; panics in every goroutine running bridge or handler code are contained (a panicking control-request handler gets an error response); live Claude Code children are killed at `session_shutdown`; a `.js`/`.mjs`/`.ts` executable runs through `node`; the isolated summary's debug tag is `compact-summary`; the startup notice waits for the TUI; a failed compaction forgets its pending summary cwd.
- fix(config): a `null` `provider` or `askClaude` section in the project file keeps the global section, keys match exactly, and a file that fails to parse is reported in the debug log.
- fix(models): the generator sorts the catalog by code point, and returned models no longer share nested maps with the catalog.
- fix(ccsession): records with other field types are kept, a `null` line is reported as malformed, an empty `tool_use` id is paired as cc-session-io pairs it, and the Claude config dir falls back to the account's home directory when `HOME` is unset.
- fix(convert): assistant content given as a list of maps is converted, and an empty thinking provider is kept, as in the TypeScript.
- fix(promptcapture): the capture-miss diagnostic runs outside the registry lock, so a callback that reads the registry cannot deadlock.
- fix(mcptools): a `tools/call` to a tool without a handler gets a JSON-RPC error instead of a panic.
- fix(ccproto): frames with an oddly typed field are kept; `remote_*` control requests are left unanswered, as the SDK leaves them; the identity variables follow the SDK's exact-key rules; `Close` kills the child when its context ends; an unknown signal is named by its number.
- fix(bridge): an abort during the `claude --version` probe ends the turn as aborted, and an abort wins over a process exit; usage is priced with the model the query started with; a rebuild mark set while a sync runs is kept; an init frame without a session id keeps the captured one; a cancelled tool handler removes only its own pending call; the run binder can no longer leave a registration behind; a rate-limit notice without a host is dropped; the diagnostic entries carry their full fields again; a panic in the Claude CLI stderr forwarder is logged.
- fix(askclaude): a mid-run abort returns the partial text gathered so far, as the TypeScript does, and the progress ticker stops even when the call panics.
- fix: the compaction debug tag renders a missing reason or retry flag as `undefined`, as the TypeScript's template string does.
- fix(tests): the live scripts fail instead of hanging or staying silent (a row for a `go test` build failure, a bounded `verify.sh`, exact SKIP reasons), the cross-resume chain check allows one root only and the script deletes only its own session, and a timed-out tool-message subtest can no longer report into a finished test.
- test: offline unit tests for every package, a live contract suite against the installed Claude Code, live integration tests and scripts under `scripts/live/` and `tests/live/`, and `scripts/dev/` generators for the argv snapshot, the conversion golden and the model catalog.
- docs: README, compatibility, provenance (UPSTREAM.md), third-party notices, security policy and verification results.
- ci: format, vet, race tests, validate-only install, compile checks for the live-tagged modules and offline self-tests of the live scripts.
- ci: the format steps name the unformatted files, and the fake Claude CLI under `testdata` is vetted.
