package mcptools

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Claude Code drops an MCP tool whose input schema has a top-level
// anyOf/oneOf/allOf, so the server advertises a flat object instead, built the
// way Claude Code's own normalizer does: root properties win over branch ones,
// root `required` stays, allOf requirements join it, and `$ref`s into the
// root's `$defs`/`definitions` are followed.
var rootCombinators = []string{"anyOf", "oneOf", "allOf"}

var localRef = regexp.MustCompile(`^#/(\$defs|definitions)/([^/]+)$`)

// FlattenRootSchema returns schema with its root combinators flattened.
func FlattenRootSchema(schema map[string]any) map[string]any {
	flat, _ := flattenRootCombinators(schema)
	return flat
}

func asRecord(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asSlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []string:
		out := make([]any, len(s))
		for i, e := range s {
			out[i] = e
		}
		return out
	case []map[string]any:
		out := make([]any, len(s))
		for i, e := range s {
			out[i] = e
		}
		return out
	}
	return nil
}

func resolveLocalRef(branch any, root map[string]any) any {
	b, ok := asRecord(branch)
	if !ok {
		return branch
	}
	ref, ok := b["$ref"].(string)
	if !ok {
		return branch
	}
	match := localRef.FindStringSubmatch(ref)
	if match == nil {
		return branch
	}
	defs, _ := asRecord(root[match[1]])
	if target, ok := asRecord(defs[match[2]]); ok {
		return target
	}
	return branch
}

// branchGroup names a branch by its required keys, else by all of its keys.
// Go maps carry no key order, so property keys are sorted.
func branchGroup(branch any) (string, bool) {
	b, ok := asRecord(branch)
	if !ok {
		return "", false
	}
	if req := asSlice(b["required"]); len(req) > 0 {
		names := make([]string, 0, len(req))
		for _, r := range req {
			s, ok := r.(string)
			if !ok {
				names = nil
				break
			}
			names = append(names, s)
		}
		if names != nil {
			return strings.Join(names, ", "), true
		}
	}
	props, _ := asRecord(b["properties"])
	if len(props) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", "), true
}

func combinatorNote(combinators []string, branchesOf func(string) []any) string {
	has := func(key string) bool {
		for _, c := range combinators {
			if c == key {
				return true
			}
		}
		return false
	}
	if !has("anyOf") && !has("oneOf") {
		return "Input constraint: all listed parameters apply together (flattened from a JSON Schema allOf)."
	}
	key, rule := "anyOf", "Provide parameters for at least one of"
	if has("oneOf") {
		key, rule = "oneOf", "Provide parameters for exactly one of"
	}
	var groups []string
	seen := map[string]bool{}
	for _, branch := range branchesOf(key) {
		if g, ok := branchGroup(branch); ok && !seen[g] {
			seen[g] = true
			groups = append(groups, "("+g+")")
		}
	}
	if len(groups) == 0 {
		return fmt.Sprintf("Input constraint: %s the documented parameter groups (flattened from a JSON Schema %s).", rule, key)
	}
	return fmt.Sprintf("Input constraint: %s: %s.", rule, strings.Join(groups, " or "))
}

// flattenRootCombinators returns the flat schema and, when it flattened
// anything, the note restating the lost constraint.
func flattenRootCombinators(schema map[string]any) (map[string]any, string) {
	var combinators []string
	for _, key := range rootCombinators {
		if _, ok := schema[key]; ok {
			combinators = append(combinators, key)
		}
	}
	if len(combinators) == 0 {
		return schema, ""
	}
	branchesOf := func(key string) []any {
		branches := asSlice(schema[key])
		out := make([]any, len(branches))
		for i, b := range branches {
			out[i] = resolveLocalRef(b, schema)
		}
		return out
	}
	properties := map[string]any{}
	required := []string{}
	addProperties := func(source any) {
		src, ok := asRecord(source)
		if !ok {
			return
		}
		props, _ := asRecord(src["properties"])
		for k, v := range props {
			if _, dup := properties[k]; !dup {
				properties[k] = v
			}
		}
	}
	addRequired := func(source any) {
		src, ok := asRecord(source)
		if !ok {
			return
		}
		for _, r := range asSlice(src["required"]) {
			s, ok := r.(string)
			if !ok {
				continue
			}
			dup := false
			for _, have := range required {
				dup = dup || have == s
			}
			if !dup {
				required = append(required, s)
			}
		}
	}

	addProperties(schema)
	for _, key := range combinators {
		for _, branch := range branchesOf(key) {
			addProperties(branch)
		}
	}
	addRequired(schema)
	for _, branch := range branchesOf("allOf") {
		addRequired(branch)
	}

	flat := map[string]any{}
	for k, v := range schema {
		flat[k] = v
	}
	for _, key := range rootCombinators {
		delete(flat, key)
	}
	flat["type"] = "object"
	flat["properties"] = properties
	if len(required) > 0 {
		flat["required"] = required
	} else {
		delete(flat, "required")
	}
	return flat, combinatorNote(combinators, branchesOf)
}
