package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// provider.webTools: the frame order is the one Claude Code 2.1.330 sent for a web
// call (ccproto's TestWebToolsRunInsideClaudeCodeBesideAServedTool pins it live).

var (
	webSearchTool = map[string]any{"name": "WebSearch", "description": "Search the web", "parameters": map[string]any{"type": "object"}}
	webFetchTool  = map[string]any{"name": "WebFetch", "description": "Fetch a URL", "parameters": map[string]any{"type": "object"}}
)

func webToolsOn(d *Deps) {
	on := true
	d.Config.Provider.WebTools = &on
}

func toolUse(id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// toolUseMessage is the stream of one API message made of the given tool_use blocks.
func toolUseMessage(msgID string, uses ...map[string]any) []any {
	ev := func(event map[string]any) map[string]any {
		return map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": event}
	}
	frames := []any{ev(map[string]any{"type": "message_start", "message": map[string]any{"id": msgID, "usage": map[string]any{}}})}
	for i, use := range uses {
		input, _ := json.Marshal(use["input"])
		frames = append(frames,
			ev(map[string]any{"type": "content_block_start", "index": i, "content_block": toolUse(str(use["id"]), str(use["name"]), map[string]any{})}),
			ev(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}}),
			assistantEcho(msgID, use),
			ev(map[string]any{"type": "content_block_stop", "index": i}))
	}
	return append(frames,
		ev(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{}}),
		ev(map[string]any{"type": "message_stop"}))
}

// webResultEcho is the user frame carrying Claude Code's own answer to a web call.
func webResultEcho(id, text string) map[string]any {
	echo := toolResultEcho(id)
	echo["message"] = map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": text}}}
	return echo
}

func webCallMsg(id string) map[string]any {
	return assistantMsg([]any{map[string]any{"type": "toolCall", "id": id, "name": "WebSearch", "arguments": map[string]any{"query": "go"}}}, "toolUse")
}

func webResultMsg(id, text string) map[string]any {
	return map[string]any{"role": "toolResult", "toolCallId": id, "toolName": "WebSearch", "isError": false, "timestamp": nextTimestamp(),
		"content": []any{map[string]any{"type": "text", "text": text}}}
}

type webAnswer struct {
	result WebToolResult
	err    error
}

// awaitWeb runs pi's side of the web tool call id.
func awaitWeb(ctx context.Context, b *Bridge, id string) <-chan webAnswer {
	out := make(chan webAnswer, 1)
	go func() {
		r, err := b.AwaitWebTool(ctx, id)
		out <- webAnswer{r, err}
	}()
	return out
}

// searches is a script: init, then a WebSearch call Claude Code runs itself.
func searches(id, toolUseID string) func(*spawn) {
	return func(s *spawn) {
		s.client.emit(initFrame(s, id))
		s.client.emit(toolUseMessage("msg_search", toolUse(toolUseID, "WebSearch", map[string]any{"query": "go"}))...)
	}
}

func waitHeld(t *testing.T, q *QueryContext) {
	t.Helper()
	waitFor(t, "the frame loop to hold Claude Code's next message", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.hold != nil
	})
}

func TestAWebToolCallIsAPiToolCallThatClaudeCodeAnswers(t *testing.T) {
	h := newProviderHarness(t, func(d *Deps) { webToolsOn(d); d.Debug = true })
	h.script(searches("cc-web", "toolu_web"))
	ctx := context.Background()
	first := h.call(ctx, "pi-web", msgs(userMsg("search go")), readTool, webSearchTool, webFetchTool)
	got := finalOf(t, first)
	want := []any{map[string]any{"type": "toolCall", "id": "toolu_web", "name": "WebSearch", "arguments": map[string]any{"query": "go"}}}
	if got["stopReason"] != "toolUse" || !reflect.DeepEqual(got["content"], want) {
		t.Fatalf("first turn = %v %v", got["stopReason"], got["content"])
	}
	s := h.lastSpawn()
	if !reflect.DeepEqual(s.opts.Tools, []string{"WebSearch", "WebFetch"}) {
		t.Fatalf("Claude Code's builtin tools = %v", s.opts.Tools)
	}
	q := h.liveFor("pi-web")

	// pi runs its WebSearch tool, which has nothing to do but wait for Claude Code.
	answer := awaitWeb(ctx, h.b, "toolu_web")
	select {
	case a := <-answer:
		t.Fatalf("the pi tool returned before Claude Code answered: %+v", a)
	case <-time.After(50 * time.Millisecond):
	}
	// Claude Code answers its own call and writes the rest of the turn at once, before
	// pi has handed the result back. The result text differs from the streamed text, so
	// the fallback that renders a result frame cannot stand in for a lost stream.
	s.client.emit(webResultEcho("toolu_web", "search results"))
	s.client.emit(textReplyFrames("Go 1.27")...)
	finish(s, "RESULT TEXT")
	if a := receive(t, answer, "Claude Code's answer"); a.err != nil || a.result != (WebToolResult{Text: "search results"}) {
		t.Fatalf("answer = %+v", a)
	}
	waitHeld(t, q)

	history := msgs(userMsg("search go"), webCallMsg("toolu_web"), webResultMsg("toolu_web", "search results"))
	second := h.call(ctx, "pi-web", history, readTool, webSearchTool, webFetchTool)
	if got := finalOf(t, second); replyText(got) != "Go 1.27" || got["stopReason"] != "stop" {
		t.Fatalf("second turn = %v", got)
	}
	if len(h.spawns()) != 1 {
		t.Fatalf("spawns = %d, the web result must not start a query", len(h.spawns()))
	}
	h.waitLive(0)
	q.mu.Lock()
	queued := len(q.pendingResults)
	q.mu.Unlock()
	if _, total := h.b.webCallCounts(q); queued != 0 || total != 0 {
		t.Fatalf("pi's copy of the web result was kept for a handler that never comes: queued=%d webCalls=%d", queued, total)
	}
	// Call, result and answer are each recorded against the session: the next turn reuses it.
	if st := h.b.sessionStateFor("pi-web"); st == nil || st.CCSessionID != ccSession(s) || len(st.Digests) != 4 || st.DirtyTail {
		t.Fatalf("mirror = %+v", st)
	}
	log := readDebugLog(t, h.agentDir)
	if strings.Contains(log, "BUG:") || strings.Contains(log, "still waiting") {
		t.Fatalf("debug log:\n%s", log)
	}
}

func TestWebToolsReachClaudeCodeOnlyWhenPiOffersThem(t *testing.T) {
	context := map[string]any{"tools": []any{readTool, webSearchTool}}
	served, builtin, toSDK, toPi := resolveMcpTools(context, "", []string{"WebSearch", "WebFetch"})
	if len(served) != 1 || served[0]["name"] != "read" {
		t.Fatalf("served over MCP = %v", served)
	}
	if !reflect.DeepEqual(builtin, []string{"WebSearch"}) {
		t.Fatalf("builtin = %v", builtin)
	}
	// A rebuilt transcript names the call as Claude Code's own tool, not as an MCP tool.
	if toSDK["WebSearch"] != "WebSearch" || toPi["WebSearch"] != "WebSearch" || toSDK["WebFetch"] != "" {
		t.Fatalf("maps = %v / %v", toSDK, toPi)
	}
	// With webTools off a pi tool of that name is a pi tool like any other.
	served, builtin, toSDK, _ = resolveMcpTools(context, "", nil)
	if len(served) != 2 || len(builtin) != 0 || builtin == nil || toSDK["WebSearch"] != "mcp__custom-tools__WebSearch" {
		t.Fatalf("webTools off: served=%d builtin=%#v sdk=%v", len(served), builtin, toSDK)
	}
}

// Claude Code runs a message's tools in order and parks on the MCP call first; pi
// answers only when all its tools are done. A web call pi waited on would deadlock.
func TestAWebCallAfterAPiToolCallStaysInsideClaudeCode(t *testing.T) {
	read := toolUse("toolu_read", "mcp__custom-tools__read", map[string]any{"path": "a"})
	search := toolUse("toolu_web", "WebSearch", map[string]any{"query": "go"})
	names := func(q *QueryContext) []string {
		var out []string
		for _, block := range blocksOfType(q, "toolCall") {
			out = append(out, str(block["name"]))
		}
		return out
	}
	newCtx := func(t *testing.T) *consumeHarness {
		h := newConsumeHarness(t, "read")
		h.q.customToolNameToPi["WebSearch"] = "WebSearch"
		return h
	}
	for name, tc := range map[string]struct {
		frames   []any
		want     []string
		webCalls int
	}{
		"after a pi tool call: hidden":                 {toolUseMessage("msg_mixed", read, search), []string{"read"}, 0},
		"before a pi tool call: shown":                 {toolUseMessage("msg_mixed", search, read), []string{"WebSearch", "read"}, 1},
		"after a pi tool call, without stream: hidden": {[]any{assistantEcho("msg_mixed", read, search)}, []string{"read"}, 0},
		"before a pi tool call, without stream: shown": {[]any{assistantEcho("msg_mixed", search, read)}, []string{"WebSearch", "read"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCtx(t)
			h.consumeFrames(tc.frames...)
			if got := names(h.q); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tool calls = %v", got)
			}
			if terminal := terminalEvents(drain(h.stream)); len(terminal) != 1 || terminal[0]["reason"] != "toolUse" {
				t.Fatalf("terminal events = %v", terminal)
			}
			if _, total := h.b.webCallCounts(h.q); total != tc.webCalls {
				t.Fatalf("web calls pi waits on = %d", total)
			}
			// The stream and the recording tap made the same call: the echoed blocks add
			// up to the message pi was given.
			if !h.q.recording.HasProgress() {
				t.Fatal("the emitted message was not recorded as covered")
			}
		})
	}
}

func TestAwaitWebTool(t *testing.T) {
	tools := []any{readTool, webSearchTool, webFetchTool}
	start := func(t *testing.T) (*providerHarness, *spawn) {
		h := newProviderHarness(t, webToolsOn)
		h.script(searches("cc-web", "toolu_web"))
		if got := finalOf(t, h.call(context.Background(), "pi-web", msgs(userMsg("search go")), tools...)); got["stopReason"] != "toolUse" {
			t.Fatalf("first turn = %v", got["stopReason"])
		}
		return h, h.lastSpawn()
	}

	t.Run("a call no query made is not waited on", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		if _, err := b.AwaitWebTool(context.Background(), "toolu_other_provider"); !errors.Is(err, ErrNoWebCall) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("Claude Code's error answer is an error result", func(t *testing.T) {
		h, s := start(t)
		echo := webResultEcho("toolu_web", "")
		echo["message"] = map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_web", "is_error": true,
			"content": []any{map[string]any{"type": "text", "text": "fetch failed"}}}}}
		s.client.emit(echo)
		if a := receive(t, awaitWeb(context.Background(), h.b, "toolu_web"), "the answer"); a.err != nil || a.result != (WebToolResult{Text: "fetch failed", IsError: true}) {
			t.Fatalf("answer = %+v", a)
		}
		s.client.exit(0, "")
	})

	t.Run("a query that ends first answers with an error", func(t *testing.T) {
		h, s := start(t)
		answer := awaitWeb(context.Background(), h.b, "toolu_web")
		s.client.exit(1, "boom")
		if a := receive(t, answer, "the answer"); a.err != nil || !a.result.IsError || !strings.Contains(a.result.Text, "Claude Code ended") {
			t.Fatalf("answer = %+v", a)
		}
		h.waitLive(0)
	})

	t.Run("a result frame with the call unanswered is not held back", func(t *testing.T) {
		h, s := start(t)
		answer := awaitWeb(context.Background(), h.b, "toolu_web")
		finish(s, "gave up")
		if a := receive(t, answer, "the answer"); a.err != nil || !a.result.IsError {
			t.Fatalf("answer = %+v", a)
		}
		h.waitLive(0)
	})

	t.Run("pi giving up on the tool aborts the query", func(t *testing.T) {
		h, s := start(t)
		ctx, cancel := context.WithCancel(context.Background())
		answer := awaitWeb(ctx, h.b, "toolu_web")
		cancel()
		if a := receive(t, answer, "the answer"); !errors.Is(a.err, context.Canceled) {
			t.Fatalf("answer = %+v", a)
		}
		h.waitLive(0)
		waitFor(t, "Claude Code to be closed", func() bool { _, closes, _ := s.client.counts(); return closes > 0 })
	})

	t.Run("a new prompt replaces a query held at Claude Code's next message", func(t *testing.T) {
		h, s := start(t)
		q := h.liveFor("pi-web")
		s.client.emit(webResultEcho("toolu_web", "search results"))
		s.client.emit(textReplyFrames("Go 1.27")...)
		waitHeld(t, q)
		// pi aborted the tool and the user typed something else.
		h.script(answers("cc-next", "ok"))
		history := msgs(userMsg("search go"), webCallMsg("toolu_web"), userMsg("never mind"))
		if got := finalOf(t, h.call(context.Background(), "pi-web", history, tools...)); replyText(got) != "ok" {
			t.Fatalf("next turn = %v", got)
		}
		if len(h.spawns()) != 2 {
			t.Fatalf("spawns = %d", len(h.spawns()))
		}
		waitFor(t, "the held query to let go", func() bool {
			q.mu.Lock()
			defer q.mu.Unlock()
			return q.hold == nil
		})
		if _, total := h.b.webCallCounts(q); total != 0 {
			t.Fatalf("the replaced query left %d web calls behind", total)
		}
	})
}
