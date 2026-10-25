package convert

import (
	"strings"
	"testing"
)

func skill(name string, disabled bool) map[string]any {
	return map[string]any{
		"name":                   name,
		"description":            name + " description",
		"filePath":               "/skills/" + name + "/SKILL.md",
		"baseDir":                "/skills/" + name,
		"disableModelInvocation": disabled,
	}
}

func TestSkillsBlockRendering(t *testing.T) {
	t.Run("formats skills and names the MCP read tool for provider queries", func(t *testing.T) {
		result, ok := RenderSkillsBlock([]map[string]any{skill("browser", false)}, "mcp")
		if !ok || !strings.HasPrefix(result, "The following skills") {
			t.Fatalf("got %q", result)
		}
		if !strings.Contains(result, "Use the read tool (mcp__custom-tools__read)") {
			t.Fatal("missing mcp read tool")
		}
		if !strings.Contains(result, "<location>/skills/browser/SKILL.md</location>") {
			t.Fatal("missing location")
		}
	})

	t.Run("keeps the native read-tool instruction for AskClaude", func(t *testing.T) {
		result, _ := RenderSkillsBlock([]map[string]any{skill("browser", false)}, "native")
		if !strings.Contains(result, "Use the read tool to load") || strings.Contains(result, "mcp__custom-tools__read") {
			t.Fatalf("got %q", result)
		}
	})

	t.Run("emits nothing without a usable reader or visible skills", func(t *testing.T) {
		for name, c := range map[string]struct {
			skills   []map[string]any
			readTool string
		}{
			"none reader":     {[]map[string]any{skill("browser", false)}, "none"},
			"no skills":       {nil, "mcp"},
			"only hidden one": {[]map[string]any{skill("hidden", true)}, "mcp"},
		} {
			if got, ok := RenderSkillsBlock(c.skills, c.readTool); ok || got != "" {
				t.Errorf("%s: got %q, %v", name, got, ok)
			}
		}
	})

	t.Run("uses Pi's XML escaping", func(t *testing.T) {
		escaped := skill("browser", false)
		escaped["description"] = "read <this> & that"
		result, _ := RenderSkillsBlock([]map[string]any{escaped}, "native")
		if !strings.Contains(result, "read &lt;this&gt; &amp; that") {
			t.Fatalf("got %q", result)
		}
	})

	t.Run("renders the exact block Pi's formatSkillsForPrompt produces", func(t *testing.T) {
		want := "The following skills provide specialized instructions for specific tasks.\n" +
			"Use the read tool to load a skill's file when the task matches its description.\n" +
			"When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.\n" +
			"\n<available_skills>\n  <skill>\n    <name>a&quot;b</name>\n    <description>it&apos;s</description>\n    <location>/x</location>\n  </skill>\n</available_skills>"
		s := map[string]any{"name": `a"b`, "description": "it's", "filePath": "/x"}
		if got, _ := RenderSkillsBlock([]map[string]any{s}, "native"); got != want {
			t.Fatalf("got %q", got)
		}
	})
}
