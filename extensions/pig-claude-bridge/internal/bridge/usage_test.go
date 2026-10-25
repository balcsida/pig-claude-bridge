package bridge

import (
	"reflect"
	"testing"
)

var pricedModel = map[string]any{
	"api": "anthropic-messages", "provider": "anthropic", "id": "claude-haiku-4-5",
	"cost": map[string]any{"input": 1_000_000, "output": 2_000_000, "cacheRead": 0, "cacheWrite": 0},
}

func emptyOutput() map[string]any { return newAssistantOutput(testModel) }

// update decodes a usage frame the way the stream does, then applies it.
func update(t *testing.T, output map[string]any, usage string, model map[string]any) UsageReport {
	t.Helper()
	return updateUsage(output, parseSdkUsage(frame(t, usage)), model)
}

func usageOf(output map[string]any) map[string]any { return output["usage"].(map[string]any) }

func TestUpdateUsage(t *testing.T) {
	t.Run("reads the nested thinking count onto usage.reasoning", func(t *testing.T) {
		o := emptyOutput()
		update(t, o, `{"output_tokens":100,"output_tokens_details":{"thinking_tokens":40}}`, testModel)
		if got := usageOf(o)["reasoning"]; got != 40 {
			t.Fatalf("reasoning = %v", got)
		}
	})
	t.Run("ignores a flat thinking_tokens, which no SDK version emits", func(t *testing.T) {
		o := emptyOutput()
		update(t, o, `{"output_tokens":100,"thinking_tokens":40}`, testModel)
		if _, ok := usageOf(o)["reasoning"]; ok {
			t.Fatal("reasoning set from a flat thinking_tokens")
		}
	})
	t.Run("ignores reasoning_tokens, a field that exists in no SDK version", func(t *testing.T) {
		o := emptyOutput()
		update(t, o, `{"output_tokens":100,"reasoning_tokens":40}`, testModel)
		if _, ok := usageOf(o)["reasoning"]; ok {
			t.Fatal("reasoning set from reasoning_tokens")
		}
	})
	t.Run("leaves reasoning untouched when the frame carries no details", func(t *testing.T) {
		o := emptyOutput()
		update(t, o, `{"input_tokens":10,"output_tokens":20}`, testModel)
		if _, ok := usageOf(o)["reasoning"]; ok {
			t.Fatal("reasoning set without details")
		}
	})
	t.Run("maps the four flat counters", func(t *testing.T) {
		o := emptyOutput()
		update(t, o, `{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}`, testModel)
		want := map[string]any{"input": 10, "output": 20, "cacheRead": 30, "cacheWrite": 40, "totalTokens": 100}
		for k, v := range want {
			if usageOf(o)[k] != v {
				t.Errorf("%s = %v, want %v", k, usageOf(o)[k], v)
			}
		}
	})
	t.Run("keeps reasoning out of the token total", func(t *testing.T) {
		o := emptyOutput()
		update(t, o, `{"output_tokens":100,"output_tokens_details":{"thinking_tokens":40}}`, testModel)
		if got := usageOf(o)["totalTokens"]; got != 100 {
			t.Fatalf("totalTokens = %v", got)
		}
	})
	t.Run("reports the cache share of the prompt", func(t *testing.T) {
		if got := update(t, emptyOutput(), `{"input_tokens":25,"cache_read_input_tokens":75}`, testModel).CachePct; got != 75 {
			t.Fatalf("cachePct = %d", got)
		}
	})
	t.Run("reports zero cache share when there is no prompt", func(t *testing.T) {
		if got := update(t, emptyOutput(), `{"output_tokens":10}`, testModel).CachePct; got != 0 {
			t.Fatalf("cachePct = %d", got)
		}
	})
	t.Run("only overwrites counters the frame actually carries", func(t *testing.T) {
		o := emptyOutput()
		usageOf(o)["input"] = 5
		update(t, o, `{"output_tokens":7}`, testModel)
		if got := usageOf(o)["input"]; got != 5 {
			t.Fatalf("input = %v", got)
		}
	})
	t.Run("bills the counters and never the reasoning tokens", func(t *testing.T) {
		o := emptyOutput()
		update(t, o, `{"input_tokens":10,"output_tokens":100,"output_tokens_details":{"thinking_tokens":40}}`, pricedModel)
		// 10 input at 1 plus 100 output at 2; folding reasoning into output would bill 290.
		cost := usageOf(o)["cost"].(map[string]any)
		if cost["total"] != 210.0 || cost["input"] != 10.0 || cost["output"] != 200.0 {
			t.Fatalf("cost = %v", cost)
		}
	})
	t.Run("reports the thinking count carried by this frame", func(t *testing.T) {
		r := update(t, emptyOutput(), `{"output_tokens":100,"output_tokens_details":{"thinking_tokens":40}}`, testModel)
		if r.Reasoning == nil || *r.Reasoning != 40 {
			t.Fatalf("reasoning = %v", r.Reasoning)
		}
	})
	t.Run("omits reasoning for a frame that carries none, even after one that did", func(t *testing.T) {
		o := emptyOutput()
		update(t, o, `{"output_tokens":100,"output_tokens_details":{"thinking_tokens":40}}`, testModel)
		r := update(t, o, `{"output_tokens":120}`, testModel)
		// output.usage.reasoning keeps the earlier 40; the report must not.
		if r.Reasoning != nil || usageOf(o)["reasoning"] != 40 {
			t.Fatalf("report = %v, usage reasoning = %v", r.Reasoning, usageOf(o)["reasoning"])
		}
	})
}

func TestMapStopReason(t *testing.T) {
	for in, want := range map[string]string{"tool_use": "toolUse", "max_tokens": "length", "end_turn": "stop", "": "stop", "other": "stop"} {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePartialJSON(t *testing.T) {
	if got := parsePartialJSON(`{"a":1}`); !reflect.DeepEqual(got, map[string]any{"a": 1.0}) {
		t.Errorf("complete = %v", got)
	}
	for _, in := range []string{"", `{"a"`, `5`, `[1]`} {
		if got := parsePartialJSON(in); got != nil {
			t.Errorf("parsePartialJSON(%q) = %v, want nil", in, got)
		}
	}
}

func TestMapToolArgs(t *testing.T) {
	if got := mapToolArgs("read", map[string]any{"file_path": "a", "limit": 1.0}); !reflect.DeepEqual(got, map[string]any{"path": "a", "limit": 1.0}) {
		t.Errorf("read = %v", got)
	}
	if got := mapToolArgs("edit", map[string]any{"old_string": "x", "oldText": "y"}); !reflect.DeepEqual(got, map[string]any{"oldText": "y"}) {
		t.Errorf("edit alias collision = %v", got)
	}
	if got := mapToolArgs("Bash", map[string]any{"command": "ls"}); got["timeout"] != 120 {
		t.Errorf("bash default timeout = %v", got)
	}
	if got := mapToolArgs("bash", map[string]any{"timeout": 5.0}); got["timeout"] != 5.0 {
		t.Errorf("bash explicit timeout = %v", got)
	}
	if got := mapToolArgs("notify_parent", nil); len(got) != 0 {
		t.Errorf("nil args = %v", got)
	}
}
