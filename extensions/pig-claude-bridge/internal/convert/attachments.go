package convert

import (
	"encoding/json"
	"fmt"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
)

// Claude Code expands an `@file` mention itself and writes the expansion as an
// attachment record pi never sees, so a rebuild from pi's history would keep the
// prompt text but lose the file. Only `file` is carried: edited_text_file
// duplicates a tool call pi already holds and usually hangs off a tool result,
// which has no prompt ordinal, and the other kinds Claude Code rewrites every turn.
const contentBearingAttachment = "file"

// CarriedAttachment is an attachment plus where its parent prompt sits.
type CarriedAttachment struct {
	Attachment map[string]any
	// UserOrdinal is the parent's position among the text-bearing user records.
	UserOrdinal int
	// Text is that record's text, to verify the ordinal still points at the same turn.
	Text string
}

// userPromptText is the text of a user record holding a prompt, as opposed to one
// holding tool results.
func userPromptText(record map[string]any) (string, bool) {
	if record["type"] != "user" {
		return "", false
	}
	message, _ := record["message"].(map[string]any)
	return promptText(message["content"])
}

func promptText(content any) (string, bool) {
	blocks, _ := flatBlocks(content)
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return "", false
		}
	}
	text := MessageContentToText(content)
	return text, text != ""
}

// CollectCarriedAttachments finds the content-bearing attachments in a session,
// each tagged with its parent's ordinal among the prompts. The ordinal is the key
// rather than the record index because a rebuild does not reproduce the record
// list one-for-one. Attachments chain to one another, so an ordinal is inherited
// up the parent links until it reaches a prompt, including through kinds not carried.
func CollectCarriedAttachments(records []ccsession.RawRecord) []CarriedAttachment {
	ordinalOf := map[string]int{}
	textOf := map[string]string{}
	ordinal := 0
	var carried []CarriedAttachment

	for _, raw := range records {
		var record map[string]any
		if json.Unmarshal(raw, &record) != nil {
			continue
		}
		uuid := str(record["uuid"])
		if text, ok := userPromptText(record); ok {
			ordinalOf[uuid], textOf[uuid] = ordinal, text
			ordinal++
			continue
		}
		if record["type"] != "attachment" {
			continue
		}
		parent, isString := record["parentUuid"].(string)
		if !isString {
			continue
		}
		inherited, known := ordinalOf[parent]
		if !known {
			continue
		}
		ordinalOf[uuid], textOf[uuid] = inherited, textOf[parent]

		attachment, _ := record["attachment"].(map[string]any)
		if attachment == nil || attachment["type"] != contentBearingAttachment {
			continue
		}
		carried = append(carried, CarriedAttachment{Attachment: attachment, UserOrdinal: inherited, Text: textOf[parent]})
	}
	return carried
}

// PlaceCarriedAttachments resolves each carried attachment to an index in msgs,
// the messages after conversion and repair (repair is idempotent, so the indices
// stay valid through ImportMessages). It is conservative: attaching a file to the
// wrong turn tells the model it saw something at a point it did not, so an ordinal
// that misses or lands on a prompt whose text changed is reported in skipped and dropped.
func PlaceCarriedAttachments(carried []CarriedAttachment, msgs []ccsession.Message) (attachments []ccsession.ImportAttachment, skipped []string) {
	type prompt struct {
		index int
		text  string
	}
	var prompts []prompt
	for i, msg := range msgs {
		if msg.Role != "user" {
			continue
		}
		if text, ok := promptText(msg.Content); ok {
			prompts = append(prompts, prompt{i, text})
		}
	}
	for _, item := range carried {
		name := fmt.Sprint(item.Attachment["type"])
		if f, ok := item.Attachment["filename"]; ok && f != nil {
			name = fmt.Sprint(f)
		}
		if item.UserOrdinal >= len(prompts) {
			skipped = append(skipped, fmt.Sprintf("%s: prompt #%d is no longer in history", name, item.UserOrdinal))
			continue
		}
		candidate := prompts[item.UserOrdinal]
		if candidate.text != item.Text {
			skipped = append(skipped, fmt.Sprintf("%s: prompt #%d changed", name, item.UserOrdinal))
			continue
		}
		attachments = append(attachments, ccsession.ImportAttachment{AfterIndex: candidate.index, Attachment: item.Attachment})
	}
	return attachments, skipped
}
