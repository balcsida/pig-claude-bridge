package bridge

import (
	"reflect"
	"testing"
)

func TestCrossSessionMarks(t *testing.T) {
	const parent, child = "pi-parent", "pi-child"

	t.Run("a rewrite of one session leaves another session's mirror untouched", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.setSessionStateFor(parent, &SessionState{SessionID: "cc-P", Cursor: 2, PiSessionID: parent})
		b.setSessionStateFor(child, &SessionState{SessionID: "cc-C", Cursor: 1, PiSessionID: child})
		b.markRebuildForSession(child, "session_compact")
		if !b.sessionStateFor(child).NeedsRebuild {
			t.Fatal("the rewriting session must be marked")
		}
		if got := b.sessionStateFor(parent); got.NeedsRebuild || got.SessionID != "cc-P" {
			t.Fatalf("parent mirror changed: %+v", got)
		}
		if b.historyRewrittenFor(parent) || !b.historyRewrittenFor(child) {
			t.Fatal("only the child may be armed")
		}
	})
	t.Run("only parked queries of the rewritten session go stale", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		p, c := b.addContext(parent), b.addContext(child)
		b.markRebuildForSession(parent, "session_tree")
		if !b.isStale(p) || b.isStale(c) {
			t.Fatalf("parent stale=%v child stale=%v", b.isStale(p), b.isStale(c))
		}
	})
	t.Run("a context registered after the mark is armed by armStaleContexts", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.markRebuildForSession(parent, "session_compact")
		c := b.addContext(parent)
		b.armStaleContexts()
		if !b.isStale(c) {
			t.Fatal("context not armed")
		}
	})
	t.Run("a child's state never aliases the parent's", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.setSessionStateFor(parent, &SessionState{SessionID: "cc-P", Cursor: 2})
		got := b.sessionStateFor(parent)
		got.Cursor = 99
		if b.sessionStateFor(parent).Cursor != 2 {
			t.Fatal("sessionStateFor must return a copy")
		}
		if b.sessionStateFor(child) != nil {
			t.Fatal("child has no mirror")
		}
	})
}

// TestOrphanResultAfterRestart pins the state side of unit-orphan-result-after-restart.mjs.
// The prompt wording and the query it starts belong to the provider entry (Task 20);
// here: a restarted process has no mirror and no mark, a compaction with no
// mirror still records the mark, and an abort in one session does not orphan another.
func TestOrphanResultAfterRestart(t *testing.T) {
	t.Run("after a restart there is no mirror and no mark", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		if b.sessionStateFor("pi-restarted") != nil || b.historyRewrittenFor("pi-restarted") {
			t.Fatal("a new process must start blank")
		}
	})
	t.Run("after compaction rewrote the history, the mark is recorded without a mirror", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.markRebuildForSession("pi-compacted", "session_compact:overflow")
		if !b.historyRewrittenFor("pi-compacted") || b.sessionStateFor("pi-compacted") != nil {
			t.Fatal("mark must be recorded, and no mirror invented")
		}
	})
	t.Run("the none key is marked on its mirror but never armed", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.setSessionStateFor("", &SessionState{SessionID: "cc-none"})
		b.markRebuildForSession("", "session_compact")
		if !b.sessionStateFor("").NeedsRebuild || b.historyRewrittenFor("") || b.historyRewrittenFor(noSession) {
			t.Fatal("(none) mirror is marked, the discard set holds real ids only")
		}
	})
	t.Run("an abort mark in one session does not orphan another session's result", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.setSessionStateFor("pi-other", &SessionState{SessionID: "cc-other", NeedsRebuild: true, ForceRotate: true})
		if b.sessionStateFor("pi-mine") != nil {
			t.Fatal("pi-mine must have no mirror")
		}
		want := SessionState{SessionID: "cc-other", NeedsRebuild: true, ForceRotate: true}
		if got := *b.sessionStateFor("pi-other"); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v", got)
		}
	})
}
