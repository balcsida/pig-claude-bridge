# pi-claude-bridge mechanism inventory (commit 6aa276a)

A mechanism-level map of the TypeScript extension being ported, with references into its `src/` and `tests/`. Line numbers are indicative; read the source at `6aa276a` for exact code. Produced 2026-10-25 by a read-only recon pass.

---

# pi-claude-bridge: Architecture for Go Port (PiG)

**Repository:** pi-claude-bridge (TypeScript/Node.js)  
**Target:** Mechanism-level port to Go (PiG)  
**Scope:** Provider lifecycle, MCP server, config, dependencies, Pi API surface, test structure

---

## 1. Provider Path: registerProvider → streamSimple Lifecycle

### Entry Point & Provider Registration

- **src/index.ts:2463** — `export default (pi: ExtensionAPI)`: Extension activation
- **src/index.ts:2643** — `pi.registerProvider(PROVIDER_ID, providerConfig)`: Registers "claude-bridge" provider
- **src/index.ts:1788** — `function streamClaudeAgentSdk()`: Main provider stream function (the actual query handler)

### Full Turn Lifecycle

1. **Pi receives user message → calls streamClaudeAgentSdk(model, context, options)**
   - `context: Context` contains `messages[]`, `systemPrompt`, `tools[]`
   - `options?: SimpleStreamOptions` includes `sessionId`, `signal` (abort), `cacheRetention` (for summaries), `onResponse` callback
   - Returns `AssistantMessageEventStream` (eventEmitter-like push stream)

2. **Prompt Conversion (src/index.ts:1800–1850)**
   - Pi 0.86+ sends transcript-shaped messages (system role in message array)
   - **src/transcript.ts:58** — `toBridgeContext()`: Converts transcript format back to prompt/tools fields
   - **src/prompt-capture.ts** — `promptCaptures.resolveOrDerive()`: Matches system prompt against recorded captures to detect instruction divergence
   - **src/convert.ts:100–150** — `convertPiMessages()`: Pi messages → Claude Code session JSONL format
   - Drops thinking blocks without signatures, aborted turns, non-anthropic providers

3. **Session Sync Decision (src/index.ts:1900)**
   - **src/index.ts:600** — `syncSharedSession()`: Decides resume vs. rebuild
   - Case 1 (REUSE): Latest messages match cursor position → `--resume` with existing sessionId
   - Case 2 (APPEND): New messages after cursor → `--resume` and feed new ones
   - Case 3 (REBUILD): History rewound (via /compact, tree nav) → delete session, create fresh, write full history
   - Case 4 (POST-ABORT): Abort case with orphan writes → rotate sessionId with deleteSession then createSession
   - **cc-session-io** functions: `openSession()`, `deleteSession()`, `createSession()` — manages `~/.claude/projects/<hash>/JSONL`

4. **MCP Server Setup (src/index.ts:1920)**
   - **src/mcp-server.ts:1** — `createToolServer()`: In-process MCP server instance
   - Exposes pi tools verbatim as JSON Schema (no Zod roundtrip)
   - Handles top-level `anyOf`/`oneOf`/`allOf` flattening for Claude Code compatibility
   - **src/skills.ts:1** — `renderSkillsBlock()`: Rewrite skill file references to use MCP read tool
   - Tool names under MCP: `mcp__custom-tools__<pi-name>` (e.g., `mcp__custom-tools__bash`)

5. **Query Invocation (src/index.ts:1950–2050)**
   - **@anthropic-ai/claude-agent-sdk** `query()` call with:
     - `prompt: AsyncIterable<SDKUserMessage>` (streaming-input generator; see `prompt-stream.ts`)
     - `model: string` — CLI model id, e.g. `claude-opus-5-5[1m]` (with 1M suffix if eligible)
     - `options: QueryOptions`:
       - `cwd`: Session working directory (src/session-cwd.ts)
       - `env`: Child process env with CC_CHILD_ENV (no claudeai MCP, no autocompact, no API logging)
       - `tools: []` — Provider path runs with zero tools (all via MCP)
       - `permissionMode: "bypassPermissions"` — Trust pi's tool execution
       - `includePartialMessages: true` — Stream thinking and partial blocks
       - `settings: { ...claudeCodeSettings(), claudeMdExcludes, includeGitInstructions: false }`
       - `systemPrompt: { type: "preset", preset: "claude_code", append: ... }`
       - `extraArgs: { model, "replay-user-messages": null, "strict-mcp-config": null if enabled, "thinking-display": "summarized" if reasoning enabled }`
       - `mcpServers: [{ type: "sdk", name, instance }]` — Single in-process MCP server
       - `resume: sessionId` (if reusing session)
       - `effort: "low"|"medium"|"high"|"max"` (from thinking level mapping)
       - `pathToClaudeCodeExecutable` (if custom path configured)
       - `debug, debugFile, stderr` (if CLAUDE_BRIDGE_DEBUG=1)

### Streaming Events & Message Loop

- **src/index.ts:1500** — `consumeQuery()`: Async generator consumer running in background
  - Iterates: `for await (const message of sdkQuery) { ... }`
  - SDK message types: `stream_event`, `assistant`, `result`, `rate_limit_event`, `user` (replay ack)
  
### Stream Event Processing

- **src/index.ts:1300** — `processStreamEvent()`: Handles `message_start`, `content_block_start/delta/stop`, `message_stop`
  - Pushes pi stream events: `text_start`, `text_delta`, `text_end`, `thinking_start/delta/end`, `toolcall_start/end`
  
- **src/index.ts:1400** — `processAssistantMessage()`: Fallback for non-streaming message delivery
  - Maps Claude Code blocks (text, thinking, tool_use) to pi format
  - Extracts tool_use blocks, looks up pi tool name via `piToolNameFor()`
  
- Tool calls trigger MCP handler blocking:
  - **src/index.ts:1070** — `handleMcpToolCall()`: MCP handler blocks generator on Promise
  - Call awaits result from `pendingResults` map or queued in `pendingToolCalls`
  - **src/index.ts:1830** — `deliverToolResults()`: On next streamSimple call with tool result, resolves awaiting handler

### Tool Result Delivery (Parked Query Mechanism)

- Pi calls streamSimple again with tool result appended to context
- **src/index.ts:1830** — `contextForToolResults()`: Detects results in messages
- **src/index.ts:1815** — `extractAllToolResults()`: Via `cc-session-io`, returns piToolName + result content
- Results matched by Claude Code's `_meta["claudecode/toolUseId"]` (MCP call signature requirement)
- **src/prompt-stream.ts** — `PromptStream`: Long-lived async generator for streaming steer messages to Claude Code's stdin
  - Steer arrives mid-tool execution, written before tool result, drained at next tool boundary
  - Ensures prompt changes take effect same turn

### Session State Tracking

- **src/index.ts:350** — Module-level `sessionStateFor()`: Per-pi-session state map
- State includes: sessionId, cursor (message count), whether rebuild is armed
- **src/index.ts:500** — `markRebuildForSession()`: Arms stale flag for session when pi rewrites via /compact or tree nav
- **src/index.ts:520** — Subagent instances sponsor marks to serving instance via shared registry

### Abort Signal Handling

- **src/index.ts:2120** — `AbortSignal` from options bound to query at fresh-query setup
- **src/index.ts:2150** — Abort listener: `consumeQuery` breaks on `wasAborted()` check
- **src/index.ts:2140** — Post-abort: `discardRewrittenQuery()` if history was rewritten, else normal query end
- **src/index.ts:2160** — Post-abort rebuild rotates sessionId (avoids race with late CC flush)

### Streaming Event → Pi Event Mapping

| Stream Event | Pi Stream Type | Notes |
|---|---|---|
| `message_start` | (internal) | Sets turnStreamMessageId, initializes turn |
| `content_block_start` (type=text) | `text_start` | contentIndex tracks block position |
| `content_block_delta` (type=text) | `text_delta` | Carries delta string |
| `content_block_stop` (type=text) | `text_end` | Complete text content |
| `thinking_start` | `thinking_start` | contentIndex, signature validation |
| `thinking_delta` | `thinking_delta` | Drops if no signature (invalid verification) |
| `thinking_stop` | `thinking_end` | Complete thinking text |
| Tool use block | `toolcall_start`, `toolcall_end` | Paired; blocks waiting for result |
| `message_stop` | (closes stream) | When tool_use emitted; else soft close awaits tool results |
| `rate_limit_event` | (not pushed) | Held; reported via `after_provider_response` headers (rate-limit.ts) |

### Error Handling & Fallback

- **src/index.ts:1600** — `processAssistantMessage()` fallback: Replays non-streamed message (CC's stalled-stream recovery)
- **src/index.ts:1700** — `dropAbandonedStreamBlocks()`: On fallback after stall, discards incomplete blocks from first attempt
- **src/index.ts:1450** — Result error detection: Checks `message.is_error`, reads `errors[]` array
- Exhausted quota ("You're out of extra usage") recognized via `describeRateLimitFailure()`; treated as rate_limit, not generic error

### Usage & Cost Tracking

- **src/usage.ts** — `updateUsage()`: Merges SDK usage frames into pi AssistantMessage
- Fields: `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `output_tokens_details.thinking_tokens`
- Computes: `totalTokens`, `cachePct` (cache-read share), `cost` via pi's `calculateCost()`
- Thinking tokens extracted to `usage.reasoning` (subset of output, not added to total)

### Rate Limit Event Handling

- **src/index.ts:1550** — `rate_limit_event` message: Extracts `rate_limit_info`
- **src/rate-limit.ts** — Builds `anthropic-ratelimit-unified-*` headers for pi's `after_provider_response` callback
- Status values: "rejected", "allowed", "allowed_warning"
- Windows: "5h", "7d", "7d_oi" (overage included) with utilization %, reset Unix seconds
- Pi notifications on: rejected (with reset time), allowed_warning at 5% steps

### Prompt Capture & System Prompt Validation

- **src/prompt-capture.ts** — Per-process registry of system prompts observed
- Recorded at `before_agent_start`, `agent_start`, `turn_start` (key is full prompt text)
- Resolution: Exact key match on turn; if absent, attempts to derive from recorded components (custom + append + files + skills)
- Validation: Refuses prompts with pi's harness text or both `docs/custom-provider.md` + `docs/packages.md` (Anthropic 3P-gate risk)
- Failure: Ends turn with error event, names closest known prompt and divergence point

### Hooks & Extension Integration

- **src/index.ts:2520** — `pi.on("before_agent_start")`: Records system prompt components
- **src/index.ts:2530** — `pi.on("agent_start")`: Records widened prompt (MCP-enhanced)
- **src/index.ts:2535** — `pi.on("turn_start")`: Re-keys for mid-turn prompt changes
- **src/index.ts:2540** — `pi.on("session_compact")`, `pi.on("session_tree")`: Arms rebuild mark
- Subagent queries load fresh module instance; parent's session mirrors preserved via activeQueryContexts

### Compaction & Branch Summary Routing

- **src/index.ts:1805** — `cacheRetention: "none"` (from pi's summaries) → `isolatedStreamFn()`
- **src/index.ts:2300** — `isolatedStreamFn()`: Fresh Claude Code child process, no session sync, no persistent JSONL
- Used for `/compact` summaries, `/bug-report`, branch summaries (pi owns them natively in 0.9.3+)

---

## 2. AskClaude Tool: Parameters, Modes, UI, Config Gating

### Tool Registration & Schema

- **src/index.ts:2700** — `pi.registerTool(askClaudeToolName, ...)` (if enabled)
- **src/askclaude-schema.ts** — `buildAskClaudeParams()`: TypeBox schema with fields:
  - `prompt`: string — the task/question
  - `mode?`: "full" | "read" | "none" — execution mode (configurable availability)
  - `model?`: string — claude model shortcut/id
  - `thinking?`: "off" | "minimal" | "low" | "medium" | "high" | "xhigh" — reasoning effort
  - `isolated?`: boolean — whether to use fresh Claude Code session

### Execution Modes

- **"read"** (default): Read-only; Claude Code can use read, grep, find tools only
- **"full"**: Write + bash allowed; high-risk, subject to permission mode enforcement
- **"none"**: No file access; knowledge-only queries

### AskClaude Isolated Queries

- **src/index.ts:1750** — `isolatedStreamFn()`: Per-call fresh Claude Code process
- Settings:
  - `persistSession: false` — no JSONL written
  - `tools: []` — via mode, filtered to allowed set
  - `autoMemoryEnabled: false` — no CC-side memory
  - No session sync; no rebuild logic
- UI: **src/askclaude-ui.ts** — `buildActionSummary()`: Collapses tool-call sequence into human-readable action tags
- Action summary shown in TUI; raw SDK tool names (`mcp__custom-tools__*`) normalized to pi names

### Config Gating

- **src/config.ts** — `Config.askClaude`:
  - `enabled: boolean` (default false) — register tool at all
  - `name: string` (default "AskClaude") — pi-side tool name
  - `label: string` (default "Ask Claude Code") — TUI label
  - `description: string` — override tool description
  - `defaultMode: "full" | "read" | "none"` (default "read")
  - `defaultIsolated: boolean` (default false)
  - `allowFullMode: boolean` (default true) — gate mode: "full" from enum if false
  - `appendSkills: boolean` (default true) — include pi's skills in AskClaude prompt

- **src/index.ts:2710** — Startup notice if `enabled` unset and Pi >= 0.86
- **src/index.ts:2720** — Deprecation notice on first use

### Tool Invocation Path

1. **src/index.ts:1750** — `askClaudeCallHandler()`: Receives tool call from pi
2. Resolves mode/model/thinking via `resolveAskClaude*()` functions in askclaude-schema.ts
3. Calls `isolatedStreamFn()` with:
   - Context: user prompt only (no history)
   - Rebuilt tools set (read-only if mode="read")
   - System prompt: claude_code preset, no pi-specific context files/AGENTS.md
4. Streams response back to pi; action summary via `buildActionSummary()`

---

## 3. Config File & Models

### Config Locations & Merge

- **~/.pi/agent/claude-bridge.json** (global) — user preferences, notices
- **.pi/claude-bridge.json** (project) — project-level overrides
- Merge: project over global for every key (src/config.ts:loadConfig)

### Config Structure

**Global (`~/.pi/agent/claude-bridge.json`)**
```json
{
  "startupNoticeShown": "YYYY-MM-DD",
  "askClaudeDeprecationShown": "YYYY-MM-DD",
  "askClaude": { ... },
  "provider": { ... }
}
```

**Keys:**
- `startupNoticeShown`, `askClaudeDeprecationShown` — written by extension, marks date first notice shown
- `askClaude` — see section 2 above
- `provider`:
  - `plan: "pro" | "max"` (default "pro") — subscription tier for 1M context eligibility
  - `longContextExtraUsage: boolean` (default false) — enable paid 1M context models
  - `forceTwoHundredK: string[]` — model ids to pin to 200K even if 1M declared
  - `strictMcpConfig: boolean` (default true) — block `~/.claude.json` + `.mcp.json` MCP servers
  - `autoMemoryEnabled: boolean` (default false) — enable Claude Code's auto-memory
  - `pathToClaudeCodeExecutable: string` — path to `claude` binary (for Nix, etc.)

### Model Selection & Derivation

**src/models.ts — Model List**
- **`buildModels(piAiModels)`**: Projects pi-ai's Anthropic catalog
  - Filters out dated snapshots (e.g., `claude-opus-4-5-20251101`)
  - Sorts by family (Fable → Opus → Sonnet → Haiku) then newest version first
  - Strips cost fields (zeroed so pi's footer doesn't multiply huge token counts)
  - Forwards `thinkingLevelMap` per-model reasoning overrides

**1M Context Eligibility**
- **`MEASURED_ONE_M`** set (src/models.ts:70): Models verified to serve 1M on all plans
  - Fable 5, 5.1
  - Opus 5.5, 5, 4.8, 4.7
  - Sonnet 5, 5.5
  - Haiku 5.5

- **`PLAN_GATED_ONE_M`** (src/models.ts:80): Models gated by subscription
  - Opus 4.6: needs plan="max" OR longContextExtraUsage=true
  - Sonnet 4.6: needs longContextExtraUsage=true

- **`resolveClaudeCodeRuntimeModel()`** (src/models.ts:100):
  - Consults `forceTwoHundredK` first (user override)
  - Then measured sets
  - CLI model id: bare id at 200K, or `{id}[1m]` for 1M
  - Returns: `{ cliModelId, contextWindow }` (200K or 1M)

**Model Registration**
- **`applyLongContext()`** (src/models.ts:150): Patches registered models' `contextWindow` and `name` fields
  - Name label appended: " 1M" if 1M (and not already present)

**Resolution & Shortcuts**
- **`resolveModel()`** (src/models.ts:130): Resolves "opus" → newest Opus, "sonnet" → newest Sonnet, etc.
  - Exact match first, then partial (to avoid dated snapshot shadowing bare id)
  - Newest partial match by version rank

---

## 4. Runtime Dependencies & Implementation Scope for Go

| Package | Version | Purpose | Go Port: From-Scratch? |
|---|---|---|---|
| `@anthropic-ai/claude-agent-sdk` | ^0.3.308 | SDK query(), message streaming, effort/reasoning mapping | **YES** — core provider protocol; controls CLI subprocess, streaming stdin/stdout parsing, session file format, MCP transport |
| `@modelcontextprotocol/sdk` | ^1.29.0 | MCP server (ListTools, CallTool RPC handlers) | **Partial** — implement JSON-RPC transport for tools/call, tools/list; main bridge logic is tool schema flattening (simple tree walk) |
| `cc-session-io` | ^0.4.0 | Session JSONL read/write, project-path hashing, repair tool pairing | **YES** — file format versioning, deterministic path-to-hash, transcript walking, orphan-result repair |
| `change-case` | ^5.4.4 | Tool name casing (pascalCase for SDK tools) | **NO** — single use in convert.ts:30, trivial string transform |
| `@earendil-works/pi-ai` | >=0.86.1 (peer) | Model catalog, message types, cost calculation, skill formatting | **NO** — bridge receives models + messages as data; only reads catalog at startup |
| `@earendil-works/pi-coding-agent` | >=0.86.1 (peer) | Extension API types (ExtensionAPI, ExtensionContext, buildSessionContext) | **NO** — Pi Go will provide equivalent interface; bridge calls `.registerProvider()`, `.registerTool()`, `.on()` events |
| `@earendil-works/pi-tui` | >=0.86.1 (peer) | UI notification + custom modal (Markdown rendering future goal) | **NO** — bridge calls `ctx.ui.notify()` only; PI Go provides this |
| `typebox` | ^1.3.7 (peer) | JSON Schema type definitions for tool parameters | **NO** — bridges uses existing TypeBox schemas from pi; only validates they are object types |

### From-Scratch Reimplementation Checklist for Go

1. **Claude Agent SDK equivalent**:
   - Subprocess spawning with custom env, cwd, args
   - Streaming stdin writer (for user messages, steers)
   - Streaming stdout parser (message_start, stream_event, etc.)
   - Session ID generation & persistence
   - Thinking block verification via signature
   - Model validation (context window eligibility checks)
   - API error classification (rate limits, quotas)
   - Effort level → CLI flag mapping (xhigh → max, etc.)

2. **cc-session-io equivalent**:
   - JSON JSONL append/read, per-message framing
   - Project path → deterministic hash (djb2, sanitization)
   - Transcript walking: find cursor, extract messages at range
   - Tool pairing repair: orphan `tool_result` → preceding `tool_use`
   - Signature validation for thinking blocks
   - Model id sanitization

3. **MCP protocol (minimal)**:
   - JSON-RPC 2.0 over stdio
   - tools/list request → tool definitions with flattened schemas
   - tools/call request → invoke handler, return content/isError
   - Error handling (unknown tool, missing _meta[toolUseId])

4. **NOT reimplemented** — already provided by Pi Go or trivial:
   - Tool name case transforms (use strings.Title, etc.)
   - Config file JSON parsing (standard library)
   - Event system (Pi Go extension API)
   - Model catalog lookup (Pi Go provides)
   - Tree walking for schema flattening (straightforward recursion)

---

## 5. Pi Extension API Surface Used

### Events (via `pi.on()`)

| Event | Handler Args | Purpose | Line |
|---|---|---|---|
| `session_start` | `{ reason: "new" | "resume" | "fork" }`, `ctx` | Reset session state; remember cwd | 2510 |
| `before_agent_start` | `{ systemPrompt, systemPromptOptions }` | Record system prompt for capture key | 2520 |
| `agent_start` | `ctx` | Record widened prompt (MCP-enhanced) | 2530 |
| `turn_start` | `ctx` | Re-key prompt for mid-turn changes | 2535 |
| `session_before_compact` | `{ signal }`, `ctx` | Remember summary session id for compact | 2515 |
| `session_before_tree` | — , `ctx` | Remember summary session id for tree nav | 2516 |
| `session_compact` | `{ reason, willRetry }`, `ctx` | Arm rebuild mark (discard parked query) | 2545 |
| `session_tree` | — , `ctx` | Arm rebuild mark (discard parked query) | 2547 |
| `session_shutdown` | — , `ctx` | Clear session state, report leaks | 2540 |

### Provider & Tool Registration

- **`pi.registerProvider(id, config)`** (src/index.ts:2643)
  - `id: "claude-bridge"` — provider identifier
  - `config.models: Model[]` — registered models with contextWindow, reasoning support
  - `config.streamSimple: (model, context, options?) => AssistantMessageEventStream` — query function

- **`pi.registerTool(name, schema, handler)`** (src/index.ts:2700, if askClaude enabled)
  - `name: askClaudeToolName` (configurable)
  - `schema: TypeBox Type.Object(...)` — parameters schema
  - `handler: (args, ctx) => Promise<AssistantMessage>` — execute AskClaude

### Context Methods Used

| Method | Returns | Purpose | Line |
|---|---|---|---|
| `ctx.getSystemPrompt()` | string | Get widened system prompt (MCP-enhanced) | 2530 |
| `ctx.sessionManager.getSessionId()` | string | Current pi session id | 2510 |
| `ctx.ui.notify(text, type)` | void | Show notification (info/warning) | 1540 |
| `ctx.cwd` | string | Session working directory | 2510 |
| `ctx.modelRegistry.getProvider(id)` | Provider? | Check if provider registered | 2655 |

### System Prompt Options

- From `before_agent_start.systemPromptOptions`:
  - `customPrompt?: string` — user's custom system prompt
  - `appendSystemPrompt?: string` — appended instructions
  - `contextFiles?: { path, content }[]` — AGENTS.md, CLAUDE.md expansions
  - `skills?: Skill[]` — pi skills definitions
  - `sections?: Record<string, string>` — MCP sections, etc.
  - `selectedTools?: string[]` — enabled tools (filter skills read access)

### Stream & Message Types

- **`AssistantMessageEventStream`**: Push stream (Node.js Readable-like)
  - Events: `text_start`, `text_delta`, `text_end`, `thinking_start/delta/end`, `toolcall_start/end`, `done`, `error`
  - Pushed by bridge; consumed by pi's agent loop

- **`Context`**: Transcript container
  - `messages: Message[]` — conversation history
  - `systemPrompt?: string` — system role content
  - `tools?: Tool[]` — available tools

- **`SimpleStreamOptions`**: Query options
  - `sessionId?: string` — pi session identifier
  - `signal?: AbortSignal` — cancellation
  - `cacheRetention?: "none"` — for summaries
  - `onResponse?: (response) => void` — after-response callback (for usage headers)

---

## 6. Tests: Unit vs. Integration

### Unit Tests (Offline, Mocked)

| Test | Coverage |
|---|---|
| `unit-agent-start-capture.mjs` | Prompt capture recording at before_agent_start, agent_start, turn_start boundaries |
| `unit-agent-start-capture-gaps.mjs` | Prompts outside pi's pipeline (not captured), subagent-only edge cases |
| `unit-agents-md.mjs` | AGENTS.md merging (native + CC's copy exclusion) |
| `unit-askclaude-schema.mjs` | AskClaude parameter schema, mode/model resolution, defaults |
| `unit-attachments.mjs` | File attachment carry-forward across rebuilds |
| `unit-cache-diagnostics.mjs` | Cache hit % calculation, prompt cache locality |
| `unit-child-env.mjs` | CC subprocess environment variables (no claudeai MCP, disable autocompact) |
| `unit-compaction-coexistence.mjs` | pi's native compaction + bridge interaction, subagent isolation |
| `unit-convert-determinism.mjs` | Message conversion determinism, rebuilds extend not rewrite |
| `unit-error-result.mjs` | Error result recording when turn stream closed |
| `unit-model-resolution.mjs` | Model picker, 1M eligibility checks (plan gates), [1m] suffix logic |
| `unit-nonstreaming-fallback.mjs` | Non-streaming fallback handling when stream stalls |
| `unit-request-body.mjs` | SDK query() options validation (tools: [], permissionMode, etc.) |
| `unit-stream-replay.mjs` | Replayed SDK message shapes (from captured fixtures) |
| `unit-sync-shared-session.mjs` | REUSE/APPEND/REBUILD/post-abort sync paths |
| `unit-tool-call-result-pairing.mjs` | Matching results to calls by Claude's tool_use_id (not FIFO) |
| `unit-transcript-replay.mjs` | Pi transcript → bridge context conversion (system message stripping) |

### Integration Tests (Spawn Processes)

| Test | What It Spawns | Coverage |
|---|---|---|
| `int-smoke.sh` | Real pi + Claude Code | Basic provider query, tool execution |
| `int-multi-turn.sh` | Real pi + Claude Code | Multi-turn conversation, session resume |
| `int-session-new.mjs` | Real pi + Claude Code | Fresh session creation, JSONL write, verify |
| `int-session-resume.mjs` | Real pi + Claude Code | Session resume after tool result |
| `int-session-rebuild.mjs` | Real pi + Claude Code | Rebuild on provider switch or divergence |
| `int-session-compact.mjs` | Real pi + Claude Code | /compact compaction, rebuild logic |
| `int-cursor-after-tools.mjs` | Real pi + Claude Code | Cursor positioning after tool batch |
| `int-tool-message.mjs` | Real pi + Claude Code | Tool result delivery mid-turn |
| `int-parallel-tool-results.mjs` | Real pi + Claude Code | Multiple tools, parallel result delivery |
| `int-shutdown-kills-cc.mjs` | Real pi + Claude Code | Abort signal kills CC subprocess mid-tool |
| `int-compact-baseline.mjs` | Real pi + Claude Code | Baseline compaction state (cache behavior) |
| `int-compact-midturn-rebuild.mjs` | Real pi + Claude Code | Compaction during parked query (discard + rebuild) |
| `int-compact-splitturn.mjs` | Real pi + Claude Code | Compaction splitting assistant message mid-turn |
| `int-compact-second-compact.mjs` | Real pi + Claude Code | Consecutive compactions |
| `int-compact-auto-threshold.mjs` | Real pi + Claude Code | Auto-compaction threshold logic |
| `int-branch-summary.mjs` | Real pi + Claude Code | Branch summary isolation (cacheRetention: "none") |
| `int-image-session.mjs` | Real pi + Claude Code | Image attachments in prompts/results |
| `int-attachment-rebuild.mjs` | Real pi + Claude Code | @file expansions surviving rebuilds |
| `int-cache-git.sh` | Real pi + Claude Code | Git status cache behavior (includeGitInstructions: false) |
| `int-cache.sh` | Real pi + Claude Code | Prompt cache hit tracking across turns |
| `int-subagent-rpiv-codebase-locator.mjs` | Real pi + Claude Code + subagent | Subagent module isolation, parent/child session separation |
| `int-cc-contracts.mjs` | Real Claude Code (installed SDK version) | Undocumented SDK behavior (resume reordering, tool_use_id meta) |
| `int-served-window.mjs` | Real pi + Claude Code | Context window served (200K vs 1M) by model |

### Test Fixtures

- **tests/fixtures/pi-history-310.jsonl** — Real pi session history (310 messages)
- **tests/fixtures/sdk-streams/{text,single-tool,parallel-tools}.jsonl** — Captured SDK message sequences
- **tests/fixtures/{compact-file-a.txt, compact-file-b.txt}** — File contents for edit simulation
- **tests/fixtures/tree-nav-extension.ts, slow-tool-extension.ts** — Fixture pi extensions

### Diagnostic Tools (in diag/)

- **diag/audit-transcripts.mjs** — Scans session JSONL for orphaned tool calls, silent data loss
- **diag/audit-cache.mjs** — Cache hit % analysis per session, cache-bust causes
- **diag/audit-warnings.mjs** — Warns categorized by origin (bridge vs MCP handler)
- **diag/context-size.mjs** — Probe Claude Code for 1M context capability per model
- **diag/replay-write-path.mjs** — Replay any pi session through rebuild logic, verify determinism
- **diag/capture-proxy.mjs** — Capture live SDK request bodies (for protocol analysis)
- **diag/probe-git-cache.mjs** — Measure impact of includeGitInstructions on cache

---

## Key Invariants for Go Port

1. **Prompt Capture Matching**: System prompt exact-match key must be bit-identical to pi's prompt builder output (section order: preamble, tools, rules, docs, addendum, project_context, skills, cwd, custom…)
2. **Tool Pairing**: Results matched by Claude Code's `_meta["claudecode/toolUseId"]`, not call position
3. **MCP Schema Flattening**: Root `anyOf`/`oneOf`/`allOf` must be flattened (properties merged, allOf required joined) to avoid CC rejecting the tool
4. **Streaming Input**: Long-lived prompt stream (AsyncIterable) lets steers reach CC's stdin before tool result, same-turn execution
5. **Session State Isolation**: Per-pi-session state (sessionId, cursor, rebuild mark) must not leak to sibling sessions (subagents)
6. **Deterministic Conversion**: Pi history → CC JSONL must be deterministic (same input → identical bytes, so rebuild extends not rewrites)
7. **Abort Race**: Post-abort rebuild rotates sessionId to avoid race with late CC process flush writing orphan records
8. **Rebuild Attribution**: Rebuild mark scoped to pi session, not module-level, so subagent compaction doesn't discard parent's parked query

---

**Notes:**
- SDK streaming protocol (message_start/stream_event/message_stop/result) is internal to claude-agent-sdk; Go port must reimplement or wrap.
- cc-session-io file format (JSONL with deterministic path hashing) is stable contract; use cc-session-io-go if available or port.
- MCP is standard JSON-RPC; a minimal implementation (tools/list, tools/call) suffices; no Zod dependency.
- Thinking signature validation required for replay (Anthropic requirement); certificate pinning not visible to bridge.
- Pi integration assumed; bridge registers at activation and listens to session lifecycle events.
