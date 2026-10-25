package models

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

var (
	pro     = LongContextSettings{Plan: "pro"}
	maxPlan = LongContextSettings{Plan: "max"}
	extra   = LongContextSettings{Plan: "pro", LongContextExtraUsage: true}
)

// mockModel mimics a pi-ai registry entry, including fields the projection must not forward.
func mockModel(id string, extraFields map[string]any) map[string]any {
	m := map[string]any{
		"id": id, "name": id, "reasoning": true, "input": []any{"text"},
		"cost":          map[string]any{"input": 1.0, "output": 1.0},
		"contextWindow": 200000, "maxTokens": 8000,
		"baseUrl": "https://api.anthropic.com", "api": "anthropic", "provider": "anthropic",
		"headers": map[string]any{"x-api-key": "LEAK"},
	}
	for k, v := range extraFields {
		m[k] = v
	}
	return m
}

func oneM(id string) map[string]any { return mockModel(id, map[string]any{"contextWindow": 1000000}) }

func find(models []map[string]any, id string) map[string]any {
	for _, m := range models {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

func ids(models []map[string]any) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i], _ = m["id"].(string)
	}
	return out
}

// window reads contextWindow whether it holds an int or a JSON-decoded float64.
func window(t *testing.T, m map[string]any) int {
	t.Helper()
	switch v := m["contextWindow"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	t.Fatalf("contextWindow of %v is not a number", m["id"])
	return 0
}

func name(m map[string]any) string { s, _ := m["name"].(string); return s }

func requireRuntime(t *testing.T, id string, s LongContextSettings, want RuntimeModel) {
	t.Helper()
	if got := ResolveClaudeCodeRuntimeModel(id, s); got != want {
		t.Errorf("ResolveClaudeCodeRuntimeModel(%q, %+v) = %+v, want %+v", id, s, got, want)
	}
}

func resolvedID(models []map[string]any, input string) string {
	m, ok := ResolveModel(models, input)
	if !ok {
		return ""
	}
	return m["id"].(string)
}

func TestModelsProjection(t *testing.T) {
	t.Run("driven by pi-ai's real anthropic catalog, minus dated snapshot aliases", func(t *testing.T) {
		models := BuildModels(Catalog())
		for _, m := range models {
			id := m["id"].(string)
			if datedAlias.MatchString(id) {
				t.Errorf("dated snapshot id %q in the picker", id)
			}
			for _, leaky := range []string{"baseUrl", "api", "headers"} {
				if _, ok := m[leaky]; ok {
					t.Errorf("%s forwarded for %s", leaky, id)
				}
			}
			if m["provider"] != "claude-bridge" {
				t.Errorf("provider of %s = %v, want claude-bridge", id, m["provider"])
			}
			wantCost := map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}
			if !reflect.DeepEqual(m["cost"], wantCost) {
				t.Errorf("cost of %s = %v, want zeroed", id, m["cost"])
			}
		}
		for _, id := range []string{"claude-opus-5", "claude-fable-5-1", "claude-haiku-4-5"} {
			if find(models, id) == nil {
				t.Errorf("%s missing", id)
			}
		}
	})

	t.Run("sorts newest generation first within each family", func(t *testing.T) {
		models := BuildModels([]map[string]any{
			oneM("claude-opus-4-7"), oneM("claude-opus-5"), oneM("claude-sonnet-5"), oneM("claude-opus-4-6"),
		})
		want := []string{"claude-opus-5", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-5"}
		if got := ids(models); !slices.Equal(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	})

	t.Run("keeps dated aliases from stealing shortcuts from bare ids", func(t *testing.T) {
		models := BuildModels([]map[string]any{mockModel("claude-opus-4-5-20251101", nil), mockModel("claude-opus-4-5", nil)})
		if got := ids(models); !slices.Equal(got, []string{"claude-opus-4-5"}) {
			t.Errorf("ids = %v", got)
		}
		if got := resolvedID(models, "opus-4-5"); got != "claude-opus-4-5" {
			t.Errorf("opus-4-5 resolved to %q", got)
		}
	})

	t.Run("sinks unknown families below the known shortcut families", func(t *testing.T) {
		models := BuildModels([]map[string]any{oneM("claude-proxy-x"), oneM("claude-opus-5")})
		if got := ids(models); !slices.Equal(got, []string{"claude-opus-5", "claude-proxy-x"}) {
			t.Errorf("ids = %v", got)
		}
	})

	t.Run("forwards pi-ai's thinkingLevelMap verbatim", func(t *testing.T) {
		levels := map[string]any{"xhigh": "xhigh", "max": "max"}
		models := BuildModels([]map[string]any{mockModel("claude-sonnet-5", map[string]any{"thinkingLevelMap": levels})})
		if got := find(models, "claude-sonnet-5")["thinkingLevelMap"]; !reflect.DeepEqual(got, levels) {
			t.Errorf("thinkingLevelMap = %v", got)
		}
	})

	t.Run("forwards undefined thinkingLevelMap unchanged (no fabricated defaults)", func(t *testing.T) {
		models := BuildModels([]map[string]any{mockModel("claude-haiku-4-5", nil)})
		if v, ok := find(models, "claude-haiku-4-5")["thinkingLevelMap"]; ok {
			t.Errorf("thinkingLevelMap = %v, want absent", v)
		}
	})
}

func TestResolveModel(t *testing.T) {
	models := BuildModels(Catalog())

	t.Run("opus shortcut resolves to the newest opus in the catalog", func(t *testing.T) {
		var newest string
		for _, m := range models {
			if strings.Contains(m["id"].(string), "opus") {
				newest = m["id"].(string)
				break
			}
		}
		if newest == "" {
			t.Fatal("catalog contains no opus model")
		}
		if got := resolvedID(models, "opus"); got != newest {
			t.Errorf("opus resolved to %q, want %q", got, newest)
		}
		reversed := slices.Clone(models)
		slices.Reverse(reversed)
		if got := resolvedID(reversed, "opus"); got != newest {
			t.Errorf("reversed opus resolved to %q, want %q", got, newest)
		}
	})

	t.Run("exact id beats newer partial match (claude-fable-5 → fable-5, not 5-1)", func(t *testing.T) {
		if got := resolvedID(models, "claude-fable-5"); got != "claude-fable-5" {
			t.Errorf("resolved to %q", got)
		}
	})
}

func TestClaudeCodeRuntimePolicy(t *testing.T) {
	t.Run("measured-1M ids send [1m] on every plan", func(t *testing.T) {
		for _, id := range []string{"claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-fable-5", "claude-fable-5-1", "claude-sonnet-5-5", "claude-sonnet-5", "claude-haiku-5-5"} {
			requireRuntime(t, id, pro, RuntimeModel{id + "[1m]", 1000000})
		}
	})

	t.Run("unmeasured ids serve bare at 200K even when pi-ai declares 1M (sonnet-4-5)", func(t *testing.T) {
		requireRuntime(t, "claude-sonnet-4-5", pro, RuntimeModel{"claude-sonnet-4-5", 200000})
	})

	t.Run("declared 200K maps to bare id", func(t *testing.T) {
		requireRuntime(t, "claude-haiku-4-5", pro, RuntimeModel{"claude-haiku-4-5", 200000})
	})

	t.Run("measured exception: opus-4-6 1M is plan-gated", func(t *testing.T) {
		requireRuntime(t, "claude-opus-4-6", pro, RuntimeModel{"claude-opus-4-6", 200000})
		requireRuntime(t, "claude-opus-4-6", maxPlan, RuntimeModel{"claude-opus-4-6[1m]", 1000000})
		requireRuntime(t, "claude-opus-4-6", extra, RuntimeModel{"claude-opus-4-6[1m]", 1000000})
	})

	t.Run("measured exception: sonnet-4-6 1M requires extra usage", func(t *testing.T) {
		requireRuntime(t, "claude-sonnet-4-6", pro, RuntimeModel{"claude-sonnet-4-6", 200000})
		requireRuntime(t, "claude-sonnet-4-6", extra, RuntimeModel{"claude-sonnet-4-6[1m]", 1000000})
	})

	t.Run("forceTwoHundredK overrides an optimistic 1M declaration", func(t *testing.T) {
		s := LongContextSettings{Plan: "pro", ForceTwoHundredK: []string{"claude-future-9"}}
		requireRuntime(t, "claude-future-9", s, RuntimeModel{"claude-future-9", 200000})
	})

	t.Run("unknown model falls back to bare id at 200K", func(t *testing.T) {
		requireRuntime(t, "claude-future-9-9", pro, RuntimeModel{"claude-future-9-9", 200000})
	})
}

func TestClaudeCodeModelID(t *testing.T) {
	t.Run("returns the measured SDK request id", func(t *testing.T) {
		for id, want := range map[string]string{
			"claude-opus-5":    "claude-opus-5[1m]",
			"claude-opus-4-7":  "claude-opus-4-7[1m]",
			"claude-haiku-4-5": "claude-haiku-4-5",
		} {
			if got := ClaudeCodeModelID(id, pro); got != want {
				t.Errorf("ClaudeCodeModelID(%q) = %q, want %q", id, got, want)
			}
		}
	})
}

func TestApplyLongContext(t *testing.T) {
	models := BuildModels(Catalog())

	t.Run("registers 1M for measured-1M models", func(t *testing.T) {
		registered := ApplyLongContext(models, pro)
		for _, id := range []string{"claude-opus-5", "claude-opus-4-7", "claude-fable-5-1"} {
			if got := window(t, find(registered, id)); got != 1000000 {
				t.Errorf("%s window = %d", id, got)
			}
		}
	})

	t.Run("leaves unmeasured sonnet-4-5 at 200K", func(t *testing.T) {
		if got := window(t, find(ApplyLongContext(models, pro), "claude-sonnet-4-5")); got != 200000 {
			t.Errorf("window = %d", got)
		}
	})

	t.Run("haiku shortcut and no-match still resolve", func(t *testing.T) {
		if got := resolvedID(models, "haiku"); got != "claude-haiku-5-5" {
			t.Errorf("haiku resolved to %q", got)
		}
		if _, ok := ResolveModel(models, "gpt-9"); ok {
			t.Error("gpt-9 resolved")
		}
	})

	t.Run("plan gates only the measured exceptions", func(t *testing.T) {
		check := func(label string, s LongContextSettings, opus46, sonnet46 int) {
			registered := ApplyLongContext(models, s)
			if got := window(t, find(registered, "claude-opus-4-6")); got != opus46 {
				t.Errorf("%s opus-4-6 window = %d, want %d", label, got, opus46)
			}
			if got := window(t, find(registered, "claude-sonnet-4-6")); got != sonnet46 {
				t.Errorf("%s sonnet-4-6 window = %d, want %d", label, got, sonnet46)
			}
		}
		check("pro", pro, 200000, 200000)
		check("max", maxPlan, 1000000, 200000)
		check("extra", extra, 1000000, 1000000)

		// The source table used for id resolution is not mutated.
		if got := window(t, find(models, "claude-opus-4-6")); got != 1000000 {
			t.Errorf("source opus-4-6 window = %d, want 1000000", got)
		}
	})

	t.Run("labels exactly the registered 1M models", func(t *testing.T) {
		registered := ApplyLongContext(models, pro)
		for id, want := range map[string]string{
			"claude-opus-5":    "Claude Opus 5 1M",
			"claude-opus-4-6":  "Claude Opus 4.6",
			"claude-haiku-4-5": "Claude Haiku 4.5 (latest)",
		} {
			if got := name(find(registered, id)); got != want {
				t.Errorf("%s name = %q, want %q", id, got, want)
			}
		}
		if got := name(find(ApplyLongContext(models, extra), "claude-sonnet-4-6")); got != "Claude Sonnet 4.6 1M" {
			t.Errorf("sonnet-4-6 extra name = %q", got)
		}
	})

	t.Run("does not double the 1M suffix", func(t *testing.T) {
		in := []map[string]any{mockModel("claude-opus-5", map[string]any{"name": "Claude Opus 5 1M"})}
		if got := name(ApplyLongContext(in, pro)[0]); got != "Claude Opus 5 1M" {
			t.Errorf("name = %q", got)
		}
	})
}

func TestOpus55RequestsOneMOnPro(t *testing.T) {
	t.Run("claude-opus-5-5 requests 1M on Pro", func(t *testing.T) {
		requireRuntime(t, "claude-opus-5-5", pro, RuntimeModel{"claude-opus-5-5[1m]", 1000000})
	})
}

func TestCatalogContainsCurrentModels(t *testing.T) {
	catalog := Catalog()
	for _, id := range []string{"claude-opus-5-5", "claude-haiku-4-5", "claude-fable-5-1"} {
		if find(catalog, id) == nil {
			t.Errorf("catalog from %s lacks %s", CatalogSource, id)
		}
	}
	for _, id := range ids(BuildModels(catalog)) {
		if datedAlias.MatchString(id) {
			t.Errorf("BuildModels kept dated alias %s", id)
		}
	}
}
