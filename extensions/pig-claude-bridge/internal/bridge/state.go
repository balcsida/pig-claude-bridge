package bridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// Per-session state (index.ts "Session persistence"). One mirror per pi session:
// a bridge process serves several sessions at once, and each session's reads,
// writes and teardown marks touch only its own. Mirrors are pointers so a query
// can hold the identity it synced from and compare-and-set against it at commit
// (a stale completion never overwrites a replacement). Only DirtyTail is ever
// written in place; everything else is replaced whole.

// sessionStateFor returns the mirror for piSessionID, or nil when it has none. A pi
// session reads its persisted pointer on first use (restart, /reload). Unattributed
// calls ("" id) cannot safely share a mirror, so they have none and always import.
func (b *Bridge) sessionStateFor(piSessionID string) *SessionState {
	if piSessionID == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.loaded[piSessionID] {
		b.known[piSessionID] = true
		b.loaded[piSessionID] = true
		stored, rejected, err := LoadSessionState(b.deps.AgentDir, piSessionID)
		if err != nil {
			b.debugf("session state: %s: %v", piSessionID, err)
		}
		if rejected != "" {
			b.diagDump("session_state_rejected", map[string]any{"piSessionId": piSessionID, "message": "Rejected persisted Claude session state: " + rejected})
		}
		if stored != nil {
			b.sessions[piSessionID] = stored
		}
	}
	return b.sessions[piSessionID]
}

// setSessionStateFor replaces (or plants) the mirror; nil removes it.
func (b *Bridge) setSessionStateFor(piSessionID string, state *SessionState) {
	if piSessionID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.known[piSessionID] = true
	b.loaded[piSessionID] = true
	if state == nil {
		delete(b.sessions, piSessionID)
	} else {
		b.sessions[piSessionID] = state
	}
}

// inheritSessionState plants the parent's mirror, dirty, for a pi fork or clone, so
// its first turn forks the parent's Claude Code session instead of importing pi's
// history. Memory only: the parent's own pointer is saved once its child exits.
func (b *Bridge) inheritSessionState(parentFile, piSessionID string) error {
	f, err := os.Open(parentFile)
	if err != nil {
		// pi writes a session file at its first message, so a parent without one has no state.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return fmt.Errorf("no session header in %s", parentFile)
	}
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(line, &header) != nil || header.Type != "session" || header.ID == "" {
		return fmt.Errorf("no session header in %s", parentFile)
	}
	parent := b.sessionStateFor(header.ID)
	if parent == nil {
		return nil
	}
	inherited := *parent
	inherited.Recording = parent.Recording.Copy()
	inherited.DirtyTail = true
	b.setSessionStateFor(piSessionID, &inherited)
	b.debugf("session_start:fork: inherited %s from piSession=%s piSession=%s", short(parent.CCSessionID), header.ID, piSessionID)
	return nil
}

// markRebuildForSession records that pi rewrote piSessionID's history (compact,
// tree): its mirror's tail is dirty and parked queries built from it go stale.
// Other sessions' mirrors are untouched; an unattributed rewrite marks nothing.
func (b *Bridge) markRebuildForSession(piSessionID, event string) {
	state := b.sessionStateFor(piSessionID)
	b.mu.Lock()
	if state != nil {
		state.DirtyTail = true
	}
	if piSessionID != "" {
		b.historyRewritten[piSessionID] = struct{}{}
	}
	b.armStaleContextsLocked()
	b.mu.Unlock()
	if state == nil {
		b.debugf("%s: history rewritten, no session to mark yet piSession=%s", event, orNone(piSessionID))
		return
	}
	b.debugf("%s: marking dirtyTail on session %s piSession=%s", event, short(state.CCSessionID), piSessionID)
}

// armStaleContexts copies each armed session's mark onto the parked queries built from it.
func (b *Bridge) armStaleContexts() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armStaleContextsLocked()
}

func (b *Bridge) armStaleContextsLocked() {
	for c := range b.contexts {
		if _, ok := b.historyRewritten[c.piSessionID]; ok && c.piSessionID != "" {
			c.historyStale = true
		}
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// lockSync serialises syncs on one pi session and returns the unlock func: the file
// I/O of two syncs for one session must not interleave.
func (b *Bridge) lockSync(piSessionID string) (unlock func()) {
	b.mu.Lock()
	m, ok := b.syncLocks[piSessionID]
	if !ok {
		m = &sync.Mutex{}
		b.syncLocks[piSessionID] = m
	}
	b.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Children that are stopping or finishing, registered before their state is
// committed: readers of their file wait for the real child exit, never just the
// end of its frames. A healthy parked query is absent: it only appends past every
// committed cut, so a reader never waits on it.

func (b *Bridge) setWriter(ccSessionID string, exited <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writers[ccSessionID] = exited
}

// releaseWriter forgets the writer only while it is still exited's: a later query
// may already have claimed the slot.
func (b *Bridge) releaseWriter(ccSessionID string, exited <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.writers[ccSessionID] == exited {
		delete(b.writers, ccSessionID)
	}
}

// waitForSessionWriter blocks until the child last writing ccSessionID has exited,
// or ctx ends. "" waits for nothing.
func (b *Bridge) waitForSessionWriter(ctx contextDone, ccSessionID string) error {
	if ccSessionID == "" {
		return nil
	}
	b.mu.Lock()
	exited := b.writers[ccSessionID]
	b.mu.Unlock()
	if exited == nil {
		return nil
	}
	select {
	case <-exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// contextDone is the part of context.Context waitForSessionWriter needs.
type contextDone interface {
	Done() <-chan struct{}
	Err() error
}
