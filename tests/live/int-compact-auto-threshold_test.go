//go:build live

package live

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression: real auto-threshold compaction exercises native pi summarization through the bridge (port of
// tests/int-compact-auto-threshold.mjs). It goes through the pre-prompt threshold check, not RPC compact;
// keepRecentTokens=50 forces separate history and turn-prefix summary calls.
func TestIntCompactAutoThreshold(t *testing.T) {
	const (
		compactTimeout = 120 * time.Second
		testTimeout    = 240 * time.Second
	)
	agentDir := agentDirWith(t, map[string]string{
		"settings.json": `{"compaction":{"enabled":false,"reserveTokens":198000,"keepRecentTokens":50}}`,
	})
	h := New(t, Options{
		Name:           "compact-auto-threshold",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		Cwd:            projectDir(t),
		DefaultTimeout: testTimeout,
	})
	h.StartAndWait(0)

	check := func(condition bool, format string, args ...any) {
		t.Helper()
		if !condition {
			failWithLogs(t, h, format, args...)
		}
	}
	assertReadFile := func(details map[string]any, suffix string) {
		t.Helper()
		readFiles, ok := details["readFiles"].([]any)
		check(ok, "readFiles missing/invalid in details: %s", jsonString(details))
		for _, f := range readFiles {
			if strings.HasSuffix(fmt.Sprint(f), suffix) {
				return
			}
		}
		check(false, "readFiles missing %s. Got: %s", suffix, jsonString(readFiles))
	}

	t.Log("Seed turn A: force file A read while auto-compaction is disabled...")
	seedA, err := h.PromptAndWait(`Use the read tool to read tests/fixtures/compact-file-a.txt. Then reply with exactly "read-a-ok".`, testTimeout)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	check(regexp.MustCompile(`(?i)read-a-ok`).MatchString(seedA), "seed A did not confirm read. Got: %s", head(seedA, 200))

	t.Log("Seed turn B: force file B read and a larger assistant response...")
	seedB, err := h.PromptAndWait(`Use the read tool to read tests/fixtures/compact-file-b.txt. Do not write or edit files. Then reply in chat with 120 numbered lines about European capital cities. Include the exact phrase "AUTO-COMPACT-SPLIT-SEED" on line 60.`, testTimeout)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	check(strings.Contains(seedB, "AUTO-COMPACT-SPLIT-SEED"), "seed B did not include split seed marker. Got tail: %s", tail(seedB, 500))

	var mu sync.Mutex
	thresholdStarts, thresholdEnds := 0, 0
	var disableDone chan error
	h.AddListener(func(msg Event) {
		mu.Lock()
		defer mu.Unlock()
		if msg["reason"] != "threshold" {
			return
		}
		switch msg["type"] {
		case "compaction_start":
			thresholdStarts++
			if disableDone == nil {
				// Send waits for a response the reader goroutine delivers, so it cannot run on that goroutine.
				disableDone = make(chan error, 1)
				go func() {
					_, err := h.Send(map[string]any{"type": "set_auto_compaction", "enabled": false}, 30*time.Second)
					disableDone <- err
				}()
			}
		case "compaction_end":
			thresholdEnds++
		}
	})
	isThreshold := func(typ string) func(Event) bool {
		return func(m Event) bool { return m["type"] == typ && m["reason"] == "threshold" }
	}
	// Listeners first: the events can arrive before the prompt's agent_end.
	startCh, removeStart := h.listenFor(isThreshold("compaction_start"))
	defer removeStart()
	endCh, removeEnd := h.listenFor(isThreshold("compaction_end"))
	defer removeEnd()

	t.Log("Enable auto-compaction and submit a normal prompt (not RPC compact)...")
	if _, err := h.Send(map[string]any{"type": "set_auto_compaction", "enabled": true}, 30*time.Second); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	type promptResult struct {
		text string
		err  error
	}
	answerCh := make(chan promptResult, 1)
	go func() {
		text, err := h.PromptAndWait(`Do not use tools. Reply on the first line with exactly "auto-compact-continued". If you know the earlier file A path, put it on a second line; otherwise leave the second line blank.`, testTimeout)
		answerCh <- promptResult{text, err}
	}()

	if _, err := h.await(startCh, compactTimeout, "Timeout waiting for threshold compaction_start"); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	mu.Lock()
	disable := disableDone
	mu.Unlock()
	if disable != nil {
		if err := <-disable; err != nil {
			failWithLogs(t, h, "failed to disable auto-compaction after threshold start: %v", err)
		}
	}
	endEvent, err := h.await(endCh, compactTimeout, "Timeout waiting for threshold compaction_end")
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	var answer string
	select {
	case r := <-answerCh:
		if r.err != nil {
			failWithLogs(t, h, "%v", r.err)
		}
		answer = r.text
	case <-time.After(testTimeout):
		failWithLogs(t, h, "Timeout waiting for the post-compact prompt")
	}

	counts := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return thresholdStarts, thresholdEnds
	}
	starts, ends := counts()
	check(starts == 1, "expected exactly one threshold compaction_start, got %d", starts)
	check(ends == 1, "expected exactly one threshold compaction_end, got %d", ends)
	check(endEvent["aborted"] == false, "threshold compaction aborted: %s", jsonString(endEvent))
	result := asMap(endEvent["result"])
	summary, _ := result["summary"].(string)
	check(strings.TrimSpace(summary) != "", "threshold compaction returned empty summary: %s", jsonString(endEvent))
	firstKept, _ := result["firstKeptEntryId"].(string)
	check(firstKept != "", "threshold compaction returned no firstKeptEntryId: %s", jsonString(endEvent))
	tokensBefore, _ := result["tokensBefore"].(float64)
	check(tokensBefore > 0, "threshold compaction returned invalid tokensBefore: %s", jsonString(endEvent))
	check(regexp.MustCompile(`Turn Context \(split turn\)`).MatchString(summary),
		"threshold compaction summary lacks split-turn marker. Summary head: %s", head(summary, 500))
	details := asMap(result["details"])
	assertReadFile(details, "compact-file-a.txt")
	assertReadFile(details, "compact-file-b.txt")

	check(strings.TrimSpace(answer) != "", "post-compact prompt reached agent_end but returned empty text")
	check(regexp.MustCompile(`(?i)auto-compact-continued`).MatchString(answer),
		"post-compact prompt did not return continuation marker. Got: %s", head(answer, 300))
	if strings.Contains(answer, "tests/fixtures/compact-file-a.txt") {
		t.Log("  post-compact answer echoed file A path")
	}

	// Give any accidental second threshold compaction a chance to surface. The disable-on-start RPC is
	// load-bearing because the huge reserve would otherwise keep triggering threshold compaction.
	time.Sleep(1500 * time.Millisecond)
	starts, ends = counts()
	check(starts == 1, "second threshold compaction_start fired (%d total)", starts)
	check(ends == 1, "second threshold compaction_end fired (%d total)", ends)

	debugLog := readDebugLog(t, h)
	check(!regexp.MustCompile(`session_before_compact: takeover`).MatchString(debugLog), "bridge must not take over compaction")
	check(regexp.MustCompile(`provider: one-off summarizer call \(cacheRetention none\) routed to isolated summary`).MatchString(debugLog),
		"native compaction must route via the provider marker")
	compactSpawns := len(regexp.MustCompile(`compact summary: spawn`).FindAllString(debugLog, -1))
	check(compactSpawns >= 2, "expected at least 2 isolated compact summary spawns, got %d", compactSpawns)
	check(!regexp.MustCompile(`currentPiStream overwritten`).MatchString(debugLog), "debug log reported currentPiStream overwrite")

	t.Logf("  summary:      %s...", head(strings.ReplaceAll(summary, "\n", " "), 80))
	t.Logf("  tokensBefore: %v", tokensBefore)
	t.Logf("  readFiles:    %s", jsonString(details["readFiles"]))
	t.Log("PASS")
}
