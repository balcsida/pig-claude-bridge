package askclaude

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

func ptr[T any](v T) *T { return &v }

func props(c *config.AskClaudeConfig) map[string]map[string]any {
	out := map[string]map[string]any{}
	for k, v := range BuildParams(ResolveDefaults(c))["properties"].(map[string]any) {
		out[k] = v.(map[string]any)
	}
	return out
}

func toolDescription(c *config.AskClaudeConfig) string {
	var override *string
	if c != nil {
		override = c.Description
	}
	return ToolDescription(ResolveDefaults(c), override)
}

func tags(a CallArgs, c *config.AskClaudeConfig) []string { return CallTags(a, ResolveDefaults(c)) }

func equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func matches(t *testing.T, s, pattern string, want bool) {
	t.Helper()
	if regexp.MustCompile(pattern).MatchString(s) != want {
		t.Fatalf("%q matches %s = %t, want %t", s, pattern, !want, want)
	}
}

// --- unit-askclaude-schema.mjs ---

func TestDefaultConfiguration(t *testing.T) {
	t.Run("keeps the existing descriptions", func(t *testing.T) {
		p := props(nil)
		equal(t, p["prompt"]["description"], "The question or task for Claude Code. By default Claude sees the full conversation history. Don't research up front, let Claude explore.")
		equal(t, p["mode"]["description"], `"read" (default): questions about the codebase — review, analysis, explain. "none": general knowledge only (no file access). "full": allows writing and bash execution (careful: runs without feedback to pi).`)
		equal(t, p["isolated"]["description"], "When true, Claude sees only this prompt (clean session). When false (default), Claude sees the full conversation history.")
		equal(t, toolDescription(nil), "Delegate to Claude Code for a second opinion or analysis (code review, architecture questions, debugging theories), or to autonomously handle a task. Defaults to read-only mode — use full mode when the user wants to delegate a task that requires changes. Prefer to handle straightforward tasks yourself.")
	})
	t.Run("offers every mode and tags nothing on a bare call", func(t *testing.T) {
		equal(t, props(&config.AskClaudeConfig{})["mode"]["enum"], []string{"read", "full", "none"})
		equal(t, tags(CallArgs{}, &config.AskClaudeConfig{}), []string{})
	})
	t.Run("tags explicit non-default options", func(t *testing.T) {
		equal(t, tags(CallArgs{Mode: ModeFull, Model: "sonnet", Thinking: "high", Isolated: ptr(true)}, &config.AskClaudeConfig{}),
			[]string{"mode=full", "model=sonnet", "thinking=high", "isolated"})
	})
}

func TestDefaultModeFull(t *testing.T) {
	conf := &config.AskClaudeConfig{DefaultMode: ptr("full")}
	t.Run("states full as the default", func(t *testing.T) {
		desc := props(conf)["mode"]["description"].(string)
		matches(t, desc, `"full" \(default\)`, true)
		matches(t, desc, `"read" \(default\)`, false)
		matches(t, toolDescription(conf), `Defaults to full mode`, true)
	})
	t.Run("shows inherited write access and a read-only override in the status line", func(t *testing.T) {
		equal(t, tags(CallArgs{}, conf), []string{"mode=full"})
		equal(t, tags(CallArgs{Mode: ModeRead}, conf), []string{"mode=read"})
	})
}

func TestDefaultIsolatedTrue(t *testing.T) {
	conf := &config.AskClaudeConfig{DefaultIsolated: ptr(true)}
	t.Run("states isolation as the default", func(t *testing.T) {
		p := props(conf)
		equal(t, p["isolated"]["description"], "When true (default), Claude sees only this prompt (clean session). When false, Claude sees the full conversation history.")
		matches(t, p["prompt"]["description"].(string), `By default Claude sees only this prompt \(isolated session\)\.`, true)
	})
	t.Run("shows inherited isolation in the status line", func(t *testing.T) {
		equal(t, tags(CallArgs{}, conf), []string{"isolated"})
		equal(t, tags(CallArgs{Isolated: ptr(false)}, conf), []string{})
	})
}

func TestAllowFullModeFalse(t *testing.T) {
	t.Run("removes full from the schema", func(t *testing.T) {
		p := props(&config.AskClaudeConfig{AllowFullMode: ptr(false)})
		equal(t, p["mode"]["enum"], []string{"read", "none"})
		matches(t, p["mode"]["description"].(string), `"full"`, false)
	})
	t.Run("overrides a conflicting full default", func(t *testing.T) {
		conf := &config.AskClaudeConfig{AllowFullMode: ptr(false), DefaultMode: ptr("full")}
		equal(t, ResolveDefaults(conf).Mode, ModeRead)
		equal(t, tags(CallArgs{}, conf), []string{})
	})
	t.Run("rejects full mode during execution even if a caller bypasses the schema", func(t *testing.T) {
		d := ResolveDefaults(&config.AskClaudeConfig{AllowFullMode: ptr(false)})
		for _, c := range []struct {
			mode any
			want string
		}{{"full", "full mode is disabled"}, {"unexpected", "Invalid AskClaude mode"}, {nil, "Invalid AskClaude mode"}} {
			if _, err := ResolveMode(c.mode, d); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ResolveMode(%v) error = %v, want %q", c.mode, err, c.want)
			}
		}
		mode, err := ResolveMode(Unset, d)
		equal(t, err, nil)
		equal(t, mode, ModeRead)
	})
	// The TypeScript fails closed for a non-boolean allowFullMode. config.Load drops a
	// wrongly typed key, so the Go config reaches ResolveDefaults as nil (see the report).
	t.Run("keeps the package default true for a null or omitted allowFullMode", func(t *testing.T) {
		equal(t, ResolveDefaults(&config.AskClaudeConfig{}).AllowFull, true)
		equal(t, ResolveDefaults(nil).AllowFull, true)
	})
}

func TestOtherConfiguredDefaults(t *testing.T) {
	t.Run("states and renders none as the default mode, including an explicit read escalation", func(t *testing.T) {
		conf := &config.AskClaudeConfig{DefaultMode: ptr("none")}
		matches(t, props(conf)["mode"]["description"].(string), `"none" \(default\)`, true)
		matches(t, toolDescription(conf), `Defaults to no file access`, true)
		equal(t, tags(CallArgs{}, conf), []string{"mode=none"})
		equal(t, tags(CallArgs{Mode: ModeRead}, conf), []string{"mode=read"})
	})
	t.Run("falls back to generated text for a null description", func(t *testing.T) {
		equal(t, toolDescription(&config.AskClaudeConfig{Description: nil}), toolDescription(&config.AskClaudeConfig{}))
	})
	t.Run("returns a non-null configured description verbatim", func(t *testing.T) {
		equal(t, toolDescription(&config.AskClaudeConfig{DefaultMode: ptr("none"), Description: ptr("Custom override text")}), "Custom override text")
	})
	t.Run("treats a null configured mode as unset, falling back to the package default", func(t *testing.T) {
		equal(t, ResolveDefaults(&config.AskClaudeConfig{DefaultMode: nil}).Mode, ModeRead)
	})
	t.Run("fails closed for an invalid configured mode", func(t *testing.T) {
		equal(t, ResolveDefaults(&config.AskClaudeConfig{DefaultMode: ptr("unexpected")}).Mode, ModeNone)
	})
}

// TestFullModeGatedByAllowFull: allowFullMode gates full mode in the schema, the
// resolved mode and the tools Claude Code is allowed.
func TestFullModeGatedByAllowFull(t *testing.T) {
	gated := ResolveDefaults(&config.AskClaudeConfig{AllowFullMode: ptr(false)})
	open := ResolveDefaults(nil)
	equal(t, BuildParams(gated)["properties"].(map[string]any)["mode"].(map[string]any)["enum"], []string{"read", "none"})
	equal(t, BuildParams(open)["properties"].(map[string]any)["mode"].(map[string]any)["enum"], []string{"read", "full", "none"})
	if _, err := ResolveMode("full", gated); err == nil {
		t.Fatal("full accepted with allowFullMode false")
	}
	if m, err := ResolveMode("full", open); err != nil || m != ModeFull {
		t.Fatalf("full with allowFullMode true = %q, %v", m, err)
	}
}

func TestDisallowedTools(t *testing.T) {
	has := func(mode Mode, tool string) bool {
		for _, d := range DisallowedTools(mode) {
			if d == tool {
				return true
			}
		}
		return false
	}
	for _, mode := range []Mode{ModeFull, ModeRead, ModeNone} {
		if !has(mode, "AskUserQuestion") || !has(mode, "ScheduleWakeup") {
			t.Fatalf("%s does not block the always-blocked tools", mode)
		}
	}
	if has(ModeFull, "Write") || has(ModeFull, "Bash") {
		t.Fatal("full blocks write or bash")
	}
	if !has(ModeRead, "Write") || !has(ModeRead, "Bash") || has(ModeRead, "Read") || has(ModeRead, "Grep") {
		t.Fatal("read is not the read-only built-ins")
	}
	for _, tool := range []string{"Read", "Glob", "Grep", "Bash", "WebFetch", "WebSearch", "Agent"} {
		if !has(ModeNone, tool) {
			t.Fatalf("none allows %s", tool)
		}
	}
}
