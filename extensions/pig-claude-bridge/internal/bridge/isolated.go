package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/models"
)

// IsolatedStream serves a one-off query (pi's compaction, branch-summary and
// AskClaude calls) on its own persistSession:false Claude Code process, with no
// session sync, no MCP server and no touch of the bridge's session state. tools and
// allowedTools are Claude Code's built-in tool set for the process: nil tools means
// none. ctx is the call's cancellation. A failure is the stream's one error event.
func (b *Bridge) IsolatedStream(ctx context.Context, host Host, model, piContext, options map[string]any, tag string, tools, allowedTools []string) (*sdk.ModelEventStream, error) {
	stream := sdk.CreateAssistantMessageEventStream()
	b.queries.Add(1)
	go func() {
		defer b.queries.Done()
		stream.Push(b.runIsolatedSummary(ctx, host, model, piContext, options, tag, tools, allowedTools))
	}()
	return stream, nil
}

// extractIsolatedSummaryPrompt is the lone user message's text, for calls that are
// not marked one-off and so must match the [system, user] compaction shape exactly.
func extractIsolatedSummaryPrompt(messages []map[string]any) (string, error) {
	if len(messages) != 1 || str(messages[0]["role"]) != "user" {
		roles := make([]string, len(messages))
		for i, m := range messages {
			roles[i] = str(m["role"])
		}
		return "", fmt.Errorf("isolatedStreamFn: expected exactly 1 user message, got %d (%s)", len(messages), strings.Join(roles, ","))
	}
	prompt, _ := extractUserPrompt(messages)
	if prompt == "" {
		return "", errors.New("isolatedStreamFn: summarization prompt is empty")
	}
	return prompt, nil
}

// runIsolatedSummary runs the query and returns its one terminal event. A panic
// becomes an error event, never an escape.
func (b *Bridge) runIsolatedSummary(ctx context.Context, host Host, model, piContext, options map[string]any, tag string, tools, allowedTools []string) (event map[string]any) {
	fail := func(message string) map[string]any {
		out := newAssistantOutput(model)
		out["stopReason"], out["errorMessage"] = "error", message
		return map[string]any{"type": "error", "reason": "error", "error": out}
	}
	defer func() {
		if r := recover(); r != nil {
			b.debugf("isolated: recovered from panic: %v\n%s", r, debug.Stack())
			event = fail(fmt.Sprintf("claude bridge internal error: %v", r))
		}
	}()

	// PiG delivers the summarization prompt folded into a leading system message
	// (issue #106): restore the prompt and tools fields; the prompt still reaches
	// Claude Code as its system prompt.
	piContext = convert.ToBridgeContext(piContext)
	messages := messagesOf(piContext)

	// Any one-off summarizer call (cacheRetention "none") may appear in a future pi
	// release, so route on the marker; other calls must match the compaction shape.
	var promptText string
	if str(options["cacheRetention"]) == "none" {
		promptText, _ = extractUserPrompt(messages)
		if promptText == "" {
			return fail("runIsolatedSummary: one-off summary without a user prompt (last message is not user?)")
		}
	} else {
		var err error
		if promptText, err = extractIsolatedSummaryPrompt(messages); err != nil {
			return fail(err.Error())
		}
	}
	exe, err := b.resolveExecutable(host)
	if err != nil {
		return fail(err.Error())
	}
	// D7: the most recent pending summary stands for the TypeScript's per-signal lookup.
	owner, ok := b.summarySessionFor()
	if !ok {
		owner = str(options["sessionId"])
	}
	cwd := b.sessionCwdFor(owner)
	cliModel := models.ClaudeCodeModelID(str(model["id"]), b.longContext)
	effort := effortForReasoning(model, options["reasoning"])
	b.debugf("compact summary: spawn model=%s registeredModel=%v effort=%s promptLen=%d", cliModel, model["id"], orDefault(effort), len([]rune(promptText)))

	if tools == nil {
		tools = []string{}
	}
	debugFile, onStderr := b.cliDebugOptions(tag)
	opts := ccproto.Options{
		Executable: exe, Cwd: cwd, Model: cliModel, Effort: effort, MaxTurns: 1,
		Tools: tools, ToolsSet: true, AllowedTools: allowedTools,
		SettingSourcesSet: true, Skills: []string{},
		SystemPrompt: str(piContext["systemPrompt"]), SystemPromptSet: true,
		StrictMcpConfig: true, NoSessionPersistence: true,
		Settings: map[string]any{"autoMemoryEnabled": false}, DebugFile: debugFile,
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	client, err := b.deps.Spawn(runCtx, opts, ccproto.Handlers{OnStderr: onStderr})
	if err != nil {
		return fail(fmt.Sprintf("start Claude Code: %v", err))
	}
	var writer sync.WaitGroup
	writeErr := make(chan error, 1)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		if err := client.Close(closeCtx); err != nil {
			b.debugf("isolated: closing Claude Code: %v", err)
		}
		writer.Wait()
	}()
	writer.Add(1)
	go func() {
		defer writer.Done()
		if err := client.WriteUser(userMessage([]any{map[string]any{"type": "text", "text": promptText}}, "")); err != nil {
			writeErr <- fmt.Errorf("write user message: %w", err)
			return
		}
		if err := client.EndInput(); err != nil {
			b.debugf("isolated: closing Claude Code stdin: %v", err)
		}
	}()

	var assistantText, finalText, errorText string
	frames := client.Frames()
loop:
	for {
		var raw []byte
		select {
		case <-ctx.Done():
			killClient(client)
			out := newAssistantOutput(model)
			out["stopReason"], out["errorMessage"] = "aborted", "Operation aborted"
			b.debugf("compact summary: aborted")
			return map[string]any{"type": "error", "reason": "aborted", "error": out}
		case err := <-writeErr:
			return fail(err.Error())
		case r, open := <-frames:
			if !open {
				if err := b.awaitExit(ctx, client); err != nil {
					return fail(err.Error())
				}
				break loop
			}
			raw = r
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			b.debugf("isolated: skipping malformed frame: %v", err)
			continue
		}
		switch str(frame["type"]) {
		case "assistant":
			content, _ := obj(frame["message"])["content"].([]any)
			for _, block := range content {
				if bl := obj(block); str(bl["type"]) == "text" {
					assistantText += str(bl["text"])
				}
			}
		case "result":
			b.logServedContextWindow("compact summary", frame, model)
			if failure, failed := resultErrorText(frame); failed {
				errorText = failure
			} else if frame["subtype"] == "success" {
				if finalText = str(frame["result"]); finalText == "" {
					finalText = assistantText
				}
			}
			break loop
		}
	}

	text := finalText
	if text == "" {
		text = assistantText
	}
	if errorText != "" || strings.TrimSpace(text) == "" {
		if errorText == "" {
			errorText = "Claude Code summary returned empty text"
		}
		b.debugf("compact summary: error %s", errorText)
		return fail(errorText)
	}
	b.debugf("compact summary: done textLen=%d", len([]rune(text)))
	out := newAssistantOutput(model)
	out["content"] = []any{map[string]any{"type": "text", "text": text}}
	return map[string]any{"type": "done", "reason": "stop", "message": out}
}
