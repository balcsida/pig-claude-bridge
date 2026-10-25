package ccproto

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
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
	if r := responseFor(lines, "u4"); r != nil {
		t.Errorf("request_user_dialog answered: %v", r)
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
