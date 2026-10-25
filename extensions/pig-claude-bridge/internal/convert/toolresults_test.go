package convert

import (
	"reflect"
	"testing"
)

func TestExtractAllToolResults(t *testing.T) {
	msgs := decodeMessages(t, `[
		{"role":"assistant","content":[]},
		{"role":"toolResult","toolCallId":"a","content":"one"},
		{"role":"user","content":"steer"},
		{"role":"toolResult","toolCallId":"b","isError":true,"content":[{"type":"image","data":"D","mimeType":"image/png"},{"type":"text","text":"x"},{"type":"document"}]},
		{"role":"toolResult","toolCallId":"c","content":[{"type":"document"}]}
	]`)
	results, stopIdx := ExtractAllToolResults(msgs)
	if stopIdx != 0 {
		t.Fatalf("stopIdx = %d", stopIdx)
	}
	want := []McpResult{
		{Content: McpContent{{"type": "text", "text": "one"}}, ToolCallID: "a"},
		{Content: McpContent{{"type": "image", "data": "D", "mimeType": "image/png"}, {"type": "text", "text": "x"}}, IsError: true, ToolCallID: "b"},
		{Content: McpContent{{"type": "text", "text": ""}}, ToolCallID: "c"},
	}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("got %#v", results)
	}

	if _, idx := ExtractAllToolResults(msgs[1:3]); idx != -1 {
		t.Fatalf("no assistant: stopIdx = %d", idx)
	}
}
