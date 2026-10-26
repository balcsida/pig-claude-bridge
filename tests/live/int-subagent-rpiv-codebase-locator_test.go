//go:build live

package live

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Reproduction/regression for rpiv-pi codebase-locator subagents hanging while claude-bridge has a parent
// Claude Code query active (port of tests/int-subagent-rpiv-codebase-locator.mjs). It bypasses
// /skill:discover and invokes the same underlying mechanism directly: the @tintinweb/pi-subagents Agent tool
// plus rpiv-pi's pinned codebase-locator agent definition from @juicesharp/rpiv-pi v0.6.0.
func TestIntSubagentRpivCodebaseLocator(t *testing.T) {
	const (
		bridgeModel = "claude-bridge/claude-haiku-4-5"
		testTimeout = 240 * time.Second
		// Loaded straight from npm through PiG's `-e npm:` source, pinned to the version the TypeScript test uses.
		subagentsSource = "npm:@tintinweb/pi-subagents@0.14.3"
	)
	reentrantMarker := regexp.MustCompile(`provider: active query user-only call treated as reentrant fresh query`)
	stuckMarker := regexp.MustCompile(`MCP handlers still waiting after delivering 0 results|tool handler\(s\) still waiting|currentPiStream overwritten`)

	root, _, _, err := liveEnv()
	if err != nil {
		t.Fatal(err)
	}
	locator, err := os.ReadFile(filepath.Join(root, "tests", "fixtures", "agents", "codebase-locator.md"))
	if err != nil {
		t.Fatalf("missing rpiv codebase-locator fixture: %v", err)
	}
	agentDir := t.TempDir()
	project := t.TempDir()
	for path, content := range map[string]string{
		filepath.Join(".pi", "agents", "codebase-locator.md"): string(locator),
		"package.json":                          `{"name": "subagent-rpiv-locator-fixture", "private": true}` + "\n",
		filepath.Join("src", "rpiv_locator.ts"): "export const RPIV_LOCATOR_SENTINEL = 'rpiv-codebase-locator';\n",
	} {
		full := filepath.Join(project, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	h := New(t, Options{
		Name:           "subagent-rpiv-codebase-locator",
		Args:           []string{"-e", subagentsSource, "--model", bridgeModel},
		Cwd:            project,
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		DefaultTimeout: testTimeout,
	})
	logDebugTailOnFailure(t, h, 6000)
	reentrantCount := func() int { return len(reentrantMarker.FindAllString(readDebugLog(t, h), -1)) }

	runAgentPrompt := func(background bool, expectedMarker string) {
		label, runInBackground := "foreground", "false"
		lead := "Then"
		if background {
			label, runInBackground = "background", "true"
			lead = "Write 80 short numbered lines about keeping background work separate, then"
		}
		beforeReentrant := reentrantCount()
		stop := h.CollectText()
		message := "Use the Agent tool exactly once.\n\nCall it with:\n" +
			"- subagent_type: codebase-locator\n" +
			"- description: rpiv locator\n" +
			"- model: " + bridgeModel + "\n" +
			"- max_turns: 8\n" +
			"- run_in_background: " + runInBackground + "\n" +
			"- prompt: Find files related to RPIV_LOCATOR_SENTINEL in this repository. Return only file paths and matching line anchors. Do not ask questions.\n\n" +
			"After the Agent tool returns, do not use more tools. " + lead + " reply exactly " + expectedMarker + "."
		if _, err := h.Send(map[string]any{"type": "prompt", "message": message}, testTimeout); err != nil {
			failWithLogs(t, h, "%v", err)
		}
		if _, err := h.WaitForMatch(func(m Event) bool {
			return m["type"] == "tool_execution_start" && strings.Contains(jsonString(m), "Agent")
		}, label+" Agent tool_execution_start", testTimeout); err != nil {
			failWithLogs(t, h, "%v", err)
		}
		if _, err := h.WaitForEvent("agent_end", testTimeout); err != nil {
			failWithLogs(t, h, "%v", err)
		}
		text := stop()
		if !regexp.MustCompile(expectedMarker).MatchString(text) {
			failWithLogs(t, h, "parent did not report %s Agent completion. Text: %s", label, head(text, 500))
		}
		for deadline := time.Now().Add(testTimeout); reentrantCount() <= beforeReentrant; time.Sleep(250 * time.Millisecond) {
			if time.Now().After(deadline) {
				failWithLogs(t, h, "%s Agent: timed out waiting for reentrant subagent query (count stayed %d)", label, beforeReentrant)
			}
		}
	}

	h.StartAndWait(0)
	runAgentPrompt(false, "PARENT-SAW-FOREGROUND-RPIV-CODEBASE-LOCATOR")
	runAgentPrompt(true, "PARENT-STARTED-BACKGROUND-RPIV-CODEBASE-LOCATOR")

	log := readDebugLog(t, h)
	if !regexp.MustCompile(`mcp handler: Agent \[toolu_`).MatchString(log) {
		failWithLogs(t, h, "debug log never showed the parent Agent MCP handler")
	}
	if stuckMarker.MatchString(log) {
		failWithLogs(t, h, "debug log contains stuck-handler/stream-overwrite signature")
	}
	t.Log("PASS")
}
