package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

// The Go equivalent of index.ts's __test seam: helpers tests use to plant and read
// state. Tests share the package, so these touch unexported fields directly.

// recordingHost is a Host that records Notify calls.
type recordingHost struct {
	notices []string
	cwd     string
}

func (h *recordingHost) Notify(message, level string) {
	h.notices = append(h.notices, level+": "+message)
}
func (h *recordingHost) SessionID() (string, error)    { return "", nil }
func (h *recordingHost) Cwd() string                   { return h.cwd }
func (h *recordingHost) AgentDir() string              { return "" }
func (h *recordingHost) ConfigDirName() string         { return ".pig" }
func (h *recordingHost) SystemPrompt() (string, error) { return "", nil }
func (h *recordingHost) Mode() string                  { return "print" }

// newTestBridge returns a Bridge whose Claude dir and agent dir are throwaway.
func newTestBridge(t *testing.T, debug bool) (b *Bridge, claudeDir, agentDir string) {
	t.Helper()
	claudeDir, agentDir = t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", "")
	return New(Deps{AgentDir: agentDir, ConfigDirName: ".pig", Debug: debug}), claudeDir, agentDir
}

func (b *Bridge) resetSharedSession() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions = map[string]SessionState{}
	b.historyRewritten = map[string]struct{}{}
}

func (b *Bridge) historyRewrittenFor(piSessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.historyRewritten[piSessionID]
	return ok
}

func (b *Bridge) addContext(piSessionID string) *QueryContext {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := &QueryContext{piSessionID: piSessionID}
	b.contexts[c] = struct{}{}
	return c
}

// isAbandoned reports whether discardRewrittenQuery killed c's query.
func (c *QueryContext) isAbandoned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.abandoned
}

func (b *Bridge) isStale(c *QueryContext) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return c.historyStale
}

func readDebugLog(t *testing.T, agentDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentDir, "claude-bridge.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
