//go:build live

package live

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// Baseline: pi-side /compact works end-to-end through the bridge (port of tests/int-compact-baseline.mjs).
func TestIntCompactBaseline(t *testing.T) {
	const timeout = 180 * time.Second
	agentDir := agentDirWith(t, map[string]string{"settings.json": `{"compaction":{"keepRecentTokens":50}}`})
	h := New(t, Options{
		Name:           "compact-baseline",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		DefaultTimeout: timeout,
	})
	h.StartAndWait(0)
	prompt := func(msg string) string {
		t.Helper()
		text, err := h.PromptAndWait(msg, timeout)
		if err != nil {
			failWithLogs(t, h, "%v", err)
		}
		return text
	}

	t.Log("Seed: a few short turns so there is history to compact...")
	prompt("Pick a number between 1 and 100 and remember it. Reply with just the number. Do not use the memory system.")
	prompt("Now pick a color. Reply with just the color. Do not use the memory system.")
	prompt("Now pick a fruit. Reply with just the fruit. Do not use the memory system.")

	t.Log("Triggering /compact...")
	data, err := h.Send(map[string]any{"type": "compact"}, timeout)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	result := asMap(data)
	if result == nil {
		failWithLogs(t, h, "compact returned non-object: %s", jsonString(data))
	}
	summary, _ := result["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		failWithLogs(t, h, "compact returned empty summary: %s", jsonString(result))
	}
	firstKept, _ := result["firstKeptEntryId"].(string)
	if firstKept == "" {
		failWithLogs(t, h, "compact returned no firstKeptEntryId: %s", jsonString(result))
	}
	t.Logf("  summary:        %s...", head(strings.ReplaceAll(summary, "\n", " "), 80))
	t.Logf("  firstKeptEntry: %s", head(firstKept, 8))
	t.Logf("  tokensBefore:   %v", result["tokensBefore"])

	t.Log("Post-compact prompt...")
	after := prompt(`Reply with exactly "after-compact-ok".`)
	if !regexp.MustCompile(`after-compact-ok`).MatchString(after) {
		failWithLogs(t, h, "post-compact prompt did not return the expected marker. Got: %s", head(after, 200))
	}
	t.Log("PASS")
}
