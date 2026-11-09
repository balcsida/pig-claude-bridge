package ccsession

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLines(t *testing.T, claudeDir, project, id string, lines ...string) string {
	t.Helper()
	path := SessionPath(claudeDir, project, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	recU1 = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s","message":{"role":"user","content":"hi"}}`
	recA1 = `{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"x","input":{}}]}}`
	recR1 = `{"type":"user","uuid":"r1","parentUuid":"a1","sessionId":"s","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"out"}]}}`
	recAt = `{"type":"attachment","uuid":"at1","parentUuid":"r1","sessionId":"s","attachment":{"type":"skill_listing"}}`
	recQ  = `{"type":"attachment","uuid":"q1","parentUuid":"at1","sessionId":"s","attachment":{"type":"queued_command","source_uuid":"steer-1"}}`
	recA2 = `{"type":"assistant","uuid":"a2","parentUuid":"q1","sessionId":"s","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`
	recAu = `{"type":"api-request","uuid":"x","request":{}}`
)

func TestReadTranscript(t *testing.T) {
	claudeDir, project := t.TempDir(), "/proj"
	t.Run("keeps terminated lines, drops audit records and a torn final line", func(t *testing.T) {
		writeLines(t, claudeDir, project, "s1", recU1, recAu, recA1, `{"type":"user","uuid":"torn"`)
		records, err := ReadTranscript("s1", project, claudeDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 2 || records[0].UUID != "u1" || records[1].UUID != "a1" {
			t.Fatalf("records = %+v", records)
		}
		if !records[0].HasParent || !records[0].parentNull || records[1].ParentUUID != "u1" {
			t.Fatalf("parent decoding: %+v", records[0])
		}
	})
	t.Run("a terminated final line that parses is kept", func(t *testing.T) {
		writeLines(t, claudeDir, project, "s2", recU1, recA1+"\n")
		records, err := ReadTranscript("s2", project, claudeDir)
		if err != nil || len(records) != 2 {
			t.Fatalf("records = %d, %v", len(records), err)
		}
	})
	t.Run("a terminated line that does not parse is an error", func(t *testing.T) {
		writeLines(t, claudeDir, project, "s3", recU1, `{"type":"user",`+"\n", recA1)
		if _, err := ReadTranscript("s3", project, claudeDir); err == nil {
			t.Fatal("a malformed committed record must be reported")
		}
	})
}

func TestResolveCut(t *testing.T) {
	claudeDir, project := t.TempDir(), "/proj"
	writeLines(t, claudeDir, project, "s", recU1, recA1, recR1, recAt, recQ, recA2)
	records, err := ReadTranscript("s", project, claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("a steer's source uuid resolves through its queued_command replay", func(t *testing.T) {
		if i := FindBoundary(records, "steer-1"); i != 4 {
			t.Fatalf("index = %d", i)
		}
	})
	t.Run("the cut at a result extends over the attachment that follows it, not over the next input", func(t *testing.T) {
		cut, err := ResolveCut(records, "r1")
		if err != nil || cut.UUID != "at1" || cut.Index != 3 {
			t.Fatalf("cut = %+v, %v", cut, err)
		}
	})
	t.Run("a boundary off the primary chain is refused", func(t *testing.T) {
		stray := records[1]
		stray.UUID, stray.ParentUUID = "side", "nowhere"
		// Inserted before the leaf: the chain is walked back from the last entry.
		withStray := append(append(append([]Record{}, records[:5]...), stray), records[5:]...)
		if _, err := ResolveCut(withStray, "side"); err == nil || !strings.Contains(err.Error(), "not on the primary parent chain") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an absent or empty boundary is refused", func(t *testing.T) {
		if _, err := ResolveCut(records, "missing"); err == nil {
			t.Fatal("absent")
		}
		if _, err := ResolveCut(records, ""); err == nil {
			t.Fatal("empty")
		}
	})
	t.Run("tool results are indexed verbatim by tool_use_id", func(t *testing.T) {
		blocks := ToolResultBlocks(records)
		if string(blocks["t1"]) != `{"tool_use_id":"t1","type":"tool_result","content":"out"}` {
			t.Fatalf("blocks = %v", blocks)
		}
	})
}

func TestSavePrefixSession(t *testing.T) {
	claudeDir, project := t.TempDir(), "/proj"
	writeLines(t, claudeDir, project, "old", recU1, recA1, recR1)
	records, err := ReadTranscript("old", project, claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Create(Options{ProjectPath: project, ClaudeDir: claudeDir, SessionID: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUserMessage("next"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAssistantMessage([]Block{TextBlock{Type: "text", Text: "ok"}}, ""); err != nil {
		t.Fatal(err)
	}
	n, err := SavePrefixSession(s, records[:2], "a1")
	if err != nil || n != 4 {
		t.Fatalf("n = %d, %v", n, err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d", len(lines))
	}
	if lines[0] != `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"new","message":{"role":"user","content":"hi"}}` {
		t.Fatalf("prefix record not restamped in place: %s", lines[0])
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &first); err != nil || first["parentUuid"] != "a1" || first["sessionId"] != "new" {
		t.Fatalf("suffix root = %s, %v", lines[2], err)
	}
	if !strings.HasPrefix(lines[2], `{"type":"user","uuid":"`) {
		t.Fatalf("suffix key order changed: %s", lines[2])
	}
	if len(s.Records()) != 2 || len(s.pending) != 0 {
		t.Fatal("the written suffix must count as saved")
	}
	if _, err := SavePrefixSession(s, nil, "a1"); err == nil {
		t.Fatal("an existing file must not be overwritten")
	}
}

func TestWithField(t *testing.T) {
	got, err := withField([]byte(`{"a":1}`), "sessionId", "x")
	if err != nil || string(got) != `{"a":1,"sessionId":"x"}` {
		t.Fatalf("%s, %v", got, err)
	}
	got, err = withField([]byte(`{}`), "k", "v")
	if err != nil || string(got) != `{"k":"v"}` {
		t.Fatalf("%s, %v", got, err)
	}
}
