package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/mcptools"
)

// fakeClient is a ccproto.Client driven by a script: emit queues frames, exit
// simulates the process exiting, and callTool sends a tools/call through an MCP
// server as Claude Code would. One goroutine (run) owns Frames, so frames arrive in
// order and the channel closes exactly once, when the script exits or the client is
// closed.
type fakeClient struct {
	frames chan json.RawMessage
	done   chan struct{}
	script chan scriptItem
	stop   chan struct{}
	stopMu sync.Once

	attempts, sent atomic.Int32

	mu         sync.Mutex
	written    []ccproto.UserMessage
	waitErr    error
	tail       string
	interrupts int
	closes     int
	endInputs  int
	// onWrite, when set, runs for every WriteUser and decides its result.
	onWrite func(ccproto.UserMessage) error
	// onInterrupt and onClose, when set, run first in Interrupt and Close.
	onInterrupt, onClose func()
}

type scriptItem struct {
	frame json.RawMessage
	exit  *scriptExit
}

type scriptExit struct {
	code   int
	stderr string
}

func newFakeClient() *fakeClient {
	f := &fakeClient{
		frames: make(chan json.RawMessage),
		done:   make(chan struct{}),
		script: make(chan scriptItem, 256),
		stop:   make(chan struct{}),
	}
	go f.run()
	return f
}

func (f *fakeClient) run() {
	defer func() { close(f.frames); close(f.done) }()
	for {
		select {
		case <-f.stop:
			return
		case it := <-f.script:
			if it.exit != nil {
				f.mu.Lock()
				f.tail = it.exit.stderr
				if it.exit.code != 0 {
					f.waitErr = fmt.Errorf("Claude Code process exited with code %d. stderr: %s", it.exit.code, it.exit.stderr)
				}
				f.mu.Unlock()
				return
			}
			f.attempts.Add(1)
			select {
			case f.frames <- it.frame:
				f.sent.Add(1)
			case <-f.stop:
				return
			}
		}
	}
}

// emit queues frames (maps or JSON strings) for delivery in order.
func (f *fakeClient) emit(frames ...any) {
	for _, fr := range frames {
		var raw []byte
		switch v := fr.(type) {
		case string:
			raw = []byte(v)
		default:
			raw, _ = json.Marshal(v)
		}
		f.script <- scriptItem{frame: raw}
	}
}

// exit queues the process exit after the frames emitted so far.
func (f *fakeClient) exit(code int, stderr string) {
	f.script <- scriptItem{exit: &scriptExit{code, stderr}}
}

func (f *fakeClient) Frames() <-chan json.RawMessage { return f.frames }
func (f *fakeClient) Done() <-chan struct{}          { return f.done }

func (f *fakeClient) WriteUser(msg ccproto.UserMessage) error {
	f.mu.Lock()
	hook := f.onWrite
	f.written = append(f.written, msg)
	f.mu.Unlock()
	if hook != nil {
		return hook(msg)
	}
	return nil
}

func (f *fakeClient) EndInput() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endInputs++
	return nil
}

func (f *fakeClient) Interrupt(context.Context) (json.RawMessage, error) {
	f.mu.Lock()
	f.interrupts++
	hook := f.onInterrupt
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil, nil
}

func (f *fakeClient) Close(context.Context) error {
	f.mu.Lock()
	f.closes++
	hook := f.onClose
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.stopMu.Do(func() { close(f.stop) })
	<-f.done
	return nil
}

func (f *fakeClient) Wait() error {
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitErr
}

func (f *fakeClient) StderrTail() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tail
}

func (f *fakeClient) counts() (interrupts, closes, endInputs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interrupts, f.closes, f.endInputs
}

func (f *fakeClient) writes() []ccproto.UserMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ccproto.UserMessage(nil), f.written...)
}

// toolReply is the JSON-RPC answer to a tools/call.
type toolReply struct {
	Text    string
	IsError bool
	Err     string
}

// callTool sends a tools/call for name with Claude Code's tool use id through server,
// and returns the channel the reply arrives on.
func callTool(ctx context.Context, server *mcptools.Server, name, toolUseID string) <-chan toolReply {
	out := make(chan toolReply, 1)
	go func() {
		msg, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": map[string]any{}, "_meta": map[string]any{"claudecode/toolUseId": toolUseID}},
		})
		raw, has, err := server.Handle(ctx, msg)
		if err != nil || !has {
			out <- toolReply{Err: fmt.Sprintf("no reply (err=%v)", err)}
			return
		}
		var reply struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
			Error struct{ Message string }
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			out <- toolReply{Err: err.Error()}
			return
		}
		r := toolReply{IsError: reply.Result.IsError, Err: reply.Error.Message}
		if len(reply.Result.Content) > 0 {
			r.Text = reply.Result.Content[0].Text
		}
		out <- r
	}()
	return out
}

// waitFor polls cond until it holds, failing the test after two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}
