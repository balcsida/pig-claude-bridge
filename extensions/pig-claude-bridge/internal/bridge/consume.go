package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/mcptools"
)

// Goroutines of one query (port of index.ts consumeQuery and the lifecycle around it):
//   - the consume goroutine (runConsume) is the only reader of client.Frames() and
//     the only caller of the stream processors; it also tears the query down;
//   - the writer goroutine (pumpPrompt) is the only caller of client.WriteUser and
//     EndInput, fed by the PromptStream;
//   - MCP handler goroutines (toolCallHandler) are started by the client and park
//     on a per-call channel until deliverToolResults or releasePendingToolCalls
//     answers them.
// Locks: q.mu guards QueryContext state and is never held across Push, Notify,
// WriteUser, a channel operation or a call that takes Bridge.mu.

// consumeResult is how a consumed query ended.
type consumeResult struct {
	// CapturedSessionID is the id of the system/init frame, "" when none came.
	CapturedSessionID string
	// Failure is why the query ended in failure, nil for a normal end.
	Failure error
	// Abandoned: discardRewrittenQuery killed the query; its replacement owns the
	// session and the stream.
	Abandoned bool
}

// resultFailure is a failure Claude Code reported in an error result frame.
type resultFailure struct{ text string }

func (e *resultFailure) Error() string { return e.text }

// exitFailure is the Claude Code process exiting abnormally.
type exitFailure struct{ err error }

func (e *exitFailure) Error() string { return e.err.Error() }
func (e *exitFailure) Unwrap() error { return e.err }

const closeTimeout = 10 * time.Second

func (b *Bridge) registerContext(q *QueryContext) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contexts[q] = struct{}{}
}

func (b *Bridge) unregisterContext(q *QueryContext) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.contexts, q)
}

// isQueryAbandoned reports whether discardRewrittenQuery killed the query.
func (b *Bridge) isQueryAbandoned(client ccproto.Client) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.abandoned[client]
	return ok
}

func (b *Bridge) takeAbandoned(client ccproto.Client) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.abandoned[client]
	delete(b.abandoned, client)
	return ok
}

// contextForToolResults finds the live query a turn's results belong to, by the
// tool call ids it parked, queued or emitted. nil when none matches.
func (b *Bridge) contextForToolResults(results []convert.McpResult) *QueryContext {
	b.mu.Lock()
	live := make([]*QueryContext, 0, len(b.contexts))
	for c := range b.contexts {
		live = append(live, c)
	}
	b.mu.Unlock()
	for _, r := range results {
		id := r.ToolCallID
		if id == "" {
			continue
		}
		for _, c := range live {
			if c.ownsToolCall(id) {
				return c
			}
		}
	}
	return nil
}

func (c *QueryContext) ownsToolCall(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pendingToolCalls[id]; ok {
		return true
	}
	if _, ok := c.pendingResults[id]; ok {
		return true
	}
	for _, t := range c.turnToolCallIDs {
		if t == id {
			return true
		}
	}
	return false
}

// resolveMcpTools lists the pi tools to serve over MCP, minus excludeToolName,
// with the name maps in both directions (each under the exact and the lower-cased
// SDK name, as Claude sometimes lowercases it).
func resolveMcpTools(context map[string]any, excludeToolName string) (mcpTools []map[string]any, customToolNameToSdk, customToolNameToPi map[string]string) {
	customToolNameToSdk, customToolNameToPi = map[string]string{}, map[string]string{}
	for _, raw := range list(context["tools"]) {
		tool := obj(raw)
		name := str(tool["name"])
		if tool == nil || name == excludeToolName {
			continue
		}
		sdkName := convert.MCPToolPrefix + name
		mcpTools = append(mcpTools, tool)
		customToolNameToSdk[name] = sdkName
		customToolNameToSdk[strings.ToLower(name)] = sdkName
		customToolNameToPi[sdkName] = name
		customToolNameToPi[strings.ToLower(sdkName)] = name
	}
	return mcpTools, customToolNameToSdk, customToolNameToPi
}

// buildMcpServer is the MCP server bridging pi's tools to Claude Code; its tool
// handlers park on q. nil when there are no tools.
func (b *Bridge) buildMcpServer(tools []map[string]any, q *QueryContext) (*mcptools.Server, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	defs := make([]mcptools.ToolDef, len(tools))
	for i, tool := range tools {
		name := str(tool["name"])
		schema, _ := tool["parameters"].(map[string]any)
		defs[i] = mcptools.ToolDef{Name: name, Description: str(tool["description"]), InputSchema: schema, Handler: b.toolCallHandler(q, name)}
	}
	return mcptools.NewServer(convert.MCPServerName, defs)
}

// toolCallHandler is the MCP handler for pi tool toolName. The handler and pi's
// result arrive in either order, hence the two maps: a result that landed first
// waits in pendingResults for the handler to claim, and a handler that came first
// parks in pendingToolCalls. The tool use id from the call's _meta pairs them.
func (b *Bridge) toolCallHandler(q *QueryContext, toolName string) func(ctx context.Context, toolUseID string, args map[string]any) (convert.McpResult, error) {
	return func(ctx context.Context, toolUseID string, _ map[string]any) (convert.McpResult, error) {
		q.mu.Lock()
		if result, ok := q.pendingResults[toolUseID]; ok {
			delete(q.pendingResults, toolUseID)
			remaining := len(q.pendingResults)
			q.mu.Unlock()
			b.debugf("mcp handler: %s [%s] → resolved from queue (%d remaining)", toolName, toolUseID, remaining)
			return result, nil
		}
		reply := make(chan convert.McpResult, 1)
		q.pendingToolCalls[toolUseID] = PendingToolCall{ToolName: toolName, Resolve: func(r convert.McpResult) {
			select {
			case reply <- r:
			default:
			}
		}}
		q.mu.Unlock()
		b.debugf("mcp handler: %s [%s] → waiting", toolName, toolUseID)
		select {
		case r := <-reply:
			return r, nil
		case <-ctx.Done():
			// Claude Code cancelled the call: nothing will read the reply.
			q.mu.Lock()
			delete(q.pendingToolCalls, toolUseID)
			q.mu.Unlock()
			return convert.McpResult{}, ctx.Err()
		}
	}
}

// parseRateLimitInfo reads a rate_limit_event's rate_limit_info.
func parseRateLimitInfo(m map[string]any) RateLimitInfo {
	num := func(v any) *float64 {
		if f, ok := v.(float64); ok {
			return &f
		}
		return nil
	}
	info := RateLimitInfo{
		Status: str(m["status"]), ResetsAt: num(m["resetsAt"]), RateLimitType: str(m["rateLimitType"]),
		OverageStatus: str(m["overageStatus"]), OverageResetsAt: num(m["overageResetsAt"]),
		Utilization: num(m["utilization"]), SurpassedThreshold: num(m["surpassedThreshold"]),
	}
	if windows := obj(m["unifiedWindows"]); windows != nil {
		info.UnifiedWindows = map[string]*UsageWindow{}
		for name, raw := range windows {
			if w := obj(raw); w != nil {
				info.UnifiedWindows[name] = &UsageWindow{Utilization: num(w["utilization"]), ResetsAt: num(w["resetsAt"])}
			} else {
				info.UnifiedWindows[name] = nil
			}
		}
	}
	return info
}

// consumeQuery reads client's frames until the query ends and maps them to pi
// events on q.currentPiStream. Per turn, Claude Code sends stream_events (deltas)
// then an assistant message (completed blocks). On a tool_use the stream processors
// end the pi stream and the MCP handler parks Claude Code until pi delivers the
// result. ctx cancelled is an abort. It never panics: a panic is a Failure.
func (b *Bridge) consumeQuery(ctx context.Context, host Host, q *QueryContext, client ccproto.Client) (res consumeResult) {
	defer func() {
		if r := recover(); r != nil {
			b.debugf("consumeQuery: recovered from panic: %v\n%s", r, debug.Stack())
			res.Failure = fmt.Errorf("claude bridge internal error: %v", r)
		}
		res.Abandoned = b.takeAbandoned(client)
	}()
	recordPath := os.Getenv("CLAUDE_BRIDGE_RECORD_STREAM")
	frames := client.Frames()
loop:
	for {
		var raw json.RawMessage
		select {
		case <-ctx.Done():
			break loop
		case r, open := <-frames:
			if !open {
				if err := b.awaitExit(ctx, client); err != nil {
					res.Failure = err
				}
				break loop
			}
			raw = r
		}
		// CLAUDE_BRIDGE_RECORD_STREAM=<path> appends every frame, one per line, so
		// replay fixtures can be captured from real Claude Code output.
		if recordPath != "" {
			b.recordFrame(recordPath, raw)
		}
		if ctx.Err() != nil {
			break
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			b.debugf("consumeQuery: skipping malformed frame: %v", err)
			continue
		}
		b.reportHeldRateLimit(q)
		if b.consumeFrame(host, q, frame, &res) {
			break
		}
	}
	b.debugf("consumeQuery: frame loop exited, aborted=%t, capturedSessionId=%s", ctx.Err() != nil, short(res.CapturedSessionID))
	return res
}

func (b *Bridge) recordFrame(path string, raw json.RawMessage) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		b.debugf("consumeQuery: cannot record frame: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(append([]byte{}, raw...), '\n'))
}

// awaitExit waits for the process to exit once its frames ended. A nil return is a
// clean exit or an abort. The error carries the exit text and the stderr tail, once.
func (b *Bridge) awaitExit(ctx context.Context, client ccproto.Client) error {
	select {
	case <-client.Done():
	case <-ctx.Done():
		return nil
	}
	err := client.Wait()
	if err == nil {
		return nil
	}
	text := err.Error()
	if tail := strings.TrimSpace(client.StderrTail()); tail != "" && !strings.Contains(text, tail) {
		text += "\nstderr: " + tail
	}
	return &exitFailure{err: errors.New(text)}
}

// consumeFrame handles one frame and reports whether the query is over.
func (b *Bridge) consumeFrame(host Host, q *QueryContext, frame map[string]any, res *consumeResult) (stop bool) {
	typ := str(frame["type"])

	// Lifecycle frames come before the currentPiStream gate: everything below it is
	// content, which has nowhere to go once a turn ended on a tool call.
	//
	// A replay acknowledgement clears a queued steer even after its turn's stream
	// ended. For a fresh steer uuid, replay marks incorporation, not completion:
	// its MCP calls still need stdin until the following result.
	if parent, present := frame["parent_tool_use_id"]; typ == "user" && present && parent == nil && frame["isReplay"] == true {
		q.mu.Lock()
		delete(q.pendingSteerUUIDs, str(frame["uuid"]))
		q.mu.Unlock()
	}

	switch typ {
	case "result":
		// The failure a result carries is the only record the turn failed at all; it
		// is handled before the gate so a failure at a tool boundary is not lost.
		q.mu.Lock()
		model := q.model
		q.mu.Unlock()
		b.logServedContextWindow("result", frame, model)
		failure, failed := resultErrorText(frame)
		q.mu.Lock()
		ps, steers := q.promptStream, len(q.pendingSteerUUIDs)
		q.mu.Unlock()
		// A result ends a turn, not necessarily the query: steers not yet replayed can
		// still start a later turn. Nothing else closes Claude Code's stdin.
		if (failed || steers == 0) && ps != nil {
			ps.End()
		}
		if failed {
			q.mu.Lock()
			// Consume the rejection with the failure it caused, so a later unrelated
			// failure on this query does not inherit the label.
			if q.rateLimitRejection != nil {
				failure = describeRateLimitFailure(*q.rateLimitRejection, failure)
				q.rateLimitRejection = nil
			}
			// Only while the turn's stream is open: once ended, turnOutput is the
			// message pi already recorded.
			if q.turnOutput != nil && q.currentPiStream != nil {
				q.turnOutput["stopReason"] = "error"
				q.turnOutput["errorMessage"] = failure
			}
			q.mu.Unlock()
			b.debugf("consumeQuery: error result, subtype=%v, error=%s", frame["subtype"], failure)
			// The failure ends the query; Claude Code would still run a queued steer
			// whose stop reason would overwrite this one.
			res.Failure = &resultFailure{text: failure}
			return true
		}

	case "rate_limit_event":
		info := obj(frame["rate_limit_info"])
		shown, _ := json.Marshal(info)
		b.debugf("consumeQuery: rate_limit_event %s", truncateRunes(string(shown), 300))
		if info != nil {
			b.handleRateLimitEvent(host, q, parseRateLimitInfo(info))
		} else {
			b.reportHeldRateLimit(q)
		}
		return false
	}

	q.mu.Lock()
	open := q.currentPiStream != nil && q.turnOutput != nil
	q.mu.Unlock()
	if !open {
		return false
	}

	switch typ {
	case "stream_event":
		b.processStreamEvent(q, frame)
	case "assistant":
		b.processAssistantMessage(q, frame)
	case "result":
		// The success path: push the result text when no stream event delivered it.
		q.mu.Lock()
		if q.currentPiStream != nil && q.turnOutput != nil && !q.turnSawStreamEvent && frame["subtype"] == "success" {
			q.ensureTurnStartedLocked()
			text := str(frame["result"])
			idx := q.addBlockLocked(map[string]any{"type": "text", "text": text})
			q.emitLocked("text_start", idx, nil)
			q.emitLocked("text_delta", idx, map[string]any{"delta": text})
			q.emitLocked("text_end", idx, map[string]any{"content": text})
		}
		q.mu.Unlock()
	case "system":
		if frame["subtype"] == "init" {
			res.CapturedSessionID = str(frame["session_id"])
		}
	case "user":
		// Prompt and tool-result echoes carry no content events; replay
		// acknowledgements were handled above the gate.
	default:
		b.debugf("consumeQuery: unhandled SDK message type %v", frame["type"])
	}
	return false
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// pumpPrompt is the writer goroutine: it moves the prompt stream's messages to
// Claude Code's stdin and closes stdin when the stream ends. A write error fails
// the stream so every in-flight Push settles.
func (b *Bridge) pumpPrompt(ctx context.Context, client ccproto.Client, ps *PromptStream) {
	for {
		msg, ok := ps.Next(ctx)
		if !ok {
			break
		}
		if err := client.WriteUser(msg); err != nil {
			b.debugf("provider: write to Claude Code stdin failed: %v", err)
			ps.Fail(fmt.Errorf("write user message: %w", err))
		}
	}
	if err := client.EndInput(); err != nil {
		b.debugf("provider: closing Claude Code stdin: %v", err)
	}
}

// runConsume runs a query to its end on the calling goroutine (start it with go):
// the writer goroutine, the frame loop, the failure or abort outcome, then the
// teardown that releases every parked handler. complete runs for a normal end only
// (nil is fine). A panic anywhere becomes the query's failure, never escapes.
func (b *Bridge) runConsume(ctx context.Context, host Host, q *QueryContext, client ccproto.Client, complete func(consumeResult)) {
	q.mu.Lock()
	ps := q.promptStream
	q.mu.Unlock()
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		if ps != nil {
			b.pumpPrompt(ctx, client, ps)
		}
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				b.debugf("provider: recovered from panic: %v\n%s", r, debug.Stack())
				b.failQuery(q, client, fmt.Errorf("claude bridge internal error: %v", r))
			}
		}()
		res := b.consumeQuery(ctx, host, q, client)
		b.debugf("provider: consumeQuery completed, stopReason=%v, aborted=%t", b.turnField(q, "stopReason"), ctx.Err() != nil)
		switch {
		case res.Abandoned:
			b.debugf("provider: discarded query completed, leaving session and stream to its replacement")
		case ctx.Err() != nil:
			b.finishAborted(q)
		case res.Failure != nil:
			b.failQuery(q, client, res.Failure)
		case complete != nil:
			complete(res)
		}
	}()

	b.teardownQuery(q, client, ps)
	<-pumpDone
}

func (b *Bridge) turnField(q *QueryContext, key string) any {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.turnOutput[key]
}

// finishAborted ends an aborted query. The killed subprocess may flush a late
// record into the session JSONL, so its mirror rebuilds on a fresh id. A turn whose
// stream already ended was returned to pi and is left alone.
func (b *Bridge) finishAborted(q *QueryContext) {
	b.markSessionNeedsRebuild(q.piSessionID, true)
	b.debugf("provider: abort detected, marked session needsRebuild + forceRotate")
	q.mu.Lock()
	defer q.mu.Unlock()
	q.heldRateLimit = nil
	if q.currentPiStream == nil || q.turnOutput == nil {
		return
	}
	q.turnOutput["stopReason"] = "aborted"
	q.turnOutput["errorMessage"] = "Operation aborted"
	q.endStreamLocked(map[string]any{"type": "error", "reason": "aborted", "error": deepCopy(q.turnOutput)})
}

// failQuery ends a failed query with exactly one error event on the open stream.
// A turn whose stream already ended is left unchanged (issue #181).
func (b *Bridge) failQuery(q *QueryContext, client ccproto.Client, failure error) {
	b.debugf("provider: query error, error=%v", failure)
	var reported *resultFailure
	if errors.As(failure, &reported) {
		// The conversation is in an unknown state: drop the mirror, like the SDK's
		// throw after an error result did.
		b.setSessionStateFor(q.piSessionID, nil)
	} else {
		b.markSessionNeedsRebuild(q.piSessionID, false)
	}
	q.mu.Lock()
	ps := q.promptStream
	if q.turnOutput != nil && q.currentPiStream != nil {
		q.turnOutput["stopReason"] = "error"
		// Prefer the cause consumeQuery recorded off the result itself.
		if q.turnOutput["errorMessage"] == nil {
			q.turnOutput["errorMessage"] = failure.Error()
		}
	}
	q.mu.Unlock()
	if ps != nil {
		ps.Fail(failure)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.currentPiStream != nil && q.turnOutput != nil {
		q.endStreamLocked(map[string]any{"type": "error", "reason": "error", "error": deepCopy(q.turnOutput)})
	}
}

// teardownQuery settles whatever the dead subprocess would leave awaiting: the
// prompt stream's acks, the parked handlers, the routing entry; then it closes the
// client. A later query that claimed q keeps what it claimed.
func (b *Bridge) teardownQuery(q *QueryContext, client ccproto.Client, ps *PromptStream) {
	if ps != nil {
		ps.Fail(errors.New("query ended"))
	}
	q.mu.Lock()
	if q.promptStream == ps {
		q.promptStream = nil
	}
	own := q.activeQuery == client || q.activeQuery == nil
	if own {
		q.activeQuery = nil
	}
	q.mu.Unlock()
	if own {
		q.releasePendingToolCalls("Query ended")
		b.unregisterContext(q)
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		b.debugf("provider: closing Claude Code: %v", err)
	}
}

// killClient stops the subprocess: interrupt asks the CLI to stop gracefully and
// close kills it; both are needed, interrupt alone lets the current API call finish.
func killClient(client ccproto.Client) {
	if client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		_, _ = client.Interrupt(ctx)
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		_ = client.Close(ctx)
	}()
}

// newRunBinder returns the binder for q.bindRunSignal and the unbind that ends it.
// A later run continuing q from a tool result brings its own context; only the
// latest is bound. Its abort counts only while that run's stream is live: hosts
// that cancel each request's context after it ends would otherwise kill the query
// parked at the next tool call. owner is the opening run's context, already wired
// to onAbort by the caller.
func (b *Bridge) newRunBinder(q *QueryContext, owner context.Context, onAbort func()) (bind func(ctx context.Context, runStream *sdk.ModelEventStream), unbind func()) {
	var stop func() bool
	unbind = func() {
		if stop != nil {
			stop()
			stop = nil
		}
	}
	bind = func(ctx context.Context, runStream *sdk.ModelEventStream) {
		unbind()
		if ctx == nil || ctx == owner {
			return
		}
		onRunAbort := func() {
			q.mu.Lock()
			_, ended := q.completedStreams[runStream]
			q.mu.Unlock()
			if !ended {
				onAbort()
			}
		}
		if ctx.Err() != nil {
			onRunAbort()
			return
		}
		stop = context.AfterFunc(ctx, onRunAbort)
	}
	return bind, unbind
}

// drainForAbort settles everything that would otherwise be left awaiting a
// subprocess about to be killed: an in-flight prompt-stream Push would hang
// forever and take tool-result delivery with it.
func (b *Bridge) drainForAbort(q *QueryContext) {
	q.mu.Lock()
	ps := q.promptStream
	q.mu.Unlock()
	if ps != nil {
		ps.Fail(errors.New("Operation aborted"))
	}
	q.releasePendingToolCalls("Operation aborted")
}

// discardRewrittenQuery drops a Claude Code query parked at a tool boundary whose
// conversation pi has since rewritten (/compact, tree navigation; issue #101).
// Delivering the tool result into it would hand Claude Code the context pi just
// shrank. The caller takes the fresh-query path, where REBUILD imports pi's
// rewritten history, this tool result included. reason is what the parked handlers
// are answered with.
func (b *Bridge) discardRewrittenQuery(q *QueryContext, reason string) {
	q.mu.Lock()
	discarded := q.activeQuery
	q.activeQuery = nil
	q.turnToolCallIDs = []string{}
	ps := q.promptStream
	q.promptStream = nil
	q.mu.Unlock()

	b.mu.Lock()
	if discarded != nil {
		b.abandoned[discarded] = struct{}{}
	}
	// Leaving the routing set stops this result coming straight back here.
	delete(b.contexts, q)
	if q.piSessionID != "" {
		delete(b.historyRewritten, q.piSessionID)
	}
	b.mu.Unlock()

	if ps != nil {
		ps.Fail(errors.New("conversation rewritten"))
	}
	// Settle the parked handlers before killing the CLI: one left awaiting a dead
	// subprocess never settles.
	q.releasePendingToolCalls(reason)
	killClient(discarded)
	// The CLI may still flush a record into the session JSONL and the rebuild is
	// next, so rotate rather than race it. Only this session's mirror.
	b.markSessionNeedsRebuild(q.piSessionID, true)
	b.debugf("provider: history rewritten under a parked query — discarded it, rebuilding from current history")
}

// steerMissedSession records a steer that never made it into Claude Code's
// session. The cursor has already counted it, so count-based sync would skip it
// forever: rebuild instead, which re-imports the message from pi's context.
func (b *Bridge) steerMissedSession(q *QueryContext, text string) {
	q.mu.Lock()
	q.missedSteer = true
	q.mu.Unlock()
	b.markSessionNeedsRebuild(q.piSessionID, false)
	b.debugf("provider: steer never reached CC, marked query for rebuild: %s", truncateRunes(text, 60))
}

func newUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// deliverToolResults releases this turn's tool results to their MCP handlers,
// after first pushing any steer to Claude Code.
//
// The order is mandatory. The steer and the MCP tool result travel back over the
// same stdin FIFO; waiting for the push ack (the writer is done with the message)
// before resolving any handler guarantees Claude Code enqueues the steer before it
// reads the tool result, so its post-tool-call drain sees it this turn. Resolve
// first and the steer silently degrades to follow-up semantics. Both the drain and
// the FIFO ordering are Claude Code internals.
//
// Results pair with handlers by tool use id. host may be nil (no stuck-handler notice).
func (b *Bridge) deliverToolResults(ctx context.Context, host Host, q *QueryContext, results []convert.McpResult, steer []any, contextLength int) {
	if len(steer) > 0 {
		b.pushSteer(ctx, q, steer)
	}

	type release struct {
		pending PendingToolCall
		result  convert.McpResult
	}
	var releases []release
	var logs []string
	q.mu.Lock()
	logs = append(logs, fmt.Sprintf("provider: tool results, %d results, %d waiting handlers, ctx.msgs=%d", len(results), len(q.pendingToolCalls), contextLength))
	for _, result := range results {
		id := result.ToolCallID
		if pending, ok := q.pendingToolCalls[id]; ok && id != "" {
			delete(q.pendingToolCalls, id)
			releases = append(releases, release{pending, result})
			shown, _ := json.Marshal(result.Content)
			errored := ""
			if result.IsError {
				errored = " (error)"
			}
			logs = append(logs, fmt.Sprintf("provider: resolving %s [%s]%s %s", pending.ToolName, id, errored, truncateRunes(string(shown), 200)))
		} else if id != "" {
			q.pendingResults[id] = result
			logs = append(logs, fmt.Sprintf("provider: queued result [%s] (%d pending)", id, len(q.pendingResults)))
		} else {
			logs = append(logs, "WARNING: tool result without toolCallId, cannot match")
		}
		if len(q.pendingToolCalls) > 0 && len(q.pendingResults) > 0 {
			logs = append(logs, fmt.Sprintf("BUG: both maps non-empty! handlers=%d results=%d", len(q.pendingToolCalls), len(q.pendingResults)))
		}
	}
	waiting := len(q.pendingToolCalls)
	q.mu.Unlock()

	for _, l := range logs {
		b.debugf("%s", l)
	}
	for _, r := range releases {
		r.pending.Resolve(r.result)
	}
	if waiting > 0 {
		b.debugf("WARNING: %d MCP handlers still waiting after delivering %d results", waiting, len(results))
		if host != nil {
			host.Notify(fmt.Sprintf("Claude bridge: %d tool handler(s) still waiting — provider may be stuck", waiting), "warning")
		}
	}
}

// pushSteer writes a steer to Claude Code's stdin and waits for the write. A steer
// that cannot be written is recorded as missed; the tool results are delivered anyway.
func (b *Bridge) pushSteer(ctx context.Context, q *QueryContext, steer []any) {
	parts := make([]string, len(steer))
	for i, raw := range steer {
		if blk := obj(raw); blk["type"] == "text" {
			parts[i] = str(blk["text"])
		} else {
			parts[i] = "[image]"
		}
	}
	text := strings.Join(parts, "\n")

	q.mu.Lock()
	ps := q.promptStream
	uuid := ""
	if ps != nil {
		uuid = newUUID()
		// Registered before the write: a result can arrive during it.
		q.pendingSteerUUIDs[uuid] = struct{}{}
	}
	q.mu.Unlock()
	if ps == nil {
		b.debugf("WARNING: steer with no prompt stream, dropping: %s", truncateRunes(text, 60))
		b.steerMissedSession(q, text)
		return
	}
	msg := userMessage(steer, "next")
	msg.UUID = uuid
	if err := ps.Push(ctx, msg); err != nil {
		// The query is ending: further input would wedge tool-result delivery. The
		// steer is still in pi's context but the cursor is past it, so force a rebuild.
		q.mu.Lock()
		delete(q.pendingSteerUUIDs, uuid)
		q.mu.Unlock()
		b.debugf("provider: steer push rejected, delivering tool result anyway: %v", err)
		b.steerMissedSession(q, text)
		return
	}
	b.debugf("provider: steer written to CC stdin before tool result: %s", truncateRunes(text, 60))
}

// extractUserPrompt is the current user turn as text, or ok=false when the last
// message is not a user message. Empty messages are dropped before joining, so an
// all-empty turn yields "" and trips the caller's empty-prompt guard.
func extractUserPrompt(messages []map[string]any) (prompt string, ok bool) {
	turn := messages[turnStart(messages):]
	if len(turn) == 0 {
		return "", false
	}
	var parts []string
	for _, m := range turn {
		text, isString := m["content"].(string)
		if !isString {
			text = convert.MessageContentToText(m["content"])
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n"), true
}

// extractUserPromptBlocks is the current user turn as content blocks, or nil when
// it has no image (the caller falls back to the text prompt).
func (b *Bridge) extractUserPromptBlocks(messages []map[string]any) ([]any, error) {
	turn := messages[turnStart(messages):]
	if len(turn) == 0 {
		return nil, nil
	}
	hasImage := false
	blocks := []any{}
	for _, m := range turn {
		var content []any
		switch c := m["content"].(type) {
		case string:
			content = []any{map[string]any{"type": "text", "text": c}}
		case []any:
			content = c
		default:
			// Off-type content violates the contract: fail rather than degrade, and name
			// the shape, since the cause is almost always another extension.
			return nil, fmt.Errorf("extractUserPromptBlocks: user message content must be a string or block array, got %T — likely a malformed message from another extension", m["content"])
		}
		for _, raw := range content {
			block := obj(raw)
			switch block["type"] {
			case "text":
				if text := str(block["text"]); text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image":
				data, mime := str(block["data"]), str(block["mimeType"])
				if data == "" || mime == "" {
					b.debugf("image block missing data or mimeType, skipping")
					continue
				}
				b.debugf("image block: mimeType=%s, data length=%d", mime, len(data))
				hasImage = true
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": data}})
			}
		}
	}
	types := make([]string, len(blocks))
	for i, blk := range blocks {
		types[i] = str(obj(blk)["type"])
	}
	b.debugf("extractUserPromptBlocks: %d msgs in turn, %d blocks, types=%s", len(turn), len(blocks), strings.Join(types, ","))
	if !hasImage {
		return nil, nil
	}
	return blocks, nil
}

// steerBlocks is the trailing user turn as content blocks, nil when there is none.
// Blocks rather than text so image steers keep their images.
func (b *Bridge) steerBlocks(messages []map[string]any) ([]any, error) {
	blocks, err := b.extractUserPromptBlocks(messages)
	if err != nil || blocks != nil {
		return blocks, err
	}
	if text, _ := extractUserPrompt(messages); text != "" {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	return nil, nil
}
