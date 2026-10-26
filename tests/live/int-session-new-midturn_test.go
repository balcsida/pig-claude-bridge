//go:build live

package live

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The mid-turn variant of the new_session orphan check: TestIntSessionNew snapshots between turns, when no
// Claude Code is alive, so its check is vacuous. Here a tool call is parked, the state in which Claude Code
// does not exit on stdin EOF, and new_session must still take it down: PiG sends session_shutdown before it
// replaces the extension runner (rpc.md, D70), and the bridge kills its children there.
func TestIntSessionNewMidTurn(t *testing.T) {
	h, cc := startWithToolInFlight(t, "session-new-midturn")
	if _, err := h.Send(map[string]any{"type": "new_session"}, 60*time.Second); err != nil {
		failWithLogs(t, h, "new_session: %v", err)
	}
	var survivors []string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		survivors = survivors[:0]
		for _, p := range cc {
			if isAlive(p) {
				survivors = append(survivors, fmt.Sprintf("%d %s", p.pid, p.command))
			}
		}
		if len(survivors) == 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(survivors) > 0 {
		log := readDebugLog(t, h)
		failWithLogs(t, h, "Claude Code from the replaced session outlived new_session by 10 s: %s\n"+
			"  bridge saw session_shutdown: %t; bridge saw session_start:new: %t",
			strings.Join(survivors, "; "), strings.Contains(log, "session_shutdown: clearing"), strings.Contains(log, "session_start:new"))
	}
}
