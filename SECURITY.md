# Security

## What the extension does

**It spawns Claude Code.** Each turn it runs `claude` (from `PATH` or `provider.pathToClaudeCodeExecutable`) as a child process with `--permission-mode bypassPermissions` and speaks its stdio protocol. Bypassing Claude Code's own permission prompts is deliberate: PiG's tool loop is in control. Claude Code's built-in tools are turned off and PiG's tools are served to it through an in-process MCP server, so every tool call is executed by PiG under PiG's own approvals. The exception is the opt-in AskClaude tool, which runs Claude Code with its own native tools except those the chosen mode blocks (in `full` mode none is blocked, so Claude Code can change files without PiG's approvals); `askClaude.allowFullMode: false` locks that out. The child is started with `strictMcpConfig` on by default, so MCP servers from `~/.claude.json` and `.mcp.json` do not load.

**It runs `claude --version`** to check the supported minimum (2.1.310). A `pathToClaudeCodeExecutable` ending in `.js`, `.mjs` or `.ts` is run through `node`, for the version check and the query alike.

**It kills its Claude Code children** when PiG sends `session_shutdown` (exit, `/new`, `/resume`, `/fork`, `/clone`, reload). A PiG killed with SIGKILL cannot do this.

**It writes:**

- Claude Code session files under `~/.claude/projects/<project hash>/` (or `$CLAUDE_CONFIG_DIR/projects/...`): a mirror of PiG's conversation, so Claude Code can resume it. They contain your prompts, tool calls and tool results in clear text, like any Claude Code session.
- With `CLAUDE_BRIDGE_DEBUG=1` only: the bridge debug log `claude-bridge.log` and the per-query `cc-cli-logs/` in PiG's agent directory (or beside `CLAUDE_BRIDGE_DEBUG_PATH`). They can contain prompt text, tool results and, when the bridge rejects a prompt, the full system prompt. Do not attach them to a public issue without reading them first.
- Always, debug or not: `claude-bridge-diag.log` in the agent directory, one JSON line per "should never happen" path (a system prompt the bridge cannot account for, an empty prompt). Entries hold counts, message roles and error texts; a prompt-capture error quotes the part of the system prompt where it diverged.
- With `CLAUDE_BRIDGE_RECORD_STREAM=<path>` only: every frame Claude Code sends, appended to that path, which can be anywhere. The frames carry the whole conversation as Claude Code sees it: prompts, replies, thinking, tool calls and tool results.
- `claude-bridge.json` in the agent directory: only the notice flags `startupNoticeShown` and `askClaudeDeprecationShown`, every other key preserved. An unparseable file is left untouched.

**It reads:** `claude-bridge.json` in the agent directory and the project config directory, the session files it wrote (to verify them and to carry file attachments across a rebuild), and the output of `claude --version`.

**It never handles API keys.** Authentication is Claude Code's own login. Note that an exported `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` is inherited by the Claude Code child and redirects it; see the README.

**It makes no network connection of its own.** All traffic is Claude Code's, started with `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` as the TypeScript bridge sets it, so its non-essential traffic (update checks, the MCP registry, telemetry) stays off.

## Supported versions

Only the latest release.

## Reporting a vulnerability

Please report privately through GitHub security advisories on [balcsida/pig-claude-bridge](https://github.com/balcsida/pig-claude-bridge/security/advisories/new), not in a public issue. Include the version, what you did and what you saw; leave out session files and logs unless they are needed, and redact them if so.
