package bridge

// Port of pi-claude-bridge src/session-sync.ts and the recording half of
// src/session-transcript.ts: the content coverage shared by imports and live
// queries. A "" uuid is the TypeScript's null: an intentional import omission,
// never an unacknowledged live message.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/digest"
)

// Recording is, per pi message of a Claude Code session, its digest, the uuid of
// the record that covers it ("" when none does) and whether the session can be cut
// right after it.
type Recording struct {
	Digests  []string
	UUIDs    []string
	Cuttable []bool
}

// ErrInvalidRecording is a recording that breaks the slot invariants; persisted state rejects only this.
var ErrInvalidRecording = errors.New("invalid session recording")

// Validate checks the slot invariants.
func (r Recording) Validate() error {
	if r.Digests == nil || r.UUIDs == nil || r.Cuttable == nil || len(r.Digests) != len(r.UUIDs) || len(r.Digests) != len(r.Cuttable) {
		return fmt.Errorf("%w: arrays must be present and aligned", ErrInvalidRecording)
	}
	for i := range r.Digests {
		if r.UUIDs[i] == "" && r.Cuttable[i] {
			return fmt.Errorf("%w: slot %d", ErrInvalidRecording, i)
		}
	}
	return nil
}

func emptyRecording() Recording {
	return Recording{Digests: []string{}, UUIDs: []string{}, Cuttable: []bool{}}
}

// Copy returns a detached copy; a zero Recording copies as an empty one.
func (r Recording) Copy() Recording {
	if r.Digests == nil && r.UUIDs == nil && r.Cuttable == nil {
		return emptyRecording()
	}
	return Recording{Digests: append([]string{}, r.Digests...), UUIDs: append([]string{}, r.UUIDs...), Cuttable: append([]bool{}, r.Cuttable...)}
}

// Prefix is the first count slots.
func (r Recording) Prefix(count int) Recording {
	return Recording{Digests: append([]string{}, r.Digests[:count]...), UUIDs: append([]string{}, r.UUIDs[:count]...), Cuttable: append([]bool{}, r.Cuttable[:count]...)}
}

func (r Recording) append(digest, uuid string, cuttable bool) Recording {
	return Recording{Digests: append(r.Digests, digest), UUIDs: append(r.UUIDs, uuid), Cuttable: append(r.Cuttable, cuttable)}
}

// Comparison is how pi's history relates to a recording.
type Comparison struct {
	CommonPrefix, CutCount int
	Equal                  bool
}

// CompareHistory finds the common prefix of priors and the recording, and the last
// cut at or before it.
func CompareHistory(priors []string, recorded Recording) Comparison {
	c := Comparison{}
	for c.CommonPrefix < len(priors) && c.CommonPrefix < len(recorded.Digests) && priors[c.CommonPrefix] == recorded.Digests[c.CommonPrefix] {
		c.CommonPrefix++
	}
	c.CutCount = c.CommonPrefix
	for c.CutCount > 0 && !recorded.Cuttable[c.CutCount-1] {
		c.CutCount--
	}
	c.Equal = c.CommonPrefix == len(priors) && c.CommonPrefix == len(recorded.Digests)
	return c
}

func updatePendingTools(pending map[string]struct{}, message map[string]any) {
	switch message["role"] {
	case "assistant":
		content := list(message["content"])
		if len(content) == 0 {
			return
		}
		for k := range pending {
			delete(pending, k)
		}
		for _, raw := range content {
			if block := obj(raw); block["type"] == "toolCall" {
				pending[str(block["id"])] = struct{}{}
			}
		}
	case "toolResult":
		delete(pending, str(message["toolCallId"]))
	}
}

// ImportHistory writes messages into session and returns their recording. The
// repaired messages are emitted here rather than through ImportMessages, so each
// write's uuid is known and a mixed user message's two records are both tracked.
func ImportHistory(session *ccsession.Session, messages []map[string]any, custom map[string]string, carried []convert.CarriedAttachment, results map[string]json.RawMessage) (Recording, error) {
	anthropic, sanitized, origins, err := convert.ConvertPiMessages(messages, custom, results)
	if err != nil {
		return Recording{}, fmt.Errorf("convert pi messages: %w", err)
	}
	repaired, origin := ccsession.RepairWithOrigin(anthropic)
	placed, _ := convert.PlaceCarriedAttachments(carried, repaired)
	type write struct {
		uuid    string
		sources []int
		pending int
	}
	var writes []write
	direct := map[int]bool{}
	pending := map[string]struct{}{}
	var previousSources []int
	emit := func(sources []int) { writes = append(writes, write{session.LastUUID(), sources, len(pending)}) }
	for i, message := range repaired {
		var sources []int
		if origin[i] < 0 {
			sources = previousSources
		} else {
			for _, index := range origins[origin[i]] {
				pi := messages[index]
				if pi["role"] != "toolResult" {
					sources = append(sources, index)
					continue
				}
				blocks, _ := message.Content.([]ccsession.Block)
				for _, b := range blocks {
					if id, ok := toolResultBlockID(b); ok && id == sanitized[str(pi["toolCallId"])] {
						sources = append(sources, index)
						break
					}
				}
			}
			for _, index := range sources {
				direct[index] = true
			}
		}
		if message.Role == "assistant" {
			var blocks []ccsession.Block
			if text, ok := message.Content.(string); ok {
				blocks = []ccsession.Block{ccsession.TextBlock{Type: "text", Text: text}}
			} else {
				blocks, _ = message.Content.([]ccsession.Block)
			}
			for _, b := range blocks {
				if tu, ok := b.(ccsession.ToolUseBlock); ok {
					pending[tu.ID] = struct{}{}
				}
			}
			if err := session.AddAssistantMessage(blocks, ""); err != nil {
				return Recording{}, err
			}
			emit(sources)
		} else if text, ok := message.Content.(string); ok {
			if err := session.AddUserMessage(text); err != nil {
				return Recording{}, err
			}
			emit(sources)
		} else {
			blocks, _ := message.Content.([]ccsession.Block)
			var toolResults, rest []ccsession.Block
			for _, b := range blocks {
				if id, ok := toolResultBlockID(b); ok {
					delete(pending, id)
					toolResults = append(toolResults, b)
				} else {
					rest = append(rest, b)
				}
			}
			if len(toolResults) > 0 {
				// Verbatim, so a block copied from CC keeps CC's key order.
				if err := session.AddToolResultBlocks(toolResults); err != nil {
					return Recording{}, err
				}
				emit(sources)
			}
			if len(rest) > 0 {
				if err := session.AddUserMessage(rest); err != nil {
					return Recording{}, err
				}
				emit(sources)
			}
		}
		for _, a := range placed {
			if a.AfterIndex == i {
				if err := session.AddAttachment(a.Attachment); err != nil {
					return Recording{}, err
				}
				emit(sources)
			}
		}
		previousSources = sources
	}
	digests, err := digest.Messages(messages)
	if err != nil {
		return Recording{}, err
	}
	recorded := Recording{Digests: digests, UUIDs: []string{}, Cuttable: []bool{}}
	boundary := -1
	// Repair closes CC's batches with synthetic results, but only a real pi
	// result closes the logical prefix: a later suffix must retain its call.
	piPending := map[string]struct{}{}
	for i := range messages {
		updatePendingTools(piPending, messages[i])
		for j, w := range writes {
			if containsInt(w.sources, i) && j > boundary {
				boundary = j
			}
		}
		real := direct[i] && boundary >= 0
		uuid := ""
		cuttable := false
		if real {
			uuid = writes[boundary].uuid
			cuttable = len(piPending) == 0 && writes[boundary].pending == 0
			for _, w := range writes[:boundary+1] {
				for _, index := range w.sources {
					if index > i {
						cuttable = false
					}
				}
			}
		}
		recorded.UUIDs = append(recorded.UUIDs, uuid)
		recorded.Cuttable = append(recorded.Cuttable, cuttable)
	}
	if err := recorded.Validate(); err != nil {
		return Recording{}, err
	}
	return recorded, nil
}

func toolResultBlockID(b ccsession.Block) (string, bool) {
	switch tr := b.(type) {
	case ccsession.ToolResultBlock:
		return tr.ToolUseID, true
	case ccsession.RawBlock:
		var head struct {
			ID string `json:"tool_use_id"`
		}
		_ = json.Unmarshal(tr, &head)
		return head.ID, true
	}
	return "", false
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ResolveRecordingCut resolves the cut after the first count slots of recorded in
// the transcript. Live result and replay order can differ from disk order, so every
// anchor of the logical prefix is retained, not just the last slot's own uuid.
func ResolveRecordingCut(records []ccsession.Record, recorded Recording, count int) (ccsession.Cut, error) {
	if err := recorded.Validate(); err != nil {
		return ccsession.Cut{}, err
	}
	if count == 0 || count > len(recorded.Digests) || !recorded.Cuttable[count-1] {
		return ccsession.Cut{}, fmt.Errorf("invalid session cut count %d", count)
	}
	index := -1
	for _, uuid := range recorded.UUIDs[:count] {
		if uuid == "" {
			continue
		}
		i := ccsession.FindBoundary(records, uuid)
		if i < 0 {
			return ccsession.Cut{}, fmt.Errorf("session boundary %s is absent from the transcript", uuid)
		}
		if i > index {
			index = i
		}
	}
	if index < 0 {
		return ccsession.Cut{}, errors.New("cannot resolve a null or empty session boundary")
	}
	cut, err := ccsession.ResolveCut(records, records[index].UUID)
	if err != nil {
		return ccsession.Cut{}, err
	}
	// A later anchor absent from the file (a torn final append) cannot be inside the cut.
	for _, uuid := range recorded.UUIDs[count:] {
		if uuid == "" {
			continue
		}
		if i := ccsession.FindBoundary(records, uuid); i >= 0 && i <= cut.Index {
			return ccsession.Cut{}, fmt.Errorf("session cut includes later pi content at %s", uuid)
		}
	}
	return cut, nil
}

// RecordedToolResults is the block Claude Code recorded for each tool result pi
// still holds unchanged (its digest is in recordedDigests), by pi tool call id. CC
// sends oversized output as a notice naming a saved file, while pi holds the full
// output: a rewrite from pi's form would send the model content it never saw.
func RecordedToolResults(records []ccsession.Record, recordedDigests []string, messages []map[string]any) (map[string]json.RawMessage, error) {
	byID := ccsession.ToolResultBlocks(records)
	unchanged := map[string]bool{}
	for _, d := range recordedDigests {
		unchanged[d] = true
	}
	ids := map[string]string{}
	results := map[string]json.RawMessage{}
	for _, m := range messages {
		if m["role"] != "toolResult" {
			continue
		}
		d, err := digest.Message(m)
		if err != nil {
			return nil, err
		}
		if !unchanged[d] {
			continue
		}
		piID := str(m["toolCallId"])
		id, ok := ids[piID]
		if !ok {
			id = convert.SanitizeToolID(piID)
			ids[piID] = id
		}
		if block, ok := byID[id]; ok {
			results[piID] = block
		}
	}
	return results, nil
}

// --- live recording ---

type liveSlot struct {
	digest      string
	uuid        string
	toolCalls   []string
	resultID    string
	assistant   bool
	nonCuttable bool
	resetTools  bool
}

type steerBinding struct {
	index int
	slot  *liveSlot
}

type steerReceipt struct {
	first    int
	bound    []steerBinding
	overlaps bool
	unsafe   bool
	steering bool
}

// LiveRecording extends a recording as a live query acknowledges pi content.
// Pending slots keep their pi positions even when acknowledgements arrive out of
// order; only the contiguous acknowledged prefix is exposed as Recorded. Its
// methods are safe for concurrent use.
type LiveRecording struct {
	mu       sync.Mutex
	recorded Recording
	slots    map[int]*liveSlot
	steers   map[string]*steerReceipt
	progress int
	// duplicated: CC consumed a second copy of pi content, so its file holds a
	// record no pi slot represents. Receipt order is not disk order, so every cut
	// from the copied run on may retain it; only earlier cuts stay valid.
	duplicated   bool
	resultUUIDs  map[string]string
	outstanding  map[string]struct{}
	nextIndex    int
	previousUUID string
	hasPrevious  bool
}

// NewLiveRecording starts from recorded (nil for an empty one) over messages, the
// pi history it covers, whose pending tool calls carry into the live part.
func NewLiveRecording(recorded *Recording, messages []map[string]any) (*LiveRecording, error) {
	l := &LiveRecording{slots: map[int]*liveSlot{}, steers: map[string]*steerReceipt{}, resultUUIDs: map[string]string{}, outstanding: map[string]struct{}{}}
	if recorded == nil {
		l.recorded = emptyRecording()
	} else {
		if err := recorded.Validate(); err != nil {
			return nil, err
		}
		l.recorded = recorded.Copy()
	}
	l.nextIndex = len(l.recorded.Digests)
	for i := 0; i < l.nextIndex && i < len(messages); i++ {
		updatePendingTools(l.outstanding, messages[i])
	}
	return l, nil
}

// Recorded is a copy of the acknowledged prefix.
func (l *LiveRecording) Recorded() Recording {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recorded.Copy()
}

// HasProgress reports whether a complete assistant step was recorded.
func (l *LiveRecording) HasProgress() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.progress > 0
}

// HasGaps reports pi content Claude Code holds that no recorded slot represents.
func (l *LiveRecording) HasGaps() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.duplicated || l.nextIndex > len(l.recorded.Digests)
}

// Gap records an unrecordable live message (an emitted assistant with no SDK id).
func (l *LiveRecording) Gap() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextIndex++
}

// AssistantObserved marks every pending steer unsafe to cut at: a later-turn steer
// follows output pi merges into its next assistant, and cutting at that steer would
// keep the unrepresented earlier output twice.
func (l *LiveRecording) AssistantObserved() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.steers {
		if s.steering {
			s.unsafe = true
		}
	}
}

func (l *LiveRecording) duplicate(first int) {
	l.duplicated = true
	for i := first; i < len(l.recorded.Cuttable); i++ {
		l.recorded.Cuttable[i] = false
	}
}

// Divergence is where pi's live history first leaves this recording: Index when
// Diverged, Duplicate when CC holds a record no pi slot represents, neither while
// the history still continues it. Live equality includes pending slots.
type Divergence struct {
	Diverged  bool
	Index     int
	Duplicate bool
}

func (l *LiveRecording) Divergence(messages []map[string]any) (Divergence, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.duplicated {
		return Divergence{Duplicate: true}, nil
	}
	for i := 0; i < l.nextIndex; i++ {
		d := ""
		if i < len(l.recorded.Digests) {
			d = l.recorded.Digests[i]
		} else if s, ok := l.slots[i]; ok {
			d = s.digest
		}
		if d == "" || i >= len(messages) {
			return Divergence{Diverged: true, Index: i}, nil
		}
		got, err := digest.Message(messages[i])
		if err != nil {
			return Divergence{}, err
		}
		if got != d {
			return Divergence{Diverged: true, Index: i}, nil
		}
	}
	return Divergence{}, nil
}

// Users binds a receipt (the prompt or a steer written under uuid) to the exact
// user slots it delivered from start on. An index already covered, or pending with
// other content, keeps its first snapshot; an identical pending slot is shared, and
// whichever receipt is acknowledged first owns it.
func (l *LiveRecording) Users(messages []map[string]any, start int, uuid string, acknowledged bool, steering bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	receipt := &steerReceipt{first: len(messages), steering: steering}
	for i := start; i < len(messages); i++ {
		if messages[i]["role"] != "user" {
			continue
		}
		if i < receipt.first {
			receipt.first = i
		}
		d, err := digest.Message(messages[i])
		if err != nil {
			return err
		}
		existing := l.slots[i]
		if i < len(l.recorded.Digests) || (existing != nil && (existing.digest != d || existing.uuid != "")) {
			receipt.overlaps = true
		} else if existing != nil {
			receipt.bound = append(receipt.bound, steerBinding{i, existing})
		} else {
			slot := &liveSlot{digest: d}
			l.slots[i] = slot
			receipt.bound = append(receipt.bound, steerBinding{i, slot})
		}
	}
	if len(messages) > l.nextIndex {
		l.nextIndex = len(messages)
	}
	l.steers[uuid] = receipt
	if acknowledged {
		l.acknowledgeLocked(uuid)
	} else {
		l.flush()
	}
	return nil
}

// Acknowledge records a receipt CC took into a turn. Repeated or unknown receipts
// are no-ops; it reports whether this one was pending.
func (l *LiveRecording) Acknowledge(uuid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acknowledgeLocked(uuid)
}

func (l *LiveRecording) acknowledgeLocked(uuid string) bool {
	receipt, ok := l.steers[uuid]
	if !ok {
		return false
	}
	delete(l.steers, uuid)
	if receipt.overlaps {
		l.duplicate(receipt.first)
	}
	for _, b := range receipt.bound {
		if l.slots[b.index] != b.slot || b.slot.uuid != "" {
			l.duplicate(receipt.first)
			continue
		}
		b.slot.uuid = uuid
		b.slot.nonCuttable = receipt.unsafe
	}
	l.flush()
	return true
}

// Assistant takes the next slot for an emitted assistant step; uuid may be ""
// until CompleteAssistant names it. resetTools false keeps the outstanding tool
// calls (an assistant with no content).
func (l *LiveRecording) Assistant(digest string, toolCalls []string, uuid string, resetTools bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	index := l.nextIndex
	l.nextIndex++
	l.slots[index] = &liveSlot{digest: digest, uuid: uuid, toolCalls: append([]string{}, toolCalls...), assistant: true, resetTools: resetTools}
	l.flush()
	return index
}

// CompleteAssistant names the record covering the assistant slot at index.
func (l *LiveRecording) CompleteAssistant(index int, uuid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.slots[index]; ok {
		s.uuid = uuid
	}
	l.flush()
}

// Results registers the tool results pi delivered, at their pi positions.
func (l *LiveRecording) Results(messages []map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.recorded.Digests); i < len(messages); i++ {
		m := messages[i]
		if m["role"] != "toolResult" {
			continue
		}
		if _, ok := l.slots[i]; ok {
			continue
		}
		d, err := digest.Message(m)
		if err != nil {
			return err
		}
		id := str(m["toolCallId"])
		l.slots[i] = &liveSlot{digest: d, uuid: l.resultUUIDs[id], resultID: id}
	}
	if len(messages) > l.nextIndex {
		l.nextIndex = len(messages)
	}
	l.flush()
	return nil
}

// Result names the record CC wrote for tool call id.
func (l *LiveRecording) Result(id, uuid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resultUUIDs[id] = uuid
	for _, s := range l.slots {
		if s.resultID == id {
			s.uuid = uuid
		}
	}
	l.flush()
}

func (l *LiveRecording) flush() {
	for !l.duplicated {
		i := len(l.recorded.Digests)
		s, ok := l.slots[i]
		if !ok || s.uuid == "" {
			return
		}
		if s.resetTools {
			for k := range l.outstanding {
				delete(l.outstanding, k)
			}
		}
		for _, id := range s.toolCalls {
			l.outstanding[id] = struct{}{}
		}
		if s.resultID != "" {
			delete(l.outstanding, s.resultID)
		}
		// Each live slot stores its own anchor. Stream acknowledgement order is not
		// physical file order; cut resolution derives the group boundary from all
		// retained anchors, including steer source_uuid aliases.
		if i > 0 && l.hasPrevious && l.previousUUID == s.uuid {
			l.recorded.Cuttable[i-1] = false
		}
		cuttable := len(l.outstanding) == 0 && !s.nonCuttable
		l.recorded = l.recorded.append(s.digest, s.uuid, cuttable)
		l.previousUUID, l.hasPrevious = s.uuid, true
		if s.assistant {
			l.progress++
		}
		delete(l.slots, i)
	}
}

// sortedUUIDs is a debug helper: the pending receipts in uuid order.
func (l *LiveRecording) pendingReceipts() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.steers))
	for u := range l.steers {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}
