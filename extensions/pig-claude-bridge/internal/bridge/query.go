package bridge

import (
	"context"
	"errors"
	"sync"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// PendingToolCall is an MCP handler parked until pi delivers the tool's result.
type PendingToolCall struct {
	ToolName string
	Resolve  func(convert.McpResult)
}

// rateLimitRejection is the last rejection seen on a query: Claude Code sends it
// just before the failure it caused. Nil ResetsAt means absent.
type rateLimitRejection struct {
	RateLimitType string
	ResetsAt      *float64
}

// QueryContext is all per-query and per-turn mutable state (query-state.ts
// QueryContext). Reentrant queries each get their own.
//
// Locking: every field is guarded by mu except
//   - historyStale, guarded by Bridge.mu (markRebuildForSession sets it while it
//     walks Bridge.contexts);
//   - piSessionID, set before the context is registered in Bridge.contexts and
//     read-only afterwards, so Bridge.mu readers need no further lock.
type QueryContext struct {
	mu sync.Mutex

	// Query-scoped.
	activeQuery     ccproto.Client
	currentPiStream *sdk.ModelEventStream
	// model is the model of the turn in progress, set by resetTurnState; usage cost
	// comes from its rates.
	model map[string]any
	// customToolNameToPi maps the SDK's tool names to pi's for this query.
	customToolNameToPi map[string]string
	// completedStreams are the streams that got their terminal event (markStreamComplete).
	completedStreams map[*sdk.ModelEventStream]struct{}
	// currentOnResponse is the onResponse of the provider call that owns currentPiStream.
	currentOnResponse func(map[string]any, map[string]any) error
	// heldRateLimit is the latest rate_limit_event not yet reported; CC sends it
	// after message_stop, so it waits for the next call's stream.
	heldRateLimit    *RateLimitInfo
	latestCursor     int
	pendingToolCalls map[string]PendingToolCall
	pendingResults   map[string]convert.McpResult
	// turnToolCallIDs are the tool_use ids emitted this turn; they route a
	// delivered result to the owning query. Not reset by resetTurnState.
	turnToolCallIDs []string
	// promptStream is how steers reach CC mid-turn.
	promptStream *PromptStream
	// pendingSteerUUIDs are steers written or awaiting a write, not yet replayed
	// as incorporated into a turn.
	pendingSteerUUIDs map[string]struct{}
	// bindRunSignal binds a later run's cancellation to this query; ctx is nil-safe
	// (the TypeScript signal may be undefined).
	bindRunSignal func(ctx context.Context, runStream *sdk.ModelEventStream)
	// rateLimitRejection is nil when none was seen.
	rateLimitRejection *rateLimitRejection
	// lastRateLimitWarnStep is the highest 5% utilization bucket notified; nil
	// until one was. The threshold is nil when none was reported.
	lastRateLimitWarnStep      *int
	lastRateLimitWarnThreshold *float64
	piSessionID                string
	historyStale               bool // guarded by Bridge.mu, see above
	// missedSteer: a steer never reached CC; completion must carry this into the
	// mirror it creates.
	missedSteer bool

	// Per-turn (reset together by resetTurnState).
	turnOutput          map[string]any
	turnStarted         bool
	turnSawStreamEvent  bool
	turnSawToolCall     bool
	turnStreamMessageID string
	turnStreamOpen      bool
	// turnStreamBlockStart is len(turnBlocks) at message_start.
	turnStreamBlockStart int
}

func newQueryContext() *QueryContext {
	return &QueryContext{
		pendingToolCalls:  map[string]PendingToolCall{},
		pendingResults:    map[string]convert.McpResult{},
		pendingSteerUUIDs: map[string]struct{}{},
	}
}

// turnBlocks returns the current turn's content blocks; resetTurnState must have run.
func (c *QueryContext) turnBlocks() ([]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnBlocksLocked()
}

func (c *QueryContext) turnBlocksLocked() ([]any, error) {
	if c.turnOutput == nil {
		return nil, errors.New("turnBlocks accessed before resetTurnState")
	}
	blocks, _ := c.turnOutput["content"].([]any)
	return blocks, nil
}

// appendTurnBlock adds a block to turnOutput.content (Go slices cannot be mutated
// through a returned header, so appending goes through the context).
func (c *QueryContext) appendTurnBlock(block any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	blocks, err := c.turnBlocksLocked()
	if err != nil {
		return err
	}
	c.turnOutput["content"] = append(blocks, block)
	return nil
}

// releasePendingToolCalls answers every parked MCP handler with reason and drops
// the turn's queued results. Handlers are resolved, never dropped: one left
// waiting on a dead subprocess keeps CC's tools/call open and wedges pi's turn.
func (c *QueryContext) releasePendingToolCalls(reason string) {
	c.mu.Lock()
	pending := c.pendingToolCalls
	c.pendingToolCalls = map[string]PendingToolCall{}
	c.pendingResults = map[string]convert.McpResult{}
	c.mu.Unlock()
	for _, p := range pending {
		p.Resolve(convert.McpResult{Content: convert.McpContent{{"type": "text", "text": reason}}})
	}
}

// resetTurnState starts a fresh turn. turnToolCallIDs survives: it persists across
// tool-result deliveries within one assistant message so results keep routing here.
func (c *QueryContext) resetTurnState(model map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.model = model
	c.turnOutput = newAssistantOutput(model)
	c.turnStarted = false
	c.turnSawStreamEvent = false
	c.turnSawToolCall = false
	c.turnStreamMessageID = ""
	c.turnStreamOpen = false
	c.turnStreamBlockStart = 0
}

// newAssistantOutput is the zeroed assistant message for model (index.ts newAssistantOutput).
func newAssistantOutput(model map[string]any) map[string]any {
	return map[string]any{
		"role":     "assistant",
		"content":  []any{},
		"api":      model["api"],
		"provider": model["provider"],
		"model":    model["id"],
		"usage": map[string]any{
			"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0,
			"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0},
		},
		"stopReason": "stop",
		"timestamp":  time.Now().UnixMilli(),
	}
}

var errPromptStreamClosed = errors.New("prompt stream closed")

type promptItem struct {
	msg     ccproto.UserMessage
	ack     chan error // buffered 1, settled once
	settled bool
}

// PromptStream is the long-lived input queue feeding Claude Code's stdin (prompt-stream.ts).
// The single writer loop calls Next; Push returns once the writer has finished
// with the message, i.e. when it comes back to Next, mirroring the generator
// resuming past its yield. Callers wait on that before releasing an MCP tool
// result, which travels the same stdin FIFO.
type PromptStream struct {
	mu       sync.Mutex
	queue    []*promptItem
	inflight *promptItem // handed out by Next, not yet acked
	wake     chan struct{}
	done     bool
	failure  error
}

func newPromptStream() *PromptStream {
	return &PromptStream{wake: make(chan struct{}, 1)}
}

func (p *PromptStream) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// settle resolves an item's ack once; callers hold p.mu.
func (it *promptItem) settle(err error) {
	if it.settled {
		return
	}
	it.settled = true
	it.ack <- err
}

// Push enqueues msg and waits until the writer is done with it. It errors, never
// hangs, once the stream is ended, failed or abandoned. If ctx ends first the
// message may still be written; only the wait is abandoned.
func (p *PromptStream) Push(ctx context.Context, msg ccproto.UserMessage) error {
	p.mu.Lock()
	if p.failure != nil {
		err := p.failure
		p.mu.Unlock()
		return err
	}
	if p.done {
		p.mu.Unlock()
		return errPromptStreamClosed
	}
	it := &promptItem{msg: msg, ack: make(chan error, 1)}
	p.queue = append(p.queue, it)
	p.mu.Unlock()
	p.kick()
	select {
	case err := <-it.ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Next hands the writer the next message, first acking the previous one. It
// returns false once the stream is ended and drained, failed, or ctx is
// cancelled (abandonment: pending acks are rejected as closed).
func (p *PromptStream) Next(ctx context.Context) (ccproto.UserMessage, bool) {
	p.mu.Lock()
	if p.inflight != nil {
		// A cancelled ctx means the writer is abandoning, not finishing, the message.
		if ctx.Err() != nil {
			p.inflight.settle(errPromptStreamClosed)
		} else {
			p.inflight.settle(nil)
		}
		p.inflight = nil
	}
	for {
		if p.failure != nil {
			p.done = true
			p.mu.Unlock()
			return ccproto.UserMessage{}, false
		}
		if ctx.Err() != nil {
			p.closeLocked()
			p.mu.Unlock()
			return ccproto.UserMessage{}, false
		}
		if len(p.queue) > 0 {
			it := p.queue[0]
			p.queue = p.queue[1:]
			p.inflight = it
			p.mu.Unlock()
			return it.msg, true
		}
		if p.done {
			p.mu.Unlock()
			return ccproto.UserMessage{}, false
		}
		p.mu.Unlock()
		select {
		case <-p.wake:
		case <-ctx.Done():
		}
		p.mu.Lock()
	}
}

// closeLocked marks the stream closed and rejects what nothing will drain.
func (p *PromptStream) closeLocked() {
	p.done = true
	for _, it := range p.queue {
		it.settle(errPromptStreamClosed)
	}
	p.queue = nil
}

// End closes the input: queued messages still drain, then Next returns false.
func (p *PromptStream) End() {
	p.mu.Lock()
	p.done = true
	p.mu.Unlock()
	p.kick()
}

// Fail abandons the input, rejecting every queued and in-flight ack. The first
// failure wins: the query's cleanup fails the stream again with a generic
// reason that would otherwise mask the real cause.
func (p *PromptStream) Fail(err error) {
	if err == nil {
		err = errors.New("prompt stream failed")
	}
	p.mu.Lock()
	if p.failure != nil {
		p.mu.Unlock()
		return
	}
	p.failure = err
	for _, it := range p.queue {
		it.settle(err)
	}
	p.queue = nil
	if p.inflight != nil {
		p.inflight.settle(err)
	}
	p.mu.Unlock()
	p.kick()
}

// userMessage builds a user frame. UUID stays empty: ordinary messages need no
// dedup, and steering callers add their own.
func userMessage(content any, priority string) ccproto.UserMessage {
	return ccproto.UserMessage{Content: content, Priority: priority}
}
