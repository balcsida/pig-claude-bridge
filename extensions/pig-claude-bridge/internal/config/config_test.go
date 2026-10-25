package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

const configDirName = ".pi"

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ptr[T any](v T) *T { return &v }

func TestClaudeCodeSettings(t *testing.T) {
	t.Run("disables auto-memory by default", func(t *testing.T) {
		want := map[string]any{"autoMemoryEnabled": false}
		if got := ClaudeCodeSettings(nil); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if got := ClaudeCodeSettings(&ProviderConfig{}); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("allows auto-memory to be enabled", func(t *testing.T) {
		got := ClaudeCodeSettings(&ProviderConfig{AutoMemoryEnabled: ptr(true)})
		if want := map[string]any{"autoMemoryEnabled": true}; !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func TestLoad(t *testing.T) {
	t.Run("loads project config from Pi's configured project directory", func(t *testing.T) {
		agentDir, cwd := t.TempDir(), t.TempDir()
		write(t, ProjectConfigPath(cwd, configDirName), `{"provider":{"plan":"max"},"askClaude":{"enabled":false}}`)

		want := Config{
			Provider:  &ProviderConfig{Plan: ptr("max")},
			AskClaude: &AskClaudeConfig{Enabled: ptr(false)},
		}
		if got := Load(agentDir, cwd, configDirName); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("merges project config over global config", func(t *testing.T) {
		agentDir, cwd := t.TempDir(), t.TempDir()
		write(t, GlobalConfigPath(agentDir), `{"provider":{"plan":"pro","strictMcpConfig":true},"askClaude":{"enabled":true,"defaultMode":"read"}}`)
		write(t, ProjectConfigPath(cwd, configDirName), `{"provider":{"plan":"max","autoMemoryEnabled":true},"askClaude":{"enabled":false}}`)

		want := Config{
			Provider:  &ProviderConfig{Plan: ptr("max"), StrictMcpConfig: ptr(true), AutoMemoryEnabled: ptr(true)},
			AskClaude: &AskClaudeConfig{Enabled: ptr(false), DefaultMode: ptr("read")},
		}
		if got := Load(agentDir, cwd, configDirName); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("markStartupNoticeShown records today's date without dropping existing settings", func(t *testing.T) {
		agentDir, cwd := t.TempDir(), t.TempDir()
		path := GlobalConfigPath(agentDir)
		write(t, path, `{"askClaude":{"enabled":false},"provider":{"strictMcpConfig":false}}`)

		if err := MarkStartupNoticeShown(agentDir, "2026-10-25"); err != nil {
			t.Fatal(err)
		}
		want := "{\n  \"askClaude\": {\n    \"enabled\": false\n  },\n  \"provider\": {\n    \"strictMcpConfig\": false\n  },\n  \"startupNoticeShown\": \"2026-10-25\"\n}\n"
		if got := read(t, path); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		cfg := Load(agentDir, cwd, configDirName)
		if cfg.StartupNoticeShown != "2026-10-25" || !datePattern.MatchString(cfg.StartupNoticeShown) {
			t.Fatalf("startupNoticeShown = %q", cfg.StartupNoticeShown)
		}
		if !reflect.DeepEqual(cfg.AskClaude, &AskClaudeConfig{Enabled: ptr(false)}) ||
			!reflect.DeepEqual(cfg.Provider, &ProviderConfig{StrictMcpConfig: ptr(false)}) {
			t.Fatalf("settings dropped: %+v", cfg)
		}
	})

	t.Run("markAskClaudeDeprecationNoticeShown records today's date without dropping settings", func(t *testing.T) {
		agentDir := t.TempDir()
		path := GlobalConfigPath(agentDir)
		write(t, path, `{"askClaude":{"enabled":true}}`)

		if err := MarkAskClaudeDeprecationNoticeShown(agentDir, "2026-10-25"); err != nil {
			t.Fatal(err)
		}
		want := "{\n  \"askClaude\": {\n    \"enabled\": true\n  },\n  \"askClaudeDeprecationShown\": \"2026-10-25\"\n}\n"
		if got := read(t, path); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("markStartupNoticeShown leaves an unparseable config untouched", func(t *testing.T) {
		agentDir := t.TempDir()
		path := GlobalConfigPath(agentDir)
		malformed := `{ "askClaude": { "enabled": true }, }`
		write(t, path, malformed)

		if err := MarkStartupNoticeShown(agentDir, "2026-10-25"); err != nil {
			t.Fatal(err)
		}
		if got := read(t, path); got != malformed {
			t.Fatalf("a typo must not cost the user their config: got %q", got)
		}
	})

	t.Run("markStartupNoticeShown creates the config when there is none", func(t *testing.T) {
		agentDir, cwd := t.TempDir(), t.TempDir()
		agentDir = filepath.Join(agentDir, "nested", "agent")
		if got := Load(agentDir, cwd, configDirName).StartupNoticeShown; got != "" {
			t.Fatalf("got %q before marking", got)
		}
		if err := MarkStartupNoticeShown(agentDir, "2026-10-25"); err != nil {
			t.Fatal(err)
		}
		if got := Load(agentDir, cwd, configDirName).StartupNoticeShown; got != "2026-10-25" {
			t.Fatalf("got %q after marking", got)
		}
	})

	t.Run("resolves global config via the agent dir, not hardcoded ~/.pi/agent", func(t *testing.T) {
		agentDir, cwd := t.TempDir(), t.TempDir()
		write(t, filepath.Join(agentDir, "claude-bridge.json"), `{"provider":{"plan":"max"}}`)

		want := Config{Provider: &ProviderConfig{Plan: ptr("max")}, AskClaude: &AskClaudeConfig{}}
		if got := Load(agentDir, cwd, configDirName); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("invalid JSON counts as empty", func(t *testing.T) {
		agentDir, cwd := t.TempDir(), t.TempDir()
		write(t, GlobalConfigPath(agentDir), `{ "provider": { "plan": "pro" }, }`)
		write(t, ProjectConfigPath(cwd, configDirName), `{"askClaude":{"enabled":false}}`)

		want := Config{Provider: &ProviderConfig{}, AskClaude: &AskClaudeConfig{Enabled: ptr(false)}}
		if got := Load(agentDir, cwd, configDirName); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}
