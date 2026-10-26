package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// The zeros are what buildModels ships: Claude Code billing is per-plan.
var testModel = map[string]any{
	"api": "anthropic-messages", "provider": "anthropic", "id": "claude-haiku-4-5",
	"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
}

// newStreamContext is a QueryContext with an open pi stream, as a provider call leaves it.
func newStreamContext(toolNames ...string) (*QueryContext, *sdk.ModelEventStream) {
	q := newQueryContext()
	stream := sdk.CreateAssistantMessageEventStream()
	q.currentPiStream = stream
	q.model = testModel
	q.resetTurnState(testModel)
	q.customToolNameToPi = map[string]string{}
	for _, n := range toolNames {
		q.customToolNameToPi["mcp__custom-tools__"+n] = n
	}
	return q, stream
}

// drain returns the events pushed and not yet read: the SDK queue hands each event
// out once, so a later drain sees only later events. A stream without a terminal
// event never closes, so the wait is bounded.
func drain(stream *sdk.ModelEventStream) []map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var events []map[string]any
	for e := range stream.Events(ctx) {
		events = append(events, e)
	}
	return events
}

func eventTypes(events []map[string]any) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i], _ = e["type"].(string)
	}
	return types
}

// readFrames reads a JSON-lines file of SDK messages.
func readFrames(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return frames
}

// replayResult is what a replay produced besides the pushed events.
type replayResult struct {
	stream          *sdk.ModelEventStream
	Events          []map[string]any
	Frames          []map[string]any
	CapturedSession string
}

// feed routes one frame as consumeQuery does for the content path: nothing is
// processed once the turn's stream is gone.
func (b *Bridge) feed(q *QueryContext, frame map[string]any, captured *string) {
	q.mu.Lock()
	open := q.currentPiStream != nil && q.turnOutput != nil
	q.mu.Unlock()
	if frame["type"] == "system" && frame["subtype"] == "init" {
		if id, _ := frame["session_id"].(string); id != "" {
			*captured = id
		}
	}
	if !open {
		return
	}
	switch frame["type"] {
	case "stream_event":
		b.processStreamEvent(q, frame)
	case "assistant":
		b.processAssistantMessage(q, frame)
	}
}

// replayFrames feeds a sdk-streams fixture through the two processors in order and
// returns the events collected from the stream the context held at the start.
func replayFrames(t *testing.T, b *Bridge, q *QueryContext, path string) replayResult {
	t.Helper()
	q.mu.Lock()
	stream := q.currentPiStream
	q.mu.Unlock()
	frames := readFrames(t, path)
	var captured string
	for _, f := range frames {
		b.feed(q, f, &captured)
	}
	return replayResult{stream: stream, Frames: frames, CapturedSession: captured, Events: drain(stream)}
}
