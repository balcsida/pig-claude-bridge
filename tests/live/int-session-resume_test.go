//go:build live

package live

import (
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Context continuity across provider switches, and AskClaude shared vs isolated mode (port of
// tests/int-session-resume.mjs). Needs the alternative provider from the environment only.
// AskClaude is enabled through <agent dir>/claude-bridge.json, in a per-test agent dir so no other test sees it.
func TestIntSessionResume(t *testing.T) {
	otherProvider, otherModel := os.Getenv("CLAUDE_BRIDGE_TESTING_ALT_PROVIDER"), os.Getenv("CLAUDE_BRIDGE_TESTING_ALT_MODEL")
	if otherProvider == "" || otherModel == "" {
		t.Skip("CLAUDE_BRIDGE_TESTING_ALT_PROVIDER / CLAUDE_BRIDGE_TESTING_ALT_MODEL unset")
	}
	const (
		timeout     = 180 * time.Second
		bridgeModel = "claude-bridge/claude-haiku-4-5"
	)
	token := func(prefix string) string { return fmt.Sprintf("%s%04x", prefix, rand.Intn(0x10000)) }
	// Random words to avoid Claude memorizing test values across runs.
	wordA, wordB, wordC := token("alpha"), token("beta"), token("gamma")
	// The AskClaude turns run after the first provider exchange only: once the transcript holds a second
	// "recall the words" exchange, the API refuses the request. This code is stated only by the user.
	code := token("code")

	agentDir := agentDirWith(t, map[string]string{
		"settings.json":      `{}`,
		"claude-bridge.json": `{"askClaude":{"enabled":true}}` + "\n",
	})
	h := New(t, Options{
		Name:           "session-resume",
		Args:           []string{"--model", otherProvider + "/" + otherModel},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		Cwd:            t.TempDir(),
		DefaultTimeout: timeout,
	})

	var mu sync.Mutex
	var lastToolResult *string
	var lastToolArgs any
	// The AskClaude turns depend on what the calling model put in the tool's prompt. Both assertions are only
	// meaningful when the prompt does not already contain the word being asked about.
	promptContains := func(word string) bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.ToLower(jsonString(lastToolArgs)), word)
	}
	resultText := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if lastToolResult == nil {
			return "", false
		}
		return *lastToolResult, true
	}
	// Like the harness's agent_end wait, but captures the last tool result and the last tool call's arguments.
	// Registered before the caller sends, so an immediate agent_end cannot be missed.
	idleWaiter := func() func() error {
		done := make(chan struct{})
		var once sync.Once
		remove := h.AddListener(func(msg Event) {
			if msg["type"] != "agent_end" {
				return
			}
			var calls []any
			var lastResult *string
			messages, _ := msg["messages"].([]any)
			for _, raw := range messages {
				m := asMap(raw)
				content, _ := m["content"].([]any)
				switch m["role"] {
				case "assistant":
					for _, b := range content {
						if bm := asMap(b); bm["type"] == "toolCall" {
							calls = append(calls, bm["arguments"])
						}
					}
				case "toolResult":
					var sb strings.Builder
					for _, c := range content {
						s, _ := asMap(c)["text"].(string)
						sb.WriteString(s)
					}
					text := sb.String()
					lastResult = &text
				}
			}
			mu.Lock()
			lastToolArgs = nil
			if len(calls) > 0 {
				lastToolArgs = calls[len(calls)-1]
			}
			if lastResult != nil {
				lastToolResult = lastResult
			}
			mu.Unlock()
			once.Do(func() { close(done) })
		})
		return func() error {
			defer remove()
			select {
			case <-done:
				return nil
			case <-time.After(timeout):
				return fmt.Errorf("Timeout waiting for idle")
			}
		}
	}
	promptAndWait := func(message string) string {
		t.Helper()
		stop := h.CollectText()
		wait := idleWaiter()
		if _, err := h.Send(map[string]any{"type": "prompt", "message": message}, timeout); err != nil {
			stop()
			failWithLogs(t, h, "%v", err)
		}
		err := wait()
		text := stop()
		if err != nil {
			failWithLogs(t, h, "%v", err)
		}
		return text
	}
	setModel := func(provider, modelID string) {
		t.Helper()
		if _, err := h.Send(map[string]any{"type": "set_model", "provider": provider, "modelId": modelID}, timeout); err != nil {
			failWithLogs(t, h, "%v", err)
		}
	}
	mustInclude := func(turn, text string, words ...string) {
		t.Helper()
		lower := strings.ToLower(text)
		for _, w := range words {
			if !strings.Contains(lower, w) {
				failWithLogs(t, h, "%s response missing '%s': %s", turn, w, text)
			}
		}
	}

	h.StartAndWait(0)

	// Turn 1: Non-provider prompt — establishes context before our provider is used
	t.Log("Turn 1: Non-provider prompt (establish context)...")
	text1 := promptAndWait(fmt.Sprintf("The first word is '%s'. The access code is '%s'. Acknowledge and be very brief.", wordA, code))
	if text1 == "" {
		failWithLogs(t, h, "Turn 1 produced no text")
	}
	t.Logf("  Response: %s", head(text1, 80))

	// Switch to provider — first provider turn with prior history (Case 2)
	bridgeProvider, bridgeModelID, _ := strings.Cut(bridgeModel, "/")
	t.Logf("Switching to %s...", bridgeModel)
	setModel(bridgeProvider, bridgeModelID)

	// Turn 2: First provider turn — should see wordA from prior non-provider history
	t.Log("Turn 2: First provider turn with prior history (Case 2)...")
	text2 := promptAndWait(fmt.Sprintf("The second word is '%s'. Also, what was the first word? Reply with both words separated by a comma.", wordB))
	t.Logf("  Response: %s", head(text2, 80))
	mustInclude("Turn 2", text2, wordA, wordB)

	// Turn 3: AskClaude shared mode — should see code, which the non-provider model was told
	t.Logf("Switching to %s/%s...", otherProvider, otherModel)
	setModel(otherProvider, otherModel)

	t.Log("Turn 3: AskClaude shared mode (should see non-provider context)...")
	promptAndWait(`Use the AskClaude tool with prompt="What is the access code? Reply with just the code."`)
	result, _ := resultText()
	t.Logf("  AskClaude args: %s", jsonString(lastToolArgs))
	t.Logf("  AskClaude result: %s", head(result, 120))
	if promptContains(code) {
		t.Logf("  INCONCLUSIVE: %s put '%s' in the prompt, so a correct answer proves nothing about shared context", otherModel, code)
	} else if !strings.Contains(strings.ToLower(result), code) {
		failWithLogs(t, h, "Turn 3 AskClaude tool result missing '%s': %s", code, result)
	}

	// Turn 4: AskClaude isolated mode — should NOT see conversation history
	t.Log("Turn 4: AskClaude isolated mode (should not see context)...")
	mu.Lock()
	lastToolResult = nil
	mu.Unlock()
	promptAndWait(`Use the AskClaude tool with prompt="What is the access code? If you don't know, say UNKNOWN." and isolated=true`)
	result, have := resultText()
	t.Logf("  AskClaude args: %s", jsonString(lastToolArgs))
	t.Logf("  AskClaude result: %s", head(result, 120))
	if promptContains(code) {
		// The ~1-in-5 flake: isolated CC is echoing a code it was handed, not one it recovered.
		t.Logf("  INCONCLUSIVE: %s put '%s' in the prompt, so isolation cannot be judged from the response", otherModel, code)
	} else if have && strings.Contains(strings.ToLower(result), code) {
		failWithLogs(t, h, "Turn 4 isolated AskClaude should not know '%s' (not in its prompt, so this is a real context leak): %s", code, result)
	}

	// Turn 5: Non-provider prompt — adds context that provider must see on switch-back
	t.Log("Turn 5: Non-provider prompt (creates missed messages)...")
	text5 := promptAndWait(fmt.Sprintf("The third word is '%s'. Acknowledge and be very brief.", wordC))
	if text5 == "" {
		failWithLogs(t, h, "Turn 5 produced no text")
	}
	t.Logf("  Response: %s", head(text5, 80))

	// Switch back to provider — context includes all prior turns (Case 4)
	t.Logf("Switching back to %s...", bridgeModel)
	setModel(bridgeProvider, bridgeModelID)

	// Turn 6: Provider resumes with missed messages (Case 4)
	t.Log("Turn 6: Provider resume with missed messages (Case 4)...")
	text6 := promptAndWait("What were all three words? Reply with just the three words separated by commas.")
	t.Logf("  Response: %s", head(text6, 80))
	mustInclude("Turn 6", text6, wordA, wordB, wordC)

	// Turn 7: Abort mid-stream — session should be invalidated, next turn should recover
	t.Log("Turn 7: Abort mid-stream (session recovery)...")
	if _, err := h.Send(map[string]any{"type": "prompt", "message": "Write a detailed 500-word essay about the history of timekeeping."}, timeout); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	// Set up idle listener before abort so we don't miss agent_end
	idle7 := idleWaiter()
	time.Sleep(2 * time.Second)
	if _, err := h.Send(map[string]any{"type": "abort"}, timeout); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	if err := idle7(); err != nil {
		failWithLogs(t, h, "%v", err)
	}

	// Turn 8: Provider turn after abort — should NOT get "conversation not found"
	t.Log("Turn 8: Provider turn after abort (should recover)...")
	text8 := promptAndWait("What were all three words? Reply with just the three words separated by commas.")
	t.Logf("  Response: %s", head(text8, 80))
	mustInclude("Turn 8", text8, wordA, wordB, wordC)

	// sessionId stability: stable across normal rebuilds, rotating exactly once per abort (the post-abort
	// rebuild takes a fresh UUID on purpose). One abort (Turn 7) means exactly 2 unique sessionIds.
	debugLog := readDebugLog(t, h)
	sessionIDs := map[string]struct{}{}
	rotatedPostAbort := 0
	for _, m := range regexp.MustCompile(`syncResult: path=(reuse|rebuild) sessionId=([a-f0-9-]+)(?: priors=\d+ (\S+))?`).FindAllStringSubmatch(debugLog, -1) {
		sessionIDs[m[2]] = struct{}{}
		if m[3] == "rotated-post-abort" {
			rotatedPostAbort++
		}
	}
	if len(sessionIDs) == 0 {
		failWithLogs(t, h, "no syncResult markers found in debug log")
	}
	if len(sessionIDs) > 2 {
		failWithLogs(t, h, "expected ≤2 distinct sessionIds (one pre-abort, one post-abort rotation), got %d: %v", len(sessionIDs), sessionIDs)
	}
	if rotatedPostAbort != 1 {
		failWithLogs(t, h, "expected exactly 1 post-abort rotation, got %d", rotatedPostAbort)
	}
	t.Logf("  sessionIds observed: %d (expected 2 due to 1 post-abort rotation)", len(sessionIDs))
	t.Log("PASS")
}
