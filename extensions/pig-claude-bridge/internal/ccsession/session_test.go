package ccsession

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newSession(t *testing.T) (*Session, string) {
	t.Helper()
	claudeDir := t.TempDir()
	s, err := Create(Options{ProjectPath: t.TempDir(), ClaudeDir: claudeDir, SessionID: "11111111-2222-4333-8444-555555555555"})
	if err != nil {
		t.Fatal(err)
	}
	return s, claudeDir
}

func parse(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCreateSaveAppendRoundTrip(t *testing.T) {
	s, _ := newSession(t)
	if err := s.AddUserMessage("héllo <b> & more"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAssistantMessage([]Block{TextBlock{Type: "text", Text: "hi"}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUserMessage("second"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatal("file must end with newline")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	first := lines[0]
	wantPrefix := `{"type":"user","uuid":"`
	if !strings.HasPrefix(first, wantPrefix) {
		t.Fatalf("prefix: %s", first)
	}
	keys := regexp.MustCompile(`"(type|uuid|parentUuid|sessionId|timestamp|isSidechain|cwd|userType|version|gitBranch|slug|entrypoint|message)":`).FindAllStringSubmatch(first, -1)
	var got []string
	for _, k := range keys {
		got = append(got, k[1])
	}
	want := []string{"type", "uuid", "parentUuid", "sessionId", "timestamp", "isSidechain", "cwd", "userType", "version", "gitBranch", "slug", "entrypoint", "message"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("key order %v", got)
	}
	if !strings.Contains(first, `"parentUuid":null,`) || !strings.Contains(first, `"message":{"role":"user","content":"héllo <b> & more"}}`) {
		t.Fatalf("unexpected bytes: %s", first)
	}
	for _, def := range []string{`"isSidechain":false`, `"userType":"external"`, `"version":"2.1.83"`, `"gitBranch":"HEAD"`, `"entrypoint":"cli"`} {
		if !strings.Contains(first, def) {
			t.Fatalf("missing %s in %s", def, first)
		}
	}
	asst := lines[1]
	if !regexp.MustCompile(`^\{"type":"assistant","uuid":".*"slug":"[a-z-]+","entrypoint":"cli","requestId":"req_syn_[0-9a-f]{24}","message":\{"id":"msg_syn_[0-9a-f]{24}","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":\[\{"type":"text","text":"hi"\}\],"stop_reason":"end_turn","stop_sequence":null,"usage":\{"input_tokens":0,"output_tokens":0\}\}\}$`).MatchString(asst) {
		t.Fatalf("assistant bytes: %s", asst)
	}

	// Parsed records equal what Records() reports; timestamps rise 1 ms each.
	recs := s.Records()
	if len(recs) != 3 {
		t.Fatalf("records %d", len(recs))
	}
	var prev time.Time
	for i, line := range lines {
		if !bytes.Equal(recs[i], []byte(line)) {
			t.Fatalf("record %d differs from file", i)
		}
		m := parse(t, []byte(line))
		ts, err := time.Parse("2006-01-02T15:04:05.000Z", m["timestamp"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && ts.Sub(prev) != time.Millisecond {
			t.Fatalf("timestamp step %v", ts.Sub(prev))
		}
		prev = ts
		if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(m["uuid"].(string)) {
			t.Fatalf("uuid %v", m["uuid"])
		}
	}

	if w := VerifyWrittenSession(s.Path(), s.ID(), 3); len(w) != 0 {
		t.Fatalf("verify: %v", w)
	}
}

func TestChainIntegrity(t *testing.T) {
	s, _ := newSession(t)
	steps := []func() error{
		func() error { return s.AddUserMessage("a") },
		func() error { return s.AddAttachment(map[string]any{"type": "x"}) },
		func() error {
			return s.AddAssistantMessage([]Block{ToolUseBlock{ID: "t1", Name: "N", Input: map[string]any{}}}, "")
		},
		func() error {
			return s.AddToolResults([]ToolResultBlock{{ToolUseID: "t1", Content: "ok", IsError: true}})
		},
		func() error { return s.AddToolCalls([]ToolUseBlock{{Name: "M", Input: 1}}) },
	}
	for _, f := range steps {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	var prevUUID any
	for i, r := range s.Records() {
		m := parse(t, r)
		if m["parentUuid"] != prevUUID {
			t.Fatalf("record %d parentUuid=%v want %v", i, m["parentUuid"], prevUUID)
		}
		prevUUID = m["uuid"]
	}
	recs := s.Records()
	if parse(t, recs[2])["message"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatal("stop_reason should default to tool_use")
	}
	if !strings.Contains(string(recs[3]), `"is_error":true`) {
		t.Fatalf("is_error missing: %s", recs[3])
	}
	if !regexp.MustCompile(`"id":"toolu_syn_[0-9a-f]{20}"`).Match(recs[4]) {
		t.Fatalf("tool id: %s", recs[4])
	}
	if !regexp.MustCompile(`^\{"type":"attachment","attachment":\{"type":"x"\},"uuid":`).Match(recs[1]) {
		t.Fatalf("attachment bytes: %s", recs[1])
	}
}

func TestRejectsEmptyContent(t *testing.T) {
	s, _ := newSession(t)
	for name, content := range map[string]any{
		"[]Block": []Block{}, "nil []Block": []Block(nil), "[]TextBlock": []TextBlock{},
		"[]any": []any{}, "[]map[string]any": []map[string]any{},
	} {
		if err := s.AddUserMessage(content); err == nil {
			t.Fatalf("empty user content %s accepted", name)
		}
	}
	if err := s.AddAssistantMessage(nil, ""); err == nil {
		t.Fatal("empty assistant content accepted")
	}
	if err := s.AddToolResults(nil); err == nil {
		t.Fatal("empty tool results accepted")
	}
	if len(s.Records()) != 0 {
		t.Fatal("rejected content must not be recorded")
	}
}

func TestRejectsDanglingParent(t *testing.T) {
	s, _ := newSession(t)
	if err := s.AddUserMessage("a"); err != nil {
		t.Fatal(err)
	}
	last := parse(t, s.Records()[0])["uuid"].(string)
	mk := func(parent, sid string) RawRecord {
		p := "null"
		if parent != "" {
			p = `"` + parent + `"`
		}
		return RawRecord(`{"type":"user","uuid":"new","parentUuid":` + p + `,"sessionId":"` + sid + `"}`)
	}
	if err := s.appendRaw(mk("not-the-last", s.ID())); err == nil || !strings.Contains(err.Error(), "parentUuid") {
		t.Fatalf("dangling parent: %v", err)
	}
	if err := s.appendRaw(mk("", s.ID())); err == nil {
		t.Fatal("null parent after records accepted")
	}
	if err := s.appendRaw(mk(last, "other-session")); err == nil || !strings.Contains(err.Error(), "sessionId") {
		t.Fatalf("session mismatch: %v", err)
	}
	if len(s.Records()) != 1 {
		t.Fatal("rejected records must not be stored")
	}
	if err := s.appendRaw(mk(last, s.ID())); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
}

func TestDeleteRemovesCompanionDir(t *testing.T) {
	s, claudeDir := newSession(t)
	if err := s.AddUserMessage("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	companion := strings.TrimSuffix(s.Path(), ".jsonl")
	if err := os.MkdirAll(filepath.Join(companion, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := s.cwd
	if err := Delete(s.ID(), projectPath, claudeDir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Path(), companion} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists (%v)", p, err)
		}
	}
	if err := Delete(s.ID(), projectPath, claudeDir); err != nil {
		t.Fatalf("deleting a missing session: %v", err)
	}
}

func TestOpenPreservesUnknownRecordsVerbatim(t *testing.T) {
	s, claudeDir := newSession(t)
	projectPath := s.cwd
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	unknown := `{"type":"queue-operation","z":1,  "a":[1,2],"operation":"enqueue"}`
	user := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"` + s.ID() + `","timestamp":"2026-01-01T00:00:00.000Z","isSidechain":false,"cwd":"/work","userType":"external","version":"9.9.9","gitBranch":"main","slug":"keen-wild-oak","entrypoint":"cli","message":{"role":"user","content":"x"}}`
	trailing := `{"type":"last-prompt","lastPrompt":"x"}`
	if err := os.WriteFile(s.Path(), []byte(unknown+"\n"+user+"\n"+trailing+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := Open(s.ID(), projectPath, claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	recs := o.Records()
	if len(recs) != 3 || string(recs[0]) != unknown || string(recs[2]) != trailing {
		t.Fatalf("records not verbatim: %q", recs)
	}
	if err := o.AddUserMessage("next"); err != nil {
		t.Fatal(err)
	}
	if err := o.Save(); err != nil {
		t.Fatal(err)
	}
	m := parse(t, o.Records()[3])
	if m["parentUuid"] != "u1" || m["version"] != "9.9.9" || m["gitBranch"] != "main" || m["slug"] != "keen-wild-oak" || m["cwd"] != "/work" {
		t.Fatalf("appended record did not inherit from the file: %v", m)
	}
	data, _ := os.ReadFile(o.Path())
	if !strings.HasPrefix(string(data), unknown+"\n"+user+"\n"+trailing+"\n{") {
		t.Fatalf("existing lines must be untouched, got %s", data)
	}
}

func TestOpenMissingFileErrors(t *testing.T) {
	if _, err := Open("nope", t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteRejectsUnsafeSessionID(t *testing.T) {
	s, claudeDir := newSession(t)
	if err := s.AddUserMessage("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../x", "a/b", `a\b`} {
		if err := Delete(id, s.cwd, claudeDir); err == nil {
			t.Fatalf("Delete(%q) accepted", id)
		}
	}
	if _, err := os.Stat(s.Path()); err != nil {
		t.Fatalf("session removed by rejected Delete: %v", err)
	}
}
