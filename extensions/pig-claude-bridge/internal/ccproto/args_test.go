package ccproto

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestBuildArgsProviderShape(t *testing.T) {
	o := Options{
		Effort: "high", Tools: []string{}, ToolsSet: true, PermissionMode: "bypassPermissions",
		IncludePartialMessages: true, Resume: "abc-123", Model: "claude-opus-4-7",
		ReplayUserMessages: true, StrictMcpConfig: true, ThinkingDisplay: "summarized",
		Settings:  map[string]any{"includeGitInstructions": false, "claudeMdExcludes": []string{"**/CLAUDE.md"}},
		DebugFile: "/tmp/cli.log",
	}
	want := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--effort=high", "--tools=", "--permission-mode=bypassPermissions", "--include-partial-messages",
		"--resume=abc-123", "--model=claude-opus-4-7", "--replay-user-messages", "--strict-mcp-config",
		"--thinking-display=summarized",
		"--settings", `{"claudeMdExcludes":["**/CLAUDE.md"],"includeGitInstructions":false}`,
		"--debug-file=/tmp/cli.log",
	}
	if got := BuildArgs(o); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestBuildArgsIsolatedShape(t *testing.T) {
	o := Options{Tools: nil, ToolsSet: true, NoSessionPersistence: true, StrictMcpConfig: true, Model: "m"}
	want := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--tools=", "--model=m", "--strict-mcp-config", "--no-session-persistence"}
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

func TestBuildArgsNeverEmitsMcpOrSystemPrompt(t *testing.T) {
	got := BuildArgs(Options{SDKMcpServers: []string{"pig"}, AppendSystemPrompt: "hi"})
	for _, a := range got {
		switch a {
		case "--mcp-config", "--append-system-prompt", "--system-prompt":
			t.Fatalf("unexpected %s", a)
		}
		if len(a) >= 22 && a[:22] == "--permission-prompt-to" {
			t.Fatalf("unexpected %s", a)
		}
	}
}

func TestBuildEnv(t *testing.T) {
	base := []string{"PATH=/bin", "NODE_OPTIONS=--inspect", "DEBUG=*", "TRACEPARENT=t", "TRACESTATE=s", "HOME=/h"}
	env := BuildEnv(base, Options{})
	want := []string{"PATH=/bin", "HOME=/h",
		"CLAUDE_CODE_ENTRYPOINT=sdk-ts", "CLAUDE_AGENT_SDK_VERSION=0.3.310", "CLAUDE_CODE_SDK_READS_SESSION_STATE=1",
		"CLAUDE_CODE_ELEGANT_MEADOW=0", "DISABLE_AUTO_COMPACT=1", "ENABLE_CLAUDEAI_MCP_SERVERS=0", "PI_CODING_AGENT=true"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("got %q\nwant %q", env, want)
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
	want := `{"type":"control_request","request_id":"req_1","request":{"subtype":"initialize","sdkMcpServers":["pig"],"appendSystemPrompt":"be <brief>"}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	var m struct {
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal(BuildInitialize(Options{}, "r"), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"systemPrompt", "appendSystemPrompt", "hooks"} {
		if _, ok := m.Request[k]; ok {
			t.Errorf("%s present", k)
		}
	}
	if !reflect.DeepEqual(m.Request["sdkMcpServers"], []any{}) {
		t.Errorf("sdkMcpServers = %v", m.Request["sdkMcpServers"])
	}
}
