//go:build live

package live

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// Regression: split-turn /compact completes with isolated summaries, issue #18 (port of
// tests/int-compact-splitturn.mjs). keepRecentTokens=50 in a temp agent dir forces isSplitTurn.
func TestIntCompactSplitturn(t *testing.T) {
	const (
		compactTimeout = 90 * time.Second // compact should finish in ~10s; hang = timeout
		testTimeout    = 180 * time.Second
	)
	agentDir := agentDirWith(t, map[string]string{"settings.json": `{"compaction":{"keepRecentTokens":50}}`})
	h := New(t, Options{
		Name:           "compact-splitturn",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		DefaultTimeout: testTimeout,
	})
	h.StartAndWait(0)
	prompt := func(msg string) string {
		t.Helper()
		text, err := h.PromptAndWait(msg, testTimeout)
		if err != nil {
			failWithLogs(t, h, "%v", err)
		}
		return text
	}

	// Two substantive turns so messagesToSummarize and turnPrefixMessages are both non-empty.
	t.Log("Seed: two substantive turns...")
	prompt("List 15 European capital cities, one per line, numbered. Nothing else.")
	prompt("List 15 Asian capital cities, one per line, numbered. Nothing else.")

	t.Log("Triggering /compact (forces split-turn dual-summary)...")
	compactStarted := time.Now()
	data, err := h.Send(map[string]any{"type": "compact"}, compactTimeout)
	if err != nil {
		failWithLogs(t, h, "compact did not complete within %d s — split-turn summary failed (issue #18). Underlying: %v", int(compactTimeout.Seconds()), err)
	}
	t.Logf("  compact returned in %.1fs", time.Since(compactStarted).Seconds())

	summary, _ := asMap(data)["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		failWithLogs(t, h, "compact returned empty summary: %s", jsonString(data))
	}
	// The split-turn marker proves compact exercised the intended summary path.
	if !regexp.MustCompile(`Turn Context \(split turn\)`).MatchString(summary) {
		failWithLogs(t, h, "compact summary lacks the \"Turn Context (split turn)\" marker — isSplitTurn did not fire, "+
			"so this run did not exercise split-turn summaries. Adjust keepRecentTokens or seed content. Summary head: %s", head(summary, 200))
	}
	t.Log("  split-turn marker present")

	t.Log("Post-compact prompt...")
	after := prompt(`Reply with exactly "after-compact-ok".`)
	if !regexp.MustCompile(`after-compact-ok`).MatchString(after) {
		failWithLogs(t, h, "post-compact prompt did not return expected marker. Got: %s", head(after, 200))
	}
	t.Log("PASS")
}
