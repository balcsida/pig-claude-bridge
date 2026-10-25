package askclaude

import (
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

func TestDefinitionUsesConfiguredIdentity(t *testing.T) {
	def := Definition(nil, config.Config{AskClaude: &config.AskClaudeConfig{Name: ptr("Delegate"), Label: ptr("Delegate!"), Description: ptr("d")}}, "", nil, nil)
	if def.Name != "Delegate" || def.Label != "Delegate!" || def.Description != "d" {
		t.Fatalf("definition = %q %q %q", def.Name, def.Label, def.Description)
	}
	def = Definition(nil, config.Config{}, "", nil, nil)
	if def.Name != "AskClaude" || def.Label != "Ask Claude Code" {
		t.Fatalf("defaults = %q %q", def.Name, def.Label)
	}
	if def.Execute == nil || def.RenderCall == nil || def.RenderResult == nil || !reflect.DeepEqual(def.Parameters["required"], []string{"prompt"}) {
		t.Fatal("definition is missing its handlers or required prompt")
	}
}

func TestRenderCall(t *testing.T) {
	d := ResolveDefaults(nil)
	got := renderCall(map[string]any{"prompt": "why?", "mode": "full", "isolated": true}, d)
	equal(t, got, []string{`AskClaude [mode=full, isolated] "why?"`})
	long := strings.Repeat("a\n", 10)
	got = renderCall(map[string]any{"prompt": long}, d)
	if len(got) != previewMaxLines || !strings.HasSuffix(got[len(got)-1], `" …`) {
		t.Fatalf("long prompt rendered as %q", got)
	}
}

func text(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }

func TestRenderResult(t *testing.T) {
	t.Run("partial shows the status", func(t *testing.T) {
		equal(t, renderResult(sdk.ToolRenderResult{Content: text("3s — Read(a.go)")}, sdk.ToolRenderResultOptions{IsPartial: true}), []string{"◉ Claude Code 3s — Read(a.go)"})
		equal(t, renderResult(sdk.ToolRenderResult{}, sdk.ToolRenderResultOptions{IsPartial: true}), []string{"◉ Claude Code working..."})
	})
	t.Run("collapsed result has time, actions and a preview", func(t *testing.T) {
		r := sdk.ToolRenderResult{Content: text("answer"), Details: map[string]any{"executionTime": float64(2500), "actions": "Read(a.go)", "prompt": "q"}}
		equal(t, renderResult(r, sdk.ToolRenderResultOptions{}), []string{"✓ Claude Code 2.5s Read(a.go)", "answer"})
	})
	t.Run("expanded result adds the prompt and a rule", func(t *testing.T) {
		r := sdk.ToolRenderResult{Content: text("answer"), Details: map[string]any{"prompt": "q"}}
		equal(t, renderResult(r, sdk.ToolRenderResultOptions{Expanded: true}), []string{"✓ Claude Code", "Prompt: q", strings.Repeat("─", 40), "answer"})
	})
	t.Run("an error result is marked", func(t *testing.T) {
		r := sdk.ToolRenderResult{Content: text("Error: boom"), Details: map[string]any{"error": true}}
		equal(t, renderResult(r, sdk.ToolRenderResultOptions{}), []string{"✗ Claude Code error", "Error: boom"})
	})
	t.Run("a long body is cut with an expand hint", func(t *testing.T) {
		r := sdk.ToolRenderResult{Content: text(strings.Repeat("line\n", 10)), Details: map[string]any{}}
		got := renderResult(r, sdk.ToolRenderResultOptions{})
		if got[len(got)-1] != "… (to expand)" || len(got) != 1+previewMaxLines+1 {
			t.Fatalf("rendered %q", got)
		}
	})
}

func TestShowDeprecationNoticeOnce(t *testing.T) {
	var notes []string
	notify := func(m, l string) { notes = append(notes, l+": "+m) }
	marks := 0
	mark := func() error { marks++; return nil }

	var shown atomic.Bool
	showDeprecationNoticeOnce(&shown, "print", notify, mark)
	if len(notes) != 0 || marks != 0 || shown.Load() {
		t.Fatal("a non-tui call showed the notice")
	}
	showDeprecationNoticeOnce(&shown, "tui", notify, mark)
	showDeprecationNoticeOnce(&shown, "tui", notify, mark)
	equal(t, notes, []string{"warning: " + deprecationNotice})
	equal(t, marks, 1)
	if !shown.Load() {
		t.Fatal("flag not set")
	}

	notes = nil
	var fresh atomic.Bool
	showDeprecationNoticeOnce(&fresh, "tui", notify, func() error { return errors.New("disk full") })
	equal(t, notes, []string{"warning: " + deprecationNotice, "warning: AskClaude: cannot record the deprecation notice: disk full"})

	notes = nil
	var already atomic.Bool
	already.Store(true)
	showDeprecationNoticeOnce(&already, "tui", notify, mark)
	equal(t, len(notes), 0)
}

func TestCircularGuard(t *testing.T) {
	if circularGuard("anthropic") != nil {
		t.Fatal("guard fired for another provider")
	}
	r := circularGuard("claude-bridge")
	if r == nil || r.Content != circularError || r.Details.(map[string]any)["error"] != true {
		t.Fatalf("result = %+v", r)
	}
}

func TestContainPanic(t *testing.T) {
	run := func() (result any, err error) {
		defer containPanic("q", time.Now(), &result, &err)
		panic("boom")
	}
	result, err := run()
	res, ok := result.(sdk.ToolResult)
	if err != nil || !ok || res.Content != "Error: claude bridge internal error: boom" || res.Details.(map[string]any)["error"] != true {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}
