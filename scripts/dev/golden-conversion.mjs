// Generates extensions/pig-claude-bridge/internal/convert/testdata/golden-pi-history-310.jsonl:
// the session records the TypeScript bridge writes for tests/fixtures/pi-history-310.jsonl,
// with identity and clock values replaced by placeholders.
//
// Usage: run with the TypeScript clone as the working directory, so `--import tsx` resolves there:
//   cd <typescript-clone> && node --import tsx <this-repo>/scripts/dev/golden-conversion.mjs <typescript-clone> <this-repo>
//
// The clone's src/convert.ts and cc-session-io are loaded by absolute path.
// Placeholders: uuid, parentUuid, timestamp and slug values (top-level keys) and
// msg_syn_/req_syn_/toolu_syn_ ids become "<n>", numbered by first appearance in
// the text of the records, one counter and one table for all kinds. The Go test
// (internal/convert/golden_test.go) applies the same rule to its own output.
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [cloneArg, repoArg] = process.argv.slice(2);
if (!cloneArg || !repoArg) {
	console.error("usage: node golden-conversion.mjs <typescript-clone> <this-repo>");
	process.exit(2);
}
const clone = resolve(cloneArg);
const repo = resolve(repoArg);
const { convertPiMessages } = await import(pathToFileURL(join(clone, "src", "convert.ts")).href);
const { createSession, repairToolPairing } = await import(pathToFileURL(join(clone, "node_modules", "cc-session-io", "dist", "index.js")).href);

const messages = readFileSync(join(repo, "tests", "fixtures", "pi-history-310.jsonl"), "utf8")
	.split("\n").filter(Boolean).map((line) => JSON.parse(line));

const claudeDir = mkdtempSync(join(tmpdir(), "golden-"));
try {
	const session = createSession({
		sessionId: "00000000-0000-4000-8000-000000000000",
		projectPath: "/golden",
		cwd: "/golden",
		gitBranch: "HEAD",
		claudeDir,
	});
	session.importMessages(repairToolPairing(convertPiMessages(messages).anthropicMessages));

	const placeholders = new Map();
	const placeholder = (value) => {
		if (!placeholders.has(value)) placeholders.set(value, `<${placeholders.size + 1}>`);
		return placeholders.get(value);
	};
	const volatile = /("(?:uuid|parentUuid|timestamp|slug)":)"([^"]*)"|"((?:msg|req|toolu)_syn_[0-9a-f]+)"/g;
	const lines = session.records.map((record) => JSON.stringify(record).replace(
		volatile,
		(_, key, value, id) => (key ? `${key}"${placeholder(value)}"` : `"${placeholder(id)}"`),
	));
	writeFileSync(join(repo, "extensions", "pig-claude-bridge", "internal", "convert", "testdata", "golden-pi-history-310.jsonl"), `${lines.join("\n")}\n`);
	console.log(`wrote ${lines.length} records`);
} finally {
	rmSync(claudeDir, { recursive: true, force: true });
}
