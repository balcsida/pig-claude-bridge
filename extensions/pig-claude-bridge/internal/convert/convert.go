// Package convert ports the pi-claude-bridge conversion helpers: pi messages to
// Anthropic messages, and the small formatters around them.
package convert

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
)

const ProviderID = "claude-bridge"

var unsafeToolIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// SanitizeToolID replaces every character the API rejects in a tool id with "_".
// JavaScript counts UTF-16 units, so an astral character becomes two.
func SanitizeToolID(id string) string {
	return unsafeToolIDChars.ReplaceAllStringFunc(id, func(c string) string {
		if []rune(c)[0] > 0xFFFF {
			return "__"
		}
		return "_"
	})
}

// MapPiToolNameToSdk names a pi tool as a rebuilt transcript has to call it.
//
// The provider runs Claude Code with no builtin tools, so every callable tool is a
// pi tool served as mcp__custom-tools__<name>, and custom carries the served
// tool's exact casing. A name it lacks is a tool pi ran that is not served now;
// naming it after a Claude Code builtin would announce a tool the model cannot
// call (the phantom-call deadlock), so it gets the MCP prefix. A nil map is an
// empty one.
//
// It errors on a name that already carries the MCP prefix: pi history holds pi
// tool names, and prefixing twice invents a tool nobody serves.
func MapPiToolNameToSdk(name string, custom map[string]string) (string, error) {
	if name == "" {
		return "", nil
	}
	normalized := strings.ToLower(name)
	if strings.HasPrefix(normalized, MCPToolPrefix) {
		return "", fmt.Errorf("mapPiToolNameToSdk: %q is already an SDK tool name — pi history holds pi tool names", name)
	}
	if sdk, ok := custom[name]; ok {
		return sdk, nil
	}
	if sdk, ok := custom[normalized]; ok {
		return sdk, nil
	}
	return MCPToolPrefix + name, nil
}

// flatBlock is the part of a content block the text helpers read, whether it
// arrived as decoded JSON or as a ccsession block.
type flatBlock struct{ Type, Text, Data, MimeType string }

func str(v any) string {
	s, _ := v.(string)
	return s
}

func flatBlocks(content any) ([]flatBlock, bool) {
	var out []flatBlock
	fromMap := func(m map[string]any) {
		typ := "undefined"
		if s, ok := m["type"].(string); ok {
			typ = s
		}
		out = append(out, flatBlock{typ, str(m["text"]), str(m["data"]), str(m["mimeType"])})
	}
	switch c := content.(type) {
	case []any:
		out = []flatBlock{}
		for _, b := range c {
			if m, ok := b.(map[string]any); ok {
				fromMap(m)
			}
		}
	case []map[string]any:
		out = []flatBlock{}
		for _, m := range c {
			fromMap(m)
		}
	case []ccsession.Block:
		// Only converted user messages arrive as ccsession blocks (PlaceCarriedAttachments),
		// and those hold text, image and tool_result blocks.
		out = []flatBlock{}
		for _, b := range c {
			switch b := b.(type) {
			case ccsession.TextBlock:
				out = append(out, flatBlock{Type: "text", Text: b.Text})
			case ccsession.ImageBlock:
				out = append(out, flatBlock{Type: "image", Data: b.Source.Data, MimeType: b.Source.MediaType})
			case ccsession.ToolResultBlock:
				out = append(out, flatBlock{Type: "tool_result"})
			}
		}
	default:
		return nil, false
	}
	return out, true
}

// MessageContentToText flattens a message's content to text. Blocks that are
// neither text nor image leave a "[type]" marker, but only alongside real text.
func MessageContentToText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, _ := flatBlocks(content)
	var parts []string
	hasText := false
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
			hasText = true
		} else if b.Type != "text" && b.Type != "image" {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	if !hasText {
		return ""
	}
	return strings.Join(parts, "\n")
}

func base64Image(mimeType, data string) ccsession.ImageBlock {
	return ccsession.ImageBlock{Type: "image", Source: ccsession.ImageSource{Type: "base64", MediaType: mimeType, Data: data}}
}

// toolResultContent flattens a tool result to text, which is how Claude Code
// stores most of them. A result carrying an image keeps the block array shape
// instead, as Claude Code writes for screenshots.
func toolResultContent(content any) any {
	blocks, isArray := flatBlocks(content)
	hasImage := false
	for _, b := range blocks {
		hasImage = hasImage || (b.Type == "image" && b.Data != "" && b.MimeType != "")
	}
	if !isArray || !hasImage {
		return MessageContentToText(content)
	}
	out := []ccsession.Block{}
	for _, b := range blocks {
		switch {
		case b.Type == "text" && b.Text != "":
			out = append(out, ccsession.TextBlock{Type: "text", Text: b.Text})
		case b.Type == "image" && b.Data != "" && b.MimeType != "":
			out = append(out, base64Image(b.MimeType, b.Data))
		case b.Type != "text" && b.Type != "image":
			// Same marker MessageContentToText leaves, so the text and image paths
			// describe an extension's output the same way.
			out = append(out, ccsession.TextBlock{Type: "text", Text: "[" + b.Type + "]"})
		}
	}
	return out
}

// nonEmptyToolResultContent is the result's content, or the placeholder Claude Code
// itself substitutes live for an empty one (the API rejects an empty result marked is_error).
func nonEmptyToolResultContent(msg map[string]any, custom map[string]string) (any, error) {
	content := toolResultContent(msg["content"])
	if text, ok := content.(string); !ok || strings.TrimSpace(text) != "" {
		return content, nil
	}
	if isError, _ := msg["isError"].(bool); isError {
		return "Command failed with no output", nil
	}
	name, err := MapPiToolNameToSdk(str(msg["toolName"]), custom)
	if err != nil {
		return nil, err
	}
	return "(" + name + " completed with no output)", nil
}

// ConvertPiMessages converts a pi message array to Anthropic messages. recorded
// replaces a tool result, by pi tool call id, with the block Claude Code recorded
// for it, verbatim. origins lists, per Anthropic message, the indices of the pi
// messages it came from (a results message collects every result of its turn).
func ConvertPiMessages(messages []map[string]any, custom map[string]string, recorded map[string]json.RawMessage) (anthropic []ccsession.Message, sanitizedIDs map[string]string, origins [][]int, err error) {
	anthropic = []ccsession.Message{}
	origins = [][]int{}
	sanitizedIDs = map[string]string{}
	appendMsg := func(m ccsession.Message, index int) {
		anthropic = append(anthropic, m)
		origins = append(origins, []int{index})
	}
	sanitize := func(id string) string {
		if existing := sanitizedIDs[id]; existing != "" {
			return existing
		}
		clean := SanitizeToolID(id)
		sanitizedIDs[id] = clean
		return clean
	}
	// The user message collecting this assistant turn's tool results and the
	// index of the assistant message it follows; both reset at every assistant
	// message that is kept.
	turnResultsIdx, turnAssistantIdx := -1, -1
	var turnResults []ccsession.Block

	for piIndex, msg := range messages {
		switch msg["role"] {
		case "user":
			appendMsg(ccsession.Message{Role: "user", Content: userContent(msg["content"])}, piIndex)
		case "assistant":
			var content []map[string]any
			switch c := msg["content"].(type) {
			case []map[string]any:
				content = c
			case []any:
				for _, raw := range c {
					block, _ := raw.(map[string]any)
					content = append(content, block)
				}
			}
			blocks := []ccsession.Block{}
			for _, block := range content {
				switch typ, _ := block["type"].(string); {
				case typ == "text" && str(block["text"]) != "":
					blocks = append(blocks, ccsession.TextBlock{Type: "text", Text: str(block["text"])})
				case typ == "thinking":
					// Only replay thinking Claude Code itself produced: a signature
					// minted by any other provider is not ours to hand back, and
					// Anthropic rejects ones it cannot verify.
					if sig := str(block["thinkingSignature"]); msg["provider"] == ProviderID && sig != "" {
						blocks = append(blocks, ccsession.ThinkingBlock{Type: "thinking", Thinking: str(block["thinking"]), Signature: sig})
					}
				case typ == "toolCall":
					input := block["arguments"]
					if input == nil {
						input = map[string]any{}
					}
					name, err := MapPiToolNameToSdk(str(block["name"]), custom)
					if err != nil {
						return nil, nil, nil, err
					}
					blocks = append(blocks, ccsession.ToolUseBlock{Type: "tool_use", ID: sanitize(str(block["id"])), Name: name, Input: input})
				}
			}
			// A turn aborted before anything streamed carries no content at all.
			// A placeholder would invent a reply and cost the downstream prompt
			// cache on every rebuild, so drop it. turnResults is deliberately left
			// alone: clearing it would split parallel results across an abort and
			// make the repair stub the real one.
			if len(content) == 0 {
				continue
			}
			// Blocks were present but every one was filtered; empty content is
			// rejected by the API and dropping the message would break tool pairing.
			if len(blocks) == 0 {
				blocks = append(blocks, ccsession.TextBlock{Type: "text", Text: "[incompatible content omitted]"})
			}
			turnResultsIdx, turnResults = -1, nil
			turnAssistantIdx = len(anthropic)
			appendMsg(ccsession.Message{Role: "assistant", Content: blocks}, piIndex)
		case "toolResult":
			// Pi records one message per tool result, but the repair only pairs
			// results sharing the user message directly after their assistant
			// message. Collect them into the turn's first results message, placed
			// right after the assistant message even when a steer landed between.
			id := sanitize(str(msg["toolCallId"]))
			var block ccsession.Block
			if raw, ok := recorded[str(msg["toolCallId"])]; ok {
				block = ccsession.RawBlock(raw)
			} else {
				content, err := nonEmptyToolResultContent(msg, custom)
				if err != nil {
					return nil, nil, nil, err
				}
				isError, _ := msg["isError"].(bool)
				block = ccsession.ToolResultBlock{Type: "tool_result", ToolUseID: id, Content: content, IsError: isError}
			}
			turnResults = append(turnResults, block)
			if turnResultsIdx >= 0 {
				anthropic[turnResultsIdx].Content = turnResults
				origins[turnResultsIdx] = append(origins[turnResultsIdx], piIndex)
				continue
			}
			results := ccsession.Message{Role: "user", Content: turnResults}
			// A result with no assistant message before it is malformed history;
			// appending keeps it in order for the repair to discard.
			turnResultsIdx = len(anthropic)
			if turnAssistantIdx >= 0 {
				turnResultsIdx = turnAssistantIdx + 1
			}
			anthropic = append(anthropic, ccsession.Message{})
			copy(anthropic[turnResultsIdx+1:], anthropic[turnResultsIdx:])
			anthropic[turnResultsIdx] = results
			origins = append(origins, nil)
			copy(origins[turnResultsIdx+1:], origins[turnResultsIdx:])
			origins[turnResultsIdx] = []int{piIndex}
		}
	}
	return anthropic, sanitizedIDs, origins, nil
}

func userContent(content any) any {
	if s, ok := content.(string); ok {
		if s == "" {
			return "[empty]"
		}
		return s
	}
	blocks, isArray := flatBlocks(content)
	if !isArray {
		return "[empty]"
	}
	parts := []ccsession.Block{}
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, ccsession.TextBlock{Type: "text", Text: b.Text})
		} else if b.Type == "image" && b.Data != "" && b.MimeType != "" {
			parts = append(parts, base64Image(b.MimeType, b.Data))
		}
	}
	if len(parts) == 0 {
		return "[image]"
	}
	return parts
}
