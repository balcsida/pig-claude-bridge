// Records what the Node Agent SDK 0.3.310 emits for the bridge's options and writes
// internal/ccproto/testdata/sdk-argv-snapshot.json. Needs network (npm install) and Node.
//
//   node /path/to/scripts/dev/record-sdk-argv.mjs <this repo path>
//
// The SDK is installed into a temp dir (removed afterwards) and imported from there, so the
// working directory does not matter. Options mirror pi-claude-bridge src/index.ts:2030-2096
// (provider) and :590-613 (isolated summary).
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const repo = resolve(process.argv[2] ?? "");
if (!process.argv[2]) throw new Error("usage: record-sdk-argv.mjs <this repo path>");
const recorder = join(dirname(fileURLToPath(import.meta.url)), "recorder-claude.mjs");
const tmp = mkdtempSync(join(tmpdir(), "sdk-argv-"));

try {
	writeFileSync(join(tmp, "package.json"), '{"type":"module","private":true}');
	execFileSync("npm", ["install", "--ignore-scripts", "--no-audit", "--no-fund", "--omit=optional",
		"@anthropic-ai/claude-agent-sdk@0.3.310"], { cwd: tmp, stdio: "inherit" });
	const { query, createSdkMcpServer } = await import(
		pathToFileURL(join(tmp, "node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs")).href);

	const CC_CHILD_ENV = { ENABLE_CLAUDEAI_MCP_SERVERS: "0", DISABLE_AUTO_COMPACT: "1", CLAUDE_CODE_ELEGANT_MEADOW: "0" };
	// Not process.env: the recording must not depend on the invoking shell (a Claude Code or pi session
	// leaks CLAUDE_* and PI_* variables). PI_CODING_AGENT is what pi sets for its children.
	const env = { PATH: process.env.PATH, PI_CODING_AGENT: "true", ...CC_CHILD_ENV };
	const cwd = tmp;

	const entries = [
		["provider", {
			cwd, env, tools: [], permissionMode: "bypassPermissions", includePartialMessages: true,
			settings: { autoMemoryEnabled: false, claudeMdExcludes: ["**/CLAUDE.md", "**/AGENTS.md", "**/.claude/rules/**"], includeGitInstructions: false },
			systemPrompt: { type: "preset", preset: "claude_code", append: "APPEND" },
			extraArgs: { model: "claude-opus-5-5", "replay-user-messages": null, "strict-mcp-config": null, "thinking-display": "summarized" },
			effort: "high",
			mcpServers: { "custom-tools": createSdkMcpServer({ name: "custom-tools", tools: [] }) },
		}],
		["isolated-summary", {
			cwd, env, settings: { autoMemoryEnabled: false }, tools: [], strictMcpConfig: true, settingSources: [],
			skills: [], persistSession: false, systemPrompt: "SYSTEM", model: "claude-opus-5-5", maxTurns: 1, effort: "high",
			resume: "11111111-1111-1111-1111-111111111111",
		}],
	];

	const snapshot = [];
	for (const [name, options] of entries) {
		const out = join(tmp, `${name}.json`);
		options.env = { ...options.env, RECORDER_OUT: out };
		options.pathToClaudeCodeExecutable = recorder;
		try {
			for await (const _ of query({ prompt: "hi", options })) { /* recorder exits before any message */ }
		} catch { /* the recorder exits without speaking the protocol */ }
		const rec = JSON.parse(readFileSync(out, "utf8"));
		const normalise = (s) => s.replaceAll(tmp, "<tmp>");
		let argv = rec.argv.map(normalise);
		argv = argv.map((a) => (a.startsWith("--debug-file=") ? "--debug-file=<debug-file>" : a));
		const initialize = JSON.parse(rec.initialize);
		initialize.request_id = "<id>";
		snapshot.push({ name, argv, env: rec.env, initialize });
	}
	for (const e of snapshot) delete e.env.RECORDER_OUT;
	writeFileSync(join(repo, "extensions/pig-claude-bridge/internal/ccproto/testdata/sdk-argv-snapshot.json"),
		JSON.stringify(snapshot, null, "\t") + "\n");
} finally {
	rmSync(tmp, { recursive: true, force: true });
}
