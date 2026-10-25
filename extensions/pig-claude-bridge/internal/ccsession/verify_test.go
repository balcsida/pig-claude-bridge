package ccsession

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Cases mirror tests/unit-session-integrity.mjs "verifyWrittenSession".
func TestVerifyWrittenSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	const sid = "abc-123"
	rec := func(sessionID string, i int) string { return fmt.Sprintf(`{"sessionId":%q,"idx":%d}`, sessionID, i) }
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	single := func(t *testing.T, warnings []string, pattern string) {
		t.Helper()
		if len(warnings) != 1 {
			t.Fatalf("want 1 warning, got %v", warnings)
		}
		if !regexp.MustCompile(pattern).MatchString(warnings[0]) {
			t.Fatalf("warning %q does not match %s", warnings[0], pattern)
		}
	}

	t.Run("no warnings when file round-trips correctly", func(t *testing.T) {
		write(strings.Join([]string{rec(sid, 0), rec(sid, 1), rec(sid, 2)}, "\n") + "\n")
		if w := VerifyWrittenSession(path, sid, 3); len(w) != 0 {
			t.Fatalf("got %v", w)
		}
	})
	t.Run("warns when file is missing", func(t *testing.T) {
		single(t, VerifyWrittenSession(filepath.Join(dir, "nope.jsonl"), sid, 0), `file missing`)
	})
	t.Run("warns on record count mismatch", func(t *testing.T) {
		write(strings.Join([]string{rec(sid, 0), rec(sid, 1)}, "\n") + "\n")
		single(t, VerifyWrittenSession(path, sid, 5), `record count mismatch.*expected=5.*actual=2`)
	})
	t.Run("warns on sessionId drift", func(t *testing.T) {
		write(strings.Join([]string{rec(sid, 0), rec("different-sid", 1)}, "\n") + "\n")
		single(t, VerifyWrittenSession(path, sid, 2), `sessionId drift`)
	})
	t.Run("warns on malformed JSONL", func(t *testing.T) {
		write("not json\n")
		single(t, VerifyWrittenSession(path, sid, 1), `malformed JSONL`)
	})
}
