//go:build live

package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const webToolsConfig = `{"provider":{"webTools":true}}` + "\n"

// toolRuns records the tools PiG runs: name, result and error flag of each finished call.
type toolRuns struct {
	mu   sync.Mutex
	done []Event
}

func watchToolRuns(h *Harness) *toolRuns {
	r := &toolRuns{}
	h.AddListener(func(e Event) {
		if e["type"] == "tool_execution_end" {
			r.mu.Lock()
			r.done = append(r.done, e)
			r.mu.Unlock()
		}
	})
	return r
}

func (r *toolRuns) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.done...)
}

// provider.webTools: Claude Code runs WebFetch itself and PiG shows it as a call of its own WebFetch tool,
// whose result is Claude Code's. The next turn reuses the session; a model switch then rebuilds it from
// PiG's history, and the rebuilt session still holds the call and its result.
func TestIntWebTools(t *testing.T) {
	const timeout = 90 * time.Second
	agentDir := agentDirWith(t, map[string]string{"settings.json": `{}`, "claude-bridge.json": webToolsConfig})
	h := New(t, Options{
		Name:           "web-tools",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		Cwd:            t.TempDir(),
		DefaultTimeout: timeout,
	})
	runs := watchToolRuns(h)
	h.StartAndWait(0)
	prompt, check := prompter(t, h, timeout), checker(t, h)

	text1 := prompt("Use the WebFetch tool once on https://example.com and reply with the page's heading only. Do not use any other tool.")
	check(strings.Contains(text1, "Example Domain"), "turn 1 should carry the fetched heading, got %q", text1)
	done := runs.all()
	check(len(done) == 1 && done[0]["toolName"] == "WebFetch" && done[0]["isError"] != true &&
		strings.Contains(jsonString(done[0]["result"]), "Example Domain"),
		"PiG should have run WebFetch once and got Claude Code's result, got %s", jsonString(done))

	text2 := prompt("Which URL did you just fetch? Reply with the URL only.")
	check(strings.Contains(text2, "example.com"), "turn 2 should remember the fetch, got %q", text2)

	// A model switch makes the bridge write PiG's history into a new Claude Code session.
	if _, err := h.Send(map[string]any{"type": "set_model", "provider": "claude-bridge", "modelId": "claude-haiku-5-5"}, timeout); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	text3 := prompt("Reply with the heading of the page you fetched earlier, nothing else. Do not use any tool.")
	check(strings.Contains(text3, "Example Domain"), "turn 3 should still know the page, got %q", text3)
	check(len(runs.all()) == 1, "only turn 1 should have run a tool, got %s", jsonString(runs.all()))

	log := readDebugLog(t, h)
	var paths []string
	for _, m := range regexp.MustCompile(`syncResult: path=([a-z-]+)`).FindAllStringSubmatch(log, -1) {
		paths = append(paths, m[1])
	}
	check(len(paths) == 3 && paths[1] == "reuse" && paths[2] == "rebuild", "sync paths should be start, reuse, rebuild: %s", strings.Join(paths, ", "))
	check(!regexp.MustCompile(`BUG:|MCP handlers still waiting`).MatchString(log), "the debug log reports a stuck or mismatched tool result")

	// The rebuilt session is a new file, so a WebFetch call in it came from PiG's history.
	ended := regexp.MustCompile(`query-end: outcome=complete ccSession=([0-9a-f]+)`).FindAllStringSubmatch(log, -1)
	check(len(ended) == 3, "expected three completed queries, got %d", len(ended))
	files, _ := filepath.Glob(filepath.Join(claudeProjectDir(t, h.opts.Cwd), ended[2][1]+"*.jsonl"))
	check(len(files) == 1, "session file of the rebuilt query: %v", files)
	data, err := os.ReadFile(files[0])
	check(err == nil, "%v", err)
	var call, result bool
	for _, line := range strings.Split(string(data), "\n") {
		var rec struct {
			Message struct {
				Content []map[string]any `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		for _, block := range rec.Message.Content {
			call = call || (block["type"] == "tool_use" && block["name"] == "WebFetch")
			result = result || (block["type"] == "tool_result" && strings.Contains(jsonString(block["content"]), "Example Domain"))
		}
	}
	check(call && result, "the rebuilt session should hold the WebFetch call (%t) and its result (%t): %s", call, result, files[0])
}

// WebSearch takes the same path as WebFetch: PiG runs its WebSearch tool once, the result is the one Claude
// Code's search returned, and the next turn reuses the session.
func TestIntWebSearch(t *testing.T) {
	const timeout = 120 * time.Second
	agentDir := agentDirWith(t, map[string]string{"settings.json": `{}`, "claude-bridge.json": webToolsConfig})
	h := New(t, Options{
		Name:           "web-search",
		Args:           []string{"--model", "claude-bridge/claude-haiku-4-5"},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		Cwd:            t.TempDir(),
		DefaultTimeout: timeout,
	})
	runs := watchToolRuns(h)
	h.StartAndWait(0)
	prompt, check := prompter(t, h, timeout), checker(t, h)

	text1 := prompt("Use the WebSearch tool once with the query \"Go programming language official website\", " +
		"then reply with the domain of the official site only. Do not use any other tool.")
	check(strings.Contains(text1, "go.dev") || strings.Contains(text1, "golang.org"), "turn 1 should name the official site, got %q", text1)
	done := runs.all()
	// Claude Code's result is "Web search results for query: ..." followed by the links it found.
	check(len(done) == 1 && done[0]["toolName"] == "WebSearch" && done[0]["isError"] != true &&
		strings.Contains(jsonString(done[0]["result"]), "Web search results for query"),
		"PiG should have run WebSearch once and got Claude Code's result, got %s", head(jsonString(done), 600))

	text2 := prompt("Which query did you just search for? Reply with the query only.")
	check(strings.Contains(strings.ToLower(text2), "go programming language"), "turn 2 should remember the search, got %q", text2)
	check(len(runs.all()) == 1, "only turn 1 should have run a tool, got %s", head(jsonString(runs.all()), 600))

	log := readDebugLog(t, h)
	var paths []string
	for _, m := range regexp.MustCompile(`syncResult: path=([a-z-]+)`).FindAllStringSubmatch(log, -1) {
		paths = append(paths, m[1])
	}
	check(len(paths) == 2 && paths[1] == "reuse", "turn 2 must reuse the session, sync paths: %s", strings.Join(paths, ", "))
	check(!regexp.MustCompile(`BUG:|MCP handlers still waiting`).MatchString(log), "the debug log reports a stuck or mismatched tool result")
}

// Another provider's model sees the tool PiG lists, but Claude Code is not there to run it.
func TestIntWebToolsOnAnotherProvider(t *testing.T) {
	const timeout = 90 * time.Second
	provider, model := altProviderModel(t)
	agentDir := agentDirWith(t, map[string]string{"settings.json": `{}`, "claude-bridge.json": webToolsConfig})
	h := New(t, Options{
		Name:           "web-tools-other-provider",
		Args:           []string{"--model", provider + "/" + model},
		Env:            map[string]string{"PIG_CODING_AGENT_DIR": agentDir},
		Cwd:            t.TempDir(),
		DefaultTimeout: timeout,
	})
	runs := watchToolRuns(h)
	h.StartAndWait(0)
	prompter(t, h, timeout)("Call the WebSearch tool once with the query \"golang\", then reply with the exact text it returned.")
	done := runs.all()
	checker(t, h)(len(done) > 0 && done[0]["toolName"] == "WebSearch" && done[0]["isError"] == true &&
		strings.Contains(jsonString(done[0]["result"]), "only available on claude-bridge models"),
		"WebSearch should fail as claude-bridge only, got %s", jsonString(done))
}
