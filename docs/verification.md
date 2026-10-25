# Verification

Evidence that the Go port behaves like the TypeScript bridge against the installed Claude Code. Sections
marked *(Task 25)* are filled in by the final verification task.

## Versions

- Claude Code: `2.1.310 (Claude Code)` (`claude --version`, `~/.local/bin/claude`)
- Agent SDK mirrored: 0.3.310
- TypeScript reference: pi-claude-bridge commit 6aa276a
- Go: see `extensions/pig-claude-bridge/go.mod`

## Contract tests

`extensions/pig-claude-bridge/internal/ccproto/live_contract_test.go` (build tag `live`) pins the undocumented
Claude Code behaviour the bridge depends on. It ports every test in `tests/int-cc-contracts.mjs`, plus
`TestInitializeResultAcceptedByCLI`, `TestToolsCallCarriesToolUseIdMeta` (S2) and `TestDisallowedToolsRemovesBash`.

How to run (by hand, never in CI; it spends a little `claude-haiku-4-5` quota through the real Claude Code login):

```
cd extensions/pig-claude-bridge
go test -race -tags live -run 'Contract|InitializeResult|ToolsCall|Disallowed|ToolsEmpty|Unserved|IsError|ResultModel|IncludePartial|MessageDelta|Streamed|McpMessage|JSONSchema|Resume|Arbitrary|CCWrites|RepairTool|Enable|StrictMcp|ThinkingDisplay|ClaudeMd|IncludeGit' ./internal/ccproto/ -v
```

- Without the tag the file is not compiled; with it, each test skips itself when `claude` is not on PATH.
- Each test runs the child in a fresh scratch directory; the only part of `~/.claude` touched is that
  directory's project hash directory, which is removed afterwards.
- Re-run on every Claude Code bump. A failure names the broken assumption.
- Not covered (as in TypeScript): `DISABLE_AUTO_COMPACT`, steer draining at tool boundaries, `rate_limit_event`
  shape; `updatedInput` on allow cannot be expressed through the client (it answers `can_use_tool` with the
  "not provided" error).

Differences from the TypeScript file:
- "the SDK treats our McpServer as an opaque endpoint" has no Go counterpart; `TestMcpMessageMethodsStayWithinTheServedSet`
  pins the CLI side (it only sends `initialize`, `notifications/initialized`, `tools/list`, `tools/call`).
- "a streamed prompt keeps the query open" is checked on the child process: it stays alive 5 s after `result` with
  stdin open and exits once input ends.

Observed (recorded, not asserted):
- After the prompt-too-long `is_error` result, ending input makes the CLI exit with code 1 and empty stderr
  (`Claude Code process exited with code 1. stderr: `), where the TypeScript SDK throws "too long".
- The CLI's MCP `initialize` params: `protocolVersion` `2025-11-25`, `capabilities` `{}`, `clientInfo`
  `{name: claude-code, title: Claude Code, description, websiteUrl, version: 2.1.310}`.
- `TestUnservedToolUseIsAnsweredByCCNeverDispatched`: the model declined the bait this run, so only the
  never-dispatched half was exercised.

Run on 2026-10-25, claude 2.1.310 (all 22 contract tests passed; `-v` output trimmed of `=== RUN` lines and the
unrelated unit tests the pattern also matched):

```
--- PASS: TestToolsCallCarriesToolUseIdMeta (7.01s)
--- PASS: TestToolsCallNamesTheBareToolNotTheMcpAlias (7.89s)
--- PASS: TestToolsEmptyExposesNoBuiltinTools (6.66s)
    live_contract_test.go:441: model declined the bait; nothing more to pin
--- PASS: TestUnservedToolUseIsAnsweredByCCNeverDispatched (11.36s)
--- PASS: TestDisallowedToolsRemovesBash (13.15s)
    live_contract_test.go:515: exit after the failure result: Claude Code process exited with code 1. stderr: 
--- PASS: TestIsErrorCanBeTrueOnAResultWhoseSubtypeIsStillSuccess (4.53s)
--- PASS: TestResultModelUsageReportsTheServedContextWindow (7.79s)
--- PASS: TestIncludePartialMessagesYieldsTheStreamEventShapesProcessStreamEventDestructures (8.09s)
--- PASS: TestMessageDeltaUsageCarriesThinkingTokensNestedUnderOutputTokensDetails (6.70s)
--- PASS: TestAStreamedPromptKeepsTheQueryOpenPastResultUntilInputEnds (12.65s)
--- PASS: TestMcpMessageMethodsStayWithinTheServedSet (15.95s)
    live_contract_test.go:688: MCP initialize params: map[capabilities:map[] clientInfo:map[description:Anthropic's agentic coding tool name:claude-code title:Claude Code version:2.1.310 websiteUrl:https://claude.com/claude-code] protocolVersion:2025-11-25]
--- PASS: TestInitializeResultAcceptedByCLI (9.08s)
--- PASS: TestJSONSchemaIsServedVerbatimNestedObjectsAndAnyOfConstSurvive (9.76s)
--- PASS: TestResumeRereadsTheJSONLFromDiskOnEveryCall (12.64s)
--- PASS: TestArbitrarySanitizedToolUseIdsInAnImportedTranscriptResumeFine (7.14s)
--- PASS: TestCCWritesEachToolResultOfAParallelTurnAsItsOwnTranscriptRecord (8.32s)
--- PASS: TestRepairToolPairingKeepsEveryResultOnlyWhenTheyShareOneUserMessage (0.00s)
--- PASS: TestEnableClaudeaiMcpServersZeroSuppressesClaudeAiCloudMcpServers (8.12s)
--- PASS: TestStrictMcpConfigSuppressesFilesystemMcpServers (10.72s)
--- PASS: TestThinkingDisplaySummarizedIsStillAnAcceptedFlagValue (6.91s)
--- PASS: TestClaudeMdExcludesPreventsNativeAgentsMdFromDuplicatingForwardedInstructions (9.51s)
--- PASS: TestIncludeGitInstructionsFalseStripsGitStatusAndKeepsThePresetStaticAcrossGitTransitions (19.87s)
PASS
ok  	github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto	206.047s
```

### Fix-round re-run

Review fixes: bounded the exit waits (15 s), asserted the recorded exit behaviour (non-nil exit error, no "too long" in it
or in stderr) and added a `Read` positive control to `TestDisallowedToolsRemovesBash`. Compile check, which costs no quota:
`cd extensions/pig-claude-bridge && gofmt -l . && go vet -tags live ./internal/ccproto/` (clean). Targeted re-run with
`claude --version` = `2.1.310 (Claude Code)`:

```
go test -race -tags live -count=1 -run 'TestIsError|TestDisallowedToolsRemovesBash' ./internal/ccproto/ -v
--- PASS: TestDisallowedToolsRemovesBash (13.30s)
--- PASS: TestIsErrorCanBeTrueOnAResultWhoseSubtypeIsStillSuccess (4.69s)
PASS
ok  	github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto	19.779s
```

## Live integration *(Task 25)*

Part 1 of 3 (Task 25a): the harnesses, the four shell scripts, the quick gate and one RPC test. Parts 25b/25c add the
remaining RPC tests. Everything below spends real Claude Code quota and is run by hand, never in CI.

Layout: `scripts/live/{lib.sh,verify.sh,run-all.sh,int-smoke.sh,int-multi-turn.sh,int-cache.sh,int-cache-git.sh}` and
`tests/live/` (its own zero-dependency Go module, every file behind `//go:build live`).

Environment contract (`lib.sh` and `tests/live/harness.go` agree): one work directory per run
(`CLAUDE_BRIDGE_TEST_WORK_DIR`, default `scripts/live/work/<timestamp>`), logs in `CLAUDE_BRIDGE_TEST_LOG_DIR`
(default `<work>/logs`); fresh `PIG_HOME`/`PIG_CODING_AGENT_DIR` under the work dir with settings
`{"defaultProvider":"claude-bridge","defaultModel":"claude-haiku-4-5"}`; `PI_CODING_AGENT_DIR` and `PIG_USE_PI_DIRS`
unset; `-e <repo>` on every `pig` call; real `HOME` (Claude Code's login); `CLAUDE_BRIDGE_DEBUG=1` with a per-test
`CLAUDE_BRIDGE_DEBUG_PATH`. The alternative provider comes only from `CLAUDE_BRIDGE_TESTING_ALT_PROVIDER` /
`CLAUDE_BRIDGE_TESTING_ALT_MODEL`; no `.env*` file is read. When they are unset the AskClaude check prints
`SKIP (CLAUDE_BRIDGE_TESTING_ALT_PROVIDER unset)` and counts as skipped. AskClaude is enabled through the agent-dir
`claude-bridge.json` (`{"askClaude":{"enabled":true}}`): PiG's project config dir name is host-decided, so this is the
stable equivalent of the TypeScript smoke's project `.pi/claude-bridge.json`.

How to run: `scripts/live/verify.sh` (quick gate), `scripts/live/int-*.sh`, `cd tests/live && go test -race -tags live
-count=1 -v ./...`, or `scripts/live/run-all.sh` for everything plus a results table. Scripts fail on a process that
outlives pig (identified by the work dir in its environment); the Go harness records pig's descendants before stopping
it and fails the test on any survivor.

Adaptations from the TypeScript suite:
- `pi` is `pig`; `-e "$DIR"` is `-e "$ROOT"`. The `jq` paths are unchanged: PiG's `--mode json` records have the same
  `.message.role/provider/model`, `.assistantMessageEvent`, `.message.usage` and `tool_execution_start.args` shapes.
- The multi-turn and cache scripts read `package.json`, `README.md`, `tsconfig.json` and `LICENSE` from their working
  directory; this repo has no README or tsconfig and its package is `pig-claude-bridge`, so `lib.sh` writes a fixture
  project (`<work>/project`, name `pi-claude-bridge`, version 4.7.2, target ES2022, this repo's LICENSE) and runs there.
  Prompts and assertions are verbatim; `EXPECTED_VERSION` is read from the fixture.
- The deadlock regex drops the `[id]` segment of the Go debug line: `^\[[0-9]{4}-[0-9]{2}-[0-9]{2}T[^]]+\] (BUG:|WARNING: [0-9]+ MCP handlers still waiting)`,
  with `check_deadlock_logs --selftest` (run first by `run-all.sh`).
- `TestIntSessionNew` ports `int-session-new.mjs` with its markers unchanged and adds an orphan check after `new_session`.

Run on 2026-10-25 (one work dir, `scripts/live/work/gate`): pig `0.5.3+1.4.0.74f46864`, claude `2.1.310 (Claude Code)`,
go `1.27.3 darwin/arm64`; Node was not run by this task (PiG runs it for its own extensions).

The table was assembled by hand from the individual runs above; `run-all.sh` was not run (its row builder is proven
by `run-all.sh --selftest` without quota).

| step | result | seconds |
|---|---|---|
| `verify.sh` (validate-only, int-smoke, idle footprint) | ok | 45 |
| `int-smoke.sh` | PASS: 5 passed | 37 |
| `int-smoke.sh: tool: AskClaude responds` | SKIP (CLAUDE_BRIDGE_TESTING_ALT_PROVIDER unset) | 0 |
| `int-multi-turn.sh` | PASS: 5 of 5 | 76 |
| `int-cache.sh` | PASS: 7 turns, hit 97-99% from turn 2, clean-start 1 / reuse 4 / rebuild 0 | 37 |
| `int-cache-git.sh` | PASS: 12 turns, commit inside turn 6, hit% 98 -> 98 -> 99, clean-start 1 / reuse 7 / rebuild 0 | 73 |
| `TestIntSessionNew` | PASS (clearing marker seen, first post-`/new` syncResult `clean-start`) | 22 |
| deadlock check over the run's logs | no bridge deadlock warnings | 0 |

Orphan check (`TestIntSessionNew`): between turns pig's only descendant was the extension runner; no `claude` process
was alive at that point, so the check passed without a survivor but also without a parked `claude` to observe (it
needs a snapshot taken mid-turn to be non-vacuous). The pre-`/new` runner was gone after the next prompt.

Fix-round re-run (`TestIntSessionNew` only; it now logs the claude count in the snapshot and matches `claude` by the
executable's basename). The orphan check is vacuous here, as the log line says:

```
go test -race -tags live -count=1 -run TestIntSessionNew -v ./...
    int-session-new_test.go:99: claude processes in the pre-/new snapshot: 0 (0 means the orphan check below is vacuous)
    int-session-new_test.go:114: pre-/new descendant pid 80147 still alive: false (.../runner)
--- PASS: TestIntSessionNew (23.77s)
ok  	github.com/balcsida/pig-claude-bridge/tests/live	25.050s
```

### Live integration, part 2 of 3 *(Task 25b)*

Session and compaction RPC tests under `tests/live/`, each run once on its own
(`go test -race -tags live -count=1 -run 'Test<Name>$' -v ./...`) in one work dir (`scripts/live/work/25b`) on
2026-10-25: pig `0.5.3+1.4.0.74f46864`, claude `2.1.310 (Claude Code)`, go `1.27.3 darwin/arm64`. Hand-assembled, one row
per test; SKIP rows are separate. The temp agent dirs of the compaction tests carry `settings.json` only
(`compaction.keepRecentTokens: 50` and so on), the project dir is a temp copy of what the original read from the repo root.

| test | result | seconds |
|---|---|---|
| `TestIntServedWindow` | PASS (`result: served contextWindow=200000 ... registered=200000`) | 9.0 |
| `TestIntCursorAfterTools` | PASS (no rebuild, at least one reuse) | 16.5 |
| `TestIntCompactBaseline` | PASS | 32.9 |
| `TestIntSessionCompact` | PASS (first post-compact sync: rebuild, preserved, same sessionId) | 32.3 |
| `TestIntCompactSplitturn` | PASS (compact returned in 9.3 s, split-turn marker present) | 34.4 |
| `TestIntCompactSecondCompact` | PASS | 52.9 |
| `TestIntBranchSummary` | PASS | 19.9 |
| `TestIntCompactAutoThreshold` | PASS (one threshold compaction, tokensBefore 14953) | 57.0 |
| `TestIntCompactMidturnRebuild` | PASS (after Finding 1 fix: tokens 81303 before the compaction, 12710 after) | 64.9 |
| `TestDeleteSessionPlusCreateSessionSessionIdPreservedAcrossFullWipe` (`internal/ccproto/live_rebuild_test.go`, case 2) | PASS | 12.8 |
| `TestRebuildOverCCWrittenToolUseRecordsResolvesCleanly` (same file, case 3) | PASS | 18.6 |
| `TestDeleteSessionWipesTheCompanionDirectory` (same file, case 4, offline) | PASS | 0.0 |
| deadlock check over the run's logs (10 debug logs) | no bridge deadlock warnings | 0 |

SKIP rows:

| test | result |
|---|---|
| `TestIntSessionResume` | SKIP (CLAUDE_BRIDGE_TESTING_ALT_PROVIDER / CLAUDE_BRIDGE_TESTING_ALT_MODEL unset) |

Findings:
1. Marker parity, `TestIntCompactMidturnRebuild` (fixed): the Go forward passed the bare `session_compact` and dropped
   the event's `reason` and `willRetry`; the TypeScript tags `session_compact:${reason}:willRetry=${willRetry}`
   (`session_tree` is bare there too, so it was already right). The bridge now logs
   `session_compact:threshold:willRetry=false: marking needsRebuild on session <id8>`; the regex stayed verbatim and the
   test passed on its one rerun.
2. `int-session-rebuild.mjs` is not an RPC test (cc-session-io plus the Agent SDK, no pi), so its four cases live beside the
   contract tests in `extensions/pig-claude-bridge/internal/ccproto/live_rebuild_test.go` (`-tags live`, run with
   `go test -race -tags live -count=1 -run 'TestDeleteSessionPlusCreateSession|TestRebuildOverCCWritten|TestDeleteSessionWipes' ./internal/ccproto/`).
   Case 1 (openSession + clear + re-add) is not duplicated: `ccsession` has no Open+Clear primitive, and
   `TestResumeRereadsTheJSONLFromDiskOnEveryCall` already asserts that a rewritten session file is what the next resume
   sees with no stale content. Case 4: `ccsession.Delete` already removes the companion directory, no parity gap.
   These three add to the 22 contract tests counted above (that run predates them).

### Live integration, part 3 of 3 *(Task 25c)*

The remaining integration tests, the cross-resume script and this results section. Each test ran once on its own
(`go test -race -tags live -count=1 -run '^Test<Name>$' -v ./...`) in one work dir (`scripts/live/work/25c`) on 2026-10-25.

Versions: pig `0.5.3+1.4.0.74f46864`, claude `2.1.310 (Claude Code)`, go `1.27.3 darwin/arm64`; node `v26.12.1`.

| item | result | seconds |
|---|---|---|
| `TestIntToolMessage` (9 subtests, one pig process) | PASS (all nine subtests; steer reached CC as a `queued_command` between the tool result and the next assistant record) | 156.8 |
| `TestIntAttachmentRebuild` (runs the two `internal/ccproto` tests below) | PASS (token returned with the carry; control did not return it) | 23.5 |
| `TestIntParallelToolResults` (runs the two `internal/ccproto` tests below) | PASS (3 results on disk; Claude read all three back) | 8.8 |
| `TestIntShutdownKillsCcAbort` | PASS (the `claude` under pig was gone within 5 s of the abort) | 11.2 |
| `TestIntShutdownKillsCcSigterm` | PASS (pig exited, no `claude` survivor) | 8.4 |
| `TestIntShutdownKillsCcStdinClose` | PASS (pig exited on end of input, no `claude` survivor) | 8.3 |
| `TestIntSubagentRpivCodebaseLocator` | PASS (foreground and background Agent runs, reentrant marker seen, no stuck-handler signature) | 53.9 |
| `int-cross-resume.sh` | PASS: 7 checks (fix-round rerun, see Cross-resume; the earlier runs read an untruncated debug log) | 10 |
| deadlock check over the run's logs (6 debug logs) | no bridge deadlock warnings | 0 |

SKIP rows:

| test | result |
|---|---|
| `TestIntImageSession` | SKIP (CLAUDE_BRIDGE_TESTING_ALT_PROVIDER / CLAUDE_BRIDGE_TESTING_ALT_MODEL unset) |

Adaptations from the TypeScript tests:
- `int-attachment-rebuild.mjs` and `int-parallel-tool-results.mjs` are not RPC tests: they call the conversion and
  session code and the Agent SDK directly, which `tests/live` (a separate zero-dependency module) cannot import. As for
  `int-session-rebuild` in 25b, the bodies are `-tags live` tests in `extensions/pig-claude-bridge/internal/ccproto`
  (`live_attachment_test.go`, `live_parallel_results_test.go`), and `tests/live/int-attachment-rebuild_test.go` and
  `int-parallel-tool-results_test.go` run them with `go test -tags live` and fail on a non-pass. Differences from the
  originals: the resumed query runs with no builtin tools (`--tools ""`, as the 25b rebuild tests), the attachment
  fixture is a temp dir outside the scratch project, and the scratch project replaces `process.cwd()`.
- `int-tool-message.mjs`: `-e ./tests/fixtures/slow-tool-extension.ts` becomes the absolute fixture path; each
  `it` is a `t.Run` with its own bound (the `{timeout}` of the original, run in a goroutine so a hang fails the subtest);
  `afterEach` restarts pig if it died. `getProjectDir(harness.DIR)` becomes the project hash of the harness cwd
  (`<work>/project`), read through the debug log's `query done, session=<id8>`. Markers, prompts and assertions are verbatim.
- `int-shutdown-kills-cc.mjs`: the two TypeScript cases (abort; SIGTERM) are kept and a third added for the stdin close,
  PiG's documented shutdown path. The chain is pig -> extension runner -> `claude`; `claude` is found by the executable's
  basename in the harness's descendant listing, with the same 5 s grace. A survivor would be a Finding; there was none.
- `int-image-session.mjs`: `--provider/--model` come from the environment only; the cleanup removes the project hash
  directory of the test's own cwd.
- `int-subagent-rpiv-codebase-locator.mjs`: ported, not skipped. `pig install npm:@tintinweb/pi-subagents@0.14.3
  --validate-only --json` validates (tools `Agent`, `get_subagent_result`, `steer_subagent`), and the Agent tool runs under
  pig. `PI_CODING_AGENT_DIR` becomes `PIG_CODING_AGENT_DIR` in a temp agent dir; the agent file is our copy at
  `tests/fixtures/agents/codebase-locator.md`, placed in the project's `.pi/agents/` (the directory pi-subagents reads).

## Footprint *(Task 25)*

Idle `pig --mode rpc` with the extension loaded and no prompt sent (`verify.sh` step 3, `footprint -p`, physical
footprint): **32 MiB across 2 processes** (pig 26 MiB, extension runner 6 MiB). Method: descendants of the pig pid, as
in SoL-PiG's `scripts/live/verify.sh`; stdin is a fifo held open until measured.

| side | version | command | MiB | processes | Claude Code at idle |
|---|---|---|---|---|---|
| pig (this port) | pig `0.5.3+1.4.0.74f46864` | `pig --mode rpc`, extension loaded, `verify.sh` step 3 | 32 (pig 26 + runner 6) | 2 | not spawned until the first prompt |
| pi with the TypeScript bridge | pi 1.4.0 | `pi --no-session -ne -e . --mode rpc`, idle before any prompt, fresh `PI_CODING_AGENT_DIR` | 205 (controller measurement, recorded as given) | 1 | not spawned until the first prompt |

Same descendant method on both sides. The TypeScript row was measured by the controller, not by this task.

## Cross-resume *(Task 25)*

Forward direction (a conversation recorded by the TypeScript bridge, resumed by this port): `scripts/live/int-cross-resume.sh`.

`tests/fixtures/pi-history-310.jsonl` is not a pi session file: it is the bare list of the 310 pi messages a real
claude-bridge session held (one JSON message per line, no header, no entry ids, no `cwd`; text, arguments and results are
placeholders such as `t0:128`). The script wraps it into a version-3 session file in the temp project (header with that
project's `cwd`, one `message` entry per line chained by `parentId`) and resumes it with `pig --session <file> -e <repo>
--mode json -p '<question>'`. PiG accepted the file, so the RPC `switch_session` fallback and the foreign-`cwd` case did not
arise (the header `cwd` is the temp project). The question, "What was the exact text of my very first message in this
conversation? Reply with just that text.", can only be answered from the recorded history (`t0:128`).

| check | result |
|---|---|
| the answer quotes the first message of the recorded history | PASS (`t0:128`) |
| the first turn's `syncResult` is `path=rebuild` | PASS |
| the rebuilt Claude Code session file exists under the project hash of the temp dir | PASS (`<claude dir>/projects/<hash>/<id>.jsonl`) |
| the first record is `type: user` | PASS |
| at least 311 records (324: the 310 messages as 297 rebuilt records, plus Claude Code's own records for the new turn) | PASS |
| the records chain (first record a root, every `parentUuid` names an earlier record) | PASS |
| `check_deadlock_logs` over the run | no bridge deadlock warnings |

The first run failed the chain check as first written (each `parentUuid` equal to the previous record's `uuid`): Claude Code
appends `queue-operation`, `attachment` and similar records of its own after the rebuilt ones, which break a strict
previous-record chain. The rebuilt records themselves chain strictly (checked by hand on that run's file). The check was
changed to the referential form above and the script rerun once. That rerun shared its work dir with the first run and
the script did not truncate the debug log, so its `path=rebuild` check could have read the first run's line. Fix round: the
script now truncates `$DEBUG_LOG` before the `pig` call and the check reads only this run's log; rerun once more, all seven
checks passed (10 s). Output of that rerun:

```
Answer: t0:128
PASS: the answer quotes the first message of the recorded history (t0:128)
First sync: syncResult: path=rebuild
PASS: the first turn rebuilt a Claude Code session (syncResult: path=rebuild)
PASS: the rebuilt Claude Code session file exists under the project hash
Records: 324, first type: user
PASS: the first record is a user record
PASS: the session holds at least the 310 recorded messages plus the new turn (>= 311 records)
PASS: the records chain in order (first record is a root, every parentUuid names an earlier record)
Failed: 0
```

| direction | status |
|---|---|
| TypeScript-recorded session resumed by this port | PASS (above) |
| pig-created session resumed by the TypeScript bridge under pi | **pending: the controller's manual check** (not run by this task; pi is not run here) |

## Prompt text *(Task 25)*

`grep -rn 'prompt-capture:'` over every debug log of the 25a-25c work dirs (`gate`: 6 logs, `25b`: 10, `25c`: 6):
no line in any log of the main agent, so every main-agent turn took the exact-key capture path (PiG handed the same system
prompt to `before_agent_start` and `streamSimple`).

Finding (not a regression, recorded for the controller): two `prompt-capture: no match` lines, both in
`subagent-rpiv-codebase-locator-debug.log`, one per Agent run (foreground and background):

```
prompt-capture: no match for 4133-char system prompt. closest known (26567-char) shares its first 0 chars and diverges at offset 0: "<active_agent name=\"codebase-locator\"/>\n\nYou are a pi coding" known keys=1
```

The prompt is the subagent's own session prompt (`<active_agent name="codebase-locator"/>` and the agent file), which
pi-subagents builds for its child session; it never passes the parent agent's `before_agent_start`, so no capture exists
for it. The bridge logged `DIAG: prompt_capture_unresolved` and the test passed. The original TypeScript test does not read
this line, and whether the TypeScript bridge logs the same miss for that child prompt was not checked (pi is not run here).

Spike S3 (the system message as a `sections` map): PiG's context carries the system message with empty `content` and a
`sections` map (and `toolsAdded`), not a plain string. The bridge does not use that map to find the capture: it keys on the
plain `systemPrompt` string of the provider context and on the `systemPromptOptions` it recorded at `before_agent_start`
(whose `sections`, an ordered list built in `extension.go`, feed the capture's inputs). The map in the system message is
only read when the transcript is rebuilt (`convert/transcript.go` replays the patches). From the code and the absence of
any miss above; the `sections` map path has no separate live probe in this task.
