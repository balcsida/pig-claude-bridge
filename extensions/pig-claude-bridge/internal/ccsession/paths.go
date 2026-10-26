// Package ccsession ports the subset of the Node library cc-session-io 0.4.0
// that the bridge uses: Claude Code session file paths, records and chained
// writes.
package ccsession

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

const maxSanitizedLength = 200

// ClaudeDir returns override, else CLAUDE_CONFIG_DIR, else ~/.claude. An empty
// CLAUDE_CONFIG_DIR counts as unset: the library's ?? would use it as a relative
// path and scatter session files into the working directory.
func ClaudeDir(override string) string {
	if override != "" {
		return override
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// HOME is unset: Node's os.homedir() then reads the passwd entry.
		// shortcut: without a passwd entry either the dir is relative where Node throws; return an error if a caller must tell.
		if u, uerr := user.Current(); uerr == nil {
			home = u.HomeDir
		}
	}
	return filepath.Join(home, ".claude")
}

// NormalizeProjectPath resolves symlinks then applies NFC; when the path
// cannot be resolved it applies NFC to the raw path.
func NormalizeProjectPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			return norm.NFC.String(abs)
		}
	}
	return norm.NFC.String(p)
}

// ProjectPathToHash replaces every non-alphanumeric UTF-16 code unit with "-"
// (as the library's JS regex does) and, past 200 units, truncates and appends
// the base36 of the absolute 32-bit djb2 hash.
func ProjectPathToHash(p string) string {
	units := utf16.Encode([]rune(p))
	sanitized := make([]byte, len(units))
	for i, u := range units {
		if u < 128 && (u >= '0' && u <= '9' || u >= 'a' && u <= 'z' || u >= 'A' && u <= 'Z') {
			sanitized[i] = byte(u)
		} else {
			sanitized[i] = '-'
		}
	}
	if len(sanitized) <= maxSanitizedLength {
		return string(sanitized)
	}
	var h int32
	for _, u := range units {
		h = h<<5 - h + int32(u)
	}
	abs := int64(h)
	if abs < 0 {
		abs = -abs
	}
	return string(sanitized[:maxSanitizedLength]) + "-" + strconv.FormatInt(abs, 36)
}

// ProjectDir is <claudeDir>/projects/<hash of the normalised project path>.
func ProjectDir(claudeDir, projectPath string) string {
	return filepath.Join(ClaudeDir(claudeDir), "projects", ProjectPathToHash(NormalizeProjectPath(projectPath)))
}

// SessionPath is the session's JSONL file inside ProjectDir.
func SessionPath(claudeDir, projectPath, sessionID string) string {
	return filepath.Join(ProjectDir(claudeDir, projectPath), sessionID+".jsonl")
}
