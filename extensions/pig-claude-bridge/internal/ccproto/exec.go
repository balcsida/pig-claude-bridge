// Package ccproto builds the pure parts of the Claude Code process client:
// executable resolution, argv, environment and the initialize frame.
package ccproto

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinVersion is the oldest Claude Code CLI the bridge supports.
const MinVersion = "2.1.310"

// ResolveExecutable returns the configured path, else the claude found on PATH.
func ResolveExecutable(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude executable not found on PATH; install Claude Code or set provider.pathToClaudeCodeExecutable: %w", err)
	}
	return path, nil
}

// commandLine is how exe runs with args: a .js, .mjs or .ts entry point through node, as the SDK does.
func commandLine(exe string, args ...string) (string, []string, error) {
	switch filepath.Ext(exe) {
	case ".js", ".mjs", ".ts":
		node, err := exec.LookPath("node")
		if err != nil {
			return "", nil, fmt.Errorf("%s is a script and needs node, which is not on PATH: %w", exe, err)
		}
		return node, append([]string{exe}, args...), nil
	}
	return exe, args, nil
}

var semverRE = regexp.MustCompile(`^\s*(\d+\.\d+\.\d+)`)

// VersionTimeout bounds `claude --version`: a binary that never answers must not hang the turn.
var VersionTimeout = 10 * time.Second

// Version runs `<exe> --version` and returns the leading semver ("2.1.310 (Claude Code)" gives "2.1.310").
func Version(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), VersionTimeout)
	defer cancel()
	name, args, err := commandLine(exe, "--version")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// A grandchild can keep stdout open after the kill; stop waiting for it.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("run %s --version: no answer within %s", exe, VersionTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", exe, err)
	}
	m := semverRE.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("parse %s --version output %q: no leading semver", exe, strings.TrimSpace(string(out)))
	}
	return m[1], nil
}

// OlderThan reports whether version is numerically older than minimum; unparsable input is not older.
func OlderThan(version, minimum string) bool {
	a, okA := parseSemver(version)
	b, okB := parseSemver(minimum)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func parseSemver(s string) ([3]int, bool) {
	var v [3]int
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i, p := range strings.Split(m[1], ".") {
		v[i], _ = strconv.Atoi(p)
	}
	return v, true
}
