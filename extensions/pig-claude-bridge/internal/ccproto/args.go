package ccproto

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// sdkVersion is the Node Agent SDK version whose behaviour is mirrored.
const sdkVersion = "0.3.310"

// CCChildEnv is applied to every Claude Code child (index.ts CC_CHILD_ENV).
var CCChildEnv = map[string]string{
	"ENABLE_CLAUDEAI_MCP_SERVERS": "0",
	"DISABLE_AUTO_COMPACT":        "1",
	"CLAUDE_CODE_ELEGANT_MEADOW":  "0",
}

// ExtraArg is one extraArgs entry; a nil Value emits the bare flag.
type ExtraArg struct {
	Key   string
	Value *string
}

// Options describes one Claude Code child process.
type Options struct {
	Executable, Cwd                       string
	Env                                   []string // extra entries appended to the base environment by BuildEnv
	Model, Effort, Resume                 string
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
	if err := enc.Encode(settings); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// BuildEnv returns the child environment: base plus o.Env, minus NODE_OPTIONS, DEBUG, TRACEPARENT and
// TRACESTATE, with the SDK identity variables added when unset, then CCChildEnv,
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 (index.ts sets it on process.env at load) and PI_CODING_AGENT=true.
// Later entries win over earlier duplicates in the returned slice (execve semantics for the consumer).
func BuildEnv(base []string, o Options) []string {
	strip := map[string]bool{"NODE_OPTIONS": true, "DEBUG": true, "TRACEPARENT": true, "TRACESTATE": true}
	env := make([]string, 0, len(base)+len(o.Env)+8)
	for _, kv := range append(append([]string{}, base...), o.Env...) {
		if !strip[strings.SplitN(kv, "=", 2)[0]] {
			env = append(env, kv)
		}
	}
	isSet := func(key string) bool {
		for _, kv := range env {
			if strings.EqualFold(strings.SplitN(kv, "=", 2)[0], key) {
				return true
			}
		}
		return false
	}
	for _, d := range [][2]string{
		{"CLAUDE_CODE_ENTRYPOINT", "sdk-ts"},
		{"CLAUDE_AGENT_SDK_VERSION", sdkVersion},
		{"CLAUDE_CODE_SDK_READS_SESSION_STATE", "1"},
	} {
		if !isSet(d[0]) {
			env = append(env, d[0]+"="+d[1])
		}
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
