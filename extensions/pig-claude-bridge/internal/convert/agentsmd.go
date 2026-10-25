package convert

import "strings"

// FormatProjectContext formats the context files Pi loaded, in Pi's order, so
// Claude receives the same instructions Pi applies.
func FormatProjectContext(files []map[string]any) (string, bool) {
	if len(files) == 0 {
		return "", false
	}
	var prompt strings.Builder
	prompt.WriteString("<project_context>\n\nProject-specific instructions and guidelines:\n\n")
	for _, f := range files {
		prompt.WriteString(`<project_instructions path="` + str(f["path"]) + "\">\n" + str(f["content"]) + "\n</project_instructions>\n\n")
	}
	prompt.WriteString("</project_context>")
	return prompt.String(), true
}
