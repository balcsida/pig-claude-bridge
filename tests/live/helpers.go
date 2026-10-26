//go:build live

package live

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// failWithLogs ends the test with the message and the two log paths, as the TypeScript scripts print them.
func failWithLogs(t *testing.T, h *Harness, format string, args ...any) {
	t.Helper()
	t.Fatalf("%s\n  RPC log:    %s\n  Debug log:  %s", fmt.Sprintf(format, args...), h.RPCLog, h.DebugLog)
}

// prompter returns prompt(msg), which sends msg and returns the reply text, failing the test with the logs on an
// error. Call it on the test goroutine only.
func prompter(t *testing.T, h *Harness, timeout time.Duration) func(string) string {
	return func(msg string) string {
		t.Helper()
		text, err := h.PromptAndWait(msg, timeout)
		if err != nil {
			failWithLogs(t, h, "%v", err)
		}
		return text
	}
}

// checker returns check(condition, format, args...), which fails the test with the logs when condition is false.
func checker(t *testing.T, h *Harness) func(bool, string, ...any) {
	return func(condition bool, format string, args ...any) {
		t.Helper()
		if !condition {
			failWithLogs(t, h, format, args...)
		}
	}
}

// logDebugTailOnFailure prints the last n characters of the debug log if the test fails, as the TypeScript
// scripts that print a "Debug tail" from their catch block do.
func logDebugTailOnFailure(t *testing.T, h *Harness, n int) {
	t.Cleanup(func() {
		if b, err := os.ReadFile(h.DebugLog); err == nil && t.Failed() {
			t.Logf("  Debug tail: %s", tail(string(b), n))
		}
	})
}

// altProviderModel returns the alternative provider and model from the environment, skipping the test with the
// names of the variables that are unset.
func altProviderModel(t *testing.T) (provider, model string) {
	t.Helper()
	provider, model = os.Getenv("CLAUDE_BRIDGE_TESTING_ALT_PROVIDER"), os.Getenv("CLAUDE_BRIDGE_TESTING_ALT_MODEL")
	var missing []string
	if provider == "" {
		missing = append(missing, "CLAUDE_BRIDGE_TESTING_ALT_PROVIDER")
	}
	if model == "" {
		missing = append(missing, "CLAUDE_BRIDGE_TESTING_ALT_MODEL")
	}
	if len(missing) > 0 {
		t.Skip(strings.Join(missing, " / ") + " unset")
	}
	return provider, model
}

// agentDirWith returns a temp PiG agent dir holding the given files (name -> content), for Options.Env.
func agentDirWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// projectDir returns a temp project holding what the TypeScript tests read from the repo root:
// package.json, tsconfig.json, LICENSE and the compact fixtures at tests/fixtures/.
func projectDir(t *testing.T) string {
	t.Helper()
	root, _, _, err := liveEnv()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, content []byte) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", []byte("{\n  \"name\": \"pi-claude-bridge\",\n  \"version\": \"4.7.2\"\n}\n"))
	write("tsconfig.json", []byte("{\n  \"compilerOptions\": {\n    \"target\": \"ES2022\",\n    \"module\": \"ESNext\"\n  }\n}\n"))
	for _, rel := range []string{"LICENSE", "tests/fixtures/compact-file-a.txt", "tests/fixtures/compact-file-b.txt"} {
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		write(rel, content)
	}
	return dir
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func readDebugLog(t *testing.T, h *Harness) string {
	t.Helper()
	b, err := os.ReadFile(h.DebugLog)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	return string(b)
}

func head(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func tail(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

// projectHash is Claude Code's directory name for a project path: every non-alphanumeric UTF-16 unit becomes
// "-", and past 200 units the name is cut and suffixed with the base36 of the absolute djb2 hash.
func projectHash(path string) string {
	units := utf16.Encode([]rune(path))
	sanitized := make([]byte, len(units))
	for i, u := range units {
		if u < 128 && (u >= '0' && u <= '9' || u >= 'a' && u <= 'z' || u >= 'A' && u <= 'Z') {
			sanitized[i] = byte(u)
		} else {
			sanitized[i] = '-'
		}
	}
	if len(sanitized) <= 200 {
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
	return string(sanitized[:200]) + "-" + strconv.FormatInt(abs, 36)
}

// claudeProjectDir is the directory Claude Code keeps the sessions of cwd in. Tests read only this directory
// under the Claude config dir. The path is not NFC-normalised (the module has no dependencies); test paths are ASCII.
func claudeProjectDir(t *testing.T, cwd string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "projects", projectHash(resolved))
}

var queryDoneRE = regexp.MustCompile(`query done, session=([0-9a-f]+)`)

// sessionRecordsFrom reads Claude Code's transcript of the last query in a slice of the debug log from the
// project's session directory dir, as parsed JSONL records in write order. The log carries the session id
// abbreviated, which is enough to pick the file. fail must not return.
func sessionRecordsFrom(fail func(string, ...any), dir, log string) []map[string]any {
	ids := queryDoneRE.FindAllStringSubmatch(log, -1)
	if len(ids) == 0 {
		fail("no session id in debug log — the query never completed")
	}
	prefix := ids[len(ids)-1][1]
	entries, err := os.ReadDir(dir)
	if err != nil {
		fail("%v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".jsonl") {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				fail("%v", err)
			}
			var records []map[string]any
			for _, line := range strings.Split(string(data), "\n") {
				if line == "" {
					continue
				}
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					fail("%v", err)
				}
				records = append(records, rec)
			}
			return records
		}
	}
	fail("no session file for %s in %s", prefix, dir)
	return nil
}

// runExtensionLiveTests runs the named live tests of the extension module and fails unless each one passed.
// A few originals (attachment and parallel-result rebuild) call the conversion and session code directly
// instead of driving pig, and only the extension module can import that code (Go internal packages).
func runExtensionLiveTests(t *testing.T, names ...string) {
	t.Helper()
	root, _, _, err := liveEnv()
	if err != nil {
		t.Fatal(err)
	}
	pattern := "^(" + strings.Join(names, "|") + ")$"
	cmd := exec.Command("go", "test", "-race", "-tags", "live", "-count=1", "-v", "-run", pattern, "./internal/ccproto/")
	cmd.Dir = filepath.Join(root, "extensions", "pig-claude-bridge")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("go test %s: %v", pattern, err)
	}
	// go test is silent about a name that matches nothing, so each name needs its own PASS line.
	for _, name := range names {
		if !strings.Contains(string(out), "--- PASS: "+name+" (") {
			t.Fatalf("%s did not pass", name)
		}
	}
}
