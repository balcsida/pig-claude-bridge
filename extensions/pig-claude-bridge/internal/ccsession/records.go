package ccsession

import "encoding/json"

// Field order in every struct below is the key order cc-session-io writes
// (chunk-7JUXR4CP.js: baseFields at 262-281, addUserMessage, addAssistantMessage,
// addAttachment), so encoding/json reproduces JSON.stringify output.

// Base holds the fields shared by user, assistant and attachment records.
type Base struct {
	UUID        string  `json:"uuid"`
	ParentUUID  *string `json:"parentUuid"`
	SessionID   string  `json:"sessionId"`
	Timestamp   string  `json:"timestamp"`
	IsSidechain bool    `json:"isSidechain"`
	Cwd         string  `json:"cwd"`
	UserType    string  `json:"userType"`
	Version     string  `json:"version"`
	GitBranch   string  `json:"gitBranch"`
	Slug        string  `json:"slug"`
	Entrypoint  string  `json:"entrypoint"`
}

// Message is an API-shaped message; Content is a string or a []Block.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// UserRecord is a "user" line.
type UserRecord struct {
	Type string `json:"type"`
	Base
	Message Message `json:"message"`
}

// Usage is the zeroed token usage the library writes.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AssistantMessage is the payload of an assistant record.
type AssistantMessage struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []Block `json:"content"`
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        Usage   `json:"usage"`
}

// AssistantRecord is an "assistant" line.
type AssistantRecord struct {
	Type string `json:"type"`
	Base
	RequestID string           `json:"requestId"`
	Message   AssistantMessage `json:"message"`
}

// AttachmentRecord is an "attachment" line; the attachment precedes the base
// fields, as in addAttachment.
type AttachmentRecord struct {
	Type       string         `json:"type"`
	Attachment map[string]any `json:"attachment"`
	Base
}

// ImportAttachment positions an attachment after the message at AfterIndex.
type ImportAttachment struct {
	AfterIndex int
	Attachment map[string]any
}

// RawRecord is a record read from disk and re-emitted verbatim.
type RawRecord = json.RawMessage

// Block is a content block: one of the *Block types below.
type Block interface{ blockType() string }

type TextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type ToolUseBlock struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}

// ToolResultBlock content is a string or a []Block.
type ToolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   any    `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type ImageBlock struct {
	Type   string      `json:"type"`
	Source ImageSource `json:"source"`
}

func (TextBlock) blockType() string       { return "text" }
func (ThinkingBlock) blockType() string   { return "thinking" }
func (ToolUseBlock) blockType() string    { return "tool_use" }
func (ToolResultBlock) blockType() string { return "tool_result" }
func (ImageBlock) blockType() string      { return "image" }
