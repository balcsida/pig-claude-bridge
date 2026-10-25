package bridge

import "os"

// rememberSessionCwd records the cwd of a pi session. In the pi CLI the process cwd
// is the session's cwd, but a host serving many sessions from one process has a
// single process cwd, so Claude Code would run in the host's directory.
func (b *Bridge) rememberSessionCwd(piSessionID, cwd string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cwds[piSessionID] = cwd
}

func (b *Bridge) forgetSessionCwd(piSessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cwds, piSessionID)
}

// sessionCwdFor is the remembered cwd, else the process cwd.
func (b *Bridge) sessionCwdFor(piSessionID string) string {
	b.mu.Lock()
	cwd, ok := b.cwds[piSessionID]
	b.mu.Unlock()
	if ok && piSessionID != "" {
		return cwd
	}
	wd, _ := os.Getwd()
	return wd
}

// rememberSummarySession marks the pi session whose compaction summary runs next.
// The TypeScript keys this by AbortSignal identity, which Go lacks (divergence D7):
// the most recent remembered summary wins until forgetSummarySession.
func (b *Bridge) rememberSummarySession(piSessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pendingSummary = piSessionID
}

// summarySessionFor returns the pending summary session without clearing it: pi
// makes several summary calls per operation (compaction makes two).
func (b *Bridge) summarySessionFor() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pendingSummary, b.pendingSummary != ""
}

// forgetSummarySession ends piSessionID's summary operation. It does nothing when
// another session's summary has been remembered since.
func (b *Bridge) forgetSummarySession(piSessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pendingSummary == piSessionID {
		b.pendingSummary = ""
	}
}
