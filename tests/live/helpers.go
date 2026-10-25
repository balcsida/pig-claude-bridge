//go:build live

package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// failWithLogs ends the test with the message and the two log paths, as the TypeScript scripts print them.
func failWithLogs(t *testing.T, h *Harness, format string, args ...any) {
	t.Helper()
	t.Fatalf("%s\n  RPC log:    %s\n  Debug log:  %s", fmt.Sprintf(format, args...), h.RPCLog, h.DebugLog)
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
