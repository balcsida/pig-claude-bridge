package ccproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
)

// frameHead is the part of a stdout frame the transport inspects.
type frameHead struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  *struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	} `json:"response"`
}

type controlResult struct {
	payload json.RawMessage
	err     error
}

// newRequestID mirrors Math.random().toString(36).slice(2, 15): up to 13 base36 characters.
func newRequestID() string { return strconv.FormatUint(rand.Uint64(), 36) }

func marshalFrame(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (p *Process) writeJSON(v any) error {
	line, err := marshalFrame(v)
	if err != nil {
		return err
	}
	return p.write(line)
}

// register reserves a response slot for an outbound request id.
func (p *Process) register(id string) (chan controlResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdoutClosed {
		return nil, errors.New("claude process closed")
	}
	ch := make(chan controlResult, 1)
	p.pending[id] = ch
	return ch, nil
}

func (p *Process) unregister(id string) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

// request sends a control request and waits for its response; on ctx cancellation it sends control_cancel_request.
func (p *Process) request(ctx context.Context, request any) (json.RawMessage, error) {
	id := newRequestID()
	ch, err := p.register(id)
	if err != nil {
		return nil, err
	}
	if err := p.writeJSON(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		p.unregister(id)
		return nil, err
	}
	select {
	case r := <-ch:
		return r.payload, r.err
	case <-ctx.Done():
		p.unregister(id)
		_ = p.writeJSON(map[string]any{"type": "control_cancel_request", "request_id": id})
		return nil, ctx.Err()
	}
}

// deliver hands a control_response to its waiter; responses with ids that are not ours (CLI echoes) are dropped.
func (p *Process) deliver(h frameHead) {
	if h.Response == nil {
		return
	}
	p.mu.Lock()
	ch, ok := p.pending[h.Response.RequestID]
	delete(p.pending, h.Response.RequestID)
	p.mu.Unlock()
	if !ok {
		return
	}
	if h.Response.Subtype == "error" {
		ch <- controlResult{err: errors.New(h.Response.Error)}
		return
	}
	ch <- controlResult{payload: h.Response.Response}
}

// failPending rejects every outstanding request once stdout is gone.
func (p *Process) failPending() {
	p.mu.Lock()
	pending := p.pending
	p.pending = map[string]chan controlResult{}
	p.stdoutClosed = true
	p.mu.Unlock()
	for _, ch := range pending {
		ch <- controlResult{err: errors.New("claude process closed before responding")}
	}
}

// Interrupt sends the interrupt control request and returns the raw response payload.
func (p *Process) Interrupt(ctx context.Context) (json.RawMessage, error) {
	return p.request(ctx, map[string]any{"subtype": "interrupt", "cancel_queued": true})
}

func (p *Process) sendSuccess(id string, payload any) {
	_ = p.writeJSON(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": payload}})
}

func (p *Process) sendError(id, text string) {
	_ = p.writeJSON(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "error", "request_id": id, "error": text}})
}

// serveRequest starts a goroutine for one inbound control request; duplicate in-flight ids are ignored.
func (p *Process) serveRequest(h frameHead) {
	p.mu.Lock()
	if _, dup := p.inflight[h.RequestID]; dup {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.inflight[h.RequestID] = cancel
	p.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			p.mu.Lock()
			delete(p.inflight, h.RequestID)
			p.mu.Unlock()
		}()
		// A panicking handler must not kill the host process; Claude Code is answered instead of left waiting.
		defer func() {
			if r := recover(); r != nil {
				p.sendError(h.RequestID, fmt.Sprintf("claude-bridge: panic serving control request: %v", r))
			}
		}()
		p.handleRequest(ctx, h)
	}()
}

func (p *Process) cancelInflight(id string) {
	p.mu.Lock()
	cancel := p.inflight[id]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *Process) handleRequest(ctx context.Context, h frameHead) {
	var req struct {
		Subtype    string          `json:"subtype"`
		ServerName string          `json:"server_name"`
		Message    json.RawMessage `json:"message"`
		CallbackID string          `json:"callback_id"`
	}
	if err := json.Unmarshal(h.Request, &req); err != nil {
		p.sendError(h.RequestID, "Invalid control request: "+err.Error())
		return
	}
	switch req.Subtype {
	case "mcp_message":
		p.handleMcp(ctx, h.RequestID, req.ServerName, req.Message)
	case "elicitation":
		p.sendSuccess(h.RequestID, map[string]string{"action": "decline"})
	case "can_use_tool":
		p.sendError(h.RequestID, "canUseTool callback is not provided.")
	case "hook_callback":
		p.sendError(h.RequestID, "No hook callback found for ID: "+req.CallbackID)
	case "request_user_dialog", "remote_tool_call", "remote_plumbing_call", "remote_tools_probe", "remote_tools_reannounce":
		// left unanswered, as the SDK leaves them (reference 2 and 2.7)
	default:
		p.sendError(h.RequestID, "Unsupported control request subtype: "+req.Subtype)
	}
}

func (p *Process) handleMcp(ctx context.Context, id, server string, message json.RawMessage) {
	if p.handlers.OnMcpMessage == nil {
		p.sendError(id, "SDK MCP server not found: "+server)
		return
	}
	reply, hasReply, err := p.handlers.OnMcpMessage(ctx, server, message)
	switch {
	case err != nil:
		p.sendError(id, err.Error())
	case hasReply:
		p.sendSuccess(id, map[string]json.RawMessage{"mcp_response": reply})
	default:
		p.sendSuccess(id, map[string]json.RawMessage{"mcp_response": json.RawMessage(`{"jsonrpc":"2.0","result":{},"id":0}`)})
	}
}
