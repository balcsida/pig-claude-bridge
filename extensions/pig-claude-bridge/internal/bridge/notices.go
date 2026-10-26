package bridge

import (
	"fmt"
	"strings"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/models"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/promptcapture"
)

// queueStartupNotices collects the defaults that silently cost the user something
// (no Opus 1M on Max). They are announced once, at the first bridge query rather
// than at startup: showing them persists a flag to the global config, and doing
// that for every session that merely has the extension installed would write the
// file needlessly.
func (b *Bridge) queueStartupNotices() {
	cfg := b.deps.Config
	if cfg.StartupNoticeShown == "" && (cfg.Provider == nil || cfg.Provider.Plan == nil) {
		b.pendingNotices = append(b.pendingNotices, `Are you using a Max plan? You need to set provider.plan to "max" to unlock 1M context in Opus.`)
	}
}

// showStartupNoticeOnce shows the pending notices as one message, because
// consecutive info notifies overwrite each other in the TUI. Outside the TUI they
// stay pending: hasUI is true in RPC mode too, but only a terminal user can act on
// them.
func (b *Bridge) showStartupNoticeOnce(host Host) {
	if host.Mode() != "tui" {
		return
	}
	b.mu.Lock()
	notices := b.pendingNotices
	b.pendingNotices = nil
	b.mu.Unlock()
	if len(notices) == 0 {
		return
	}
	path := config.GlobalConfigPath(b.deps.AgentDir)
	if err := config.MarkStartupNoticeShown(b.deps.AgentDir, b.deps.Now().Format("2006-01-02")); err != nil {
		b.debugf("startup notice: cannot record it as shown: %v", err)
	}
	// The inner reset drops back to the terminal default rather than the host's dim.
	title := fmt.Sprintf("\x1b[33mWelcome to pi-claude-bridge\x1b[39m — settings live in %s", path)
	lines := []string{title}
	for _, n := range append(notices, "This message only appears once. See README.md for more.") {
		lines = append(lines, "• "+n)
	}
	lines = append(lines, strings.Repeat("─", 64))
	host.Notify(strings.Join(lines, "\n"), "info")
}

// reportLeaks names whatever a settled session left behind in one greppable line.
// Every one of these should be empty once the last turn ends, and each costs
// something real: a retained context routes a later orphaned tool result into the
// delivery path and returns a stream nobody ends; a pending tool call is an MCP
// handler Claude Code is still waiting on; a live prompt stream is an unresolved ack.
func (b *Bridge) reportLeaks(label string) {
	contexts := b.liveContexts()
	pendingCalls, liveStreams := 0, 0
	for _, c := range contexts {
		c.mu.Lock()
		pendingCalls += len(c.pendingToolCalls)
		if c.promptStream != nil {
			liveStreams++
		}
		c.mu.Unlock()
	}
	if len(contexts) == 0 && pendingCalls == 0 && liveStreams == 0 {
		return
	}
	b.debugf("WARNING: %s left state behind — contexts=%d pendingToolCalls=%d promptStreams=%d", label, len(contexts), pendingCalls, liveStreams)
}

// debugCaptureMiss logs why a system prompt matched no capture.
func (b *Bridge) debugCaptureMiss(d promptcapture.Diagnostic) {
	if len(d.Matches) == 0 {
		b.debugf("prompt-capture: no match for %d-char system prompt. no known captures to compare against. known keys=0", len(d.SystemPrompt))
		return
	}
	first := d.Matches[0]
	at := first.FirstDivergent
	from, to := max(at-40, 0), min(at+60, len(d.SystemPrompt))
	b.debugf("prompt-capture: no match for %d-char system prompt. closest known (%d-char) shares its first %d chars and diverges at offset %d: %q known keys=%d",
		len(d.SystemPrompt), len(first.Key), at, at, d.SystemPrompt[from:to], len(d.Matches))
}

// longContextSettings reads the plan facts that decide whether a [1m] id is served.
func longContextSettings(p *config.ProviderConfig) models.LongContextSettings {
	s := models.LongContextSettings{Plan: "pro"}
	if p == nil {
		return s
	}
	if p.Plan != nil {
		s.Plan = *p.Plan
	}
	if p.LongContextExtraUsage != nil {
		s.LongContextExtraUsage = *p.LongContextExtraUsage
	}
	s.ForceTwoHundredK = p.ForceTwoHundredK
	return s
}

// reasoningToEffort maps pi reasoning levels to Claude Code effort levels.
var reasoningToEffort = map[string]string{
	"minimal": "low", "low": "low", "medium": "medium", "high": "high", "xhigh": "max", "max": "max",
}

var validEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// effortForReasoning prefers the model's own thinkingLevelMap (per-model overrides,
// e.g. routing xhigh to xhigh where the generic table maps it to max); the table
// covers models without one and the levels a map leaves unnamed. A null entry means
// the level is unsupported on that model: no effort is sent, so Claude Code's own
// default applies. A map value is trusted only when it names a level Claude Code
// accepts. "" means no effort.
func effortForReasoning(model map[string]any, reasoning any) string {
	level, _ := reasoning.(string)
	if level == "" {
		return ""
	}
	mapped, present := obj(model["thinkingLevelMap"])[level]
	if !present {
		return reasoningToEffort[level]
	}
	if effort, ok := mapped.(string); ok && validEfforts[effort] {
		return effort
	}
	return ""
}
