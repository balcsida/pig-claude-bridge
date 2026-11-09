package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/models"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/promptcapture"
)

// claudeMdExcludes keeps Claude Code from loading instruction files pi already forwards.
var claudeMdExcludes = []string{"**/CLAUDE.md", "**/AGENTS.md", "**/.claude/rules/**"}

// The prompts a continuation query opens with: the pi turn goes on, but its last
// message is a tool result rather than a prompt, and Claude Code cannot resume a
// session without one.
const (
	continueAfterRewritePrompt = "[Your context was compacted. What precedes this is a summary plus the most recent messages, " +
		"ending with the tool result you were waiting for. Continue the task from there.]"
	continueAfterResumePrompt = "[Your session was interrupted and resumed. What precedes this ends with the tool result " +
		"you were waiting for. Continue the task from there.]"
	discardedQueryReason = "Context changed; this query was discarded."
)

// errorStream is a stream that fails its turn with message. The provider contract
// reports a failed turn on the stream, so any caller sees it, not only pi's agent
// loop.
func errorStream(model map[string]any, message string) *sdk.ModelEventStream {
	stream := sdk.CreateAssistantMessageEventStream()
	stream.Push(map[string]any{"type": "error", "reason": "error", "error": newAssistantOutput(model, "", "error", message)})
	return stream
}

// abortedStream is a stream that ends its turn as aborted.
func abortedStream(model map[string]any) *sdk.ModelEventStream {
	stream := sdk.CreateAssistantMessageEventStream()
	stream.Push(map[string]any{"type": "error", "reason": "aborted", "error": newAssistantOutput(model, "", "aborted", "Operation aborted")})
	return stream
}

// StreamSimple is the provider entry point. PiG calls it for each new prompt and
// each tool result: either the result is delivered to the parked query that owns
// it, or a fresh query starts. ctx is the call's cancellation (pi's abort signal).
// A panic in bridge code becomes an error event, never an escape into the host.
func (b *Bridge) StreamSimple(ctx context.Context, host Host, model, piContext, options map[string]any) (stream *sdk.ModelEventStream, err error) {
	defer func() {
		if r := recover(); r != nil {
			b.debugf("provider: recovered from panic: %v\n%s", r, debug.Stack())
			stream, err = errorStream(model, fmt.Sprintf("claude bridge internal error: %v", r)), nil
		}
	}()
	return b.streamSimple(ctx, host, model, piContext, options), nil
}

// messagesOf is the context's messages, whichever slice type the host decoded them to.
func messagesOf(piContext map[string]any) []map[string]any {
	switch m := piContext["messages"].(type) {
	case []map[string]any:
		return m
	case []any:
		out := make([]map[string]any, 0, len(m))
		for _, raw := range m {
			if msg := obj(raw); msg != nil {
				out = append(out, msg)
			}
		}
		return out
	}
	return nil
}

func (b *Bridge) askClaudeToolName() string {
	if a := b.deps.Config.AskClaude; a != nil && a.Name != nil {
		return *a.Name
	}
	return "AskClaude"
}

// nonSystem is the conversation without pi's system messages.
func nonSystem(messages []map[string]any) []map[string]any {
	all := make([]any, len(messages))
	for i, m := range messages {
		all[i] = m
	}
	return convert.NonSystemMessages(all)
}

func (b *Bridge) streamSimple(ctx context.Context, host Host, model, piContext, options map[string]any) *sdk.ModelEventStream {
	b.showStartupNoticeOnce(host)
	// PiG hands providers a transcript (prompt and tools folded into system
	// messages): fold it back out to the prompt and tools fields every sync and
	// prompt-capture lookup below assumes.
	piContext = convert.ToBridgeContext(piContext)
	messages := messagesOf(piContext)

	// Compaction, branch-summary and bug-report calls use cacheRetention "none". Their
	// prompts bypass the capture boundaries, so they go to an isolated process with no
	// session sync.
	if str(options["cacheRetention"]) == "none" {
		b.debugf("provider: one-off summarizer call (cacheRetention none) routed to isolated summary, msgs=%d", len(messages))
		isolated, _ := b.IsolatedStream(ctx, host, model, piContext, options, "compact-summary", nil, nil) // never errs: a failure is the stream's error event
		return isolated
	}

	stream := sdk.CreateAssistantMessageEventStream()
	piSessionID := str(options["sessionId"])

	lastMsgRole, lastMsg := "", map[string]any(nil)
	if len(messages) > 0 {
		lastMsg = messages[len(messages)-1]
		lastMsgRole = str(lastMsg["role"])
	}
	activeQuery := b.hasActiveQuery()
	b.debugf("provider: streamSimple called, activeQuery=%t, lastMsgRole=%s", activeQuery, lastMsgRole)

	var allResults []convert.McpResult
	var resultCtx *QueryContext
	if len(b.liveContexts()) > 0 {
		allResults, _ = convert.ExtractAllToolResults(messages)
		if len(allResults) > 0 {
			resultCtx = b.contextForToolResults(allResults, piSessionID)
		}
	}

	// pi rewrote its history while this query sat parked at a tool boundary, so the
	// query answers about a conversation that no longer exists. Discard it and let
	// this tool result carry the turn into a fresh query over the rewritten history.
	// The staleness mark is per pi session: a subagent's compaction must not discard
	// the parent's parked query, and vice versa. Extensions can also edit the selected
	// history without a rewrite event; compare the live prefix before feeding this
	// query a result.
	rewrittenUnderQuery := false
	if resultCtx != nil {
		stale := b.contextStale(resultCtx)
		var div Divergence
		if !stale {
			var err error
			if div, err = resultCtx.recording.Divergence(nonSystem(messages)); err != nil {
				// A history that can't be checked can't continue the query either.
				b.discardRewrittenQuery(resultCtx, discardedQueryReason)
				return errorStream(model, err.Error())
			}
		}
		if stale || div.Diverged || div.Duplicate {
			reason := "rewrite-event"
			switch {
			case div.Duplicate:
				reason = "history-diverged at a duplicate record"
			case div.Diverged:
				reason = fmt.Sprintf("history-diverged at message %d", div.Index)
			}
			b.debugf("provider: discarding parked query: %s", reason)
			b.discardRewrittenQuery(resultCtx, discardedQueryReason)
			resultCtx = nil
			rewrittenUnderQuery = true
			// Recomputed, not cleared: a reentrant subagent may still hold a query of its own.
			activeQuery = b.hasActiveQuery()
		}
	}
	if activeQuery && lastMsgRole == "user" && len(allResults) == 0 {
		b.debugf("provider: active query user-only call treated as reentrant fresh query, ctx.msgs=%d", len(messages))
	}

	// --- Tool result delivery ---
	// pi appends tool results to the context and calls back. This turn's results go to
	// the waiting MCP handlers; results that arrive before their handler are queued.
	if resultCtx != nil {
		// User messages (steer or follow-up) pi injected during the active query,
		// appended alongside the tool result.
		var steer []any
		if lastMsgRole == "user" {
			var err error
			if steer, err = b.steerBlocks(messages); err != nil {
				return errorStream(model, err.Error())
			}
		}
		b.claimCurrentPiStream(stream, "tool-result", resultCtx)
		resultCtx.resetTurnState(model)
		resultCtx.mu.Lock()
		bind := resultCtx.bindRunSignal
		resultCtx.mu.Unlock()
		if bind != nil {
			bind(ctx, stream)
		}
		b.reportHeldRateLimit(resultCtx)
		// A rewrite that armed the mark after this query parked gets copied here,
		// though markRebuildForSession usually reaches it directly.
		b.armContextIfRewritten(resultCtx)
		// Delivery is detached because the steer must reach Claude Code's stdin before
		// the tool result does (deliverToolResults), while the provider still returns its
		// stream synchronously. Aborts reach the query through the run binder, not here.
		b.queries.Add(1)
		go func() {
			defer b.queries.Done()
			defer b.recoverHandler("deliverToolResults")
			b.deliverToolResults(context.WithoutCancel(ctx), host, resultCtx, allResults, steer, nonSystem(messages))
		}()
		return stream
	}

	// --- Orphaned tool result (e.g. the user aborted a tool call) ---
	// The query is gone but pi still delivered the result: end the turn so pi waits for
	// the next real user message. Only when this call's own context is cancelled: a
	// result that arrives otherwise (after a restart, a crash, a stall, an overflow or
	// quota retry) has no parked query either and falls through to the fresh-query
	// path, which resumes the Claude Code session and delivers it.
	unparkedToolResult := lastMsgRole == "toolResult" && !rewrittenUnderQuery
	if unparkedToolResult && ctx.Err() != nil {
		b.debugf("provider: orphaned tool result after abort, emitting end_turn")
		// No query owns this result, so there is no context to reset: a throwaway
		// supplies the empty message this turn ends with.
		c := newQueryContext()
		c.resetTurnState(model)
		stream.Push(map[string]any{"type": "done", "reason": "stop", "message": c.turnOutput})
		return stream
	}

	// A fresh request replaces a parked query of this same pi session. Its
	// synchronous progress commit happens before sync takes a state identity.
	if piSessionID != "" {
		for _, parked := range b.liveContexts() {
			if parked.piSessionID == piSessionID {
				b.discardRewrittenQuery(parked, discardedQueryReason)
			}
		}
	}

	return b.freshQuery(ctx, host, stream, model, piContext, messages, options, piSessionID, unparkedToolResult, rewrittenUnderQuery)
}

// freshQuery starts a Claude Code process for this call and streams its turn.
func (b *Bridge) freshQuery(ctx context.Context, host Host, stream *sdk.ModelEventStream, model, piContext map[string]any, messages []map[string]any,
	options map[string]any, piSessionID string, unparkedToolResult, rewrittenUnderQuery bool) *sdk.ModelEventStream {
	// Resolved first: an unaccountable system prompt fails this query before anything
	// is claimed or reset, leaving no half-built query behind.
	mcpTools, customToolNameToSdk, customToolNameToPi := resolveMcpTools(piContext, b.askClaudeToolName())
	readTool := "none"
	for _, tool := range mcpTools {
		if tool["name"] == "read" {
			readTool = "mcp"
		}
	}
	// Built from what pi loaded for this run, so --no-context-files and --no-skills
	// reach Claude Code by leaving nothing to forward. The key is derived from the
	// transcript replay (ToBridgeContext), not from the recorded keys: under a forced
	// prompt the transcript head is projected after turn_start, so the host's system
	// prompt is not the head.
	systemPrompt := str(piContext["systemPrompt"])
	capture, err := b.captures.ResolveOrDerive(systemPrompt)
	systemPromptAppend := ""
	if err == nil && capture != nil {
		systemPromptAppend, err = promptcapture.ProjectPromptCapture(capture, readTool, config.DebugLogPath(b.deps.AgentDir))
	}
	lastMsgRole := ""
	if len(messages) > 0 {
		lastMsgRole = str(messages[len(messages)-1]["role"])
	}
	if err != nil {
		// The resolution and the sendability guard stop a turn that would lose its
		// instructions or leak pi's harness text.
		results, _ := convert.ExtractAllToolResults(messages)
		b.diagDump("prompt_capture_unresolved", map[string]any{
			"promptChars": len([]rune(systemPrompt)), "knownKeys": b.captures.Size(),
			"reentrantUserQuery": b.hasActiveQuery() && lastMsgRole == "user" && len(results) == 0,
			"error":              err.Error(),
		})
		return errorStream(model, err.Error())
	}

	exe, err := b.resolveExecutable(ctx, host)
	if err != nil {
		if ctx.Err() != nil {
			return abortedStream(model)
		}
		return errorStream(model, err.Error())
	}
	env, err := providerEnv(options)
	if err != nil {
		return errorStream(model, err.Error())
	}

	// Readers of a session file wait for the child last writing it to exit, never
	// just for the end of its frames.
	if before := b.sessionStateFor(piSessionID); before != nil {
		if err := b.waitForSessionWriter(ctx, before.CCSessionID); err != nil {
			return abortedStream(model)
		}
	}

	cwd := b.sessionCwdFor(piSessionID)
	// cliModel is the id sent to Claude Code (may carry [1m]); model["id"] is the pi-registered id.
	cliModel := models.ClaudeCodeModelID(str(model["id"]), b.longContext)
	// Read before the sync: with no query parked (an overflow retried after
	// compaction), the session's mark is the only sign of the rewrite.
	historyRewritten := rewrittenUnderQuery || b.hasRewriteMark(piSessionID)
	sum := sha256.Sum256([]byte(systemPromptAppend))
	promptDigest := hex.EncodeToString(sum[:])
	syncResult, err := b.syncSharedSession(host, piSessionID, messages, customToolNameToSdk, cwd, cliModel, promptDigest)
	if err != nil {
		return errorStream(model, err.Error())
	}
	// This query starts from the history pi has now: consume this session's armed
	// rewrite; a sibling session's stays armed for its own queries.
	b.clearRewriteMark(piSessionID)
	history := nonSystem(messages)
	recording, err := NewLiveRecording(&syncResult.Recorded, history)
	if err != nil {
		return errorStream(model, err.Error())
	}
	// A created session that never launches is deleted again below.
	cleanupCreated := func() {
		if syncResult.Created {
			if err := ccsession.Delete(syncResult.TargetSessionID, cwd, ""); err != nil {
				b.debugf("provider: could not delete unlaunched session %s: %v", short(syncResult.TargetSessionID), err)
			}
		}
	}

	promptBlocks, err := b.extractUserPromptBlocks(messages)
	if err != nil {
		cleanupCreated()
		return errorStream(model, err.Error())
	}
	promptText, _ := extractUserPrompt(messages)
	hasPrompt := func() bool { return promptText != "" || promptBlocks != nil }
	// A turn continuing past a rewrite or a resume ends at its tool result, not at a
	// prompt: say what happened rather than falling into the empty-prompt recovery
	// below, which is for a shape we do not expect.
	if historyRewritten && !hasPrompt() {
		promptText = continueAfterRewritePrompt
		b.debugf("provider: continuing the turn after a rewritten history, %d msgs rebuilt", len(messages))
	}
	if unparkedToolResult && !hasPrompt() {
		promptText = continueAfterResumePrompt
		b.debugf("provider: resuming the turn at a tool result with no parked query, %d msgs", len(messages))
	}
	q := newQueryContext()
	q.recording = recording
	// Guard: an empty prompt means the last context message isn't a user message.
	if !hasPrompt() {
		roles := make([]string, len(messages))
		for i, m := range messages {
			roles[i] = fmt.Sprintf("[%d]%v", i, m["role"])
		}
		var shared any
		if st := b.sessionStateFor(piSessionID); st != nil {
			shared = map[string]any{"sessionId": short(st.CCSessionID), "recorded": len(st.Digests)}
		}
		b.diagDump("empty_prompt", map[string]any{
			"contextLength": len(messages), "lastMsgRole": lastMsgRole, "isReentrant": b.hasActiveQuery(),
			"activeQueryContexts": len(b.liveContexts()), "activeQueryExists": q.activeQuery != nil,
			"sharedSession": shared, "messageRoles": strings.Join(roles, " "),
		})
		// Recover with a continuation prompt so Claude Code is not sent an empty text block.
		promptText = "[continue]"
	}

	q.piSessionID = piSessionID
	q.customToolNameToPi = customToolNameToPi
	mcpServer, err := b.buildMcpServer(mcpTools, q)
	if err != nil {
		cleanupCreated()
		return errorStream(model, err.Error())
	}

	// Always a prompt stream rather than a one-shot prompt: a parked input is what lets
	// steers reach Claude Code's stdin mid-turn.
	promptStream := newPromptStream()
	var promptContent any = promptBlocks
	if promptBlocks == nil {
		promptContent = []any{map[string]any{"type": "text", "text": promptText}}
	}
	// A durable tool-result continuation has no user slot. Its synthetic instruction
	// belongs to the following assistant's boundary, not the imported prior result.
	// Like a steer, the prompt is recorded only on CC's replay acknowledgement or,
	// when CC never replays it, on the query's first output (tapRecording): a stdin
	// write does not prove CC persisted its record.
	promptUUID := newUUID()
	if err := recording.Users(history, turnStart(history), promptUUID, false, false); err != nil {
		cleanupCreated()
		return errorStream(model, err.Error())
	}
	q.turnOpener = promptUUID

	providerSettings := b.deps.Config.Provider
	// --strict-mcp-config makes Claude Code use only the servers passed
	// programmatically: filesystem MCP entries are pure token overhead since pi
	// executes the tools.
	strictMcp := providerSettings == nil || providerSettings.StrictMcpConfig == nil || *providerSettings.StrictMcpConfig
	effort := effortForReasoning(model, options["reasoning"])
	extraArgs := []ccproto.ExtraArg{{Key: "model", Value: &cliModel}, {Key: "replay-user-messages"}}
	if strictMcp {
		extraArgs = append(extraArgs, ccproto.ExtraArg{Key: "strict-mcp-config"})
	}
	if effort != "" {
		// Opus 4.7 defaults thinking.display to "omitted"; summarized makes thinking_delta events arrive.
		summarized := "summarized"
		extraArgs = append(extraArgs, ccproto.ExtraArg{Key: "thinking-display", Value: &summarized})
	}
	// includeGitInstructions false drops the gitStatus block from the preset: a
	// git-state transition would rewrite it and bust the prompt cache from there on.
	settings := config.ClaudeCodeSettings(providerSettings)
	settings["claudeMdExcludes"] = claudeMdExcludes
	settings["includeGitInstructions"] = false
	// A user's output style is persona text written for standalone Claude Code; PiG owns the persona here.
	settings["outputStyle"] = "default"
	b.applyFastMode(settings)
	debugFile, onStderr := b.cliDebugOptions("provider")
	opts := ccproto.Options{
		Executable: exe, Cwd: cwd, Env: env, Effort: effort, Resume: syncResult.SessionID,
		ThinkingDisabled: thinkingDisabledForReasoning(model, options["reasoning"]),
		// User settings only (pi-claude-bridge #198): a repo's .claude/settings.json and
		// settings.local.json can carry hooks, apiKeyHelper, env and plugins, and Claude Code
		// trusts the workspace when it isn't interactive, so they would run with no prompt.
		// ~/.claude/settings.json still loads: Bedrock/Vertex users keep env and apiKeyHelper there.
		SettingSourcesSet: true, SettingSources: []string{"user"},
		Tools: []string{}, ToolsSet: true, PermissionMode: "bypassPermissions", IncludePartialMessages: true,
		Settings: settings, DebugFile: debugFile, AppendSystemPrompt: systemPromptAppend, ExtraArgs: extraArgs,
	}
	if syncResult.ForkAt != "" {
		opts.ForkSession, opts.ResumeSessionAt = true, syncResult.ForkAt
	}
	// --session-id names a fresh launch or a fork's target; a prewritten import resumes
	// its own id, and CC rejects --session-id beside it.
	if syncResult.Created && (syncResult.SessionID == "" || syncResult.ForkAt != "") {
		opts.SessionID = syncResult.TargetSessionID
	}
	handlers := ccproto.Handlers{OnStderr: onStderr}
	if mcpServer != nil {
		opts.SDKMcpServers = []string{convert.MCPServerName}
		handlers.OnMcpMessage = func(ctx context.Context, serverName string, message json.RawMessage) (json.RawMessage, bool, error) {
			if serverName != convert.MCPServerName {
				return nil, false, fmt.Errorf("SDK MCP server not found: %s", serverName)
			}
			return mcpServer.Handle(ctx, message)
		}
	}
	b.debugf("provider: fresh query model=%s msgs=%d tools=%d resume=%s fork=%s effort=%s strictMcp=%t piSession=%s prompt=%s",
		cliModel, len(messages), len(mcpTools), orNone(short(syncResult.SessionID)), orNone(short(syncResult.ForkAt)), orDefault(effort), strictMcp, orNone(piSessionID), truncateRunes(promptText, 60))

	// The query outlives this call, so its process runs on its own context; abort cancels it.
	// Until the stream takes over below, the call's abort reaches the start-up too.
	queryCtx, cancelQuery := context.WithCancel(context.Background())
	stopStartupAbort := context.AfterFunc(ctx, cancelQuery)
	client, err := b.deps.Spawn(queryCtx, opts, handlers)
	stopStartupAbort()
	if err != nil {
		cancelQuery()
		cleanupCreated()
		if ctx.Err() != nil {
			b.debugf("provider: aborted while Claude Code started: %v", err)
			return abortedStream(model)
		}
		return errorStream(model, fmt.Sprintf("start Claude Code: %v", err))
	}

	b.claimCurrentPiStream(stream, "fresh-query", q)
	q.resetTurnState(model)
	q.mu.Lock()
	q.model = model
	q.promptStream = promptStream
	q.activeQuery = client
	q.mu.Unlock()
	b.registerContext(q)
	b.queries.Add(2)
	go func() {
		defer b.queries.Done()
		defer b.recoverHandler("initial prompt push")
		msg := userMessage(promptContent, "")
		msg.UUID = promptUUID
		if err := promptStream.Push(queryCtx, msg); err != nil {
			b.debugf("provider: initial prompt push rejected: %v", err)
		}
	}()

	// Session bookkeeping of this query (index.ts commitRecording/persistRecording).
	// Every commit ends this query's writing (abort, discard, failure, completion), so
	// later readers of its file wait for the child to exit. Memory is authoritative at
	// once; the file is saved after exit, since a replayed uuid can precede its record
	// on disk and a pointer saved earlier could outlive a crash naming records that
	// never land.
	target := syncResult.TargetSessionID
	exited := client.Done()
	stopWriting := func() { b.setWriter(target, exited) }
	var commitMu sync.Mutex
	expected := syncResult.State
	var unsaved *SessionState
	commit := func(dirty bool) {
		stopWriting()
		commitMu.Lock()
		defer commitMu.Unlock()
		if !dirty && (recording.HasGaps() || (syncResult.Created && !recording.HasProgress())) {
			// Costs the next turn a prefix write.
			b.debugf("recording: clean end left messages unrecorded piSession=%s", orNone(piSessionID))
		}
		if piSessionID == "" || b.sessionStateFor(piSessionID) != expected || (syncResult.Created && !recording.HasProgress()) {
			return
		}
		state := &SessionState{CCSessionID: target, Cwd: cwd, Model: cliModel, PromptDigest: promptDigest,
			Recording: recording.Recorded(), DirtyTail: dirty || b.contextStale(q) || recording.HasGaps()}
		b.setSessionStateFor(piSessionID, state)
		expected, unsaved = state, state
		b.debugf("session-recorded: ccSession=%s digests=%d dirtyTail=%t piSession=%s", short(target), len(state.Digests), state.DirtyTail, piSessionID)
	}
	// persist saves the latest committed snapshot once the child has exited, and
	// only while it is still the session's current one.
	persist := func() error {
		<-exited
		commitMu.Lock()
		state := unsaved
		unsaved = nil
		commitMu.Unlock()
		if state == nil || piSessionID == "" || b.sessionStateFor(piSessionID) != state {
			return nil
		}
		return SaveSessionState(b.deps.AgentDir, piSessionID, *state)
	}
	q.mu.Lock()
	q.commitRecording = commit
	q.mu.Unlock()

	var abortOnce sync.Once
	onAbort := func() {
		abortOnce.Do(func() {
			// context.AfterFunc runs this on a goroutine of its own.
			defer b.recoverHandler("abort")
			// The final snapshot for an aborted query, before anything is torn down.
			commit(true)
			b.unregisterContext(q)
			q.mu.Lock()
			q.turnToolCallIDs = []string{}
			q.mu.Unlock()
			b.drainForAbort(q)
			// Interrupt asks the CLI to stop gracefully and close kills it; both are needed.
			b.killClient(client)
			cancelQuery()
		})
	}
	stopOwnerAbort := context.AfterFunc(ctx, onAbort)
	bind, unbind := b.newRunBinder(q, ctx, onAbort)
	q.mu.Lock()
	q.bindRunSignal = bind
	q.abort = onAbort
	q.mu.Unlock()

	complete := func(res consumeResult) { b.completeQuery(q, client, syncResult, res, commit, persist) }
	go func() {
		defer b.queries.Done()
		defer func() {
			if r := recover(); r != nil {
				b.debugf("provider: recovered from panic: %v\n%s", r, debug.Stack())
			}
			stopOwnerAbort()
			unbind()
			q.mu.Lock()
			q.bindRunSignal = nil
			q.commitRecording = nil
			q.mu.Unlock()
			cancelQuery()
			stopWriting()
			if err := persist(); err != nil {
				b.debugf("provider: saving session state after exit: %v", err)
			}
			if syncResult.Created && !recording.HasProgress() {
				if err := ccsession.Delete(target, cwd, ""); err != nil {
					b.debugf("provider: could not delete session %s without progress: %v", short(target), err)
				} else {
					b.debugf("provider: deleted session %s: no complete assistant progress", short(target))
				}
			}
			b.releaseWriter(target, exited)
		}()
		b.runConsume(queryCtx, host, q, client, complete)
	}()
	return stream
}

// completeQuery is the normal end of a query: record what the turn emitted, commit
// the recording, save it once the child exits, then finish the stream. A Claude
// Code session other than the one synced is a protocol violation: fatal, nothing
// committed, the turn still ends.
func (b *Bridge) completeQuery(q *QueryContext, client ccproto.Client, syncResult SyncResult, res consumeResult, commit func(bool), persist func() error) {
	if captured := res.CapturedSessionID; captured != "" && captured != syncResult.TargetSessionID {
		err := fmt.Errorf("Claude Code initialized unexpected session %s; expected %s", captured, syncResult.TargetSessionID)
		b.debugf("provider: %v", err)
		b.failStream(q, err)
		return
	}
	q.mu.Lock()
	if q.currentPiStream != nil {
		b.recordAssistantEmissionLocked(q)
	}
	sawResult := q.sawResult
	q.mu.Unlock()
	commit(!sawResult)
	// Completion waits for the save, so a failed one ends the turn with it. The child
	// has its stdin ended by now; closing makes sure it exits.
	closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	_ = client.Close(closeCtx)
	cancel()
	if err := persist(); err != nil {
		b.debugf("provider: saving session state failed: %v", err)
		b.failStream(q, err)
		return
	}
	q.mu.Lock()
	if q.activeQuery == client {
		b.debugf("provider: clearing activeQuery before final stream completion")
		q.activeQuery = nil
	}
	stopReason, _ := q.turnOutput["stopReason"].(string)
	q.mu.Unlock()
	b.unregisterContext(q)
	b.finalizeCurrentStream(q, stopReason)
	b.debugf("query-end: outcome=complete ccSession=%s", short(syncResult.TargetSessionID))
}

// failStream ends the live pi stream with a query failure, leaving a turn whose
// stream already ended unchanged.
func (b *Bridge) failStream(q *QueryContext, failure error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.activeQuery = nil
	if q.turnOutput != nil && q.currentPiStream != nil {
		q.turnOutput["stopReason"] = "error"
		if q.turnOutput["errorMessage"] == nil {
			q.turnOutput["errorMessage"] = failure.Error()
		}
		q.endStreamLocked(map[string]any{"type": "error", "reason": "error", "error": deepCopy(q.turnOutput)})
	}
}

func orDefault(effort string) string {
	if effort == "" {
		return "default"
	}
	return effort
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// sessionStorageVars are read by the bridge through PiG's own values to find Claude
// Code's session files; a child pointed elsewhere would resume a session the bridge never wrote.
var sessionStorageVars = []string{"CLAUDE_CONFIG_DIR", "HOME", "USERPROFILE"}

// providerEnv is pi's per-request provider env (a stored credential's `env`, or the
// caller's), as NAME=value entries for ccproto.Options.Env; an empty value unsets.
func providerEnv(options map[string]any) ([]string, error) {
	raw := obj(options["env"])
	if len(raw) == 0 {
		return nil, nil
	}
	var storage, env []string
	for _, name := range sortedKeys(raw) {
		if slices.Contains(sessionStorageVars, strings.ToUpper(name)) {
			storage = append(storage, name)
		}
		env = append(env, name+"="+str(raw[name]))
	}
	if len(storage) > 0 {
		return nil, fmt.Errorf("claude-bridge: provider env can't set %s; the bridge reads Claude Code's session files through PiG's own values", strings.Join(storage, ", "))
	}
	return env, nil
}

// liveContexts are the queries in the routing set.
func (b *Bridge) liveContexts() []*QueryContext {
	b.mu.Lock()
	defer b.mu.Unlock()
	live := make([]*QueryContext, 0, len(b.contexts))
	for c := range b.contexts {
		live = append(live, c)
	}
	return live
}

// hasActiveQuery reports whether any registered query still has a Claude Code process.
func (b *Bridge) hasActiveQuery() bool {
	for _, c := range b.liveContexts() {
		c.mu.Lock()
		active := c.activeQuery != nil
		c.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func (b *Bridge) contextStale(q *QueryContext) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return q.historyStale
}

// armContextIfRewritten copies the session's rewrite mark onto q.
func (b *Bridge) armContextIfRewritten(q *QueryContext) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.historyRewritten[q.piSessionID]; ok && !q.historyStale && q.piSessionID != "" {
		q.historyStale = true
	}
}

func (b *Bridge) hasRewriteMark(piSessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.historyRewritten[piSessionID]
	return ok && piSessionID != ""
}

func (b *Bridge) clearRewriteMark(piSessionID string) {
	if piSessionID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.historyRewritten, piSessionID)
}

// resolveExecutable finds the Claude Code binary once per process and, on that
// first resolution, warns about a binary older than the supported minimum. A missing
// binary fails the query, with a notice each time it is tried. ctx ending stops the
// version probe; nothing is cached then, so the next call probes again.
func (b *Bridge) resolveExecutable(ctx context.Context, host Host) (string, error) {
	// Single flight: concurrent first calls share one version probe.
	b.resolveMu.Lock()
	defer b.resolveMu.Unlock()
	b.mu.Lock()
	exe := b.executable
	b.mu.Unlock()
	if exe != "" {
		return exe, nil
	}
	configured := ""
	if p := b.deps.Config.Provider; p != nil && p.PathToClaudeCodeExecutable != nil {
		configured = *p.PathToClaudeCodeExecutable
	}
	exe, err := ccproto.ResolveExecutable(configured)
	if err == nil {
		var version string
		if version, err = ccproto.Version(ctx, exe); err == nil {
			if ccproto.OlderThan(version, ccproto.MinVersion) {
				host.Notify(fmt.Sprintf("Claude Code %s is older than the supported %s; update it. The bridge continues, but some behaviour may be missing.", version, ccproto.MinVersion), "warning")
			}
		} else if ctx.Err() != nil {
			return "", ctx.Err()
		} else if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
			// A version the bridge cannot read does not stop the query; spawning reports a real failure.
			b.debugf("provider: cannot check the Claude Code version: %v", err)
			err = nil
		}
	}
	if err != nil {
		host.Notify(err.Error(), "error")
		return "", err
	}
	b.mu.Lock()
	b.executable = exe
	b.mu.Unlock()
	return exe, nil
}
