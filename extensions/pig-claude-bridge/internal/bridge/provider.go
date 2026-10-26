package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"runtime/debug"
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
	discardedQueryReason = "Context was compacted; this query was discarded."
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

func (b *Bridge) streamSimple(ctx context.Context, host Host, model, piContext, options map[string]any) *sdk.ModelEventStream {
	b.showStartupNoticeOnce(host)
	// PiG hands providers a transcript (prompt and tools folded into system
	// messages): fold it back out to the prompt and tools fields every cursor write,
	// sync and prompt-capture lookup below assumes.
	piContext = convert.ToBridgeContext(piContext)
	messages := messagesOf(piContext)

	// Compaction, branch-summary and bug-report calls use cacheRetention "none". Their
	// prompts bypass the capture boundaries, so they go to an isolated process with no
	// session sync.
	if str(options["cacheRetention"]) == "none" {
		b.debugf("provider: one-off summarizer call (cacheRetention none) routed to isolated summary, msgs=%d", len(messages))
		isolated, _ := b.IsolatedStream(ctx, host, model, piContext, options, "compact-summary", nil, nil)
		return isolated
	}

	stream := sdk.CreateAssistantMessageEventStream()
	piSessionID := str(options["sessionId"])
	onResponse, _ := options["onResponse"].(func(map[string]any, map[string]any) error)

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
			resultCtx = b.contextForToolResults(allResults)
		}
	}

	// pi rewrote its history while this query sat parked at a tool boundary, so the
	// query answers about a conversation that no longer exists. Discard it and let
	// this tool result carry the turn into a fresh query over the rewritten history.
	// The staleness mark is per pi session: a subagent's compaction must not discard
	// the parent's parked query, and vice versa.
	rewrittenUnderQuery := resultCtx != nil && b.contextStale(resultCtx)
	if rewrittenUnderQuery {
		b.discardRewrittenQuery(resultCtx, discardedQueryReason)
		resultCtx = nil
		// Recomputed, not cleared: a reentrant subagent may still hold a query of its own.
		activeQuery = b.hasActiveQuery()
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
		b.claimCurrentPiStream(stream, "tool-result", resultCtx, onResponse)
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
			b.deliverToolResults(context.WithoutCancel(ctx), host, resultCtx, allResults, steer, len(messages))
		}()
		// The shared cursor tracks the top-level conversation: a reentrant subagent
		// delivering its own results must not drag it back to that subagent's count.
		b.setCursor(resultCtx.piSessionID, len(messages))
		resultCtx.mu.Lock()
		resultCtx.latestCursor = max(resultCtx.latestCursor, len(messages))
		resultCtx.mu.Unlock()
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
		// With no query in flight anywhere, the session this result belongs to is the
		// one whose turn just ended: its cursor advances to count the result.
		if len(b.liveContexts()) == 0 {
			b.setCursor(piSessionID, len(messages))
		}
		// No query owns this result, so there is no context to reset: a throwaway
		// supplies the empty message this turn ends with.
		c := newQueryContext()
		c.resetTurnState(model)
		stream.Push(map[string]any{"type": "done", "reason": "stop", "message": c.turnOutput})
		return stream
	}

	return b.freshQuery(ctx, host, stream, model, piContext, messages, options, piSessionID, onResponse, unparkedToolResult, rewrittenUnderQuery)
}

// freshQuery starts a Claude Code process for this call and streams its turn.
func (b *Bridge) freshQuery(ctx context.Context, host Host, stream *sdk.ModelEventStream, model, piContext map[string]any, messages []map[string]any,
	options map[string]any, piSessionID string, onResponse func(map[string]any, map[string]any) error, unparkedToolResult, rewrittenUnderQuery bool) *sdk.ModelEventStream {
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
	if err != nil {
		// The resolution and the sendability guard stop a turn that would lose its
		// instructions or leak pi's harness text.
		b.diagDump("prompt_capture_unresolved", map[string]any{
			"promptChars": len([]rune(systemPrompt)), "knownKeys": b.captures.Size(), "error": err.Error(),
		})
		return errorStream(model, err.Error())
	}

	exe, err := b.resolveExecutable(host)
	if err != nil {
		return errorStream(model, err.Error())
	}

	cwd := b.sessionCwdFor(piSessionID)
	// cliModel is the id sent to Claude Code (may carry [1m]); model["id"] is the pi-registered id.
	cliModel := models.ClaudeCodeModelID(str(model["id"]), b.longContext)
	// Read before the sync: with no query parked (an overflow retried after
	// compaction), the session's mark is the only sign of the rewrite.
	historyRewritten := rewrittenUnderQuery || b.hasRewriteMark(piSessionID)
	syncResult, err := b.syncSharedSession(host, piSessionID, messages, customToolNameToSdk, cwd, cliModel)
	if err != nil {
		return errorStream(model, err.Error())
	}
	// This query starts from the history pi has now: consume this session's armed
	// rewrite; a sibling session's stays armed for its own queries.
	b.clearRewriteMark(piSessionID)

	promptBlocks, err := b.extractUserPromptBlocks(messages)
	if err != nil {
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
	// Guard: an empty prompt means the last context message isn't a user message.
	if !hasPrompt() {
		roles := make([]string, len(messages))
		for i, m := range messages {
			roles[i] = fmt.Sprintf("[%d]%v", i, m["role"])
		}
		b.diagDump("empty_prompt", map[string]any{
			"contextLength": len(messages), "activeQueryContexts": len(b.liveContexts()),
			"messageRoles": strings.Join(roles, " "),
		})
		// Recover with a continuation prompt so Claude Code is not sent an empty text block.
		promptText = "[continue]"
	}

	q := newQueryContext()
	q.piSessionID = piSessionID
	q.customToolNameToPi = customToolNameToPi
	mcpServer, err := b.buildMcpServer(mcpTools, q)
	if err != nil {
		return errorStream(model, err.Error())
	}

	// Always a prompt stream rather than a one-shot prompt: a parked input is what lets
	// steers reach Claude Code's stdin mid-turn.
	promptStream := newPromptStream()
	var promptContent any = promptBlocks
	if promptBlocks == nil {
		promptContent = []any{map[string]any{"type": "text", "text": promptText}}
	}

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
	debugFile, onStderr := b.cliDebugOptions("provider")
	opts := ccproto.Options{
		Executable: exe, Cwd: cwd, Effort: effort, Resume: syncResult.SessionID,
		Tools: []string{}, ToolsSet: true, PermissionMode: "bypassPermissions", IncludePartialMessages: true,
		Settings: settings, DebugFile: debugFile, AppendSystemPrompt: systemPromptAppend, ExtraArgs: extraArgs,
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
	b.debugf("provider: fresh query model=%s msgs=%d tools=%d resume=%s effort=%s strictMcp=%t prompt=%s",
		cliModel, len(messages), len(mcpTools), short(syncResult.SessionID), orDefault(effort), strictMcp, truncateRunes(promptText, 60))

	// The query outlives this call, so its process runs on its own context; abort cancels it.
	// Until the stream takes over below, the call's abort reaches the start-up too.
	queryCtx, cancelQuery := context.WithCancel(context.Background())
	stopStartupAbort := context.AfterFunc(ctx, cancelQuery)
	client, err := b.deps.Spawn(queryCtx, opts, handlers)
	stopStartupAbort()
	if err != nil {
		cancelQuery()
		if ctx.Err() != nil {
			b.debugf("provider: aborted while Claude Code started: %v", err)
			return abortedStream(model)
		}
		return errorStream(model, fmt.Sprintf("start Claude Code: %v", err))
	}

	b.claimCurrentPiStream(stream, "fresh-query", q, onResponse)
	q.resetTurnState(model)
	q.mu.Lock()
	q.promptStream = promptStream
	q.activeQuery = client
	q.mu.Unlock()
	b.registerContext(q)
	b.queries.Add(2)
	go func() {
		defer b.queries.Done()
		defer b.recoverHandler("initial prompt push")
		if err := promptStream.Push(queryCtx, userMessage(promptContent, "")); err != nil {
			b.debugf("provider: initial prompt push rejected: %v", err)
		}
	}()

	var abortOnce sync.Once
	onAbort := func() {
		abortOnce.Do(func() {
			// context.AfterFunc runs this on a goroutine of its own.
			defer b.recoverHandler("abort")
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

	complete := func(res consumeResult) { b.completeQuery(q, client, syncResult, cwd, len(messages), res) }
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
			q.mu.Unlock()
			cancelQuery()
		}()
		b.runConsume(queryCtx, host, q, client, complete)
	}()
	return stream
}

func orDefault(effort string) string {
	if effort == "" {
		return "default"
	}
	return effort
}

// completeQuery is the normal end of a query: capture the session id into this
// query's own mirror, then finish the stream. A reentrant subagent completing does
// not reassign the parent's conversation to the child's Claude Code file.
func (b *Bridge) completeQuery(q *QueryContext, client ccproto.Client, syncResult SyncResult, cwd string, contextLength int, res consumeResult) {
	captured := res.CapturedSessionID
	if syncResult.PreserveSharedSession {
		state := b.sessionStateFor(q.piSessionID)
		if captured != "" && (state == nil || captured != state.SessionID) {
			if err := ccsession.Delete(captured, cwd, ""); err != nil {
				b.debugf("provider: could not delete ephemeral session %s: %v", short(captured), err)
			}
			b.debugf("provider: query done, deleted ephemeral session %s to preserve shared session", short(captured))
		}
		b.debugf("provider: query done, ignoring captured session %s to preserve shared session", orNone(short(captured)))
	} else {
		q.mu.Lock()
		latest, missedSteer := q.latestCursor, q.missedSteer
		q.mu.Unlock()
		b.updateSession(q.piSessionID, func(s *SessionState) bool {
			id := captured
			if id == "" {
				id = s.SessionID
			}
			if id == "" {
				return false
			}
			cursor := max(contextLength, latest, s.Cursor)
			b.debugf("provider: query done, session=%s, cursor=%d", short(id), cursor)
			// A missed steer may precede the first mirror or arrive while this query is
			// still able to complete: preserve both rebuild signals.
			s.SessionID, s.Cursor, s.Cwd, s.PiSessionID = id, cursor, cwd, q.piSessionID
			s.NeedsRebuild = missedSteer || s.NeedsRebuild
			return true
		})
	}
	q.mu.Lock()
	if q.activeQuery == client {
		b.debugf("provider: clearing activeQuery before final stream completion")
		q.activeQuery = nil
	}
	stopReason, _ := q.turnOutput["stopReason"].(string)
	q.mu.Unlock()
	b.finalizeCurrentStream(q, stopReason)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// updateSession applies fn to a copy of the mirror (zero when it has none) and
// stores the result when fn reports true. fn must not call back into the Bridge.
func (b *Bridge) updateSession(piSessionID string, fn func(*SessionState) bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := sessionKey(piSessionID)
	s := b.sessions[key]
	if fn(&s) {
		b.sessions[key] = s
	}
}

// setCursor moves an existing mirror's cursor; a session with no mirror has none to move.
func (b *Bridge) setCursor(piSessionID string, cursor int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := sessionKey(piSessionID)
	if s, ok := b.sessions[key]; ok {
		s.Cursor = cursor
		b.sessions[key] = s
	}
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
// binary fails the query, with a notice each time it is tried.
func (b *Bridge) resolveExecutable(host Host) (string, error) {
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
		if version, err = ccproto.Version(exe); err == nil {
			if ccproto.OlderThan(version, ccproto.MinVersion) {
				host.Notify(fmt.Sprintf("Claude Code %s is older than the supported %s; update it. The bridge continues, but some behaviour may be missing.", version, ccproto.MinVersion), "warning")
			}
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
