package ccproto

import "encoding/json"

// UserMessage is one user turn in streaming-input mode (reference section 4).
type UserMessage struct {
	Content  any
	Priority string
	UUID     string
}

// MarshalJSON emits the SDKUserMessage wire shape; Priority and UUID are omitted when empty.
func (m UserMessage) MarshalJSON() ([]byte, error) {
	type message struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	return json.Marshal(struct {
		Type            string  `json:"type"`
		SessionID       string  `json:"session_id"`
		Message         message `json:"message"`
		ParentToolUseID *string `json:"parent_tool_use_id"`
		Priority        string  `json:"priority,omitempty"`
		UUID            string  `json:"uuid,omitempty"`
	}{"user", "", message{"user", m.Content}, nil, m.Priority, m.UUID})
}
