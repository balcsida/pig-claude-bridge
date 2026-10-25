package config

import (
	"os"
	"path/filepath"
)

// DebugLogPath resolves per call so the env override can change mid-process.
func DebugLogPath(agentDir string) string {
	if p := os.Getenv("CLAUDE_BRIDGE_DEBUG_PATH"); p != "" {
		return p
	}
	return filepath.Join(agentDir, "claude-bridge.log")
}

func DiagLogPath(agentDir string) string {
	return filepath.Join(agentDir, "claude-bridge-diag.log")
}

// CLIDebugLogDir holds the per-invocation Claude CLI debug logs, beside the debug log.
func CLIDebugLogDir(agentDir string) string {
	return filepath.Join(filepath.Dir(DebugLogPath(agentDir)), "cc-cli-logs")
}
