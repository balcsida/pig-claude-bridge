package bridge

// The recording taps of index.ts (tapRecording, incorporate, recordAssistantEmission,
// completeAssistantRecording): what a live query learns from Claude Code's frames
// about which pi messages its session file now covers.

import (
	"strings"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/digest"
)

// topLevel reports whether frame has parent_tool_use_id present and null: a frame of
// the main conversation, not of a Claude Code subagent.
func topLevel(frame map[string]any) bool {
	v, present := frame["parent_tool_use_id"]
	return present && v == nil
}

// incorporate records that CC took the input written under uuid into a turn, and
// drops it from the steers that keep stdin open. Stdin still stays open until that
// turn's result, since its MCP calls need it. It reports whether the uuid was pending.
func (b *Bridge) incorporate(q *QueryContext, uuid string) bool {
	recorded := q.recording.Acknowledge(uuid)
	q.mu.Lock()
	removed := q.removePendingSteer(uuid)
	q.mu.Unlock()
	return removed || recorded
}

// tapRecording runs on every frame, before the UI gate: echoes after a tool boundary
// still belong to the emitted step, not to whatever stream is currently attached.
func (b *Bridge) tapRecording(q *QueryContext, frame map[string]any) {
	typ := str(frame["type"])
	msg := obj(frame["message"])
	synthetic := msg["model"] == "<synthetic>"
	eventType := str(obj(frame["event"])["type"])
	if (typ == "assistant" && topLevel(frame) && !synthetic) ||
		(typ == "stream_event" && topLevel(frame) && (eventType == "content_block_start" || eventType == "content_block_delta")) {
		q.recording.AssistantObserved()
		// CC persists a turn's opening input before calling the API but never replays
		// one holding its own tags (e.g. <task-notification>), so the turn's first
		// output incorporates it.
		q.mu.Lock()
		opener := q.turnOpener
		q.turnOpener = ""
		q.mu.Unlock()
		if opener != "" && b.incorporate(q, opener) {
			b.debugf("recording: turn opener %s was never replayed; its first output acknowledges it", opener)
		}
	}
	if typ == "result" {
		q.mu.Lock()
		q.sawResult = true
		q.turnOpener = ""
		if len(q.pendingSteerUUIDs) > 0 {
			q.turnOpener = q.pendingSteerUUIDs[0]
		}
		q.mu.Unlock()
	}
	if uuid := str(frame["uuid"]); typ == "user" && topLevel(frame) && uuid != "" {
		if frame["isReplay"] == true {
			b.incorporate(q, uuid)
		}
		for _, raw := range list(msg["content"]) {
			if block := obj(raw); block["type"] == "tool_result" {
				q.recording.Result(str(block["tool_use_id"]), uuid)
			}
		}
	}
	if typ != "assistant" || !topLevel(frame) || synthetic || frame["aborted"] == true {
		return
	}
	id := str(msg["id"])
	q.mu.Lock()
	defer q.mu.Unlock()
	step := q.assistantSteps[id]
	if step == nil {
		step = &assistantStep{seen: map[string]bool{}}
		q.assistantSteps[id] = step
	}
	uuid := str(frame["uuid"])
	if step.seen[uuid] {
		return
	}
	step.seen[uuid] = true
	if step.emitted == nil {
		q.lastAssistantMessageID = id
	}
	for _, raw := range list(msg["content"]) {
		block := obj(raw)
		switch block["type"] {
		case "text":
			if text := str(block["text"]); text != "" {
				step.content = append(step.content, map[string]any{"type": "text", "text": text})
			}
		case "thinking":
			step.content = append(step.content, map[string]any{"type": "thinking", "thinking": block["thinking"], "thinkingSignature": block["signature"]})
		case "tool_use":
			if name, ok := piToolNameFor(str(block["name"]), q.customToolNameToPi); ok {
				step.content = append(step.content, map[string]any{"type": "toolCall", "id": block["id"], "name": name, "arguments": mapToolArgs(name, obj(block["input"]))})
			}
		}
	}
	step.uuid = uuid
	b.completeAssistantRecordingLocked(q, step)
}

// finishedToolArgsLocked is the input Claude Code's completed tool_use carries (the
// input it runs and records, e.g. "5" converted for an integer param), nil when the
// finished block has not arrived. The caller holds q.mu.
func (q *QueryContext) finishedToolArgsLocked(id string) map[string]any {
	step := q.assistantSteps[q.turnStreamMessageID]
	if step == nil {
		return nil
	}
	for _, raw := range step.content {
		if block := obj(raw); block["type"] == "toolCall" && str(block["id"]) == id {
			return obj(deepCopy(block["arguments"]))
		}
	}
	return nil
}

// recordAssistantEmissionLocked snapshots the assistant message pi is given at done.
// SDK completed blocks can arrive before or after; equality proves the whole step
// is covered. The caller holds q.mu.
func (b *Bridge) recordAssistantEmissionLocked(q *QueryContext) {
	out := q.turnOutput
	if out == nil || out["stopReason"] == "error" || out["stopReason"] == "aborted" || q.turnEmissionRecorded {
		return
	}
	q.turnEmissionRecorded = true
	id := q.turnStreamMessageID
	if id == "" {
		id = q.lastAssistantMessageID
	}
	if id == "" {
		q.recording.Gap()
		b.debugf("recording gap: emitted assistant has no completed SDK message id")
		return
	}
	step := q.assistantSteps[id]
	if step == nil {
		step = &assistantStep{seen: map[string]bool{}}
		q.assistantSteps[id] = step
	}
	if step.emitted == nil {
		d, err := digest.Message(out)
		if err != nil {
			q.recording.Gap()
			b.debugf("recording gap: emitted assistant cannot be digested: %v", err)
			return
		}
		content := q.blocksLocked()
		toolCalls := q.toolCallIDsLocked()
		ids := append([]string{}, q.turnAssistantIDs...)
		if len(ids) == 0 {
			ids = []string{id}
		}
		template := map[string]any{}
		for k, v := range out {
			template[k] = v
		}
		template["content"] = []any{}
		step.emitted = &emittedStep{digest: d, index: q.recording.Assistant(d, toolCalls, "", len(content) > 0), template: template, ids: ids}
		for _, apiID := range ids {
			if part := q.assistantSteps[apiID]; part != nil {
				part.emitted = step.emitted
			}
		}
	}
	b.completeAssistantRecordingLocked(q, step)
}

// completeAssistantRecordingLocked names the record covering an emitted step once
// every API message it was made of has echoed and their blocks add up to what pi
// was given. The caller holds q.mu.
func (b *Bridge) completeAssistantRecordingLocked(q *QueryContext, step *assistantStep) {
	e := step.emitted
	if e == nil {
		return
	}
	var content []any
	lastUUID := ""
	for i, apiID := range e.ids {
		part := q.assistantSteps[apiID]
		if part == nil || part.uuid == "" {
			b.debugf("recording gap: emitted assistant is not fully covered by completed SDK messages (%s)", strings.Join(e.ids, ","))
			return
		}
		content = append(content, part.content...)
		if i == len(e.ids)-1 {
			lastUUID = part.uuid
		}
	}
	whole := map[string]any{}
	for k, v := range e.template {
		whole[k] = v
	}
	if content == nil {
		content = []any{}
	}
	whole["content"] = content
	d, err := digest.Message(whole)
	if err != nil || d != e.digest {
		b.debugf("recording gap: emitted assistant is not fully covered by completed SDK messages (%s)", strings.Join(e.ids, ","))
		return
	}
	q.recording.CompleteAssistant(e.index, lastUUID)
}
