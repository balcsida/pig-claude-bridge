package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLogPaths(t *testing.T) {
	t.Run("redirects the bridge debug log away from the real one", func(t *testing.T) {
		agentDir := t.TempDir()
		override := filepath.Join(t.TempDir(), "debug.log")
		t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", override)
		if got := DebugLogPath(agentDir); got != override {
			t.Fatalf("got %q, want %q", got, override)
		}
		if got := DebugLogPath(agentDir); got == filepath.Join(agentDir, "claude-bridge.log") {
			t.Fatal("debug log must not resolve to the real one")
		}
	})

	t.Run("resolves lazily from env changes made after first use", func(t *testing.T) {
		t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", "")
		first := DebugLogPath("/tmp/a")
		t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", "/tmp/log-paths-lazy-probe-debug.log")
		if got := DebugLogPath("/tmp/a"); got != "/tmp/log-paths-lazy-probe-debug.log" || got == first {
			t.Fatalf("got %q", got)
		}
		if got := DiagLogPath("/tmp/log-paths-lazy-probe"); got != "/tmp/log-paths-lazy-probe/claude-bridge-diag.log" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("follow the agent dir when no debug path override is set", func(t *testing.T) {
		t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", "")
		if got := DebugLogPath("/tmp/pi-agent-dir-probe"); got != "/tmp/pi-agent-dir-probe/claude-bridge.log" {
			t.Fatalf("got %q", got)
		}
		if got := DiagLogPath("/tmp/pi-agent-dir-probe"); got != "/tmp/pi-agent-dir-probe/claude-bridge-diag.log" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("keeps every path-derived location inside the agent dir", func(t *testing.T) {
		t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", "")
		root := t.TempDir()
		for _, p := range []string{GlobalConfigPath(root), DebugLogPath(root), DiagLogPath(root), CLIDebugLogDir(root)} {
			if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("%q escapes %q", p, root)
			}
		}
	})

	t.Run("CLI debug logs sit beside the debug log", func(t *testing.T) {
		t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", "/tmp/x/debug.log")
		if got := CLIDebugLogDir("/agent"); got != "/tmp/x/cc-cli-logs" {
			t.Fatalf("got %q", got)
		}
	})
}
