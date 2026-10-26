package bridge

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Every function here that takes a *QueryContext locks q.mu itself, once, and
// calls only *Locked helpers underneath. Pushing to a ModelEventStream never
// blocks, so events are pushed under the lock.

func mapStopReason(s string) string {
	switch s {
	case "tool_use":
		return "toolUse"
	case "max_tokens":
		return "length"
	}
	return "stop"
}

// parsePartialJSON returns the decoded object, or nil when s is empty or not a JSON object.
func parsePartialJSON(s string) map[string]any {
	if s == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// deepCopy copies the JSON-shaped values assistant messages are made of. Events
// carry copies because the SDK marshals them asynchronously while the message
// keeps mutating. Strings are shared, so a copy costs the turn's blocks and keys,
// not its text.
// shortcut: every event copies the whole turn so far, O(events x blocks); share immutable blocks if long turns show up in profiles.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string         { s, _ := v.(string); return s }
func list(v any) []any         { l, _ := v.([]any); return l }

// markStreamComplete records that stream got its terminal event. The caller holds q.mu.
func (q *QueryContext) markStreamComplete(stream *sdk.ModelEventStream) {
	if stream == nil {
		return
	}
	if q.completedStreams == nil {
		q.completedStreams = map[*sdk.ModelEventStream]struct{}{}
	}
	q.completedStreams[stream] = struct{}{}
}

// recoverFrame keeps a panic from malformed input out of a long-lived process.
func (b *Bridge) recoverFrame(what string) {
	if r := recover(); r != nil {
		b.debugf("%s: recovered from panic: %v\n%s", what, r, debug.Stack())
	}
}

// recordUsage applies one usage frame and logs the same debug line at every call site.
func (b *Bridge) recordUsage(output map[string]any, usage SdkUsage, model map[string]any) {
	report := updateUsage(output, usage, model)
	u := obj(output["usage"])
	reasoning := ""
	if report.Reasoning != nil {
		reasoning = fmt.Sprintf(" reasoning=%d", *report.Reasoning)
	}
	b.debugf("usage: in=%v out=%v cacheRead=%v cacheWrite=%v total=%v%s cachePct=%d%% model=%v",
		u["input"], u["output"], u["cacheRead"], u["cacheWrite"], u["totalTokens"], reasoning, report.CachePct, model["id"])
}

// logServedContextWindow logs the context window an SDK result message says was
// served, which can differ from the one pi registered (issue #18).
func (b *Bridge) logServedContextWindow(label string, frame, model map[string]any) {
	usage := obj(frame["modelUsage"])
	keys := make([]string, 0, len(usage))
	for k := range usage {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	show := func(v any) any {
		if v == nil {
			return "?"
		}
		return v
	}
	for _, k := range keys {
		v := obj(usage[k])
		b.debugf("%s: served contextWindow=%v maxOutputTokens=%v servedModel=%s registered=%v",
			label, show(v["contextWindow"]), show(v["maxOutputTokens"]), k, model["contextWindow"])
	}
}

// claimCurrentPiStream makes stream the one events go to, warning when it replaces
// a stream that never got its terminal event.
func (b *Bridge) claimCurrentPiStream(stream *sdk.ModelEventStream, label string, q *QueryContext, onResponse func(map[string]any, map[string]any) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.currentPiStream != nil {
		if _, done := q.completedStreams[q.currentPiStream]; !done {
			b.debugf("WARNING: currentPiStream overwritten before terminal event (%s); activeQuery=%t pendingHandlers=%d", label, q.activeQuery != nil, len(q.pendingToolCalls))
		}
	}
	q.currentPiStream = stream
	q.currentOnResponse = onResponse
}

// pushLocked pushes an event carrying a deep copy of the turn so far.
func (q *QueryContext) pushLocked(event map[string]any) {
	event["partial"] = deepCopy(q.turnOutput)
	q.currentPiStream.Push(event)
}

func (q *QueryContext) ensureTurnStartedLocked() {
	if !q.turnStarted && q.currentPiStream != nil && q.turnOutput != nil {
		q.pushLocked(map[string]any{"type": "start"})
		q.turnStarted = true
	}
}

func (b *Bridge) ensureTurnStarted(q *QueryContext) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ensureTurnStartedLocked()
}

// endStreamLocked pushes the terminal event and detaches the stream. Push after a
// terminal event is dropped by the SDK, so there is no separate end call.
func (q *QueryContext) endStreamLocked(event map[string]any) {
	stream := q.currentPiStream
	stream.Push(event)
	q.markStreamComplete(stream)
	q.currentPiStream = nil
}

// finalizeCurrentStream ends the turn's stream: an error event when the turn
// failed, otherwise done. The events carry copies, so a later change to turnOutput
// cannot rewrite what pi already received.
func (b *Bridge) finalizeCurrentStream(q *QueryContext, stopReason string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.currentPiStream == nil || q.turnOutput == nil {
		return
	}
	shown, _ := json.Marshal(struct {
		StopReason any `json:"stopReason,omitempty"`
		Error      any `json:"error,omitempty"`
	}{q.turnOutput["stopReason"], q.turnOutput["errorMessage"]})
	b.debugf("provider: finalizeCurrentStream called, stopReason=%s, turnOutput=%s", stopReason, shown)
	q.ensureTurnStartedLocked()
	if q.turnOutput["stopReason"] == "error" {
		q.endStreamLocked(map[string]any{"type": "error", "reason": "error", "error": deepCopy(q.turnOutput)})
		return
	}
	reason := "stop"
	if stopReason == "length" {
		reason = "length"
	}
	q.endStreamLocked(map[string]any{"type": "done", "reason": reason, "message": deepCopy(q.turnOutput)})
}

// endOnToolUseLocked ends the pi stream on a completed tool call; the MCP handler
// blocks Claude Code until pi delivers the result on its next call.
func (q *QueryContext) endOnToolUseLocked() {
	q.turnOutput["stopReason"] = "toolUse"
	q.endStreamLocked(map[string]any{"type": "done", "reason": "toolUse", "message": deepCopy(q.turnOutput)})
}

func (q *QueryContext) blocksLocked() []any {
	blocks, _ := q.turnOutput["content"].([]any)
	return blocks
}

// addBlockLocked appends block and returns its content index.
func (q *QueryContext) addBlockLocked(block map[string]any) int {
	blocks := append(q.blocksLocked(), block)
	q.turnOutput["content"] = blocks
	return len(blocks) - 1
}

func (q *QueryContext) toolCallIDsLocked() []string {
	ids := []string{}
	for _, b := range q.blocksLocked() {
		if m := obj(b); m["type"] == "toolCall" {
			ids = append(ids, str(m["id"]))
		}
	}
	return ids
}

func (q *QueryContext) sawToolCallLocked() bool {
	return len(q.toolCallIDsLocked()) > 0
}

// emitLocked pushes a content event. A nil stream is skipped, as the TypeScript's
// optional chaining does in the fallback path.
func (q *QueryContext) emitLocked(typ string, index int, extra map[string]any) {
	if q.currentPiStream == nil {
		return
	}
	event := map[string]any{"type": typ, "contentIndex": index}
	for k, v := range extra {
		event[k] = v
	}
	q.pushLocked(event)
}

// processStreamEvent maps one stream_event frame to pi events. On message_stop
// after a completed tool call it ends the pi stream so pi can run the tool.
func (b *Bridge) processStreamEvent(q *QueryContext, frame map[string]any) {
	defer b.recoverFrame("processStreamEvent")
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.currentPiStream == nil || q.turnOutput == nil {
		return
	}
	q.turnSawStreamEvent = true
	event := obj(frame["event"])
	typ := str(event["type"])

	switch typ {
	case "message_start":
		msg := obj(event["message"])
		// Still open from an earlier message_start: Claude Code gave up on that
		// stream and is retrying it. Its blocks were never completed.
		if q.turnStreamOpen {
			b.dropAbandonedStreamBlocksLocked(q, fmt.Sprintf("restreamed as %v", msg["id"]))
		}
		q.turnToolCallIDs = []string{}
		q.turnStreamMessageID = str(msg["id"])
		q.turnStreamOpen = true
		q.turnStreamBlockStart = len(q.blocksLocked())
		if u := obj(msg["usage"]); u != nil {
			b.recordUsage(q.turnOutput, parseSdkUsage(u), q.model)
		}

	case "content_block_start":
		q.ensureTurnStartedLocked()
		cb := obj(event["content_block"])
		switch str(cb["type"]) {
		case "text":
			q.emitLocked("text_start", q.addBlockLocked(withIndex(map[string]any{"type": "text", "text": ""}, event)), nil)
		case "thinking":
			q.emitLocked("thinking_start", q.addBlockLocked(withIndex(map[string]any{"type": "thinking", "thinking": "", "thinkingSignature": ""}, event)), nil)
		case "tool_use":
			piName, ok := piToolNameFor(str(cb["name"]), q.customToolNameToPi)
			if !ok {
				b.debugf("processStreamEvent: skipping tool_use for unserved tool %s [%s] — CC rejects it and retries", str(cb["name"]), str(cb["id"]))
				return
			}
			q.turnToolCallIDs = append(q.turnToolCallIDs, str(cb["id"]))
			input := obj(cb["input"])
			if input == nil {
				input = map[string]any{}
			}
			q.emitLocked("toolcall_start", q.addBlockLocked(withIndex(map[string]any{
				"type": "toolCall", "id": cb["id"], "name": piName, "arguments": input, "partialJson": "",
			}, event)), nil)
		default:
			b.debugf("processStreamEvent: unhandled content_block_start type %v", cb["type"])
		}

	case "content_block_delta":
		index, block := q.findBlockLocked(event["index"])
		if block == nil {
			return
		}
		delta := obj(event["delta"])
		switch dt := str(delta["type"]); {
		case dt == "text_delta" && block["type"] == "text":
			block["text"] = str(block["text"]) + str(delta["text"])
			q.emitLocked("text_delta", index, map[string]any{"delta": str(delta["text"])})
		case dt == "thinking_delta" && block["type"] == "thinking":
			block["thinking"] = str(block["thinking"]) + str(delta["thinking"])
			q.emitLocked("thinking_delta", index, map[string]any{"delta": str(delta["thinking"])})
		case dt == "input_json_delta" && block["type"] == "toolCall":
			block["partialJson"] = str(block["partialJson"]) + str(delta["partial_json"])
			if args := parsePartialJSON(str(block["partialJson"])); args != nil {
				block["arguments"] = args
			}
			q.emitLocked("toolcall_delta", index, map[string]any{"delta": str(delta["partial_json"])})
		case dt == "signature_delta" && block["type"] == "thinking":
			block["thinkingSignature"] = str(block["thinkingSignature"]) + str(delta["signature"])
		default:
			b.debugf("processStreamEvent: unhandled content_block_delta type %v", delta["type"])
		}

	case "content_block_stop":
		index, block := q.findBlockLocked(event["index"])
		if block == nil {
			return
		}
		delete(block, "index")
		switch block["type"] {
		case "text":
			q.emitLocked("text_end", index, map[string]any{"content": block["text"]})
		case "thinking":
			q.emitLocked("thinking_end", index, map[string]any{"content": block["thinking"]})
		case "toolCall":
			q.turnSawToolCall = true
			args := parsePartialJSON(str(block["partialJson"]))
			if args == nil {
				args = obj(block["arguments"])
			}
			block["arguments"] = mapToolArgs(str(block["name"]), args)
			delete(block, "partialJson")
			q.emitLocked("toolcall_end", index, map[string]any{"toolCall": deepCopy(block)})
		}

	case "message_delta":
		q.turnOutput["stopReason"] = mapStopReason(str(obj(event["delta"])["stop_reason"]))
		if u := obj(event["usage"]); u != nil {
			b.recordUsage(q.turnOutput, parseSdkUsage(u), q.model)
		}

	case "message_stop":
		b.dropUnfinishedStreamBlocksLocked(q, "message_stop for "+q.turnStreamMessageID)
		q.turnStreamOpen = false
		if q.turnSawToolCall {
			// The SDK still yields an assistant message for this turn; the caller skips
			// it because the stream is gone. Cursor update is the next call's job.
			q.endOnToolUseLocked()
		}

	case "ping":
	default:
		b.debugf("processStreamEvent: unhandled event type %v", event["type"])
	}
}

// withIndex records Claude Code's block index; its absence from the block later
// marks a block that closed. An absent index stays absent.
func withIndex(block, event map[string]any) map[string]any {
	if idx := event["index"]; idx != nil {
		block["index"] = idx
	}
	return block
}

// findBlockLocked finds the block carrying Claude Code's index (nil matches nil,
// as undefined === undefined does).
func (q *QueryContext) findBlockLocked(index any) (int, map[string]any) {
	for i, raw := range q.blocksLocked() {
		if m := obj(raw); m != nil && m["index"] == index {
			return i, m
		}
	}
	return -1, nil
}

func (b *Bridge) dropUnfinishedStreamBlocks(q *QueryContext, why string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.turnOutput == nil {
		return
	}
	b.dropUnfinishedStreamBlocksLocked(q, why)
}

// dropUnfinishedStreamBlocksLocked removes the blocks of the message that just
// stopped which never got their content_block_stop, because the stream was cut
// inside them: a tool call carrying whatever JSON survived, under an id Claude Code
// drops and never dispatches, or a thinking block with no signature. Per block, not
// per message: blocks that did complete stay, tool calls included. Nothing here
// ends the pi turn; with no finished tool call left the resume is more of this turn.
func (b *Bridge) dropUnfinishedStreamBlocksLocked(q *QueryContext, why string) {
	blocks := q.blocksLocked()
	var dropped []string
	kept := append([]any{}, blocks[:min(q.turnStreamBlockStart, len(blocks))]...)
	for i := len(kept); i < len(blocks); i++ {
		m := obj(blocks[i])
		// index is deleted at content_block_stop, so it marks a block that never closed.
		if _, open := m["index"]; !open {
			kept = append(kept, blocks[i])
			continue
		}
		name := str(m["type"])
		if id := str(m["id"]); id != "" {
			name += "[" + id + "]"
		}
		dropped = append(dropped, name)
	}
	if len(dropped) == 0 {
		return
	}
	q.turnOutput["content"] = kept
	b.debugf("dropUnfinishedStreamBlocks: %s; dropped %s", why, strings.Join(dropped, ","))
	q.turnToolCallIDs = q.toolCallIDsLocked()
	q.turnSawToolCall = q.sawToolCallLocked()
}

func (b *Bridge) dropAbandonedStreamBlocks(q *QueryContext, why string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.turnOutput == nil {
		return
	}
	b.dropAbandonedStreamBlocksLocked(q, why)
}

// dropAbandonedStreamBlocksLocked removes the blocks a stream Claude Code
// abandoned mid-message: a thinking block with no signature and a tool call CC will
// never dispatch would otherwise hang the turn or replay a broken block.
func (b *Bridge) dropAbandonedStreamBlocksLocked(q *QueryContext, why string) {
	blocks := q.blocksLocked()
	start := min(q.turnStreamBlockStart, len(blocks))
	types := make([]string, 0, len(blocks)-start)
	for _, raw := range blocks[start:] {
		types = append(types, str(obj(raw)["type"]))
	}
	b.debugf("dropAbandonedStreamBlocks: %s; dropped %d blocks from %s types=%s", why, len(types), q.turnStreamMessageID, strings.Join(types, ","))
	q.turnOutput["content"] = append([]any{}, blocks[:start]...)
	q.turnToolCallIDs = []string{}
	q.turnSawToolCall = q.sawToolCallLocked()
	q.turnStreamOpen = false
}

// processAssistantMessage maps a complete assistant frame. It is the content path
// when no stream events preceded it (first message after a tool result) and when a
// stalled stream is retried without streaming, which arrives as one message under
// a new id. It keeps the stream lifecycle of processStreamEvent, ending on tool_use.
func (b *Bridge) processAssistantMessage(q *QueryContext, frame map[string]any) {
	defer b.recoverFrame("processAssistantMessage")
	q.mu.Lock()
	defer q.mu.Unlock()
	msg := obj(frame["message"])
	content, ok := msg["content"].([]any)
	if !ok || q.turnOutput == nil {
		return
	}
	// Claude Code reports API failures and its own notices as a <synthetic> assistant
	// message: a report, not model output. Keep the wording on the failed turn (the
	// error event carries it) but emit no events, so the turn still reads as a call
	// that produced no output. Issue #162.
	if msg["model"] == "<synthetic>" {
		if q.turnSawStreamEvent && q.turnStreamOpen {
			b.dropAbandonedStreamBlocksLocked(q, "synthetic failure report")
		}
		b.debugf("processAssistantMessage: <synthetic> message, keeping %d block(s) off the stream", len(content))
		for _, raw := range content {
			block := obj(raw)
			if block["type"] == "text" && str(block["text"]) != "" {
				q.addBlockLocked(map[string]any{"type": "text", "text": block["text"]})
			} else {
				b.debugf("processAssistantMessage: unhandled <synthetic> block type %v", block["type"])
			}
		}
		if u := obj(msg["usage"]); u != nil {
			b.recordUsage(q.turnOutput, parseSdkUsage(u), q.model)
		}
		return
	}
	if q.turnSawStreamEvent {
		// Same id was already delivered; a new id is CC's non-streaming fallback.
		// Deliberately delivered even if the stream stopped, at the risk of
		// duplication if CC renumbers it.
		id := str(msg["id"])
		if id == "" || q.turnStreamMessageID == "" || id == q.turnStreamMessageID {
			return
		}
		if q.turnStreamOpen {
			b.dropAbandonedStreamBlocksLocked(q, "non-streaming fallback "+id)
		}
	}
	q.turnToolCallIDs = []string{}
	types := make([]string, 0, len(content))
	for _, raw := range content {
		types = append(types, str(obj(raw)["type"]))
	}
	b.debugf("processAssistantMessage fallback: %d blocks, types=%s", len(content), strings.Join(types, ","))
	for _, raw := range content {
		block := obj(raw)
		switch block["type"] {
		case "text":
			text := str(block["text"])
			if text == "" {
				continue
			}
			q.ensureTurnStartedLocked()
			idx := q.addBlockLocked(map[string]any{"type": "text", "text": text})
			q.emitLocked("text_start", idx, nil)
			q.emitLocked("text_delta", idx, map[string]any{"delta": text})
			q.emitLocked("text_end", idx, map[string]any{"content": text})
		case "thinking":
			thinking := str(block["thinking"])
			q.ensureTurnStartedLocked()
			idx := q.addBlockLocked(map[string]any{"type": "thinking", "thinking": thinking, "thinkingSignature": str(block["signature"])})
			q.emitLocked("thinking_start", idx, nil)
			if thinking != "" {
				q.emitLocked("thinking_delta", idx, map[string]any{"delta": thinking})
			}
			q.emitLocked("thinking_end", idx, map[string]any{"content": thinking})
		case "tool_use":
			piName, ok := piToolNameFor(str(block["name"]), q.customToolNameToPi)
			if !ok {
				b.debugf("processAssistantMessage: skipping tool_use for unserved tool %s [%s] — CC rejects it and retries", str(block["name"]), str(block["id"]))
				continue
			}
			q.ensureTurnStartedLocked()
			q.turnSawToolCall = true
			q.turnToolCallIDs = append(q.turnToolCallIDs, str(block["id"]))
			idx := q.addBlockLocked(map[string]any{
				"type": "toolCall", "id": block["id"], "name": piName, "arguments": mapToolArgs(piName, obj(block["input"])),
			})
			q.emitLocked("toolcall_start", idx, nil)
			q.emitLocked("toolcall_end", idx, map[string]any{"toolCall": deepCopy(q.blocksLocked()[idx])})
		default:
			b.debugf("processAssistantMessage: unhandled block type %v", block["type"])
		}
	}
	if u := obj(msg["usage"]); u != nil {
		b.recordUsage(q.turnOutput, parseSdkUsage(u), q.model)
	}
	if q.turnSawToolCall && q.currentPiStream != nil {
		q.endOnToolUseLocked()
	}
}
