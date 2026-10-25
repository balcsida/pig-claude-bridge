package ccsession

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

const (
	defaultVersion   = "2.1.83"
	defaultGitBranch = "HEAD"
	defaultModel     = "claude-sonnet-4-6"
	timestampLayout  = "2006-01-02T15:04:05.000Z"
)

var adjectives = []string{
	"ancient", "bold", "bright", "calm", "clever", "cool", "crimson", "daring", "eager", "fast", "fierce",
	"gentle", "golden", "happy", "hidden", "iron", "keen", "lively", "mighty", "nimble", "pale", "proud",
	"quick", "rapid", "sharp", "silent", "smooth", "steady", "swift", "vivid", "warm", "wild",
}

var nouns = []string{
	"arrow", "badge", "beacon", "blade", "brook", "castle", "cedar", "cloud", "comet", "crest", "dawn",
	"drift", "eagle", "ember", "falcon", "flame", "forge", "gale", "grove", "harbor", "hawk", "jade",
	"kettle", "lance", "maple", "marsh", "needle", "oak", "pearl", "pine", "ridge", "river", "rune",
	"sage", "shade", "spark", "stone", "thorn", "tower", "wave",
}

// Options configures Create. Empty fields take the library's defaults.
type Options struct {
	ProjectPath, ClaudeDir, Cwd, GitBranch, Version, Model, SessionID string
}

// Session is one Claude Code session file. It holds unguarded mutable state
// and must be used from a single goroutine.
type Session struct {
	sessionID     string
	path          string
	records       []RawRecord
	pending       []RawRecord
	lastUUID      *string
	slug          string
	cwd           string
	version       string
	gitBranch     string
	model         string
	fileExists    bool
	nextTimestamp time.Time
}

// ID returns the session id.
func (s *Session) ID() string { return s.sessionID }

// Path returns the JSONL file path.
func (s *Session) Path() string { return s.path }

// Records returns saved records followed by pending ones.
func (s *Session) Records() []json.RawMessage {
	out := make([]json.RawMessage, 0, len(s.records)+len(s.pending))
	out = append(out, s.records...)
	return append(out, s.pending...)
}

// Create starts an empty session; nothing touches disk until Save.
func Create(o Options) (*Session, error) {
	if o.ProjectPath == "" {
		return nil, errors.New("ccsession: create: project path is empty")
	}
	id := o.SessionID
	if id == "" {
		var err error
		if id, err = randomUUID(); err != nil {
			return nil, fmt.Errorf("ccsession: create: %w", err)
		}
	}
	projectPath := NormalizeProjectPath(o.ProjectPath)
	cwd := projectPath
	if o.Cwd != "" {
		cwd = NormalizeProjectPath(o.Cwd)
	}
	slug, err := generateSlug()
	if err != nil {
		return nil, fmt.Errorf("ccsession: create: %w", err)
	}
	return &Session{
		sessionID:     id,
		path:          SessionPath(o.ClaudeDir, projectPath, id),
		slug:          slug,
		cwd:           cwd,
		version:       orDefault(o.Version, defaultVersion),
		gitBranch:     orDefault(o.GitBranch, defaultGitBranch),
		model:         orDefault(o.Model, defaultModel),
		nextTimestamp: time.Now().UTC().Truncate(time.Millisecond),
	}, nil
}

// recordHead is the part of a stored record that Open and the chain check read.
type recordHead struct {
	Type       string  `json:"type"`
	UUID       string  `json:"uuid"`
	ParentUUID *string `json:"parentUuid"`
	SessionID  string  `json:"sessionId"`
	Cwd        string  `json:"cwd"`
	Version    string  `json:"version"`
	GitBranch  string  `json:"gitBranch"`
	Slug       string  `json:"slug"`
}

// Open reads an existing session file. Records are kept verbatim; session
// settings come from the first user or assistant record, as in the library.
func Open(sessionID, projectPath, claudeDir string) (*Session, error) {
	path := SessionPath(claudeDir, projectPath, sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ccsession: open: %w", err)
	}
	s := &Session{
		sessionID:     sessionID,
		path:          path,
		cwd:           NormalizeProjectPath(projectPath),
		version:       defaultVersion,
		gitBranch:     defaultGitBranch,
		model:         defaultModel,
		fileExists:    true,
		nextTimestamp: time.Now().UTC().Truncate(time.Millisecond),
	}
	s.slug, err = generateSlug()
	if err != nil {
		return nil, fmt.Errorf("ccsession: open: %w", err)
	}
	seenFirst := false
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var head recordHead
		if err := json.Unmarshal([]byte(line), &head); err != nil {
			return nil, fmt.Errorf("ccsession: open: %s line %d: %w", path, i+1, err)
		}
		s.records = append(s.records, RawRecord(line))
		if !seenFirst && (head.Type == "user" || head.Type == "assistant") {
			seenFirst = true
			s.sessionID = orDefault(head.SessionID, s.sessionID)
			s.slug = orDefault(head.Slug, s.slug)
			s.cwd = orDefault(head.Cwd, s.cwd)
			s.version = orDefault(head.Version, s.version)
			s.gitBranch = orDefault(head.GitBranch, s.gitBranch)
		}
		if head.Type == "user" || head.Type == "assistant" || head.Type == "attachment" {
			uuid := head.UUID
			s.lastUUID = &uuid
		}
	}
	return s, nil
}

// Delete removes the session file and its companion directory.
func Delete(sessionID, projectPath, claudeDir string) error {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return fmt.Errorf("ccsession: delete: invalid session id %q", sessionID)
	}
	path := SessionPath(claudeDir, projectPath, sessionID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ccsession: delete: %w", err)
	}
	if err := os.RemoveAll(strings.TrimSuffix(path, ".jsonl")); err != nil {
		return fmt.Errorf("ccsession: delete: %w", err)
	}
	return nil
}

// AddUserMessage adds a user message; content is a string or a []Block.
func (s *Session) AddUserMessage(content any) error {
	if v := reflect.ValueOf(content); v.Kind() == reflect.Slice && v.Len() == 0 {
		return errors.New("ccsession: AddUserMessage: content array is empty; Anthropic rejects empty message content")
	}
	return s.appendRecord(func(base Base) any {
		return UserRecord{Type: "user", Base: base, Message: Message{Role: "user", Content: content}}
	})
}

// AddAssistantMessage adds an assistant message. An empty stopReason becomes
// "tool_use" when the content holds a tool_use block, else "end_turn".
func (s *Session) AddAssistantMessage(content []Block, stopReason string) error {
	if len(content) == 0 {
		return errors.New("ccsession: AddAssistantMessage: content array is empty")
	}
	if stopReason == "" {
		stopReason = "end_turn"
		for _, b := range content {
			if b.blockType() == "tool_use" {
				stopReason = "tool_use"
			}
		}
	}
	msgID, err := randomHex("msg_syn_", 12)
	if err != nil {
		return err
	}
	reqID, err := randomHex("req_syn_", 12)
	if err != nil {
		return err
	}
	return s.appendRecord(func(base Base) any {
		return AssistantRecord{
			Type:      "assistant",
			Base:      base,
			RequestID: reqID,
			Message: AssistantMessage{
				ID: msgID, Type: "message", Role: "assistant", Model: s.model,
				Content: content, StopReason: stopReason,
			},
		}
	})
}

// AddToolResults adds a user message holding the tool results.
func (s *Session) AddToolResults(results []ToolResultBlock) error {
	if len(results) == 0 {
		return errors.New("ccsession: AddToolResults: results are empty; Anthropic rejects empty message content")
	}
	blocks := make([]Block, len(results))
	for i, r := range results {
		r.Type = "tool_result"
		blocks[i] = r
	}
	return s.AddUserMessage(blocks)
}

// AddToolCalls adds one assistant message of tool_use blocks (stop reason
// "tool_use"), assigning a toolu_syn_ id to any call without one.
func (s *Session) AddToolCalls(calls []ToolUseBlock) error {
	blocks := make([]Block, len(calls))
	for i, c := range calls {
		c.Type = "tool_use"
		if c.ID == "" {
			id, err := randomHex("toolu_syn_", 10)
			if err != nil {
				return err
			}
			c.ID = id
		}
		blocks[i] = c
	}
	return s.AddAssistantMessage(blocks, "tool_use")
}

// AddAttachment adds an attachment record, a link in the uuid chain.
func (s *Session) AddAttachment(att map[string]any) error {
	return s.appendRecord(func(base Base) any {
		return AttachmentRecord{Type: "attachment", Attachment: att, Base: base}
	})
}

// appendRecord builds a record on the next base fields and stores it through
// appendRaw. A failed append leaves the session unchanged.
func (s *Session) appendRecord(build func(Base) any) error {
	uuid, err := randomUUID()
	if err != nil {
		return err
	}
	base := Base{
		UUID:       uuid,
		ParentUUID: s.lastUUID,
		SessionID:  s.sessionID,
		Timestamp:  s.nextTimestamp.Format(timestampLayout),
		Cwd:        s.cwd,
		UserType:   "external",
		Version:    s.version,
		GitBranch:  s.gitBranch,
		Slug:       s.slug,
		Entrypoint: "cli",
	}
	raw, err := marshalRecord(build(base))
	if err != nil {
		return err
	}
	if err := s.appendRaw(raw); err != nil {
		return err
	}
	s.nextTimestamp = s.nextTimestamp.Add(time.Millisecond)
	return nil
}

// appendRaw is the single gate for pending records: the record's parentUuid
// must be the current chain tail and its sessionId this session's, because a
// dangling parent makes Claude Code resume with an empty context silently.
func (s *Session) appendRaw(raw RawRecord) error {
	var head recordHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("ccsession: append: %w", err)
	}
	if head.SessionID != s.sessionID {
		return fmt.Errorf("ccsession: append: sessionId %q does not match session %q", head.SessionID, s.sessionID)
	}
	if !sameUUID(head.ParentUUID, s.lastUUID) {
		return fmt.Errorf("ccsession: append: parentUuid %s is not the last record's uuid %s; a broken chain makes Claude Code resume with empty context",
			describeUUID(head.ParentUUID), describeUUID(s.lastUUID))
	}
	s.pending = append(s.pending, raw)
	uuid := head.UUID
	s.lastUUID = &uuid
	return nil
}

// Save writes pending records: a new file on first save, an append afterwards.
func (s *Session) Save() error {
	if len(s.pending) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("ccsession: save: %w", err)
	}
	var data []byte
	for _, r := range s.pending {
		data = append(append(data, r...), '\n')
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if s.fileExists {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(s.path, flags, 0o644)
	if err != nil {
		return fmt.Errorf("ccsession: save: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("ccsession: save: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ccsession: save: %w", err)
	}
	s.fileExists = true
	s.records = append(s.records, s.pending...)
	s.pending = nil
	return nil
}

// marshalRecord matches JSON.stringify: no HTML escaping, no trailing newline.
func marshalRecord(v any) (RawRecord, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("ccsession: encode record: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func sameUUID(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func describeUUID(u *string) string {
	if u == nil {
		return "null"
	}
	return *u
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func randomHex(prefix string, nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ccsession: random id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ccsession: random uuid: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func generateSlug() (string, error) {
	pick := func(words []string) (string, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
		if err != nil {
			return "", fmt.Errorf("ccsession: slug: %w", err)
		}
		return words[n.Int64()], nil
	}
	a, err := pick(adjectives)
	if err != nil {
		return "", err
	}
	b, err := pick(adjectives)
	if err != nil {
		return "", err
	}
	n, err := pick(nouns)
	if err != nil {
		return "", err
	}
	return a + "-" + b + "-" + n, nil
}

// ImportMessages repairs msgs and adds them to the session, dispatching by
// role and content type as the library's importMessages does. Each attachment
// follows the record(s) of the message whose original index is AfterIndex;
// out-of-range attachments are skipped, as the library does (it only warns).
func (s *Session) ImportMessages(msgs []Message, attachments []ImportAttachment) error {
	repaired, origin := RepairWithOrigin(msgs)
	following := map[int][]ImportAttachment{}
	for _, a := range attachments {
		if a.AfterIndex >= 0 && a.AfterIndex < len(msgs) {
			following[a.AfterIndex] = append(following[a.AfterIndex], a)
		}
	}
	for i, msg := range repaired {
		if err := s.importMessage(msg); err != nil {
			return fmt.Errorf("ccsession: import message %d: %w", i, err)
		}
		if origin[i] < 0 {
			continue
		}
		for _, a := range following[origin[i]] {
			if err := s.AddAttachment(a.Attachment); err != nil {
				return fmt.Errorf("ccsession: import attachment after message %d: %w", origin[i], err)
			}
		}
	}
	return nil
}

func (s *Session) importMessage(msg Message) error {
	if msg.Role == "assistant" {
		if text, ok := msg.Content.(string); ok {
			return s.AddAssistantMessage([]Block{TextBlock{Type: "text", Text: text}}, "")
		}
		blocks, _ := msg.Content.([]Block)
		return s.AddAssistantMessage(blocks, "")
	}
	blocks, ok := msg.Content.([]Block)
	if !ok {
		return s.AddUserMessage(msg.Content)
	}
	var results []ToolResultBlock
	var rest []Block
	for _, b := range blocks {
		if tr, ok := b.(ToolResultBlock); ok {
			results = append(results, tr)
		} else {
			rest = append(rest, b)
		}
	}
	if len(results) > 0 {
		if err := s.AddToolResults(results); err != nil {
			return err
		}
	}
	if len(rest) > 0 {
		return s.AddUserMessage(rest)
	}
	return nil
}
