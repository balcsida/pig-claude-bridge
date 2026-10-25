package ccproto

import (
	"bytes"
	"encoding/json"
	"sort"
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
	Executable, Cwd                                           string
	Env                                                       []string // extra entries appended to the base environment by BuildEnv
	Model, Effort, ThinkingDisplay, Resume                    string
	StrictMcpConfig, ReplayUserMessages, NoSessionPersistence bool
	IncludePartialMessages                                    bool
	Tools                                                     []string
	ToolsSet                                                  bool
	AllowedTools                                              []string
	PermissionMode                                            string
	Settings                                                  map[string]any
	DebugFile                                                 string
	SDKMcpServers                                             []string
	AppendSystemPrompt                                        string
	ExtraArgs                                                 []ExtraArg
}

// cy mirrors the SDK helper: two tokens, or one k=v token when the value starts with "-".
func cy(key, value string) []string {
	if strings.HasPrefix(value, "-") {
		return []string{key + "=" + value}
	}
	return []string{key, value}
}

// BuildArgs returns the argv in the SDK's emission order (reference 1.2) for the options the bridge uses.
// Model, ReplayUserMessages, StrictMcpConfig and ThinkingDisplay are emitted where the bridge's extraArgs
// land, before settings (reference section 6). SDK MCP servers and the system prompt never appear on argv.
func BuildArgs(o Options) []string {
	args := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json"}
	if o.Effort != "" {
		args = append(args, "--effort="+o.Effort)
	}
	if len(o.AllowedTools) > 0 {
		args = append(args, "--allowedTools="+strings.Join(o.AllowedTools, ","))
	}
	if o.ToolsSet {
		args = append(args, "--tools="+strings.Join(o.Tools, ","))
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode="+o.PermissionMode)
	}
	if o.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if o.Resume != "" {
		args = append(args, "--resume="+o.Resume)
	}
	if o.Model != "" {
		args = append(args, "--model="+o.Model)
	}
	if o.ReplayUserMessages {
		args = append(args, "--replay-user-messages")
	}
	if o.StrictMcpConfig {
		args = append(args, "--strict-mcp-config")
	}
	if o.ThinkingDisplay != "" {
		args = append(args, "--thinking-display="+o.ThinkingDisplay)
	}
	if len(o.Settings) > 0 {
		args = append(args, cy("--settings", settingsJSON(o.Settings))...)
	}
	for _, e := range o.ExtraArgs {
		if e.Value == nil {
			args = append(args, "--"+e.Key)
		} else {
			args = append(args, cy("--"+e.Key, *e.Value)...)
		}
	}
	if o.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}
	if o.DebugFile != "" {
		args = append(args, "--debug-file="+o.DebugFile)
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
// TRACESTATE, with the SDK identity variables added when unset, then CCChildEnv and PI_CODING_AGENT=true.
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
	return append(env, "PI_CODING_AGENT=true")
}

// BuildInitialize returns the initialize control request (reference 2.2) as one JSON object without newline.
// systemPrompt is omitted (preset) and no hooks are sent.
func BuildInitialize(o Options, requestID string) []byte {
	servers := o.SDKMcpServers
	if servers == nil {
		servers = []string{}
	}
	type request struct {
		Subtype            string   `json:"subtype"`
		SDKMcpServers      []string `json:"sdkMcpServers"`
		AppendSystemPrompt string   `json:"appendSystemPrompt,omitempty"`
	}
	frame := struct {
		Type      string  `json:"type"`
		RequestID string  `json:"request_id"`
		Request   request `json:"request"`
	}{"control_request", requestID, request{"initialize", servers, o.AppendSystemPrompt}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(frame) // plain strings and slices cannot fail to encode
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
