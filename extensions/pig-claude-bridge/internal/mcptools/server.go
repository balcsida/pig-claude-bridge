// Package mcptools is the JSON-RPC side of the in-process MCP server through
// which Claude Code reaches pi's tools. It speaks only JSON-RPC; wrapping
// messages in the control protocol is the transport's job.
package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// toolUseIDMeta is the _meta key Claude Code stamps on every tools/call with
// the id of the tool_use block it came from, the only reliable way to pair a
// call with its result.
const toolUseIDMeta = "claudecode/toolUseId"

// MCP SDK 1.29.0 types.js: SUPPORTED_PROTOCOL_VERSIONS, newest first.
var supportedProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"}

const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// ToolDef is one tool the server advertises. Handler runs a tools/call with the
// tool_use id Claude Code stamped on it; ctx is cancelled when Claude Code cancels
// the request.
type ToolDef struct {
	Name, Description string
	InputSchema       map[string]any
	Handler           func(ctx context.Context, toolUseID string, args map[string]any) (convert.McpResult, error)
}

// Server is immutable after NewServer, so Handle is safe for concurrent use.
type Server struct {
	name   string
	tools  []ToolDef
	byName map[string]ToolDef
}

// NewServer rejects any tool whose input schema is not an object schema.
func NewServer(name string, tools []ToolDef) (*Server, error) {
	s := &Server{name: name, tools: slices.Clone(tools), byName: map[string]ToolDef{}}
	for _, t := range tools {
		s.byName[t.Name] = t
	}
	for _, t := range tools {
		if t.InputSchema == nil {
			return nil, fmt.Errorf("%s: MCP tool parameters must be an object schema, got undefined", t.Name)
		}
		if t.InputSchema["type"] != "object" {
			shown, _ := json.Marshal(t.InputSchema)
			return nil, fmt.Errorf("%s: MCP tool parameters must be an object schema, got %s", t.Name, shown)
		}
	}
	return s, nil
}

type rpcError struct {
	code    int
	message string
}

func (e *rpcError) Error() string { return e.message }

// Handle answers one JSON-RPC message. hasReply is false for notifications and
// for a request whose ctx was cancelled (the SDK sends nothing then either).
// err is returned only for a message that is not valid JSON-RPC.
func (s *Server) Handle(ctx context.Context, msg json.RawMessage) (reply json.RawMessage, hasReply bool, err error) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(msg, &req); err != nil {
		return nil, false, fmt.Errorf("mcptools: invalid JSON-RPC message: %w", err)
	}
	if req.Method == "" || req.ID == nil {
		return nil, false, nil // notification, or a response we never asked for
	}

	result, callErr := s.dispatch(ctx, req.Method, req.Params)
	if ctx.Err() != nil {
		return nil, false, nil
	}
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if callErr != nil {
		code := codeInternalError
		if re, ok := callErr.(*rpcError); ok {
			code = re.code
		}
		out["error"] = map[string]any{"code": code, "message": callErr.Error()}
	} else {
		out["result"] = result
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		version := supportedProtocolVersions[0]
		for _, v := range supportedProtocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": "1.0.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.list(), nil
	case "tools/call":
		return s.call(ctx, params)
	}
	return nil, &rpcError{codeMethodNotFound, "Method not found"}
}

func (s *Server) list() any {
	tools := make([]any, 0, len(s.tools))
	for _, t := range s.tools {
		schema, note := flattenRootCombinators(t.InputSchema)
		var parts []string
		for _, p := range []string{note, t.Description} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		tools = append(tools, map[string]any{"name": t.Name, "description": strings.Join(parts, "\n\n"), "inputSchema": schema})
	}
	return map[string]any{"tools": tools}
}

func (s *Server) call(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Meta      map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{codeInvalidParams, "Invalid tools/call request: params must be an object with a tool name"}
	}
	tool, ok := s.byName[p.Name]
	if !ok {
		return nil, fmt.Errorf("Unknown tool: %s", p.Name)
	}
	id, ok := p.Meta[toolUseIDMeta].(string)
	if !ok {
		return nil, fmt.Errorf("%s: tools/call is missing _meta[%q] — cannot pair the result with its tool call", tool.Name, toolUseIDMeta)
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	if tool.Handler == nil {
		return nil, fmt.Errorf("%s: tool has no handler", tool.Name)
	}
	res, err := tool.Handler(ctx, id, p.Arguments)
	if err != nil {
		return nil, err
	}
	content := res.Content
	if content == nil {
		content = convert.McpContent{}
	}
	return map[string]any{"content": content, "isError": res.IsError}, nil
}
