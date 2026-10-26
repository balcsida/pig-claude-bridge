package ccproto

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type snapshotEntry struct {
	Name       string            `json:"name"`
	Argv       []string          `json:"argv"`
	Env        map[string]string `json:"env"`
	Initialize json.RawMessage   `json:"initialize"`
}

var providerSnapshotOptions = Options{
	Effort: "high", Tools: []string{}, ToolsSet: true, PermissionMode: "bypassPermissions",
	IncludePartialMessages: true,
	Settings: map[string]any{
		"autoMemoryEnabled": false, "includeGitInstructions": false,
		"claudeMdExcludes": []string{"**/CLAUDE.md", "**/AGENTS.md", "**/.claude/rules/**"},
	},
	SDKMcpServers: []string{"custom-tools"}, AppendSystemPrompt: "APPEND",
	ExtraArgs: []ExtraArg{
		{Key: "model", Value: ptr("claude-opus-5-5")}, {Key: "replay-user-messages"},
		{Key: "strict-mcp-config"}, {Key: "thinking-display", Value: ptr("summarized")},
	},
}

// withDebugFile is o as CLAUDE_BRIDGE_DEBUG=1 runs it; the recorder replaces the file path with <debug-file>.
func withDebugFile(o Options) Options {
	o.DebugFile = "<debug-file>"
	return o
}

// snapshotOptions are the Options equivalent to the options record-sdk-argv.mjs gave the Node SDK.
var snapshotOptions = map[string]Options{
	"provider":       providerSnapshotOptions,
	"provider-debug": withDebugFile(providerSnapshotOptions),
	"isolated-summary": {
		Model: "claude-opus-5-5", Effort: "high", MaxTurns: 1, Resume: "11111111-1111-1111-1111-111111111111",
		Tools: []string{}, ToolsSet: true, SettingSourcesSet: true, StrictMcpConfig: true, NoSessionPersistence: true,
		Settings:     map[string]any{"autoMemoryEnabled": false},
		SystemPrompt: "SYSTEM", SystemPromptSet: true, Skills: []string{},
	},
}

func ptr(s string) *string { return &s }

// TestArgvMatchesNodeSdkSnapshot compares argv, filtered env and the initialize frame byte for byte, except that
// the --settings value is compared after parsing: TypeScript emits keys in insertion order, Go sorts them.
func TestArgvMatchesNodeSdkSnapshot(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-argv-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []snapshotEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(snapshotOptions) {
		t.Fatalf("snapshot has %d entries, test knows %d", len(entries), len(snapshotOptions))
	}
	for _, e := range entries {
		t.Run(e.Name, func(t *testing.T) {
			o, ok := snapshotOptions[e.Name]
			if !ok {
				t.Fatalf("no options for snapshot entry %q", e.Name)
			}
			gotArgs := BuildArgs(o)
			if len(gotArgs) != len(e.Argv) {
				t.Fatalf("argv\n got  %q\n want %q", gotArgs, e.Argv)
			}
			for i, want := range e.Argv {
				if i > 0 && e.Argv[i-1] == "--settings" {
					if !equalJSON(t, gotArgs[i], want) {
						t.Errorf("--settings: got %s want %s", gotArgs[i], want)
					}
				} else if gotArgs[i] != want {
					t.Errorf("argv[%d] = %q, want %q", i, gotArgs[i], want)
				}
			}

			gotEnv := map[string]string{}
			for _, kv := range BuildEnv(nil, o) {
				k, v, _ := strings.Cut(kv, "=")
				gotEnv[k] = v
			}
			// The recorder builds env without process.env, where the TypeScript bridge sets this one.
			delete(gotEnv, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC")
			if !reflect.DeepEqual(gotEnv, e.Env) {
				t.Errorf("env\n got  %v\n want %v", gotEnv, e.Env)
			}

			var want bytes.Buffer
			if err := json.Compact(&want, e.Initialize); err != nil {
				t.Fatal(err)
			}
			if got := BuildInitialize(o, "<id>"); !bytes.Equal(got, want.Bytes()) {
				t.Errorf("initialize\n got  %s\n want %s", got, want.Bytes())
			}
		})
	}
}

func equalJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}
