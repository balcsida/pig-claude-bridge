# pig-claude-bridge

A native Go extension for [PiG](https://github.com/MichaelKinsy/PiG) that integrates Claude Code. It is a port of [pi-claude-bridge](https://github.com/elidickinson/pi-claude-bridge) (TypeScript, for Pi) with the same behaviour, and it speaks Claude Code's stdio protocol itself instead of loading the Node Agent SDK. Pi-claude-bridge was originally based on [claude-agent-sdk-pi](https://github.com/prateekmedia/claude-agent-sdk-pi) by Prateek Sunal.

1. **Provider**: use Opus/Sonnet/Haiku as models in PiG, with all tool calls flowing through PiG's tool loop
2. **AskClaude tool**: delegate tasks or questions to Claude Code when using another provider (opt-in, deprecated upstream)

**FYI:** Anthropic [announced and then unannounced](https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan) a change to how tools that use the Agent SDK are billed. Claude Code started by this extension currently uses your regular subscription quota, like Claude Code itself.

## Install

```sh
pig install npm:pig-claude-bridge
```

Update it with `pig update npm:pig-claude-bridge`. To pin a release, install `npm:pig-claude-bridge@0.2.0`; to follow `main` instead of releases, install `git:github.com/balcsida/pig-claude-bridge`.

Requirements:

- PiG 0.5.3 or newer.
- Go 1.26 or newer. PiG builds the extension from source the first time it loads it.
- Claude Code 2.1.310 or newer, logged in, as `claude` on `PATH` or at the path in `provider.pathToClaudeCodeExecutable`. An older binary works but shows a one-time notice, because the port is verified against 2.1.310.
- macOS or Linux. Windows is untested (the package is Unix-only; see [docs/compatibility.md](docs/compatibility.md)).

To try it without installing: `pig -e /path/to/pig-claude-bridge`.

## Provider

Use `/model` to select any Claude model in the catalog, e.g. `claude-bridge/claude-opus-5` or `claude-bridge/claude-haiku-4-5`.

Behind the scenes, PiG's tools are bridged to Claude Code but everything works like normal in PiG. Bash commands get Claude Code's 120-second default timeout since PiG's bash has none. Skills are forwarded to Claude Code's system prompt, and steering mid-turn reaches Claude at the next tool boundary.

The model list is a static table generated from the Anthropic catalog of `@earendil-works/pi-ai` 1.2.0 (`scripts/dev/gen-catalog.mjs`) and registered when the extension loads; it does not follow later catalog updates. Dated snapshot ids are not shown. See [docs/compatibility.md](docs/compatibility.md) (D5).

**1M context:** Fable 5/5.1, Opus 5.5/5/4.8/4.7, and Sonnet 5.5/5 get 1M context. Opus 4.6 gets 1M only on a Max plan or with Extra Usage, and Sonnet 4.6 only with Extra Usage: set `provider.plan` and/or `provider.longContextExtraUsage` as described in [Configuration](#configuration).

## AskClaude Tool

**Deprecated upstream:** AskClaude is an opt-in legacy tool. Set `askClaude.enabled` to `true` (see [Configuration](#configuration)) to use it. A PiG subagent extension can recreate similar functionality.

## Configuration

Config file `claude-bridge.json`, in two places (the project file is merged over the global one, key by key):

- global: PiG's agent directory, `~/.pig/agent` by default (`$PIG_HOME/agent`, or `PIG_CODING_AGENT_DIR`). With `PIG_USE_PI_DIRS=1` PiG shares Pi's tree and this is `~/.pi/agent` (or `PI_CODING_AGENT_DIR`).
- project: `<project>/.pig/claude-bridge.json`; under `PIG_USE_PI_DIRS=1`, `<project>/.pi/claude-bridge.json`.

The extension asks PiG for both directories; nothing is hard-coded. A missing or unparseable file counts as empty; with `CLAUDE_BRIDGE_DEBUG` set, a parse failure is written to the debug log. A key with a value of the wrong type is ignored, except `askClaude.allowFullMode` and `askClaude.defaultMode`, which fail closed (full mode denied, mode `none`).

```json
{
  "askClaude": {
    "enabled": true,
    "allowFullMode": true,
    "defaultIsolated": false,
    "description": "Custom tool description override"
  },
  "provider": {
    "plan": "max",
    "longContextExtraUsage": false,
    "strictMcpConfig": true,
    "pathToClaudeCodeExecutable": "/home/you/.nix-profile/bin/claude"
  }
}
```

`askClaude`:
- `enabled`: register the AskClaude tool (default `false`).
- `name`: override the tool's name (default `"AskClaude"`)
- `label`: override the label (default `"Ask Claude Code"`)
- `description`: override the tool description
- `defaultMode`: `"read"` (default), `"none"`, or `"full"`
- `defaultIsolated`: start each call in a fresh session (default `false`)
- `allowFullMode`: allow `mode: "full"`; set `false` to lock it out
- `appendSkills`: forward PiG's skills block into the system prompt (default `true`)

`provider`:
- `plan` (default `"pro"`): set to `"max"` if you have a Max (or Team Premium/Enterprise) Anthropic plan. This enables Opus with 1M context.
- `longContextExtraUsage`: set to `true` to enable 1M context models even if they cost money through Extra Usage on your plan. It enables Sonnet 4.6 with 1M on every plan and Opus 4.6 with 1M on Pro.
- `forceTwoHundredK`: array of model ids to pin to 200K context (bare id, no `[1m]` suffix). Use if the catalog declares a model at 1M but Claude Code won't serve it on your plan.
- `strictMcpConfig`: block MCP servers from `~/.claude.json` / `.mcp.json` (default `true`). Cloud MCP (Gmail/Drive via claude.ai OAuth) is always blocked.
- `autoMemoryEnabled`: enable Claude Code's auto-memory system (default `false`)
- `pathToClaudeCodeExecutable`: path to the `claude` binary, used instead of the one on `PATH`. A path ending in `.js`, `.mjs` or `.ts` is run through `node`, as the Agent SDK does.

**Notices:** the startup notice, shown at the first bridge turn in the TUI only (it stays pending in RPC, JSON and print mode), points out when `provider.plan` is unset and records `startupNoticeShown` (a date) in the global config. A deprecation notice on first use of AskClaude records its display date as `askClaudeDeprecationShown`. The extension rewrites the global file only to add these keys and leaves every other key as it was; an unparseable global file is left alone.

## Why Go

The extension is a single Go program run by PiG's extension runner. It needs no Node, no Agent SDK and no package install at run time. Measured idle, before the first prompt (`footprint -p`, physical footprint, descendants of the PiG/Pi process):

| side | MiB | processes |
|---|---|---|
| pig 0.5.3 + this extension (pig 26 + runner 6) | **32** | 2 |
| pi 1.4.0 + TypeScript pi-claude-bridge | **205** | 1 |

Neither side spawns Claude Code until the first prompt. Method and commands: [docs/verification.md](docs/verification.md#footprint-task-25).

## Tests

Offline, quota-free:

```sh
cd extensions/pig-claude-bridge && gofmt -l . && go vet ./... && go test -race ./...
```

Live tests spawn real `pig` and Claude Code processes and spend a little quota; run them by hand, never in CI. Claude Code must be logged in (`claude` on `PATH`, the real `~/.claude`, which must be writable: a sandbox that blocks it makes `--resume` fail with `No conversation found with session ID`). Every live run uses a fresh `PIG_HOME`/`PIG_CODING_AGENT_DIR`, so the alternative provider for the AskClaude, image and session-resume checks must be exported in your shell, together with that provider's API-key variable:

```sh
export CLAUDE_BRIDGE_TESTING_ALT_PROVIDER=<provider>
export CLAUDE_BRIDGE_TESTING_ALT_MODEL=<model>
export <PROVIDER_API_KEY_VARIABLE>=...
```

Nothing is read from `.env*` files. Without these variables those checks print `SKIP`. Then:

```sh
scripts/live/verify.sh          # quick gate: validate-only, smoke turn, idle footprint
scripts/live/run-all.sh         # everything, with a results table and the deadlock check
cd tests/live && go test -race -tags live -count=1 -v ./...
cd extensions/pig-claude-bridge && go test -race -tags live ./internal/ccproto/   # contract tests
```

Details, versions and recorded results are in [docs/verification.md](docs/verification.md).

## Debugging

Set `CLAUDE_BRIDGE_DEBUG=1` to enable debug output:

- **Bridge log** at `claude-bridge.log` in PiG's agent directory: provider calls, session sync decisions, tool results, Claude Code stderr. Override the location with `CLAUDE_BRIDGE_DEBUG_PATH`.
- **Per-query Claude Code logs** at `cc-cli-logs/<timestamp>-<tag>-<seq>.log` beside the bridge log (in `dirname(CLAUDE_BRIDGE_DEBUG_PATH)` when that is set): the subprocess's own debug stream; tag is `provider` or `askclaude`.

Both can contain prompt text; see [SECURITY.md](SECURITY.md). When filing a bug about a session-resume failure (e.g. "No conversation found"), the most useful attachments are the `syncResult:` lines from the bridge log plus the matching `cc-cli-logs/` file for the failing query. The `syncResult:` lines are kept in the same format as the TypeScript bridge's.

## Compatibility with other extensions

### Which injection routes reach Claude Code

The bridge forwards PiG's structured prompt parts (project context files, skills, custom prompt, appended instructions, and custom prompt sections) and drops the rest.

| Route | Reaches Claude Code |
|---|---|
| `before_agent_start` -> `message` | Yes, as literal prompt text in the user turn |
| `context` editing the last user message | Yes, as literal prompt text |
| `--append-system-prompt` | Yes, with the appended instructions |
| `context_with_system` editing the system message | Yes when it wraps PiG's prompt; the turn fails when it replaces one |
| `before_agent_start` -> `systemPromptOptions.sections` | Yes, one `<name>` block per section (several sections in one system message are written in name order) |
| `before_agent_start` -> `systemPrompt` | No, dropped |

These rows are the TypeScript bridge's measured results (`diag/capture-proxy.mjs`), carried over; this port reproduces the same capture logic, but the request-body measurement was not repeated under PiG.

System-prompt edits work by *wrapping*: replacing PiG's prompt leaves the bridge with nothing to match, so it refuses the turn rather than send Claude Code a request missing your context files, skills and custom instructions. The error names the closest known prompt and where it diverged. To add instructions, use `message`, `context`, or a system-message edit that keeps PiG's prompt intact. When the result still contains the built-in system prompt text of Pi or PiG, or the two documentation paths Anthropic looks for, the bridge stops the turn instead of sending it, since Anthropic may bill such requests as Extra Usage. `CLAUDE_BRIDGE_DEBUG=1` writes the full prompt to the bridge log when this happens.

### Hooks written for Claude Code

`~/.claude/settings.json` hooks fire inside bridge turns, so a hook injecting Claude-specific guidance duplicates what PiG's extensions already provide. The bridge sets `PI_CODING_AGENT=true` for the Claude Code child; a hook can skip itself on that:

```sh
[ -n "$PI_CODING_AGENT" ] && exit 0
```

Hooks do not fire on isolated summary queries.

### Compaction and branch summaries

PiG runs compaction and branch summaries natively. The bridge routes their `cacheRetention: "none"` calls to isolated Claude Code queries, leaving the live session untouched until PiG completes the history rewrite.

### Plan usage for footers and quota extensions

Claude Code sends a `rate_limit_event` when a usage window changes. The TypeScript bridge passes it to Pi's `after_provider_response` event as `anthropic-ratelimit-unified-*` headers. PiG's host does not emit that event, so this port writes the headers to the debug log only; the user-visible rate-limit notices stay. The bridge also shows the plan's limits in PiG's footer status bar, for example `Claude 5h 6% ↻3h · 7d 76% ↻8h · Fable 99% ↻8h · reset available`. On each event it asks Claude Code for its `/usage` rows (`get_usage`), which name per-model limits; when that fails it shows the event's own windows. This is difference D1 in [docs/compatibility.md](docs/compatibility.md).

## Known issues

**A session rebuild re-sends the whole conversation.** The bridge rewrites Claude Code's session from PiG's history whenever the two diverge (after an abort, `/compact`, tree navigation, an API error, or on returning to a session), and the next request usually misses the prompt cache for everything past the system prompt. Abort-heavy sessions cost noticeably more.

**Files Claude Code edits are not carried across a rebuild.** The edit itself survives in the history as a tool call and result; what is lost is the post-edit file snapshot. `@file` expansions *are* carried.

**System prompt changes mid-session may not reach the model.** Project context (AGENTS.md/CLAUDE.md), skills, and extension-written instructions are captured on the first request and reused on resume. This keeps the cached prefix stable, but later changes may not take effect until a rebuild or compaction. Start a new session if updated instructions must take effect immediately.

**Exported Anthropic environment variables override the Claude Code child (pi-claude-bridge issue #107).** An exported `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, or `ANTHROPIC_AUTH_TOKEN` redirects Claude Code to that gateway and every turn fails with its auth error. Unset them for the PiG process.

## More

- [docs/compatibility.md](docs/compatibility.md): pins, and every difference from the TypeScript bridge with its reason
- [docs/verification.md](docs/verification.md): contract tests, live integration results, footprint, cross-resume
- [UPSTREAM.md](UPSTREAM.md): which TypeScript source each Go file ports, and how to bump the pins
- [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md), [SECURITY.md](SECURITY.md), [CHANGELOG.md](CHANGELOG.md)

MIT licensed, see [LICENSE](LICENSE).
