package bridge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

var fakeModel = map[string]any{"api": "anthropic", "provider": "anthropic", "id": "test-model"}

func TestQueryContextClass(t *testing.T) {
	t.Run("turnBlocks throws before resetTurnState", func(t *testing.T) {
		c := newQueryContext()
		_, err := c.turnBlocks()
		if err == nil || !strings.Contains(err.Error(), "turnBlocks accessed before resetTurnState") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("turnBlocks reflects turnOutput.content after resetTurnState", func(t *testing.T) {
		c := newQueryContext()
		c.resetTurnState(fakeModel)
		blocks, err := c.turnBlocks()
		if err != nil || len(blocks) != 0 {
			t.Fatalf("blocks = %v, err = %v", blocks, err)
		}
		c.mu.Lock()
		c.addBlockLocked(map[string]any{"type": "text", "text": "hello"})
		c.mu.Unlock()
		content := c.turnOutput["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["text"] != "hello" {
			t.Fatalf("content = %v", content)
		}
		again, _ := c.turnBlocks()
		if len(again) != 1 || &again[0] != &content[0] {
			t.Fatalf("turnBlocks must be turnOutput.content itself, not a copy: %v vs %v", again, content)
		}
	})
	t.Run("resetTurnState preserves turnToolCallIds", func(t *testing.T) {
		c := newQueryContext()
		c.turnToolCallIDs = []string{"id1", "id2"}
		c.resetTurnState(fakeModel)
		if !reflect.DeepEqual(c.turnToolCallIDs, []string{"id1", "id2"}) {
			t.Fatalf("turnToolCallIDs = %v", c.turnToolCallIDs)
		}
	})
	t.Run("resetTurnState clears the per-turn flags", func(t *testing.T) {
		c := newQueryContext()
		c.turnStarted, c.turnSawToolCall, c.turnStreamOpen = true, true, true
		c.turnStreamMessageID, c.turnStreamBlockStart = "msg_1", 3
		c.resetTurnState(fakeModel)
		if c.turnStarted || c.turnSawToolCall || c.turnStreamOpen || c.turnStreamMessageID != "" || c.turnStreamBlockStart != 0 {
			t.Fatalf("turn state not reset: %+v", c)
		}
	})
	t.Run("releasePendingToolCalls resolves every handler and clears the maps", func(t *testing.T) {
		c := newQueryContext()
		var got []convert.McpResult
		c.pendingToolCalls["a"] = PendingToolCall{ToolName: "t", Resolve: func(r convert.McpResult) { got = append(got, r) }}
		c.pendingResults["b"] = convert.McpResult{}
		c.releasePendingToolCalls("gone")
		if len(got) != 1 || got[0].Content[0]["text"] != "gone" {
			t.Fatalf("got = %v", got)
		}
		if len(c.pendingToolCalls) != 0 || len(c.pendingResults) != 0 {
			t.Fatal("maps not cleared")
		}
	})
	t.Run("newAssistantOutput carries text, stop reason and error message, usage zeroed", func(t *testing.T) {
		before := time.Now().UnixMilli()
		m := newAssistantOutput(fakeModel, "", "stop", "")
		if m["role"] != "assistant" || m["api"] != "anthropic" || m["provider"] != "anthropic" || m["model"] != "test-model" || m["stopReason"] != "stop" {
			t.Fatalf("m = %v", m)
		}
		if c, ok := m["content"].([]any); !ok || len(c) != 0 {
			t.Fatalf("content = %#v", m["content"])
		}
		if _, ok := m["errorMessage"]; ok {
			t.Fatal("an empty error message is set")
		}
		failed := newAssistantOutput(fakeModel, "partial", "error", "boom")
		if !reflect.DeepEqual(failed["content"], []any{map[string]any{"type": "text", "text": "partial"}}) || failed["stopReason"] != "error" || failed["errorMessage"] != "boom" {
			t.Fatalf("failed = %v", failed)
		}
		usage := m["usage"].(map[string]any)
		cost := usage["cost"].(map[string]any)
		for _, k := range []string{"input", "output", "cacheRead", "cacheWrite", "totalTokens"} {
			if usage[k] != 0 {
				t.Fatalf("usage[%s] = %v", k, usage[k])
			}
		}
		for _, k := range []string{"input", "output", "cacheRead", "cacheWrite", "total"} {
			if cost[k] != 0 {
				t.Fatalf("cost[%s] = %v", k, cost[k])
			}
		}
		if ts := m["timestamp"].(int64); ts < before {
			t.Fatalf("timestamp = %d", ts)
		}
	})
}

func text(m ccproto.UserMessage) string { return m.Content.([]map[string]any)[0]["text"].(string) }

func textMsg(s string) ccproto.UserMessage {
	return userMessage([]map[string]any{{"type": "text", "text": s}}, "")
}

// pump drives the stream like the SDK: write each message, then pull the next.
func pump(ctx context.Context, ps *PromptStream, write func(ccproto.UserMessage)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			m, ok := ps.Next(ctx)
			if !ok {
				return
			}
			write(m)
		}
	}()
	return done
}

func pushAsync(ps *PromptStream, m ccproto.UserMessage) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- ps.Push(context.Background(), m) }()
	return ch
}

func waitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("push hung")
		return nil
	}
}

func waitDone(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("pump hung")
	}
}

func TestMakePromptStream(t *testing.T) {
	t.Run("resolves the ack only after the write completes", func(t *testing.T) {
		ps := newPromptStream()
		writing, release := make(chan string, 1), make(chan struct{})
		pumping := pump(context.Background(), ps, func(m ccproto.UserMessage) { writing <- text(m); <-release })

		ack := pushAsync(ps, textMsg("steer"))
		if got := <-writing; got != "steer" {
			t.Fatalf("written = %q", got)
		}
		select {
		case err := <-ack:
			t.Fatalf("ack resolved before the write completed: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		close(release)
		if err := waitErr(t, ack); err != nil {
			t.Fatal(err)
		}
		ps.End()
		waitDone(t, pumping)
	})
	t.Run("delivers messages in push order and ends after draining", func(t *testing.T) {
		ps := newPromptStream()
		var written []string
		pumping := pump(context.Background(), ps, func(m ccproto.UserMessage) { written = append(written, text(m)) })
		for _, s := range []string{"first", "second"} {
			if err := ps.Push(context.Background(), textMsg(s)); err != nil {
				t.Fatal(err)
			}
		}
		ps.End()
		waitDone(t, pumping)
		if !reflect.DeepEqual(written, []string{"first", "second"}) {
			t.Fatalf("written = %v", written)
		}
	})
	t.Run("carries priority through to the SDK message", func(t *testing.T) {
		m := userMessage([]map[string]any{{"type": "text", "text": "steer"}}, "next")
		if m.Priority != "next" || m.UUID != "" {
			t.Fatalf("m = %+v", m)
		}
		if b, err := m.MarshalJSON(); err != nil || !strings.Contains(string(b), `"parent_tool_use_id":null`) {
			t.Fatalf("json = %s, err = %v", b, err)
		}
	})
	t.Run("rejects a push after end instead of hanging", func(t *testing.T) {
		ps := newPromptStream()
		pumping := pump(context.Background(), ps, func(ccproto.UserMessage) {})
		ps.End()
		waitDone(t, pumping)
		err := waitErr(t, pushAsync(ps, textMsg("late")))
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("fail settles queued and in-flight acks", func(t *testing.T) {
		ps := newPromptStream()
		writing, stuck := make(chan struct{}, 1), make(chan struct{})
		defer close(stuck)
		pump(context.Background(), ps, func(ccproto.UserMessage) { writing <- struct{}{}; <-stuck })

		inflight := pushAsync(ps, textMsg("inflight"))
		<-writing
		queued := pushAsync(ps, textMsg("queued"))
		for { // wait until the queued push is parked
			ps.mu.Lock()
			n := len(ps.queue)
			ps.mu.Unlock()
			if n == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		ps.Fail(errors.New("query ended"))
		for name, ch := range map[string]<-chan error{"inflight": inflight, "queued": queued, "after": pushAsync(ps, textMsg("after"))} {
			if err := waitErr(t, ch); err == nil || !strings.Contains(err.Error(), "query ended") {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
	})
	t.Run("settles the ack when the consumer abandons the stream", func(t *testing.T) {
		ps := newPromptStream()
		ctx, cancel := context.WithCancel(context.Background())
		taken := make(chan struct{})
		go func() { // takes one message, then abandons
			ps.Next(ctx)
			close(taken)
			cancel()
			ps.Next(ctx)
		}()
		err := waitErr(t, pushAsync(ps, textMsg("abandoned")))
		<-taken
		if err == nil {
			t.Fatal("push acked although the consumer abandoned it")
		}
	})
	t.Run("a Next parked on an empty queue returns false when its ctx is cancelled", func(t *testing.T) {
		ps := newPromptStream()
		ctx, cancel := context.WithCancel(context.Background())
		parked := make(chan bool, 1)
		go func() { _, ok := ps.Next(ctx); parked <- ok }()
		time.Sleep(20 * time.Millisecond) // let Next park on the empty queue
		cancel()
		select {
		case ok := <-parked:
			if ok {
				t.Fatal("Next returned a message")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Next stayed parked after its ctx was cancelled")
		}
	})
	t.Run("End then a cancelled Next rejects what is still queued", func(t *testing.T) {
		ps := newPromptStream()
		queued := pushAsync(ps, textMsg("queued"))
		for {
			ps.mu.Lock()
			n := len(ps.queue)
			ps.mu.Unlock()
			if n == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		ps.End()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, ok := ps.Next(ctx); ok {
			t.Fatal("a cancelled Next handed out a message")
		}
		if err := waitErr(t, queued); err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rejects a push made after the consumer abandoned the stream", func(t *testing.T) {
		ps := newPromptStream()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, ok := ps.Next(ctx); ok {
			t.Fatal("Next returned a message on a cancelled ctx")
		}
		err := waitErr(t, pushAsync(ps, textMsg("later")))
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("keeps the first failure rather than the last", func(t *testing.T) {
		ps := newPromptStream()
		queued := pushAsync(ps, textMsg("queued"))
		for {
			ps.mu.Lock()
			n := len(ps.queue)
			ps.mu.Unlock()
			if n == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		ps.Fail(errors.New("CLI exited with code 1"))
		ps.Fail(errors.New("query ended"))
		for name, ch := range map[string]<-chan error{"queued": queued, "after": pushAsync(ps, textMsg("after"))} {
			if err := waitErr(t, ch); err == nil || !strings.Contains(err.Error(), "CLI exited with code 1") {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
		if _, ok := ps.Next(context.Background()); ok {
			t.Fatal("Next returned a message after Fail")
		}
	})
}
