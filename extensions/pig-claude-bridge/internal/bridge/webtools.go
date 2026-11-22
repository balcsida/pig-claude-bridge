package bridge

// provider.webTools: Claude Code's own WebSearch and WebFetch, shown to pi as tool
// calls. Claude Code runs the call itself; pi "runs" a tool of the same name whose
// only work is to wait here for Claude Code's answer (AwaitWebTool). So pi renders
// and records an ordinary tool call and result, and a session rebuilt from pi's
// history carries them.
//
// Two things differ from a pi tool served over MCP, where Claude Code parks on the
// handler until pi answers:
//   - Claude Code does not wait for pi. Once it has its answer it starts the next
//     API message while pi is still handing the result back, so the frame loop
//     holds that message until pi's next request attaches a stream (holdForPi).
//   - Claude Code runs a message's tools in order and parks on an MCP call before
//     starting a web call that follows it. pi answers only once all of its tools
//     are done, so a web call it waited on there would never return: such a call
//     stays inside Claude Code, unseen by pi (piToolNameLocked).

import (
	"context"
	"errors"
	"strings"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// WebToolResult is Claude Code's answer to a web tool call.
type WebToolResult struct {
	Text    string
	IsError bool
}

// ErrNoWebCall is AwaitWebTool's answer for a call no live query made: another
// provider's model called the tool, or the query that made it has ended.
var ErrNoWebCall = errors.New("no Claude Code query is running this web tool call")

// webCall is one web tool call pi was handed. done closes once result is set.
type webCall struct {
	q      *QueryContext
	done   chan struct{}
	result WebToolResult
}

// isWebToolLocked reports whether name is a Claude Code web tool of this query: the
// only names mapped to pi without the MCP prefix. The caller holds q.mu.
func (q *QueryContext) isWebToolLocked(name string) bool {
	_, ok := q.customToolNameToPi[name]
	return ok && !strings.HasPrefix(strings.ToLower(name), convert.MCPToolPrefix)
}

// piToolNameLocked is the pi tool a tool_use block of API message msgID is shown to
// pi as; ok is false for a block pi must not see. The stream, the assistant-message
// fallback and the recording tap all ask it, in block order, so they agree: a web
// call is hidden when a pi tool call precedes it in its message (see the file
// comment), and the first answer for a call stands. The caller holds q.mu.
func (q *QueryContext) piToolNameLocked(msgID string, block map[string]any) (string, bool) {
	name := str(block["name"])
	pi, ok := piToolNameFor(name, q.customToolNameToPi)
	if !ok {
		return "", false
	}
	if !q.isWebToolLocked(name) {
		q.piToolInMessage[msgID] = true
		return pi, true
	}
	id := str(block["id"])
	hidden, decided := q.hiddenWebCalls[id]
	if !decided {
		hidden = q.piToolInMessage[msgID]
		q.hiddenWebCalls[id] = hidden
	}
	return pi, !hidden
}

// expectWebCallsLocked registers the web calls of the message pi is about to be
// handed, so the pi tool finds them. The caller holds q.mu.
func (b *Bridge) expectWebCallsLocked(q *QueryContext) {
	b.webMu.Lock()
	defer b.webMu.Unlock()
	for _, raw := range q.blocksLocked() {
		block := obj(raw)
		id := str(block["id"])
		if block["type"] != "toolCall" || id == "" || !q.isWebToolLocked(str(block["name"])) {
			continue
		}
		if _, known := b.webCalls[id]; !known {
			b.webCalls[id] = &webCall{q: q, done: make(chan struct{})}
		}
	}
}

// answerWebCalls hands the tool results of a user frame to the web calls waiting on them.
func (b *Bridge) answerWebCalls(frame map[string]any) {
	for _, raw := range list(obj(frame["message"])["content"]) {
		block := obj(raw)
		if block["type"] != "tool_result" {
			continue
		}
		b.webMu.Lock()
		call := b.webCalls[str(block["tool_use_id"])]
		if call != nil && !call.answered() {
			call.result = WebToolResult{Text: toolResultText(block["content"]), IsError: block["is_error"] == true}
			close(call.done)
			b.debugf("web tool: Claude Code answered [%s]", str(block["tool_use_id"]))
		}
		b.webMu.Unlock()
	}
}

func (c *webCall) answered() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// toolResultText flattens a tool_result's content (a string, or text blocks) to text.
func toolResultText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var parts []string
	for _, raw := range list(content) {
		if block := obj(raw); block["type"] == "text" {
			parts = append(parts, str(block["text"]))
		}
	}
	return strings.Join(parts, "\n")
}

// webCallCounts are q's web calls pi has not handed back yet, and how many of them
// Claude Code has not answered.
func (b *Bridge) webCallCounts(q *QueryContext) (unanswered, total int) {
	b.webMu.Lock()
	defer b.webMu.Unlock()
	for _, call := range b.webCalls {
		if call.q != q {
			continue
		}
		total++
		if !call.answered() {
			unanswered++
		}
	}
	return unanswered, total
}

// takeWebResults drops from results the ones pi hands back for q's web calls:
// Claude Code produced them itself, so no MCP handler waits for them.
func (b *Bridge) takeWebResults(q *QueryContext, results []convert.McpResult) []convert.McpResult {
	b.webMu.Lock()
	defer b.webMu.Unlock()
	kept := make([]convert.McpResult, 0, len(results))
	for _, r := range results {
		if call := b.webCalls[r.ToolCallID]; call != nil && call.q == q {
			delete(b.webCalls, r.ToolCallID)
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// endWebCalls settles q's web calls when its Claude Code process is going away: a
// pi tool still waiting is answered with reason, and nothing of q stays behind.
func (b *Bridge) endWebCalls(q *QueryContext, reason string) {
	b.webMu.Lock()
	defer b.webMu.Unlock()
	for id, call := range b.webCalls {
		if call.q != q {
			continue
		}
		if !call.answered() {
			call.result = WebToolResult{Text: reason, IsError: true}
			close(call.done)
		}
		delete(b.webCalls, id)
	}
}

// AwaitWebTool is the pi side of a web tool call: it returns what Claude Code
// answered the call toolCallID with, waiting for it if need be. ErrNoWebCall means
// no query made that call. ctx ending is pi giving up on the tool (an abort): the
// query that made the call is aborted too, or Claude Code would write the rest of
// the turn for nobody.
func (b *Bridge) AwaitWebTool(ctx context.Context, toolCallID string) (WebToolResult, error) {
	b.webMu.Lock()
	call := b.webCalls[toolCallID]
	b.webMu.Unlock()
	if call == nil {
		return WebToolResult{}, ErrNoWebCall
	}
	select {
	case <-call.done:
		return call.result, nil
	case <-ctx.Done():
		call.q.mu.Lock()
		abort := call.q.abort
		call.q.mu.Unlock()
		if abort != nil {
			b.debugf("web tool: pi gave up on [%s], aborting its query", toolCallID)
			abort()
		}
		return WebToolResult{}, ctx.Err()
	}
}

// holdForPi keeps the frame loop from reading past the point pi has caught up
// with. After a turn ended on a web call, Claude Code goes on to its next API
// message as soon as it has the result, while pi is still returning that result:
// with no stream attached the message would have nowhere to go. frame waits here
// until pi's next request attaches a stream (releaseHold), the query is replaced,
// or ctx ends. Frames that are not new content pass: the tool results that answer
// pi's tools, late echoes of the message pi already has, and everything while a web
// call is still unanswered (a result frame then means the call never will be).
// Claude Code is not blocked meanwhile: the client queues what it sends.
func (b *Bridge) holdForPi(ctx context.Context, q *QueryContext, frame map[string]any) {
	switch str(frame["type"]) {
	case "result":
	case "stream_event":
		if !topLevel(frame) {
			return
		}
	case "assistant":
		q.mu.Lock()
		step := q.assistantSteps[str(obj(frame["message"])["id"])]
		late := step != nil && step.emitted != nil
		q.mu.Unlock()
		if !topLevel(frame) || late {
			return
		}
	default:
		return
	}
	held := false
	for ctx.Err() == nil {
		q.mu.Lock()
		waiting := q.currentPiStream == nil && !q.abandoned
		if waiting && q.hold == nil {
			q.hold = make(chan struct{})
		}
		hold := q.hold
		q.mu.Unlock()
		if !waiting {
			break
		}
		if unanswered, total := b.webCallCounts(q); total == 0 || unanswered > 0 {
			break
		}
		if !held {
			held = true
			b.debugf("web tool: holding Claude Code's next %s until pi returns the web tool result", str(frame["type"]))
		}
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	if held {
		b.debugf("web tool: released, aborted=%t", ctx.Err() != nil)
	}
}

// releaseHoldLocked wakes a frame loop waiting in holdForPi. The caller holds q.mu.
func (q *QueryContext) releaseHoldLocked() {
	if q.hold != nil {
		close(q.hold)
		q.hold = nil
	}
}
