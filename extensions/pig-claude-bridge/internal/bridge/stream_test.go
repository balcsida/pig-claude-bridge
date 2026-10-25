package bridge

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const fixtureDir = "../../../../tests/fixtures/sdk-streams/"

func frame(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func streamEvent(t *testing.T, event string) map[string]any {
	return frame(t, `{"type":"stream_event","event":`+event+`}`)
}

func play(t *testing.T, b *Bridge, q *QueryContext, frames ...map[string]any) {
	t.Helper()
	var captured string
	for _, f := range frames {
		b.feed(q, f, &captured)
	}
}

func blocksOfType(q *QueryContext, typ string) []map[string]any {
	var out []map[string]any
	for _, raw := range q.turnOutput["content"].([]any) {
		if m := raw.(map[string]any); m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

// blockSummary is [type, id, arguments] per block.
func blockSummary(q *QueryContext) [][]any {
	var out [][]any
	for _, raw := range q.turnOutput["content"].([]any) {
		m := raw.(map[string]any)
		out = append(out, []any{m["type"], m["id"], m["arguments"]})
	}
	return out
}

func blockTypes(q *QueryContext) []string {
	var out []string
	for _, raw := range q.turnOutput["content"].([]any) {
		out = append(out, raw.(map[string]any)["type"].(string))
	}
	return out
}

// usageFrames is every usage object in a fixture, in stream order.
func usageFrames(frames []map[string]any) []map[string]any {
	var out []map[string]any
	for _, f := range frames {
		ev, _ := f["event"].(map[string]any)
		msg, _ := ev["message"].(map[string]any)
		am, _ := f["message"].(map[string]any)
		for _, u := range []any{msg["usage"], ev["usage"], am["usage"]} {
			if m, ok := u.(map[string]any); ok {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// thinkingCounts are the thinking_tokens counts in stream order. The first is what
// this pi turn ends on: a fixture ending on a tool call spans several cycles, and
// the bridge closes the pi stream at the first tool-call boundary.
func thinkingCounts(frames []map[string]any) []int {
	var out []int
	for _, u := range usageFrames(frames) {
		if n := parseSdkUsage(u).ThinkingTokens; n != nil {
			out = append(out, *n)
		}
	}
	return out
}

func TestReplayTextOnlyTurn(t *testing.T) {
	t.Run("produces the assistant text and a clean stop", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("read")
		r := replayFrames(t, b, q, fixtureDir+"text.jsonl")

		var text string
		for _, blk := range blocksOfType(q, "text") {
			text += blk["text"].(string)
		}
		if strings.TrimSpace(text) != "ALPHA" {
			t.Errorf("text = %q", text)
		}
		if q.turnOutput["stopReason"] != "stop" || q.turnSawToolCall {
			t.Errorf("stopReason = %v, sawToolCall = %v", q.turnOutput["stopReason"], q.turnSawToolCall)
		}
		types := eventTypes(r.Events)
		if types[0] != "start" || !contains(types, "text_delta") {
			t.Errorf("event types = %v", types)
		}
		for _, e := range r.Events {
			if e["partial"] == nil {
				t.Errorf("%v carries no partial", e["type"])
			}
		}

		// The turn only ends with finalizeCurrentStream: done carries the message.
		b.finalizeCurrentStream(q, "")
		done := drain(r.stream)
		last := done[len(done)-1]
		if last["type"] != "done" || last["reason"] != "stop" {
			t.Fatalf("last event = %v", last)
		}
		if msg := last["message"].(map[string]any); !reflect.DeepEqual(msg["content"], q.turnOutput["content"]) {
			t.Errorf("done message content = %v", msg["content"])
		}
	})

	t.Run("reports usage and captures the session id", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("read")
		r := replayFrames(t, b, q, fixtureDir+"text.jsonl")
		u := usageOf(q.turnOutput)
		if intOf(u["output"]) <= 0 {
			t.Error("no output tokens")
		}
		if intOf(u["input"])+intOf(u["cacheRead"])+intOf(u["cacheWrite"]) <= 0 {
			t.Error("no prompt tokens")
		}
		if !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(r.CapturedSession) {
			t.Errorf("session id = %q", r.CapturedSession)
		}
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestReasoningTokensFromRecordedTurn(t *testing.T) {
	for _, name := range []string{"text", "single-tool", "parallel-tools"} {
		t.Run("surfaces the "+name+" turn's thinking tokens as usage.reasoning", func(t *testing.T) {
			b, _, _ := newTestBridge(t, false)
			q, _ := newStreamContext("read")
			r := replayFrames(t, b, q, fixtureDir+name+".jsonl")
			counts := thinkingCounts(r.Frames)
			if len(counts) == 0 || counts[0] <= 0 {
				t.Fatalf("fixture must carry a thinking-token count, got %v", counts)
			}
			u := usageOf(q.turnOutput)
			if u["reasoning"] != counts[0] {
				t.Errorf("reasoning = %v, want %d", u["reasoning"], counts[0])
			}
			if counts[0] > intOf(u["output"]) {
				t.Errorf("thinking_tokens %d > output_tokens %v", counts[0], u["output"])
			}
		})
	}

	t.Run("ignores counts from cycles that belong to the next pi turn", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("read")
		r := replayFrames(t, b, q, fixtureDir+"single-tool.jsonl")
		counts := thinkingCounts(r.Frames)
		if len(counts) < 2 {
			t.Fatalf("fixture must span more than one cycle, got %v", counts)
		}
		if q.turnOutput["stopReason"] != "toolUse" {
			t.Errorf("stopReason = %v", q.turnOutput["stopReason"])
		}
		if usageOf(q.turnOutput)["reasoning"] != counts[0] {
			t.Errorf("reasoning = %v, want %d", usageOf(q.turnOutput)["reasoning"], counts[0])
		}
	})
}

func TestReplaySingleToolTurn(t *testing.T) {
	t.Run("surfaces the tool call under its pi name and ends the turn on it", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("read")
		r := replayFrames(t, b, q, fixtureDir+"single-tool.jsonl")

		calls := blocksOfType(q, "toolCall")
		if len(calls) != 1 {
			t.Fatalf("tool calls = %d", len(calls))
		}
		if calls[0]["name"] != "read" || !strings.HasPrefix(calls[0]["id"].(string), "toolu_") {
			t.Errorf("call = %v", calls[0])
		}
		if !reflect.DeepEqual(calls[0]["arguments"], map[string]any{"path": "one.txt"}) {
			t.Errorf("arguments = %v", calls[0]["arguments"])
		}
		if _, open := calls[0]["partialJson"]; open {
			t.Error("partialJson survived content_block_stop")
		}
		if !q.turnSawToolCall || !reflect.DeepEqual(q.turnToolCallIDs, []string{calls[0]["id"].(string)}) {
			t.Errorf("sawToolCall = %v ids = %v", q.turnSawToolCall, q.turnToolCallIDs)
		}

		if q.currentPiStream != nil {
			t.Error("stream still attached after the tool call")
		}
		types := eventTypes(r.Events)
		if !reflect.DeepEqual(types[len(types)-2:], []string{"toolcall_end", "done"}) {
			t.Fatalf("tail = %v", types[len(types)-2:])
		}
		end, done := r.Events[len(r.Events)-2], r.Events[len(r.Events)-1]
		if end["toolCall"].(map[string]any)["name"] != "read" || end["contentIndex"] != 1 {
			t.Errorf("toolcall_end = %v", end)
		}
		if done["reason"] != "toolUse" || done["message"].(map[string]any)["stopReason"] != "toolUse" {
			t.Errorf("done = %v", done)
		}
		// The thinking block precedes the call: start, thinking, tool call.
		if !reflect.DeepEqual(dedupe(types[:len(types)-1]), []string{"start", "thinking_start", "thinking_delta", "thinking_end", "toolcall_start", "toolcall_delta", "toolcall_end"}) {
			t.Errorf("sequence = %v", dedupe(types))
		}
	})
}

func dedupe(ss []string) []string {
	var out []string
	for i, s := range ss {
		if i == 0 || s != ss[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func TestReplayParallelToolTurn(t *testing.T) {
	t.Run("keeps every parallel call, in emission order", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("read")
		replayFrames(t, b, q, fixtureDir+"parallel-tools.jsonl")

		calls := blocksOfType(q, "toolCall")
		if len(calls) < 2 {
			t.Fatalf("expected a parallel batch, got %d", len(calls))
		}
		var ids []string
		seen := map[string]bool{}
		for _, c := range calls {
			id := c["id"].(string)
			ids = append(ids, id)
			seen[id] = true
			if c["name"] != "read" {
				t.Errorf("name = %v", c["name"])
			}
		}
		if !reflect.DeepEqual(q.turnToolCallIDs, ids) {
			t.Errorf("routing ids %v != emitted %v", q.turnToolCallIDs, ids)
		}
		if len(seen) != len(calls) {
			t.Error("duplicate ids")
		}
	})

	t.Run("leaves nothing unmapped when the served tool list is empty", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext()
		replayFrames(t, b, q, fixtureDir+"parallel-tools.jsonl")
		if n := len(blocksOfType(q, "toolCall")); n != 0 || q.turnSawToolCall {
			t.Errorf("tool calls = %d, sawToolCall = %v", n, q.turnSawToolCall)
		}
	})
}

// A stream that got as far as a thinking block and the start of a tool call, then stalled.
func stalledStream(t *testing.T, id string) []map[string]any {
	return []map[string]any{
		streamEvent(t, `{"type":"message_start","message":{"id":"`+id+`"}}`),
		streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me look"}}`),
		streamEvent(t, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"mcp__custom-tools__bash","id":"toolu_dead","input":{}}}`),
		streamEvent(t, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`),
	}
}

func fallbackMessage(t *testing.T, id string) map[string]any {
	return frame(t, `{"type":"assistant","message":{"id":"`+id+`","content":[
		{"type":"thinking","thinking":"Let me look at both.","signature":"sig"},
		{"type":"text","text":"Reading the log."},
		{"type":"tool_use","name":"mcp__custom-tools__bash","id":"toolu_a","input":{"command":"ls"}},
		{"type":"tool_use","name":"mcp__custom-tools__bash","id":"toolu_b","input":{"command":"pwd"}}]}}`)
}

func TestNonStreamingFallbackAfterStalledStream(t *testing.T) {
	t.Run("delivers the fallback's tool calls to pi and ends the stream on toolUse", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, stream := newStreamContext("bash")
		play(t, b, q, append(stalledStream(t, "msg_dead"), fallbackMessage(t, "msg_fallback"))...)

		var ids []string
		for _, c := range blocksOfType(q, "toolCall") {
			ids = append(ids, c["id"].(string))
		}
		if !reflect.DeepEqual(ids, []string{"toolu_a", "toolu_b"}) || !reflect.DeepEqual(q.turnToolCallIDs, ids) {
			t.Errorf("ids = %v, turnToolCallIDs = %v", ids, q.turnToolCallIDs)
		}
		if q.turnOutput["stopReason"] != "toolUse" || q.currentPiStream != nil {
			t.Errorf("stopReason = %v, stream attached = %v", q.turnOutput["stopReason"], q.currentPiStream != nil)
		}
		events := drain(stream)
		if last := events[len(events)-1]; last["type"] != "done" || last["reason"] != "toolUse" {
			t.Errorf("last event = %v", last)
		}
	})

	t.Run("drops the dead stream's partial blocks instead of replaying them", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("bash")
		play(t, b, q, append(stalledStream(t, "msg_dead"), fallbackMessage(t, "msg_fallback"))...)

		if got := blockTypes(q); !reflect.DeepEqual(got, []string{"thinking", "text", "toolCall", "toolCall"}) {
			t.Fatalf("types = %v", got)
		}
		first := q.turnOutput["content"].([]any)[0].(map[string]any)
		if first["thinking"] != "Let me look at both." || first["thinkingSignature"] != "sig" {
			t.Errorf("thinking block = %v", first)
		}
		for _, s := range blockSummary(q) {
			if s[1] == "toolu_dead" {
				t.Error("dead tool call survived")
			}
		}
	})

	t.Run("drops the dead stream when Claude Code retries it as a new stream", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("bash")
		play(t, b, q, stalledStream(t, "msg_dead")...)
		play(t, b, q,
			streamEvent(t, `{"type":"message_start","message":{"id":"msg_retry"}}`),
			streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"mcp__custom-tools__bash","id":"toolu_retry","input":{}}}`),
			streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`),
			streamEvent(t, `{"type":"content_block_stop","index":0}`),
			streamEvent(t, `{"type":"message_stop"}`),
		)
		want := [][]any{{"toolCall", "toolu_retry", map[string]any{"command": "ls", "timeout": 120}}}
		if got := blockSummary(q); !reflect.DeepEqual(got, want) {
			t.Errorf("blocks = %v", got)
		}
		if q.turnOutput["stopReason"] != "toolUse" {
			t.Errorf("stopReason = %v", q.turnOutput["stopReason"])
		}
	})

	t.Run("still skips the assistant message that repeats a completed stream", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("bash")
		play(t, b, q,
			streamEvent(t, `{"type":"message_start","message":{"id":"msg_1"}}`),
			streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
			streamEvent(t, `{"type":"content_block_stop","index":0}`),
			frame(t, `{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"hi"}]}}`),
			streamEvent(t, `{"type":"message_stop"}`),
		)
		blocks := blocksOfType(q, "text")
		if len(blocks) != 1 || blocks[0]["text"] != "hi" {
			t.Errorf("blocks = %v", blocks)
		}
	})
}

const notifyParent = "mcp__custom-tools__notify_parent"

func cutInsideToolInput(t *testing.T, id string) []map[string]any {
	return []map[string]any{
		streamEvent(t, `{"type":"message_start","message":{"id":"`+id+`"}}`),
		streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Reporting both fixes now."}}`),
		streamEvent(t, `{"type":"content_block_stop","index":0}`),
		streamEvent(t, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"`+notifyParent+`","id":"toolu_cut","input":{}}}`),
		streamEvent(t, `{"type":"message_stop"}`),
	}
}

func TestToolCallWhoseContentBlockStopNeverArrived(t *testing.T) {
	summary := "Both fixes done."
	resumedSummary := [][]any{{"text", nil, nil}, {"toolCall", "toolu_resumed", map[string]any{"summary": summary}}}

	t.Run("is not dispatched to pi, and does not end pi's turn", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, stream := newStreamContext("notify_parent")
		play(t, b, q, cutInsideToolInput(t, "msg_cut")...)

		if got := blockTypes(q); !reflect.DeepEqual(got, []string{"text"}) {
			t.Errorf("types = %v", got)
		}
		if q.turnOutput["stopReason"] == "toolUse" || len(q.turnToolCallIDs) != 0 {
			t.Errorf("stopReason = %v ids = %v", q.turnOutput["stopReason"], q.turnToolCallIDs)
		}
		// Left open: Claude Code's resume is more of this same pi turn.
		if q.currentPiStream != stream {
			t.Fatal("pi's turn was ended")
		}
		for _, typ := range eventTypes(drain(stream)) {
			if typ == "done" || typ == "error" {
				t.Errorf("terminal event %q on an open turn", typ)
			}
		}
	})

	t.Run("is replaced by the resumed message's call, which ends the turn", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, stream := newStreamContext("notify_parent")
		play(t, b, q, cutInsideToolInput(t, "msg_cut")...)
		play(t, b, q,
			streamEvent(t, `{"type":"message_start","message":{"id":"msg_resumed"}}`),
			streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"`+notifyParent+`","id":"toolu_resumed","input":{}}}`),
			streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"summary\":\"Both fixes done.\"}"}}`),
			streamEvent(t, `{"type":"content_block_stop","index":0}`),
			streamEvent(t, `{"type":"message_stop"}`),
		)
		if got := blockSummary(q); !reflect.DeepEqual(got, resumedSummary) {
			t.Errorf("blocks = %v", got)
		}
		if !reflect.DeepEqual(q.turnToolCallIDs, []string{"toolu_resumed"}) || q.turnOutput["stopReason"] != "toolUse" || q.currentPiStream != nil {
			t.Errorf("ids = %v stopReason = %v", q.turnToolCallIDs, q.turnOutput["stopReason"])
		}
		events := drain(stream)
		if events[len(events)-1]["type"] != "done" {
			t.Errorf("last event = %v", events[len(events)-1]["type"])
		}
	})

	t.Run("is replaced by a resume that arrives as an assistant message instead", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("notify_parent")
		play(t, b, q, cutInsideToolInput(t, "msg_cut")...)
		play(t, b, q, frame(t, `{"type":"assistant","message":{"id":"msg_resumed","content":[
			{"type":"tool_use","name":"`+notifyParent+`","id":"toolu_resumed","input":{"summary":"Both fixes done."}}]}}`))
		if got := blockSummary(q); !reflect.DeepEqual(got, resumedSummary) {
			t.Errorf("blocks = %v", got)
		}
		if q.turnOutput["stopReason"] != "toolUse" || q.currentPiStream != nil {
			t.Errorf("stopReason = %v", q.turnOutput["stopReason"])
		}
	})

	t.Run("leaves a sibling call that did close to be dispatched", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, _ := newStreamContext("notify_parent")
		play(t, b, q,
			streamEvent(t, `{"type":"message_start","message":{"id":"msg_pair"}}`),
			streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"`+notifyParent+`","id":"toolu_first","input":{}}}`),
			streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"summary\":\"First.\"}"}}`),
			streamEvent(t, `{"type":"content_block_stop","index":0}`),
			streamEvent(t, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"`+notifyParent+`","id":"toolu_cut","input":{}}}`),
			streamEvent(t, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"summ"}}`),
			streamEvent(t, `{"type":"message_stop"}`),
		)
		want := [][]any{{"toolCall", "toolu_first", map[string]any{"summary": "First."}}}
		if got := blockSummary(q); !reflect.DeepEqual(got, want) {
			t.Errorf("blocks = %v", got)
		}
		if !reflect.DeepEqual(q.turnToolCallIDs, []string{"toolu_first"}) || q.turnOutput["stopReason"] != "toolUse" || q.currentPiStream != nil {
			t.Errorf("ids = %v stopReason = %v", q.turnToolCallIDs, q.turnOutput["stopReason"])
		}
	})

	t.Run("still dispatches a tool call that does close, with its full arguments", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, stream := newStreamContext("notify_parent")
		play(t, b, q,
			streamEvent(t, `{"type":"message_start","message":{"id":"msg_whole"}}`),
			streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Reporting now."}}`),
			streamEvent(t, `{"type":"content_block_stop","index":0}`),
			streamEvent(t, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"`+notifyParent+`","id":"toolu_whole","input":{}}}`),
			streamEvent(t, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"summary\":"}}`),
			streamEvent(t, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Both fixes done.\"}"}}`),
			streamEvent(t, `{"type":"content_block_stop","index":1}`),
			streamEvent(t, `{"type":"message_stop"}`),
		)
		if got := blockTypes(q); !reflect.DeepEqual(got, []string{"text", "toolCall"}) {
			t.Errorf("types = %v", got)
		}
		if got := blocksOfType(q, "toolCall")[0]["arguments"]; !reflect.DeepEqual(got, map[string]any{"summary": summary}) {
			t.Errorf("arguments = %v", got)
		}
		if !reflect.DeepEqual(q.turnToolCallIDs, []string{"toolu_whole"}) || q.turnOutput["stopReason"] != "toolUse" || q.currentPiStream != nil {
			t.Errorf("ids = %v", q.turnToolCallIDs)
		}
		events := drain(stream)
		if events[len(events)-1]["type"] != "done" {
			t.Errorf("last event = %v", events[len(events)-1]["type"])
		}
	})
}

// The Go counterpart of unit-finalized-immutable: that test fails a finalized turn
// through consumeQuery teardown; the property it pins is that pi's copy of a
// finalized turn is never rewritten, which here means the terminal event carries a
// copy and a finalized stream is never finalized again.
func TestFinalizedTurnIsImmutable(t *testing.T) {
	build := func(t *testing.T) []map[string]any {
		return []map[string]any{
			streamEvent(t, `{"type":"message_start","message":{"id":"msg_tool","usage":{}}}`),
			streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"mcp__custom-tools__read","input":{}}}`),
			streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`),
			streamEvent(t, `{"type":"content_block_stop","index":0}`),
			streamEvent(t, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{}}`),
			streamEvent(t, `{"type":"message_stop"}`),
		}
	}

	// The teardown paths (abort, SDK exception, error result after a tool call) are
	// covered by Task 20's provider-level tests. By the time these run the stream is
	// detached, so they pin only the deep-copy property: the delivered message is a
	// copy that later writes to turnOutput cannot reach.
	for name, fail := range map[string]func(b *Bridge, q *QueryContext){
		"an abort": func(b *Bridge, q *QueryContext) {
			q.turnOutput["stopReason"] = "aborted"
			q.turnOutput["errorMessage"] = "aborted"
			b.finalizeCurrentStream(q, "")
		},
		"an SDK exception": func(b *Bridge, q *QueryContext) {
			q.turnOutput["stopReason"] = "error"
			q.turnOutput["errorMessage"] = "CLI exited"
			b.finalizeCurrentStream(q, "")
		},
		"an error result": func(b *Bridge, q *QueryContext) {
			q.turnOutput["stopReason"] = "error"
			q.turnOutput["errorMessage"] = "API Error: Overloaded"
			b.finalizeCurrentStream(q, "")
		},
	} {
		t.Run("a turn finalized on a tool call is unchanged after "+name, func(t *testing.T) {
			b, _, _ := newTestBridge(t, false)
			q, stream := newStreamContext("read")
			play(t, b, q, build(t)...)
			done := stream.Result()
			if done["stopReason"] != "toolUse" {
				t.Fatalf("stopReason = %v", done["stopReason"])
			}
			raw, err := json.Marshal(done)
			if err != nil {
				t.Fatal(err)
			}

			fail(b, q)

			if now, _ := json.Marshal(done); string(now) != string(raw) {
				t.Errorf("finalized message changed:\n%s\n%s", now, raw)
			}
			if got := drain(stream); got[len(got)-1]["type"] != "done" {
				t.Errorf("last event = %v", got[len(got)-1]["type"])
			}
		})
	}

	t.Run("a turn still streaming reports an error result", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, stream := newStreamContext("read")
		play(t, b, q,
			streamEvent(t, `{"type":"message_start","message":{"id":"msg_text","usage":{}}}`),
			streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		)
		q.turnOutput["stopReason"] = "error"
		q.turnOutput["errorMessage"] = "API Error: Overloaded"
		b.finalizeCurrentStream(q, "")

		reply := stream.Result()
		if reply["stopReason"] != "error" || reply["errorMessage"] != "API Error: Overloaded" {
			t.Errorf("reply = %v", reply)
		}
		events := drain(stream)
		last := events[len(events)-1]
		if last["type"] != "error" || last["reason"] != "error" {
			t.Errorf("last event = %v", last)
		}
		// The host's error event carries the message in "error" (no "partial"): the text so far survives.
		errMsg := last["error"].(map[string]any)
		if txt := errMsg["content"].([]any)[0].(map[string]any)["text"]; txt != "partial" || errMsg["errorMessage"] != "API Error: Overloaded" {
			t.Errorf("error message = %v", errMsg)
		}
		if q.currentPiStream != nil {
			t.Error("stream still attached")
		}
	})
}

func TestFinalizeCurrentStream(t *testing.T) {
	t.Run("maps length to done/length and anything else to stop", func(t *testing.T) {
		for in, want := range map[string]string{"length": "length", "": "stop", "stop": "stop"} {
			b, _, _ := newTestBridge(t, false)
			q, stream := newStreamContext()
			b.finalizeCurrentStream(q, in)
			events := drain(stream)
			if got := eventTypes(events); !reflect.DeepEqual(got, []string{"start", "done"}) || events[1]["reason"] != want {
				t.Errorf("finalize(%q) = %v reason %v", in, got, events[1]["reason"])
			}
		}
	})
	t.Run("is a no-op without a stream and never finalizes twice", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q := newQueryContext()
		b.finalizeCurrentStream(q, "") // no stream, no turn
		q, stream := newStreamContext()
		b.finalizeCurrentStream(q, "")
		b.finalizeCurrentStream(q, "")
		if n := len(drain(stream)); n != 2 {
			t.Errorf("events = %d", n)
		}
	})
	t.Run("claiming over an unfinished stream warns, over a finished one does not", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		q, _ := newStreamContext()
		next, _ := newStreamContext()
		b.claimCurrentPiStream(next.currentPiStream, "first", q, nil)
		b.finalizeCurrentStream(q, "")
		b.claimCurrentPiStream(next.currentPiStream, "second", q, nil)
		log := readDebugLog(t, agentDir)
		if !strings.Contains(log, "overwritten before terminal event (first)") || strings.Contains(log, "(second)") {
			t.Errorf("log = %q", log)
		}
	})
}

func TestEventsCarryIndependentPartials(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	q, stream := newStreamContext("bash")
	play(t, b, q,
		streamEvent(t, `{"type":"message_start","message":{"id":"m"}}`),
		streamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`),
		streamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"b"}}`),
	)
	events := drain(stream)
	texts := []string{}
	for _, e := range events {
		if e["type"] == "text_delta" {
			texts = append(texts, e["partial"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string))
		}
	}
	if !reflect.DeepEqual(texts, []string{"a", "ab"}) {
		t.Errorf("partial text per delta = %v", texts)
	}
}

func TestMalformedFramesAreSkippedWithoutPanicking(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	q, _ := newStreamContext("bash")
	play(t, b, q,
		frame(t, `{"type":"stream_event"}`),
		frame(t, `{"type":"stream_event","event":"x"}`),
		streamEvent(t, `{"type":"content_block_start","index":0,"content_block":"x"}`),
		streamEvent(t, `{"type":"content_block_delta","index":9,"delta":{"type":"text_delta"}}`),
		streamEvent(t, `{"type":"content_block_stop","index":9}`),
		streamEvent(t, `{"type":"some_future_event"}`),
		streamEvent(t, `{"type":"ping"}`),
		frame(t, `{"type":"assistant","message":{"content":"x"}}`),
		frame(t, `{"type":"assistant","message":{"content":[null,5,{"type":"text","text":3}]}}`),
	)
	if len(q.turnOutput["content"].([]any)) != 0 {
		t.Errorf("blocks = %v", q.turnOutput["content"])
	}
}

func TestSyntheticAssistantMessageEmitsNoEvents(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	q, stream := newStreamContext("bash")
	play(t, b, q, stalledStream(t, "msg_dead")...)
	drain(stream) // Events pops what it returns, so later calls see only new events.
	play(t, b, q, frame(t, `{"type":"assistant","message":{"model":"<synthetic>","id":"s","content":[{"type":"text","text":"API Error: 529"}],"usage":{"input_tokens":3}}}`))
	if n := len(drain(stream)); n != 0 {
		t.Errorf("synthetic report pushed %d events", n)
	}
	if got := blockTypes(q); !reflect.DeepEqual(got, []string{"text"}) {
		t.Errorf("types = %v", got)
	}
	if usageOf(q.turnOutput)["input"] != 3 {
		t.Errorf("input = %v", usageOf(q.turnOutput)["input"])
	}
}

// ev is an expected event: type and contentIndex (-1 where the event has none).
type ev struct {
	typ string
	idx int
}

func rep(typ string, idx, n int) []ev {
	out := make([]ev, n)
	for i := range out {
		out[i] = ev{typ, idx}
	}
	return out
}

func seq(parts ...any) []ev {
	var out []ev
	for _, p := range parts {
		switch v := p.(type) {
		case ev:
			out = append(out, v)
		case []ev:
			out = append(out, v...)
		}
	}
	return out
}

func gotSeq(events []map[string]any) []ev {
	out := make([]ev, len(events))
	for i, e := range events {
		idx := -1
		if n, ok := e["contentIndex"]; ok {
			idx = n.(int)
		}
		out[i] = ev{e["type"].(string), idx}
	}
	return out
}

// toolCallEnds are the toolcall_end events' toolCall.arguments, in order.
func toolCallEnds(events []map[string]any) []any {
	var out []any
	for _, e := range events {
		if e["type"] == "toolcall_end" {
			out = append(out, e["toolCall"].(map[string]any)["arguments"])
		}
	}
	return out
}

// The thinking-delta counts are the thinking_delta frames before the first message_stop in each fixture.
func TestReplayExactEventSequences(t *testing.T) {
	start := ev{"start", -1}
	cases := []struct {
		name, file string
		want       []ev
		args       []any
		terminal   string
	}{
		{"text.jsonl", "text", seq(start, ev{"thinking_start", 0}, rep("thinking_delta", 0, 13), ev{"thinking_end", 0},
			ev{"text_start", 1}, ev{"text_delta", 1}, ev{"text_end", 1}), nil, ""},
		{"single-tool.jsonl", "single-tool", seq(start, ev{"thinking_start", 0}, rep("thinking_delta", 0, 12), ev{"thinking_end", 0},
			ev{"toolcall_start", 1}, rep("toolcall_delta", 1, 4), ev{"toolcall_end", 1}, ev{"done", -1}),
			[]any{map[string]any{"path": "one.txt"}}, "toolUse"},
		{"parallel-tools.jsonl", "parallel-tools", seq(start, ev{"thinking_start", 0}, rep("thinking_delta", 0, 24), ev{"thinking_end", 0},
			ev{"toolcall_start", 1}, rep("toolcall_delta", 1, 4), ev{"toolcall_end", 1},
			ev{"toolcall_start", 2}, rep("toolcall_delta", 2, 4), ev{"toolcall_end", 2},
			ev{"toolcall_start", 3}, rep("toolcall_delta", 3, 4), ev{"toolcall_end", 3}, ev{"done", -1}),
			[]any{map[string]any{"path": "one.txt"}, map[string]any{"path": "two.txt"}, map[string]any{"path": "three.txt"}}, "toolUse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _, _ := newTestBridge(t, false)
			q, _ := newStreamContext("read")
			r := replayFrames(t, b, q, fixtureDir+c.file+".jsonl")
			if got := gotSeq(r.Events); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("events = %v\nwant     %v", got, c.want)
			}
			if got := toolCallEnds(r.Events); !reflect.DeepEqual(got, c.args) {
				t.Errorf("toolcall_end arguments = %v, want %v", got, c.args)
			}
			if c.terminal != "" {
				if done := r.Events[len(r.Events)-1]; done["reason"] != c.terminal || done["message"].(map[string]any)["stopReason"] != c.terminal {
					t.Errorf("done = %v", done)
				}
			}
			for _, e := range r.Events {
				if e["type"] == "thinking_end" && e["content"] == "" {
					t.Errorf("thinking_end carries no content")
				}
			}
		})
	}
}

func TestFallbackMessageEmitsExactEvents(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	q, stream := newStreamContext("bash")
	play(t, b, q, fallbackMessage(t, "msg_fallback"))
	events := drain(stream)

	want := seq(ev{"start", -1},
		ev{"thinking_start", 0}, ev{"thinking_delta", 0}, ev{"thinking_end", 0},
		ev{"text_start", 1}, ev{"text_delta", 1}, ev{"text_end", 1},
		ev{"toolcall_start", 2}, ev{"toolcall_end", 2},
		ev{"toolcall_start", 3}, ev{"toolcall_end", 3},
		ev{"done", -1})
	if got := gotSeq(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	byType := func(typ string, n int) map[string]any {
		for _, e := range events {
			if e["type"] == typ {
				if n == 0 {
					return e
				}
				n--
			}
		}
		t.Fatalf("no %s event", typ)
		return nil
	}
	if byType("thinking_delta", 0)["delta"] != "Let me look at both." || byType("thinking_end", 0)["content"] != "Let me look at both." {
		t.Errorf("thinking events = %v / %v", byType("thinking_delta", 0), byType("thinking_end", 0))
	}
	if byType("text_delta", 0)["delta"] != "Reading the log." || byType("text_end", 0)["content"] != "Reading the log." {
		t.Errorf("text events = %v / %v", byType("text_delta", 0), byType("text_end", 0))
	}
	// mapToolArgs gives bash its default timeout.
	wantArgs := []any{map[string]any{"command": "ls", "timeout": 120}, map[string]any{"command": "pwd", "timeout": 120}}
	if got := toolCallEnds(events); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("toolcall_end arguments = %v", got)
	}
	if id := byType("toolcall_end", 1)["toolCall"].(map[string]any)["id"]; id != "toolu_b" {
		t.Errorf("second call id = %v", id)
	}
}
