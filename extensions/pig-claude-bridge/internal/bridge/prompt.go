package bridge

import (
	"fmt"
	"strings"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// extractUserPrompt is the current user turn as text, or ok=false when the last
// message is not a user message. Empty messages are dropped before joining, so an
// all-empty turn yields "" and trips the caller's empty-prompt guard.
func extractUserPrompt(messages []map[string]any) (prompt string, ok bool) {
	turn := messages[turnStart(messages):]
	if len(turn) == 0 {
		return "", false
	}
	var parts []string
	for _, m := range turn {
		text, isString := m["content"].(string)
		if !isString {
			text = convert.MessageContentToText(m["content"])
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n"), true
}

// extractUserPromptBlocks is the current user turn as content blocks, or nil when
// it has no image (the caller falls back to the text prompt).
func (b *Bridge) extractUserPromptBlocks(messages []map[string]any) ([]any, error) {
	turn := messages[turnStart(messages):]
	if len(turn) == 0 {
		return nil, nil
	}
	hasImage := false
	blocks := []any{}
	for _, m := range turn {
		var content []any
		switch c := m["content"].(type) {
		case string:
			content = []any{map[string]any{"type": "text", "text": c}}
		case []any:
			content = c
		default:
			// Off-type content violates the contract: fail rather than degrade, and name
			// the shape, since the cause is almost always another extension.
			return nil, fmt.Errorf("extractUserPromptBlocks: user message content must be a string or block array, got %T — likely a malformed message from another extension", m["content"])
		}
		for _, raw := range content {
			block := obj(raw)
			switch block["type"] {
			case "text":
				if text := str(block["text"]); text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image":
				data, mime := str(block["data"]), str(block["mimeType"])
				if data == "" || mime == "" {
					b.debugf("image block missing data or mimeType, skipping")
					continue
				}
				b.debugf("image block: mimeType=%s, data length=%d", mime, len(data))
				hasImage = true
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": data}})
			}
		}
	}
	types := make([]string, len(blocks))
	for i, blk := range blocks {
		types[i] = str(obj(blk)["type"])
	}
	b.debugf("extractUserPromptBlocks: %d msgs in turn, %d blocks, types=%s", len(turn), len(blocks), strings.Join(types, ","))
	if !hasImage {
		return nil, nil
	}
	return blocks, nil
}

// steerBlocks is the trailing user turn as content blocks, nil when there is none.
// Blocks rather than text so image steers keep their images.
func (b *Bridge) steerBlocks(messages []map[string]any) ([]any, error) {
	blocks, err := b.extractUserPromptBlocks(messages)
	if err != nil || blocks != nil {
		return blocks, err
	}
	if text, _ := extractUserPrompt(messages); text != "" {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	return nil, nil
}
