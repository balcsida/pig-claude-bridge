package ccsession

import (
	"reflect"
	"testing"
)

func use(id string) Block {
	return ToolUseBlock{Type: "tool_use", ID: id, Name: "X", Input: map[string]any{}}
}
func res(id string) Block {
	return ToolResultBlock{Type: "tool_result", ToolUseID: id, Content: "ok"}
}
func synth(id string) Block {
	return ToolResultBlock{Type: "tool_result", ToolUseID: id, Content: "[no tool result recorded]", IsError: true}
}

func TestRepairToolPairing(t *testing.T) {
	t.Run("passes through a paired tool_use/tool_result", func(t *testing.T) {
		msgs := []Message{{Role: "assistant", Content: []Block{use("t1")}}, {Role: "user", Content: []Block{res("t1")}}}
		if got := RepairToolPairing(msgs); !reflect.DeepEqual(got, msgs) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("synthesizes a tool_result for an orphan tool_use", func(t *testing.T) {
		msgs := []Message{{Role: "assistant", Content: []Block{use("orphan")}}, {Role: "user", Content: "next turn"}}
		want := []Message{msgs[0], {Role: "user", Content: []Block{synth("orphan"), TextBlock{Type: "text", Text: "next turn"}}}}
		if got := RepairToolPairing(msgs); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("empty input returns empty", func(t *testing.T) {
		if got := RepairToolPairing(nil); len(got) != 0 {
			t.Fatalf("got %#v", got)
		}
	})
}

func TestRepairInsertsMissingResult(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", Content: []Block{use("a"), use("b")}},
		{Role: "user", Content: []Block{res("b"), TextBlock{Type: "text", Text: "hi"}}},
	}
	want := []Message{msgs[0], {Role: "user", Content: []Block{synth("a"), res("b"), TextBlock{Type: "text", Text: "hi"}}}}
	if got := RepairToolPairing(msgs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestRepairDropsOrphanResult(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: []Block{TextBlock{Type: "text", Text: "a"}}},
		{Role: "user", Content: []Block{res("ghost"), TextBlock{Type: "text", Text: "keep"}}},
	}
	want := []Message{msgs[0], msgs[1], {Role: "user", Content: []Block{TextBlock{Type: "text", Text: "keep"}}}}
	if got := RepairToolPairing(msgs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestRepairAppendsTrailingUnanswered(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: []Block{use("a")}}}
	want := append(msgs[:2:2], Message{Role: "user", Content: []Block{synth("a")}})
	if got := RepairToolPairing(msgs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestRepairFirstMessageEmptied(t *testing.T) {
	msgs := []Message{{Role: "user", Content: []Block{res("ghost")}}, {Role: "assistant", Content: []Block{TextBlock{Type: "text", Text: "a"}}}}
	want := []Message{{Role: "user", Content: []Block{TextBlock{Type: "text", Text: "[orphaned tool result removed]"}}}, msgs[1]}
	got, origin := RepairWithOrigin(msgs)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(origin, []int{-1, 1}) {
		t.Fatalf("got %#v %v", got, origin)
	}
}

func TestRepairOriginMap(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: []Block{use("a")}},
		{Role: "assistant", Content: []Block{use("b")}},
		{Role: "user", Content: []Block{res("b"), res("ghost")}},
		{Role: "user", Content: []Block{res("ghost")}},
		{Role: "assistant", Content: []Block{use("c")}},
	}
	_, origin := RepairWithOrigin(msgs)
	if want := []int{0, 1, -1, 2, 3, 5, -1}; !reflect.DeepEqual(origin, want) {
		t.Fatalf("origin = %v, want %v", origin, want)
	}
}
