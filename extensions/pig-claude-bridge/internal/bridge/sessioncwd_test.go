package bridge

import (
	"os"
	"testing"
)

func TestSessionCwd(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	wd, _ := os.Getwd()

	t.Run("returns the remembered cwd for its session only", func(t *testing.T) {
		b.rememberSessionCwd("s1", "/agents/Martin")
		defer b.forgetSessionCwd("s1")
		if got := b.sessionCwdFor("s1"); got != "/agents/Martin" {
			t.Fatalf("s1 = %q", got)
		}
		if got := b.sessionCwdFor("other"); got != wd {
			t.Fatalf("other = %q, want process cwd %q", got, wd)
		}
	})
	t.Run("falls back to the process cwd for the empty session", func(t *testing.T) {
		if got := b.sessionCwdFor(""); got != wd {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("forget removes the entry", func(t *testing.T) {
		b.rememberSessionCwd("s2", "/x")
		b.forgetSessionCwd("s2")
		if got := b.sessionCwdFor("s2"); got != wd {
			t.Fatalf("got %q", got)
		}
	})
}

// TestSummaryCwd ports unit-summary-cwd.mjs. The TypeScript drives two real pi
// sessions compacting concurrently; Go keys the pending summary by recency
// instead of AbortSignal (D7), so this pins the remember/consume contract and
// that the remembered session resolves to its own cwd.
func TestSummaryCwd(t *testing.T) {
	b, _, _ := newTestBridge(t, false)
	b.rememberSessionCwd("one", "/work/one")
	b.rememberSessionCwd("two", "/work/two")

	t.Run("concurrent summaries each use their owning session's cwd", func(t *testing.T) {
		b.rememberSummarySession("one")
		id, ok := b.summarySessionFor()
		if !ok || b.sessionCwdFor(id) != "/work/one" {
			t.Fatalf("first summary = %q %v", id, ok)
		}
		b.rememberSummarySession("two")
		id, ok = b.summarySessionFor()
		if !ok || b.sessionCwdFor(id) != "/work/two" {
			t.Fatalf("second summary = %q %v", id, ok)
		}
	})
	t.Run("a summary session is consumed once", func(t *testing.T) {
		b.rememberSummarySession("one")
		b.summarySessionFor()
		if id, ok := b.summarySessionFor(); ok {
			t.Fatalf("second read returned %q", id)
		}
	})
	t.Run("the most recent pending summary wins", func(t *testing.T) {
		b.rememberSummarySession("one")
		b.rememberSummarySession("two")
		if id, _ := b.summarySessionFor(); id != "two" {
			t.Fatalf("got %q", id)
		}
	})
}
