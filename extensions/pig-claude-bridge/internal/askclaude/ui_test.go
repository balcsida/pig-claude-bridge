package askclaude

import (
	"strings"
	"testing"
)

func call(name string, input map[string]any) ToolCallState {
	return ToolCallState{Name: name, Status: "complete", RawInput: input}
}

func TestBuildActionSummary(t *testing.T) {
	const cwd = "/work/proj"
	for _, c := range []struct {
		name  string
		calls []ToolCallState
		want  string
	}{
		{"path-aware labels", []ToolCallState{
			call("read", map[string]any{"file_path": "/work/proj/src/foo.ts"}),
			call("grep", map[string]any{"pattern": "TODO"}),
			call("bash", map[string]any{"command": "git log --oneline"}),
		}, "Read(src/foo.ts); Grep(TODO); Bash(git log --oneline)"},
		{"consecutive calls to the same tool keep only the latest", []ToolCallState{
			call("read", map[string]any{"file_path": "/work/proj/a.go"}),
			call("read", map[string]any{"file_path": "/work/proj/b.go"}),
			call("glob", map[string]any{"pattern": "**/*.go"}),
			call("read", map[string]any{"file_path": "/work/proj/c.go"}),
		}, "Read(b.go); Glob(**/*.go); Read(c.go)"},
		{"BashOutput is redundant with the preceding Bash call", []ToolCallState{
			call("bash", map[string]any{"command": "make"}), call("BashOutput", nil),
		}, "Bash(make)"},
		{"AskClaude is not shown in its own summary", []ToolCallState{call("AskClaude", nil), call("read", map[string]any{"path": "x.md"})}, "Read(x.md)"},
		{"write, edit and multiedit are Edit", []ToolCallState{call("write", map[string]any{"file_path": "/work/proj/a"}), call("multiedit", nil)}, "Edit(a); Edit"},
		{"long commands are cut to 80 characters", []ToolCallState{
			call("bash", map[string]any{"command": "echo " + strings.Repeat("x", 100)}),
		}, "Bash(echo " + strings.Repeat("x", 75) + ")"},
		{"a deep absolute path outside cwd keeps its last two segments", []ToolCallState{call("read", map[string]any{"file_path": "/a/b/c/d.txt"})}, "Read(c/d.txt)"},
		{"agent, skill and unknown tools", []ToolCallState{
			call("agent", map[string]any{"description": "find callers"}), call("skill", map[string]any{"skill": "review"}), call("WebFetch", nil),
		}, "Agent(find callers); Skill(review); WebFetch"},
		{"todos show the task in progress, else the first pending", []ToolCallState{
			call("TodoWrite", map[string]any{"todos": []any{
				map[string]any{"status": "pending", "content": "later"}, map[string]any{"status": "in_progress", "content": "now"},
			}}),
		}, "now"},
		{"todos with nothing active show nothing", []ToolCallState{call("TodoWrite", map[string]any{"todos": []any{map[string]any{"status": "completed", "content": "done"}}})}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := BuildActionSummary(cwd, c.calls); got != c.want {
				t.Fatalf("summary = %q, want %q", got, c.want)
			}
		})
	}
}

func TestToolCallsKeepFirstSeenOrder(t *testing.T) {
	var calls ToolCalls
	calls.Set("1", ToolCallState{Name: "read", Status: "running"})
	calls.Set("2", ToolCallState{Name: "glob", Status: "running"})
	calls.Set("1", ToolCallState{Name: "read", Status: "complete"})
	got := calls.States()
	if len(got) != 2 || got[0].Name != "read" || got[0].Status != "complete" || got[1].Name != "glob" {
		t.Fatalf("states = %+v", got)
	}
}

func TestMapToolName(t *testing.T) {
	for in, want := range map[string]string{"Read": "read", "BASH": "bash", "mcp__custom-tools__grep": "grep", "WebFetch": "WebFetch"} {
		if got := MapToolName(in); got != want {
			t.Fatalf("MapToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
