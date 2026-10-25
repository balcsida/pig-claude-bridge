package bridge

import (
	"sort"
	"strings"
)

// piToolNameFor maps a Claude Code tool name to pi's, trying the lower-cased name
// second. ok is false for a tool the bridge does not serve.
func piToolNameFor(name string, customToolNameToPi map[string]string) (string, bool) {
	if pi, ok := customToolNameToPi[name]; ok {
		return pi, true
	}
	pi, ok := customToolNameToPi[strings.ToLower(name)]
	return pi, ok
}

// sdkKeyRenames are Claude Code parameter names that differ from pi's native ones.
// Keys not listed pass through unchanged, so new pi params work automatically.
var sdkKeyRenames = map[string]map[string]string{
	"read":  {"file_path": "path"},
	"write": {"file_path": "path"},
	"edit":  {"file_path": "path", "old_string": "oldText", "new_string": "newText", "old_text": "oldText", "new_text": "newText"},
}

// mapToolArgs renames SDK tool args to pi's. Pi's own prepareArguments hooks handle
// structural transforms. When two keys rename to the same name the first one wins,
// in the order of the sorted keys (Go maps carry no insertion order).
func mapToolArgs(toolName string, args map[string]any) map[string]any {
	renames := sdkKeyRenames[strings.ToLower(toolName)]
	result := make(map[string]any, len(args))
	// Originals first, so a native key beats an alias that renames onto it.
	for key, value := range args {
		if _, aliased := renames[key]; !aliased {
			result[key] = value
		}
	}
	for _, key := range sortedKeys(args) {
		if piKey, aliased := renames[key]; aliased {
			if _, taken := result[piKey]; !taken {
				result[piKey] = args[key]
			}
		}
	}
	// Pi bash has no default timeout; add a safety default.
	if strings.ToLower(toolName) == "bash" && result["timeout"] == nil {
		result["timeout"] = 120
	}
	return result
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
