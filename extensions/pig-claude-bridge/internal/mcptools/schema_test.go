package mcptools

import (
	"reflect"
	"sort"
	"testing"
)

func keys(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestFlattenRootSchema(t *testing.T) {
	t.Run("returns a schema without combinators unchanged", func(t *testing.T) {
		in := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
		if got := FlattenRootSchema(in); !reflect.DeepEqual(got, in) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("flattens anyOf: root wins, branch-only requirement dropped", func(t *testing.T) {
		in := map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []any{"operation"},
			"properties": map[string]any{
				"operation": map[string]any{"type": "string", "enum": []any{"start", "status"}},
				"input":     map[string]any{"type": "string"},
			},
			"anyOf": []any{
				map[string]any{"type": "object", "properties": map[string]any{"operation": map[string]any{"enum": []any{"start"}}, "baseRef": map[string]any{"type": "string"}}},
				map[string]any{"type": "object", "properties": map[string]any{"operation": map[string]any{"enum": []any{"status"}}}, "required": []any{"input"}},
			},
		}
		got := FlattenRootSchema(in)
		if _, ok := got["anyOf"]; ok {
			t.Fatal("anyOf leaked")
		}
		if !reflect.DeepEqual(keys(got["properties"].(map[string]any)), []string{"baseRef", "input", "operation"}) {
			t.Fatalf("properties %v", got["properties"])
		}
		if !reflect.DeepEqual(got["properties"].(map[string]any)["operation"], in["properties"].(map[string]any)["operation"]) {
			t.Fatal("root declaration must win")
		}
		if !reflect.DeepEqual(got["required"], []string{"operation"}) || got["additionalProperties"] != false || got["type"] != "object" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("allOf requirements join required", func(t *testing.T) {
		got := FlattenRootSchema(map[string]any{
			"type": "object", "properties": map[string]any{"a": map[string]any{}}, "required": []any{"a"},
			"allOf": []any{map[string]any{"properties": map[string]any{"b": map[string]any{}}, "required": []any{"b"}}},
		})
		if !reflect.DeepEqual(got["required"], []string{"a", "b"}) {
			t.Fatalf("required %v", got["required"])
		}
	})

	t.Run("oneOf of $defs refs, no required", func(t *testing.T) {
		got := FlattenRootSchema(map[string]any{
			"type": "object",
			"$defs": map[string]any{
				"byId":   map[string]any{"properties": map[string]any{"id": map[string]any{}}, "required": []any{"id"}},
				"byName": map[string]any{"properties": map[string]any{"name": map[string]any{}, "scope": map[string]any{}}, "required": []any{"name"}},
			},
			"oneOf": []any{map[string]any{"$ref": "#/$defs/byId"}, map[string]any{"$ref": "#/$defs/byName"}},
		})
		if !reflect.DeepEqual(keys(got["properties"].(map[string]any)), []string{"id", "name", "scope"}) {
			t.Fatalf("properties %v", got["properties"])
		}
		if _, ok := got["required"]; ok {
			t.Fatal("required must be absent")
		}
	})
}
