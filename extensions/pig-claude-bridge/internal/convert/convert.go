// Package convert ports the pi-claude-bridge conversion helpers: pi messages to
// Anthropic messages, and the small formatters around them.
package convert

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
)

const ProviderID = "claude-bridge"

// PiToSdkToolName maps pi tool names to Claude Code's builtin names. It is only
// correct on the AskClaude path, where Claude Code runs its own tools — see
// MapPiToolNameToSdk. Treat it as read-only.
var PiToSdkToolName = map[string]string{
	"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash",
}

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
// A non-nil custom map selects the provider path: the query runs with no builtin
// tools, so every callable tool is a pi tool served as mcp__custom-tools__<name>,
// and the map carries the served tool's exact casing. A name it lacks is a tool
// pi ran that is not served now; naming it after a Claude Code builtin would
// announce a tool the model cannot call (the phantom-call deadlock), so it gets
// the MCP prefix. A nil map selects the AskClaude path, where builtin names are
// real.
//
// It panics on a name that already carries the MCP prefix: pi history holds pi
// tool names, and prefixing twice invents a tool nobody serves.
func MapPiToolNameToSdk(name string, custom map[string]string) string {
	if name == "" {
		return ""
	}
	normalized := strings.ToLower(name)
	if strings.HasPrefix(normalized, MCPToolPrefix) {
		panic(fmt.Sprintf("convert: MapPiToolNameToSdk: %q is already an SDK tool name — pi history holds pi tool names", name))
	}
	if custom == nil {
		if sdk, ok := PiToSdkToolName[normalized]; ok {
			return sdk
		}
		return pascalCase(name)
	}
	if sdk, ok := custom[name]; ok {
		return sdk
	}
	if sdk, ok := custom[normalized]; ok {
		return sdk
	}
	return MCPToolPrefix + name
}

var (
	splitLowerUpper = regexp.MustCompile(`([\p{Ll}\d])(\p{Lu})`)
	splitUpperUpper = regexp.MustCompile(`(\p{Lu})(\p{Lu}\p{Ll})`)
	stripNonWord    = regexp.MustCompile(`[^\p{L}\d]+`)
)

// pascalCase is change-case's pascalCase with default options.
func pascalCase(input string) string {
	s := strings.TrimSpace(input)
	s = splitLowerUpper.ReplaceAllString(s, "${1}\x00${2}")
	s = splitUpperUpper.ReplaceAllString(s, "${1}\x00${2}")
	s = strings.Trim(stripNonWord.ReplaceAllString(s, "\x00"), "\x00")
	if s == "" {
		return ""
	}
	var out strings.Builder
	for i, word := range strings.Split(s, "\x00") {
		first := []rune(word)[0]
		if i > 0 && first >= '0' && first <= '9' {
			out.WriteString("_" + string(first))
		} else {
			out.WriteRune(unicode.ToUpper(first))
		}
		out.WriteString(strings.ToLower(string([]rune(word)[1:])))
	}
	return out.String()
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
		out = []flatBlock{}
		for _, b := range c {
			switch b := b.(type) {
			case ccsession.TextBlock:
				out = append(out, flatBlock{Type: "text", Text: b.Text})
			case ccsession.ImageBlock:
				out = append(out, flatBlock{Type: "image", Data: b.Source.Data, MimeType: b.Source.MediaType})
			case ccsession.ThinkingBlock:
				out = append(out, flatBlock{Type: "thinking"})
			case ccsession.ToolUseBlock:
				out = append(out, flatBlock{Type: "tool_use"})
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

// Dropped is what ConvertPiMessages discarded, for the debug line. Nothing
// downstream can tell a stripped thinking block from one never sent.
type Dropped struct {
	Thinking     int
	AbortedTurns int
	Providers    []string // insertion-ordered, without duplicates
	Other        map[string]int
}

func (d *Dropped) addProvider(p string) {
	for _, q := range d.Providers {
		if q == p {
			return
		}
	}
	d.Providers = append(d.Providers, p)
}

// ConvertPiMessages converts a pi message array to Anthropic messages.
func ConvertPiMessages(messages []map[string]any, custom map[string]string) (anthropic []ccsession.Message, sanitizedIDs map[string]string, dropped Dropped) {
	anthropic = []ccsession.Message{}
	sanitizedIDs = map[string]string{}
	dropped.Other = map[string]int{}
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

	for _, msg := range messages {
		switch msg["role"] {
		case "user":
			anthropic = append(anthropic, ccsession.Message{Role: "user", Content: userContent(msg["content"])})
		case "assistant":
			content, _ := msg["content"].([]any)
			blocks := []ccsession.Block{}
			for _, raw := range content {
				block, _ := raw.(map[string]any)
				switch typ, _ := block["type"].(string); {
				case typ == "text" && str(block["text"]) != "":
					blocks = append(blocks, ccsession.TextBlock{Type: "text", Text: str(block["text"])})
				case typ == "thinking":
					// Only replay thinking Claude Code itself produced: a signature
					// minted by any other provider is not ours to hand back, and
					// Anthropic rejects ones it cannot verify.
					if sig := str(block["thinkingSignature"]); msg["provider"] == ProviderID && sig != "" {
						blocks = append(blocks, ccsession.ThinkingBlock{Type: "thinking", Thinking: str(block["thinking"]), Signature: sig})
					} else {
						dropped.Thinking++
						provider := str(msg["provider"])
						if provider == "" {
							provider = "unknown"
						}
						dropped.addProvider(provider)
					}
				case typ == "toolCall":
					input := block["arguments"]
					if input == nil {
						input = map[string]any{}
					}
					blocks = append(blocks, ccsession.ToolUseBlock{Type: "tool_use", ID: sanitize(str(block["id"])), Name: MapPiToolNameToSdk(str(block["name"]), custom), Input: input})
				default:
					dropped.Other[typ]++
				}
			}
			// A turn aborted before anything streamed carries no content at all.
			// A placeholder would invent a reply and cost the downstream prompt
			// cache on every rebuild, so drop it. turnResults is deliberately left
			// alone: clearing it would split parallel results across an abort and
			// make the repair stub the real one.
			if len(content) == 0 {
				dropped.AbortedTurns++
				continue
			}
			// Blocks were present but every one was filtered; empty content is
			// rejected by the API and dropping the message would break tool pairing.
			if len(blocks) == 0 {
				blocks = append(blocks, ccsession.TextBlock{Type: "text", Text: "[incompatible content omitted]"})
			}
			turnResultsIdx, turnResults = -1, nil
			turnAssistantIdx = len(anthropic)
			anthropic = append(anthropic, ccsession.Message{Role: "assistant", Content: blocks})
		case "toolResult":
			// Pi records one message per tool result, but the repair only pairs
			// results sharing the user message directly after their assistant
			// message. Collect them into the turn's first results message, placed
			// right after the assistant message even when a steer landed between.
			isError, _ := msg["isError"].(bool)
			turnResults = append(turnResults, ccsession.ToolResultBlock{
				Type: "tool_result", ToolUseID: sanitize(str(msg["toolCallId"])), Content: toolResultContent(msg["content"]), IsError: isError,
			})
			if turnResultsIdx >= 0 {
				anthropic[turnResultsIdx].Content = turnResults
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
		}
	}
	return anthropic, sanitizedIDs, dropped
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
