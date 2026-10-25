//go:build live

package ccproto

// Port of pi-claude-bridge tests/int-session-rebuild.mjs (commit 6aa276a): the primitives
// syncSharedSession's REBUILD path depends on. Same harness and quota rules as live_contract_test.go.
//
// Case 1 ("openSession + clear + re-add: CC resolves the replaced content") is not ported:
// ccsession has no Open+Clear primitive (the bridge rebuilds by Delete+Create), and
// TestResumeRereadsTheJSONLFromDiskOnEveryCall already asserts the same thing, that a rewrite of the session
// file between two resumes is what CC sees and no stale content survives.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
)

func seedTextSession(t *testing.T, cwd, id, token string) *ccsession.Session {
	t.Helper()
	s, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: liveModel, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUserMessage("Please remember: the token is " + token + "."); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAssistantMessage([]ccsession.Block{ccsession.TextBlock{Type: "text", Text: "Got it, the token is " + token + "."}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s
}

func askToken(t *testing.T, cwd, id string) string {
	t.Helper()
	o := resumeOptions(t, cwd, id)
	o.ToolsSet, o.Tools = true, nil
	return strings.ToLower(assistantText(run(t, startLive(t, o, nil), "What token did I ask you to remember? Reply with just the word.")))
}

type recordCounts struct{ total, toolUse, toolResult int }

func countRecords(t *testing.T, path string) recordCounts {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return recordCounts{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var c recordCounts
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		c.total++
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		blocks, _ := at(rec, "message", "content").([]any)
		for _, b := range blocks {
			switch at(b, "type") {
			case "tool_use":
				c.toolUse++
			case "tool_result":
				c.toolResult++
			}
		}
	}
	return c
}

func TestDeleteSessionPlusCreateSessionSessionIdPreservedAcrossFullWipe(t *testing.T) {
	cwd, id := scratchDir(t), newUUID(t)
	seedTextSession(t, cwd, id, "ALPHA")
	if r1 := askToken(t, cwd, id); !strings.Contains(r1, "alpha") {
		t.Fatalf("expected ALPHA on first resume, got: %s", r1)
	}

	if err := ccsession.Delete(id, cwd, ""); err != nil {
		t.Fatal(err)
	}
	s2 := seedTextSession(t, cwd, id, "BETA")
	if s2.ID() != id {
		t.Fatalf("Create should preserve the session id when Options.SessionID is passed: %s != %s", s2.ID(), id)
	}
	if _, err := os.Stat(s2.Path()); err != nil {
		t.Fatalf("Save should have recreated the file at the preserved path: %v", err)
	}
	r2 := askToken(t, cwd, id)
	if !strings.Contains(r2, "beta") {
		t.Errorf("expected BETA after delete+recreate, got: %s", r2)
	}
	if strings.Contains(r2, "alpha") {
		t.Errorf("stale ALPHA returned after delete+recreate: %s", r2)
	}
}

func TestRebuildOverCCWrittenToolUseRecordsResolvesCleanly(t *testing.T) {
	cwd, id := scratchDir(t), newUUID(t)
	if err := os.WriteFile(filepath.Join(cwd, "package.json"), []byte("{\n  \"name\": \"pi-claude-bridge\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s1 := seedTextSession(t, cwd, id, "GAMMA")

	// Provoke a real tool call so CC writes tool_use/tool_result records to the session file mid-execution.
	o := resumeOptions(t, cwd, id)
	o.Tools, o.MaxTurns = []string{"Read"}, 6
	run(t, startLive(t, o, nil), "Use the Read tool to read package.json and tell me the top-level name field (one word).")

	afterToolUse := countRecords(t, s1.Path())
	if afterToolUse.toolUse == 0 {
		t.Fatalf("CC did not write tool_use records, so the test does not exercise the target path. Records: %+v", afterToolUse)
	}

	// Rebuild with the preserved session id via the primitive syncSharedSession uses.
	if err := ccsession.Delete(id, cwd, ""); err != nil {
		t.Fatal(err)
	}
	s2, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: liveModel, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.AddUserMessage("Please remember: the token is DELTA. Forget any previous tokens."); err != nil {
		t.Fatal(err)
	}
	if err := s2.AddAssistantMessage([]ccsession.Block{ccsession.TextBlock{Type: "text", Text: "Got it, the token is DELTA."}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s2.Save(); err != nil {
		t.Fatal(err)
	}

	after := countRecords(t, s2.Path())
	if after.total != 2 || after.toolUse != 0 || after.toolResult != 0 {
		t.Fatalf("rebuild should leave exactly 2 records and no tool records, got %+v", after)
	}
	r2 := askToken(t, cwd, id)
	if !strings.Contains(r2, "delta") {
		t.Errorf("expected DELTA after rebuild over tool records, got: %s", r2)
	}
	if strings.Contains(r2, "gamma") {
		t.Errorf("stale GAMMA after rebuild: %s", r2)
	}
}

func TestDeleteSessionWipesTheCompanionDirectory(t *testing.T) {
	cwd, id := scratchDir(t), newUUID(t)
	s1 := seedTextSession(t, cwd, id, "EPSILON")
	companion := strings.TrimSuffix(s1.Path(), ".jsonl")

	// Simulate CC's runtime behaviour by seeding a file inside the companion dir.
	sentinel := filepath.Join(companion, "tool-results", "sentinel.txt")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("stale artifact from a previous rebuild"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ccsession.Delete(id, cwd, ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s1.Path(), sentinel, companion} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted (stat err: %v)", p, err)
		}
	}
}
