//go:build live

package live

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A pig shutdown has to take the Claude Code subprocess with it (port of tests/int-shutdown-kills-cc.mjs).
//
// The SDK spawns `claude` with plain pipes and no parent-death watchdog. CC does exit on stdin EOF while it is
// merely waiting on an API response, but not while a tool call is parked: its MCP transport is gone, so CC
// reads each tool call as an ordinary tool error, feeds it to the model and issues another API request,
// forever. So the repro shuts pig down with a tool call in flight. Nothing throws when this regresses and the
// pig-side logs simply stop, so the assertion is on the process table.
//
// Under PiG the chain is pig -> extension runner -> claude, so "pi shutdown" becomes two cases: SIGTERM
// (PiG's rpc mode turns it into session_shutdown) and stdin close (PiG's documented shutdown path).

type claudeProcess struct {
	pid     int
	command string
}

// claudeDescendants are the `claude` processes under pid, at any depth. It matches on the executable's
// basename, not the whole command line: every pig process here carries the repository path in its args.
func claudeDescendants(pid int) []claudeProcess {
	var found []claudeProcess
	for p, command := range Descendants(pid) {
		if f := strings.Fields(command); p != pid && len(f) > 0 && filepath.Base(f[0]) == "claude" {
			found = append(found, claudeProcess{p, head(command, 100)})
		}
	}
	return found
}

func isAlive(p claudeProcess) bool {
	return syscall.Kill(p.pid, 0) == nil
}

// startWithToolInFlight starts pig with SlowTool, parks a 60 s tool call and returns the claude processes
// under pig at that point.
func startWithToolInFlight(t *testing.T, name string) (*Harness, []claudeProcess) {
	t.Helper()
	root, _, _, err := liveEnv()
	if err != nil {
		t.Fatal(err)
	}
	h := New(t, Options{
		Name:           name,
		Args:           []string{"-e", filepath.Join(root, "tests", "fixtures", "slow-tool-extension.ts"), "--model", "claude-bridge/claude-haiku-4-5"},
		DefaultTimeout: 60 * time.Second,
	})
	h.StartAndWait(0)
	// SlowTool never returns within the test: the bridge's handler is still waiting when pig goes down, which
	// is the state CC does not survive on its own.
	if _, err := h.Send(map[string]any{"type": "prompt", "message": "Call SlowTool with seconds=60."}, 0); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	if _, err := h.WaitForEvent("tool_execution_start", 60*time.Second); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	cc := claudeDescendants(h.Pig().Process.Pid)
	if len(cc) == 0 {
		failWithLogs(t, h, "expected a Claude Code subprocess under pig while a tool call is in flight")
	}
	t.Logf("claude under pig: %v", cc)
	// Leaving a survivor behind is the very failure under test, and it bills API requests until something stops it.
	t.Cleanup(func() {
		for _, p := range cc {
			syscall.Kill(p.pid, syscall.SIGKILL)
		}
	})
	return h, cc
}

// assertGone waits up to 5 s (the kill is a signal to another process, not a synchronous call) for every
// recorded claude to be gone and fails with the survivors.
func assertGone(t *testing.T, h *Harness, cc []claudeProcess, message string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		alive := false
		for _, p := range cc {
			alive = alive || isAlive(p)
		}
		if !alive {
			return
		}
	}
	var survivors []string
	for _, p := range cc {
		if isAlive(p) {
			survivors = append(survivors, fmt.Sprintf("%d %s", p.pid, p.command))
		}
	}
	if len(survivors) > 0 {
		failWithLogs(t, h, "%s: %s", message, strings.Join(survivors, "; "))
	}
}

func TestIntShutdownKillsCcAbort(t *testing.T) {
	h, cc := startWithToolInFlight(t, "abort-kills-cc")
	// Abort fails the prompt stream, which closes the child's stdin and with it the MCP control channel. pig
	// stays alive, so nothing else will ever reap the child: it has to be killed here or it retries the dead
	// tool call against a fresh API request forever.
	idle, remove := h.listenFor(func(m Event) bool { return m["type"] == "agent_end" })
	defer remove()
	if _, err := h.Send(map[string]any{"type": "abort"}, 0); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	if _, err := h.await(idle, 30*time.Second, "Timeout waiting for agent_end"); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	assertGone(t, h, cc, "Claude Code outlived the aborted query")
}

func TestIntShutdownKillsCcSigterm(t *testing.T) {
	h, cc := startWithToolInFlight(t, "shutdown-kills-cc")
	pig := h.Pig()
	exited := h.exitedChan()
	_ = pig.Process.Signal(syscall.SIGTERM) // rpc mode turns this into session_shutdown
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		failWithLogs(t, h, "pig did not exit within 15 s of SIGTERM")
	}
	assertGone(t, h, cc, "Claude Code outlived pig after SIGTERM")
}

func TestIntShutdownKillsCcStdinClose(t *testing.T) {
	h, cc := startWithToolInFlight(t, "stdin-close-kills-cc")
	exited := h.exitedChan()
	h.mu.Lock()
	stdin := h.stdin
	h.mu.Unlock()
	if err := stdin.Close(); err != nil { // PiG's documented shutdown path: end of input
		failWithLogs(t, h, "close stdin: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		failWithLogs(t, h, "pig did not exit within 15 s of stdin close")
	}
	assertGone(t, h, cc, "Claude Code outlived pig after stdin close")
}
