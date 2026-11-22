package claudebridge

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/bridge"
)

// A model of another provider can call the tool PiG lists, but no Claude Code query
// made that call, so there is nothing to wait for.
func TestWebToolsAnswerOnlyOnClaudeBridgeModels(t *testing.T) {
	b := bridge.New(bridge.Deps{})
	for provider, want := range map[string]string{
		"openai":        "WebSearch is only available on claude-bridge models",
		"":              "WebSearch is only available on claude-bridge models",
		"claude-bridge": "WebSearch: Claude Code is no longer running this call",
	} {
		got := webToolResult(context.Background(), b, "WebSearch", provider, "call_1")
		if !reflect.DeepEqual(got, sdk.ToolResult{Content: want, IsError: true}) {
			t.Errorf("provider %q: result = %+v", provider, got)
		}
	}
}

func TestWebToolDefinitionsRequireNothing(t *testing.T) {
	b := bridge.New(bridge.Deps{})
	for name := range webToolSpecs {
		d := webToolDefinition(b, name)
		if d.Name != name || d.Label == "" || d.Execute == nil {
			t.Errorf("%s: definition = %+v", name, d)
		}
		// PiG validates arguments before Execute; a required field would let PiG answer
		// a call Claude Code already ran with a validation error of its own.
		if _, has := d.Parameters["required"]; has || d.Parameters["type"] != "object" {
			t.Errorf("%s: parameters = %v", name, d.Parameters)
		}
	}
}
