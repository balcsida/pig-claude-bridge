package askclaude

import (
	"strings"
	"sync"
	"unicode"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// Status-line helpers: while Claude Code runs inside a call, the TUI has one row for
// the whole delegation. They shape a tool_use into a short label ("Read(src/foo.ts)")
// and collapse runs of the same tool so the line does not flicker.

type ToolCallState struct {
	Name, Status string
	RawInput     any
}

// ToolCalls records tool calls in first-seen order; setting an id again replaces its state in place.
type ToolCalls struct {
	mu     sync.Mutex
	order  []string
	states map[string]ToolCallState
}

func (c *ToolCalls) Set(id string, s ToolCallState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.states == nil {
		c.states = map[string]ToolCallState{}
	}
	if _, seen := c.states[id]; !seen {
		c.order = append(c.order, id)
	}
	c.states[id] = s
}

func (c *ToolCalls) States() []ToolCallState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ToolCallState, len(c.order))
	for i, id := range c.order {
		out[i] = c.states[id]
	}
	return out
}

// MapToolName gives the pi name of a Claude Code tool: builtins are real here, the
// bridge's own MCP prefix is dropped.
func MapToolName(name string) string {
	normalized := strings.ToLower(name)
	switch normalized {
	case "read", "write", "edit", "bash":
		return normalized
	}
	if strings.HasPrefix(normalized, convert.MCPToolPrefix) {
		return name[len(convert.MCPToolPrefix):]
	}
	return name
}

// truncate keeps at most n characters.
func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func inputField(raw any, key string) (string, bool) {
	in, _ := raw.(map[string]any)
	s, ok := in[key].(string)
	return s, ok
}

func ExtractPath(raw any) (string, bool) {
	if s, ok := inputField(raw, "file_path"); ok {
		return s, true
	}
	if s, ok := inputField(raw, "path"); ok {
		return s, true
	}
	if s, ok := inputField(raw, "command"); ok {
		return truncate(s, 80), true
	}
	return "", false
}

// ShortPath is p relative to cwd, or its last two segments when it is a deep absolute path.
func ShortPath(cwd, p string) string {
	if strings.HasPrefix(p, cwd+"/") {
		return p[len(cwd)+1:]
	}
	if strings.HasPrefix(p, "/") {
		if parts := strings.Split(p, "/"); len(parts) > 3 {
			return strings.Join(parts[len(parts)-2:], "/")
		}
	}
	return p
}

// verb is the lowercased first whitespace-separated word of a tool name.
func verb(name string) string {
	lower := strings.ToLower(name)
	if i := strings.IndexFunc(lower, unicode.IsSpace); i >= 0 {
		return lower[:i]
	}
	return lower
}

// FormatToolAction is the label of one call; ok is false for calls the summary omits.
func FormatToolAction(cwd string, tc ToolCallState) (string, bool) {
	path, hasPath := ExtractPath(tc.RawInput)
	labelled := func(name, arg string) (string, bool) {
		if arg == "" {
			return name, true
		}
		return name + "(" + arg + ")", true
	}
	switch v := verb(tc.Name); v {
	case "read", "readfile":
		if hasPath {
			path = ShortPath(cwd, path)
		}
		return labelled("Read", path)
	case "glob":
		pat, _ := inputField(tc.RawInput, "pattern")
		return labelled("Glob", truncate(pat, 40))
	case "edit", "write", "writefile", "multiedit":
		if hasPath {
			path = ShortPath(cwd, path)
		}
		return labelled("Edit", path)
	case "bashoutput":
		return "", false // redundant with the preceding Bash call
	case "bash", "terminal":
		return labelled("Bash", path)
	case "agent":
		d, _ := inputField(tc.RawInput, "description")
		return "Agent(" + truncate(d, 40) + ")", true
	case "grep":
		pat, _ := inputField(tc.RawInput, "pattern")
		return labelled("Grep", truncate(pat, 40))
	case "skill":
		name, _ := inputField(tc.RawInput, "skill")
		return labelled("Skill", truncate(name, 40))
	case "todowrite", "taskcreate", "taskupdate":
		in, _ := tc.RawInput.(map[string]any)
		todos, _ := in["todos"].([]any)
		var current map[string]any
		for _, status := range []string{"in_progress", "pending"} {
			for _, t := range todos {
				if m, _ := t.(map[string]any); m["status"] == status {
					current = m
					break
				}
			}
			if current != nil {
				break
			}
		}
		label := ""
		if current != nil {
			label, _ = current["content"].(string)
		}
		label = truncate(label, 40)
		return label, label != ""
	case "askclaude":
		return "", false // recursive: not shown in its own summary
	}
	return tc.Name, true
}

// BuildActionSummary joins the actions of calls with "; ", keeping only the latest of consecutive calls to the same tool.
func BuildActionSummary(cwd string, calls []ToolCallState) string {
	var parts []string
	prev := ""
	for _, tc := range calls {
		action, ok := FormatToolAction(cwd, tc)
		if !ok || action == "" {
			continue
		}
		v := verb(tc.Name)
		if v == prev {
			parts[len(parts)-1] = action
		} else {
			parts = append(parts, action)
		}
		prev = v
	}
	return strings.Join(parts, "; ")
}
