package ccproto

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sameJSON compares two JSON documents structurally.
func sameJSON(t *testing.T, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		b, _ := json.Marshal(got)
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

func echoHandler(reply string, has bool) Handlers {
	return Handlers{OnMcpMessage: func(context.Context, string, json.RawMessage) (json.RawMessage, bool, error) {
		return json.RawMessage(reply), has, nil
	}}
}

func TestMcpMessageRoundTripWrapsResponse(t *testing.T) {
	reply := `{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`
	_, out := startFake(t, map[string]string{
		"FAKECLAUDE_MCP_CALL": `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`,
	}, echoHandler(reply, true))
	lines := waitStdin(t, out, func(l []map[string]any) bool { return responseFor(l, "mcp1") != nil })
	sameJSON(t, responseFor(lines, "mcp1"), `{"request_id":"mcp1","response":{"mcp_response":`+reply+`},"subtype":"success"}`)
}

func TestMcpNotificationGetsFixedReply(t *testing.T) {
	_, out := startFake(t, map[string]string{
		"FAKECLAUDE_MCP_CALL": `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	}, echoHandler("", false))
	lines := waitStdin(t, out, func(l []map[string]any) bool { return responseFor(l, "mcp1") != nil })
	sameJSON(t, responseFor(lines, "mcp1"), `{"request_id":"mcp1","response":{"mcp_response":{"jsonrpc":"2.0","result":{},"id":0}},"subtype":"success"}`)
}

func TestUnknownControlSubtypeErrorText(t *testing.T) {
	_, out := startFake(t, map[string]string{"FAKECLAUDE_SCRIPT": "unknown_subtype.jsonl"}, Handlers{})
	lines := waitStdin(t, out, func(l []map[string]any) bool { return responseFor(l, "u5") != nil })
	for id, want := range map[string]string{
		"u1": "Unsupported control request subtype: bogus",
		"u2": "canUseTool callback is not provided.",
		"u3": "No hook callback found for ID: hook_9",
	} {
		r := responseFor(lines, id)
		if r == nil || r["subtype"] != "error" || r["error"] != want {
			t.Errorf("%s: got %v, want error %q", id, r, want)
		}
	}
	// Left unanswered, as the SDK leaves them.
	for _, id := range []string{"u4", "r1", "r2", "r3", "r4"} {
		if r := responseFor(lines, id); r != nil {
			t.Errorf("%s answered: %v", id, r)
		}
	}
}

func TestControlCancelRequestCancelsTheHandler(t *testing.T) {
	cancelled := make(chan struct{})
	startFake(t, map[string]string{"FAKECLAUDE_SCRIPT": "cancel.jsonl"}, Handlers{
		OnMcpMessage: func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, bool, error) {
			<-ctx.Done()
			close(cancelled)
			return nil, false, ctx.Err()
		},
	})
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("control_cancel_request did not cancel the handler's context")
	}
}

func TestDuplicateInflightRequestIDIsIgnored(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	p, out := startFake(t, map[string]string{"FAKECLAUDE_SCRIPT": "duplicate_id.jsonl"}, Handlers{
		OnMcpMessage: func(context.Context, string, json.RawMessage) (json.RawMessage, bool, error) {
			calls.Add(1)
			<-release
			return json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`), true, nil
		},
	})
	<-p.Frames() // the frame after both d1 requests: frames are dispatched in order, so both were seen
	close(release)
	lines := waitStdin(t, out, func(l []map[string]any) bool { return responseFor(l, "d1") != nil })
	n := 0
	for _, l := range lines {
		if r, _ := l["response"].(map[string]any); l["type"] == "control_response" && r["request_id"] == "d1" {
			n++
		}
	}
	if calls.Load() != 1 || n != 1 {
		t.Fatalf("handler ran %d times, %d responses; want 1 and 1", calls.Load(), n)
	}
}

func TestInboundHandlersRunConcurrently(t *testing.T) {
	entered := make(chan struct{}, 2)
	both := make(chan struct{})
	_, out := startFake(t, map[string]string{"FAKECLAUDE_SCRIPT": "two_mcp_calls.jsonl"}, Handlers{
		OnMcpMessage: func(context.Context, string, json.RawMessage) (json.RawMessage, bool, error) {
			entered <- struct{}{}
			select {
			case <-both:
			case <-time.After(5 * time.Second):
				return nil, false, fmt.Errorf("the other handler never ran alongside this one")
			}
			return json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`), true, nil
		},
	})
	<-entered
	<-entered
	close(both)
	lines := waitStdin(t, out, func(l []map[string]any) bool { return responseFor(l, "p1") != nil && responseFor(l, "p2") != nil })
	for _, id := range []string{"p1", "p2"} {
		if r := responseFor(lines, id); r["subtype"] != "success" {
			t.Errorf("%s = %v", id, r)
		}
	}
}

func TestElicitationDeclined(t *testing.T) {
	_, out := startFake(t, map[string]string{"FAKECLAUDE_SCRIPT": "elicitation.jsonl"}, Handlers{})
	lines := waitStdin(t, out, func(l []map[string]any) bool { return responseFor(l, "e1") != nil })
	b, _ := json.Marshal(responseFor(lines, "e1"))
	if string(b) != `{"request_id":"e1","response":{"action":"decline"},"subtype":"success"}` {
		t.Fatalf("got %s", b)
	}
}

func TestInterruptReceipt(t *testing.T) {
	p, out := startFake(t, nil, Handlers{})
	got, err := p.Interrupt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"still_queued":[]}` {
		t.Fatalf("receipt = %s", got)
	}
	lines := waitStdin(t, out, func(l []map[string]any) bool { return len(l) >= 2 })
	req, _ := lines[1]["request"].(map[string]any)
	if req["subtype"] != "interrupt" || req["cancel_queued"] != true {
		t.Fatalf("interrupt frame = %v", lines[1])
	}
}

// A panicking handler must not take the fused PiG process down: Claude Code gets an
// error response and the transport keeps serving.
func TestPanickingHandlerGetsAnErrorResponseAndServingContinues(t *testing.T) {
	reply := `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`
	_, out := startFake(t, map[string]string{"FAKECLAUDE_SCRIPT": "two_mcp_calls.jsonl"}, Handlers{
		OnMcpMessage: func(_ context.Context, _ string, msg json.RawMessage) (json.RawMessage, bool, error) {
			if strings.Contains(string(msg), `"id":1`) {
				panic("handler exploded")
			}
			return json.RawMessage(reply), true, nil
		},
	})
	lines := waitStdin(t, out, func(l []map[string]any) bool { return responseFor(l, "p1") != nil && responseFor(l, "p2") != nil })
	if r := responseFor(lines, "p1"); r["subtype"] != "error" || !strings.Contains(fmt.Sprint(r["error"]), "handler exploded") {
		t.Fatalf("p1 = %v", r)
	}
	sameJSON(t, responseFor(lines, "p2"), `{"request_id":"p2","response":{"mcp_response":`+reply+`},"subtype":"success"}`)
}
