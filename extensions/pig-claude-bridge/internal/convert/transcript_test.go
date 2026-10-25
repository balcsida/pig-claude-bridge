package convert

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func decodeMessages(t *testing.T, src string) []map[string]any {
	t.Helper()
	var msgs []map[string]any
	if err := json.Unmarshal([]byte(src), &msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

func asAny(msgs []map[string]any) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}

// bridgeContext is how PiG delivers a context: only messages, the tool set
// riding inside the system messages.
func bridgeContext(t *testing.T, src string) map[string]any {
	return ToBridgeContext(map[string]any{"messages": asAny(decodeMessages(t, src))})
}

func replayedSystemPrompt(t *testing.T, src string) any {
	return bridgeContext(t, src)["systemPrompt"]
}

const headWithRead = `{"role":"system","content":"","timestamp":0,"toolsAdded":[{"name":"read","description":"","parameters":{}}]}`
const goMessage = `{"role":"user","content":[{"type":"text","text":"go"}],"timestamp":0}`

func TestToBridgeContextSectionReplayOrder(t *testing.T) {
	join := func(parts ...string) string { return strings.Join(parts, "\n\n") }

	t.Run("re-emits sections in canonical order after a delete-then-re-add reorders the Map", func(t *testing.T) {
		got := replayedSystemPrompt(t, `[`+headWithRead+`,
			{"role":"system","content":"","timestamp":0,"sections":{"preamble":"P","tools":"T","rules":"R","docs":"D","cwd":"/w"}},
			{"role":"system","content":"","timestamp":0,"sections":{"skills":null}},
			{"role":"system","content":"","timestamp":0,"sections":{"skills":"S","cwd":"/w"}},`+goMessage+`]`)
		if want := join("P", "T", "R", "D", "S", "/w"); got != want {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("ranks a section pi does not know about after cwd", func(t *testing.T) {
		got := replayedSystemPrompt(t, `[`+headWithRead+`,
			{"role":"system","content":"","timestamp":0,"sections":{"preamble":"P","tools":"T","rules":"R","docs":"D","environment":"E","cwd":"/w"}},`+goMessage+`]`)
		if want := join("P", "T", "R", "D", "/w", "E"); got != want {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("puts a late-introduced custom section after cwd, matching pi's builder order", func(t *testing.T) {
		got := replayedSystemPrompt(t, `[`+headWithRead+`,
			{"role":"system","content":"","timestamp":0,"sections":{"preamble":"P","tools":"T","rules":"R","docs":"D","project_context":"C","skills":"S","cwd":"/w"}},
			{"role":"system","content":"","timestamp":0,"sections":{"addendum":"A","mcp_servers":"M"}},`+goMessage+`]`)
		if want := join("P", "T", "R", "D", "A", "C", "S", "/w", "M"); got != want {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("keeps a custom section appended after the built-ins", func(t *testing.T) {
		got := replayedSystemPrompt(t, `[`+headWithRead+`,
			{"role":"system","content":"","timestamp":0,"sections":{"preamble":"P","tools":"T","cwd":"/w","myextension":"X"}},`+goMessage+`]`)
		if want := join("P", "T", "/w", "X"); got != want {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("renders the canonical head alone unchanged (fresh-session shape)", func(t *testing.T) {
		got := replayedSystemPrompt(t, `[
			{"role":"system","content":"","timestamp":0,"sections":{"preamble":"P","tools":"T","rules":"R","docs":"D","skills":"S","cwd":"/w"}},`+goMessage+`]`)
		if want := join("P", "T", "R", "D", "S", "/w"); got != want {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("joins text-array content like string content", func(t *testing.T) {
		got := replayedSystemPrompt(t, `[
			{"role":"system","content":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"timestamp":0},
			{"role":"system","content":"","timestamp":0,"sections":{"cwd":"/w"}},`+goMessage+`]`)
		if want := join("A\nB", "/w"); got != want {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("reconstructs the tool set through removals and re-adds", func(t *testing.T) {
		const grep = `{"name":"grep","description":"","parameters":{}}`
		const read = `{"name":"read","description":"","parameters":{}}`
		context := bridgeContext(t, `[
			{"role":"system","content":"","timestamp":0,"toolsAdded":[`+read+`,`+grep+`]},
			{"role":"system","content":"","timestamp":0,"toolsRemoved":[{"name":"read"}]},
			{"role":"system","content":"","timestamp":0,"toolsAdded":[`+read+`]},`+goMessage+`]`)
		var want []any
		if err := json.Unmarshal([]byte(`[`+grep+`,`+read+`]`), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(context["tools"], want) {
			t.Fatalf("tools = %#v", context["tools"])
		}
	})

	t.Run("returns a context without system messages unchanged (systemless call shape)", func(t *testing.T) {
		context := map[string]any{"messages": asAny(decodeMessages(t, `[`+goMessage+`]`)), "systemPrompt": "P", "tools": []any{}}
		got := ToBridgeContext(context)
		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(context).Pointer() {
			t.Fatal("want the same map — a true no-op")
		}
	})

	t.Run("hands the host's transcript shape to downstream consumers", func(t *testing.T) {
		context := bridgeContext(t, `[`+headWithRead+`,`+goMessage+`]`)
		msgs, ok := context["messages"].([]map[string]any)
		if !ok || len(msgs) != 1 || msgs[0]["role"] != "user" {
			t.Fatalf("messages = %#v", context["messages"])
		}
		if tools, _ := context["tools"].([]any); len(tools) != 1 {
			t.Fatalf("tools = %#v", context["tools"])
		}
		if _, has := context["systemPrompt"]; has {
			t.Fatalf("empty prompt must be omitted, got %#v", context["systemPrompt"])
		}
	})
}
