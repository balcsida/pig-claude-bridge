# Changelog

## Unreleased

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
- test: offline unit tests for every package, a live contract suite against the installed Claude Code, live integration tests and scripts under `scripts/live/` and `tests/live/`, and `scripts/dev/` generators for the argv snapshot, the conversion golden and the model catalog.
- docs: README, compatibility, provenance (UPSTREAM.md), third-party notices, security policy and verification results.
- ci: format, vet, race tests, validate-only install, compile checks for the live-tagged modules and offline self-tests of the live scripts.
