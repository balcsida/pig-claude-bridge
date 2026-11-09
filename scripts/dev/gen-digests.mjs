// Generates tests/fixtures/message-digests.json: pi messages and the digest pi-claude-bridge's
// src/message-digests.ts computes for each, so the Go port (internal/digest) is pinned to it.
//
//   cd <typescript-clone> && node --import tsx <this-repo>/scripts/dev/gen-digests.mjs <typescript-clone> <this-repo>
import { writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [cloneArg, repoArg] = process.argv.slice(2);
if (!cloneArg || !repoArg) {
	console.error("usage: node gen-digests.mjs <typescript-clone> <this-repo>");
	process.exit(2);
}
const { digestMessage } = await import(pathToFileURL(join(resolve(cloneArg), "src", "message-digests.ts")).href);

const sig = "sig-0123";
const messages = [
	{ name: "user string", message: { role: "user", content: "hello", timestamp: 1 } },
	{ name: "user string, metadata ignored", message: { role: "user", content: "hello", timestamp: 2, id: "x" } },
	{ name: "user empty string", message: { role: "user", content: "" } },
	{ name: "user non-array content", message: { role: "user", content: 5 } },
	{ name: "user blocks with text and image", message: { role: "user", content: [{ type: "text", text: "see" }, { type: "image", data: "AAAA", mimeType: "image/png" }, { type: "text", text: "" }] } },
	{ name: "user blocks with nothing usable", message: { role: "user", content: [{ type: "text", text: "" }, { type: "image", data: "" }] } },
	{ name: "user text with astral and accented characters", message: { role: "user", content: "árvíztűrő 😀 tükörfúrógép" } },
	{ name: "toolResult text", message: { role: "toolResult", toolCallId: "call_1", toolName: "read", content: [{ type: "text", text: "A" }], isError: false } },
	{ name: "toolResult error", message: { role: "toolResult", toolCallId: "call_1", toolName: "read", content: [{ type: "text", text: "A" }], isError: true } },
	{ name: "toolResult string content", message: { role: "toolResult", toolCallId: "call_2", content: "plain" } },
	{ name: "toolResult with image", message: { role: "toolResult", toolCallId: "call_3", content: [{ type: "text", text: "shot" }, { type: "image", data: "QUJD", mimeType: "image/jpeg" }, { type: "widget" }] } },
	{ name: "toolResult with a non-text block and no image", message: { role: "toolResult", toolCallId: "call_4", content: [{ type: "text", text: "x" }, { type: "widget" }] } },
	{ name: "toolResult id with newline framing", message: { role: "toolResult", toolCallId: "a\nb", content: "c" } },
	{ name: "toolResult id framing swapped", message: { role: "toolResult", toolCallId: "a", content: "\nbc" } },
	{ name: "assistant omitted", message: { role: "assistant", provider: "claude-bridge", content: [] } },
	{ name: "assistant non-array content", message: { role: "assistant", provider: "claude-bridge", content: "text" } },
	{ name: "assistant placeholder: foreign thinking only", message: { role: "assistant", provider: "anthropic", content: [{ type: "thinking", thinking: "t", thinkingSignature: sig }] } },
	{ name: "assistant placeholder: unsigned bridge thinking", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "thinking", thinking: "t" }] } },
	{ name: "assistant text", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "text", text: "hi" }], usage: { input: 9 }, stopReason: "stop" } },
	{ name: "assistant signed bridge thinking and text", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "thinking", thinking: "t", thinkingSignature: sig }, { type: "text", text: "hi" }] } },
	{ name: "assistant signed bridge thinking without text", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "thinking", thinkingSignature: sig }] } },
	{ name: "assistant tool call with canonical arguments", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "toolCall", id: "call_1", name: "read", arguments: { b: 1, a: "x" } }] } },
	{ name: "assistant tool call with the same arguments in another key order", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "toolCall", id: "call_1", name: "read", arguments: { a: "x", b: 1 } }] } },
	{ name: "assistant tool call without arguments", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "toolCall", id: "call_1", name: "read" }] } },
	{ name: "assistant tool call with every JSON value kind", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "toolCall", id: "c", name: "t", arguments: { n: [1, 1.5, -2, 1e21, 1e-7, 0.000001, 123456789012345680000, -0, 0, 3.0], b: [true, false], z: null, o: { k: [] }, s: "" } }] } },
	{ name: "assistant parallel tool calls and text", message: { role: "assistant", provider: "claude-bridge", content: [{ type: "text", text: "doing" }, { type: "toolCall", id: "a", name: "bash", arguments: { command: "ls" } }, { type: "toolCall", id: "b", name: "read", arguments: { path: "/x" } }] } },
];

const out = messages.map(({ name, message }) => ({ name, message, digest: digestMessage(message) }));
writeFileSync(join(resolve(repoArg), "tests", "fixtures", "message-digests.json"), `${JSON.stringify(out, null, "\t")}\n`);
console.log(`wrote ${out.length} digests`);
