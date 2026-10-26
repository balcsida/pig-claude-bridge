package ccsession

import "slices"

// Port of repairWithOrigin (cc-session-io chunk-7JUXR4CP.js:62-121).

const (
	noResultText      = "[no tool result recorded]"
	orphanedResultTxt = "[orphaned tool result removed]"
)

// RepairToolPairing returns msgs with every tool_use answered by a tool_result
// and every tool_result matched to a tool_use.
func RepairToolPairing(msgs []Message) []Message {
	repaired, _ := RepairWithOrigin(msgs)
	return repaired
}

// RepairWithOrigin repairs msgs and reports, for each repaired message, the
// index of the input message it came from, or -1 when it was synthesised.
func RepairWithOrigin(msgs []Message) (repaired []Message, origin []int) {
	repaired = []Message{}
	origin = []int{}
	var pending []string // insertion-ordered set, like the library's Set
	synthetic := func(id string) Block {
		return ToolResultBlock{Type: "tool_result", ToolUseID: id, Content: noResultText, IsError: true}
	}
	flushPending := func() {
		if len(pending) > 0 {
			blocks := make([]Block, len(pending))
			for i, id := range pending {
				blocks[i] = synthetic(id)
			}
			repaired = append(repaired, Message{Role: "user", Content: blocks})
			origin = append(origin, -1)
		}
		pending = nil
	}
	for index, msg := range msgs {
		blocks, isArray := msg.Content.([]Block)
		if msg.Role == "assistant" {
			flushPending()
			// Every string id counts, "" included, as in the library.
			for _, b := range blocks {
				if tu, ok := b.(ToolUseBlock); ok && !slices.Contains(pending, tu.ID) {
					pending = append(pending, tu.ID)
				}
			}
			repaired = append(repaired, msg)
			origin = append(origin, index)
			continue
		}
		hasToolResults := false
		for _, b := range blocks {
			if b.blockType() == "tool_result" {
				hasToolResults = true
			}
		}
		if pending == nil && !hasToolResults {
			repaired = append(repaired, msg)
			origin = append(origin, index)
			continue
		}
		input := blocks
		if text, ok := msg.Content.(string); ok && !isArray && text != "" {
			input = []Block{TextBlock{Type: "text", Text: text}}
		}
		var provided []string
		var kept []Block
		for _, b := range input {
			tr, ok := b.(ToolResultBlock)
			if !ok {
				kept = append(kept, b)
			} else if slices.Contains(pending, tr.ToolUseID) {
				provided = append(provided, tr.ToolUseID)
				kept = append(kept, b)
			}
		}
		if pending != nil {
			var missing []Block
			for _, id := range pending {
				if !slices.Contains(provided, id) {
					missing = append(missing, synthetic(id))
				}
			}
			kept = append(missing, kept...)
			pending = nil
		}
		if len(kept) == 0 {
			if len(repaired) == 0 {
				repaired = append(repaired, Message{Role: "user", Content: []Block{TextBlock{Type: "text", Text: orphanedResultTxt}}})
				origin = append(origin, -1)
			}
			continue
		}
		msg.Content = kept
		repaired = append(repaired, msg)
		origin = append(origin, index)
	}
	flushPending()
	return repaired, origin
}
