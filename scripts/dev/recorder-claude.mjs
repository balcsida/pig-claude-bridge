// Stand-in for `claude`: records argv, the CLAUDE_*-style environment and the first stdin line
// (the initialize request) to the file named by RECORDER_OUT, then exits 0.
import { writeFileSync } from "node:fs";
import { createInterface } from "node:readline";

const prefixes = ["CLAUDE_", "ENABLE_CLAUDEAI_", "DISABLE_AUTO_COMPACT", "NODE_OPTIONS", "DEBUG", "PI_CODING_AGENT"];
const env = Object.fromEntries(
	Object.entries(process.env).filter(([k]) => prefixes.some((p) => k.startsWith(p))),
);
const rl = createInterface({ input: process.stdin });
rl.once("line", (initialize) => {
	writeFileSync(process.env.RECORDER_OUT, JSON.stringify({ argv: process.argv.slice(2), env, initialize }));
	process.exit(0);
});
