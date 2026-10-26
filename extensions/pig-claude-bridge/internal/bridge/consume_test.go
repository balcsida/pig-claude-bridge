package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/mcptools"
)

// recorder logs the interleaving of stdin writes, acks and handler releases.
type recorder struct {
	mu    sync.Mutex
	items []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, s)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.items...)
}

// consumeHarness is a live query: a QueryContext with an open pi stream, a fake
// client and the real MCP server for its tools.
type consumeHarness struct {
	b       *Bridge
	host    *recordingHost
	agent   string
	q       *QueryContext
	stream  *sdk.ModelEventStream
	client  *fakeClient
	server  *mcptools.Server
	ctx     context.Context
	cancelQ context.CancelFunc
	started []<-chan struct{}
}

func newConsumeHarness(t *testing.T, tools ...string) *consumeHarness {
	t.Helper()
	b, _, agent := newTestBridge(t, true)
	h := &consumeHarness{b: b, host: &recordingHost{}, agent: agent, client: newFakeClient()}
	h.q, h.stream = newStreamContext(tools...)
	h.q.activeQuery = h.client
	b.registerContext(h.q)
	h.ctx, h.cancelQ = context.WithCancel(context.Background())
	t.Cleanup(func() {
		h.cancelQ()
		_ = h.client.Close(context.Background())
		for _, done := range h.started { // the log directory is removed after this
			<-done
		}
	})
	if len(tools) > 0 {
		var defs []map[string]any
		for _, n := range tools {
			defs = append(defs, map[string]any{"name": n, "description": n, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
		}
		server, err := b.buildMcpServer(defs, h.q)
		if err != nil {
			t.Fatal(err)
		}
		h.server = server
	}
	return h
}

// start runs the query on its own goroutine, as the provider does.
func (h *consumeHarness) start() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.b.runConsume(h.ctx, h.host, h.q, h.client, nil)
	}()
	h.started = append(h.started, done)
	return done
}

// call sends a tools/call; a call still parked when the test ends is cancelled.
func (h *consumeHarness) call(t *testing.T, name, id string) <-chan toolReply {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return callTool(ctx, h.server, name, id)
}

func parkedIDs(q *QueryContext) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := []string{}
	for id := range q.pendingToolCalls {
		ids = append(ids, id)
	}
	return ids
}

func queuedIDs(q *QueryContext) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := []string{}
	for id := range q.pendingResults {
		ids = append(ids, id)
	}
	return ids
}

func waitParked(t *testing.T, q *QueryContext, n int) {
	t.Helper()
	waitFor(t, "parked handlers", func() bool { return len(parkedIDs(q)) == n })
}

func mcpResult(id, text string, isError bool) convert.McpResult {
	return convert.McpResult{ToolCallID: id, Content: convert.McpContent{{"type": "text", "text": text}}, IsError: isError}
}

func terminalEvents(events []map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e["type"] == "done" || e["type"] == "error" {
			out = append(out, e)
		}
	}
	return out
}

// --- frames ---

func cev(event string) string { return `{"type":"stream_event","event":` + event + `}` }

func toolUseFrames(id string) []any {
	return []any{
		cev(`{"type":"message_start","message":{"id":"msg_` + id + `","usage":{}}}`),
		cev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"` + id + `","name":"mcp__custom-tools__read","input":{}}}`),
		cev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`),
		cev(`{"type":"content_block_stop","index":0}`),
		cev(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{}}`),
		cev(`{"type":"message_stop"}`),
	}
}

func textStartFrames() []any {
	return []any{
		cev(`{"type":"message_start","message":{"id":"msg_text","usage":{}}}`),
		cev(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		cev(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
	}
}

func successFrame() map[string]any {
	return map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "DONE"}
}

const overloaded = "API Error: Overloaded"

func errorResultFrame() map[string]any {
	return map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": overloaded}
}

// replayOf is the replay acknowledgement Claude Code sends for a message we wrote.
func replayOf(t *testing.T, msg ccproto.UserMessage, extra map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["isReplay"] = true
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func wireOf(t *testing.T, msg ccproto.UserMessage) map[string]any {
	t.Helper()
	m := replayOf(t, msg, nil)
	delete(m, "isReplay")
	return m
}

// consumeFrames runs consumeQuery over a scripted client that exits cleanly after frames.
func (h *consumeHarness) consumeFrames(frames ...any) consumeResult {
	fc := newFakeClient()
	defer fc.Close(context.Background())
	fc.emit(frames...)
	fc.exit(0, "")
	return h.b.consumeQuery(h.ctx, h.host, h.q, fc)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// --- unit-queue.mjs ---

func TestToolResultQueue(t *testing.T) {
	tools := []string{"alpha", "beta", "gamma"}
	start := func(t *testing.T, ids ...string) *consumeHarness {
		h := newConsumeHarness(t, tools...)
		h.q.turnToolCallIDs = ids
		return h
	}
	deliver := func(h *consumeHarness, results ...convert.McpResult) {
		h.b.deliverToolResults(context.Background(), h.host, h.q, results, nil, 4)
	}

	t.Run("hands a handler the result that arrived before it, isError included", func(t *testing.T) {
		h := start(t, "toolu_1")
		deliver(h, mcpResult("toolu_1", "early", true))
		if got := queuedIDs(h.q); !reflect.DeepEqual(got, []string{"toolu_1"}) {
			t.Fatalf("queued = %v", got)
		}
		reply := receive(t, h.call(t, "alpha", "toolu_1"), "reply")
		if reply.Text != "early" || !reply.IsError {
			t.Fatalf("reply = %+v", reply)
		}
		if len(queuedIDs(h.q)) != 0 {
			t.Fatal("queue not drained")
		}
	})

	t.Run("parks a handler that arrives before its result, then releases it", func(t *testing.T) {
		h := start(t, "toolu_1")
		call := h.call(t, "alpha", "toolu_1")
		waitParked(t, h.q, 1)
		h.q.mu.Lock()
		name := h.q.pendingToolCalls["toolu_1"].ToolName
		h.q.mu.Unlock()
		if name != "alpha" {
			t.Fatalf("parked tool = %q", name)
		}
		deliver(h, mcpResult("toolu_1", "late", false))
		reply := receive(t, call, "reply")
		if reply.Text != "late" || reply.IsError {
			t.Fatalf("reply = %+v", reply)
		}
		if len(parkedIDs(h.q)) != 0 {
			t.Fatal("handler still parked")
		}
	})

	t.Run("pairs parallel calls by tool_use id, not by call or delivery order", func(t *testing.T) {
		h := start(t, "toolu_1", "toolu_2", "toolu_3")
		third := h.call(t, "gamma", "toolu_3")
		first := h.call(t, "alpha", "toolu_1")
		second := h.call(t, "beta", "toolu_2")
		waitParked(t, h.q, 3)
		deliver(h, mcpResult("toolu_2", "r2", false), mcpResult("toolu_3", "r3", false), mcpResult("toolu_1", "r1", false))
		for _, c := range []struct {
			ch   <-chan toolReply
			want string
		}{{first, "r1"}, {second, "r2"}, {third, "r3"}} {
			if got := receive(t, c.ch, "reply").Text; got != c.want {
				t.Fatalf("reply = %q, want %q", got, c.want)
			}
		}
		if len(parkedIDs(h.q)) != 0 || len(queuedIDs(h.q)) != 0 {
			t.Fatal("maps not empty")
		}
	})

	t.Run("moves an id between the maps without ever holding it in both", func(t *testing.T) {
		h := start(t, "toolu_1", "toolu_2")
		assertDisjoint := func() {
			for _, id := range parkedIDs(h.q) {
				for _, queued := range queuedIDs(h.q) {
					if id == queued {
						t.Fatalf("%s is in both maps", id)
					}
				}
			}
		}
		one := h.call(t, "alpha", "toolu_1") // parks
		waitParked(t, h.q, 1)
		assertDisjoint()
		deliver(h, mcpResult("toolu_1", "r1", false)) // releases it rather than queueing
		if got := receive(t, one, "reply").Text; got != "r1" {
			t.Fatalf("reply = %q", got)
		}
		if len(queuedIDs(h.q)) != 0 {
			t.Fatal("result queued although a handler was waiting")
		}
		deliver(h, mcpResult("toolu_2", "r2", false)) // queues: no handler yet
		if got := queuedIDs(h.q); !reflect.DeepEqual(got, []string{"toolu_2"}) {
			t.Fatalf("queued = %v", got)
		}
		assertDisjoint()
		if got := receive(t, h.call(t, "beta", "toolu_2"), "reply").Text; got != "r2" { // claims it from the queue
			t.Fatalf("reply = %q", got)
		}
		if len(queuedIDs(h.q)) != 0 || len(parkedIDs(h.q)) != 0 {
			t.Fatal("maps not empty")
		}
	})

	t.Run("leaves only the unanswered handler parked when a result never arrives", func(t *testing.T) {
		h := start(t, "toolu_1", "toolu_2")
		answered := h.call(t, "alpha", "toolu_1")
		h.call(t, "beta", "toolu_2") // never gets a result
		waitParked(t, h.q, 2)
		deliver(h, mcpResult("toolu_1", "r1", false))
		if got := receive(t, answered, "reply").Text; got != "r1" {
			t.Fatalf("reply = %q", got)
		}
		if got := parkedIDs(h.q); !reflect.DeepEqual(got, []string{"toolu_2"}) {
			t.Fatalf("parked = %v", got)
		}
	})
}

func TestAbandoningAQuery(t *testing.T) {
	t.Run("answers every parked handler and forgets queued results", func(t *testing.T) {
		h := newConsumeHarness(t, "alpha", "beta", "gamma")
		one, two := h.call(t, "alpha", "toolu_1"), h.call(t, "beta", "toolu_2")
		waitParked(t, h.q, 2)
		h.b.deliverToolResults(context.Background(), h.host, h.q, []convert.McpResult{mcpResult("toolu_3", "r3", false)}, nil, 4) // queued, no handler ever came

		h.q.releasePendingToolCalls("Query ended")

		if got := receive(t, one, "reply").Text; got != "Query ended" {
			t.Fatalf("reply = %q", got)
		}
		if got := receive(t, two, "reply").Text; got != "Query ended" {
			t.Fatalf("reply = %q", got)
		}
		if len(parkedIDs(h.q)) != 0 || len(queuedIDs(h.q)) != 0 {
			t.Fatal("a result nobody claimed must not outlive its query")
		}
	})

	// The abort race: the user aborts while delivery is parked on the steer's write
	// ack. Failing the prompt stream unwedges that wait.
	t.Run("unwedges a delivery parked on the steer ack, leaving no handler waiting", func(t *testing.T) {
		h := newConsumeHarness(t, "alpha")
		call := h.call(t, "alpha", "toolu_1")
		waitParked(t, h.q, 1)

		// Real stream, no consumer: the push parks exactly as when the CLI stops reading stdin.
		h.q.promptStream = newPromptStream()
		delivered := make(chan struct{})
		go func() {
			defer close(delivered)
			h.b.deliverToolResults(context.Background(), h.host, h.q, []convert.McpResult{mcpResult("toolu_1", "r1", false)}, []any{map[string]any{"type": "text", "text": "stop"}}, 4)
		}()
		select {
		case <-delivered:
			t.Fatal("delivery must still be parked on the ack")
		case <-time.After(30 * time.Millisecond):
		}
		if len(parkedIDs(h.q)) != 1 {
			t.Fatal("handler released before the steer was acknowledged")
		}

		h.b.drainForAbort(h.q)

		receive(t, delivered, "delivery to return") // returns rather than failing: the steer is lost, not the turn
		if got := receive(t, call, "reply").Text; got != "Operation aborted" {
			t.Fatalf("reply = %q", got)
		}
		if len(parkedIDs(h.q)) != 0 {
			t.Fatal("handler still parked")
		}
	})
}

func TestDeliveringAnExtractedTurnToItsHandlers(t *testing.T) {
	h := newConsumeHarness(t, "alpha", "beta")
	beta := h.call(t, "beta", "toolu_2")
	alpha := h.call(t, "alpha", "toolu_1")
	waitParked(t, h.q, 2)

	results, _ := convert.ExtractAllToolResults([]map[string]any{
		{"role": "user", "content": "prompt"},
		{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": "toolu_1"}, map[string]any{"type": "toolCall", "id": "toolu_2"}}},
		{"role": "toolResult", "toolCallId": "toolu_1", "content": "read the file"},
		{"role": "user", "content": "steer"},
		{"role": "toolResult", "toolCallId": "toolu_2", "content": "ran the command"},
	})
	h.b.deliverToolResults(context.Background(), h.host, h.q, results, nil, 5)

	if got := receive(t, alpha, "reply").Text; got != "read the file" {
		t.Fatalf("alpha = %q", got)
	}
	if got := receive(t, beta, "reply").Text; got != "ran the command" {
		t.Fatalf("beta = %q", got)
	}
	if len(parkedIDs(h.q)) != 0 {
		t.Fatal("every handler for the turn must be released")
	}
}

// --- unit-deliver-tool-results.mjs ---

func TestDeliverToolResults(t *testing.T) {
	steerText := []any{map[string]any{"type": "text", "text": "actually stop"}}
	type setup struct {
		b    *Bridge
		q    *QueryContext
		fc   *fakeClient
		rec  *recorder
		stop func()
	}
	// newDelivery wires a prompt stream to a fake client through the real writer goroutine.
	newDelivery := func(t *testing.T, behavior string) *setup {
		b, _, _ := newTestBridge(t, false)
		s := &setup{b: b, q: newQueryContext(), fc: newFakeClient(), rec: &recorder{}}
		t.Cleanup(func() { _ = s.fc.Close(context.Background()) })
		s.fc.onWrite = func(msg ccproto.UserMessage) error {
			var labels []string
			for _, blk := range msg.Content.([]any) {
				m := blk.(map[string]any)
				if text, ok := m["text"].(string); ok {
					labels = append(labels, text)
				} else {
					labels = append(labels, "["+m["type"].(string)+"]")
				}
			}
			s.rec.add("push:" + strings.Join(labels, ","))
			if behavior == "reject" {
				s.rec.add("push-rejected")
				return errors.New("prompt stream closed")
			}
			// The ack is the ordering boundary: the writer has only finished with the
			// message once it comes back for the next one.
			time.Sleep(5 * time.Millisecond)
			s.rec.add("ack")
			return nil
		}
		s.q.promptStream = newPromptStream()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); b.pumpPrompt(ctx, s.fc, s.q.promptStream) }()
		s.stop = func() { s.q.promptStream.End(); <-done; cancel() }
		t.Cleanup(s.stop)
		return s
	}
	handler := func(s *setup, name string) PendingToolCall {
		return PendingToolCall{ToolName: name, Resolve: func(convert.McpResult) { s.rec.add("resolve:" + name) }}
	}
	deliver := func(s *setup, results []convert.McpResult, steer []any) {
		s.b.deliverToolResults(context.Background(), nil, s.q, results, steer, 4)
	}
	result := func(id string) convert.McpResult { return mcpResult(id, "ok", false) }

	t.Run("writes the steer to stdin before releasing any tool result", func(t *testing.T) {
		s := newDelivery(t, "ack")
		s.q.pendingToolCalls["call-1"] = handler(s, "read")
		s.q.pendingToolCalls["call-2"] = handler(s, "bash")
		deliver(s, []convert.McpResult{result("call-1"), result("call-2")}, steerText)
		want := []string{"push:actually stop", "ack", "resolve:read", "resolve:bash"}
		if got := s.rec.list(); !reflect.DeepEqual(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	})

	t.Run("sends the steer with priority next so CC drains it at the tool boundary", func(t *testing.T) {
		s := newDelivery(t, "ack")
		s.q.pendingToolCalls["call-1"] = handler(s, "read")
		deliver(s, []convert.McpResult{result("call-1")}, steerText)
		sent := s.fc.writes()
		if len(sent) != 1 || sent[0].Priority != "next" || !reflect.DeepEqual(sent[0].Content, steerText) {
			t.Fatalf("sent = %+v", sent)
		}
		if !uuidPattern.MatchString(sent[0].UUID) {
			t.Fatalf("uuid = %q", sent[0].UUID)
		}
	})

	t.Run("keeps image blocks in the steer", func(t *testing.T) {
		s := newDelivery(t, "ack")
		withImage := []any{
			map[string]any{"type": "text", "text": "look at this"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBOR"}},
		}
		deliver(s, nil, withImage)
		if sent := s.fc.writes(); len(sent) != 1 || !reflect.DeepEqual(sent[0].Content, withImage) {
			t.Fatalf("sent = %+v", sent)
		}
	})

	// The caller has already advanced the session cursor past the steer, so a steer
	// that never reached CC would be skipped forever by count-based sync.
	t.Run("marks the session for rebuild when the push is rejected, and still delivers results", func(t *testing.T) {
		s := newDelivery(t, "reject")
		s.q.pendingToolCalls["call-1"] = handler(s, "read")
		s.b.setSessionStateFor("", &SessionState{SessionID: "abc", Cursor: 3, Cwd: "/tmp"})
		deliver(s, []convert.McpResult{result("call-1")}, steerText)
		want := []string{"push:actually stop", "push-rejected", "resolve:read"}
		if got := s.rec.list(); !reflect.DeepEqual(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
		if state := s.b.sessionStateFor(""); state == nil || !state.NeedsRebuild {
			t.Fatalf("state = %+v", state)
		}
		if !s.q.missedSteer {
			t.Fatal("missedSteer not set")
		}
	})

	t.Run("marks the session for rebuild when there is no prompt stream", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q := newQueryContext()
		b.setSessionStateFor("", &SessionState{SessionID: "abc", Cursor: 3, Cwd: "/tmp"})
		b.deliverToolResults(context.Background(), nil, q, nil, steerText, 4)
		if state := b.sessionStateFor(""); state == nil || !state.NeedsRebuild {
			t.Fatalf("state = %+v", state)
		}
	})

	t.Run("queues a result whose handler has not arrived yet", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q := newQueryContext()
		b.deliverToolResults(context.Background(), nil, q, []convert.McpResult{result("call-late")}, nil, 4)
		if q.pendingResults["call-late"].ToolCallID != "call-late" || len(q.pendingToolCalls) != 0 {
			t.Fatalf("pending = %v / %v", q.pendingResults, q.pendingToolCalls)
		}
	})

	t.Run("pairs results to handlers by tool_use id, not arrival order", func(t *testing.T) {
		s := newDelivery(t, "ack")
		s.q.pendingToolCalls["call-1"] = handler(s, "read")
		s.q.pendingToolCalls["call-2"] = handler(s, "bash")
		deliver(s, []convert.McpResult{result("call-2"), result("call-1")}, nil)
		want := []string{"resolve:bash", "resolve:read"}
		if got := s.rec.list(); !reflect.DeepEqual(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	})

	t.Run("warns when handlers are still waiting after delivery", func(t *testing.T) {
		s := newDelivery(t, "ack")
		host := &recordingHost{}
		s.q.pendingToolCalls["call-1"] = handler(s, "read")
		s.b.deliverToolResults(context.Background(), host, s.q, []convert.McpResult{result("other")}, nil, 4)
		if len(host.notices) != 1 || !strings.Contains(host.notices[0], "1 tool handler(s) still waiting") {
			t.Fatalf("notices = %v", host.notices)
		}
	})
}

// A steer into a prompt stream that already failed is a missed steer, with or
// without an earlier mirror (the consume-loop part of unit-cross-session-turns.mjs;
// the completion that must carry the mark is Task 20).
func TestMissedSteerAfterFailedPromptStream(t *testing.T) {
	steer := []any{map[string]any{"type": "text", "text": "missed steer"}}
	for _, hasMirror := range []bool{true, false} {
		name := "rebuilds even without an existing mirror"
		if hasMirror {
			name = "marks the existing mirror for rebuild"
		}
		t.Run(name, func(t *testing.T) {
			b, _, _ := newTestBridge(t, false)
			q := newQueryContext()
			q.piSessionID = "pi-parent"
			q.promptStream = newPromptStream()
			q.promptStream.Fail(errors.New("prompt stream closed"))
			if hasMirror {
				b.setSessionStateFor("pi-parent", &SessionState{SessionID: "cc", Cursor: 3, PiSessionID: "pi-parent"})
			}
			b.deliverToolResults(context.Background(), nil, q, nil, steer, 4)
			if !q.missedSteer || len(q.pendingSteerUUIDs) != 0 {
				t.Fatalf("missedSteer=%t uuids=%v", q.missedSteer, q.pendingSteerUUIDs)
			}
			state := b.sessionStateFor("pi-parent")
			if hasMirror != (state != nil && state.NeedsRebuild) {
				t.Fatalf("state = %+v", state)
			}
		})
	}
}

// --- unit-queued-steer.mjs ---

// steerHarness is a query whose input goes through the real writer goroutine.
type steerHarness struct {
	*consumeHarness
	writer   *fakeClient
	pumpDone chan struct{}
}

func newSteerHarness(t *testing.T) *steerHarness {
	h := &steerHarness{consumeHarness: newConsumeHarness(t), writer: newFakeClient(), pumpDone: make(chan struct{})}
	h.q.promptStream = newPromptStream()
	go func() {
		defer close(h.pumpDone)
		h.b.pumpPrompt(h.ctx, h.writer, h.q.promptStream)
	}()
	t.Cleanup(func() { h.cancelQ(); <-h.pumpDone; _ = h.writer.Close(context.Background()) })
	return h
}

func (h *steerHarness) steer(n int) {
	h.b.deliverToolResults(h.ctx, h.host, h.q, nil, []any{map[string]any{"type": "text", "text": "Continue with the next tool."}}, n)
}

func (h *steerHarness) assertClosed(t *testing.T) {
	t.Helper()
	receive(t, h.pumpDone, "the writer to finish")
	err := h.q.promptStream.Push(context.Background(), userMessage("too late", ""))
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("push after close = %v", err)
	}
}

func TestQueuedSteer(t *testing.T) {
	t.Run("keeps input open until every queued steer is replayed and its turn completes", func(t *testing.T) {
		h := newSteerHarness(t)
		h.steer(1)
		first := h.writer.writes()[0]
		if !uuidPattern.MatchString(first.UUID) {
			t.Fatalf("uuid = %q", first.UUID)
		}
		h.consumeFrames(successFrame(), wireOf(t, first), successFrame(),
			replayOf(t, first, map[string]any{"parent_tool_use_id": "subagent"}), successFrame())

		h.steer(2)
		sent := h.writer.writes()
		if h.q.missedSteer || sent[0].UUID == sent[1].UUID {
			t.Fatalf("missed=%t uuids=%q/%q", h.q.missedSteer, sent[0].UUID, sent[1].UUID)
		}
		h.consumeFrames(replayOf(t, first, nil), successFrame())
		if err := h.q.promptStream.Push(context.Background(), userMessage("input still connected", "")); err != nil {
			t.Fatalf("input closed early: %v", err)
		}
		h.consumeFrames(replayOf(t, sent[1], nil), successFrame())
		h.assertClosed(t)
	})

	t.Run("keeps input open during user replay and closes at the following result", func(t *testing.T) {
		h := newSteerHarness(t)
		h.steer(1)
		h.consumeFrames(replayOf(t, h.writer.writes()[0], nil))
		if err := h.q.promptStream.Push(context.Background(), userMessage("input stays open until the result", "")); err != nil {
			t.Fatalf("input closed early: %v", err)
		}
		h.consumeFrames(successFrame())
		h.assertClosed(t)
	})

	t.Run("protects a steer before its input write is acknowledged", func(t *testing.T) {
		h := newSteerHarness(t)
		gate := make(chan struct{})
		var once sync.Once
		h.writer.onWrite = func(ccproto.UserMessage) error {
			once.Do(func() { <-gate })
			return nil
		}
		delivery := make(chan struct{})
		go func() { defer close(delivery); h.steer(1) }()
		waitFor(t, "the steer write", func() bool { return len(h.writer.writes()) == 1 })
		message := h.writer.writes()[0]
		h.consumeFrames(successFrame()) // the steer is not acknowledged yet: input must stay open
		close(gate)                     // acknowledges the write
		receive(t, delivery, "delivery")
		if err := h.q.promptStream.Push(context.Background(), userMessage("still connected", "")); err != nil {
			t.Fatalf("input closed early: %v", err)
		}
		h.consumeFrames(replayOf(t, message, nil), successFrame())
		receive(t, h.pumpDone, "the writer to finish")
		if _, _, ends := h.writer.counts(); ends != 1 {
			t.Fatalf("EndInput calls = %d", ends)
		}
	})

	t.Run("does not wait for a failed steer write at completion", func(t *testing.T) {
		h := newSteerHarness(t)
		h.writer.onWrite = func(ccproto.UserMessage) error { return errors.New("write failed") }
		h.steer(1)
		h.consumeFrames(successFrame())
		receive(t, h.pumpDone, "the writer to finish")
		if !h.q.missedSteer || len(h.q.pendingSteerUUIDs) != 0 {
			t.Fatalf("missed=%t uuids=%v", h.q.missedSteer, h.q.pendingSteerUUIDs)
		}
		if err := h.q.promptStream.Push(context.Background(), userMessage("too late", "")); err == nil {
			t.Fatal("push accepted after the failed write")
		}
	})

	for _, c := range []struct {
		name   string
		result map[string]any
		steer  bool
	}{
		{"ordinary completion", successFrame(), false},
		{"success failure", errorResultFrame(), true},
		{"error_during_execution failure", map[string]any{"type": "result", "subtype": "error_during_execution", "errors": []any{"failed"}}, true},
	} {
		t.Run("closes on "+c.name+" without steer replays", func(t *testing.T) {
			h := newSteerHarness(t)
			if c.steer {
				h.steer(1)
			}
			h.consumeFrames(c.result)
			h.assertClosed(t)
		})
	}

	// Claude Code still runs a queued steer after an error result, even with stdin
	// closed. Its turn must not reach pi, where its stop reason replaced the failure.
	t.Run("ends the query at an error result even with a steer queued", func(t *testing.T) {
		h := newSteerHarness(t)
		h.steer(1)
		later := []any{
			replayOf(t, h.writer.writes()[0], nil),
			cev(`{"type":"message_start","message":{"id":"msg_steer","usage":{}}}`),
			cev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`),
			successFrame(),
		}
		fc := newFakeClient()
		defer fc.Close(context.Background())
		fc.emit(errorResultFrame())
		fc.emit(later...)
		res := h.b.consumeQuery(h.ctx, h.host, h.q, fc)

		if res.Failure == nil || res.Failure.Error() != overloaded {
			t.Fatalf("failure = %v", res.Failure)
		}
		waitFor(t, "the script to move on", func() bool { return fc.attempts.Load() >= 2 })
		if fc.sent.Load() != 1 {
			t.Fatalf("frames pulled = %d, want 1", fc.sent.Load())
		}
		if h.q.turnOutput["stopReason"] != "error" || h.q.turnOutput["errorMessage"] != overloaded {
			t.Fatalf("turn = %v", h.q.turnOutput)
		}
		h.assertClosed(t)
	})
}

// --- unit-discard-rewritten-query.mjs ---

type parked struct {
	q      *QueryContext
	client *fakeClient
	events *recorder
	ps     *PromptStream
}

// parkedQuery is a query parked mid-turn: CC asked for a tool and waits for the answer.
func parkedQuery(t *testing.T, b *Bridge, toolCallID, piSessionID string) *parked {
	t.Helper()
	p := &parked{q: newQueryContext(), client: newFakeClient(), events: &recorder{}, ps: newPromptStream()}
	t.Cleanup(func() { _ = p.client.Close(context.Background()) })
	p.client.onInterrupt = func() { p.events.add("interrupt") }
	p.client.onClose = func() { p.events.add("close") }
	p.q.activeQuery = p.client
	p.q.piSessionID = piSessionID
	p.q.turnToolCallIDs = []string{toolCallID}
	p.q.pendingToolCalls[toolCallID] = PendingToolCall{ToolName: "read", Resolve: func(r convert.McpResult) {
		text := r.Content[0]["text"].(string)
		p.events.add("release:" + text[:min(20, len(text))])
	}}
	p.q.promptStream = p.ps
	b.registerContext(p.q)
	return p
}

func TestDiscardRewrittenQuery(t *testing.T) {
	toolResults := []convert.McpResult{mcpResult("call_1", "file contents", false)}
	const discardReason = "Context was compacted; this query was discarded."

	t.Run("stops the parked query being a routing target for the turn's result", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		p := parkedQuery(t, b, "call_1", "pi-parent")
		if b.contextForToolResults(toolResults) != p.q {
			t.Fatal("precondition: the result routes to the parked query")
		}
		b.discardRewrittenQuery(p.q, discardReason)
		if b.contextForToolResults(toolResults) != nil {
			t.Fatal("a result routed here would answer over the conversation pi just discarded")
		}
		b.mu.Lock()
		_, routed := b.contexts[p.q]
		b.mu.Unlock()
		if p.q.activeQuery != nil || routed {
			t.Fatalf("activeQuery=%v routed=%t", p.q.activeQuery, routed)
		}
	})

	t.Run("settles everything awaiting the subprocess before killing it", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		p := parkedQuery(t, b, "call_1", "pi-parent")
		b.discardRewrittenQuery(p.q, discardReason)
		waitFor(t, "interrupt and close", func() bool {
			i, c, _ := p.client.counts()
			return i >= 1 && c >= 1
		})
		events := p.events.list()
		index := func(prefix string) int {
			for i, e := range events {
				if strings.HasPrefix(e, prefix) {
					return i
				}
			}
			return -1
		}
		released, interrupted := index("release:"), index("interrupt")
		if released == -1 || interrupted == -1 || released > interrupted || released > index("close") {
			t.Fatalf("handlers have to be released before the CLI goes: %v", events)
		}
		if err := p.ps.Push(context.Background(), userMessage("x", "")); err == nil || !strings.Contains(err.Error(), "conversation rewritten") {
			t.Fatalf("the parked ack has nothing left to resume it: %v", err)
		}
		if p.q.promptStream != nil || len(p.q.pendingToolCalls) != 0 {
			t.Fatalf("promptStream=%v pending=%v", p.q.promptStream, p.q.pendingToolCalls)
		}
	})

	t.Run("marks the query abandoned so its completion cannot overwrite the rebuild", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		p := parkedQuery(t, b, "call_1", "pi-parent")
		if p.q.isAbandoned() {
			t.Fatal("abandoned before the discard")
		}
		b.discardRewrittenQuery(p.q, discardReason)
		if !p.q.isAbandoned() {
			t.Fatal("its completion handler would capture the stale session id over the rebuilt one")
		}
	})

	t.Run("consumes the mark for its pi session, leaving sibling sessions armed", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		p := parkedQuery(t, b, "call_1", "pi-b")
		parkedQuery(t, b, "call_2", "pi-c")
		b.markRebuildForSession("pi-c", "session_compact:threshold")
		b.markRebuildForSession("pi-b", "session_compact:threshold")
		b.armStaleContexts()
		if !b.historyRewrittenFor("pi-b") {
			t.Fatal("precondition: armed")
		}
		b.discardRewrittenQuery(p.q, discardReason)
		if b.historyRewrittenFor("pi-b") || !b.historyRewrittenFor("pi-c") {
			t.Fatalf("pi-b=%t pi-c=%t", b.historyRewrittenFor("pi-b"), b.historyRewrittenFor("pi-c"))
		}
	})

	t.Run("rotates the session id of the discarding session's own mirror, because the rebuild follows the kill immediately", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.setSessionStateFor("pi-parent", &SessionState{SessionID: "abc", Cursor: 3, Cwd: "/tmp", PiSessionID: "pi-parent"})
		p := parkedQuery(t, b, "call_1", "pi-parent")
		b.discardRewrittenQuery(p.q, discardReason)
		if s := b.sessionStateFor("pi-parent"); s == nil || !s.ForceRotate || !s.NeedsRebuild {
			t.Fatalf("state = %+v", s)
		}
	})

	t.Run("leaves a foreign session's mirror untouched when the foreign query discards", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.setSessionStateFor("pi-parent", &SessionState{SessionID: "parent", Cursor: 3, Cwd: "/tmp", PiSessionID: "pi-parent"})
		p := parkedQuery(t, b, "call_1", "pi-b")
		b.markRebuildForSession("pi-b", "session_compact:threshold")
		b.armStaleContexts()
		b.discardRewrittenQuery(p.q, discardReason)
		if s := b.sessionStateFor("pi-parent"); s.NeedsRebuild || s.ForceRotate {
			t.Fatalf("the child's rebuild must not touch the parent's mirror: %+v", s)
		}
		if b.sessionStateFor("pi-b") != nil {
			t.Fatal("the child never had a mirror to rotate")
		}
	})

	t.Run("is safe on a context whose query already ended", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q := newQueryContext()
		b.registerContext(q)
		b.discardRewrittenQuery(q, discardReason)
		b.mu.Lock()
		_, routed := b.contexts[q]
		b.mu.Unlock()
		if q.activeQuery != nil || routed {
			t.Fatal("not discarded")
		}
	})

	// The set key was consumed by the first discard; a second compaction in the same
	// turn re-adds it and must arm the replacement query too.
	t.Run("arms the discarding session's replacement after a second rewrite (mid-turn double compaction)", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		first := parkedQuery(t, b, "call_1", "pi-parent")
		b.markRebuildForSession("pi-parent", "session_compact:threshold")
		b.armStaleContexts()
		b.discardRewrittenQuery(first.q, discardReason)
		if !b.isStale(first.q) {
			t.Fatal("the discard's own query is stale by construction")
		}
		replacement := parkedQuery(t, b, "call_2", "pi-parent")
		b.markRebuildForSession("pi-parent", "session_compact:threshold")
		b.armStaleContexts()
		if !b.isStale(replacement.q) {
			t.Fatal("a history rewritten under the replacement's predecessor makes this one stale too")
		}
		b.discardRewrittenQuery(replacement.q, discardReason)
		if b.historyRewrittenFor("pi-parent") {
			t.Fatal("the second discard must consume the mark again")
		}
	})
}

// --- unit-continuation-abort.mjs (the run-context binding; the end-of-turn
// "aborted" outcome of the provider call itself is Task 20) ---

func TestAbortOnARunThatContinuesAParkedQuery(t *testing.T) {
	// continued parks a query at a tool call, then continues it with a second run.
	type continued struct {
		*consumeHarness
		done    <-chan struct{}
		bind    func(context.Context, *sdk.ModelEventStream)
		second  *sdk.ModelEventStream
		aborted chan struct{}
	}
	park := func(t *testing.T) *continued {
		h := newConsumeHarness(t, "read")
		c := &continued{consumeHarness: h, aborted: make(chan struct{}, 4)}
		var once sync.Once
		onAbort := func() {
			once.Do(func() {
				c.aborted <- struct{}{}
				h.cancelQ()
				h.b.drainForAbort(h.q)
				h.b.killClient(h.client)
			})
		}
		bind, unbind := h.b.newRunBinder(h.q, h.ctx, onAbort)
		t.Cleanup(unbind)
		c.bind = bind
		h.client.emit(toolUseFrames("toolu_1")...)
		c.done = h.start()
		waitFor(t, "the first run to end on its tool call", func() bool {
			h.q.mu.Lock()
			defer h.q.mu.Unlock()
			return h.q.currentPiStream == nil
		})
		// The second run takes over the parked query.
		c.second = sdk.CreateAssistantMessageEventStream()
		h.b.claimCurrentPiStream(c.second, "tool-result", h.q, nil)
		h.q.resetTurnState(testModel)
		return c
	}

	t.Run("stops the query", func(t *testing.T) {
		c := park(t)
		runCtx, cancel := context.WithCancel(context.Background())
		c.bind(runCtx, c.second)
		c.client.emit(textStartFrames()...)
		waitFor(t, "the essay to start", func() bool { return c.client.sent.Load() == 9 })
		cancel()
		receive(t, c.done, "the abort to end the query")
		events := terminalEvents(drain(c.second))
		if len(events) != 1 || events[0]["reason"] != "aborted" {
			t.Fatalf("terminal events = %v", events)
		}
		if _, closes, _ := c.client.counts(); closes < 1 {
			t.Fatal("the query is not closed")
		}
	})

	t.Run("stops the query when the context is already cancelled", func(t *testing.T) {
		c := park(t)
		runCtx, cancel := context.WithCancel(context.Background())
		cancel()
		c.bind(runCtx, c.second)
		receive(t, c.done, "the abort to end the query")
		events := terminalEvents(drain(c.second))
		if len(events) != 1 || events[0]["reason"] != "aborted" {
			t.Fatalf("terminal events = %v", events)
		}
	})

	// Hosts that cancel each request's context once it ends must not kill the query
	// that run left parked at its own tool call.
	t.Run("ignores the abort once that run has ended at the next tool call", func(t *testing.T) {
		c := park(t)
		runCtx, cancel := context.WithCancel(context.Background())
		c.bind(runCtx, c.second)
		c.client.emit(toolUseFrames("toolu_2")...)
		waitFor(t, "the second run to end on its tool call", func() bool {
			c.q.mu.Lock()
			defer c.q.mu.Unlock()
			return c.q.currentPiStream == nil
		})
		events := terminalEvents(drain(c.second))
		if len(events) != 1 || events[0]["reason"] != "toolUse" {
			t.Fatalf("terminal events = %v", events)
		}
		cancel()
		select {
		case <-c.aborted:
			t.Fatal("the abort killed the parked query")
		case <-c.done:
			t.Fatal("the parked query ended")
		case <-time.After(50 * time.Millisecond):
		}
		if i, closes, _ := c.client.counts(); closes != 0 || i != 0 {
			t.Fatalf("interrupts=%d closes=%d: the parked query must survive", i, closes)
		}
	})
}

// --- unit-unserved-tool-use.mjs ---

func TestToolUseForAToolWeDoNotServe(t *testing.T) {
	toolMap := map[string]string{"mcp__custom-tools__bash": "bash"}
	turn := func(name, id string) []any {
		return []any{
			cev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"` + name + `","id":"` + id + `","input":{}}}`),
			cev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`),
			cev(`{"type":"content_block_stop","index":0}`),
			cev(`{"type":"message_stop"}`),
		}
	}
	newCtx := func(t *testing.T) *consumeHarness {
		h := newConsumeHarness(t)
		h.q.customToolNameToPi = toolMap
		return h
	}
	toolCalls := func(q *QueryContext) [][2]string {
		var out [][2]string
		for _, blk := range blocksOfType(q, "toolCall") {
			out = append(out, [2]string{str(blk["name"]), str(blk["id"])})
		}
		return out
	}

	t.Run("is skipped, and only the retry under the served name reaches pi", func(t *testing.T) {
		h := newCtx(t)
		h.consumeFrames(append(turn("bash", "toolu_phantom"), turn("mcp__custom-tools__bash", "toolu_retry")...)...)
		if got := toolCalls(h.q); !reflect.DeepEqual(got, [][2]string{{"bash", "toolu_retry"}}) {
			t.Fatalf("tool calls = %v", got)
		}
		if !reflect.DeepEqual(h.q.turnToolCallIDs, []string{"toolu_retry"}) {
			t.Fatalf("turnToolCallIDs = %v", h.q.turnToolCallIDs)
		}
		if args := blocksOfType(h.q, "toolCall")[0]["arguments"]; !reflect.DeepEqual(args, map[string]any{"command": "ls", "timeout": 120}) {
			t.Fatalf("arguments = %v", args)
		}
	})

	t.Run("does not end the pi stream, so the turn continues into the retry", func(t *testing.T) {
		h := newCtx(t)
		h.consumeFrames(turn("Bash", "toolu_phantom")...)
		if h.q.turnSawToolCall || len(h.q.turnOutput["content"].([]any)) != 0 {
			t.Fatalf("sawToolCall=%t content=%v", h.q.turnSawToolCall, h.q.turnOutput["content"])
		}
		if terminal := terminalEvents(drain(h.stream)); len(terminal) != 0 {
			t.Fatalf("terminal events = %v", terminal)
		}
	})

	// The assistant-message fallback has to make the same call, or the deadlock comes
	// back through the other door.
	t.Run("is skipped on the assistant-message path too", func(t *testing.T) {
		h := newCtx(t)
		h.consumeFrames(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "bash", "id": "toolu_phantom", "input": map[string]any{"command": "ls"}},
			map[string]any{"type": "tool_use", "name": "mcp__custom-tools__bash", "id": "toolu_retry", "input": map[string]any{"command": "ls"}},
		}}})
		if got := toolCalls(h.q); !reflect.DeepEqual(got, [][2]string{{"bash", "toolu_retry"}}) {
			t.Fatalf("tool calls = %v", got)
		}
	})
}

// --- unit-error-result.mjs (resultErrorText itself is pinned in ratelimit_test.go) ---

func TestARateLimitedFailure(t *testing.T) {
	rejection := map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected", "resetsAt": 1786141800, "rateLimitType": "five_hour"}}
	limitResult := map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "You're out of extra usage \u00b7 resets 6:30pm (America/New_York)"}
	toolFailurePrefix := regexp.MustCompile(`(?i)^[\w.:@/-]+ failed (?:(?:\(exit \d+\):)|(?:with exit code \d+))(?:\s|$)`)
	rateLimit := regexp.MustCompile(`(?i)rate\s*limit`)

	t.Run("is named as a rate limit so fallback chains fire", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames(rejection, limitResult)
		msg := str(h.q.turnOutput["errorMessage"])
		if !rateLimit.MatchString(msg) || toolFailurePrefix.MatchString(msg) {
			t.Fatalf("message = %q", msg)
		}
		if !strings.Contains(msg, limitResult["result"].(string)) || !strings.Contains(msg, "five_hour") {
			t.Fatalf("message = %q", msg)
		}
	})

	t.Run("labels only the failure it caused, not a later one", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames(rejection, limitResult)
		h.q.resetTurnState(testModel) // the failure ends that query; the next one fails on its own
		h.consumeFrames(errorResultFrame())
		if got := h.q.turnOutput["errorMessage"]; got != overloaded {
			t.Fatalf("message = %v", got)
		}
	})

	t.Run("leaves an unrelated failure alone", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames(errorResultFrame())
		if got := h.q.turnOutput["errorMessage"]; got != overloaded {
			t.Fatalf("message = %v", got)
		}
	})
}

func syntheticReport() map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": overloaded}}}}
}

func TestErrorResults(t *testing.T) {
	t.Run("marks the turn errored and finalizes with an error event", func(t *testing.T) {
		h := newConsumeHarness(t)
		res := h.consumeFrames(errorResultFrame())
		var failure *resultFailure
		if !errors.As(res.Failure, &failure) || failure.text != overloaded {
			t.Fatalf("failure = %v", res.Failure)
		}
		if h.q.turnOutput["stopReason"] != "error" || h.q.turnOutput["errorMessage"] != overloaded {
			t.Fatalf("turn = %v", h.q.turnOutput)
		}
		h.b.finalizeCurrentStream(h.q, str(h.q.turnOutput["stopReason"]))
		terminal := terminalEvents(drain(h.stream))
		if len(terminal) != 1 || terminal[0]["type"] != "error" || terminal[0]["reason"] != "error" ||
			obj(terminal[0]["error"])["errorMessage"] != overloaded {
			t.Fatalf("terminal = %v", terminal)
		}
	})

	t.Run("does not re-emit text the synthetic assistant message already delivered", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames(syntheticReport(), errorResultFrame())
		var texts []string
		for _, blk := range blocksOfType(h.q, "text") {
			texts = append(texts, str(blk["text"]))
		}
		if !reflect.DeepEqual(texts, []string{overloaded}) {
			t.Fatalf("texts = %v", texts)
		}
	})

	// A consumer that fails over only before output commits cannot reach the next
	// provider if Claude Code's own failure report counts as output: only start and
	// thinking events may precede the terminal error.
	t.Run("keeps a synthetic report off the stream, so failover is still possible", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames(syntheticReport(), errorResultFrame())
		h.b.finalizeCurrentStream(h.q, "error")
		events := drain(h.stream)
		for _, e := range events[:len(events)-1] {
			if typ := str(e["type"]); typ != "start" && !strings.HasPrefix(typ, "thinking_") {
				t.Fatalf("synthetic report committed output: %v", eventTypes(events))
			}
		}
		last := events[len(events)-1]
		if last["type"] != "error" || obj(last["error"])["errorMessage"] != overloaded {
			t.Fatalf("last = %v", last)
		}
		if got := h.q.turnOutput["content"]; !reflect.DeepEqual(got, []any{map[string]any{"type": "text", "text": overloaded}}) {
			t.Fatalf("content = %v", got)
		}
	})

	t.Run("synthetic report after a stalled stream drops the abandoned partial blocks", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames(
			cev(`{"type":"message_start","message":{"id":"msg_stalled"}}`),
			cev(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
			cev(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me look"}}`),
			cev(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"mcp__custom-tools__bash","id":"toolu_dead","input":{}}}`),
			cev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`),
			syntheticReport(), errorResultFrame())
		if got := h.q.turnOutput["content"]; !reflect.DeepEqual(got, []any{map[string]any{"type": "text", "text": overloaded}}) {
			t.Fatalf("content = %v", got)
		}
		if h.q.turnSawToolCall || h.q.turnStreamOpen {
			t.Fatalf("sawToolCall=%t streamOpen=%t", h.q.turnSawToolCall, h.q.turnStreamOpen)
		}
	})

	t.Run("still streams and finalizes a successful result normally", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "done"})
		if h.q.turnOutput["stopReason"] != "stop" || h.q.turnOutput["errorMessage"] != nil {
			t.Fatalf("turn = %v", h.q.turnOutput)
		}
		if got := h.q.turnOutput["content"]; !reflect.DeepEqual(got, []any{map[string]any{"type": "text", "text": "done"}}) {
			t.Fatalf("content = %v", got)
		}
		h.b.finalizeCurrentStream(h.q, "stop")
		events := drain(h.stream)
		if last := events[len(events)-1]; last["type"] != "done" {
			t.Fatalf("events = %v", eventTypes(events))
		}
		if got := strings.Join(eventTypes(events), ","); got != "start,text_start,text_delta,text_end,done" {
			t.Fatalf("events = %s", got)
		}
	})

	// A turn that ended on a tool call has closed its pi stream, and its turnOutput is
	// the message pi recorded (issue #181). A failure arriving then still closes stdin
	// and consumes the rate-limit rejection, but leaves that message alone.
	t.Run("leaves a turn that ended on a tool call unchanged", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.q.currentPiStream = nil // what the tool boundary leaves behind
		h.q.promptStream = newPromptStream()
		finalized := deepCopy(h.q.turnOutput)
		h.consumeFrames(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected"}}, errorResultFrame())
		if !reflect.DeepEqual(h.q.turnOutput, finalized) {
			t.Fatalf("turn changed: %v", h.q.turnOutput)
		}
		if err := h.q.promptStream.Push(context.Background(), userMessage("x", "")); err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("stdin not ended: %v", err)
		}
		if h.q.rateLimitRejection != nil {
			t.Fatal("rejection not consumed")
		}
	})

	t.Run("reports the dedicated error subtypes", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "errors": []any{"hit the cap"}})
		if h.q.turnOutput["stopReason"] != "error" || h.q.turnOutput["errorMessage"] != "hit the cap" {
			t.Fatalf("turn = %v", h.q.turnOutput)
		}
	})
}

// --- the consume loop itself ---

func TestConsumeLoop(t *testing.T) {
	t.Run("captures the session id of the init frame", func(t *testing.T) {
		h := newConsumeHarness(t)
		res := h.consumeFrames(map[string]any{"type": "system", "subtype": "init", "session_id": "cc-session"})
		if res.CapturedSessionID != "cc-session" || res.Failure != nil {
			t.Fatalf("res = %+v", res)
		}
	})

	t.Run("keeps the captured session id when a later init frame has none", func(t *testing.T) {
		h := newConsumeHarness(t)
		res := h.consumeFrames(
			map[string]any{"type": "system", "subtype": "init", "session_id": "cc-session"},
			map[string]any{"type": "system", "subtype": "init"},
			map[string]any{"type": "system", "subtype": "init", "session_id": ""})
		if res.CapturedSessionID != "cc-session" {
			t.Fatalf("captured = %q", res.CapturedSessionID)
		}
	})

	// Claude Code can cancel a tools/call and issue another under the same tool use id;
	// the cancelled handler must take only its own entry with it.
	t.Run("a cancelled tool call leaves a later call's parked entry alone", func(t *testing.T) {
		h := newConsumeHarness(t)
		handler := h.b.toolCallHandler(h.q, "read")
		firstCtx, cancelFirst := context.WithCancel(context.Background())
		firstDone := make(chan struct{})
		go func() { defer close(firstDone); _, _ = handler(firstCtx, "toolu_1", nil) }()
		waitParked(t, h.q, 1)
		h.q.mu.Lock()
		firstReply := h.q.pendingToolCalls["toolu_1"].reply
		h.q.mu.Unlock()

		second := make(chan convert.McpResult, 1)
		go func() { r, _ := handler(context.Background(), "toolu_1", nil); second <- r }()
		waitFor(t, "the second call to park", func() bool {
			h.q.mu.Lock()
			defer h.q.mu.Unlock()
			return h.q.pendingToolCalls["toolu_1"].reply != firstReply
		})
		cancelFirst()
		<-firstDone

		h.b.deliverToolResults(context.Background(), nil, h.q, []convert.McpResult{{ToolCallID: "toolu_1", Content: convert.McpContent{{"type": "text", "text": "file"}}}}, nil, 3)
		if got := receive(t, second, "the second call's result"); got.Content[0]["text"] != "file" {
			t.Fatalf("result = %+v", got)
		}
	})

	t.Run("skips a malformed frame and keeps consuming", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.consumeFrames("not json", map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "done"})
		if got := h.q.turnOutput["content"]; !reflect.DeepEqual(got, []any{map[string]any{"type": "text", "text": "done"}}) {
			t.Fatalf("content = %v", got)
		}
	})

	t.Run("hands every recorded rate_limit_event to the held-event report", func(t *testing.T) {
		h := newConsumeHarness(t)
		frames := readFrames(t, fixtureDir+"text.jsonl")
		events := 0
		var all []any
		for _, f := range frames {
			if f["type"] == "rate_limit_event" {
				events++
			}
			all = append(all, f)
		}
		if events == 0 {
			t.Fatal("the fixture should carry rate_limit_event frames")
		}
		h.consumeFrames(all...)
		log := readDebugLog(t, h.agent)
		if got := strings.Count(log, "consumeQuery: rate_limit_event"); got != events {
			t.Fatalf("rate_limit_event lines = %d, want %d", got, events)
		}
		if got := strings.Count(log, "rate limit: headers"); got != events {
			t.Fatalf("reported = %d, want %d", got, events)
		}
		if h.q.turnOutput["stopReason"] != "stop" {
			t.Fatalf("turn = %v", h.q.turnOutput)
		}
	})

	t.Run("a panic in bridge code becomes one error event and never escapes", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.host = &recordingHost{}
		panicking := &panicHost{}
		done := make(chan struct{})
		h.client.emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected"}})
		go func() { defer close(done); h.b.runConsume(h.ctx, panicking, h.q, h.client, nil) }()
		h.started = append(h.started, done)
		receive(t, done, "the consume goroutine")
		terminal := terminalEvents(drain(h.stream))
		if len(terminal) != 1 || terminal[0]["type"] != "error" || !strings.Contains(str(obj(terminal[0]["error"])["errorMessage"]), "internal error") {
			t.Fatalf("terminal = %v", terminal)
		}
	})
}

type panicHost struct{ recordingHost }

func (*panicHost) Notify(string, string) { panic("host exploded") }

// --- Review Focus 4 ---

func TestProcessExitWhileParkedReleasesHandlerAndMarksRebuild(t *testing.T) {
	h := newConsumeHarness(t, "read")
	h.q.piSessionID = "pi-exit"
	h.b.setSessionStateFor("pi-exit", &SessionState{SessionID: "abc", Cursor: 3, Cwd: "/tmp", PiSessionID: "pi-exit"})
	done := h.start()
	reply := h.call(t, "read", "toolu_1")
	waitParked(t, h.q, 1)

	h.client.exit(1, "boom")

	got := receive(t, reply, "the parked handler to return")
	if got.Text != "Query ended" || got.Err != "" {
		t.Fatalf("reply = %+v", got)
	}
	receive(t, done, "the consume goroutine")
	var errorEvents []map[string]any
	for _, e := range drain(h.stream) {
		if e["type"] == "error" {
			errorEvents = append(errorEvents, e)
		}
	}
	if len(errorEvents) != 1 {
		t.Fatalf("error events = %d, want exactly 1", len(errorEvents))
	}
	msg := str(obj(errorEvents[0]["error"])["errorMessage"])
	if !strings.Contains(msg, "boom") || !strings.Contains(msg, "exited with code 1") || strings.Count(msg, "boom") != 1 {
		t.Fatalf("message = %q", msg)
	}
	if state := h.b.sessionStateFor("pi-exit"); state == nil || !state.NeedsRebuild {
		t.Fatalf("state = %+v", state)
	}
	if len(parkedIDs(h.q)) != 0 {
		t.Fatal("a handler is still parked")
	}
}

// --- unit-finalized-immutable.mjs and the held event of unit-rate-limit.mjs ---

type lateFailure struct {
	name       string
	frames     []any
	trigger    func(h *consumeHarness)
	stopReason string
}

func lateFailures() []lateFailure {
	steerTurn := []any{
		map[string]any{"type": "user", "parent_tool_use_id": nil, "isReplay": true, "uuid": "steer", "message": map[string]any{"role": "user", "content": "steer"}},
		cev(`{"type":"message_start","message":{"id":"msg_steer","usage":{}}}`),
		cev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`),
		successFrame(),
	}
	return []lateFailure{
		{"an abort", nil, func(h *consumeHarness) { h.cancelQ() }, "aborted"},
		{"an SDK exception", nil, func(h *consumeHarness) { h.client.exit(1, "CLI exited") }, "error"},
		{"an error result", append([]any{errorResultFrame()}, steerTurn...), func(*consumeHarness) {}, "error"},
	}
}

func TestATurnFinalizedOnAToolCall(t *testing.T) {
	for _, failure := range lateFailures() {
		t.Run("is unchanged after "+failure.name, func(t *testing.T) {
			h := newConsumeHarness(t, "read")
			h.client.emit(toolUseFrames("toolu_1")...)
			done := h.start()
			waitFor(t, "the turn to end on its tool call", func() bool {
				h.q.mu.Lock()
				defer h.q.mu.Unlock()
				return h.q.currentPiStream == nil && h.q.turnOutput["stopReason"] == "toolUse"
			})
			h.q.mu.Lock()
			snapshot := deepCopy(h.q.turnOutput)
			h.q.mu.Unlock()
			h.client.emit(failure.frames...)
			failure.trigger(h)
			receive(t, done, "teardown")
			if !reflect.DeepEqual(h.q.turnOutput, snapshot) {
				t.Fatalf("turn changed:\n got %v\nwant %v", h.q.turnOutput, snapshot)
			}
		})
	}
}

func TestATurnStillStreaming(t *testing.T) {
	for _, failure := range lateFailures() {
		t.Run("reports "+failure.name, func(t *testing.T) {
			h := newConsumeHarness(t)
			h.client.emit(textStartFrames()...)
			done := h.start()
			waitFor(t, "the text to stream", func() bool {
				h.q.mu.Lock()
				defer h.q.mu.Unlock()
				blocks := h.q.blocksLocked()
				return len(blocks) == 1 && str(obj(blocks[0])["text"]) == "partial"
			})
			h.client.emit(failure.frames...)
			failure.trigger(h)
			receive(t, done, "teardown")
			terminal := terminalEvents(drain(h.stream))
			if len(terminal) != 1 || terminal[0]["type"] != "error" || terminal[0]["reason"] != failure.stopReason {
				t.Fatalf("terminal = %v", terminal)
			}
			if msg := obj(terminal[0]["error"])["errorMessage"]; msg == nil || msg == "" {
				t.Fatalf("no error message: %v", terminal[0])
			}
		})
	}

	t.Run("drops the session mirror at an error result, so a queued steer is rebuilt", func(t *testing.T) {
		h := newConsumeHarness(t)
		h.q.piSessionID = "pi-finalized"
		h.b.setSessionStateFor("pi-finalized", &SessionState{SessionID: "cc", Cursor: 2, PiSessionID: "pi-finalized"})
		h.client.emit(textStartFrames()...)
		h.client.emit(errorResultFrame())
		receive(t, h.start(), "teardown")
		if h.b.sessionStateFor("pi-finalized") != nil {
			t.Fatal("the mirror survived the error result")
		}
	})
}

func TestAbortedQueryDropsItsHeldRateLimitEvent(t *testing.T) {
	h := newConsumeHarness(t, "read")
	h.client.emit(toolUseFrames("toolu_1")...)
	h.client.emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
		"status": "allowed", "rateLimitType": "five_hour",
		"unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": 0.1, "resetsAt": 1789770600}},
	}})
	done := h.start()
	// The stream ended on the tool call, so the event stays held for the next call.
	waitFor(t, "the event to be held", func() bool {
		h.q.mu.Lock()
		defer h.q.mu.Unlock()
		return h.q.heldRateLimit != nil
	})
	h.cancelQ()
	receive(t, done, "teardown")

	next := sdk.CreateAssistantMessageEventStream()
	h.b.claimCurrentPiStream(next, "next query", h.q, nil)
	h.b.reportHeldRateLimit(h.q)
	if log := readDebugLog(t, h.agent); strings.Contains(log, "rate limit: headers") {
		t.Fatalf("the next query heard from the aborted one:\n%s", log)
	}
}

// --- the pieces around the loop ---

func TestResolveMcpTools(t *testing.T) {
	context := map[string]any{"tools": []any{
		map[string]any{"name": "Read", "description": "r"},
		map[string]any{"name": "bash", "description": "b"},
	}}
	tools, toSDK, toPi := resolveMcpTools(context, "bash")
	if len(tools) != 1 || tools[0]["name"] != "Read" {
		t.Fatalf("tools = %v", tools)
	}
	wantSDK := map[string]string{"Read": "mcp__custom-tools__Read", "read": "mcp__custom-tools__Read"}
	wantPi := map[string]string{"mcp__custom-tools__Read": "Read", "mcp__custom-tools__read": "Read"}
	if !reflect.DeepEqual(toSDK, wantSDK) || !reflect.DeepEqual(toPi, wantPi) {
		t.Fatalf("maps = %v / %v", toSDK, toPi)
	}
	if tools, _, _ := resolveMcpTools(map[string]any{}, ""); len(tools) != 0 {
		t.Fatalf("tools = %v", tools)
	}
}

func TestSteerBlocks(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	user := func(content any) map[string]any { return map[string]any{"role": "user", "content": content} }
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

	got, err := b.steerBlocks([]map[string]any{{"role": "assistant"}, user("one"), user("two")})
	if err != nil || !reflect.DeepEqual(got, []any{text("one\ntwo")}) {
		t.Fatalf("text steer = %v, %v", got, err)
	}
	withImage := user([]any{text("look"), map[string]any{"type": "image", "data": "iVBOR", "mimeType": "image/png"}})
	got, err = b.steerBlocks([]map[string]any{withImage})
	want := []any{text("look"), map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBOR"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("image steer = %v, %v", got, err)
	}
	if got, err := b.steerBlocks([]map[string]any{user("hi"), {"role": "assistant"}}); got != nil || err != nil {
		t.Fatalf("no trailing user turn = %v, %v", got, err)
	}
	if _, err := b.steerBlocks([]map[string]any{user(42.0)}); err == nil || !strings.Contains(err.Error(), "malformed message from another extension") {
		t.Fatalf("malformed content = %v", err)
	}
}

// bind and unbind run on different goroutines in production (the provider call and
// the teardown); the binder's stop function must be safe against that.
func TestRunBinderBindAndUnbindFromDifferentGoroutines(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	q := newQueryContext()
	owner := context.Background()
	for range 300 {
		bind, unbind := b.newRunBinder(q, owner, func() {})
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); bind(ctx, sdk.CreateAssistantMessageEventStream()) }()
		go func() { defer wg.Done(); unbind() }()
		wg.Wait()
		cancel()
	}
}

// A bind that loses the race to the teardown's unbind must not leave an abort
// registered: the query is gone, and its run's cancellation must not reach it.
func TestRunBinderIgnoresABindAfterUnbind(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	aborted := make(chan struct{}, 1)
	bind, unbind := b.newRunBinder(newQueryContext(), context.Background(), func() { aborted <- struct{}{} })
	unbind()
	ctx, cancel := context.WithCancel(context.Background())
	bind(ctx, sdk.CreateAssistantMessageEventStream())
	cancel()
	select {
	case <-aborted:
		t.Fatal("a bind after unbind aborted the query")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPanicInTheResultFallbackStillEndsWithOneErrorEvent(t *testing.T) {
	h := newConsumeHarness(t)
	h.b.resultFallbackHook = func() { panic("fallback exploded") }
	h.client.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "x"})
	done := h.start()
	receive(t, done, "the consume goroutine (a held q.mu would deadlock failQuery)")
	terminal := terminalEvents(drain(h.stream))
	if len(terminal) != 1 || terminal[0]["type"] != "error" || !strings.Contains(str(obj(terminal[0]["error"])["errorMessage"]), "internal error") {
		t.Fatalf("terminal = %v", terminal)
	}
}

// A panic in a goroutine the bridge starts would kill the fused PiG process; it becomes a debug line.
func TestKillClientContainsPanics(t *testing.T) {
	b, _, agentDir := newTestBridge(t, true)
	client := newFakeClient()
	client.onInterrupt = func() { panic("interrupt exploded") }
	client.onClose = func() { panic("close exploded") }
	b.killClient(client)
	waitFor(t, "both panics to be logged", func() bool {
		data, _ := os.ReadFile(filepath.Join(agentDir, "claude-bridge.log"))
		log := string(data)
		return strings.Contains(log, "killClient interrupt: recovered from panic: interrupt exploded") &&
			strings.Contains(log, "killClient close: recovered from panic: close exploded")
	})
}
