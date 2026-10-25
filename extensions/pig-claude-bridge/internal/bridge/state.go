package bridge

import "sync"

// SessionState mirrors the Claude Code conversation one pi session's turns run on.
// It is a value: the Bridge stores and returns copies, so a caller can never race
// the map by holding one.
type SessionState struct {
	SessionID string
	Cursor    int
	Cwd       string
	// PiSessionID is the pi session this CC conversation serves. Attribution for
	// history rewrites: a subagent's compaction must not rebuild another session.
	PiSessionID string
	// NeedsRebuild forces the next sync down REBUILD: pi mutated its history
	// (compact, tree navigation) or an abort left the JSONL indeterminate.
	NeedsRebuild bool
	// ForceRotate is set only where a CC subprocess was just killed. The dying
	// process may still flush a late record to the old file, so REBUILD takes a
	// fresh id and leaves the old file alone instead of racing it.
	ForceRotate bool

	// markSeq counts markRebuildForSession calls, so a sync can tell a mark that
	// landed during its file I/O from the one it is consuming.
	markSeq uint64
}

// noSession is the key for calls with no pi session id; they share one mirror.
const noSession = "(none)"

func sessionKey(piSessionID string) string {
	if piSessionID == "" {
		return noSession
	}
	return piSessionID
}

// sessionStateFor returns a copy of the mirror for piSessionID, or nil when it has none yet.
func (b *Bridge) sessionStateFor(piSessionID string) *SessionState {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sessionKey(piSessionID)]
	if !ok {
		return nil
	}
	return &s
}

// setSessionStateFor stores a copy of state, or deletes the mirror when state is nil.
func (b *Bridge) setSessionStateFor(piSessionID string, state *SessionState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if state == nil {
		delete(b.sessions, sessionKey(piSessionID))
		return
	}
	b.sessions[sessionKey(piSessionID)] = *state
}

// markRebuildForSession records that pi rewrote piSessionID's history: its mirror
// must REBUILD, and parked queries built from it go stale. Other sessions' mirrors
// are untouched.
func (b *Bridge) markRebuildForSession(piSessionID, event string) {
	b.mu.Lock()
	key := sessionKey(piSessionID)
	state, ok := b.sessions[key]
	if ok {
		state.NeedsRebuild = true
		state.markSeq++
		b.sessions[key] = state
	}
	if piSessionID != "" {
		b.historyRewritten[piSessionID] = struct{}{}
	}
	b.armStaleContextsLocked()
	b.mu.Unlock()
	if !ok {
		b.debugf("%s: history rewritten, no session to mark yet", event)
		return
	}
	b.debugf("%s: marking needsRebuild on session %s", event, short(state.SessionID))
}

// markSessionNeedsRebuild flags piSessionID's mirror for REBUILD, and with rotate
// for a fresh session id too (a CC subprocess was just killed and may still flush
// to the old file). Unlike markRebuildForSession it arms no parked query. The mark
// bumps markSeq so a sync in flight keeps it. A session with no mirror has nothing
// to mark; false says so.
func (b *Bridge) markSessionNeedsRebuild(piSessionID string, rotate bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := sessionKey(piSessionID)
	state, ok := b.sessions[key]
	if !ok {
		return false
	}
	state.NeedsRebuild = true
	state.ForceRotate = state.ForceRotate || rotate
	state.markSeq++
	b.sessions[key] = state
	return true
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

// lockSync serialises syncs on one session key and returns the unlock func.
func (b *Bridge) lockSync(piSessionID string) (unlock func()) {
	key := sessionKey(piSessionID)
	b.mu.Lock()
	m, ok := b.syncLocks[key]
	if !ok {
		m = &sync.Mutex{}
		b.syncLocks[key] = m
	}
	b.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// advanceCursor updates the live mirror, so a mark set meanwhile survives.
func (b *Bridge) advanceCursor(piSessionID string, cursor int, cwd string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := sessionKey(piSessionID)
	if cur, ok := b.sessions[key]; ok {
		cur.Cursor, cur.Cwd = cursor, cwd
		b.sessions[key] = cur
	}
}

// storeRebuilt stores the rebuilt mirror, keeping any rewrite mark or forced
// rotation that landed after snapshot was read: the new file predates that rewrite.
func (b *Bridge) storeRebuilt(piSessionID string, snapshot *SessionState, rebuilt SessionState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := sessionKey(piSessionID)
	if cur, ok := b.sessions[key]; ok {
		var was SessionState
		if snapshot != nil {
			was = *snapshot
		}
		if cur.markSeq != was.markSeq {
			rebuilt.NeedsRebuild = true
		}
		if cur.ForceRotate && !was.ForceRotate {
			rebuilt.NeedsRebuild, rebuilt.ForceRotate = true, true
		}
		rebuilt.markSeq = cur.markSeq
	}
	b.sessions[key] = rebuilt
}
