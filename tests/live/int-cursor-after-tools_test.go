//go:build live

package live

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// Regression: cursor after a tool-using first turn (port of tests/int-cursor-after-tools.mjs, issue #4).
// With a tool-using Case 1 clean start, turn 2 must reuse the session, not rebuild it.
func TestIntCursorAfterTools(t *testing.T) {
	const timeout = 60 * time.Second
	h := New(t, Options{
		Name:           "cursor-after-tools",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Cwd:            projectDir(t),
		DefaultTimeout: timeout,
	})
	h.StartAndWait(0)

	// Turn 1: force a tool call
	text1, err := h.PromptAndWait("Use the Read tool to read package.json and tell me the top-level name field. Just the name, nothing else.", timeout)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	if text1 == "" {
		failWithLogs(t, h, "Turn 1 should produce text")
	}
	// Turn 2: text-only, should reuse session
	text2, err := h.PromptAndWait("What was the name you just read? Repeat it.", timeout)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	if text2 == "" {
		failWithLogs(t, h, "Turn 2 should produce text")
	}

	var paths []string
	for _, m := range regexp.MustCompile(`syncResult: path=([a-z-]+)`).FindAllStringSubmatch(readDebugLog(t, h), -1) {
		paths = append(paths, m[1])
	}
	rebuilds, reuses := 0, 0
	for _, p := range paths {
		switch p {
		case "rebuild":
			rebuilds++
		case "reuse":
			reuses++
		}
	}
	if rebuilds != 0 {
		failWithLogs(t, h, "spurious rebuild(s) — cursor likely stuck at 1 after tool-using first turn (issue #4). sync paths: %s", strings.Join(paths, ", "))
	}
	if reuses < 1 {
		failWithLogs(t, h, "expected at least 1 reuse for turn 2, got %d. sync paths: %s", reuses, strings.Join(paths, ", "))
	}
	t.Log("PASS")
}
