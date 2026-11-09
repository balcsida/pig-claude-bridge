package ccsession

// Port of pi-claude-bridge src/session-transcript.ts: reading a Claude Code
// transcript the bridge is about to keep a prefix of, resolving a cut on its
// primary parent chain, and writing a retained prefix ahead of a fresh suffix.
// Claude Code's loader recovers parallel siblings by API message id and tool-use
// id, not just parentUuid (2.1.308), so the physical prefix is kept whole,
// siblings, snapshots and attachments included; bridge changes always mint a
// fresh file.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Record is one transcript line with the fields the cut logic reads decoded.
type Record struct {
	Raw  RawRecord
	Type string
	UUID string
	// ParentUUID is the record's parent; HasParent is false when the key is absent
	// (a chain entry has it as a string or JSON null).
	ParentUUID string
	HasParent  bool
	parentNull bool
	// AttachmentType and SourceUUID are the attachment record's type and, for a
	// queued_command, the uuid of the input it replays.
	AttachmentType string
	SourceUUID     string
	// Content is message.content when it is an array, else nil.
	Content []json.RawMessage
}

// chainEntry reports whether r is a link in the uuid parent chain.
func (r Record) chainEntry() bool {
	switch r.Type {
	case "user", "assistant", "attachment", "system":
	default:
		return false
	}
	return r.UUID != "" && r.HasParent && (r.parentNull || r.ParentUUID != "")
}

// auditTypes are Claude Code's per-request log records, which it never reads back.
var auditTypes = map[string]bool{"api-request": true, "api-request-shape": true, "api-request-blob": true}

func decodeRecord(line []byte) (Record, error) {
	var head struct {
		Type       string          `json:"type"`
		UUID       string          `json:"uuid"`
		ParentUUID json.RawMessage `json:"parentUuid"`
		Attachment struct {
			Type       string `json:"type"`
			SourceUUID string `json:"source_uuid"`
		} `json:"attachment"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return Record{}, err
	}
	r := Record{Raw: append(RawRecord(nil), line...), Type: head.Type, UUID: head.UUID,
		AttachmentType: head.Attachment.Type, SourceUUID: head.Attachment.SourceUUID}
	if len(head.ParentUUID) > 0 {
		r.HasParent = true
		if bytes.Equal(head.ParentUUID, []byte("null")) {
			r.parentNull = true
		} else {
			_ = json.Unmarshal(head.ParentUUID, &r.ParentUUID)
		}
	}
	if len(head.Message.Content) > 0 && head.Message.Content[0] == '[' {
		_ = json.Unmarshal(head.Message.Content, &r.Content)
	}
	return r, nil
}

// ReadTranscript reads a session file. A record is committed by its terminating
// newline: every terminated line must parse; an unterminated final line that fails
// to parse is an append a crash cut short and is not part of the transcript. Audit
// records are dropped.
func ReadTranscript(sessionID, projectPath, claudeDir string) ([]Record, error) {
	path := SessionPath(claudeDir, projectPath, sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ccsession: read transcript: %w", err)
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	var records []Record
	for i, line := range bytes.Split(data[:end], []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		r, err := decodeRecord(line)
		if err != nil {
			return nil, fmt.Errorf("ccsession: read transcript: %s line %d: %w", path, i+1, err)
		}
		if !auditTypes[r.Type] {
			records = append(records, r)
		}
	}
	if tail := bytes.TrimSpace(data[end:]); len(tail) > 0 {
		if r, err := decodeRecord(tail); err == nil && !auditTypes[r.Type] {
			records = append(records, r)
		}
	}
	return records, nil
}

// FindBoundary is the index of the record with uuid, or of the queued_command
// attachment replaying the input with that uuid; -1 when absent.
func FindBoundary(records []Record, uuid string) int {
	for i, r := range records {
		if r.UUID == uuid || (r.Type == "attachment" && r.AttachmentType == "queued_command" && r.SourceUUID == uuid) {
			return i
		}
	}
	return -1
}

func boundaryIndex(records []Record, uuid string) (int, error) {
	if uuid == "" {
		return -1, errors.New("cannot resolve a null or empty session boundary")
	}
	i := FindBoundary(records, uuid)
	if i < 0 {
		return -1, fmt.Errorf("session boundary %s is absent from the transcript", uuid)
	}
	return i, nil
}

// Cut is a resolved boundary: the uuid to resume at and its index in the transcript.
type Cut struct {
	UUID  string
	Index int
}

// ResolveCut resolves the boundary at uuid on the primary parent chain, then extends
// it over the chain records that follow before the next input or output (an
// attachment, a system record), so the cut keeps what belongs to that boundary.
func ResolveCut(records []Record, uuid string) (Cut, error) {
	index, err := boundaryIndex(records, uuid)
	if err != nil {
		return Cut{}, err
	}
	byUUID := map[string]Record{}
	for _, r := range records {
		if r.chainEntry() {
			byUUID[r.UUID] = r
		}
	}
	path := map[string]bool{}
	var leaf *Record
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].chainEntry() {
			leaf = &records[i]
			break
		}
	}
	for leaf != nil && leaf.UUID != "" {
		if path[leaf.UUID] {
			return Cut{}, fmt.Errorf("cycle in session parent chain at %s", leaf.UUID)
		}
		path[leaf.UUID] = true
		if leaf.parentNull || leaf.ParentUUID == "" {
			break
		}
		parent, ok := byUUID[leaf.ParentUUID]
		if !ok {
			break
		}
		leaf = &parent
	}
	resolved := records[index].UUID
	if !path[resolved] {
		return Cut{}, fmt.Errorf("cut boundary %s is not on the primary parent chain", resolved)
	}
	end := index
	for i := index + 1; i < len(records); i++ {
		r := records[i]
		if r.Type == "user" || r.Type == "assistant" || r.AttachmentType == "queued_command" {
			break
		}
		if r.chainEntry() && path[r.UUID] && r.ParentUUID == resolved {
			resolved, end = r.UUID, i
		}
	}
	return Cut{UUID: resolved, Index: end}, nil
}

// ToolResultBlocks indexes every tool_result block in the transcript's user
// messages by tool_use_id, verbatim.
func ToolResultBlocks(records []Record) map[string]json.RawMessage {
	byID := map[string]json.RawMessage{}
	for _, r := range records {
		if r.Type != "user" {
			continue
		}
		for _, block := range r.Content {
			var head struct {
				Type string `json:"type"`
				ID   string `json:"tool_use_id"`
			}
			if json.Unmarshal(block, &head) == nil && head.Type == "tool_result" {
				byID[head.ID] = block
			}
		}
	}
	return byID
}

// SavePrefixSession writes prefix (retained records of another session, restamped
// with this session's id) followed by this session's records, the first of which
// is linked to parentUUID. The file must not exist. It returns the record count.
func SavePrefixSession(s *Session, prefix []Record, parentUUID string) (int, error) {
	var out [][]byte
	for _, r := range prefix {
		line, err := withField(r.Raw, "sessionId", s.sessionID)
		if err != nil {
			return 0, fmt.Errorf("ccsession: save prefix: %w", err)
		}
		out = append(out, line)
	}
	for i, raw := range s.Records() {
		line := []byte(raw)
		if i == 0 {
			var err error
			if line, err = withField(line, "parentUuid", parentUUID); err != nil {
				return 0, fmt.Errorf("ccsession: save prefix: %w", err)
			}
		}
		out = append(out, line)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return 0, fmt.Errorf("ccsession: save prefix: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, fmt.Errorf("ccsession: save prefix: %w", err)
	}
	for _, line := range out {
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			return 0, fmt.Errorf("ccsession: save prefix: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("ccsession: save prefix: %w", err)
	}
	s.fileExists = true
	s.records = append(s.records, s.pending...)
	s.pending = nil
	return len(out), nil
}

// withField sets one top-level string field of a compact JSON object, keeping the
// object's key order: an existing key is rewritten in place, a missing one is appended.
func withField(line []byte, key, value string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(value)
	quoted, _ := json.Marshal(key)
	if old, ok := fields[key]; ok {
		needle := append(append(append([]byte{}, quoted...), ':'), bytes.TrimSpace(old)...)
		if i := bytes.Index(line, needle); i >= 0 {
			return append(append(append([]byte{}, line[:i]...), append(append(quoted, ':'), encoded...)...), line[i+len(needle):]...), nil
		}
	}
	trimmed := bytes.TrimRight(line, " \t\r\n")
	if !bytes.HasSuffix(trimmed, []byte("}")) {
		return nil, errors.New("record is not a JSON object")
	}
	body := bytes.TrimSuffix(trimmed, []byte("}"))
	sep := ","
	if strings.TrimSpace(string(bytes.TrimPrefix(body, []byte("{")))) == "" {
		sep = ""
	}
	return append(append(append(body, []byte(sep)...), append(append(quoted, ':'), encoded...)...), '}'), nil
}
