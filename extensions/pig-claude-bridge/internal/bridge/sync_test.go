package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

// Ports of the session-sync cases of pi-claude-bridge tests/unit-query-lifecycle.mjs
// and unit-session-sync (#222), driven through the provider with scripted fakes.

// completedTurn runs one text turn for session and returns the Claude Code session it ran.
func completedTurn(t *testing.T, h *providerHarness, session, prompt, answer string) string {
	t.Helper()
	h.script(answers("cc", answer))
	finalOf(t, h.call(context.Background(), session, msgs(userMsg(prompt))))
	h.waitLive(0)
	return ccSession(h.lastSpawn())
}

func TestSessionSyncDecisions(t *testing.T) {
	t.Run("an unchanged history reuses the session; a clean completion persists its state", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) { d.Debug = true })
		first := completedTurn(t, h, "pi-reuse", "hi", "one")
		loaded, rejected, err := LoadSessionState(h.agentDir, "pi-reuse")
		if err != nil || rejected != "" || loaded == nil || loaded.CCSessionID != first || len(loaded.Digests) != 2 {
			t.Fatalf("persisted = %+v %q %v", loaded, rejected, err)
		}
		h.script(answers("cc", "two"))
		finalOf(t, h.call(context.Background(), "pi-reuse", msgs(userMsg("hi"), sayMsg("one"), userMsg("again"))))
		if o := h.lastSpawn().opts; o.Resume != first || o.ForkSession || o.SessionID != "" {
			t.Fatalf("options = %+v", o)
		}
		if log := readDebugLog(t, h.agentDir); !strings.Contains(log, "path=reuse") {
			t.Fatalf("log:\n%s", log)
		}
	})

	t.Run("a restart reloads the persisted pointer dirty and forks at its last valid cut", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		first := completedTurn(t, h, "pi-restart", "hi", "one")
		h.b.resetSharedSession() // a restart: memory gone, the file stays
		h.script(answers("cc", "two"))
		finalOf(t, h.call(context.Background(), "pi-restart", msgs(userMsg("hi"), sayMsg("one"), userMsg("again"))))
		o := h.lastSpawn().opts
		if o.Resume != first || !o.ForkSession || o.ResumeSessionAt == "" || o.SessionID == "" || o.SessionID == first {
			t.Fatalf("a dirty pointer forks the session at its last cut: %+v", o)
		}
		h.waitLive(0)
		if st := h.b.sessionStateFor("pi-restart"); st == nil || st.CCSessionID != o.SessionID || st.DirtyTail {
			t.Fatalf("the fork's completion becomes the clean mirror: %+v", st)
		}
	})

	t.Run("a restart on another model imports pi's history instead of reusing the pointer", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		first := completedTurn(t, h, "pi-model", "hi", "one")
		h.b.resetSharedSession()
		h.script(answers("cc", "two"))
		model := map[string]any{"api": "anthropic-messages", "provider": "anthropic", "id": "claude-opus-5-5"}
		stream, _ := h.b.StreamSimple(context.Background(), h.host, model, map[string]any{"messages": msgs(userMsg("hi"), sayMsg("one"), userMsg("again"))}, map[string]any{"sessionId": "pi-model"})
		finalOf(t, stream)
		o := h.lastSpawn().opts
		if o.Resume == first || o.Resume == "" || o.ForkSession || o.SessionID != "" {
			t.Fatalf("a model change imports into a new session and resumes it: %+v", o)
		}
		data, err := os.ReadFile(h.sessionFile(o.Resume, o.Cwd))
		if err != nil || !strings.Contains(string(data), `"one"`) {
			t.Fatalf("imported transcript: %v %s", err, data)
		}
	})

	t.Run("a restart whose transcript is gone drops the persisted pointer and imports", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		first := completedTurn(t, h, "pi-gone", "hi", "one")
		h.b.resetSharedSession()
		if err := os.Remove(h.sessionFile(first, h.lastSpawn().opts.Cwd)); err != nil {
			t.Fatal(err)
		}
		h.script(answers("cc", "two"))
		finalOf(t, h.call(context.Background(), "pi-gone", msgs(userMsg("hi"), sayMsg("one"), userMsg("again"))))
		if o := h.lastSpawn().opts; o.Resume == first || o.Resume == "" || o.ForkSession {
			t.Fatalf("options = %+v", o)
		}
		if _, err := os.Stat(statePath(h.agentDir, "pi-gone")); !os.IsNotExist(err) {
			// The old pointer is deleted; the new turn persists its own.
			loaded, _, _ := LoadSessionState(h.agentDir, "pi-gone")
			if loaded == nil || loaded.CCSessionID == first {
				t.Fatalf("stale pointer survived: %+v", loaded)
			}
		}
	})

	t.Run("an invalid persisted file is rejected, logged, and treated as no state", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) { d.Debug = true })
		if err := os.MkdirAll(sessionsDir(h.agentDir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath(h.agentDir, "pi-bad"), []byte("{bad"), 0o600); err != nil {
			t.Fatal(err)
		}
		h.script(answers("cc", "one"))
		finalOf(t, h.call(context.Background(), "pi-bad", msgs(userMsg("hi"))))
		if o := h.lastSpawn().opts; o.Resume != "" || o.SessionID == "" {
			t.Fatalf("options = %+v", o)
		}
		diag, err := os.ReadFile(config.DiagLogPath(h.agentDir))
		if err != nil || !strings.Contains(string(diag), "session_state_rejected") || !strings.Contains(string(diag), "invalid JSON") {
			t.Fatalf("diag log: %s, %v", diag, err)
		}
	})

	t.Run("an edited earlier answer keeps the prefix before it and imports the rest", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) { d.Debug = true })
		first := completedTurn(t, h, "pi-edit", "hi", "one")
		h.script(answers("cc", "three"))
		finalOf(t, h.call(context.Background(), "pi-edit", msgs(userMsg("hi"), sayMsg("one (edited)"), userMsg("again"))))
		o := h.lastSpawn().opts
		if o.Resume == "" || o.Resume == first || o.ForkSession || o.SessionID != "" {
			t.Fatalf("a prefix write resumes the new file it wrote: %+v", o)
		}
		data, err := os.ReadFile(h.sessionFile(o.Resume, o.Cwd))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, `"hi"`) || !strings.Contains(text, "one (edited)") || strings.Contains(text, `"text":"one"}`) {
			t.Fatalf("the kept prefix and the imported suffix: %s", text)
		}
		if log := readDebugLog(t, h.agentDir); !strings.Contains(log, "path=prefix-write") {
			t.Fatalf("log:\n%s", log)
		}
	})

	t.Run("an unattributed query imports every time and never writes provider state", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(answers("cc", "one"))
		finalOf(t, h.call(context.Background(), "", msgs(userMsg("hi"))))
		h.script(answers("cc", "two"))
		finalOf(t, h.call(context.Background(), "", msgs(userMsg("hi"), sayMsg("one"), userMsg("again"))))
		h.waitLive(0)
		if o := h.lastSpawn().opts; o.Resume == "" || o.ForkSession {
			t.Fatalf("options = %+v", o)
		}
		if entries, _ := os.ReadDir(sessionsDir(h.agentDir)); len(entries) != 0 {
			t.Fatalf("state written for an unattributed session: %v", entries)
		}
	})

	t.Run("a pi fork or clone forks its parent's Claude Code session instead of importing", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		first := completedTurn(t, h, "pi-parent", "hi", "one")
		parentFile := filepath.Join(t.TempDir(), "parent.jsonl")
		if err := os.WriteFile(parentFile, []byte(`{"type":"session","id":"pi-parent","version":1}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.host.sessionID, h.host.cwd = "pi-child", h.lastSpawn().opts.Cwd
		h.b.OnSessionStart(h.host, "fork", parentFile)
		h.script(answers("cc", "two"))
		finalOf(t, h.call(context.Background(), "pi-child", msgs(userMsg("hi"), sayMsg("one"), userMsg("again"))))
		if o := h.lastSpawn().opts; o.Resume != first || !o.ForkSession || o.SessionID == "" {
			t.Fatalf("the child forks the parent's session: %+v", o)
		}
		if st := h.b.sessionStateFor("pi-parent"); st == nil || st.CCSessionID != first || st.DirtyTail {
			t.Fatalf("the parent's own mirror is untouched: %+v", st)
		}
	})
}

func TestParkedQueriesAndHistoryChanges(t *testing.T) {
	t.Run("a fresh same-session prompt closes the parked query and keeps its progress", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(parksOnTool("cc", "toolu_1"))
		finalOf(t, h.call(context.Background(), "pi-park", msgs(userMsg("read a")), readTool))
		parked := h.liveFor("pi-park")
		reply := callTool(context.Background(), spawnMcp{h.lastSpawn()}, "read", "toolu_1")
		waitParked(t, parked, 1)

		h.script(answers("cc", "fresh"))
		fresh := finalOf(t, h.call(context.Background(), "pi-park", msgs(userMsg("read a"), userMsg("never mind, say fresh"))))
		if replyText(fresh) != "fresh" || len(h.spawns()) != 2 {
			t.Fatalf("reply = %v spawns = %d", fresh, len(h.spawns()))
		}
		if got := receive(t, reply, "the parked handler"); !strings.Contains(got.Text, "discarded") {
			t.Fatalf("parked handler = %+v", got)
		}
		if !parked.isAbandoned() {
			t.Fatal("the parked query must be closed")
		}
	})

	t.Run("a history edited under a parked query discards it and continues from pi's history", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(parksOnTool("cc", "toolu_2"))
		ask := userMsg("read a")
		parkedReply := finalOf(t, h.call(context.Background(), "pi-diverge", msgs(ask), readTool))
		reply := callTool(context.Background(), spawnMcp{h.lastSpawn()}, "read", "toolu_2")
		waitParked(t, h.liveFor("pi-diverge"), 1)

		// An extension edited the prompt while the tool ran; the result arrives over the new history.
		h.script(answersPrompt("cc", "continued"))
		edited := msgs(userMsg("read a (edited)"), parkedReply, toolResultMsg("toolu_2"))
		continued := finalOf(t, h.call(context.Background(), "pi-diverge", edited, readTool))
		if replyText(continued) != "continued" || len(h.spawns()) != 2 {
			t.Fatalf("reply = %v spawns = %d", continued, len(h.spawns()))
		}
		if got := receive(t, reply, "the parked handler"); !strings.Contains(got.Text, "discarded") {
			t.Fatalf("parked handler = %+v", got)
		}
		if got := firstPrompt(t, h.lastSpawn()); !strings.Contains(got, "context was compacted") {
			t.Fatalf("prompt = %q", got)
		}
		data, err := os.ReadFile(h.sessionFile(h.lastSpawn().opts.Resume, h.lastSpawn().opts.Cwd))
		if err != nil || !strings.Contains(string(data), "read a (edited)") {
			t.Fatalf("the fresh query runs over pi's edited history: %v %s", err, data)
		}
	})
}
