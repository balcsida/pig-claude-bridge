// Package config loads the user-facing claude-bridge.json: the global file in
// the agent dir, overridden per key by the project file in the project config
// directory. Missing or unparseable files count as empty so the extension
// always starts.
package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
)

type Config struct {
	// StartupNoticeShown is the date (YYYY-MM-DD) the one-time startup notice was shown. Written by the extension, not the user.
	StartupNoticeShown string `json:"startupNoticeShown,omitempty"`
	// AskClaudeDeprecationShown is the date (YYYY-MM-DD) the AskClaude deprecation notice was shown. Written by the extension, not the user.
	AskClaudeDeprecationShown string           `json:"askClaudeDeprecationShown,omitempty"`
	AskClaude                 *AskClaudeConfig `json:"askClaude,omitempty"`
	// Provider is low-level Claude Agent SDK plumbing. Most users won't need it.
	Provider *ProviderConfig `json:"provider,omitempty"`
}

type AskClaudeConfig struct {
	Enabled         *bool   `json:"enabled,omitempty"`
	Name            *string `json:"name,omitempty"`
	Label           *string `json:"label,omitempty"`
	Description     *string `json:"description,omitempty"`
	DefaultMode     *string `json:"defaultMode,omitempty"` // "full" | "read" | "none"
	DefaultIsolated *bool   `json:"defaultIsolated,omitempty"`
	AllowFullMode   *bool   `json:"allowFullMode,omitempty"`
	AppendSkills    *bool   `json:"appendSkills,omitempty"`
}

type ProviderConfig struct {
	StrictMcpConfig            *bool   `json:"strictMcpConfig,omitempty"`
	AutoMemoryEnabled          *bool   `json:"autoMemoryEnabled,omitempty"`
	PathToClaudeCodeExecutable *string `json:"pathToClaudeCodeExecutable,omitempty"`
	// Plan is the subscription tier ("pro" | "max"). "max" enables Opus 4.6 at 1M context.
	Plan *string `json:"plan,omitempty"`
	// LongContextExtraUsage opts into metered 1M context usage. Enables Sonnet 4.6 [1m]
	// on every plan and Opus 4.6 [1m] on Pro.
	LongContextExtraUsage *bool `json:"longContextExtraUsage,omitempty"`
	// ForceTwoHundredK lists model ids whose declared 1M context Claude Code does not
	// actually serve; they are pinned to the bare id at 200K.
	ForceTwoHundredK []string `json:"forceTwoHundredK,omitempty"`
}

func GlobalConfigPath(agentDir string) string {
	return filepath.Join(agentDir, "claude-bridge.json")
}

func ProjectConfigPath(cwd, configDirName string) string {
	return filepath.Join(cwd, configDirName, "claude-bridge.json")
}

// ClaudeCodeSettings returns the settings object handed to Claude Code; auto-memory is off unless set.
func ClaudeCodeSettings(p *ProviderConfig) map[string]any {
	enabled := false
	if p != nil && p.AutoMemoryEnabled != nil {
		enabled = *p.AutoMemoryEnabled
	}
	return map[string]any{"autoMemoryEnabled": enabled}
}

// Load merges the project file over the global file per key; a key the project file
// does not set keeps the global value. Unreadable or unparseable files count as empty.
func Load(agentDir, cwd, configDirName string) Config {
	cfg := Config{}
	for _, path := range []string{GlobalConfigPath(agentDir), ProjectConfigPath(cwd, configDirName)} {
		if data, err := os.ReadFile(path); err == nil {
			decodeValidKeys(data, &cfg)
		}
	}
	if cfg.AskClaude == nil {
		cfg.AskClaude = &AskClaudeConfig{}
	}
	if cfg.Provider == nil {
		cfg.Provider = &ProviderConfig{}
	}
	return cfg
}

// decodeValidKeys merges the keys of a JSON file into cfg, skipping any key whose
// value has the wrong type (TS passes those through; encoding/json would leave a
// zero-value pointer behind). A syntax error leaves cfg untouched.
func decodeValidKeys(data []byte, cfg *Config) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	for _, section := range []struct {
		key string
		dst any
	}{{"askClaude", &cfg.AskClaude}, {"provider", &cfg.Provider}} {
		if v, ok := raw[section.key]; ok {
			delete(raw, section.key)
			var keys map[string]json.RawMessage
			if json.Unmarshal(v, &keys) == nil {
				if section.key == "askClaude" {
					failClosed(keys)
				}
				decodeKeys(keys, section.dst)
			}
		}
	}
	decodeKeys(raw, cfg)
}

// failClosed replaces a mistyped mode gate with a value that denies: dropping it would
// fall back to the permissive default (TS: any non-true allowFullMode denies, any non-string defaultMode resolves to "none").
func failClosed(keys map[string]json.RawMessage) {
	var b bool
	var str string
	if v, ok := keys["allowFullMode"]; ok && string(v) != "null" && json.Unmarshal(v, &b) != nil {
		keys["allowFullMode"] = json.RawMessage("false")
	}
	if v, ok := keys["defaultMode"]; ok && string(v) != "null" && json.Unmarshal(v, &str) != nil {
		keys["defaultMode"] = json.RawMessage(`"invalid"`)
	}
}

// decodeKeys unmarshals each key into dst only if it decodes cleanly into a fresh value.
func decodeKeys(keys map[string]json.RawMessage, dst any) {
	for k, v := range keys {
		obj, _ := json.Marshal(map[string]json.RawMessage{k: v})
		if json.Unmarshal(obj, reflect.New(reflect.TypeOf(dst).Elem()).Interface()) == nil {
			_ = json.Unmarshal(obj, dst)
		}
	}
}

// MarkStartupNoticeShown records date in the global config, preserving every other key.
func MarkStartupNoticeShown(agentDir, date string) error {
	return setGlobalKey(agentDir, "startupNoticeShown", date)
}

// MarkAskClaudeDeprecationNoticeShown records date in the global config, preserving every other key.
func MarkAskClaudeDeprecationNoticeShown(agentDir, date string) error {
	return setGlobalKey(agentDir, "askClaudeDeprecationShown", date)
}

// setGlobalKey leaves an unparseable file alone and returns nil: Load reports it as
// empty, so rewriting would replace a user's whole config with just this marker.
// Losing the notice is the cheaper failure; it simply shows again next session.
func setGlobalKey(agentDir, key, value string) error {
	path := GlobalConfigPath(agentDir)
	keys, values := []string{}, map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		var ok bool
		if keys, values, ok = parseObject(data); !ok {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, _ := json.Marshal(value)
	if _, exists := values[key]; !exists {
		keys = append(keys, key)
	}
	values[key] = raw

	var buf bytes.Buffer
	buf.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(",")
		}
		kb, _ := json.Marshal(k)
		buf.WriteString("\n  ")
		buf.Write(kb)
		buf.WriteString(": ")
		buf.Write(values[k])
	}
	buf.WriteString("\n}")
	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "  "); err != nil {
		return err
	}
	out.WriteString("\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}

// parseObject returns a JSON object's keys in file order, as JavaScript preserves them.
func parseObject(data []byte) (keys []string, values map[string]json.RawMessage, ok bool) {
	if json.Unmarshal(data, &values) != nil || values == nil {
		return nil, nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	_, _ = dec.Token() // opening brace
	for dec.More() {
		k, _ := dec.Token()
		var skip json.RawMessage
		_ = dec.Decode(&skip)
		if name := k.(string); !contains(keys, name) {
			keys = append(keys, name)
		}
	}
	return keys, values, true
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
