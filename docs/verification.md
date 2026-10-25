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

## Footprint *(Task 25)*

## Cross-resume *(Task 25)*

## Prompt text *(Task 25)*
