package claudebridge

import (
	"context"
	"errors"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/bridge"
)

// webToolSpecs are the PiG tools standing for Claude Code's own web tools
// (provider.webTools). On a claude-bridge model Claude Code runs the call and the
// tool only returns its answer, so the schemas describe Claude Code's inputs for
// PiG's tool card and for another provider's model, which gets an error anyway.
// Nothing is required: PiG must reach Execute for every call Claude Code ran, and
// Claude Code validates the input itself.
var webToolSpecs = map[string]struct {
	label, description string
	properties         map[string]any
}{
	"WebSearch": {
		"Web Search", "Search the web with Claude Code's WebSearch tool and return the results.",
		map[string]any{
			"query":           map[string]any{"type": "string", "description": "The search query."},
			"allowed_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only include results from these domains."},
			"blocked_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Never include results from these domains."},
		},
	},
	"WebFetch": {
		"Web Fetch", "Fetch a URL with Claude Code's WebFetch tool and answer a prompt about its content.",
		map[string]any{
			"url":    map[string]any{"type": "string", "description": "The URL to fetch."},
			"prompt": map[string]any{"type": "string", "description": "What to extract from the page."},
		},
	},
}

const onlyOnClaudeBridge = " is only available on claude-bridge models"

// webToolDefinition is the PiG tool for Claude Code's web tool name.
func webToolDefinition(b *bridge.Bridge, name string) sdk.ToolDefinition {
	spec := webToolSpecs[name]
	return sdk.ToolDefinition{
		Name:        name,
		Label:       spec.label,
		Description: spec.description + " Claude Code runs it, so the tool" + onlyOnClaudeBridge + ".",
		Parameters:  sdk.Schema{"type": "object", "properties": spec.properties},
		Execute: func(ctx sdk.Context, _ map[string]any) (any, error) {
			return webToolResult(requestContext(ctx), b, name, ctx.ModelProvider(), ctx.ToolCallID()), nil
		},
	}
}

// webToolResult is what the PiG tool name returns for the call toolCallID: Claude
// Code's answer to it, or an error when no Claude Code query made the call.
func webToolResult(ctx context.Context, b *bridge.Bridge, name, provider, toolCallID string) sdk.ToolResult {
	result, err := b.AwaitWebTool(ctx, toolCallID)
	switch {
	case err == nil:
		return sdk.ToolResult{Content: result.Text, IsError: result.IsError}
	case errors.Is(err, bridge.ErrNoWebCall) && provider != "claude-bridge":
		return sdk.ToolResult{Content: name + onlyOnClaudeBridge, IsError: true}
	case errors.Is(err, bridge.ErrNoWebCall):
		return sdk.ToolResult{Content: name + ": Claude Code is no longer running this call", IsError: true}
	}
	return sdk.ToolResult{Content: name + ": " + err.Error(), IsError: true}
}
