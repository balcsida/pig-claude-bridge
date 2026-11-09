package ccproto

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The provider routes its model, the strict MCP flag and the thinking display through extraArgs
// (index.ts:2030-2096), so they come after the options, as two-token pairs or bare flags.
func TestBuildArgsProviderShape(t *testing.T) {
	o := Options{
		Effort: "high", Tools: []string{}, ToolsSet: true, PermissionMode: "bypassPermissions",
		IncludePartialMessages: true, Resume: "abc-123",
		Settings:  map[string]any{"includeGitInstructions": false, "claudeMdExcludes": []string{"**/CLAUDE.md"}},
		DebugFile: "/tmp/cli.log",
		ExtraArgs: []ExtraArg{{Key: "model", Value: ptr("claude-opus-4-7")}, {Key: "replay-user-messages"},
			{Key: "strict-mcp-config"}, {Key: "thinking-display", Value: ptr("summarized")}},
	}
	want := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--effort=high", "--debug-file=/tmp/cli.log", "--resume=abc-123", "--tools=",
		"--permission-mode=bypassPermissions", "--include-partial-messages",
		"--model", "claude-opus-4-7", "--replay-user-messages", "--strict-mcp-config", "--thinking-display", "summarized",
		"--settings", `{"claudeMdExcludes":["**/CLAUDE.md"],"includeGitInstructions":false}`,
	}
	if got := BuildArgs(o); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// The minimal isolated flag set and its order. The full isolated argv, with --effort, --resume
// and --settings, is pinned by the Node SDK snapshot (isolated-summary).
func TestBuildArgsIsolatedShape(t *testing.T) {
	o := Options{Tools: nil, ToolsSet: true, NoSessionPersistence: true, StrictMcpConfig: true, Model: "m", MaxTurns: 2, SettingSourcesSet: true}
	want := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--max-turns=2", "--model=m", "--tools=", "--setting-sources=", "--strict-mcp-config", "--no-session-persistence"}
	if got := BuildArgs(o); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestBuildArgsEmptyToolsEmitsEqualsSign(t *testing.T) {
	got := BuildArgs(Options{ToolsSet: true, Tools: []string{}})
	if !slices.Contains(got, "--tools=") {
		t.Fatalf("no --tools= in %q", got)
	}
	if slices.Contains(BuildArgs(Options{}), "--tools=") {
		t.Fatal("--tools= emitted when ToolsSet is false")
	}
}

func TestExtraArgNilEmitsBareFlag(t *testing.T) {
	v, dash := "x", "-1"
	got := BuildArgs(Options{ExtraArgs: []ExtraArg{{Key: "replay-user-messages"}, {Key: "k", Value: &v}, {Key: "n", Value: &dash}}})
	want := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--replay-user-messages", "--k", "x", "--n=-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

// SDK MCP servers, the system prompt and skills travel in initialize; none adds an argv token.
func TestBuildArgsNeverEmitsMcpOrSystemPrompt(t *testing.T) {
	got := BuildArgs(Options{SDKMcpServers: []string{"pig"}, AppendSystemPrompt: "hi", SystemPrompt: "s", SystemPromptSet: true, Skills: []string{}})
	if want := BuildArgs(Options{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildEnv(t *testing.T) {
	base := []string{"PATH=/bin", "NODE_OPTIONS=--inspect", "DEBUG=*", "TRACEPARENT=t", "TRACESTATE=s", "HOME=/h"}
	env := BuildEnv(base, Options{})
	want := []string{"PATH=/bin", "HOME=/h",
		"CLAUDE_CODE_ENTRYPOINT=sdk-ts", "CLAUDE_AGENT_SDK_VERSION=0.3.310", "CLAUDE_CODE_SDK_READS_SESSION_STATE=1",
		"CLAUDE_CODE_DISABLE_FEATURE_FLAGS=1", "CLAUDE_CODE_ELEGANT_MEADOW=0", "CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH=16384",
		"CLAUDE_CODE_RESUME_INTERRUPTED_TURN_MAX_AGE_MS=1", "CLAUDE_CODE_SILENT_TURN_REMINDER=0",
		"DISABLE_AUTO_COMPACT=1", "ENABLE_CLAUDEAI_MCP_SERVERS=0",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "PI_CODING_AGENT=true"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("got %q\nwant %q", env, want)
	}
}

// When PiG was started from inside a Claude Code session (CLAUDECODE set), that session's markers
// are dropped so the SDK stamps our own entrypoint and version; without CLAUDECODE they pass through.
func TestBuildEnvDropsParentClaudeCodeSessionMarkers(t *testing.T) {
	parent := []string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=parent", "CLAUDE_ENV_FILE=/tmp/env", "CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_SSE_PORT=1234", "AI_AGENT=claude", "CLAUDE_CONFIG_DIR=/cfg"}
	env := BuildEnv(parent, Options{})
	for _, gone := range []string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=parent", "CLAUDE_ENV_FILE=/tmp/env", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_SSE_PORT=1234"} {
		if slices.Contains(env, gone) {
			t.Errorf("%s survived: %q", gone, env)
		}
	}
	for _, kept := range []string{"AI_AGENT=claude", "CLAUDE_CONFIG_DIR=/cfg", "CLAUDE_CODE_ENTRYPOINT=sdk-ts"} {
		if !slices.Contains(env, kept) {
			t.Errorf("missing %s in %q", kept, env)
		}
	}
	if env := BuildEnv([]string{"CLAUDE_CODE_SESSION_ID=other"}, Options{}); !slices.Contains(env, "CLAUDE_CODE_SESSION_ID=other") {
		t.Errorf("without CLAUDECODE the marker should pass through: %q", env)
	}
}

// thinking:{type:"disabled"} is --thinking=disabled, right after the fixed prefix and before --effort (sdk.mjs initialize()).
func TestBuildArgsThinkingDisabled(t *testing.T) {
	got := BuildArgs(Options{ThinkingDisabled: true, Effort: "low"})
	want := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--thinking=disabled", "--effort=low"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The TypeScript sets CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 on its own process.env at load
// (index.ts:2465), overwriting any inherited value, and every child inherits it.
func TestBuildEnvOverridesNonessentialTraffic(t *testing.T) {
	env := BuildEnv([]string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=0"}, Options{})
	if got := slices.Index(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"); got < slices.Index(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=0") {
		t.Fatalf("the inherited value wins: %q", env)
	}
}

// The SDK sets the entry point and version when their exact-case key is missing or empty, and
// the session-state flag when no key matches it in any case.
func TestBuildEnvChecksIdentityVariablesAsTheSdkDoes(t *testing.T) {
	env := BuildEnv([]string{"claude_code_entrypoint=custom", "CLAUDE_AGENT_SDK_VERSION="}, Options{})
	for _, w := range []string{"CLAUDE_CODE_ENTRYPOINT=sdk-ts", "CLAUDE_AGENT_SDK_VERSION=0.3.310", "CLAUDE_CODE_SDK_READS_SESSION_STATE=1"} {
		if !slices.Contains(env, w) {
			t.Errorf("missing %s in %q", w, env)
		}
	}
	env = BuildEnv([]string{"Claude_Code_Sdk_Reads_Session_State="}, Options{})
	if slices.Contains(env, "CLAUDE_CODE_SDK_READS_SESSION_STATE=1") {
		t.Errorf("a case variant of the session-state flag must keep the SDK from setting it: %q", env)
	}
}

func TestBuildEnvKeepsPresetIdentityAndChildEnvWins(t *testing.T) {
	base := []string{"CLAUDE_CODE_ENTRYPOINT=custom", "CLAUDE_AGENT_SDK_VERSION=9", "claude_code_sdk_reads_session_state=0", "DISABLE_AUTO_COMPACT=0"}
	env := BuildEnv(base, Options{})
	for _, w := range []string{"CLAUDE_CODE_ENTRYPOINT=custom", "CLAUDE_AGENT_SDK_VERSION=9", "claude_code_sdk_reads_session_state=0"} {
		if !slices.Contains(env, w) {
			t.Errorf("missing %s in %q", w, env)
		}
	}
	for _, unwanted := range []string{"CLAUDE_CODE_ENTRYPOINT=sdk-ts", "CLAUDE_AGENT_SDK_VERSION=0.3.310", "CLAUDE_CODE_SDK_READS_SESSION_STATE=1"} {
		if slices.Contains(env, unwanted) {
			t.Errorf("unexpected %s", unwanted)
		}
	}
	if slices.Index(env, "DISABLE_AUTO_COMPACT=1") < slices.Index(env, "DISABLE_AUTO_COMPACT=0") {
		t.Errorf("CCChildEnv value must come after the inherited duplicate: %q", env)
	}
}

func TestBuildInitializeOmitsSystemPromptForPreset(t *testing.T) {
	got := string(BuildInitialize(Options{SDKMcpServers: []string{"pig"}, AppendSystemPrompt: "be <brief>"}, "req_1"))
	want := `{"request_id":"req_1","type":"control_request","request":{"subtype":"initialize","sdkMcpServers":["pig"],"appendSystemPrompt":"be <brief>"}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	var m struct {
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal(BuildInitialize(Options{}, "r"), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"systemPrompt", "appendSystemPrompt", "hooks", "sdkMcpServers", "skills"} {
		if _, ok := m.Request[k]; ok {
			t.Errorf("%s present", k)
		}
	}
}

func TestBuildArgsDisallowedTools(t *testing.T) {
	got := BuildArgs(Options{DisallowedTools: []string{"Write", "Bash"}})
	if !slices.Contains(got, "--disallowedTools=Write,Bash") {
		t.Fatalf("no --disallowedTools in %q", got)
	}
	for _, a := range BuildArgs(Options{}) {
		if strings.HasPrefix(a, "--disallowedTools") {
			t.Fatal("--disallowedTools emitted when empty")
		}
	}
}
