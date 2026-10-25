//go:build live

package ccproto

// Port of pi-claude-bridge tests/int-parallel-tool-results.mjs (commit 6aa276a): parallel tool calls survive a
// session rebuild. Pi records one message per tool result; RepairToolPairing pairs only the ones sharing the
// user message right after their assistant message, so the rebuild must put them in one message. The unit tests
// pin the conversion; this pins what lands on disk and that Claude can read the results back after a resume.
//
// Kept beside the contract tests because it needs convert and ccsession, which only this module can import.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

var parallelTokens = []string{"ZEPHYR", "QUARTZ", "MARLIN"}

// piHistoryWithParallelCall is pi-shaped: one assistant turn calling read three times, one pi message per
// result, exactly what a parallel tool call leaves in pi.
func piHistoryWithParallelCall() []map[string]any {
	var calls []any
	var results []map[string]any
	for i, token := range parallelTokens {
		id := fmt.Sprintf("toolu_par%d", i)
		calls = append(calls, map[string]any{"type": "toolCall", "id": id, "name": "read", "arguments": map[string]any{"path": fmt.Sprintf("token%d.txt", i)}})
		results = append(results, map[string]any{"role": "toolResult", "toolCallId": id, "content": "The token in this file is " + token + "."})
	}
	history := []map[string]any{
		{"role": "user", "content": "Read the three token files."},
		{"role": "assistant", "provider": "claude-bridge", "content": calls},
	}
	return append(history, results...)
}

func seedRebuiltParallelSession(t *testing.T, cwd, id string) *ccsession.Session {
	t.Helper()
	s, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: liveModel, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	anthropic, _, _, err := convert.ConvertPiMessages(piHistoryWithParallelCall(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportMessages(ccsession.RepairToolPairing(anthropic), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s
}

func toolResultContents(t *testing.T, path string) (contents []any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		blocks, _ := at(rec, "message", "content").([]any)
		for _, b := range blocks {
			if at(b, "type") == "tool_result" {
				contents = append(contents, at(b, "content"))
			}
		}
	}
	return contents
}

func TestARebuiltParallelToolCallKeepsEveryResultOnDisk(t *testing.T) {
	s := seedRebuiltParallelSession(t, scratchDir(t), newUUID(t))
	results := toolResultContents(t, s.Path())

	if len(results) != len(parallelTokens) {
		t.Fatalf("expected %d tool_result blocks, got %d", len(parallelTokens), len(results))
	}
	for _, token := range parallelTokens {
		found := false
		for _, c := range results {
			if text, ok := c.(string); ok && strings.Contains(text, token) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from the rebuilt session — results: %v", token, results)
		}
	}
}

func TestCCResumesItAndClaudeCanReadAllThreeResultsBack(t *testing.T) {
	cwd, id := scratchDir(t), newUUID(t)
	seedRebuiltParallelSession(t, cwd, id)

	o := resumeOptions(t, cwd, id)
	o.ToolsSet, o.Tools = true, nil
	answer := assistantText(run(t, startLive(t, o, nil),
		"List the three tokens from the files you read earlier, uppercase, separated by spaces. Nothing else."))

	for _, token := range parallelTokens {
		if !regexp.MustCompile("(?i)" + token).MatchString(answer) {
			t.Errorf("Claude could not see %s after the rebuild — answer: %s", token, strings.TrimSpace(answer))
		}
	}
}
