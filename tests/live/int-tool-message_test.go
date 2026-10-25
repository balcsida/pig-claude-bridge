//go:build live

package live

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Tool execution plus message interaction scenarios (port of tests/int-tool-message.mjs): pig in RPC mode
// with the bridge and the SlowTool test extension, exercising how messages arriving during a tool call
// are handled. One pig process serves every subtest, as in the TypeScript describe block.
func TestIntToolMessage(t *testing.T) {
	const testTimeout = 40 * time.Second
	root, _, _, err := liveEnv()
	if err != nil {
		t.Fatal(err)
	}
	h := New(t, Options{
		Name:           "tool-message",
		Args:           []string{"-e", filepath.Join(root, "tests", "fixtures", "slow-tool-extension.ts"), "--model", "claude-bridge/claude-haiku-4-5"},
		DefaultTimeout: testTimeout,
	})
	h.StartAndWait(0)
	cwd := h.opts.Cwd

	// The debug log accumulates across subtests (one pig process), so scope assertions to the bytes a single
	// subtest wrote.
	logMark := func() int {
		info, err := os.Stat(h.DebugLog)
		if err != nil {
			failWithLogs(t, h, "%v", err)
		}
		return int(info.Size())
	}
	logSince := func(mark int) string { return readDebugLog(t, h)[mark:] }

	// test runs body with the given timeout (a test-level bound as in node:test). The body runs in its own
	// goroutine so a hang fails the subtest instead of the whole run; fail ends the body.
	test := func(name string, timeout time.Duration, body func(fail func(format string, args ...any))) {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				fail := func(format string, args ...any) {
					t.Errorf("%s\n  RPC log:    %s\n  Debug log:  %s", fmt.Sprintf(format, args...), h.RPCLog, h.DebugLog)
					runtime.Goexit()
				}
				body(fail)
			}()
			select {
			case <-done:
			case <-time.After(timeout):
				t.Errorf("test timed out after %s\n  RPC log:    %s\n  Debug log:  %s", timeout, h.RPCLog, h.DebugLog)
			}
			// afterEach: restart pig when the test left it dead.
			select {
			case <-h.exitedChan():
				h.StartAndWait(0)
			default:
			}
		})
	}

	// must fails the subtest on an error from a harness call.
	must := func(fail func(string, ...any), err error) {
		if err != nil {
			fail("%v", err)
		}
	}
	prompt := func(fail func(string, ...any), message, behavior string) {
		cmd := map[string]any{"type": "prompt", "message": message}
		if behavior != "" {
			cmd["streamingBehavior"] = behavior
		}
		_, err := h.Send(cmd, testTimeout)
		must(fail, err)
	}
	waitEvent := func(fail func(string, ...any), typ string) {
		_, err := h.WaitForEvent(typ, testTimeout)
		must(fail, err)
	}
	textDelta := func(m Event) bool {
		return m["type"] == "message_update" && asMap(m["assistantMessageEvent"])["type"] == "text_delta"
	}
	records := func(log string) []map[string]any { return sessionRecordsFrom(t, h, cwd, log) }
	// mentions mirrors the TypeScript check: the token appears in the record's attachment or message content.
	mentions := func(r map[string]any, token string) bool {
		return strings.Contains(jsonString(r["attachment"]), token) || strings.Contains(jsonString(asMap(r["message"])["content"]), token)
	}
	slowToolCompleted := regexp.MustCompile(`slowtool completed`)

	test("tool call completes normally", testTimeout, func(fail func(string, ...any)) {
		text, err := h.PromptAndWait("Call SlowTool with seconds=1. Then repeat exactly what it returned, nothing else.", testTimeout)
		must(fail, err)
		if !slowToolCompleted.MatchString(strings.ToLower(text)) {
			fail("no `slowtool completed` in: %s", head(text, 300))
		}
	})

	test("followUp during tool execution delivers after tool completes", testTimeout, func(fail func(string, ...any)) {
		collector := h.CollectText()
		prompt(fail, "Call SlowTool with seconds=5. Then repeat exactly what it returned.", "")
		waitEvent(fail, "tool_execution_start")
		// followUp is queued by pi until the current turn finishes
		prompt(fail, "This is a followUp during tool execution.", "followUp")
		waitEvent(fail, "agent_end")
		text := collector()
		if !slowToolCompleted.MatchString(strings.ToLower(text)) {
			fail("no `slowtool completed` in: %s", head(text, 300))
		}
	})

	test("steer during tool execution still delivers tool result", 15*time.Second, func(fail func(string, ...any)) {
		// Issue #3: steer injects a user message into the context during an active tool call.
		// extractAllToolResults stops at the user message and returns 0 results, leaving the pending handler stuck.
		collector := h.CollectText()
		prompt(fail, "Call SlowTool with seconds=2. Then repeat exactly what it returned.", "")
		waitEvent(fail, "tool_execution_start")
		prompt(fail, "This is a steer message during tool execution.", "steer")
		waitEvent(fail, "agent_end")
		text := collector()
		if !slowToolCompleted.MatchString(strings.ToLower(text)) {
			fail("no `slowtool completed` in: %s", head(text, 300))
		}
	})

	test("parallel tool calls with steer delivers all results", testTimeout, func(fail func(string, ...any)) {
		collector := h.CollectText()
		mark := logMark()
		prompt(fail, "Call SlowTool three times in parallel: seconds=3, seconds=4, seconds=5. Then list all three results.", "")
		// Wait for at least one tool to start, then inject steer
		waitEvent(fail, "tool_execution_start")
		prompt(fail, "This is a steer during parallel tool execution. IMPORTANT: also say the exact word 'PAPAYA' on its own line in your response.", "steer")
		waitEvent(fail, "agent_end")
		text := collector()
		// All three tools should have their results in the response
		if matches := strings.Count(strings.ToLower(text), "slowtool completed"); matches < 3 {
			fail("Expected 3 SlowTool results, found %d: %s", matches, head(text, 300))
		}
		// Delivery only forwards a steer when the trailing message is a user message. Pi injects drained steers
		// between tool results too, and in that shape the steer would be dropped and the cursor advanced past
		// it, so Claude never sees it. A model-echo assert is unreliable (CC delivers a drained steer as a
		// <system-reminder> inside the tool_result content, which its own guidance tells the model to distrust),
		// so assert the transport fact: the steer reached CC's session at all.
		found := false
		for _, r := range records(logSince(mark)) {
			found = found || mentions(r, "PAPAYA")
		}
		if !found {
			fail("steer never reached CC's session — dropped between the parallel tool results")
		}
	})

	test("steer during text response (no tool call) completes both turns", testTimeout, func(fail func(string, ...any)) {
		// Steer during text-only streaming: the assistant is generating text (no tool calls), a steer arrives,
		// and pi delivers it after the current turn ends. Risk: if activeQuery hasn't been cleared by the time
		// pi calls streamSimple for the steer, the bridge enters the tool-result-delivery path incorrectly.
		collector := h.CollectText()
		prompt(fail, "Write exactly 12 short numbered sentences about the history of computing, from Babbage to modern times. Do NOT call any tools.", "")
		// Wait until text is actually streaming before injecting the steer
		_, err := h.WaitForMatch(textDelta, "text_delta during assistant response", testTimeout)
		must(fail, err)
		prompt(fail, "After you finish, also say the exact word 'PINEAPPLE' on its own line.", "steer")
		waitEvent(fail, "agent_end")
		text := collector()
		if !strings.Contains(strings.ToLower(text), "pineapple") {
			fail("no `pineapple` in: %s", head(text, 300))
		}
	})

	test("steer during tool execution is visible to assistant", 20*time.Second, func(fail func(string, ...any)) {
		// Bug: when a steer arrives during tool execution, pi drains it at the turn boundary and injects it into
		// context alongside the tool result. The bridge sees activeQuery=true, enters tool-result-delivery mode,
		// extracts the tool result, but silently ignores the trailing user message (the steer).
		mark := logMark()
		prompt(fail, "Call SlowTool with seconds=2. After it returns, repeat exactly what it returned.", "")
		waitEvent(fail, "tool_execution_start")
		prompt(fail, "IMPORTANT: Also say the exact word 'MANGO' on its own line in your response.", "steer")
		waitEvent(fail, "agent_end")
		// Structural assert: the steer must reach CC's session. Model echo is not assertable on CC 2.1.280
		// (the steer arrives as a <system-reminder> inside the tool_result content).
		found := false
		for _, r := range records(logSince(mark)) {
			found = found || mentions(r, "MANGO")
		}
		if !found {
			fail("steer during tool execution never reached CC's session — dropped at delivery")
		}
	})

	test("steer is drained at the tool boundary, mid-turn", 90*time.Second, func(fail func(string, ...any)) {
		// The point of the whole steering fix, and the tripwire for the CC CLI internals it rests on. Proof has
		// to be structural, from CC's own session transcript: what only mid-turn steering produces is a
		// `queued_command` attachment sitting between the tool result and the next assistant message. A steer
		// that lost the stdin race lands after that assistant message, and a replayed one is a plain user
		// prompt with no attachment at all.
		mark := logMark()
		steerText := "STOP. Do not call SlowTool again. Reply with only the word BANANA."
		prompt(fail, "Call SlowTool with seconds=1 exactly 12 times, strictly one at a time — wait for each result before starting the next. Do not call it twice in the same message.", "")
		waitEvent(fail, "tool_execution_start")
		prompt(fail, steerText, "steer")
		waitEvent(fail, "agent_end")

		recs := records(logSince(mark))
		steerAt := -1
		for i, r := range recs {
			att := asMap(r["attachment"])
			if att["type"] == "queued_command" && strings.Contains(jsonString(att["prompt"]), "Do not call SlowTool again") {
				steerAt = i
				break
			}
		}
		if steerAt == -1 {
			fail("steer never reached CC as a queued command — it was replayed as a follow-up")
		}

		toolResultAt := -1
		for i := steerAt - 1; i >= 0; i-- {
			if strings.Contains(jsonString(asMap(recs[i]["message"])["content"]), `"tool_result"`) {
				toolResultAt = i
				break
			}
		}
		if toolResultAt == -1 {
			fail("no tool result before the steer — test did not reach a tool boundary")
		}
		between := 0
		for _, r := range recs[toolResultAt+1 : steerAt] {
			if r["type"] == "assistant" {
				between++
			}
		}
		if between != 0 {
			fail("steer was drained after the turn continued (%d assistant message(s) between tool result and steer) — it lost the stdin race", between)
		}
		responded := false
		for _, r := range recs[steerAt:] {
			responded = responded || r["type"] == "assistant"
		}
		if !responded {
			fail("CC never responded after draining the steer")
		}
		// Corroboration retired, as in the TypeScript test: CC delivers a drained steer as a <system-reminder>
		// concatenated into the tool_result content, and model compliance reflects CC's guard, not the bridge.
	})

	test("steer at a text-only boundary is not pushed into the active query", testTimeout, func(fail func(string, ...any)) {
		// A steer at the end of a text-only turn can race the clearing of activeQuery and be routed as a
		// reentrant user query. It cannot be pushed into the previous query: there is no tool boundary to steer
		// at, and that query's input generator has already been ended. This asserts only that no push is
		// attempted; read it as a tripwire against a future change that starts pushing text-only steers.
		mark := logMark()
		collector := h.CollectText()
		prompt(fail, "Write exactly 12 short numbered sentences about the history of computing. Do NOT call any tools.", "")
		_, err := h.WaitForMatch(textDelta, "text_delta during assistant response", testTimeout)
		must(fail, err)
		prompt(fail, "After you finish, also say the exact word 'KIWI' on its own line.", "steer")
		waitEvent(fail, "agent_end")
		text := collector()
		log := logSince(mark)

		if !strings.Contains(strings.ToLower(text), "kiwi") {
			fail("no `kiwi` in: %s", head(text, 300))
		}
		if regexp.MustCompile(`steer written to CC stdin`).MatchString(log) {
			fail("text-only steer was pushed into the active query's input stream")
		}
		if regexp.MustCompile(`steer push rejected`).MatchString(log) {
			fail("text-only steer reached the push path at all")
		}
	})

	test("abort during tool execution recovers cleanly", testTimeout, func(fail func(string, ...any)) {
		prompt(fail, "Call SlowTool with seconds=30.", "")
		waitEvent(fail, "tool_execution_start")
		idle, remove := h.listenFor(func(m Event) bool { return m["type"] == "agent_end" })
		defer remove()
		_, err := h.Send(map[string]any{"type": "abort"}, testTimeout)
		must(fail, err)
		_, err = h.await(idle, testTimeout, "Timeout waiting for agent_end")
		must(fail, err)
		// Next prompt should work without hanging
		text, err := h.PromptAndWait("Reply with just the word 'recovered'.", testTimeout)
		must(fail, err)
		if !strings.Contains(strings.ToLower(text), "recovered") {
			fail("no `recovered` in: %s", head(text, 300))
		}
	})
}
