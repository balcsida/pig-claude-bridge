package ccproto

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// sdkVersion is the Node Agent SDK version whose behaviour is mirrored.
const sdkVersion = "0.3.310"

// CCChildEnv is applied to every Claude Code child (index.ts CC_CHILD_ENV, plus the entries
// pi-claude-bridge added in f502f5d and 2edde93, and CLAUDE_CODE_DISABLE_FEATURE_FLAGS, which is this port's own).
var CCChildEnv = map[string]string{
	"ENABLE_CLAUDEAI_MCP_SERVERS": "0",
	"DISABLE_AUTO_COMPACT":        "1",
	"CLAUDE_CODE_ELEGANT_MEADOW":  "0",
	// On Opus 5.5, Fable 5.1, Mythos 5.1 and Sonnet 5.5, Claude Code adds "The user hasn't heard from
	// you in a while..." after 5 assistant messages with no visible text. PiG decides what the model is
	// told; the message is not in PiG's history and the user never sees it.
	"CLAUDE_CODE_SILENT_TURN_REMINDER": "0",
	// Claude Code cuts MCP tool descriptions mid-sentence at 4096 chars; 16384 is its own larger limit.
	"CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH": "16384",
	// A resumed session that ends at a tool result reads to Claude Code as an interrupted turn, and behind
	// a server flag it inserts a "Continue from where you left off." user message ahead of PiG's prompt.
	// PiG supplies every prompt, so every turn counts as too old to resume.
	"CLAUDE_CODE_RESUME_INTERRUPTED_TURN_MAX_AGE_MS": "1",
	// No GrowthBook feature flags or experiments (tengu_*): every flag takes its built-in default, and the
	// cached assignments in ~/.claude.json are not read either. CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
	// already implies this in 2.1.319 (flag evaluation is off whenever telemetry is); the explicit
	// variable pins it. Pinned by TestDisableFeatureFlagsTurnsFlagEvaluationOff.
	"CLAUDE_CODE_DISABLE_FEATURE_FLAGS": "1",
}

// EnvAddedAfterSnapshot lists the CCChildEnv keys that testdata/sdk-argv-snapshot.json (recorded at
// pi-claude-bridge 6aa276a) does not have: added upstream later (f502f5d, 2edde93) or by this port
// (CLAUDE_CODE_DISABLE_FEATURE_FLAGS). Snapshot tests set them aside; TestBuildEnv pins their values.
var EnvAddedAfterSnapshot = []string{"CLAUDE_CODE_SILENT_TURN_REMINDER", "CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH",
	"CLAUDE_CODE_RESUME_INTERRUPTED_TURN_MAX_AGE_MS", "CLAUDE_CODE_DISABLE_FEATURE_FLAGS"}

// parentSessionVars are set by Claude Code on its own child processes, or exported by that parent. When PiG
// runs inside a Claude Code session they describe that session, not ours: the SDK stamps its own entrypoint
// and version only when they are unset, and Claude Code's Bash tool runs CLAUDE_ENV_FILE's script.
// AI_AGENT is left alone. Dropped only when CLAUDECODE says PiG was started from inside Claude Code (index.ts e93d51d).
var parentSessionVars = []string{
	"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_PID", "CLAUDE_EFFORT", "TRACEPARENT", "CLAUDE_CODE_INVOKED_SKILLS", "CLAUDE_ENV_FILE",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_AGENT_SDK_VERSION", "CLAUDE_CODE_EXECPATH",
	"CLAUDE_CODE_SSE_PORT", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
}

// ExtraArg is one extraArgs entry; a nil Value emits the bare flag.
type ExtraArg struct {
	Key   string
	Value *string
}

// Options describes one Claude Code child process.
type Options struct {
	Executable, Cwd string
	Env             []string // extra entries appended to the base environment by BuildEnv
	// Model is the SDK's model option (--model=<id>). The provider and AskClaude pass their
	// model through ExtraArgs instead (two tokens, as the TypeScript's extraArgs.model); only
	// the isolated query sets Model.
	Model, Effort, Resume string
	// ThinkingDisabled is the SDK's thinking:{type:"disabled"} (--thinking=disabled): PiG sends no
	// reasoning level when thinking is off, and Claude Code would otherwise apply its own default.
	ThinkingDisabled                      bool
	MaxTurns                              int
	StrictMcpConfig, NoSessionPersistence bool
	IncludePartialMessages                bool
	Tools                                 []string
	ToolsSet                              bool
	SettingSources                        []string
	SettingSourcesSet                     bool
	AllowedTools, DisallowedTools         []string
	PermissionMode                        string
	Settings                              map[string]any
	DebugFile                             string
	SDKMcpServers                         []string
	AppendSystemPrompt                    string
	SystemPrompt                          string // sent in initialize as a one-element array when SystemPromptSet
	SystemPromptSet                       bool
	Skills                                []string // nil is omitted from initialize; non-nil (even empty) is sent
	ExtraArgs                             []ExtraArg
}

// sdkOption mirrors the SDK helper Oy: two tokens, or one k=v token when the value is longer than "-" and starts with "-".
func sdkOption(key, value string) []string {
	if len(value) > 1 && strings.HasPrefix(value, "-") {
		return []string{key + "=" + value}
	}
	return []string{key, value}
}

// BuildArgs returns the argv in the order the Node SDK 0.3.310 emits it for the options the bridge uses
// (pinned by testdata/sdk-argv-snapshot.json). Options become --k=v tokens; ExtraArgs and --settings come
// last as two-token pairs (extraArgs first, settings after). SDK MCP servers and the system prompt never appear on argv.
func BuildArgs(o Options) []string {
	args := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json"}
	if o.ThinkingDisabled {
		args = append(args, "--thinking=disabled")
	}
	if o.Effort != "" {
		args = append(args, "--effort="+o.Effort)
	}
	if o.MaxTurns > 0 {
		args = append(args, "--max-turns="+strconv.Itoa(o.MaxTurns))
	}
	if o.Model != "" {
		args = append(args, "--model="+o.Model)
	}
	if o.DebugFile != "" {
		args = append(args, "--debug-file="+o.DebugFile)
	}
	if o.Resume != "" {
		args = append(args, "--resume="+o.Resume)
	}
	if len(o.AllowedTools) > 0 {
		args = append(args, "--allowedTools="+strings.Join(o.AllowedTools, ","))
	}
	if len(o.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools="+strings.Join(o.DisallowedTools, ","))
	}
	if o.ToolsSet {
		args = append(args, "--tools="+strings.Join(o.Tools, ","))
	}
	if o.SettingSourcesSet {
		args = append(args, "--setting-sources="+strings.Join(o.SettingSources, ","))
	}
	if o.StrictMcpConfig {
		args = append(args, "--strict-mcp-config")
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode="+o.PermissionMode)
	}
	if o.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if o.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}
	for _, e := range o.ExtraArgs {
		if e.Value == nil {
			args = append(args, "--"+e.Key)
		} else {
			args = append(args, sdkOption("--"+e.Key, *e.Value)...)
		}
	}
	if len(o.Settings) > 0 {
		args = append(args, sdkOption("--settings", settingsJSON(o.Settings))...)
	}
	return args
}

// settingsJSON matches JSON.stringify: compact, no HTML escaping (map keys are sorted by encoding/json).
func settingsJSON(settings map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(settings) // the bridge's settings are bools, strings and string lists, which cannot fail to encode
	return strings.TrimSuffix(buf.String(), "\n")
}

// BuildEnv returns the child environment: base plus o.Env, minus NODE_OPTIONS, DEBUG, TRACEPARENT and
// TRACESTATE (and parentSessionVars when CLAUDECODE is set), with the SDK identity variables added when
// unset, then CCChildEnv, CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 (the TypeScript sets it for the child
// only since e93d51d; before, on process.env at load) and PI_CODING_AGENT=true. An o.Env entry with an
// empty value ("NAME=") removes NAME from base instead: pi's provider env can only hold strings, so an
// empty one is how a caller unsets an exported variable for Claude Code (cc-child.ts ccChildEnv).
// Later entries win over earlier duplicates in the returned slice (execve semantics for the consumer).
func BuildEnv(base []string, o Options) []string {
	strip := map[string]bool{"NODE_OPTIONS": true, "DEBUG": true, "TRACEPARENT": true, "TRACESTATE": true}
	if slices.ContainsFunc(base, func(kv string) bool {
		k, v, _ := strings.Cut(kv, "=")
		return k == "CLAUDECODE" && v != ""
	}) {
		for _, k := range parentSessionVars {
			strip[k] = true
		}
	}
	overrides := make([]string, 0, len(o.Env))
	for _, kv := range o.Env {
		if k, v, _ := strings.Cut(kv, "="); v == "" {
			strip[k] = true
		} else {
			overrides = append(overrides, kv)
		}
	}
	inherited := append(append([]string{}, base...), overrides...)
	env := make([]string, 0, len(inherited)+8)
	for _, kv := range inherited {
		if !strip[strings.SplitN(kv, "=", 2)[0]] {
			env = append(env, kv)
		}
	}
	// As the SDK: the entry point and version are set when their exact-case key is missing
	// or empty (`if (!env.X)`), the session-state flag when no key matches it case-insensitively.
	value := func(key string) (v string) {
		for _, kv := range env {
			if k, val, _ := strings.Cut(kv, "="); k == key {
				v = val // the last duplicate wins
			}
		}
		return v
	}
	if value("CLAUDE_CODE_ENTRYPOINT") == "" {
		env = append(env, "CLAUDE_CODE_ENTRYPOINT=sdk-ts")
	}
	if value("CLAUDE_AGENT_SDK_VERSION") == "" {
		env = append(env, "CLAUDE_AGENT_SDK_VERSION="+sdkVersion)
	}
	if !slices.ContainsFunc(env, func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(k, "CLAUDE_CODE_SDK_READS_SESSION_STATE")
	}) {
		env = append(env, "CLAUDE_CODE_SDK_READS_SESSION_STATE=1")
	}
	keys := make([]string, 0, len(CCChildEnv))
	for k := range CCChildEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+CCChildEnv[k])
	}
	return append(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "PI_CODING_AGENT=true")
}

// BuildInitialize returns the initialize control request as one JSON object without newline, with the
// key order the Node SDK writes. sdkMcpServers is omitted when there are none; no hooks are sent.
func BuildInitialize(o Options, requestID string) []byte {
	type request struct {
		Subtype            string   `json:"subtype"`
		SystemPrompt       []string `json:"systemPrompt,omitempty"`
		SDKMcpServers      []string `json:"sdkMcpServers,omitempty"`
		AppendSystemPrompt string   `json:"appendSystemPrompt,omitempty"`
		Skills             any      `json:"skills,omitempty"` // holds o.Skills only when non-nil, so an empty list is still sent
	}
	r := request{Subtype: "initialize", SDKMcpServers: o.SDKMcpServers, AppendSystemPrompt: o.AppendSystemPrompt}
	if o.SystemPromptSet {
		r.SystemPrompt = []string{o.SystemPrompt}
	}
	if o.Skills != nil {
		r.Skills = o.Skills
	}
	frame := struct {
		RequestID string  `json:"request_id"`
		Type      string  `json:"type"`
		Request   request `json:"request"`
	}{requestID, "control_request", r}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(frame) // plain strings and slices cannot fail to encode
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
