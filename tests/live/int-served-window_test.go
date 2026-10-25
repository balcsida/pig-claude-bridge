//go:build live

package live

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Verifies the served context window is logged on a real provider turn (port of tests/int-served-window.mjs):
// the debug log must carry a "result: served contextWindow=…" line, and served must equal registered.
func TestIntServedWindow(t *testing.T) {
	const timeout = 120 * time.Second
	agentDir := agentDirWith(t, map[string]string{"settings.json": `{}`})
	h := New(t, Options{
		Name:           "served-window",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		DefaultTimeout: timeout,
	})
	h.StartAndWait(0)

	t.Log("Single bridge prompt to exercise the provider result path...")
	text, err := h.PromptAndWait(`Reply with just the word "yes".`, timeout)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	if !regexp.MustCompile(`(?i)yes`).MatchString(text) {
		failWithLogs(t, h, "unexpected reply: %s", head(text, 200))
	}

	debug := readDebugLog(t, h)
	served := regexp.MustCompile(`result: served contextWindow=`)
	var line string
	for _, l := range strings.Split(debug, "\n") {
		if served.MatchString(l) {
			line = l
			break
		}
	}
	if line == "" {
		failWithLogs(t, h, "debug log has no \"result: served contextWindow=\" line.\n\nExcerpt:\n%s", tail(debug, 2000))
	}
	t.Logf("  %s", strings.TrimSpace(line))

	// The line must carry both the served value and the registered value so the gap is observable in one place.
	servedRe, registeredRe := regexp.MustCompile(`served contextWindow=(\d+)`), regexp.MustCompile(`registered=(\d+)`)
	if !servedRe.MatchString(line) {
		failWithLogs(t, h, "missing served contextWindow number")
	}
	if !registeredRe.MatchString(line) {
		failWithLogs(t, h, "missing registered number")
	}
	if !regexp.MustCompile(`servedModel=`).MatchString(line) {
		failWithLogs(t, h, "missing servedModel")
	}

	// pi's registered window must equal the window CC actually serves, or pi's status bar and auto-compaction
	// threshold drift from reality (issue #24/#17). Account-agnostic.
	servedN, _ := strconv.Atoi(servedRe.FindStringSubmatch(line)[1])
	registeredN, _ := strconv.Atoi(registeredRe.FindStringSubmatch(line)[1])
	if servedN != registeredN {
		failWithLogs(t, h, "%s", fmt.Sprintf("served contextWindow (%d) != registered (%d); pi's compaction threshold is out of sync with the CC window.\n  %s",
			servedN, registeredN, strings.TrimSpace(line)))
	}
	t.Log("PASS")
}
