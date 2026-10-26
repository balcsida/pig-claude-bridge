//go:build live

package live

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Verifies the bridge clears its shared session after a /new (port of tests/int-session-new.mjs), and
// that a Claude Code recorded before new_session does not outlive PiG's replacement of the extension runner.
// Limit: between turns this bridge keeps no Claude Code alive (as the TypeScript bridge), so the snapshot
// normally holds only the old runner and the orphan check passes vacuously; the log line below says which.
// It can only catch a claude that was in the snapshot. The leak-on-replacement class is covered by the
// shutdown test of part 25c and, with a tool call parked, by TestIntSessionNewMidTurn.
func TestIntSessionNew(t *testing.T) {
	const timeout = 180 * time.Second
	h := New(t, Options{
		Name:           "session-new",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		DefaultTimeout: timeout,
	})
	h.StartAndWait(0)
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("%s\n  RPC log:    %s\n  Debug log:  %s", fmt.Sprintf(format, args...), h.RPCLog, h.DebugLog)
	}

	t.Log("Turn 1: seed history...")
	if _, err := h.PromptAndWait("Pick a number between 1 and 100 and remember it. Reply with just the number. Do not use the memory system.", timeout); err != nil {
		fail("%v", err)
	}
	t.Log("Turn 2: more history...")
	if _, err := h.PromptAndWait("Now pick a color. Reply with just the color. Do not use the memory system.", timeout); err != nil {
		fail("%v", err)
	}

	before, err := os.ReadFile(h.DebugLog)
	if err != nil {
		fail("%v", err)
	}
	newMarkerLog := len(before)
	pigPID := h.Pig().Process.Pid
	parked := Descendants(pigPID)
	delete(parked, pigPID)
	t.Logf("descendants before new_session: %v", parked)

	t.Log("Triggering /new...")
	if _, err := h.Send(map[string]any{"type": "new_session"}, timeout); err != nil {
		fail("%v", err)
	}

	t.Log("Turn 3: prompt after /new (should be a clean start)...")
	if _, err := h.PromptAndWait("Hello fresh session. Reply with just 'hi'.", timeout); err != nil {
		fail("%v", err)
	}

	full, err := os.ReadFile(h.DebugLog)
	if err != nil {
		fail("%v", err)
	}
	postNewLog := string(full[newMarkerLog:])

	// The bridge logs `session_start:new: clearing ... shared sessions` when it observes the event.
	if !regexp.MustCompile(`session_start:new: clearing \d+ shared session`).MatchString(postNewLog) {
		fail("no `session_start:new: clearing session` marker — bridge didn't observe /new")
	}

	// First syncResult after /new must be clean-start (sharedSession=null, no prior messages on the fresh agent state).
	var syncResults []string
	for _, m := range regexp.MustCompile(`syncResult: path=(reuse|rebuild|clean-start)`).FindAllStringSubmatch(postNewLog, -1) {
		syncResults = append(syncResults, m[1])
	}
	t.Logf("  Post-/new syncResults: %q", syncResults)
	if len(syncResults) == 0 {
		fail("no syncResult markers after /new (Turn 3 didn't reach the provider?)")
	}
	if syncResults[0] != "clean-start" {
		fail("bridge took %s path after /new — expected clean-start.\n       sharedSession should be cleared by the session_start:new handler.", syncResults[0])
	}

	// PiG replaces the extension runner and its processes on new_session (rpc.md, D70): no claude recorded
	// before the command may outlive the replacement. A survivor means the bridge leaks it.
	isClaude := func(command string) bool {
		f := strings.Fields(command)
		return len(f) > 0 && filepath.Base(f[0]) == "claude"
	}
	snapshotClaudes := 0
	for _, command := range parked {
		if isClaude(command) {
			snapshotClaudes++
		}
	}
	t.Logf("claude processes in the pre-/new snapshot: %d (0 means the orphan check below is vacuous)", snapshotClaudes)
	var leaked []string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		leaked = leaked[:0]
		for pid, command := range parked {
			if now, ok := ProcessCommand(pid); ok && now == command && isClaude(command) {
				leaked = append(leaked, fmt.Sprintf("pid %d: %s", pid, command))
			}
		}
		if len(leaked) == 0 || time.Now().After(deadline) {
			break
		}
	}
	for pid, command := range parked {
		now, ok := ProcessCommand(pid)
		t.Logf("pre-/new descendant pid %d still alive: %t (%s)", pid, ok && now == command, command)
	}
	if len(leaked) > 0 {
		fail("Claude Code processes from before new_session are still alive after the next prompt completed:\n  %s", strings.Join(leaked, "\n  "))
	}
	t.Log("PASS")
}
