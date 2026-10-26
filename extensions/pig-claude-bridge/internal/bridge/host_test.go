package bridge

import (
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

func TestNewDebugLogsConfigProblems(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", "")
	problem := "claude-bridge: failed to parse /p/claude-bridge.json: unexpected end of JSON input"
	New(Deps{AgentDir: agentDir, Debug: true, Config: config.Config{Problems: []string{problem}}})
	if log := readDebugLog(t, agentDir); !strings.Contains(log, "] "+problem+"\n") {
		t.Fatalf("debug log lacks the config problem:\n%s", log)
	}
}
