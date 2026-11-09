package bridge

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/digest"
)

// Ports of pi-claude-bridge tests/unit-live-recording.mjs (the cases that drive
// LiveRecording directly) and the importHistory half of unit-import.mjs.

func recUser(content string) map[string]any {
	return map[string]any{"role": "user", "content": content, "timestamp": float64(1)}
}

func resultMsg(id string) map[string]any {
	return map[string]any{"role": "toolResult", "toolCallId": id, "content": []any{map[string]any{"type": "text", "text": id}}, "timestamp": float64(1)}
}

func recToolCall(ids ...string) map[string]any {
	content := []any{}
	for _, id := range ids {
		content = append(content, map[string]any{"type": "toolCall", "id": id, "name": "read", "arguments": map[string]any{}})
	}
	return map[string]any{"role": "assistant", "provider": "claude-bridge", "content": content, "timestamp": float64(1)}
}

func digests(t *testing.T, msgs ...map[string]any) []string {
	t.Helper()
	d, err := digest.Messages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func live(t *testing.T, recorded *Recording, messages ...map[string]any) *LiveRecording {
	t.Helper()
	l, err := NewLiveRecording(recorded, messages)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustUsers(t *testing.T, l *LiveRecording, messages []map[string]any, start int, uuid string, acknowledged bool) {
	t.Helper()
	if err := l.Users(messages, start, uuid, acknowledged, !acknowledged); err != nil {
		t.Fatal(err)
	}
}

func TestLiveRecording(t *testing.T) {
	t.Run("an unacknowledged steer blocks later results and assistants, not an imported null omission", func(t *testing.T) {
		l := live(t, nil)
		messages := []map[string]any{recUser("prompt"), recUser("lost"), recUser("acknowledged")}
		mustUsers(t, l, messages[:1], 0, "prompt", true)
		mustUsers(t, l, messages[:2], 1, "lost", false)
		mustUsers(t, l, messages, 2, "later", false)
		l.Acknowledge("later")
		l.Assistant("done-digest", nil, "done", true)
		r := l.Recorded()
		if !reflect.DeepEqual(r.Digests, digests(t, messages[:1]...)) || !reflect.DeepEqual(r.UUIDs, []string{"prompt"}) {
			t.Fatalf("recorded = %+v", r)
		}
	})

	t.Run("out-of-order parallel results cut only at the end of the complete batch", func(t *testing.T) {
		l := live(t, nil)
		l.Assistant("assistant", []string{"X", "Y"}, "assistant-uuid", true)
		if err := l.Results([]map[string]any{{"role": "assistant", "content": []any{}}, resultMsg("X"), resultMsg("Y")}); err != nil {
			t.Fatal(err)
		}
		l.Result("Y", "first-record")
		l.Result("X", "last-record")
		r := l.Recorded()
		if !reflect.DeepEqual(r.UUIDs, []string{"assistant-uuid", "last-record", "first-record"}) || !reflect.DeepEqual(r.Cuttable, []bool{false, false, true}) {
			t.Fatalf("recorded = %+v", r)
		}
	})

	t.Run("coalesced prompt users share a boundary but only the last can be cut", func(t *testing.T) {
		l := live(t, nil)
		mustUsers(t, l, []map[string]any{recUser("one"), recUser("two")}, 0, "prompt", true)
		r := l.Recorded()
		if !reflect.DeepEqual(r.UUIDs, []string{"prompt", "prompt"}) || !reflect.DeepEqual(r.Cuttable, []bool{false, true}) {
			t.Fatalf("recorded = %+v", r)
		}
		l.Acknowledge("prompt")
		if r := l.Recorded(); !reflect.DeepEqual(r.Cuttable, []bool{false, true}) {
			t.Fatalf("recorded = %+v", r)
		}
	})

	t.Run("overlapping identical user registrations cover one slot in either ack order, and repeated receipts are no-ops", func(t *testing.T) {
		for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
			l := live(t, nil)
			messages := []map[string]any{recUser("prompt"), recUser("steer")}
			mustUsers(t, l, messages[:1], 0, "p", true)
			mustUsers(t, l, messages, 1, "a", false)
			mustUsers(t, l, messages, 1, "b", false)
			l.Acknowledge(order[0])
			if r := l.Recorded(); !reflect.DeepEqual(r.Cuttable, []bool{true, true}) {
				t.Fatalf("%v: cuttable = %v", order, r.Cuttable)
			}
			l.Acknowledge(order[1])
			l.Acknowledge(order[0])
			want := Recording{Digests: digests(t, messages...), UUIDs: []string{"p", order[0]}, Cuttable: []bool{true, false}}
			if r := l.Recorded(); !reflect.DeepEqual(r, want) {
				t.Fatalf("%v: recorded = %+v", order, r)
			}
			if c := CompareHistory(digests(t, messages...), l.Recorded()); c.CutCount != 1 {
				t.Fatalf("%v: the cut backs up before the copied run: %+v", order, c)
			}
			if !l.HasGaps() {
				t.Fatalf("%v: CC holds a second copy no slot represents", order)
			}
			l.Assistant(digests(t, recUser("later"))[0], nil, "later", true)
			if n := len(l.Recorded().Digests); n != 2 {
				t.Fatalf("%v: no later slot is covered past the duplicate, got %d", order, n)
			}
		}
	})

	t.Run("a single receipt acknowledged twice records once and keeps coverage open", func(t *testing.T) {
		l := live(t, nil)
		mustUsers(t, l, []map[string]any{recUser("prompt")}, 0, "p", false)
		l.Acknowledge("p")
		l.Acknowledge("p")
		l.Assistant("answer", nil, "answer-uuid", true)
		r := l.Recorded()
		if !reflect.DeepEqual(r.UUIDs, []string{"p", "answer-uuid"}) || !reflect.DeepEqual(r.Cuttable, []bool{true, true}) || l.HasGaps() {
			t.Fatalf("recorded = %+v gaps=%v", r, l.HasGaps())
		}
	})

	t.Run("an old receipt never records changed content registered later at its index", func(t *testing.T) {
		old, changed := []map[string]any{recUser("original")}, []map[string]any{recUser("CHANGED")}
		l := live(t, nil)
		mustUsers(t, l, old, 0, "a", false)
		mustUsers(t, l, changed, 0, "b", false)
		l.Acknowledge("a")
		if r := l.Recorded(); !reflect.DeepEqual(r.Digests, digests(t, old...)) {
			t.Fatalf("recorded = %+v", r)
		}
		if c := CompareHistory(digests(t, changed...), l.Recorded()); c.CommonPrefix != 0 {
			t.Fatalf("compare = %+v", c)
		}
		reverse := live(t, nil)
		mustUsers(t, reverse, old, 0, "a", false)
		mustUsers(t, reverse, changed, 0, "b", false)
		reverse.Acknowledge("b")
		if n := len(reverse.Recorded().Digests); n != 0 {
			t.Fatal("the new receipt cannot claim the old snapshot")
		}
		reverse.Acknowledge("a")
		if n := len(reverse.Recorded().Digests); n != 0 {
			t.Fatal("the old copy now duplicates consumed content")
		}
	})

	t.Run("divergence checks pending input and emitted output before acknowledgement", func(t *testing.T) {
		ask := recUser("original")
		tool := recToolCall("X")
		l := live(t, nil)
		mustUsers(t, l, []map[string]any{ask}, 0, "prompt", false)
		index := l.Assistant(digests(t, tool)[0], []string{"X"}, "", true)
		check := func() {
			t.Helper()
			div := func(msgs ...map[string]any) Divergence {
				d, err := l.Divergence(msgs)
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			if d := div(ask, tool, resultMsg("X")); d.Diverged || d.Duplicate {
				t.Fatalf("unchanged history diverged: %+v", d)
			}
			if d := div(recUser("changed"), tool, resultMsg("X")); !d.Diverged || d.Index != 0 {
				t.Fatalf("changed prompt: %+v", d)
			}
			if d := div(ask); !d.Diverged || d.Index != 1 {
				t.Fatalf("truncated: %+v", d)
			}
			later := map[string]any{"role": "user", "content": "original", "timestamp": float64(2)}
			if d := div(later, tool, resultMsg("X"), recUser("steer")); d.Diverged {
				t.Fatalf("metadata and a trailing steer: %+v", d)
			}
			edited := recToolCall("X")
			edited["content"].([]any)[0].(map[string]any)["arguments"] = map[string]any{"path": "changed"}
			if d := div(ask, edited, resultMsg("X")); !d.Diverged || d.Index != 1 {
				t.Fatalf("edited tool call: %+v", d)
			}
		}
		check()
		l.Acknowledge("prompt")
		check()
		l.CompleteAssistant(index, "assistant")
		check()
	})

	t.Run("divergence checks an imported prefix and pending tool results without changing coverage", func(t *testing.T) {
		ask := recUser("imported")
		imported := Recording{Digests: digests(t, ask), UUIDs: []string{"import"}, Cuttable: []bool{true}}
		l := live(t, &imported)
		tr := resultMsg("X")
		if err := l.Results([]map[string]any{ask, tr}); err != nil {
			t.Fatal(err)
		}
		if d, _ := l.Divergence([]map[string]any{ask, tr, recUser("next")}); d.Diverged {
			t.Fatalf("%+v", d)
		}
		if d, _ := l.Divergence([]map[string]any{recUser("changed"), tr}); !d.Diverged || d.Index != 0 {
			t.Fatalf("%+v", d)
		}
		changed := resultMsg("X")
		changed["content"] = []any{map[string]any{"type": "text", "text": "changed"}}
		if d, _ := l.Divergence([]map[string]any{ask, changed}); !d.Diverged || d.Index != 1 {
			t.Fatalf("%+v", d)
		}
		if r := l.Recorded(); !reflect.DeepEqual(r, imported) {
			t.Fatalf("a live match is not a durable acknowledgement: %+v", r)
		}
		l.Result("X", "result")
		if d, _ := l.Divergence([]map[string]any{ask, tr}); d.Diverged {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("divergence rejects a gap that cannot establish the live prefix", func(t *testing.T) {
		l := live(t, nil)
		l.Gap()
		if d, _ := l.Divergence([]map[string]any{recUser("unknown")}); !d.Diverged || d.Index != 0 {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("divergence rejects an acknowledged duplicate not represented in pi history", func(t *testing.T) {
		messages := []map[string]any{recUser("prompt")}
		l := live(t, nil)
		mustUsers(t, l, messages, 0, "first", true)
		mustUsers(t, l, messages, 0, "duplicate", false)
		l.Acknowledge("duplicate")
		if d, _ := l.Divergence(messages); !d.Duplicate {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("re-registering an already covered index leaves history and pending slots untouched", func(t *testing.T) {
		l := live(t, nil)
		mustUsers(t, l, []map[string]any{recUser("prompt")}, 0, "p", true)
		recorded := l.Recorded()
		mustUsers(t, l, []map[string]any{recUser("REWRITTEN"), recUser("steer")}, 0, "s", false)
		if r := l.Recorded(); !reflect.DeepEqual(r, recorded) {
			t.Fatalf("recorded = %+v", r)
		}
		l.Acknowledge("s")
		want := recorded
		want.Cuttable = []bool{false}
		if r := l.Recorded(); !reflect.DeepEqual(r, want) || !l.HasGaps() {
			t.Fatalf("a receipt that re-sent covered content cannot extend coverage or keep its cut: %+v", r)
		}
	})
}

func TestCompareHistory(t *testing.T) {
	rec := Recording{Digests: []string{"a", "b", "c"}, UUIDs: []string{"1", "2", "3"}, Cuttable: []bool{true, false, true}}
	if c := CompareHistory([]string{"a", "b", "c"}, rec); !c.Equal || c.CutCount != 3 {
		t.Fatalf("%+v", c)
	}
	if c := CompareHistory([]string{"a", "b", "x"}, rec); c.Equal || c.CommonPrefix != 2 || c.CutCount != 1 {
		t.Fatalf("%+v", c)
	}
	if c := CompareHistory([]string{"a", "b", "c", "d"}, rec); c.Equal || c.CommonPrefix != 3 {
		t.Fatalf("%+v", c)
	}
	if err := (Recording{Digests: []string{"a"}, UUIDs: []string{""}, Cuttable: []bool{true}}).Validate(); err == nil {
		t.Fatal("a null uuid cannot be cuttable")
	}
}

func TestImportHistory(t *testing.T) {
	newSession := func(t *testing.T) *ccsession.Session {
		s, err := ccsession.Create(ccsession.Options{ProjectPath: "/work", ClaudeDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Run("every message gets the uuid of the record that covers it; cuts close only at a complete turn", func(t *testing.T) {
		s := newSession(t)
		messages := []map[string]any{recUser("hi"), recToolCall("X", "Y"), resultMsg("X"), recUser("steer"), resultMsg("Y"), recToolCall()}
		messages[5]["content"] = []any{map[string]any{"type": "text", "text": "done"}}
		rec, err := ImportHistory(s, messages, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		records := s.Records()
		if len(records) != 5 { // user, assistant, results, steer, assistant
			t.Fatalf("records = %d", len(records))
		}
		uuidOf := func(i int) string {
			var head struct {
				UUID string `json:"uuid"`
			}
			_ = json.Unmarshal(records[i], &head)
			return head.UUID
		}
		if !reflect.DeepEqual(rec.Digests, digests(t, messages...)) {
			t.Fatal("digests")
		}
		// A slot's anchor is the last write at or before it in file order: the second
		// result, written in the results record before the steer, anchors at the steer.
		wantUUIDs := []string{uuidOf(0), uuidOf(1), uuidOf(2), uuidOf(3), uuidOf(3), uuidOf(4)}
		if !reflect.DeepEqual(rec.UUIDs, wantUUIDs) {
			t.Fatalf("uuids = %v, want %v", rec.UUIDs, wantUUIDs)
		}
		// The results record collects both results, so the steer that pi recorded between
		// them and the first result are not cut points; the batch closes at the last result.
		if !reflect.DeepEqual(rec.Cuttable, []bool{true, false, false, false, true, true}) {
			t.Fatalf("cuttable = %v", rec.Cuttable)
		}
	})

	t.Run("a repair's synthetic result and an aborted assistant leave null, uncuttable slots", func(t *testing.T) {
		s := newSession(t)
		aborted := map[string]any{"role": "assistant", "provider": "claude-bridge", "content": []any{}}
		messages := []map[string]any{recUser("hi"), recToolCall("X"), aborted, recUser("again"), recToolCall()}
		messages[4]["content"] = []any{map[string]any{"type": "text", "text": "ok"}}
		rec, err := ImportHistory(s, messages, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if rec.UUIDs[2] != "" || rec.UUIDs[3] == "" {
			t.Fatalf("recording = %+v", rec)
		}
		// The call stays pending until an assistant with content closes the turn, so
		// nothing between the call and that assistant is a cut point.
		if !reflect.DeepEqual(rec.Cuttable, []bool{true, false, false, false, true}) {
			t.Fatalf("recording = %+v", rec)
		}
	})

	t.Run("a recorded tool result block is written verbatim", func(t *testing.T) {
		s := newSession(t)
		messages := []map[string]any{recToolCall("X"), resultMsg("X")}
		raw := json.RawMessage(`{"tool_use_id":"X","type":"tool_result","content":"[saved to a file]"}`)
		if _, err := ImportHistory(s, messages, nil, nil, map[string]json.RawMessage{"X": raw}); err != nil {
			t.Fatal(err)
		}
		if got := string(s.Records()[1]); !strings.Contains(got, string(raw)) {
			t.Fatalf("record = %s", got)
		}
	})
}
