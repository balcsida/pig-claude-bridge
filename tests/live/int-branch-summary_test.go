//go:build live

package live

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// Native pi branch summaries route through cacheRetention:"none" to an isolated Claude Code subprocess,
// without touching the live provider session (port of tests/int-branch-summary.mjs). The fixture
// extension is loaded with -e; PiG loads the .ts file as a Node factory.
func TestIntBranchSummary(t *testing.T) {
	const timeout = 120 * time.Second
	root, _, _, err := liveEnv()
	if err != nil {
		t.Fatal(err)
	}
	h := New(t, Options{
		Name:           "branch-summary",
		Args:           []string{"-e", filepath.Join(root, "tests", "fixtures", "tree-nav-extension.ts"), "--model", "claude-bridge/claude-haiku-4-5"},
		DefaultTimeout: timeout,
	})
	h.StartAndWait(0)

	// The command returns before the summary finishes, and a slash command emits no agent_end, so wait
	// on the isolated summary's completion log.
	waitForLog := func(mark int, pattern *regexp.Regexp) string {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if slice := readDebugLog(t, h)[mark:]; pattern.MatchString(slice) {
				return slice
			}
			time.Sleep(500 * time.Millisecond)
		}
		return readDebugLog(t, h)[mark:]
	}

	// Two turns so rewinding to the first leaves something worth summarizing.
	for _, msg := range []string{"Reply with exactly the word ALPHA and nothing else.", "Reply with exactly the word BETA and nothing else."} {
		if _, err := h.PromptAndWait(msg, timeout); err != nil {
			failWithLogs(t, h, "%v", err)
		}
	}

	info, err := os.Stat(h.DebugLog)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	mark := int(info.Size())
	if _, err := h.Send(map[string]any{"type": "prompt", "message": "/rewind-summarize"}, timeout); err != nil {
		failWithLogs(t, h, "%v", err)
	}

	done := regexp.MustCompile(`compact summary: done textLen=[1-9]\d*`)
	log := waitForLog(mark, done)

	if regexp.MustCompile(`session_before_tree: takeover`).MatchString(log) {
		failWithLogs(t, h, "bridge must not take over branch summaries")
	}
	if !regexp.MustCompile(`provider: one-off summarizer call \(cacheRetention none\) routed to isolated summary`).MatchString(log) {
		failWithLogs(t, h, "native summary must route via the provider marker")
	}
	if !regexp.MustCompile(`compact summary: spawn`).MatchString(log) {
		failWithLogs(t, h, "isolated summary never started")
	}
	if !done.MatchString(log) {
		failWithLogs(t, h, "isolated summary produced no text")
	}
	if regexp.MustCompile(`no capture for this \d+-char system prompt`).MatchString(log) {
		failWithLogs(t, h, "summary consulted the live prompt resolver")
	}
	t.Log("PASS")
}
