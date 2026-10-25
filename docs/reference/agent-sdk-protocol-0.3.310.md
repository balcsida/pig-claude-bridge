# Claude Agent SDK stdio protocol reference (pinned 0.3.310)

Derived from the `@anthropic-ai/claude-agent-sdk` 0.3.308 package (type definitions and minified transport) installed in the pi-claude-bridge clone, then checked against 0.3.310 (diff in the appendix: identical for everything the bridge uses). Line references point into the 0.3.308 package files. Open items in section 7 are closed by spike S2 of the design. Produced 2026-10-25 by a read-only recon pass; verify any wire detail with a live probe before relying on it.

---

# Claude Agent SDK wire protocol (for a Go reimplementation)

Source: `@anthropic-ai/claude-agent-sdk` **0.3.308** (CLI 2.1.308, `package.json` `claudeCodeVersion`), `cc-session-io` (repo node_modules).
Paths: `SDK=node_modules/@anthropic-ai/claude-agent-sdk`. `sdk.mjs` is minified (169 lines). Reference form: `sdk.mjs:LINE` + grep term.
`sdk.d.ts:N` = line in `SDK/sdk.d.ts`. Anything not found is marked **NOT FOUND**. Anything unverified is marked *(inferred)*.

Mental model: the SDK is a thin NDJSON bridge to a **native `claude` binary**. Everything (prompt, tools, MCP, hooks) is
carried over stdin/stdout as one JSON object per line. There is no HTTP layer in the SDK.

---
## 1. Process spawning

### 1.1 Executable resolution (`sdk.mjs:163` near `pathToClaudeCodeExecutable`, `sdk.mjs:157` `function O9(`)
- 0.3.308 **ships no `cli.js`**. Without `options.pathToClaudeCodeExecutable` the SDK resolves a native binary from an
  optionalDependency platform package: `@anthropic-ai/claude-agent-sdk-{darwin|linux|win32}-{x64|arm64}` (+ `-musl` on linux;
  `-linux-<arch>-android`), file `<pkg>/claude` (`claude.exe` on win32). Linux tries musl first when `preferMusl` (libc detection `g4e`).
  Not found -> `Error("Native CLI binary for <platform>-<arch> not found. Reinstall ... without --omit=optional, or set options.pathToClaudeCodeExecutable.")`.
- `manifest.json` in SDK dir lists per-platform `checksum`/`size` (`manifestSignatureEnforcement:"flag"`); I did not trace where it is checked.
- Spawn decision (`sdk.mjs:157` `function u4e(`): if the path ends in `.js .mjs .tsx .ts .jsx` -> run `executable` (`bun` if running under bun else `node`;
  option `executable`) with `[...executableArgs, path, ...cliArgs]`; **otherwise the path is exec'd directly** with `[...executableArgs, ...cliArgs]`.
- `extractFromBunfs(path)` (`SDK/extractFromBunfs.js:98`, exported `./extract`): if the path contains `$bunfs` or `~BUN` (Bun single-file embed / Windows)
  it copies the file to a real temp dir so it can be spawned; children can't read `$bunfs`. Otherwise returns the path unchanged.
- `options.spawnClaudeCodeProcess(gn)` replaces local spawn (gn = `{command,args,cwd,env,signal}`); must return an object with
  `stdin, stdout, killed, exitCode, signalCode, kill, on/once/off`.
- Spawn call: `child_process.spawn(command,args,{cwd,stdio:["pipe","pipe","pipe"],signal,env,windowsHide:true})`.

### 1.2 argv, in emission order (`sdk.mjs:157` `initialize(){` ... `let W=["--output-format"`)
Helper `Ke(k,v)`= `"k=v"` (single token). `Cy(k,v)`: `[k,v]` as two tokens, unless v starts with `-` (then `k=v`).

| argv | Option driving it |
|---|---|
| `--output-format stream-json --verbose --input-format stream-json` | always (first 5 tokens, separate tokens) |
| `--thinking=adaptive` | `thinking:{type:'adaptive'}`, or `{type:'enabled'}` with no `budgetTokens` |
| `--max-thinking-tokens=N` | `thinking:{type:'enabled',budgetTokens:N}`; legacy `maxThinkingTokens:N` (0 => disabled) |
| `--thinking=disabled` | `thinking:{type:'disabled'}` / `maxThinkingTokens:0` |
| `--thinking-display=summarized\|omitted\|highlights` | `thinking.display` (not sent when disabled) |
| `--effort=X` | `effort` (`low\|medium\|high\|xhigh\|max`) |
| `--max-turns=N` / `--max-budget-usd=N` / `--task-budget=<taskBudget.total>` | `maxTurns` / `maxBudgetUsd` / `taskBudget` |
| `--model=M` / `--agent=A` / `--betas=a,b` | `model` / `agent` / `betas` |
| `--json-schema=<JSON>` | `outputFormat:{type:'json_schema',schema}` |
| `--debug-file=P` or `--debug` | `debugFile` / `debug`; else an auto `--debug-file=<rG()>` unless `spawnClaudeCodeProcess` set |
| `--permission-prompt-tool=stdio` | `canUseTool` given (error if also `permissionPromptToolName`); else `--permission-prompt-tool=<name>` |
| `--permission-prompts=X` | `permissionPrompts` |
| `--continue` / `--resume=ID` | `continue` / `resume` |
| `--channels=X` (repeat) | `channels` |
| `--allowedTools=a,b` | `allowedTools` (+ `Skill(name)` entries from `skills`, or `Skill` if `skills:'all'`) |
| `--disallowedTools=a,b` | `disallowedTools` |
| `--tools=a,b` / `--tools=default` | `tools` array (an empty array gives `--tools=` i.e. none) / preset object |
| `--mcp-config <JSON>` (`{"mcpServers":{...}}`) | `mcpServers`, **only non-SDK entries** (entries `{type:'sdk',instance}` are held in-process and announced via `initialize.sdkMcpServers`, never on argv) |
| `--setting-sources=user,project,local` | `settingSources` (flag omitted when undefined -> CLI default loads all) |
| `--strict-mcp-config` | `strictMcpConfig` |
| `--permission-mode=M` | `permissionMode` (`default acceptEdits bypassPermissions plan dontAsk auto`) |
| `--allow-dangerously-skip-permissions` | `allowDangerouslySkipPermissions` (note: *allow*, not `--dangerously-skip-permissions`) |
| `--fallback-model=M` | `fallbackModel` (throws if equal to `model`) |
| `--include-hook-events` / `--include-partial-messages` / `--session-mirror` | `includeHookEvents` / `includePartialMessages` / `sessionMirror` |
| `--project-config-root=P`, `--add-dir=P` (repeat) | `projectConfigRoot`, `additionalDirectories` |
| `--await-initialize` or `--plugin-dir=P` / `--plugin-dir-no-mcp=P` | `pluginsViaInitialize` / `plugins[{type:'local',path}]` |
| `--fork-session`, `--resume-session-at=X`, `--resume-drops-turn=X` | `forkSession`, `resumeSessionAt`, `resumeDropsTurn` |
| `--session-id=UUID` | `sessionId` |
| `--no-session-persistence` | `persistSession:false` |
| `--managed-settings <JSON>` | `managedSettings` |
| `--<k>` (null) / `--<k>=<v>` via `Cy` | every `extraArgs` entry; `options.settings` is merged in as `extraArgs.settings` => `--settings` (sandbox option is folded into that JSON, `E9`) |

**Not on argv** in this version (grep count 0 for `"--system-prompt`, `"--append-system-prompt`, `"--replay-user-messages`):
- system prompt / append / agents / hooks / title / skills / plugins-via-init go in the `initialize` control request (section 2.2).
- `--replay-user-messages` is only reachable via `extraArgs` (the bridge sets `extraArgs["replay-user-messages"]=null`, `src/index.ts:2039`).
- `--agents`, `--debug-to-stderr`, `--dangerously-skip-permissions` are CLI-side flags; the SDK never emits them (`--debug-to-stderr` is only parsed by the CLI debug logger).

### 1.3 Environment (`sdk.mjs:163` `Rv(` ~ `CLAUDE_CODE_ENTRYPOINT`; `sdk.mjs:157` `initialize(){`)
Base env = `options.env` if given, **else `{...process.env}`** (options.env replaces, doesn't merge). Then:
- `CLAUDE_CODE_ENTRYPOINT=sdk-ts` if unset; `CLAUDE_AGENT_SDK_VERSION=0.3.308` if unset (also written to the SDK's own `process.env`).
- `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true` if `enableFileCheckpointing`.
- `CLAUDE_CODE_SDK_READS_SESSION_STATE=1` (unless any case-variant already set); `CLAUDE_CODE_SDK_READS_STARTUP_MARKS=1` (conditional).
- `CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH=1` if `getOAuthToken`; `CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH=1` if `getHostAuthToken`.
- `CLAUDE_CODE_QUESTION_PREVIEW_FORMAT`, `CLAUDE_CODE_QUESTION_EXTENDED=1`, `CLAUDE_CODE_QUESTION_OPTIONAL_DESCRIPTIONS=1` from `toolConfig.askUserQuestion`.
- `CLAUDE_CODE_AGENT_PROGRESS_SUMMARIES` deleted when `options.env` absent and flag false.
- `CLAUDE_CONFIG_DIR` set when a `sessionStore` temp dir is used (win32 also `CLAUDE_SECURESTORAGE_CONFIG_DIR`).
- **Stripped**: `NODE_OPTIONS` always deleted; `DEBUG` deleted unless `DEBUG_CLAUDE_AGENT_SDK` truthy (then `DEBUG=1`).
- OpenTelemetry context is injected as `TRACEPARENT`/`TRACESTATE` (removed from inherited env if no active span).
- Reads: `CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS` (run-end ceiling, default `Ty`=600000 ms), `CLAUDE_AGENT_SDK_DISABLE_MCP_MANIFESTS`.

### 1.4 stdio, exit, abort (`sdk.mjs:157` `spawnLocalProcess`, `close(){`, `readMessages`)
- stdin/stdout: UTF-8 NDJSON, one `JSON.stringify(obj)+"\n"` per message; stdout read via `readline`; blank lines skipped, non-JSON lines are logged and ignored (`Non-JSON stdout`).
- stderr: `StringDecoder("utf8")`; always kept in a rolling tail (2048 chars `sv`) used in exit errors; forwarded to `options.stderr(chunk)` only when set (or `DEBUG_CLAUDE_AGENT_SDK`). Exit event is delayed until stderr closes (or 200 ms `a4e`).
- Writes before spawn completes are queued (`pendingWrites`). Write after abort throws; writes to ended stdin are dropped; writing to a dead process throws.
- Exit: code != 0 -> `Claude Code process exited with code N. stderr: <tail>`; signal -> `terminated by signal SIGx`; code 0 ok. If the last `result` was an error, that text replaces the exit error (`Claude Code returned an error result: ...`, `readMessages` catch).
- **Abort / close**: `options.abortController.abort()` or `query.close()` -> `transport.close()`: `stdin.end()`; if child still alive after **2000 ms** (`i4e`) -> `SIGTERM` (win32: `SIGKILL` after 5 s); then `SIGKILL` 5 s later. The spawn also receives `signal` (forwardedAbort) so Node kills it on abort. `process.on("exit")` kills any tracked children (`T9` tracker).
- **`interrupt()`** is NOT a signal: it is the `interrupt` control request (2.3). `close()` is the hard stop. (The bridge calls both: `src/index.ts:2106`.)
- Query end: the async iterator ends when stdout closes (`inputStream.done()`), after `cleanup`. Cleanup rejects all pending control requests, closes in-process MCP transports, waits <= 2 s for process exit.

---
## 2. Control protocol (stdin/stdout NDJSON)

Envelopes (types `sdk.d.ts:5200` `SDKControlRequest`, `:5251` `SDKControlResponse`, `:3967` `SDKControlCancelRequest`; `ControlResponse` `:422`, `ControlErrorResponse` `:397`):
```json
{"type":"control_request","request_id":"<id>","request":{"subtype":"<name>", ...}}
{"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{...}}}
{"type":"control_response","response":{"subtype":"error","request_id":"<id>","error":"<text>","error_code":"snake_case?"}}
{"type":"control_cancel_request","request_id":"<id>"}
```
- Either side originates requests. The responder echoes `request_id`. No reply to `control_cancel_request`.
- **request_id scheme**: SDK-originated: `Math.random().toString(36).substring(2,15)` (`sdk.mjs:159` `request(e,t){`); SDK-originated `mcp_message` requests use `Oa()` (uuid generator, *inferred* random UUID; the function body was not located). CLI-originated ids: opaque, just echo them.
- Error `error_code` is accepted only if it matches `/^[a-z][a-z0-9_]{0,63}$/` (`y4e`).
- Success response may also carry `pending_permission_requests[]`/`pending_user_dialog_requests[]` (re-delivery of in-flight `can_use_tool`s); the SDK only honors them on the `initialize` response and re-dispatches each as an incoming request.
- If an SDK request's AbortSignal fires after write, SDK sends `control_cancel_request{request_id}` and rejects locally.
- Responses arriving with an unknown id are buffered (max 1024) for `awaitControlResponse`.
- Duplicate in-flight inbound request ids are ignored; handlers run concurrently (not serialized).
- Sentinel `Hc`: a handler returning null (canUseTool etc.) means **send no response at all**.
- Requests with subtype `remote_tool_call, remote_plumbing_call, remote_tools_probe, remote_tools_reannounce` are deliberately left unanswered (`M9`).
- Unknown inbound subtype -> error response `Unsupported control request subtype: X`. Any thrown handler error -> `{"subtype":"error","request_id":..,"error":"<message>"}`.
- Writes of requests other than `initialize` wait until the `initialize` request has been written (`writeAfterInitialize`).

### 2.1 Direction summary
SDK -> CLI (`sdk.mjs:159` methods): `initialize, interrupt, set_permission_mode, set_model, set_max_thinking_tokens, mcp_status, mcp_set_servers, mcp_reconnect, mcp_toggle, mcp_message, rewind_files, stop_task, background_tasks, cancel_async_message, apply_flag_settings, get_settings, get_context_usage, get_usage, get_hooks_listing, get_task_output, list_permission_rules, update_settings, seed_read_state, set_cwd, reload_plugins, reload_skills, reload_output_styles, rename_session, generate_session_title, side_question, read_file, mcp_read_resource, ... (many more; full union `sdk.d.ts:5211` `SDKControlRequestInner`).
CLI -> SDK: `can_use_tool, hook_callback, mcp_message, elicitation, request_user_dialog, oauth_token_refresh, host_auth_token_refresh, remote_control_work_secret, ui_*` (`sdk.mjs:159` `processControlRequest`). Plus `control_cancel_request` (aborts the handler's AbortSignal; handler may still reply).

### 2.2 `initialize` (SDK -> CLI) `sdk.d.ts:4650`, builder `sdk.mjs:159` `buildInitializeRequest`
Sent immediately after spawn (before any user message). Fields (undefined fields omitted by JSON):
| field | meaning |
|---|---|
| `hooks` | `{ "<HookEvent>": [ {matcher?, hookCallbackIds:["hook_0",...], timeout?} ] }`; ids are `hook_<n>` counter, mapped client-side to callbacks |
| `sdkMcpServers` | names of in-process MCP servers (`["name",...]`) |
| `sdkMcpServerConfigs` | `{name:{timeout?, disableAutoBackground?, tools?:[{name,policy:'always_ask'\|'always_deny'}]}}` |
| `sdkMcpServerManifests` | `{name:{initializeResult, toolsListResult?}}` pre-captured handshake (optional optimization; only when `captureSdkMcpManifests`; the SDK waits <= 250 ms (`F9`) for it). Host MUST still answer `mcp_message` as if absent |
| `jsonSchema` | structured-output schema |
| `systemPrompt` | `string[]` (string option wrapped as `[str]`; `""` when option undefined; preset => omitted) |
| `appendSystemPrompt` | from `systemPrompt:{type:'preset',append}` |
| `systemPromptSnapshot, planModeInstructions, appendSubagentSystemPrompt, toolAliases, codeRunningMcpTools, excludeDynamicSections, title, skills(string[]), agents, promptSuggestions, agentProgressSummaries, forwardSubagentText, supportedDialogKinds, perTaskStopAffordance, rapidFollowupPreempt, forwardNestedSubagentToolCalls, workspaceTrust, plugins, webSearchIsolationExemptMcpServers` | pass-through of options |

Response `SDKControlInitializeResponse` (`sdk.d.ts:4747`): `commands: SlashCommand[]`, `agents: AgentInfo[]`, `output_style`, `available_output_styles[]`, `models: ModelInfo[]`, `account: AccountInfo`,
optional `hooks_applied`, `plugins_applied`, `sdk_mcp_manifests_parked{name:'parked'|'already_connected'|'protocol_version_mismatch'|'malformed'|'not_honoured'}`, `claude_code_version`, `fast_mode_state`, `fast_mode_disabled_reason`.
Note: capabilities are **not** on this response; they're on `system/init.capabilities` (section 3.1).

### 2.3 Simple SDK -> CLI requests (`sdk.d.ts` line given)
```json
{"subtype":"interrupt","cancel_queued":true}            // :4799 -> response {still_queued:[uuid],cancelled?:[uuid]} (interrupt_receipt_v1)
{"subtype":"set_permission_mode","mode":"acceptEdits"}   // :5310
{"subtype":"set_model","model":"claude-opus-..."}        // :5297 omit/null/'default' resets
{"subtype":"set_max_thinking_tokens","max_thinking_tokens":null,"thinking_display":"summarized"}  // :5288
{"subtype":"mcp_status"}   // :4975 -> response {mcpServers:[{name,status:'connected|failed|needs-auth|pending|disabled',serverInfo?,...}]} (sdk.d.ts:1269)
{"subtype":"mcp_set_servers","servers":{"n":{"type":"sdk","name":"n",...settings}|<process cfg>}}  // :4964 -> {added:[],removed:[],errors?} (sdk.d.ts:1338)
{"subtype":"rewind_files","user_message_id":"<uuid>","dry_run":false}   // :5262 -> {canRewind,error?,filesChanged?,insertions?,deletions?}
{"subtype":"stop_task","task_id":"..."}                  // :5323
{"subtype":"mcp_reconnect","serverName":"x"} / {"subtype":"mcp_toggle","serverName":"x","enabled":true}
{"subtype":"cancel_async_message","message_uuid":"..."}  // response {cancelled:bool}
{"subtype":"background_tasks","tool_use_id":"?"}
```
`interrupt()` returns `{still_queued,cancelled?}` or undefined if absent; `interrupt` response arrives *before* the interrupted turn's `result` on a clean interrupt.
`mcp_set_servers`: SDK first diffs in-process servers (connect new, disconnect removed), then sends sdk entries as `{type:"sdk",name,...settings}` merged with non-sdk configs.
`keep_alive`: **not a control_request** -- a top-level stdout frame `{"type":"keep_alive"}` (`sdk.d.ts:5490`), optionally with `startup_mark` string (<= 64 chars); SDK discards it. No SDK-originated keep_alive exists (NOT FOUND).

### 2.4 `can_use_tool` (CLI -> SDK) `sdk.d.ts:4991`, handler `sdk.mjs:159` `processControlRequest`
Request fields: `tool_name, input, tool_use_id, agent_id?, permission_suggestions?: PermissionUpdate[], blocked_path?, decision_reason?, decision_reason_type?('rule|mode|subcommandResults|permissionPromptTool|hook|asyncAgent|sandboxOverride|workingDir|safetyCheck|classifier|other'), classifier_approvable?, title?, display_name?, description?, mcp_server?{name,source}, suppress_always_allow_rule?, default_to_no?, requires_user_interaction?, matched_ask_rule?{source,tool_name,rule_content?}` (+ undocumented in d.ts: `decision_reason_code, server_policy_ask_only, plugin, server_prompt, computer_folder`).
Response `response` object = the callback's `PermissionResult` (`sdk.d.ts:2555`) **plus `toolUseID` = request.tool_use_id** (SDK always overwrites):
```json
{"behavior":"allow","updatedInput":{...},"updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"ls:*"}],"behavior":"allow","destination":"session"}],"toolUseID":"toolu_x","decisionClassification":"user_temporary"}
{"behavior":"deny","message":"why","interrupt":true,"toolUseID":"toolu_x"}
```
`PermissionUpdate.type`: `addRules|replaceRules|removeRules|setMode|addDirectories|removeDirectories` (`sdk.d.ts:2574`); destinations `userSettings|projectSettings|localSettings|session|cliArg`. `updatedInput` is optional in the type; whether the CLI requires it on allow: **NOT FOUND** (SDK doesn't default it; the official docs pass the original input back -- safest is to echo `input`).
Without `canUseTool` the CLI never sends this (no `--permission-prompt-tool=stdio`). If the SDK has no callback it answers an error `canUseTool callback is not provided.`

### 2.5 `hook_callback` (CLI -> SDK) `sdk.d.ts:5411`
```json
{"subtype":"hook_callback","callback_id":"hook_0","input":{"hook_event_name":"PreToolUse","session_id":"..","transcript_path":"..","cwd":"..","tool_name":"..","tool_input":{},"tool_use_id":".."},"tool_use_id":"toolu_x"}
```
`input` = `HookInput` union (`BaseHookInput` + event fields; events incl. PreToolUse, PostToolUse, PostToolUseFailure, UserPromptSubmit, Stop, SubagentStart/Stop, PreCompact, PostCompact, PermissionRequest, PermissionDenied, PostToolBatch, Pre/PostModelSwitch, ...). Response `response` = the callback's `HookJSONOutput` returned verbatim: sync form `{continue?,suppressOutput?,stopReason?,decision?:'approve'|'block',systemMessage?,reason?,hookSpecificOutput?:{hookEventName,...}}` (`sdk.d.ts:9953`), or async `{async:true,...}`. Unknown `callback_id` -> error `No hook callback found for ID: X`.

### 2.6 `mcp_message` -- in-process "sdk" MCP servers (both directions)
CLI -> SDK (`sdk.d.ts:4910`):
```json
{"type":"control_request","request_id":"r1","request":{"subtype":"mcp_message","server_name":"pi-tools","message":{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{...}}}}
```
SDK handling (`sdk.mjs:159` `processControlRequest` + `sdk.mjs:163` `handleMcpControlRequest`): unknown server -> error `SDK MCP server not found: X`.
- If `message` has `method` **and** non-null `id` (a request, `I9`): key `"<server>:<id>"` is registered in `pendingMcpResponses`, the message is delivered to the McpServer transport (`transport.onmessage(msg)`), and the control_response is sent when the server calls `transport.send(resp)` with the same id: `{"response":{"mcp_response":<JSON-RPC response object>}}`.
- Otherwise (notification or response): `onmessage(msg)` and immediate `{"mcp_response":{"jsonrpc":"2.0","result":{},"id":0}}`.
SDK -> CLI: any message the server **sends** that doesn't match a pending key (server-initiated requests/notifications, e.g. `notifications/tools/list_changed`) becomes a new control_request `{"subtype":"mcp_message","server_name":..,"message":..}` with `request_id = Oa()`.
Transport class `uv` (`sdk.mjs:157` `class uv`): `start()` no-op; `send(m)` -> `sendMcpServerMessageToCli(server,m)`; `close()` sets closed + `onclose`. `connectSdkMcpServer(name,{instance})` calls **only** `instance.connect(transport)` (`sdk.mjs:159`). Connect failure deletes the server entry.
Typical sequence the CLI drives: `initialize` (params `protocolVersion`, caps) -> `notifications/initialized` -> `tools/list` -> `tools/call`. For the optional manifest capture the SDK itself sends `initialize` with `protocolVersion:"2025-11-25"`, `capabilities:{}`, `clientInfo:{name:"claude-code",title:"Claude Code",version:"0.3.308"}` (ids prefixed `sdk-manifest-capture:`).
`createSdkMcpServer(opts)` (`sdk.mjs:169`, `sdk.d.ts` `createSdkMcpServer`) returns `{type:"sdk", name, instance: McpServer}`; the SDK treats any `mcpServers` entry with `type==="sdk" && instance` as in-process. Repo's own minimal equivalent: `src/mcp-server.ts:140` (builds an `McpServer` directly, sets handlers manually).
Tool names the model sees: `mcp__<server_name>__<tool>` (convention; from mcp_call docs `sdk.d.ts:4860`). Hard consequence: a Go port must implement MCP server-side `initialize`, `tools/list`, `tools/call` JSON-RPC itself.

### 2.7 Other CLI -> SDK requests
- `elicitation` (`sdk.d.ts:3978`): fields `mcp_server_name, message, mode, url, elicitation_id, requested_schema, title, display_name, description`; no handler => respond `{"action":"decline"}`.
- `request_user_dialog` (`dialog_kind, payload, tool_use_id`): no handler => no reply.
- `oauth_token_refresh` -> `{accessToken, reason?}`; `host_auth_token_refresh` -> `{authToken}`.

---
## 3. Messages on stdout (`SDKMessage` union `sdk.d.ts:5556`)
Every frame has `type`; most have `uuid` + `session_id`. Union members: assistant, user, user(replay), result, system(*), stream_event, rate_limit_event, tool_progress, auth_status, tool_use_summary, plus `conversation_reset`, `active_goal`, `autocompact_state`, `prompt_suggestion`, `transcript_mirror` (consumed internally), `keep_alive`, `control_request/response/cancel_request`.
`system` subtypes (grep `subtype:` in sdk.d.ts): `init, status, compact_boundary, api_retry, control_request_progress, model_refusal_fallback, model_refusal_no_fallback, local_command_output, hook_started, hook_progress, hook_response, plugin_install, task_started, task_updated, task_progress, task_notification, background_tasks_changed, thinking_tokens, session_state_changed, worker_shutting_down, commands_changed, notification, files_persisted, memory_recall, elicitation_complete, permission_denied, mirror_error, informational, post_turn_summary, task_summary, session_metadata`.

### 3.1 `system` / `init` (`sdk.d.ts:6167`)
`type:"system", subtype:"init", uuid, session_id, apiKeySource, claude_code_version, cwd, tools:string[], mcp_servers:[{name,status,source?}], model, permissionMode, slash_commands:string[], terminal_slash_commands?, output_style, skills:string[], plugins:[...], plugin_errors?, agents?:string[], betas?:string[], effort?, view_mode?, fast_mode_state?, capabilities?:string[]`.
Known capability strings: `interrupt_receipt_v1, interrupt_cancel_queued_v1, mcp_call_result_not_for_model_v1, mcp_read_resource_v1, mcp_tool_ui_meta_v1` (`sdk.d.ts` grep `_v1`).
`session_state_changed`: `{state:'idle'|'running'|'requires_action'}` (SDK uses `idle` after `result` to end a run; `sdk_host_only:true` frames are swallowed).
`status`: `{status:'compacting'|'requesting'|null, permissionMode?, compact_result?, compact_error?}`.

### 3.2 `assistant` (`sdk.d.ts:3671`)
`type, message: BetaMessage (id,type:"message",role,model,content[],stop_reason,stop_sequence,usage), parent_tool_use_id:string|null, uuid, session_id, error?, request_id?, user_message_uuid(s)?, supersedes?, aborted?, agent_id?, subagent_type?, task_description?, timestamp?, context_usage?, usage_report?`.
`error` enum: `authentication_failed oauth_org_not_allowed account_on_hold verification_required billing_error rate_limit overloaded invalid_request model_not_found server_error unknown max_output_tokens cloud_credential_error`.
With `includePartialMessages`, the same `message.id` is also streamed via `stream_event`; the complete `assistant` frame (one per content block group) follows; regroup by `message.id`.

### 3.3 `user` (`sdk.d.ts:6508` SDKUserMessage, `:6606` SDKUserMessageReplay)
`type:"user", message: MessageParam{role:"user",content}, parent_tool_use_id, session_id?, uuid?, isSynthetic?, tool_use_result?:unknown, priority?:'now'|'next'|'later', origin?, shouldQuery?, timestamp?, client_composed?, agent_id?`. Replay variant additionally `isReplay: true` (+ `uuid`, `session_id` required, `file_attachments?`); produced with `--replay-user-messages` for echoed stdin user turns. Tool results appear as `user` frames with `tool_result` content blocks and `tool_use_result` carrying the raw tool output; `isSynthetic:true` marks CLI-injected prompts.

### 3.4 `result` (`sdk.d.ts:5973` success, `:5910` error)
Common: `type:"result", subtype, uuid, session_id, duration_ms, duration_api_ms, is_error, num_turns, stop_reason:string|null, total_cost_usd, usage (NonNullableUsage), modelUsage:{model:ModelUsage}, permission_denials:[{tool_name,tool_use_id,tool_input}], terminal_reason?, queued_turn_count?, fast_mode_state?, origin?, user_message_uuid(s)?, result_index?`.
- `subtype:"success"`: + `result:string`, `structured_output?`, `deferred_tool_use?`, `api_error_status?`, timing extras (`ttft_ms`, ...).
- `subtype`: `error_during_execution | error_max_turns | error_max_budget_usd | error_max_structured_output_retries`: + `errors:string[]`, `startup_failure_reason?`.
- `terminal_reason` enum (`sdk.d.ts:10023`): blocking_limit rapid_refill_breaker prompt_too_long image_error model_error api_error malformed_tool_use_exhausted aborted_streaming aborted_tools stop_hook_prevented hook_stopped tool_deferred max_turns background_requested completed budget_exhausted structured_output_retry_exhausted tool_deferred_unavailable turn_setup_failed.
- A `success` result with `num_turns:0` and `result:""` is a "model-less turn" (SDK special-cases it).
- SDK error text for `is_error` results: `subtype=="success" ? result : errors.join("; ")`.

### 3.5 `stream_event` (`sdk.d.ts:5717`)
`{"type":"stream_event","event":<Anthropic raw SSE event>,"parent_tool_use_id":null,"uuid":"..","session_id":"..","ttft_ms?":..,"user_message_uuid?":..}`. `event.type` in `message_start | content_block_start | content_block_delta (text_delta|thinking_delta|signature_delta|input_json_delta|citations_delta) | content_block_stop | message_delta ({delta:{stop_reason,stop_sequence},usage}) | message_stop | ping`. Only with `--include-partial-messages`.

### 3.6 Others
- `rate_limit_event` (`sdk.d.ts:5871`): `{type, rate_limit_info:{status:'allowed'|'allowed_warning'|'rejected', resetsAt?:epoch s, rateLimitType?:'five_hour|seven_day|seven_day_opus|seven_day_sonnet|seven_day_overage_included|overage', utilization?, overageStatus?, overageResetsAt?, overageDisabledReason?, isUsingOverage?, overageInUse?, overageEnabled?, surpassedThreshold?, limitScope?, errorCode?, ...}, uuid, session_id}`.
- `tool_progress` (`:6404`): `tool_use_id, tool_name, parent_tool_use_id, elapsed_time_seconds, task_id?, heartbeat?, uuid, session_id`.
- `auth_status` (`:3762`): `isAuthenticating, output:string[], error?, uuid, session_id`.
- `tool_use_summary` (`:6425`): `summary, preceding_tool_use_ids[], uuid, session_id`.
- `system/api_retry` (`:3649`), `system/compact_boundary` (`:3819`), `system/task_*`, `system/hook_*`: field lists in d.ts at those lines.

---
## 4. Sending user turns (`sdk.mjs:163` `function Tv(`, `sdk.mjs:159` `streamInput`)
Wire shape (SDKUserMessage):
```json
{"type":"user","session_id":"","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"parent_tool_use_id":null}
```
- **Single-prompt mode** (`prompt` is a string): SDK writes exactly the object above (`session_id:""`, content as a text-block array), `isSingleUserTurn=true`. When the run ends (a `result` arrives and `session_state_changed idle`, or the 600 s ceiling after the last result) it calls `endInput()` => **stdin closed**; the CLI then exits.
- **Streaming-input mode** (`prompt` is `AsyncIterable<SDKUserMessage>`): each yielded object is written verbatim (`JSON + "\n"`), plus `client_composed:true` if `verbatimPrompts`. SDK sets `runEnded=false` per message. When the iterable finishes: if the query has bidirectional needs (sdk MCP servers, hooks, canUseTool, elicitation...) it first **waits for the run to end** (`waitForRunEnd`), then closes stdin. So a `result` does **not** end the process in streaming mode; the consumer closing the iterable (or `close()`) does.
- Optional fields on user frames: `uuid` (lets `rewind_files`/`cancel_async_message`/`user_message_uuid` correlate), `priority`, `parent_tool_use_id` (for subagent-addressed), `shouldQuery:false` (append without triggering a turn).
- Initialize must precede all of these (other writes queue behind it).
- Query-level end signals: stdout EOF; `result` frame (turn end); `session_state_changed` `idle`.

---
## 5. cc-session-io (`node_modules/cc-session-io`, `dist/index.d.ts`, bundle `dist/chunk-7JUXR4CP.js`)

### 5.1 Files
- Dir: `<claudeDir>/projects/<hash(normalize(projectPath))>/<sessionId>.jsonl` (`chunk-7JUXR4CP.js:32-56`). `claudeDir = arg ?? $CLAUDE_CONFIG_DIR ?? ~/.claude`.
- `normalizeProjectPath` = `realpathSync(p).normalize("NFC")` (fallback NFC of raw p).
- `projectPathToHash`: `p.replace(/[^a-zA-Z0-9]/g,"-")`; if length <= 200 return it; else `sanitized[:200] + "-" + abs(djb2(p)).toString(36)` where `h=(h<<5)-h+charCode |0` (32-bit). **Bun CLI uses `Bun.hash` (wyhash) instead**, so >200-char paths mismatch (README "Known limitation"); reading falls back to prefix match.
- Companion dir: `<sessionId>/` (`subagents/`, `tool-results/`); `deleteSession`/`clear()` remove `.jsonl` and that dir.
- `save()`: mkdir -p, `writeFileSync` (new) or `appendFileSync` (existing), `JSON.stringify(rec)+"\n"` each, UTF-8.

### 5.2 Records
Base fields written on user/assistant/attachment (`baseFields`, `:262-281`):
`uuid` (random UUID), `parentUuid` (previous chain record's uuid; null for first), `sessionId`, `timestamp` (ISO; starts `Date.now()`, +1 ms per record so strictly increasing), `isSidechain:false`, `cwd`, `userType:"external"`, `version` (default `"2.1.83"`), `gitBranch` (default `"HEAD"`), `slug` (random adjective-noun-..), `entrypoint:"cli"`.
```json
{"type":"user", ...base, "message":{"role":"user","content":"text" | [blocks]}}            // optional promptId, permissionMode, isMeta
{"type":"assistant", ...base, "requestId":"req_syn_<24hex>","message":{"id":"msg_syn_<24hex>","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[..],"stop_reason":"end_turn"|"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"attachment", ...base, "attachment":{"type":"file"|"edited_text_file"|"skill_listing"|...,...}}
```
Tool results are `user` records with `content:[{type:"tool_result",tool_use_id,content,is_error?}]`. `addToolCalls` synthesises ids `toolu_syn_<20hex>`.
Content blocks: `text, thinking{thinking,signature}, tool_use{id,name,input}, tool_result, image{source:{type:base64|url}}`.
Other record types (`summary`, `file-history-snapshot`, `queue-operation`, `last-prompt`, `custom-title`, ...) are preserved as `UnknownRecord` (opaque JSON); **their field schemas are NOT defined anywhere in cc-session-io** (no `docs/` shipped). The only chain rule: attachments are chain links (record after an attachment parents to it); queue-operation etc. are chain-external.

### 5.3 API (exports) and bridge usage
`createSession({projectPath,claudeDir?,cwd?,gitBranch?,version?,model?,sessionId?})`, `openSession({sessionId,projectPath,claudeDir?})`, `readSession(path,projectPath?)`, `deleteSession(sessionId,projectPath,claudeDir?)`, `Session{addUserMessage,addAssistantMessage,addToolResults,addToolCalls,addAttachment,dangerousAppendRecord,importMessages(msgs,{attachments:[{afterIndex,attachment}]}),clear,save, records,messages,attachments}`, `parseJsonl(File)`, `serializeRecord/Jsonl`, `repairToolPairing`, `repairWithOrigin`, path helpers (`getClaudeDir, normalizeProjectPath, projectPathToHash, getProjectDir, getSessionPath`).
Bridge imports (`src/index.ts:7`): `createSession, deleteSession, openSession, repairToolPairing`; type-only `JsonlRecord, ImportAttachment` (`src/attachments.ts:11`), `Message` (`src/convert.ts:5`).
- `openSession` + `.records`: read carried attachments before rebuild (`src/index.ts:219`); `deleteSession` then `createSession({sessionId?})` then `importMessages` then `save()` (`:834-846`); `verifyWrittenSession` (`src/session-verify.ts`) checks file exists, non-blank line count == `session.records.length`, first/last `sessionId` match.
- `extractAllToolResults` is **not** from cc-session-io: it's the repo's `src/extract-tool-results.ts:34`.
- `repairToolPairing` / `repairWithOrigin` (`chunk-7JUXR4CP.js:62`): after each assistant message with tool_use ids, the next user message must carry a tool_result for each id; missing => synthetic `{type:"tool_result",tool_use_id,content:"[no tool result recorded]",is_error:true}` prepended; orphan tool_results (id not pending) dropped; trailing unanswered tool_uses get a synthetic user message appended; a first message that becomes empty => `[orphaned tool result removed]` text. `origin[]` maps repaired index -> source index (null = synthesised).
- `importMessages` dispatch: assistant (string wrapped as text) -> `addAssistantMessage`; user string -> `addUserMessage`; user array: tool_result blocks -> `addToolResults`, remaining blocks -> separate `addUserMessage`. Attachments are emitted after the record whose original index equals `afterIndex`.

### 5.4 Resume gotchas (README "Minimum viable record", `dangerousAppendRecord`)
- Minimum for resume: `type, uuid, parentUuid, sessionId, timestamp, message` (assistant message needs `id,type,role,model,content,stop_reason,stop_sequence,usage`). `cwd, version, isSidechain, slug, entrypoint, gitBranch, userType` optional.
- **Dangling `parentUuid` => CC resumes with empty context silently** (no error). The lib throws on it in `dangerousAppendRecord`. Chain must be linear via parentUuid; no SQLite/index needed.
- `sessionId` inside every record must equal the file name id (`dangerousAppendRecord` overwrites it).
- Empty content arrays are rejected by Anthropic; lib throws.
- `projectPath` must be symlink-resolved + NFC to land in the dir CC reads; long-path (>200) hash mismatch under Bun vs Node.
- CC may still flush a late `[Request interrupted by user]` record into an old session after abort (`src/index.ts:203-209`): the bridge rotates to a fresh sessionId after aborts instead of rewriting in place.
- Repo AGENTS.md: `~/.claude/projects/**` is not evidence of CC behaviour (the bridge writes there with `msg_syn_*`/`req_syn_*`; CC re-serializes imported records).

---
## 6. Bridge's concrete invocation (reference config) `src/index.ts:2039-2096`
`query({prompt: asyncIterable, options:{env, tools:[], permissionMode:"bypassPermissions", includePartialMessages:true, systemPrompt:{type:"preset",append?}, extraArgs:{model, "replay-user-messages":null, "strict-mcp-config":null?, "thinking-display":"summarized"?}, effort?, mcpServers:{<sdk server>}, resume?, pathToClaudeCodeExecutable?}})`.
=> argv ~ `--output-format stream-json --verbose --input-format stream-json [--effort=E] --tools= [--mcp-config ..? (sdk servers excluded)] --permission-mode=bypassPermissions --include-partial-messages [--resume=ID] --model=M --replay-user-messages [--strict-mcp-config] [--thinking-display=summarized]` *(the `--tools=` empty-array form is inferred from `de.join(",")`)*.

## 7. Open items (NOT FOUND / not traced)
- `Oa()` request-id generator body; `manifest.json` checksum verification site; `updatedInput` requiredness on allow; schemas of `summary`/`custom-title`/`file-history-snapshot` records; hook event input field lists beyond `BaseHookInput`; full `SlashCommand/ModelInfo/AccountInfo` shapes (`core.d.ts`/`sdk.d.ts`, not expanded here).


---

# Appendix: 0.3.308 to 0.3.310 diff

# claude-agent-sdk 0.3.308 → 0.3.310 protocol diff

## (a) Version facts
| | 0.3.308 | 0.3.310 |
|---|---|---|
| `version` | 0.3.308 | 0.3.310 |
| `claudeCodeVersion` | 2.1.308 | 2.1.310 |
| `engines` | `{"node":">=18.0.0"}` | same |
| `exports` | `.`, `./core`, `./extract`, `./browser`, `./bridge`, `./sdk-tools`, `./sdk-tools.js` | identical |
| `dependencies` | none | none |
| manifest.json | version 2.1.308, commit 320f66eb…, build 2026-10-22T03:48:37Z | version 2.1.310, commit 0d2056ec…, build 2026-10-23T20:13:54Z (checksums and sizes differ, as expected for new binaries) |

No minimum-CLI-version note was found in either package.json. `sdk-tools.d.ts` and `core.d.ts` are byte-identical in both versions (diff empty).
Hashed chunk files are renamed (`core-36zyfan5` etc. → `core-4pv47a2z` etc.). This is bundler noise; the `core*.mjs` and `bridge.mjs` contents were not diffed.

## (b) Behavioural differences relevant to the stdio protocol
| Area | Change |
|---|---|
| CLI flags | **None.** The sets of `"--…` strings in `sdk.mjs` are identical. The argv builder window starting at `"--output-format","stream-json","--verbose","--input-format","stream-json"` has the same flag sequence in both. Only minified identifiers differ (`Ke`→`We`, etc.). |
| Control request subtype (new) | `submit_survey_feedback`. In 0.3.310 `sdk.mjs` has `async submitSurveyFeedback(e){return(await this.request({subtype:"submit_survey_feedback",...e})).response}` next to `submit_feedback`. The 0.3.308 bundle has neither the method nor the subtype. It is **not** in `sdk.d.ts` (the type is not public). Payload fields are an unspecified `...e` spread. A Go port can skip it unless it wants parity. |
| Control request fields | `mcp_status` gains optional `liveReply?: boolean`. Per its docs, when true and the CLI writes through CCR, the reply goes out as a `control_response` marked `live_only: true`. Stdio hosts always get ordinary replies. Safe to omit. |
| Other control subtypes | Otherwise identical. The `subtype:"…"` sets differ only by `submit_survey_feedback`. |
| stdout message types | Identical `type:"…"` sets except `submit_survey_feedback`, which is the request above. No new SDKMessage types. |
| `set_max_thinking_tokens` | Doc-only change. The `highlights` error conditions now read: experimental betas off, a Claude 3 model (except on Microsoft Foundry), or Bedrock (Mantle included), Vertex or a `gateway` API provider with a model older than Opus 4.7. No field changes. |
| `permission_request` `requires_user_interaction` | Doc-only. Adds the case where an MCP server's name spells a claude.ai connector key but the server did not come from claude.ai or the session launcher. Field shape unchanged. |
| initialize | Field list unchanged (`hooks`, `sdkMcpServers`, `sdkMcpServerConfigs`, …). The `sdkMcpServerConfigs` doc now says "timeout, disableAutoBackground and **connectorId** apply when first registered". `connectorId` is **not** declared in the `.d.ts` shape, only mentioned in the text. I did not verify whether the SDK sends it. The initialize-builder window (`sdkMcpServers…`) differs only in minified identifiers. |
| `Options.settings` (managed-settings text) | Doc-only. `disableAllHooks: true` binds as `allowManagedHooksOnly: true`, and any value except `false`/`"false"` does the same. `allowedHttpHookUrls` / `httpHookAllowedEnvVars` bind only as the empty list. |
| Env vars (names seen in the bundle) | Added in 0.3.310, all CLI-side and none read by the SDK transport: `CLAUDE_CODE_ARTIFACT_ASK_FIRST`, `CLAUDE_CODE_ARTIFACT_SHARE_PROBE_REUSE_MS`, `CLAUDE_CODE_CCR_OWN_CONNECTION_POOL`, `CLAUDE_CODE_DESKTOP_CONNECTOR_NAMES`, `CLAUDE_CODE_DISABLE_ULTRAREVIEW_TRUST_CHECK`, `CLAUDE_CODE_RESUME_SHOWS_CUT_OFF_CALLS`, `CLAUDE_CODE_RETRY_WATCHDOG_SCOPE`, `CLAUDE_CODE_SYNC_PLUGINS_DOWNLOAD_CONCURRENCY`, `CLAUDE_CODE_TOOL_CONTAINER_PLUGINS_NAMED`, `CLAUDE_RUNNER_ALIVE_EVERY_MS`, `CLAUDE_RUNNER_DISABLE_TURN_BEGAN_BEFORE_INIT`. None removed. |
| Process/abort timing | **Identical.** `kill("SIGTERM")`, then `setTimeout(…,5000).unref()` → `kill("SIGKILL")` if `exitCode===null`. `Promise.race([waitForExit(), sleep(2000,signal)])` in cleanup. `killAll()` does `stdin.end()` on win32 and `kill("SIGTERM")` elsewhere. Constant `200` after stderr drained is also unchanged. |
| Reader loop | Identical. It handles `control_response`, `keep_alive` (startup mark string, length-capped) and `transcript_mirror`, and emits `system`/`mirror_error`. |

## (c) Differences that do not matter for a reimplementation
- Minified identifier renames throughout `sdk.mjs`; the bundle grew from 1307250 to 1311912 bytes.
- Whitespace-only blank-line changes in `sdk.d.ts` (several `+` empty lines). This probably means internal fields were stripped from the public typings, but no names changed.
- `sdk.d.ts` grew from 557010 to 559019 bytes, all from the doc text above.
- `extractFromBunfs.js` is hardened (relevant only if embedding the binary). The extraction dir moved from `{tmpdir}/claude-{uid}` to `{tmpdir}/claude-agent-sdk-{uid}` (`claude-agent-sdk` on Windows). It now creates the dir 0700 and checks ownership via an fd, and uses `O_NOFOLLOW|O_NONBLOCK` opens, refuses symlinks or multi-link files, and compares bytes (`isExtractedCopy`). Adds `randomBytes` and a no-file-id check. The npm package is irrelevant to a Go port that spawns an installed `claude`.
- Bundled CLI/SDK-internal telemetry-style env vars listed above (the SDK does not set them).
- `bridge.d.ts` is 18097 → 18665 bytes and `bridge.mjs` is 981346 → 1005012 bytes. These were not diffed in detail. The `./bridge` export is not the stdio transport.

## (d) 0.3.310 native binary platform packages (optionalDependencies, all exactly `0.3.310`)
- `@anthropic-ai/claude-agent-sdk-linux-x64`: 0.3.310
- `@anthropic-ai/claude-agent-sdk-linux-arm64`: 0.3.310
- `@anthropic-ai/claude-agent-sdk-linux-x64-musl`: 0.3.310
- `@anthropic-ai/claude-agent-sdk-linux-arm64-musl`: 0.3.310
- `@anthropic-ai/claude-agent-sdk-darwin-x64`: 0.3.310
- `@anthropic-ai/claude-agent-sdk-darwin-arm64`: 0.3.310
- `@anthropic-ai/claude-agent-sdk-win32-x64`: 0.3.310
- `@anthropic-ai/claude-agent-sdk-win32-arm64`: 0.3.310

`npm view` confirmed all 8 exist at 0.3.310. The list is the same 8 as in 0.3.308, with only the version bumped.

## Method notes
`grep -E` caps repetition at 255, so the large windows were extracted with a small node script instead. Window diffs show only minifier-renamed identifiers. For the argv builder I additionally compared the ordered list of flag literals, and the hashes match. A full semantic diff of every non-flag expression in the argv and initialize windows was not done. The window contents were compared by eye and by diff of the first ~40 lines.
