// Package ccproto builds the pure parts of the Claude Code process client:
// executable resolution, argv, environment and the initialize frame.
package ccproto

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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

var semverRE = regexp.MustCompile(`^\s*(\d+\.\d+\.\d+)`)

// Version runs `<exe> --version` and returns the leading semver ("2.1.310 (Claude Code)" gives "2.1.310").
func Version(exe string) (string, error) {
	out, err := exec.Command(exe, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", exe, err)
	}
	m := semverRE.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("parse %s --version output %q: no leading semver", exe, strings.TrimSpace(string(out)))
	}
	return m[1], nil
}

// OlderThan reports whether version is numerically older than min; unparsable input is not older.
func OlderThan(version, min string) bool {
	a, okA := parseSemver(version)
	b, okB := parseSemver(min)
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
