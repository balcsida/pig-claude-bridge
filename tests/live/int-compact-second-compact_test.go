//go:build live

package live

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Regression: extension-provided compactions preserve cumulative file-op carry-forward across a second
// compaction (port of tests/int-compact-second-compact.mjs). The TypeScript test ran in the repo root and
// read tests/fixtures/*; here the temp project carries copies at the same relative paths.
func TestIntCompactSecondCompact(t *testing.T) {
	const timeout = 180 * time.Second
	// Force each /compact to discard older turns instead of summarizing an empty prefix.
	agentDir := agentDirWith(t, map[string]string{"settings.json": `{"compaction":{"keepRecentTokens":50}}`})
	h := New(t, Options{
		Name:           "compact-second-compact",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		Cwd:            projectDir(t),
		DefaultTimeout: timeout,
	})
	h.StartAndWait(0)

	assertMentions := func(summary, file string) {
		t.Helper()
		if !strings.Contains(summary, file) {
			failWithLogs(t, h, "summary missing %s. Summary head: %s", file, head(summary, 500))
		}
	}
	forceDiscardableHistory := func(file, marker string) {
		t.Helper()
		if _, err := h.PromptAndWait(fmt.Sprintf("Use the read tool to read %s. Then reply with exactly '%s'.", file, marker), timeout); err != nil {
			failWithLogs(t, h, "%v", err)
		}
		if _, err := h.PromptAndWait("List 15 European capital cities, one per line, numbered. Nothing else.", timeout); err != nil {
			failWithLogs(t, h, "%v", err)
		}
	}
	compact := func(which string) string {
		t.Helper()
		data, err := h.Send(map[string]any{"type": "compact"}, timeout)
		if err != nil {
			failWithLogs(t, h, "%v", err)
		}
		summary, _ := asMap(data)["summary"].(string)
		if strings.TrimSpace(summary) == "" {
			failWithLogs(t, h, "%s compact returned empty summary: %s", which, jsonString(data))
		}
		return summary
	}

	t.Log("Round 1: force a read of file A into compacted history...")
	forceDiscardableHistory("tests/fixtures/compact-file-a.txt", "read-a-ok")

	t.Log("First /compact...")
	assertMentions(compact("first"), "tests/fixtures/compact-file-a.txt")

	t.Log("Round 2: force a read of file B into compacted history...")
	forceDiscardableHistory("tests/fixtures/compact-file-b.txt", "read-b-ok")

	t.Log("Second /compact...")
	second := compact("second")
	assertMentions(second, "tests/fixtures/compact-file-a.txt")
	assertMentions(second, "tests/fixtures/compact-file-b.txt")
	t.Log("PASS")
}
