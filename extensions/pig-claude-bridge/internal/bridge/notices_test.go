package bridge

import (
	"os"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

func TestEffortForReasoning(t *testing.T) {
	model := func(levels map[string]any) map[string]any {
		if levels == nil {
			return map[string]any{"id": "claude-haiku-4-5"}
		}
		return map[string]any{"id": "claude-haiku-4-5", "thinkingLevelMap": levels}
	}
	cases := []struct {
		name      string
		model     map[string]any
		reasoning any
		want      string
	}{
		{"no reasoning means no effort", model(nil), nil, ""},
		{"an empty reasoning means no effort", model(nil), "", ""},
		{"minimal maps to low", model(nil), "minimal", "low"},
		{"xhigh maps to max without a map", model(nil), "xhigh", "max"},
		{"high maps to high", model(nil), "high", "high"},
		{"an unknown level has no effort", model(nil), "ludicrous", ""},
		{"the model's own map overrides the table", model(map[string]any{"xhigh": "xhigh"}), "xhigh", "xhigh"},
		{"a level the map leaves unnamed falls back to the table", model(map[string]any{"xhigh": "xhigh"}), "medium", "medium"},
		{"a null entry means unsupported: no effort", model(map[string]any{"high": nil}), "high", ""},
		{"a map value Claude Code does not accept is not trusted", model(map[string]any{"high": "turbo"}), "high", ""},
		{"a non-string map value is not trusted", model(map[string]any{"high": 3.0}), "high", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effortForReasoning(c.model, c.reasoning); got != c.want {
				t.Fatalf("effort = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStartupNoticeShownOnce(t *testing.T) {
	newBridge := func(t *testing.T, cfg config.Config) (*Bridge, string) {
		agentDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		return New(Deps{AgentDir: agentDir, Config: cfg}), agentDir
	}
	max := "max"

	t.Run("an unset plan queues the Max-plan notice, shown once and recorded in the global config", func(t *testing.T) {
		b, agentDir := newBridge(t, config.Config{})
		host := &fakeHost{mode: "tui"}
		b.showStartupNoticeOnce(host)
		b.showStartupNoticeOnce(host)
		n := host.noticeList()
		if len(n) != 1 || !strings.HasPrefix(n[0], "info: \x1b[33mWelcome to pi-claude-bridge\x1b[39m — settings live in "+config.GlobalConfigPath(agentDir)) ||
			!strings.Contains(n[0], "• Are you using a Max plan? You need to set provider.plan to \"max\" to unlock 1M context in Opus.\n• This message only appears once. See README.md for more.\n"+strings.Repeat("─", 64)) {
			t.Fatalf("notices = %q", n)
		}
		data, err := os.ReadFile(config.GlobalConfigPath(agentDir))
		if err != nil || !strings.Contains(string(data), `"startupNoticeShown": "`) {
			t.Fatalf("global config = %q, %v", data, err)
		}
	})
	t.Run("outside the TUI the notice stays pending and nothing is recorded", func(t *testing.T) {
		b, agentDir := newBridge(t, config.Config{})
		for _, mode := range []string{"rpc", "json", "print", ""} {
			host := &fakeHost{mode: mode}
			b.showStartupNoticeOnce(host)
			if len(host.noticeList()) != 0 {
				t.Fatalf("%q: notices = %v", mode, host.noticeList())
			}
		}
		if _, err := os.Stat(config.GlobalConfigPath(agentDir)); err == nil {
			t.Fatal("the global config was written for a notice nobody saw")
		}
		tui := &fakeHost{mode: "tui"}
		b.showStartupNoticeOnce(tui)
		if n := tui.noticeList(); len(n) != 1 || !strings.Contains(n[0], "Welcome to pi-claude-bridge") {
			t.Fatalf("the pending notice was not shown in the TUI: %q", n)
		}
	})
	t.Run("a configured plan or an earlier notice queues nothing and writes nothing", func(t *testing.T) {
		for name, cfg := range map[string]config.Config{
			"plan set":      {Provider: &config.ProviderConfig{Plan: &max}},
			"already shown": {StartupNoticeShown: "2026-01-01"},
		} {
			b, agentDir := newBridge(t, cfg)
			host := &fakeHost{mode: "tui"}
			b.showStartupNoticeOnce(host)
			if len(host.noticeList()) != 0 {
				t.Fatalf("%s: notices = %v", name, host.noticeList())
			}
			if _, err := os.Stat(config.GlobalConfigPath(agentDir)); err == nil {
				t.Fatalf("%s: the global config was written", name)
			}
		}
	})
	t.Run("the first provider call shows it", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) { d.Config.Provider.Plan = nil })
		h.host.mode = "tui"
		h.script(answers("cc-notice", "ok"))
		finalOf(t, h.call(t.Context(), "pi-notice", msgs(userMsg("hi"))))
		if n := h.host.noticeList(); len(n) != 1 || !strings.Contains(n[0], "Welcome to pi-claude-bridge") {
			t.Fatalf("notices = %q", n)
		}
	})
}

func TestReportLeaks(t *testing.T) {
	t.Run("names the contexts, parked calls and prompt streams left behind", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		q := b.addContext("pi-leak")
		q.pendingToolCalls = map[string]PendingToolCall{"toolu_1": {ToolName: "read"}, "toolu_2": {ToolName: "read"}}
		q.promptStream = newPromptStream()
		b.addContext("pi-other")
		b.reportLeaks("a label")
		want := "WARNING: a label left state behind — contexts=2 pendingToolCalls=2 promptStreams=1"
		if log := readDebugLog(t, agentDir); !strings.Contains(log, want) {
			t.Fatalf("log:\n%s", log)
		}
	})
	t.Run("is silent when nothing is left", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		b.reportLeaks("a label")
		if _, err := os.Stat(config.DebugLogPath(agentDir)); err == nil {
			t.Fatal("nothing should be logged")
		}
	})
}
