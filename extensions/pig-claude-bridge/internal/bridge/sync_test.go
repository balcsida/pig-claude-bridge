package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
)

func user(text string) map[string]any { return map[string]any{"role": "user", "content": text} }
func say(text string) map[string]any {
	return map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}, "stopReason": "stop"}
}
func system(text string) map[string]any { return map[string]any{"role": "system", "content": text} }

// seed writes a Claude Code session file holding user "Hi" / assistant "Hello.".
func seed(t *testing.T, id, cwd string, attachments []ccsession.ImportAttachment) *ccsession.Session {
	t.Helper()
	s, err := ccsession.Create(ccsession.Options{SessionID: id, ProjectPath: cwd})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []ccsession.Message{
		{Role: "user", Content: "Hi"},
		{Role: "assistant", Content: []ccsession.Block{ccsession.TextBlock{Type: "text", Text: "Hello."}}},
	}
	if err := s.ImportMessages(msgs, attachments); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s
}

func recordTypes(t *testing.T, id, cwd string) []string {
	t.Helper()
	s, err := ccsession.Open(id, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, r := range s.Records() {
		var h struct{ Type string }
		if err := json.Unmarshal(r, &h); err != nil {
			t.Fatal(err)
		}
		types = append(types, h.Type)
	}
	return types
}

const id1 = "11111111-1111-4111-8111-111111111111"

func TestSyncSharedSession(t *testing.T) {
	t.Run("takes the clean-start path when a transcript system message precedes the first user message", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		res, err := b.syncSharedSession(nil, "", []map[string]any{system("You are Claude Code."), user("Hello")}, nil, t.TempDir(), "")
		if err != nil {
			t.Fatal(err)
		}
		if res.SessionID != "" || res.Action != ActionCleanStart || res.PreserveSharedSession {
			t.Fatalf("got %+v", res)
		}
		if b.sessionStateFor("") != nil {
			t.Fatal("a clean start must not create a session state")
		}
	})

	t.Run("keeps cursor arithmetic consistent when system messages punctuate the history", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		cwd := t.TempDir()
		seed(t, id1, cwd, nil)
		b.setSessionStateFor("", &SessionState{SessionID: id1, Cursor: 2, Cwd: cwd})
		msgs := []map[string]any{user("Hi"), say("Hello."), system(""), user("Next")}
		res, err := b.syncSharedSession(nil, "", msgs, nil, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != ActionReuse || res.SessionID != id1 {
			t.Fatalf("2 priors at cursor 2 must resume, got %+v", res)
		}
		if c := b.sessionStateFor("").Cursor; c != 2 {
			t.Fatalf("cursor counts non-system messages only, got %d", c)
		}
		if got := recordTypes(t, id1, cwd); fmt.Sprint(got) != "[user assistant]" {
			t.Fatalf("resumed file = %v", got)
		}
	})

	t.Run("starts a fresh session for a shorter context and preserves the parent's", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		cwd := t.TempDir()
		main := SessionState{SessionID: id1, Cursor: 42, Cwd: cwd}
		b.setSessionStateFor("", &main)
		res, err := b.syncSharedSession(nil, "", []map[string]any{user("Summarize this conversation.")}, nil, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.SessionID != "" || !res.PreserveSharedSession {
			t.Fatalf("got %+v", res)
		}
		if got := *b.sessionStateFor(""); got != main {
			t.Fatalf("parent mirror changed: %+v", got)
		}
	})

	t.Run("does not report a count mismatch when a rebuild carries an attachment", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		cwd := t.TempDir()
		prompt := "Review @fixture.txt and remember it."
		file := filepath.Join(cwd, "fixture.txt")
		s, err := ccsession.Create(ccsession.Options{SessionID: id1, ProjectPath: cwd})
		if err != nil {
			t.Fatal(err)
		}
		err = s.ImportMessages([]ccsession.Message{
			{Role: "user", Content: prompt},
			{Role: "assistant", Content: []ccsession.Block{ccsession.TextBlock{Type: "text", Text: "Noted."}}},
		}, []ccsession.ImportAttachment{{AfterIndex: 0, Attachment: map[string]any{
			"type": "file", "filename": file,
			"content": map[string]any{"type": "text", "file": map[string]any{"filePath": file, "content": "token"}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		b.setSessionStateFor("", &SessionState{SessionID: id1, Cursor: 0, Cwd: cwd})
		host := &recordingHost{}
		res, err := b.syncSharedSession(host, "", []map[string]any{user(prompt), say("Noted."), user("Now what did it say?")}, nil, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != ActionRebuild || res.SessionID != id1 {
			t.Fatalf("got %+v", res)
		}
		n := 0
		for _, ty := range recordTypes(t, id1, cwd) {
			if ty == "attachment" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("the rebuild did not carry the attachment (%d), so this proves nothing about the count", n)
		}
		if len(host.notices) != 0 {
			t.Fatalf("notices = %v", host.notices)
		}
	})
}

func TestSyncOutcomes(t *testing.T) {
	t.Run("APPEND advances the cursor past a trailing assistant and logs the reuse line", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		cwd := t.TempDir()
		b.setSessionStateFor("pi-1", &SessionState{SessionID: id1, Cursor: 1, Cwd: "/old"})
		res, err := b.syncSharedSession(nil, "pi-1", []map[string]any{user("Hi"), say("Hello."), user("Next")}, nil, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != ActionAppend || res.SessionID != id1 || res.Cursor != 2 {
			t.Fatalf("got %+v", res)
		}
		if st := b.sessionStateFor("pi-1"); st.Cursor != 2 || st.Cwd != cwd {
			t.Fatalf("state = %+v", st)
		}
		if log := readDebugLog(t, agentDir); !strings.Contains(log, "syncResult: path=reuse sessionId="+id1+" cursor=2\n") {
			t.Fatalf("log = %s", log)
		}
	})

	t.Run("REBUILD of a first turn writes a session and logs priors", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		cwd := t.TempDir()
		res, err := b.syncSharedSession(nil, "pi-1", []map[string]any{user("Hi"), say("Hello."), user("Next")}, nil, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != ActionRebuild || res.SessionID == "" || res.Cursor != 2 {
			t.Fatalf("got %+v", res)
		}
		if st := b.sessionStateFor("pi-1"); st.SessionID != res.SessionID || st.Cursor != 2 || st.PiSessionID != "pi-1" {
			t.Fatalf("state = %+v", st)
		}
		if got := recordTypes(t, res.SessionID, cwd); fmt.Sprint(got) != "[user assistant]" {
			t.Fatalf("file = %v", got)
		}
		want := "syncResult: path=rebuild sessionId=" + res.SessionID + " priors=2 first\n"
		if log := readDebugLog(t, agentDir); !strings.Contains(log, want) {
			t.Fatalf("log = %s", log)
		}
	})

	t.Run("REBUILD after a rewrite keeps the id and replaces the file", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		cwd := t.TempDir()
		seed(t, id1, cwd, nil)
		b.setSessionStateFor("pi-1", &SessionState{SessionID: id1, Cursor: 2, Cwd: cwd, NeedsRebuild: true})
		res, err := b.syncSharedSession(nil, "pi-1", []map[string]any{user("Only"), say("one"), user("Next")}, nil, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != ActionRebuild || res.SessionID != id1 {
			t.Fatalf("got %+v", res)
		}
		if st := b.sessionStateFor("pi-1"); st.NeedsRebuild {
			t.Fatal("rebuild must clear the mark")
		}
		data, _ := os.ReadFile(ccsession.SessionPath(ccsession.ClaudeDir(""), cwd, id1))
		if !strings.Contains(string(data), "one") || strings.Contains(string(data), "Hello.") {
			t.Fatalf("file not rewritten: %s", data)
		}
		if log := readDebugLog(t, agentDir); !strings.Contains(log, "priors=2 preserved\n") {
			t.Fatalf("log = %s", log)
		}
	})

	t.Run("POST_ABORT_ROTATE takes a fresh id and leaves the old file for the dying writer", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		cwd := t.TempDir()
		seed(t, id1, cwd, nil)
		b.setSessionStateFor("pi-1", &SessionState{SessionID: id1, Cursor: 2, Cwd: cwd, NeedsRebuild: true, ForceRotate: true})
		res, err := b.syncSharedSession(nil, "pi-1", []map[string]any{user("Hi"), say("Hello."), user("Next")}, nil, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != ActionPostAbortRotate || res.SessionID == "" || res.SessionID == id1 {
			t.Fatalf("got %+v", res)
		}
		if _, err := os.Stat(ccsession.SessionPath(ccsession.ClaudeDir(""), cwd, id1)); err != nil {
			t.Fatalf("the old file must stay (orphan writes land there): %v", err)
		}
		if st := b.sessionStateFor("pi-1"); st.SessionID != res.SessionID || st.ForceRotate || st.NeedsRebuild {
			t.Fatalf("state = %+v", st)
		}
		want := "syncResult: path=rebuild sessionId=" + res.SessionID + " priors=2 rotated-post-abort\n"
		if log := readDebugLog(t, agentDir); !strings.Contains(log, want) {
			t.Fatalf("log = %s", log)
		}
	})

	t.Run("a tool batch counts assistant and results as prior messages", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		cwd := t.TempDir()
		seed(t, id1, cwd, nil)
		b.setSessionStateFor("pi-1", &SessionState{SessionID: id1, Cursor: 2, Cwd: cwd})
		call := map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{
			map[string]any{"type": "toolCall", "id": "toolu_1", "name": "read", "arguments": map[string]any{"path": "a"}}}}
		result := map[string]any{"role": "toolResult", "toolCallId": "toolu_1", "toolName": "read", "isError": false,
			"content": []any{map[string]any{"type": "text", "text": "file"}}}
		msgs := []map[string]any{user("Hi"), say("Hello."), user("read a"), call, result, say("done"), user("next")}
		// Priors = 6, cursor 2: four missed messages, so the file is rebuilt, and the cursor lands on 6.
		res, err := b.syncSharedSession(nil, "pi-1", msgs, nil, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != ActionRebuild || b.sessionStateFor("pi-1").Cursor != 6 {
			t.Fatalf("got %+v cursor %d", res, b.sessionStateFor("pi-1").Cursor)
		}
		// The same priors again sit exactly at the cursor: reuse, no rebuild.
		res, err = b.syncSharedSession(nil, "pi-1", msgs, nil, cwd, "")
		if err != nil || res.Action != ActionReuse || b.sessionStateFor("pi-1").Cursor != 6 {
			t.Fatalf("got %+v err %v cursor %d", res, err, b.sessionStateFor("pi-1").Cursor)
		}
	})

	t.Run("an already-prefixed tool name surfaces as an error, not a panic", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		bad := map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{
			map[string]any{"type": "toolCall", "id": "toolu_1", "name": "mcp__custom-tools__read", "arguments": map[string]any{}}}}
		_, err := b.syncSharedSession(nil, "pi-1", []map[string]any{user("a"), bad, user("b")}, nil, t.TempDir(), "")
		if err == nil {
			t.Fatal("want error")
		}
	})
}

// TestConcurrentSessionsDoNotShareState is Review Focus 5: two goroutines sync
// distinct pi sessions 100 times; run under -race.
func TestConcurrentSessionsDoNotShareState(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	var wg sync.WaitGroup
	for _, pi := range []string{"pi-a", "pi-b"} {
		cwd := t.TempDir()
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ids []string
			for i := 0; i < 100; i++ {
				msgs := []map[string]any{user(pi + " first"), say("ok"), user(fmt.Sprintf("turn %d", i))}
				res, err := b.syncSharedSession(nil, pi, msgs, nil, cwd, "")
				if err != nil {
					t.Error(err)
					return
				}
				ids = append(ids, res.SessionID)
				if st := b.sessionStateFor(pi); st == nil || st.PiSessionID != pi && st.PiSessionID != "" || st.Cwd != cwd && st.Cwd != "" {
					t.Errorf("%s: foreign state %+v", pi, st)
					return
				}
				if i == 50 {
					b.markRebuildForSession(pi, "session_compact")
				}
			}
			if ids[0] == "" || ids[99] != ids[0] {
				t.Errorf("%s kept changing session: first %q last %q", pi, ids[0], ids[99])
			}
		}()
	}
	wg.Wait()
	a, c := b.sessionStateFor("pi-a"), b.sessionStateFor("pi-b")
	if a.SessionID == c.SessionID {
		t.Fatal("sessions share a Claude Code session")
	}
}
