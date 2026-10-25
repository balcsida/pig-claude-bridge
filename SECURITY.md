# Security

## What the extension does

**It spawns Claude Code.** Each turn it runs `claude` (from `PATH` or `provider.pathToClaudeCodeExecutable`) as a child process with `--permission-mode bypassPermissions` and speaks its stdio protocol. Bypassing Claude Code's own permission prompts is deliberate: PiG's tool loop is in control. Claude Code's built-in tools are turned off and PiG's tools are served to it through an in-process MCP server, so every tool call is executed by PiG under PiG's own approvals. The exception is the opt-in AskClaude tool, which runs Claude Code with its own native tools except those the chosen mode blocks (in `full` mode none is blocked, so Claude Code can change files without PiG's approvals); `askClaude.allowFullMode: false` locks that out. The child is started with `strictMcpConfig` on by default, so MCP servers from `~/.claude.json` and `.mcp.json` do not load.

**It runs `claude --version`** to check the supported minimum (2.1.310).

**It writes:**

- Claude Code session files under `~/.claude/projects/<project hash>/` (or `$CLAUDE_CONFIG_DIR/projects/...`): a mirror of PiG's conversation, so Claude Code can resume it. They contain your prompts, tool calls and tool results in clear text, like any Claude Code session.
- With `CLAUDE_BRIDGE_DEBUG=1` only: the bridge debug log `claude-bridge.log` and the per-query `cc-cli-logs/` in PiG's agent directory (or beside `CLAUDE_BRIDGE_DEBUG_PATH`). They can contain prompt text, tool results and, when the bridge rejects a prompt, the full system prompt. Do not attach them to a public issue without reading them first.
- `claude-bridge.json` in the agent directory: only the notice flags `startupNoticeShown` and `askClaudeDeprecationShown`, every other key preserved. An unparseable file is left untouched.

**It reads:** `claude-bridge.json` in the agent directory and the project config directory, the session files it wrote (to verify them and to carry file attachments across a rebuild), and the output of `claude --version`.

**It never handles API keys.** Authentication is Claude Code's own login. Note that an exported `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` is inherited by the Claude Code child and redirects it; see the README.

**It makes no network connection of its own.** All traffic is Claude Code's.

## Supported versions

Only the latest release.

## Reporting a vulnerability

Please report privately through GitHub security advisories on [balcsida/pig-claude-bridge](https://github.com/balcsida/pig-claude-bridge/security/advisories/new), not in a public issue. Include the version, what you did and what you saw; leave out session files and logs unless they are needed, and redact them if so.
