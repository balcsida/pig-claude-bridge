package bridge

import "testing"

// Port of pi-claude-bridge 0b096a8 (#152): with no reasoning level, thinking is disabled on reasoning
// models unless the model's thinkingLevelMap marks "off" unsupported (null).
func TestThinkingDisabledForReasoning(t *testing.T) {
	alwaysThinks := map[string]any{"id": "claude-fable-5-1", "reasoning": true, "thinkingLevelMap": map[string]any{"max": "max", "off": nil}}
	optional := map[string]any{"id": "claude-opus-4-7", "reasoning": true, "thinkingLevelMap": map[string]any{"max": "max"}}
	noMap := map[string]any{"id": "claude-opus-4-5", "reasoning": true}
	nonReasoning := map[string]any{"id": "claude-3-5-haiku", "reasoning": false}
	cases := []struct {
		name      string
		model     map[string]any
		reasoning any
		want      bool
	}{
		{"off on a model with optional thinking", optional, nil, true},
		{"off on a model without a thinkingLevelMap", noMap, "", true},
		{"a reasoning level keeps thinking on", optional, "high", false},
		{"a model that always thinks is left alone", alwaysThinks, nil, false},
		{"a non-reasoning model is left alone", nonReasoning, nil, false},
	}
	for _, c := range cases {
		if got := thinkingDisabledForReasoning(c.model, c.reasoning); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
