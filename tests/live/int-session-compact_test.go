//go:build live

package live

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// Verifies the bridge rebuilds Claude Code's session JSONL after a pi-side /compact (port of
// tests/int-session-compact.mjs): the first syncResult after the session_compact event must not reuse.
func TestIntSessionCompact(t *testing.T) {
	const timeout = 180 * time.Second
	agentDir := agentDirWith(t, map[string]string{"settings.json": `{"compaction":{"keepRecentTokens":50}}`})
	h := New(t, Options{
		Name:           "session-compact",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		DefaultTimeout: timeout,
	})
	h.StartAndWait(0)
	prompt := func(msg string) {
		t.Helper()
		if _, err := h.PromptAndWait(msg, timeout); err != nil {
			failWithLogs(t, h, "%v", err)
		}
	}

	// A few substantive turns so pi has something to compact.
	t.Log("Turn 1: seed history...")
	prompt("Pick a number between 1 and 100 and remember it. Reply with just the number. Do not use the memory system.")
	t.Log("Turn 2: more history...")
	prompt("Now pick a color. Reply with just the color. Do not use the memory system.")
	t.Log("Turn 3: more history...")
	prompt("Now pick a fruit. Reply with just the fruit. Do not use the memory system.")

	t.Log("Triggering /compact...")
	if _, err := h.Send(map[string]any{"type": "compact"}, timeout); err != nil {
		failWithLogs(t, h, "%v", err)
	}

	t.Log("Turn 4: prompt after compact (should force REBUILD)...")
	prompt("Are you still there? Reply with just 'yes'.")

	// Split the log at the `session_compact:` marker. Reads before the marker (including the summarization
	// call pi made via our provider) don't matter: the first syncResult after the event is Turn 4's prompt.
	fullLog := readDebugLog(t, h)
	compactIdx := strings.Index(fullLog, "session_compact:")
	if compactIdx == -1 {
		failWithLogs(t, h, "no `session_compact:` debug marker — handler not subscribed?")
	}
	preEventLog, postEventLog := fullLog[:compactIdx], fullLog[compactIdx:]

	preIDs := regexp.MustCompile(`syncResult: path=(?:reuse|rebuild) sessionId=([a-f0-9-]+)`).FindAllStringSubmatch(preEventLog, -1)
	if len(preIDs) == 0 {
		failWithLogs(t, h, "no pre-compact shared sessionId found in debug log")
	}
	preCompactSessionID := preIDs[len(preIDs)-1][1]
	t.Logf("  Pre-compact sessionId: %s", preCompactSessionID)

	// Capture both the path and the rebuild flavor (preserved | rotated-post-abort | first).
	type syncResult struct{ path, sessionID, flavor string }
	var syncResults []syncResult
	for _, m := range regexp.MustCompile(`syncResult: path=(reuse|rebuild|clean-start)(?: sessionId=([a-f0-9-]+) priors=\d+ (\S+))?`).FindAllStringSubmatch(postEventLog, -1) {
		syncResults = append(syncResults, syncResult{m[1], m[2], m[3]})
	}
	t.Logf("  Post-event syncResults: %+v", syncResults)
	if len(syncResults) == 0 {
		failWithLogs(t, h, "no syncResult markers after session_compact event (Turn 4 didn't reach the provider?)")
	}
	first := syncResults[0]

	// First syncResult after the event must NOT reuse: pi's history has shrunk and CC's session JSONL is stale.
	if first.path == "clean-start" {
		failWithLogs(t, h, "first syncResult after session_compact was clean-start. The compact summarization "+
			"call landed after the event marker, so this test cannot verify Turn 4's rebuild.")
	}
	if first.path == "reuse" {
		failWithLogs(t, h, "bridge took REUSE path after session_compact — CC will resume the pre-compact session. "+
			"Expected REBUILD (or clean-start) so CC sees the post-compact history. "+
			"Symptom: triggers Claude Code's autocompact-thrashing (issue #8) on long sessions.")
	}
	// Compact has no concurrent CC writer, so the rebuild should preserve the sessionId and wipe the JSONL in place.
	if first.path == "rebuild" && first.flavor != "preserved" {
		failWithLogs(t, h, "post-compact rebuild used flavor=%s, expected \"preserved\". Compact has no concurrent CC writer — "+
			"it should rebuild in place (delete + create with the same UUID), not rotate. Rotating leaks orphan JSONL files.", first.flavor)
	}
	if first.path == "rebuild" && first.sessionID != preCompactSessionID {
		failWithLogs(t, h, "post-compact rebuild preserved sessionId=%s, but expected the original pre-compact sessionId=%s. "+
			"This means the compact summarization session replaced the shared session before the session_compact handler ran, "+
			"orphaning the main pre-compact JSONL.", first.sessionID, preCompactSessionID)
	}
	t.Log("PASS")
}
