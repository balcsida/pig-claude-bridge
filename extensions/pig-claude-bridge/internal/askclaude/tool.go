package askclaude

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/bridge"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

const (
	previewMaxChars = 1000
	previewMaxLines = 6

	deprecationNotice = "AskClaude is deprecated and may be removed in a future release. The Claude Code provider remains supported."
	circularError     = "Error: AskClaude cannot be used when the active provider is claude-bridge — you're already running through Claude Code."
	providerName      = "claude-bridge"
)

// Definition is the AskClaude tool. host and requestContext adapt one request's
// sdk.Context to the bridge's Host and to a cancellation context; they are called
// inside each handler and nothing derived from a context is stored. agentDir is where
// the deprecation notice is recorded.
func Definition(b *bridge.Bridge, cfg config.Config, agentDir string, host func(sdk.Context) bridge.Host, requestContext func(sdk.Context) context.Context) sdk.ToolDefinition {
	conf := cfg.AskClaude
	if conf == nil {
		conf = &config.AskClaudeConfig{}
	}
	d := ResolveDefaults(conf)
	name, label := "AskClaude", "Ask Claude Code"
	if conf.Name != nil {
		name = *conf.Name
	}
	if conf.Label != nil {
		label = *conf.Label
	}
	var noticeShown atomic.Bool
	noticeShown.Store(cfg.AskClaudeDeprecationShown != "")

	return sdk.ToolDefinition{
		Name:        name,
		Label:       label,
		Description: ToolDescription(d, conf.Description),
		Parameters:  BuildParams(d),
		RenderCall: func(_ sdk.Context, args map[string]any, _ sdk.ToolRenderContext, _ int) (lines []string, err error) {
			defer recoverRender(&lines, &err)
			return renderCall(args, d), nil
		},
		RenderResult: func(_ sdk.Context, result sdk.ToolRenderResult, opts sdk.ToolRenderResultOptions, _ sdk.ToolRenderContext, _ int) (lines []string, err error) {
			defer recoverRender(&lines, &err)
			return renderResult(result, opts), nil
		},
		Execute: func(ctx sdk.Context, params map[string]any) (result any, err error) {
			start := time.Now()
			prompt, _ := params["prompt"].(string)
			defer func() {
				if r := recover(); r != nil {
					result, err = errorResult(prompt, start, fmt.Sprintf("claude bridge internal error: %v", r)), nil
				}
			}()
			h := host(ctx)
			if ctx.Mode() == "tui" && noticeShown.CompareAndSwap(false, true) {
				h.Notify(deprecationNotice, "warning")
				if err := config.MarkAskClaudeDeprecationNoticeShown(agentDir, time.Now().Format("2006-01-02")); err != nil {
					h.Notify("AskClaude: cannot record the deprecation notice: "+err.Error(), "warning")
				}
			}
			// Guard: circular delegation.
			if ctx.ModelProvider() == providerName {
				return sdk.ToolResult{Content: circularError, Details: map[string]any{"error": true}}, nil
			}
			modeArg := Unset
			if v, present := params["mode"]; present {
				modeArg = v
			}
			mode, err := ResolveMode(modeArg, d)
			if err != nil {
				return nil, err
			}
			isolated := d.Isolated
			if v, ok := params["isolated"].(bool); ok {
				isolated = v
			}
			return run(ctx, h, requestContext(ctx), b, d, conf, params, mode, isolated, start), nil
		},
	}
}

func recoverRender(lines *[]string, err *error) {
	if r := recover(); r != nil {
		*lines, *err = nil, fmt.Errorf("AskClaude render: %v", r)
	}
}

func errorResult(prompt string, start time.Time, message string) sdk.ToolResult {
	return sdk.ToolResult{
		Content: "Error: " + message,
		Details: map[string]any{"prompt": prompt, "executionTime": time.Since(start).Milliseconds(), "error": true},
	}
}

// run is one delegated call: it reports progress every second and returns the answer
// with the actions Claude Code took, or an error result.
func run(ctx sdk.Context, h bridge.Host, goCtx context.Context, b *bridge.Bridge, d Defaults, conf *config.AskClaudeConfig,
	params map[string]any, mode Mode, isolated bool, start time.Time) sdk.ToolResult {
	prompt, _ := params["prompt"].(string)
	model, _ := params["model"].(string)
	thinking, _ := params["thinking"].(string)
	cwd := ctx.Cwd()
	var calls ToolCalls

	request := bridge.AskRequest{
		Prompt: prompt, DisallowedTools: DisallowedTools(mode), AppendSkills: &d.AppendSkills,
		Model: model, Thinking: thinking, Isolated: isolated,
		OnToolUse: func(id, name string, input any) {
			calls.Set(id, ToolCallState{Name: MapToolName(name), Status: "complete", RawInput: input})
		},
	}
	var err error
	if request.SystemPrompt, err = ctx.GetSystemPrompt(); err != nil {
		return errorResult(prompt, start, err.Error())
	}
	if !isolated {
		session, err := ctx.SessionManager().BuildSessionContext()
		if err != nil {
			return errorResult(prompt, start, err.Error())
		}
		request.Context = session.Messages
	}
	request.PiSessionID, _ = ctx.SessionManager().GetSessionID()

	stop, stopped := make(chan struct{}), sync.WaitGroup{}
	stopped.Add(1)
	go func() {
		defer stopped.Done()
		defer func() { _ = recover() }()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				status := fmt.Sprintf("%.0fs — working...", time.Since(start).Seconds())
				if summary := BuildActionSummary(cwd, calls.States()); summary != "" {
					status = fmt.Sprintf("%.0fs — %s", time.Since(start).Seconds(), summary)
				}
				_ = ctx.OnUpdate(sdk.ToolResult{Content: status, Details: map[string]any{"prompt": prompt, "executionTime": time.Since(start).Milliseconds()}})
			}
		}
	}()
	text, err := b.PromptAndWait(goCtx, h, request)
	close(stop)
	stopped.Wait()
	if err != nil {
		return errorResult(prompt, start, err.Error())
	}
	_ = ctx.OnUpdate(sdk.ToolResult{Content: "", Details: map[string]any{}})
	actions := BuildActionSummary(cwd, calls.States())
	if actions != "" {
		text += "\n\n[Claude Code actions: " + actions + "]"
	}
	return sdk.ToolResult{Content: text, Details: map[string]any{"prompt": prompt, "executionTime": time.Since(start).Milliseconds(), "actions": actions}}
}

// preview is s cut to the preview limits and whether anything was cut.
func preview(s string) (string, bool) {
	cut := []rune(s)
	more := len(cut) > previewMaxChars
	if more {
		cut = cut[:previewMaxChars]
	}
	lines := strings.Split(string(cut), "\n")
	if len(lines) > previewMaxLines {
		lines, more = lines[:previewMaxLines], true
	}
	return strings.Join(lines, "\n"), more || len(strings.Split(s, "\n")) > previewMaxLines
}

func renderCall(args map[string]any, d Defaults) []string {
	a := CallArgs{}
	if m, ok := args["mode"].(string); ok {
		a.Mode = Mode(m)
	}
	a.Model, _ = args["model"].(string)
	a.Thinking, _ = args["thinking"].(string)
	if v, ok := args["isolated"].(bool); ok {
		a.Isolated = &v
	}
	text := "AskClaude "
	if tags := CallTags(a, d); len(tags) > 0 {
		text += "[" + strings.Join(tags, ", ") + "] "
	}
	prompt, _ := args["prompt"].(string)
	body, more := preview(prompt)
	text += `"` + body + `"`
	if more {
		text += " …"
	}
	return strings.Split(text, "\n")
}

func resultText(result sdk.ToolRenderResult) string {
	if len(result.Content) > 0 && result.Content[0]["type"] == "text" {
		s, _ := result.Content[0]["text"].(string)
		return s
	}
	return ""
}

func renderResult(result sdk.ToolRenderResult, opts sdk.ToolRenderResultOptions) []string {
	body := resultText(result)
	if opts.IsPartial {
		status := body
		if len(result.Content) == 0 || result.Content[0]["type"] != "text" {
			status = "working..."
		}
		return []string{"◉ Claude Code " + status}
	}
	details, _ := result.Details.(map[string]any)
	text := "✓ Claude Code"
	if failed, _ := details["error"].(bool); failed {
		text = "✗ Claude Code error"
	}
	if ms, ok := details["executionTime"].(float64); ok && ms > 0 {
		text += fmt.Sprintf(" %.1fs", ms/1000)
	} else if ms, ok := details["executionTime"].(int64); ok && ms > 0 {
		text += fmt.Sprintf(" %.1fs", float64(ms)/1000)
	}
	if actions, _ := details["actions"].(string); actions != "" {
		text += " " + actions
	}
	prompt, _ := details["prompt"].(string)
	if opts.Expanded {
		if prompt != "" {
			text += "\nPrompt: " + prompt
		}
		if prompt != "" && body != "" {
			text += "\n" + strings.Repeat("─", 40)
		}
		if body != "" {
			text += "\n" + body
		}
	} else {
		shown, more := preview(body)
		if shown != "" {
			text += "\n" + shown
		}
		if more {
			text += "\n… (to expand)"
		}
	}
	return strings.Split(text, "\n")
}
