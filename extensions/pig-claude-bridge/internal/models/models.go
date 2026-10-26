// Package models ports the TypeScript model policy: the picker projection of
// pi-ai's Anthropic catalog, family shortcut resolution, and the Claude Code
// 1M-context rules.
package models

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	twoHundredKContext = 200_000
	oneMContext        = 1_000_000
)

// LongContextSettings carries the plan facts that decide whether a [1m] id is served.
type LongContextSettings struct {
	Plan                  string // "pro" or "max"
	LongContextExtraUsage bool
	// ForceTwoHundredK lists model ids whose declared 1M context Claude Code
	// turned out not to serve; they stay on the bare id at 200K.
	ForceTwoHundredK []string
}

// RuntimeModel is the id sent to Claude Code and the context window it serves.
type RuntimeModel struct {
	CLIModelID    string
	ContextWindow int
}

// pi-ai ships dated snapshot ids (claude-opus-4-5-20251101, ...) alongside the
// bare ids. They are never exposed and must not steal first-partial-match
// shortcuts like "opus-4-5" from the bare id.
var datedAlias = regexp.MustCompile(`-20\d{6}$`)

// familyOrder is the display tier: flagship families first, unknown families last.
var familyOrder = []string{"fable", "opus", "sonnet", "haiku"}

// measuredOneM holds the [1m] ids verified to serve 1M on every plan. A new
// model serves 200K until someone measures it and adds it here. Known
// exception unrelated to long context: fable-5 is not included with a Pro
// account without Extra Usage.
var measuredOneM = map[string]bool{
	"claude-fable-5":    true,
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-opus-4-8":   true,
	"claude-opus-4-7":   true,
	"claude-sonnet-5":   true,
	"claude-sonnet-5-5": true,
	"claude-haiku-5-5":  true,
}

// planGatedOneM holds the measured exceptions: pi-ai declares 1M and the [1m]
// id works, but only when the plan allows it.
var planGatedOneM = map[string]func(LongContextSettings) bool{
	// [1m] measured 1M on Max plan / extra usage; 429 on Pro without it.
	"claude-opus-4-6": func(s LongContextSettings) bool { return s.Plan == "max" || s.LongContextExtraUsage },
	// [1m] measured 1M with extra usage only.
	"claude-sonnet-4-6": func(s LongContextSettings) bool { return s.LongContextExtraUsage },
}

// versionRank splits a claude id such as claude-opus-4-7 into its family and
// (major, minor); unparsable parts rank 0.
func versionRank(id string) (family string, major, minor int) {
	parts := strings.Split(id, "-")
	at := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	major, _ = strconv.Atoi(at(2))
	minor, _ = strconv.Atoi(at(3))
	return at(1), major, minor
}

func familyTier(family string) int {
	if i := slices.Index(familyOrder, family); i >= 0 {
		return i
	}
	return len(familyOrder)
}

// BuildModels projects pi-ai's model entries down to the fields the provider
// registration expects, newest generation first. Dated aliases are dropped. The
// result shares nothing with the input, which is usually the shared Catalog().
func BuildModels(anthropic []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(anthropic))
	for _, m := range anthropic {
		if id, ok := m["id"].(string); ok && !datedAlias.MatchString(id) {
			kept = append(kept, m)
		}
	}
	slices.SortStableFunc(kept, func(a, b map[string]any) int {
		ida, idb := a["id"].(string), b["id"].(string)
		fa, majA, minA := versionRank(ida)
		fb, majB, minB := versionRank(idb)
		return cmp.Or(
			cmp.Compare(familyTier(fa), familyTier(fb)),
			cmp.Compare(majB, majA),
			cmp.Compare(minB, minA),
			strings.Compare(ida, idb),
		)
	})
	out := make([]map[string]any, len(kept))
	for i, m := range kept {
		p := map[string]any{
			"provider": "claude-bridge",
			"cost":     map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
		}
		// thinkingLevelMap is forwarded so pi-ai's per-model overrides stay visible
		// to the effort lookup; a field absent upstream stays absent.
		for _, k := range []string{"id", "name", "reasoning", "input", "contextWindow", "maxTokens", "thinkingLevelMap"} {
			if v, ok := m[k]; ok {
				if f, isNumber := v.(float64); isNumber {
					v = int(f) // token counts; ApplyLongContext writes int too
				}
				p[k] = cloneJSON(v)
			}
		}
		out[i] = p
	}
	return out
}

// cloneJSON deep-copies a JSON-decoded value's maps and slices.
func cloneJSON(v any) any {
	switch v := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(v))
		for k, x := range v {
			m[k] = cloneJSON(x)
		}
		return m
	case []any:
		s := make([]any, len(v))
		for i, x := range v {
			s[i] = cloneJSON(x)
		}
		return s
	}
	return v
}

// ResolveClaudeCodeRuntimeModel applies the measured Claude Agent SDK policy:
// the [1m] suffix is the only reliable way to request 1M context, and an
// unentitled [1m] id is rejected outright, so only measured-good ids get it.
// The registered contextWindow must match the window requested here, or the
// status bar and compaction threshold misreport.
func ResolveClaudeCodeRuntimeModel(modelID string, s LongContextSettings) RuntimeModel {
	if slices.Contains(s.ForceTwoHundredK, modelID) {
		return RuntimeModel{modelID, twoHundredKContext}
	}
	if measuredOneM[modelID] {
		return RuntimeModel{modelID + "[1m]", oneMContext}
	}
	if gate, ok := planGatedOneM[modelID]; ok && gate(s) {
		return RuntimeModel{modelID + "[1m]", oneMContext}
	}
	return RuntimeModel{modelID, twoHundredKContext}
}

// ClaudeCodeModelID returns the id to request from Claude Code.
func ClaudeCodeModelID(modelID string, s LongContextSettings) string {
	return ResolveClaudeCodeRuntimeModel(modelID, s).CLIModelID
}

// ResolveModel matches input exactly first, then as a substring, preferring the
// newest version among partial matches regardless of registration order.
func ResolveModel(models []map[string]any, input string) (map[string]any, bool) {
	lower := strings.ToLower(input)
	for _, m := range models {
		if m["id"] == lower {
			return m, true
		}
	}
	var best map[string]any
	var bestMajor, bestMinor int
	for _, m := range models {
		id, _ := m["id"].(string)
		if !strings.Contains(id, lower) {
			continue
		}
		_, major, minor := versionRank(id)
		if best == nil || major > bestMajor || (major == bestMajor && minor > bestMinor) {
			best, bestMajor, bestMinor = m, major, minor
		}
	}
	return best, best != nil
}

// oneMLabel matches a name that already says 1M, so the suffix is not doubled.
var oneMLabel = regexp.MustCompile(`(?i)\b1M\b`)

// ApplyLongContext sets each model's contextWindow to the window the bridge
// requests and labels 1M models with a " 1M" name suffix. Input maps are not mutated.
func ApplyLongContext(models []map[string]any, s LongContextSettings) []map[string]any {
	out := make([]map[string]any, len(models))
	for i, m := range models {
		id, _ := m["id"].(string)
		label, _ := m["name"].(string)
		window := ResolveClaudeCodeRuntimeModel(id, s).ContextWindow
		if window > twoHundredKContext && !oneMLabel.MatchString(label) {
			label += " 1M"
		}
		p := make(map[string]any, len(m))
		for k, v := range m {
			p[k] = v
		}
		p["contextWindow"] = window
		p["name"] = label
		out[i] = p
	}
	return out
}
