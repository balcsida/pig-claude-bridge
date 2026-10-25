package claudebridge

import (
	"context"
	"fmt"
	"os"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/bridge"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/models"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/promptcapture"
)

// sdkHost adapts one request's sdk.Context to bridge.Host. It is built fresh in
// every handler and stream call and never stored.
type sdkHost struct{ ctx sdk.Context }

// Notify drops the notice if the context is stale: a notice must never fail a turn.
func (h sdkHost) Notify(message, level string) {
	defer func() { _ = recover() }()
	h.ctx.Notify(message, level)
}

func (h sdkHost) SessionID() (string, error)    { return h.ctx.SessionManager().GetSessionID() }
func (h sdkHost) Cwd() string                   { return h.ctx.Cwd() }
func (h sdkHost) AgentDir() string              { d, _ := sdk.AgentDir(); return d }
func (h sdkHost) ConfigDirName() string         { d, _ := sdk.ConfigDirName(); return d }
func (h sdkHost) SystemPrompt() (string, error) { return h.ctx.GetSystemPrompt() }

// providerModels is the model list registered with PiG.
func providerModels(p *config.ProviderConfig) []map[string]any {
	s := models.LongContextSettings{Plan: "pro"}
	if p != nil {
		if p.Plan != nil {
			s.Plan = *p.Plan
		}
		if p.LongContextExtraUsage != nil {
			s.LongContextExtraUsage = *p.LongContextExtraUsage
		}
		s.ForceTwoHundredK = p.ForceTwoHundredK
	}
	return models.ApplyLongContext(models.BuildModels(models.Catalog()), s)
}

// requestContext is the Go context for one stream. The run signal is preferred: it
// stays live across the tool-call round trip, whereas the per-request context PiG
// cancels after each stream would kill a query parked at a tool call.
func requestContext(ctx sdk.Context) context.Context {
	if sig := ctx.Signal(); sig != nil {
		return sig
	}
	goCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return goCtx
}

// promptOptions copies systemPromptOptions with the SDK's ordered sections turned
// into the ordered slice the bridge takes.
func promptOptions(raw any) map[string]any {
	src, _ := raw.(map[string]any)
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	switch s := src["sections"].(type) {
	case *sdk.SystemPromptSections:
		if s != nil {
			out["sections"] = orderedSections(*s)
		}
	case sdk.SystemPromptSections:
		out["sections"] = orderedSections(s)
	}
	return out
}

func orderedSections(s sdk.SystemPromptSections) []promptcapture.Section {
	out := make([]promptcapture.Section, len(s))
	for i, sec := range s {
		out[i] = promptcapture.Section{Name: sec.Name, Text: sec.Value}
	}
	return out
}

func Extension() *sdk.Extension {
	ext := sdk.New("pig-claude-bridge")
	agentDir, _ := sdk.AgentDir()
	configDirName, _ := sdk.ConfigDirName()
	cwd, _ := os.Getwd()
	cfg := config.Load(agentDir, cwd, configDirName)
	b := bridge.New(bridge.Deps{
		Config:        cfg,
		AgentDir:      agentDir,
		ConfigDirName: configDirName,
		Spawn:         ccproto.Spawn,
		Now:           time.Now,
		Debug:         os.Getenv("CLAUDE_BRIDGE_DEBUG") == "1",
	})

	streamSimple := func(ctx sdk.Context, model, piContext, options map[string]any) (stream *sdk.ModelEventStream, err error) {
		defer func() {
			if r := recover(); r != nil {
				stream, err = nil, panicError{r}
			}
		}()
		return b.StreamSimple(requestContext(ctx), sdkHost{ctx}, model, piContext, options)
	}
	// Registered in the factory: a late registration misses PiG's startup model resolution.
	ext.RegisterProvider("claude-bridge", sdk.ProviderConfig{
		"baseUrl":      "claude-bridge",
		"apiKey":       "not-used",
		"api":          "claude-bridge",
		"models":       providerModels(cfg.Provider),
		"streamSimple": sdk.ProviderStreamSimpleFunc(streamSimple),
	})

	ext.OnSessionStart(func(ctx sdk.Context, data map[string]any) (any, error) {
		defer func() { _ = recover() }()
		reason, _ := data["reason"].(string)
		b.OnSessionStart(sdkHost{ctx}, reason)
		return nil, nil
	})
	forward := func(event string, fn func(h bridge.Host, data map[string]any)) {
		ext.OnEvent(event, func(ctx sdk.Context, data map[string]any) (any, error) {
			defer func() { _ = recover() }()
			fn(sdkHost{ctx}, data)
			return nil, nil
		})
	}
	forward("before_agent_start", func(h bridge.Host, data map[string]any) {
		prompt, _ := data["systemPrompt"].(string)
		b.OnBeforeAgentStart(h, prompt, promptOptions(data["systemPromptOptions"]))
	})
	forward("agent_start", func(h bridge.Host, _ map[string]any) { b.OnAgentStart(h) })
	forward("turn_start", func(h bridge.Host, _ map[string]any) { b.OnTurnStart(h) })
	forward("session_before_compact", func(h bridge.Host, _ map[string]any) { b.OnSessionBeforeCompact(h) })
	forward("session_before_tree", func(h bridge.Host, _ map[string]any) { b.OnSessionBeforeTree(h) })
	forward("session_compact", func(h bridge.Host, _ map[string]any) { b.OnSessionCompact(h) })
	forward("session_tree", func(h bridge.Host, _ map[string]any) { b.OnSessionTree(h) })
	forward("session_shutdown", func(h bridge.Host, _ map[string]any) { b.OnSessionShutdown(h) })
	return ext
}

// panicError turns a recovered panic at the provider boundary into a failed stream.
type panicError struct{ v any }

func (p panicError) Error() string { return "claude-bridge: panic: " + fmt.Sprint(p.v) }
