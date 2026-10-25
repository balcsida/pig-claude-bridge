package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

// fakeHost is the per-call PiG adapter.
type fakeHost struct {
	mu        sync.Mutex
	notices   []string
	sessionID string
	cwd       string
	prompt    string
}

func (h *fakeHost) Notify(message, level string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notices = append(h.notices, level+": "+message)
}
func (h *fakeHost) SessionID() (string, error) { return h.sessionID, nil }
func (h *fakeHost) Cwd() string                { return h.cwd }
func (h *fakeHost) AgentDir() string           { return "" }
func (h *fakeHost) ConfigDirName() string      { return ".pig" }
func (h *fakeHost) SystemPrompt() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prompt, nil
}

func (h *fakeHost) setPrompt(p string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompt = p
}

func (h *fakeHost) noticeList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.notices...)
}

// spawn is one Claude Code process the bridge started.
type spawn struct {
	opts     ccproto.Options
	handlers ccproto.Handlers
	client   *fakeClient
}

// providerHarness serves provider calls with scripted fake processes. The claude
// binary is a stub script that only prints a version.
type providerHarness struct {
	t         *testing.T
	b         *Bridge
	host      *fakeHost
	exe       string
	agentDir  string
	claudeDir string

	mu      sync.Mutex
	spawned []*spawn
	scripts []func(*spawn)
}

func writeStubClaude(t *testing.T, version string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho \""+version+" (Claude Code)\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

// newProviderHarness builds a Bridge on throwaway dirs. mutate adjusts the Deps
// (the provider config is non-nil and names the stub binary and the max plan).
func newProviderHarness(t *testing.T, mutate func(*Deps)) *providerHarness {
	t.Helper()
	h := &providerHarness{t: t, host: &fakeHost{}, claudeDir: t.TempDir(), agentDir: t.TempDir()}
	h.exe = writeStubClaude(t, "2.1.999")
	t.Setenv("CLAUDE_CONFIG_DIR", h.claudeDir)
	t.Setenv("CLAUDE_BRIDGE_DEBUG_PATH", "")
	plan := "max"
	d := Deps{
		AgentDir: h.agentDir, ConfigDirName: ".pig",
		Config: config.Config{Provider: &config.ProviderConfig{PathToClaudeCodeExecutable: &h.exe, Plan: &plan}},
		Spawn:  h.spawn,
	}
	if mutate != nil {
		mutate(&d)
	}
	h.b = New(d)
	t.Cleanup(func() {
		h.mu.Lock()
		spawned := append([]*spawn(nil), h.spawned...)
		h.mu.Unlock()
		for _, s := range spawned {
			_ = s.client.Close(context.Background())
		}
		h.b.queries.Wait()
	})
	return h
}

func (h *providerHarness) spawn(_ context.Context, o ccproto.Options, hd ccproto.Handlers) (ccproto.Client, error) {
	s := &spawn{opts: o, handlers: hd, client: newFakeClient()}
	h.mu.Lock()
	h.spawned = append(h.spawned, s)
	var script func(*spawn)
	if len(h.scripts) > 0 {
		script, h.scripts = h.scripts[0], h.scripts[1:]
	}
	h.mu.Unlock()
	if script == nil {
		return nil, errors.New("no fake script queued")
	}
	script(s)
	return s.client, nil
}

// script queues what the next spawned process does at start.
func (h *providerHarness) script(fn func(*spawn)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scripts = append(h.scripts, fn)
}

func (h *providerHarness) spawns() []*spawn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*spawn(nil), h.spawned...)
}

func (h *providerHarness) lastSpawn() *spawn {
	s := h.spawns()
	return s[len(s)-1]
}

// call is a provider call. tools may be nil.
func (h *providerHarness) call(ctx context.Context, sessionID string, messages []map[string]any, tools ...any) *sdk.ModelEventStream {
	h.t.Helper()
	stream, err := h.b.StreamSimple(ctx, h.host, testModel, map[string]any{"messages": messages, "tools": tools}, map[string]any{"sessionId": sessionID})
	if err != nil {
		h.t.Fatal(err)
	}
	return stream
}

func (h *providerHarness) waitLive(n int) {
	h.t.Helper()
	waitFor(h.t, "live queries", func() bool { return len(h.b.liveContexts()) == n })
}

// liveFor is the live query serving piSessionID.
func (h *providerHarness) liveFor(piSessionID string) *QueryContext {
	h.t.Helper()
	var found *QueryContext
	waitFor(h.t, "query of "+piSessionID, func() bool {
		for _, c := range h.b.liveContexts() {
			if c.piSessionID == piSessionID {
				found = c
				return true
			}
		}
		return false
	})
	return found
}

// --- scripted frames and pi-side messages ---

// initFrame is Claude Code's init: the session it resumed, else a new one.
func initFrame(s *spawn, id string) map[string]any {
	if s.opts.Resume != "" {
		id = s.opts.Resume
	}
	return map[string]any{"type": "system", "subtype": "init", "session_id": id}
}

func textReplyFrames(text string) []any {
	return []any{
		cev(`{"type":"message_start","message":{"id":"msg_text","usage":{}}}`),
		cev(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		cev(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + jsonString(text) + `}}`),
		cev(`{"type":"content_block_stop","index":0}`),
		cev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`),
		cev(`{"type":"message_stop"}`),
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func resultFrame(text string) map[string]any {
	return map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text}
}

// answers is a script: init, a text reply, a result, a clean exit.
func answers(id, text string) func(*spawn) {
	return func(s *spawn) {
		s.client.emit(initFrame(s, id))
		s.client.emit(textReplyFrames(text)...)
		s.client.emit(resultFrame(text))
		s.client.exit(0, "")
	}
}

// parksOnTool is a script: init, then a tool call the caller completes later.
func parksOnTool(id, toolCallID string) func(*spawn) {
	return func(s *spawn) {
		s.client.emit(initFrame(s, id))
		s.client.emit(toolUseFrames(toolCallID)...)
	}
}

// finish ends a parked process the way Claude Code does after its last result.
func finish(s *spawn, text string) {
	s.client.emit(resultFrame(text))
	s.client.exit(0, "")
}

var clock = 0

func nextTimestamp() int { clock++; return clock }

func userMsg(text string) map[string]any {
	return map[string]any{"role": "user", "content": text, "timestamp": nextTimestamp()}
}

func assistantMsg(content []any, stopReason string) map[string]any {
	return map[string]any{
		"role": "assistant", "content": content, "api": "claude-bridge", "provider": "claude-bridge", "model": "claude-bridge/opus",
		"usage": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0,
			"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}},
		"stopReason": stopReason, "timestamp": nextTimestamp(),
	}
}

func sayMsg(text string) map[string]any {
	return assistantMsg([]any{map[string]any{"type": "text", "text": text}}, "stop")
}

func toolCallMsg(id string) map[string]any {
	return assistantMsg([]any{map[string]any{"type": "toolCall", "id": id, "name": "read", "arguments": map[string]any{"path": "a"}}}, "toolUse")
}

func toolResultMsg(id string) map[string]any {
	return map[string]any{"role": "toolResult", "toolCallId": id, "toolName": "read", "isError": false, "timestamp": nextTimestamp(),
		"content": []any{map[string]any{"type": "text", "text": "file"}}}
}

func msgs(m ...map[string]any) []map[string]any { return m }

var readTool = map[string]any{"name": "read", "description": "Read a file", "parameters": map[string]any{
	"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}}

// finalOf waits for the stream's terminal message.
func finalOf(t *testing.T, s *sdk.ModelEventStream) map[string]any {
	t.Helper()
	done := make(chan map[string]any, 1)
	go func() { done <- s.Result() }()
	return receive(t, done, "the stream to end")
}

// eventsOf collects the events up to the terminal one.
func eventsOf(t *testing.T, s *sdk.ModelEventStream) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var events []map[string]any
	for e := range s.Events(ctx) {
		events = append(events, e)
	}
	if ctx.Err() != nil {
		t.Fatal("the stream never ended")
	}
	return events
}

func replyText(message map[string]any) string {
	var sb strings.Builder
	for _, blk := range list(message["content"]) {
		if b := obj(blk); b["type"] == "text" {
			sb.WriteString(str(b["text"]))
		}
	}
	return sb.String()
}

// firstPrompt is the text of the first user message written to the process.
func firstPrompt(t *testing.T, s *spawn) string {
	t.Helper()
	waitFor(t, "the prompt to be written", func() bool { return len(s.client.writes()) > 0 })
	blocks, _ := s.client.writes()[0].Content.([]any)
	return str(obj(blocks[0])["text"])
}

// sessionFile is where Claude Code's session id lives for cwd.
func (h *providerHarness) sessionFile(id, cwd string) string {
	return ccsession.SessionPath(h.claudeDir, cwd, id)
}

// mcpCall is Claude Code calling pi tool name over the spawn's SDK MCP server.
type spawnMcp struct{ s *spawn }

func (m spawnMcp) Handle(ctx context.Context, msg json.RawMessage) (json.RawMessage, bool, error) {
	return m.s.handlers.OnMcpMessage(ctx, "custom-tools", msg)
}

// --- the options handed to Spawn ---

// TestStreamSimpleBuildsProviderOptions asserts the Options reproduce the Node SDK's
// recorded argv, filtered env and initialize frame for the same inputs
// (internal/ccproto/testdata/sdk-argv-snapshot.json, entry "provider").
func TestStreamSimpleBuildsProviderOptions(t *testing.T) {
	h := newProviderHarness(t, func(d *Deps) { d.Config.Provider.ForceTwoHundredK = []string{"claude-opus-5-5"} })
	cwd := t.TempDir()
	h.host.cwd, h.host.sessionID = cwd, "pi-options"
	h.b.OnSessionStart(h.host, "startup")
	h.b.OnBeforeAgentStart(h.host, "SYSTEM", map[string]any{"appendSystemPrompt": "APPEND"})
	h.script(answers("cc-options", "ok"))
	model := map[string]any{"api": "anthropic-messages", "provider": "anthropic", "id": "claude-opus-5-5"}

	stream, err := h.b.StreamSimple(context.Background(), h.host, model,
		map[string]any{"systemPrompt": "SYSTEM", "messages": msgs(userMsg("hi")), "tools": []any{readTool}},
		map[string]any{"sessionId": "pi-options", "reasoning": "high"})
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, stream)

	got := h.lastSpawn().opts
	opus := "claude-opus-5-5"
	summarized := "summarized"
	want := ccproto.Options{
		Executable: h.exe, Cwd: cwd, Effort: "high", Tools: []string{}, ToolsSet: true, PermissionMode: "bypassPermissions",
		IncludePartialMessages: true,
		Settings: map[string]any{
			"autoMemoryEnabled": false, "includeGitInstructions": false,
			"claudeMdExcludes": []string{"**/CLAUDE.md", "**/AGENTS.md", "**/.claude/rules/**"},
		},
		SDKMcpServers: []string{"custom-tools"}, AppendSystemPrompt: "APPEND",
		ExtraArgs: []ccproto.ExtraArg{{Key: "model", Value: &opus}, {Key: "replay-user-messages"}, {Key: "strict-mcp-config"}, {Key: "thinking-display", Value: &summarized}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options\n got  %+v\n want %+v", got, want)
	}

	raw, err := os.ReadFile("../ccproto/testdata/sdk-argv-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Name       string
		Argv       []string
		Env        map[string]string
		Initialize json.RawMessage
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	var snap *struct {
		Name       string
		Argv       []string
		Env        map[string]string
		Initialize json.RawMessage
	}
	for i := range entries {
		if entries[i].Name == "provider" {
			snap = &entries[i]
		}
	}
	if snap == nil {
		t.Fatal("no provider entry in the snapshot")
	}
	argv := ccproto.BuildArgs(got)
	if len(argv) != len(snap.Argv) {
		t.Fatalf("argv\n got  %q\n want %q", argv, snap.Argv)
	}
	for i, w := range snap.Argv {
		if i > 0 && snap.Argv[i-1] == "--settings" {
			var a, b any
			if json.Unmarshal([]byte(argv[i]), &a) != nil || json.Unmarshal([]byte(w), &b) != nil || !reflect.DeepEqual(a, b) {
				t.Errorf("--settings: got %s want %s", argv[i], w)
			}
		} else if argv[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], w)
		}
	}
	env := map[string]string{}
	for _, kv := range ccproto.BuildEnv(nil, got) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if !reflect.DeepEqual(env, snap.Env) {
		t.Errorf("env\n got  %v\n want %v", env, snap.Env)
	}
	var wantInit bytes.Buffer
	if err := json.Compact(&wantInit, snap.Initialize); err != nil {
		t.Fatal(err)
	}
	if init := ccproto.BuildInitialize(got, "<id>"); !bytes.Equal(init, wantInit.Bytes()) {
		t.Errorf("initialize\n got  %s\n want %s", init, wantInit.Bytes())
	}
}

func TestStreamSimpleOptionsVariants(t *testing.T) {
	t.Run("strictMcpConfig false drops the flag, no tools means no SDK MCP server, no effort means no thinking display", func(t *testing.T) {
		off := false
		h := newProviderHarness(t, func(d *Deps) { d.Config.Provider.StrictMcpConfig = &off })
		h.script(answers("cc-variants", "ok"))
		finalOf(t, h.call(context.Background(), "pi-variants", msgs(userMsg("hi"))))
		o := h.lastSpawn().opts
		for _, a := range o.ExtraArgs {
			if a.Key == "strict-mcp-config" || a.Key == "thinking-display" {
				t.Fatalf("unexpected extra arg %s", a.Key)
			}
		}
		if o.SDKMcpServers != nil || o.Effort != "" || h.lastSpawn().handlers.OnMcpMessage != nil {
			t.Fatalf("options = %+v", o)
		}
	})
	t.Run("the session the sync resumes is passed to Claude Code", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(answers("cc-first", "one"))
		finalOf(t, h.call(context.Background(), "pi-resume", msgs(userMsg("hi"))))
		h.script(answers("cc-second", "two"))
		finalOf(t, h.call(context.Background(), "pi-resume", msgs(userMsg("hi"), sayMsg("one"), userMsg("again"))))
		if got := h.lastSpawn().opts.Resume; got != "cc-first" {
			t.Fatalf("resume = %q", got)
		}
	})
	t.Run("debug adds the CLI debug file and forwards stderr lines into the bridge log", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) { d.Debug = true })
		h.script(answers("cc-debug", "ok"))
		finalOf(t, h.call(context.Background(), "pi-debug", msgs(userMsg("hi"))))
		s := h.lastSpawn()
		if s.opts.DebugFile == "" {
			t.Fatal("no debug file")
		}
		s.handlers.OnStderr("first line\r\nsecond line\n")
		log := readDebugLog(t, h.agentDir)
		if !strings.Contains(log, "[cli-stderr provider#1] first line") || !strings.Contains(log, "[cli-stderr provider#1] second line") {
			t.Fatalf("log:\n%s", log)
		}
	})
	t.Run("a model whose id carries the long-context policy is sent with it", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(answers("cc-long", "ok"))
		model := map[string]any{"api": "anthropic-messages", "provider": "anthropic", "id": "claude-opus-5-5"}
		stream, _ := h.b.StreamSimple(context.Background(), h.host, model, map[string]any{"messages": msgs(userMsg("hi"))}, map[string]any{"sessionId": "pi-long"})
		finalOf(t, stream)
		if v := h.lastSpawn().opts.ExtraArgs[0].Value; v == nil || *v != "claude-opus-5-5[1m]" {
			t.Fatalf("model arg = %v", v)
		}
	})
}

// --- tool results ---

func TestStreamSimpleDeliversAToolResultToItsParkedQuery(t *testing.T) {
	h := newProviderHarness(t, nil)
	h.script(parksOnTool("cc-deliver", "toolu_1"))
	first := h.call(context.Background(), "pi-deliver", msgs(userMsg("read a")), readTool)
	if got := finalOf(t, first); got["stopReason"] != "toolUse" {
		t.Fatalf("first turn = %v", got["stopReason"])
	}
	s := h.lastSpawn()
	q := h.liveFor("pi-deliver")
	reply := callTool(context.Background(), spawnMcp{s}, "read", "toolu_1")
	waitParked(t, q, 1)

	history := msgs(userMsg("read a"), toolCallMsg("toolu_1"), toolResultMsg("toolu_1"))
	second := h.call(context.Background(), "pi-deliver", history, readTool)
	if got := receive(t, reply, "the tool reply"); got.Text != "file" || got.IsError {
		t.Fatalf("handler reply = %+v", got)
	}
	s.client.emit(textReplyFrames("done")...)
	finish(s, "done")
	if got := finalOf(t, second); replyText(got) != "done" {
		t.Fatalf("second turn = %v", got)
	}
	if len(h.spawns()) != 1 {
		t.Fatalf("spawns = %d, the result must not start a query", len(h.spawns()))
	}
	h.waitLive(0)
	if st := h.b.sessionStateFor("pi-deliver"); st == nil || st.SessionID != "cc-deliver" || st.Cursor != 3 {
		t.Fatalf("mirror = %+v", st)
	}
}

// --- unit-orphan-result-after-restart.mjs ---

func TestOrphanResultThroughTheProvider(t *testing.T) {
	// abortParked parks a query on a tool call and aborts it, as a user abort during a tool does.
	abortParked := func(t *testing.T, h *providerHarness, session, toolCallID string, ctx context.Context, cancel context.CancelFunc) {
		h.script(parksOnTool("cc-"+session, toolCallID))
		first := h.call(ctx, session, msgs(userMsg("read a")), readTool)
		finalOf(t, first)
		cancel()
		h.waitLive(0)
	}

	t.Run("after a restart, starts a fresh query that answers the turn", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(answers("cc-resumed", "answer after restart"))
		reply := finalOf(t, h.call(context.Background(), "pi-restarted", msgs(userMsg("read a"), toolCallMsg("toolu_1"), toolResultMsg("toolu_1"))))
		if len(h.spawns()) != 1 {
			t.Fatalf("spawns = %d, the result carries the turn into a fresh query", len(h.spawns()))
		}
		if got := firstPrompt(t, h.lastSpawn()); !strings.Contains(got, "interrupted and resumed") {
			t.Fatalf("prompt = %q", got)
		}
		if replyText(reply) != "answer after restart" {
			t.Fatalf("reply = %q", replyText(reply))
		}
	})

	t.Run("after compaction rewrote the history, tells Claude Code its context was compacted", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.b.markRebuildForSession("pi-compacted", "session_compact:overflow")
		h.script(answers("cc-rebuilt", "continued"))
		finalOf(t, h.call(context.Background(), "pi-compacted", msgs(userMsg("read a"), toolCallMsg("toolu_7"), toolResultMsg("toolu_7"))))
		if len(h.spawns()) != 1 {
			t.Fatalf("spawns = %d", len(h.spawns()))
		}
		if got := firstPrompt(t, h.lastSpawn()); !strings.Contains(got, "context was compacted") {
			t.Fatalf("prompt = %q", got)
		}
		if h.b.historyRewrittenFor("pi-compacted") {
			t.Fatal("the query consumed the session's mark")
		}
	})

	t.Run("after an abort, ends the turn without a query", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		abortParked(t, h, "pi-aborted", "toolu_2", ctx, cancel)

		// pi calls back with the run's context, which the abort left cancelled.
		reply := finalOf(t, h.call(ctx, "pi-aborted", msgs(userMsg("read a"), toolCallMsg("toolu_2"), toolResultMsg("toolu_2"))))
		if len(h.spawns()) != 1 {
			t.Fatalf("spawns = %d, no query starts for the orphaned result", len(h.spawns()))
		}
		if replyText(reply) != "" || reply["stopReason"] != "stop" {
			t.Fatalf("the turn ends empty, as before: %v", reply)
		}
	})

	t.Run("a session aborted earlier still resumes a later retry ending at a tool result", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		abortParked(t, h, "pi-retry", "toolu_5", ctx, cancel)

		// A later run (fresh context) retries after an overflow or quota error, with
		// history ending at a tool result no query is parked on.
		h.script(answers("cc-retry", "resumed"))
		reply := finalOf(t, h.call(context.Background(), "pi-retry", msgs(userMsg("read b"), toolCallMsg("toolu_6"), toolResultMsg("toolu_6"))))
		if len(h.spawns()) != 2 || replyText(reply) != "resumed" {
			t.Fatalf("spawns = %d reply = %q", len(h.spawns()), replyText(reply))
		}
	})

	t.Run("a parked query that fails resumes on pi's retry, even after an earlier abort (#192)", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		abortParked(t, h, "pi-failed", "toolu_8", ctx, cancel)

		// The query parks on a tool call, then fails (a quota error) once its result arrives.
		h.script(parksOnTool("cc-failing", "toolu_9"))
		ask := userMsg("read b")
		parked := finalOf(t, h.call(context.Background(), "pi-failed", msgs(ask), readTool))
		if parked["stopReason"] != "toolUse" {
			t.Fatalf("parked turn = %v", parked["stopReason"])
		}
		failing := h.lastSpawn()
		reply := callTool(context.Background(), spawnMcp{failing}, "read", "toolu_9")
		waitParked(t, h.liveFor("pi-failed"), 1)
		history := msgs(ask, parked, toolResultMsg("toolu_9"))
		failed := h.call(context.Background(), "pi-failed", history, readTool)
		receive(t, reply, "the tool reply")
		failing.client.exit(1, "rate limit (five_hour)")
		if got := finalOf(t, failed); got["stopReason"] != "error" {
			t.Fatalf("failing turn = %v", got["stopReason"])
		}
		h.waitLive(0)

		// pi retries the same history on a fresh context.
		h.script(answers("cc-retry", "answer after retry"))
		retry := finalOf(t, h.call(context.Background(), "pi-failed", history, readTool))
		if len(h.spawns()) != 3 || replyText(retry) != "answer after retry" {
			t.Fatalf("spawns = %d reply = %q", len(h.spawns()), replyText(retry))
		}
	})

	t.Run("an abort in one session does not orphan another session's resumed result", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		abortParked(t, h, "pi-other", "toolu_3", ctx, cancel)

		h.script(answers("cc-mine", "mine"))
		reply := finalOf(t, h.call(context.Background(), "pi-mine", msgs(userMsg("read a"), toolCallMsg("toolu_4"), toolResultMsg("toolu_4"))))
		if len(h.spawns()) != 2 || replyText(reply) != "mine" {
			t.Fatalf("spawns = %d reply = %q", len(h.spawns()), replyText(reply))
		}
	})
}

// --- unit-cross-session-turns.mjs ---

func TestCrossSessionConversationIsolation(t *testing.T) {
	const P, C = "pi-parent", "pi-child"

	t.Run("a foreground child in the parent's first turn cannot claim the parent's conversation", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		u1, a1, tr1, a2, u2 := userMsg("research X"), toolCallMsg("toolu_P1"), toolResultMsg("toolu_P1"), sayMsg("parent answer"), userMsg("now fix it")

		// The parent's first turn stays open until the child below has run, so the
		// child's completion lands inside the parent's turn.
		h.script(parksOnTool("cc-P", "toolu_P1"))
		p1 := h.call(context.Background(), P, msgs(u1))
		parent := h.lastSpawn()
		h.script(answers("cc-C", "child answer"))
		finalOf(t, h.call(context.Background(), C, msgs(userMsg("child task"))))

		parent.client.emit(textReplyFrames("parent answer")...)
		finish(parent, "parent answer")
		finalOf(t, p1)
		h.waitLive(0)

		h.script(answers("cc-P2", "ok"))
		finalOf(t, h.call(context.Background(), P, msgs(u1, a1, tr1, a2, u2)))
		if got := h.lastSpawn().opts.Resume; got != "cc-P" {
			t.Fatalf("the parent's turn 2 resumes %q, want its own conversation cc-P", got)
		}
	})

	t.Run("a background child finishing after the parent leaves the parent's conversation in place", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		u1, a2, u2 := userMsg("research X"), sayMsg("parent answer"), userMsg("now fix it")

		h.script(answers("cc-P", "parent answer"))
		finalOf(t, h.call(context.Background(), P, msgs(u1)))
		if st := h.b.sessionStateFor(P); st == nil || st.SessionID != "cc-P" {
			t.Fatalf("mirror = %+v", st)
		}

		// A background child starts and parks; the parent proceeds without it.
		h.script(func(s *spawn) { s.client.emit(initFrame(s, "cc-C")) })
		child := h.call(context.Background(), C, msgs(userMsg("child task")))
		childProc := h.lastSpawn()

		h.script(answers("cc-P2", "ok"))
		finalOf(t, h.call(context.Background(), P, msgs(u1, a2, u2)))
		if got := h.lastSpawn().opts.Resume; got != "cc-P" {
			t.Fatalf("the parent keeps resuming its own conversation, got %q", got)
		}

		childProc.client.emit(textReplyFrames("child answer")...)
		finish(childProc, "child answer")
		finalOf(t, child)
		h.waitLive(0)

		if st := h.b.sessionStateFor(P); st == nil || st.SessionID != "cc-P" {
			t.Fatalf("the child's completion reassigned the parent's conversation: %+v", st)
		}
		if st := h.b.sessionStateFor(C); st == nil || st.SessionID != "cc-C" || st.PiSessionID != C {
			t.Fatalf("the child keeps its own conversation, tagged with its session: %+v", st)
		}
	})

	// missedSteer fails the live query's prompt stream and delivers a steer to it,
	// modelling the provider's tool-result path counting a steer the CLI never took.
	missedSteer := func(h *providerHarness, session, text string, contextLength int) *QueryContext {
		q := h.liveFor(session)
		q.mu.Lock()
		ps := q.promptStream
		q.mu.Unlock()
		ps.Fail(errors.New("prompt stream closed"))
		h.b.deliverToolResults(context.Background(), h.host, q, nil, []any{map[string]any{"type": "text", "text": text}}, contextLength)
		return q
	}

	t.Run("a missed steer survives successful completion and rebuilds the next turn", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		u1, a1, u2, missed, a2, u3 := userMsg("first"), sayMsg("first answer"), userMsg("second"), userMsg("missed steer"), sayMsg("second answer"), userMsg("third")

		h.script(answers("cc-missed", "first answer"))
		finalOf(t, h.call(context.Background(), P, msgs(u1)))

		h.script(func(s *spawn) { s.client.emit(initFrame(s, "cc-P2")) })
		second := h.call(context.Background(), P, msgs(u1, a1, u2))
		secondProc := h.lastSpawn()
		if secondProc.opts.Resume != "cc-missed" {
			t.Fatalf("resume = %q", secondProc.opts.Resume)
		}

		q := missedSteer(h, P, "missed steer", 4)
		h.b.setCursor(P, 4)
		q.mu.Lock()
		q.latestCursor = 4
		q.mu.Unlock()
		if st := h.b.sessionStateFor(P); !st.NeedsRebuild {
			t.Fatal("the missed steer must mark the mirror")
		}
		secondProc.client.emit(textReplyFrames("second answer")...)
		finish(secondProc, "second answer")
		finalOf(t, second)
		h.waitLive(0)
		if st := h.b.sessionStateFor(P); !st.NeedsRebuild {
			t.Fatal("completion must not forget the missed steer")
		}

		cwd, _ := os.Getwd()
		rebuilt := h.sessionFile("cc-missed", cwd)
		if _, err := os.Stat(rebuilt); err == nil {
			t.Fatal("the fake process has not written a session file")
		}
		h.script(answers("cc-P3", "third answer"))
		finalOf(t, h.call(context.Background(), P, msgs(u1, a1, u2, missed, a2, u3)))
		data, err := os.ReadFile(rebuilt)
		if err != nil {
			t.Fatalf("the next turn imports pi history rather than reusing Claude Code's incomplete session: %v", err)
		}
		if !strings.Contains(string(data), "missed steer") {
			t.Fatal("the rebuilt history must include the undelivered steer")
		}
		h.waitLive(0)
		if st := h.b.sessionStateFor(P); st.NeedsRebuild {
			t.Fatal("the rebuilt turn clears the query's missed-steer mark")
		}
	})

	t.Run("a missed steer in the first turn rebuilds even without an existing mirror", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		const session = "pi-first-miss"
		u1, missed, a1, u2 := userMsg("first"), userMsg("missed first-turn steer"), sayMsg("first answer"), userMsg("second")

		h.script(func(s *spawn) { s.client.emit(initFrame(s, "cc-first-missed")) })
		first := h.call(context.Background(), session, msgs(u1))
		proc := h.lastSpawn()
		if h.b.sessionStateFor(session) != nil {
			t.Fatal("a clean start has no mirror until completion")
		}

		q := missedSteer(h, session, "missed first-turn steer", 2)
		q.mu.Lock()
		q.latestCursor = 2
		q.mu.Unlock()
		proc.client.emit(textReplyFrames("first answer")...)
		finish(proc, "first answer")
		finalOf(t, first)
		h.waitLive(0)
		if st := h.b.sessionStateFor(session); st == nil || !st.NeedsRebuild {
			t.Fatalf("completion records the miss despite having no earlier mirror: %+v", st)
		}

		cwd, _ := os.Getwd()
		rebuilt := h.sessionFile("cc-first-missed", cwd)
		if _, err := os.Stat(rebuilt); err == nil {
			t.Fatal("the fake process has not written a session file")
		}
		h.script(answers("cc-first-missed", "second answer"))
		finalOf(t, h.call(context.Background(), session, msgs(u1, missed, a1, u2)))
		data, err := os.ReadFile(rebuilt)
		if err != nil || !strings.Contains(string(data), "missed first-turn steer") {
			t.Fatalf("rebuilt session = %v, %v", err, string(data))
		}
		h.waitLive(0)
		if st := h.b.sessionStateFor(session); st.NeedsRebuild {
			t.Fatal("the next query starts without the prior miss")
		}
	})

	t.Run("a parent error followed by a child turn does not hand the child the parent's conversation", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		u1, a2, u2 := userMsg("research X"), sayMsg("parent answer"), userMsg("now fix it")

		h.script(answers("cc-P", "parent answer"))
		finalOf(t, h.call(context.Background(), P, msgs(u1)))
		h.script(answers("cc-C", "child answer"))
		finalOf(t, h.call(context.Background(), C, msgs(userMsg("child task"))))
		h.script(answers("cc-P2", "ok"))
		finalOf(t, h.call(context.Background(), P, msgs(u1, a2, u2)))
		if got := h.lastSpawn().opts.Resume; got != "cc-P" {
			t.Fatalf("the parent resumes %q after a child ran while it backed off", got)
		}
	})
}

// --- unit-rate-limit.mjs ---

func TestRateLimitEventsAreReportedOnTheCallThatOwnsTheStream(t *testing.T) {
	h := newProviderHarness(t, func(d *Deps) { d.Debug = true })
	usage := func(u float64) map[string]any {
		return map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
			"status": "allowed", "rateLimitType": "five_hour",
			"unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": u, "resetsAt": 1789770600}},
		}}
	}
	header := func(v string) string { return `"` + rateLimitHeaderPrefix + `-5h-utilization":"` + v + `"` }

	h.script(func(s *spawn) {
		s.client.emit(initFrame(s, "cc-rate-limit"))
		s.client.emit(toolUseFrames("toolu_1")...)
		// Claude Code reports usage after message_stop, once the tool call has ended the stream.
		s.client.emit(usage(0.05), usage(0.1))
	})
	t.Run("reports each event before the next event of the call that owns the stream", func(t *testing.T) {
		first := h.call(context.Background(), "pi-rate-limit", msgs(userMsg("go")), readTool)
		finalOf(t, first)
		proc := h.lastSpawn()
		q := h.liveFor("pi-rate-limit")
		waitFor(t, "the newest event to be held", func() bool {
			q.mu.Lock()
			defer q.mu.Unlock()
			return q.heldRateLimit != nil && q.heldRateLimit.UnifiedWindows["five_hour"].Utilization != nil && *q.heldRateLimit.UnifiedWindows["five_hour"].Utilization == 0.1
		})
		if strings.Contains(readDebugLog(t, h.agentDir), "rate limit: headers") {
			t.Fatal("a call whose stream has ended hears nothing")
		}

		history := msgs(userMsg("go"), toolCallMsg("toolu_1"), toolResultMsg("toolu_1"))
		second := h.call(context.Background(), "pi-rate-limit", history, readTool)
		proc.client.emit(textReplyFrames("done")...)
		proc.client.emit(usage(0.2), resultFrame("done"))
		proc.client.exit(0, "")
		events := eventsOf(t, second)

		log := readDebugLog(t, h.agentDir)
		if strings.Contains(log, header("0.05")) {
			t.Fatalf("a newer event replaces a held one:\n%s", log)
		}
		i1, i2 := strings.Index(log, header("0.1")), strings.Index(log, header("0.2"))
		if i1 < 0 || i2 < i1 || strings.Count(log, "rate limit: headers") != 2 {
			t.Fatalf("the latest held event precedes the next call's first event, then the open stream's own:\n%s", log)
		}
		if got := eventTypes(events); got[len(got)-2] != "text_end" || got[len(got)-1] != "done" {
			t.Fatalf("events = %v", got)
		}
	})
}

// --- unit-finalized-immutable.mjs (the teardown of a turn still streaming) ---

func TestATurnStillStreamingThroughTheProvider(t *testing.T) {
	steerTurn := []any{
		map[string]any{"type": "user", "parent_tool_use_id": nil, "isReplay": true, "uuid": "steer", "message": map[string]any{"role": "user", "content": "steer"}},
		cev(`{"type":"message_start","message":{"id":"msg_steer","usage":{}}}`),
		cev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`),
		successFrame(),
	}
	cases := []struct {
		name       string
		end        func(*spawn, context.CancelFunc)
		stopReason string
	}{
		{"an abort", func(_ *spawn, cancel context.CancelFunc) { cancel() }, "aborted"},
		{"an SDK exception", func(s *spawn, _ context.CancelFunc) { s.client.exit(1, "CLI exited") }, "error"},
		{"an error result", func(s *spawn, _ context.CancelFunc) {
			s.client.emit(errorResultFrame())
			s.client.emit(steerTurn...)
			s.client.exit(0, "")
		}, "error"},
	}
	// streaming parks a query mid-text and waits until its text block is open.
	streaming := func(t *testing.T, h *providerHarness, session string, ctx context.Context, messages []map[string]any) (*sdk.ModelEventStream, *spawn) {
		h.script(func(s *spawn) {
			s.client.emit(initFrame(s, "cc-finalized"))
			s.client.emit(textStartFrames()...)
		})
		stream := h.call(ctx, session, messages, readTool)
		q := h.liveFor(session)
		waitFor(t, "the text block", func() bool {
			q.mu.Lock()
			defer q.mu.Unlock()
			return q.turnOutput != nil && len(list(q.turnOutput["content"])) > 0
		})
		return stream, h.lastSpawn()
	}
	for _, c := range cases {
		t.Run("reports "+c.name, func(t *testing.T) {
			h := newProviderHarness(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, proc := streaming(t, h, "pi-streaming", ctx, msgs(userMsg("go")))
			c.end(proc, cancel)
			reply := finalOf(t, stream)
			h.waitLive(0)
			if reply["stopReason"] != c.stopReason || reply["errorMessage"] == nil || reply["errorMessage"] == "" {
				t.Fatalf("reply = %v", reply)
			}
		})
	}
	t.Run("drops the session mirror at an error result, so a queued steer is rebuilt", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		messages := msgs(userMsg("previous prompt"), sayMsg("previous answer"), userMsg("go"))
		stream, proc := streaming(t, h, "pi-finalized-mirror", context.Background(), messages)
		if h.b.sessionStateFor("pi-finalized-mirror") == nil {
			t.Fatal("prior history created a mirror before the query failed")
		}
		proc.client.emit(errorResultFrame())
		proc.client.emit(steerTurn...)
		proc.client.exit(0, "")
		finalOf(t, stream)
		h.waitLive(0)
		if h.b.sessionStateFor("pi-finalized-mirror") != nil {
			t.Fatal("the mirror must be dropped")
		}
	})
}

// --- unit-session-cwd.mjs ---

func TestStreamSimpleRunsClaudeCodeInTheCwdRegisteredAtSessionStart(t *testing.T) {
	h := newProviderHarness(t, nil)
	cwd := t.TempDir()
	h.host.cwd, h.host.sessionID = cwd, "s-web"
	h.b.OnSessionStart(h.host, "startup")
	h.script(answers("cc-web", "ok"))
	finalOf(t, h.call(context.Background(), "s-web", msgs(userMsg("hi"))))
	if got := h.lastSpawn().opts.Cwd; got != cwd {
		t.Fatalf("cwd = %q, want %q", got, cwd)
	}
}

// --- the provider entry itself ---

func TestStreamSimpleFailuresEndTheTurnOnTheStream(t *testing.T) {
	t.Run("a missing Claude Code binary notifies and fails the turn cleanly", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) {
			missing := filepath.Join(t.TempDir(), "no-such-claude")
			d.Config.Provider.PathToClaudeCodeExecutable = &missing
		})
		reply := finalOf(t, h.call(context.Background(), "pi-missing", msgs(userMsg("hi"))))
		if reply["stopReason"] != "error" || !strings.Contains(str(reply["errorMessage"]), "no-such-claude") {
			t.Fatalf("reply = %v", reply)
		}
		if n := h.host.noticeList(); len(n) != 1 || !strings.HasPrefix(n[0], "error: ") {
			t.Fatalf("notices = %v", n)
		}
		if len(h.spawns()) != 0 || len(h.b.liveContexts()) != 0 {
			t.Fatal("nothing may start or stay claimed")
		}
	})

	t.Run("no claude on PATH is reported the same way", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) { d.Config.Provider.PathToClaudeCodeExecutable = nil })
		t.Setenv("PATH", t.TempDir())
		reply := finalOf(t, h.call(context.Background(), "pi-nopath", msgs(userMsg("hi"))))
		if reply["stopReason"] != "error" || !strings.Contains(str(reply["errorMessage"]), "claude executable not found on PATH") {
			t.Fatalf("reply = %v", reply)
		}
	})

	t.Run("an older binary gives one notice, once per process, and the query continues", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		old := writeStubClaude(t, "2.1.100")
		h.b.deps.Config.Provider.PathToClaudeCodeExecutable = &old
		h.script(answers("cc-old-1", "one"))
		finalOf(t, h.call(context.Background(), "pi-old", msgs(userMsg("hi"))))
		h.script(answers("cc-old-2", "two"))
		finalOf(t, h.call(context.Background(), "pi-old", msgs(userMsg("hi"), sayMsg("one"), userMsg("again"))))
		n := h.host.noticeList()
		if len(n) != 1 || !strings.HasPrefix(n[0], "warning: Claude Code 2.1.100 is older than the supported "+ccproto.MinVersion) {
			t.Fatalf("notices = %v", n)
		}
		if len(h.spawns()) != 2 || h.spawns()[0].opts.Executable != old {
			t.Fatalf("spawns = %d", len(h.spawns()))
		}
	})

	t.Run("a Spawn failure fails the turn and leaves nothing claimed", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) {
			d.Spawn = func(context.Context, ccproto.Options, ccproto.Handlers) (ccproto.Client, error) {
				return nil, errors.New("exec format error")
			}
		})
		reply := finalOf(t, h.call(context.Background(), "pi-spawn", msgs(userMsg("hi"))))
		if reply["stopReason"] != "error" || !strings.Contains(str(reply["errorMessage"]), "exec format error") || len(h.b.liveContexts()) != 0 {
			t.Fatalf("reply = %v", reply)
		}
	})

	t.Run("a conversion error becomes the turn's error event", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		history := msgs(userMsg("one"), assistantMsg([]any{map[string]any{"type": "toolCall", "id": "t1", "name": "mcp__custom-tools__read", "arguments": map[string]any{}}}, "toolUse"), toolResultMsg("t1"), userMsg("two"))
		reply := finalOf(t, h.call(context.Background(), "pi-convert", history))
		if reply["stopReason"] != "error" || len(h.spawns()) != 0 {
			t.Fatalf("reply = %v", reply)
		}
	})

	t.Run("a panic in bridge code becomes one error event", func(t *testing.T) {
		h := newProviderHarness(t, func(d *Deps) { d.Config.StartupNoticeShown = ""; d.Config.Provider.Plan = nil })
		stream, err := h.b.StreamSimple(context.Background(), panickingHost{h.host}, testModel, map[string]any{"messages": msgs(userMsg("hi"))}, nil)
		if err != nil {
			t.Fatal(err)
		}
		events := eventsOf(t, stream)
		if len(events) != 1 || events[0]["type"] != "error" || !strings.Contains(str(obj(events[0]["error"])["errorMessage"]), "claude bridge internal error: host exploded") {
			t.Fatalf("events = %v", events)
		}
	})

	t.Run("a cacheRetention none call is routed to the isolated query", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(replies("the summary"))
		stream, _ := h.b.StreamSimple(context.Background(), h.host, testModel, map[string]any{"messages": msgs(userMsg("summarize"))}, map[string]any{"cacheRetention": "none"})
		reply := finalOf(t, stream)
		if content, _ := reply["content"].([]any); reply["stopReason"] != "stop" || len(content) != 1 || obj(content[0])["text"] != "the summary" {
			t.Fatalf("reply = %v", reply)
		}
		if spawns := h.spawns(); len(spawns) != 1 || !spawns[0].opts.NoSessionPersistence {
			t.Fatalf("spawns = %v", spawns)
		}
	})
}

// panickingHost fails when the bridge notifies.
type panickingHost struct{ *fakeHost }

func (panickingHost) Notify(string, string) { panic("host exploded") }

// --- unit-transcript-replay-order.mjs (the provider-level part) ---

func TestTranscriptContextResolvesAgainstTheRecordedPrompt(t *testing.T) {
	transcript := func(sections map[string]any) []map[string]any {
		return msgs(
			map[string]any{"role": "system", "content": "", "timestamp": 0, "toolsAdded": []any{map[string]any{"name": "read", "description": "", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}},
			map[string]any{"role": "system", "content": "", "timestamp": 0, "sections": sections},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "go"}}, "timestamp": 0},
		)
	}
	sections := map[string]any{"preamble": "P", "tools": "T", "cwd": "/w"}

	t.Run("the replayed prompt is the capture key, so the turn starts", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.host.setPrompt("P\n\nT\n\n/w")
		h.b.OnAgentStart(h.host)
		h.script(answers("cc-transcript", "ok"))
		stream, _ := h.b.StreamSimple(context.Background(), h.host, testModel, map[string]any{"messages": transcript(sections)}, map[string]any{"sessionId": "pi-transcript"})
		reply := finalOf(t, stream)
		if len(h.spawns()) != 1 {
			t.Fatalf("spawns = %d, reply = %v", len(h.spawns()), reply)
		}
		if got := firstPrompt(t, h.lastSpawn()); got != "go" {
			t.Fatalf("prompt = %q", got)
		}
		if len(h.lastSpawn().opts.SDKMcpServers) != 1 {
			t.Fatal("the transcript's tool set must reach the MCP server")
		}
	})

	t.Run("a prompt no boundary recorded fails the turn", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.host.setPrompt("P\n\n/w")
		h.b.OnAgentStart(h.host)
		stream, _ := h.b.StreamSimple(context.Background(), h.host, testModel, map[string]any{"messages": transcript(sections)}, map[string]any{"sessionId": "pi-transcript"})
		reply := finalOf(t, stream)
		if reply["stopReason"] != "error" || !strings.Contains(str(reply["errorMessage"]), "prompt-capture: no capture") || len(h.spawns()) != 0 {
			t.Fatalf("reply = %v", reply)
		}
	})
}
