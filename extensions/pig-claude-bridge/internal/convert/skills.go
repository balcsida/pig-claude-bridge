package convert

import "strings"

const (
	MCPServerName = "custom-tools"
	MCPToolPrefix = "mcp__" + MCPServerName + "__"
)

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// RenderSkillsBlock renders the skills Pi would put in its prompt. readTool is
// "mcp", "native" or "none"; "none" and a list with no visible skill render nothing.
func RenderSkillsBlock(skills []map[string]any, readTool string) (string, bool) {
	if readTool == "none" || len(skills) == 0 {
		return "", false
	}
	block := strings.TrimSpace(formatSkillsForPrompt(skills))
	if block == "" {
		return "", false
	}
	if readTool == "mcp" {
		return RewriteSkillsBlock(block), true
	}
	return block, true
}

// RewriteSkillsBlock names the MCP read tool in a rendered skills block.
func RewriteSkillsBlock(block string) string {
	return strings.Replace(block,
		"Use the read tool to load a skill's file",
		"Use the read tool ("+MCPToolPrefix+"read) to load a skill's file", 1)
}

// formatSkillsForPrompt is pi-coding-agent's formatSkillsForPrompt with the
// default "read" file reader (dist/core/skills.js).
func formatSkillsForPrompt(skills []map[string]any) string {
	var visible []map[string]any
	for _, s := range skills {
		if hidden, _ := s["disableModelInvocation"].(bool); !hidden {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return ""
	}
	lines := []string{
		"\n\nThe following skills provide specialized instructions for specific tasks.",
		"Use the read tool to load a skill's file when the task matches its description.",
		"When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.",
		"",
		"<available_skills>",
	}
	for _, s := range visible {
		lines = append(lines,
			"  <skill>",
			"    <name>"+xmlEscaper.Replace(str(s["name"]))+"</name>",
			"    <description>"+xmlEscaper.Replace(str(s["description"]))+"</description>",
			"    <location>"+xmlEscaper.Replace(str(s["filePath"]))+"</location>",
			"  </skill>")
	}
	return strings.Join(append(lines, "</available_skills>"), "\n")
}
