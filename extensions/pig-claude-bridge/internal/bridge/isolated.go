package bridge

import (
	"context"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// IsolatedStream serves a one-off query (pi's compaction, branch-summary and
// bug-report calls) on an isolated persistSession:false Claude Code process, with
// no session sync. It is a placeholder until Task 21 ports it: the stream carries
// one error event.
func (b *Bridge) IsolatedStream(ctx context.Context, host Host, model, piContext, options map[string]any, tag string, tools, allowedTools []string) *sdk.ModelEventStream {
	return errorStream(model, "isolated queries are implemented in Task 21")
}
