package convert

import (
	"sort"
	"strings"
)

// sectionRank is pi's canonical built-in section order. A name absent from it is
// a custom section, which pi's builder renders after every built-in. Known
// limitation, as in the TypeScript: customs keep their replayed relative order,
// while the builder takes theirs from systemPromptOptions.sections. Decoded JSON
// maps carry no key order, so customs introduced by one patch sort by name.
var sectionRank = map[string]int{
	"preamble": 0, "tools": 1, "rules": 2, "docs": 3, "addendum": 4,
	"project_context": 5, "skills": 6, "cwd": 7,
}

const customSectionRank = 8

// orderedMap is a JavaScript Map: re-setting a key keeps its position.
type orderedMap[V any] struct {
	keys   []string
	values map[string]V
}

func newOrderedMap[V any]() *orderedMap[V] { return &orderedMap[V]{values: map[string]V{}} }

func (m *orderedMap[V]) set(key string, v V) {
	if _, ok := m.values[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.values[key] = v
}

func (m *orderedMap[V]) delete(key string) {
	if _, ok := m.values[key]; !ok {
		return
	}
	delete(m.values, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			return
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// contentText joins the text blocks of a system message's content.
func contentText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, _ := flatBlocks(content)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func messageList(messages any) []map[string]any {
	var out []map[string]any
	switch ms := messages.(type) {
	case []map[string]any:
		return ms
	case []any:
		for _, m := range ms {
			if mm, ok := m.(map[string]any); ok {
				out = append(out, mm)
			}
		}
	}
	return out
}

func toolName(tool any) string {
	m, _ := tool.(map[string]any)
	return str(m["name"])
}

// currentTools resolves the tool set after applying every system-message delta in order.
func currentTools(messages []map[string]any) []any {
	tools := newOrderedMap[any]()
	for _, m := range messages {
		if m["role"] != "system" {
			continue
		}
		removed, _ := m["toolsRemoved"].([]any)
		for _, t := range removed {
			tools.delete(toolName(t))
		}
		added, _ := m["toolsAdded"].([]any)
		for _, t := range added {
			tools.set(toolName(t), t)
		}
	}
	out := make([]any, len(tools.keys))
	for i, k := range tools.keys {
		out[i] = tools.values[k]
	}
	return out
}

// canonicalSystemPrompt replays the system messages and renders them in pi's
// section order, or reports false when there is nothing to render.
func canonicalSystemPrompt(messages []map[string]any, toolCount int) (string, bool) {
	var content []string
	sections := newOrderedMap[string]()
	seenTimestamp := false
	for _, m := range messages {
		if m["role"] != "system" {
			continue
		}
		seenTimestamp = seenTimestamp || m["timestamp"] != nil
		if text := contentText(m["content"]); text != "" {
			content = append(content, text)
		}
		patch, _ := m["sections"].(map[string]any)
		for _, name := range sortedKeys(patch) {
			if patch[name] == nil {
				sections.delete(name)
			} else if s, ok := patch[name].(string); ok {
				sections.set(name, s)
			}
		}
	}
	if !seenTimestamp && toolCount == 0 {
		return "", false
	}
	names := append([]string(nil), sections.keys...)
	rank := func(name string) int {
		if r, ok := sectionRank[name]; ok {
			return r
		}
		return customSectionRank
	}
	sort.SliceStable(names, func(i, j int) bool { return rank(names[i]) < rank(names[j]) })
	var parts []string
	if text := strings.Join(content, "\n\n"); text != "" {
		parts = append(parts, text)
	}
	for _, n := range names {
		if s := sections.values[n]; s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n\n"), true
}

// ToBridgeContext restores the systemPrompt and tools fields downstream
// consumers expect from PiG's transcript-shaped context, where the tool set
// rides in the system messages, and removes those messages from the history,
// which then comes back as []map[string]any. A context without system messages
// is returned as is (the TypeScript's no-op), so its messages keep the slice type
// they arrived in; readers accept both.
func ToBridgeContext(context map[string]any) map[string]any {
	messages := messageList(context["messages"])
	hasSystem := false
	for _, m := range messages {
		hasSystem = hasSystem || m["role"] == "system"
	}
	if !hasSystem {
		return context
	}
	out := make(map[string]any, len(context)+2)
	for k, v := range context {
		out[k] = v
	}
	delete(out, "systemPrompt")
	delete(out, "tools")
	tools := currentTools(messages)
	if prompt, ok := canonicalSystemPrompt(messages, len(tools)); ok {
		out["systemPrompt"] = prompt
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	out["messages"] = NonSystemMessages(asAnySlice(messages))
	return out
}

func asAnySlice(messages []map[string]any) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i] = m
	}
	return out
}

// NonSystemMessages is messages with every prompt-state system message removed.
func NonSystemMessages(messages []any) []map[string]any {
	out := []map[string]any{}
	for _, m := range messages {
		if mm, ok := m.(map[string]any); ok && mm["role"] != "system" {
			out = append(out, mm)
		}
	}
	return out
}
