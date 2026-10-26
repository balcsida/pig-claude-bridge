//go:build live

package live

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression: a compaction that lands inside a turn must reach Claude Code, issue #101 (port of
// tests/int-compact-midturn-rebuild.mjs). Auto-compaction starts off and is switched on at the first tool
// execution of the measured turn; the threshold is calibrated by a probe run of one minimal turn.
// The TypeScript test ran in the repo root; the measured turn reads package.json, tsconfig.json, LICENSE
// and tests/fixtures/compact-file-*.txt from the temp project, which carries copies.
func TestIntCompactMidturnRebuild(t *testing.T) {
	const (
		contextWindow   = 200_000 // what the bridge registers for the model above
		testTimeout     = 300 * time.Second
		probeTimeout    = 120 * time.Second
		thresholdMargin = 10_000
		bigFileBytes    = 48 * 1024 // pi's read tool truncates at 50KB
	)
	const bridgeModel = "claude-bridge/claude-haiku-4-5"

	agentDir := t.TempDir()
	cwd := projectDir(t)
	fixtureDir := filepath.Join(cwd, ".test-output", "compact-midturn-fixtures")
	writeSettings := func(compaction string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"compaction":`+compaction+`}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A file of ~bigFileBytes that no model can shorten by guessing: the words are random.
	writeBigFixture := func(name string, seed int64) string {
		t.Helper()
		if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fixtureDir, name)
		var lines []string
		length := 0
		rnd := seed
		for length < bigFileBytes {
			words := make([]string, 12)
			for i := range words {
				// Exact in int64, where the TypeScript's float64 product drops low bits, so the words differ from
				// its fixture's; only their incompressibility matters.
				rnd = (rnd*1103515245 + 12345) & 0x7fffffff
				words[i] = strconv.FormatInt(rnd, 36)
			}
			line := fmt.Sprintf("%d: %s", len(lines)+1, strings.Join(words, " "))
			lines = append(lines, line)
			length = len(strings.Join(lines, "\n"))
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// in + cacheRead + cacheWrite off one of the bridge's `usage:` debug lines.
	usageRe := regexp.MustCompile(`usage: in=(\d+) out=\d+ cacheRead=(\d+) cacheWrite=(\d+)`)
	promptTokens := func(line string) (int, bool) {
		m := usageRe.FindStringSubmatch(line)
		if m == nil {
			return 0, false
		}
		sum := 0
		for _, s := range m[1:] {
			n, _ := strconv.Atoi(s)
			sum += n
		}
		return sum, true
	}
	usageTokens := func(debugLog string) []int {
		var out []int
		for _, line := range strings.Split(debugLog, "\n") {
			if n, ok := promptTokens(line); ok {
				out = append(out, n)
			}
		}
		return out
	}
	maxOf := func(ns []int) int {
		m := 0
		for _, n := range ns {
			m = max(m, n)
		}
		return m
	}

	// --- Probe: what does one minimal turn through this bridge cost? ---
	writeSettings(`{"enabled":false,"reserveTokens":1000,"keepRecentTokens":50}`)
	probe := New(t, Options{
		Name:           "compact-midturn-probe",
		Args:           []string{"--model", bridgeModel},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		Cwd:            cwd,
		DefaultTimeout: probeTimeout,
	})
	probe.StartAndWait(0)
	if _, err := probe.PromptAndWait(`Reply with exactly "probe-ok". Do not use tools.`, probeTimeout); err != nil {
		failWithLogs(t, probe, "%v", err)
	}
	measured := usageTokens(readDebugLog(t, probe))
	if len(measured) == 0 {
		failWithLogs(t, probe, "probe produced no usage lines — see %s", probe.DebugLog)
	}
	baseTokens := maxOf(measured)
	t.Logf("Probe: one minimal turn costs %d prompt tokens", baseTokens)
	probe.Stop()

	threshold := baseTokens + thresholdMargin
	writeSettings(fmt.Sprintf(`{"enabled":false,"reserveTokens":%d,"keepRecentTokens":50}`, contextWindow-threshold))
	t.Logf("Threshold for the run: %d tokens (reserveTokens=%d)", threshold, contextWindow-threshold)

	// --- The run ---
	bigA := writeBigFixture("big-a.txt", 11)
	bigB := writeBigFixture("big-b.txt", 977)

	h := New(t, Options{
		Name:           "compact-midturn",
		Args:           []string{"--model", bridgeModel},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		Cwd:            cwd,
		DefaultTimeout: testTimeout,
	})
	h.StartAndWait(0)
	check := checker(t, h)

	t.Log("Seed: read two large files with auto-compaction off...")
	seed := prompter(t, h, testTimeout)(fmt.Sprintf("Use the read tool to read %s and then %s, one at a time. Then reply with exactly \"seed-ok\" and nothing else.", bigA, bigB))
	check(regexp.MustCompile(`(?i)seed-ok`).MatchString(seed), "seed turn did not confirm the reads. Got: %s", head(seed, 300))
	seededTokens := maxOf(usageTokens(readDebugLog(t, h)))
	t.Logf("  seeded context: %d prompt tokens (threshold %d)", seededTokens, threshold)
	check(seededTokens > threshold, "seed did not cross the threshold (%d <= %d), so nothing would compact mid-turn", seededTokens, threshold)

	// Auto-compaction goes on once the measured turn is already running, so the compaction lands at a turn
	// boundary inside it rather than before it.
	var mu sync.Mutex
	turnRunning := false
	var enabling chan error
	compactionStarts, compactionsInTurn := 0, 0
	h.AddListener(func(msg Event) {
		mu.Lock()
		defer mu.Unlock()
		if msg["type"] == "tool_execution_start" && turnRunning && enabling == nil {
			// Send waits for a response the reader goroutine delivers, so it cannot run on that goroutine.
			enabling = make(chan error, 1)
			go func() {
				_, err := h.Send(map[string]any{"type": "set_auto_compaction", "enabled": true}, 30*time.Second)
				enabling <- err
			}()
		}
		if msg["type"] == "compaction_start" {
			compactionStarts++
			if turnRunning {
				compactionsInTurn++
			}
		}
	})

	t.Log("Measured turn: several small tool calls, auto-compaction enabled mid-turn...")
	mu.Lock()
	turnRunning = true
	mu.Unlock()
	answer, err := h.PromptAndWait("Use the read tool to read these files, one at a time, waiting for each result before "+
		"starting the next: package.json, tsconfig.json, tests/fixtures/compact-file-a.txt, "+
		"tests/fixtures/compact-file-b.txt, LICENSE. Do not read anything else. "+
		`Then reply with exactly "midturn-ok" and nothing else.`, testTimeout)
	mu.Lock()
	turnRunning = false
	enablingCh, inTurn, starts := enabling, compactionsInTurn, compactionStarts
	mu.Unlock()
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	if enablingCh != nil {
		if err := <-enablingCh; err != nil {
			failWithLogs(t, h, "%v", err)
		}
	}

	check(inTurn > 0, "no compaction fired while the turn was running (%d outside it) — the boundary this test is about was never reached", starts)
	t.Logf("  compactions during the turn: %d", inTurn)

	// The turn has to survive the compaction: discarding the query must continue the turn from the rebuilt
	// session, not end it on an orphaned tool result.
	check(regexp.MustCompile(`(?i)midturn-ok`).MatchString(answer), "the turn did not finish after the mid-turn compaction. Got: %s", head(answer, 300))

	debugLog := readDebugLog(t, h)
	lines := strings.Split(debugLog, "\n")
	compactedAt := -1
	for i, line := range lines {
		if regexp.MustCompile(`session_compact:threshold`).MatchString(line) {
			compactedAt = i
			break
		}
	}
	check(compactedAt != -1, "debug log has no session_compact:threshold line — see %s", h.DebugLog)

	beforeTokens, afterTokens := -1, -1
	if ns := usageTokens(strings.Join(lines[:compactedAt], "\n")); len(ns) > 0 {
		beforeTokens = ns[len(ns)-1]
	}
	if ns := usageTokens(strings.Join(lines[compactedAt:], "\n")); len(ns) > 0 {
		afterTokens = ns[0]
	}
	check(beforeTokens != -1, "no usage line before the compaction")
	check(afterTokens != -1, "no usage line after the compaction — the turn reported nothing further")
	t.Logf("  prompt tokens before the compaction: %d", beforeTokens)
	t.Logf("  prompt tokens after the compaction:  %d", afterTokens)

	// Half is generous either way: the compacted history is a summary plus a couple of small reads, while a
	// resumed pre-compaction query reports what it always did.
	check(float64(afterTokens) < float64(beforeTokens)*0.5,
		"Claude Code kept the pre-compaction context: %d prompt tokens after the compaction against %d before it (want under %d). "+
			"The compaction never reached the running query — each further tool result re-crosses the threshold (issue #101).",
		afterTokens, beforeTokens, int(float64(beforeTokens)*0.5+0.5))

	check(!regexp.MustCompile(`MCP handlers still waiting`).MatchString(debugLog), "a tool handler was left waiting — the discarded query stranded one")
	t.Log("PASS")
}
