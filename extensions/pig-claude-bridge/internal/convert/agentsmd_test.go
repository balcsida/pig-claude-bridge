package convert

import (
	"strings"
	"testing"
)

func TestProjectContextForwarding(t *testing.T) {
	t.Run("preserves Pi's file order and labels each with its path", func(t *testing.T) {
		prompt, ok := FormatProjectContext([]map[string]any{
			{"path": "/agent/AGENTS.md", "content": "global instructions"},
			{"path": "/project/CLAUDE.md", "content": "parent instructions"},
			{"path": "/project/nested/AGENTS.md", "content": "nested instructions"},
		})
		if !ok {
			t.Fatal("want a prompt")
		}
		if !strings.HasPrefix(prompt, "<project_context>\n\nProject-specific instructions and guidelines:") {
			t.Fatalf("prefix wrong: %q", prompt)
		}
		g, p, n := strings.Index(prompt, "global instructions"), strings.Index(prompt, "parent instructions"), strings.Index(prompt, "nested instructions")
		if g >= p || p >= n {
			t.Fatalf("order wrong: %d %d %d", g, p, n)
		}
		if !strings.Contains(prompt, `<project_instructions path="/project/CLAUDE.md">`) {
			t.Fatal("missing path label")
		}
		if !strings.HasSuffix(prompt, "</project_instructions>\n\n</project_context>") {
			t.Fatalf("suffix wrong: %q", prompt)
		}
	})

	t.Run("returns undefined when Pi finds no context files", func(t *testing.T) {
		if _, ok := FormatProjectContext(nil); ok {
			t.Fatal("want no prompt")
		}
	})
}
