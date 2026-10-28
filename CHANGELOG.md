# Changelog

## Unreleased

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
