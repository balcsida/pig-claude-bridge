package convert

// McpContent is the content array of an MCP tool result: text and image blocks.
type McpContent = []map[string]any

type McpResult struct {
	Content    McpContent
	IsError    bool
	ToolCallID string
}

// ToolResultToMcpContent converts a pi toolResult message's content to MCP
// content, never returning an empty array.
func ToolResultToMcpContent(msg map[string]any) McpContent {
	empty := McpContent{{"type": "text", "text": ""}}
	content := msg["content"]
	if s, ok := content.(string); ok {
		return McpContent{{"type": "text", "text": s}}
	}
	flat, isArray := flatBlocks(content)
	if !isArray {
		return empty
	}
	blocks := McpContent{}
	for _, b := range flat {
		if b.Type == "text" && b.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
		} else if b.Type == "image" && b.Data != "" && b.MimeType != "" {
			blocks = append(blocks, map[string]any{"type": "image", "data": b.Data, "mimeType": b.MimeType})
		}
	}
	if len(blocks) == 0 {
		return empty
	}
	return blocks
}

// ExtractAllToolResults collects this turn's tool results from the context
// tail, walking back to the nearest assistant message (the turn boundary) and
// past user messages a steer or follow-up injected mid-execution. stopIdx is the
// boundary's index, or -1.
func ExtractAllToolResults(messages []map[string]any) (results []McpResult, stopIdx int) {
	stopIdx = -1
	for i := len(messages) - 1; i >= 0 && stopIdx < 0; i-- {
		switch messages[i]["role"] {
		case "toolResult":
			isError, _ := messages[i]["isError"].(bool)
			results = append(results, McpResult{Content: ToolResultToMcpContent(messages[i]), IsError: isError, ToolCallID: str(messages[i]["toolCallId"])})
		case "assistant":
			stopIdx = i
		}
	}
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return results, stopIdx
}
