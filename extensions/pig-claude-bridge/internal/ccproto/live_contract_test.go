//go:build live

package ccproto

// Executable contracts for the undocumented Claude Code behaviour the bridge depends on, asserted against
// the installed `claude` binary. Port of pi-claude-bridge tests/int-cc-contracts.mjs (commit 6aa276a), plus
// the S2 contracts. They use the real Claude Code login (the real ~/.claude), spend a little quota on
// claude-haiku-4-5, and never run in CI: run them by hand on every Claude Code bump (docs/verification.md).
//
// Each test runs the child in its own scratch directory; the only part of ~/.claude touched is that
// directory's project hash directory (session files), which is removed again afterwards.
//
// Not covered, as in the TypeScript file: DISABLE_AUTO_COMPACT (needs a near-full context), steer draining
// at tool boundaries, rate_limit_event shape. Not expressible through the client: `updatedInput` on allow
// (the client answers can_use_tool with the SDK's "not provided" error, so the CLI never waits on a decision).

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
)

const (
	liveModel     = "claude-haiku-4-5"
	toolUseIDMeta = "claudecode/toolUseId"
	liveTimeout   = 120 * time.Second
)

// --- harness ---

func claudeOnPath(t *testing.T) string {
	t.Helper()
	exe, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH; live contract tests need the installed Claude Code")
	}
	return exe
}

// scratchDir returns a fresh symlink-resolved directory and removes its project hash directory afterwards.
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		hashDir := ccsession.ProjectDir("", dir)
		if filepath.Base(hashDir) == ccsession.ProjectPathToHash(dir) {
			os.RemoveAll(hashDir)
		}
	})
	return dir
}

// providerOptions mirrors the TypeScript providerOptions: no builtin tools, bypass permissions, strict MCP.
func providerOptions(t *testing.T) Options {
	return Options{
		Executable: claudeOnPath(t), Cwd: scratchDir(t), Model: liveModel, MaxTurns: 6,
		ToolsSet: true, PermissionMode: "bypassPermissions", StrictMcpConfig: true,
	}
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func noArgTool(name string) mcpTool {
	return mcpTool{name, "Returns the " + name + " value.", json.RawMessage(`{"type":"object","properties":{}}`)}
}

type toolCall struct {
	Name string
	Meta map[string]any
	Args json.RawMessage
}

// bareServer is the TypeScript toolServer: a bare MCP server that records what the CLI sends it. It is not
// internal/mcptools: these tests pin the CLI, not our server.
type bareServer struct {
	tools []mcpTool
	mu    sync.Mutex
	calls []toolCall
	// methods lists every JSON-RPC method the CLI sent, in order; initParams is the params of its initialize.
	methods    []string
	initParams map[string]any
}

func (s *bareServer) handle(_ context.Context, _ string, message json.RawMessage) (json.RawMessage, bool, error) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(message, &req); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	s.methods = append(s.methods, req.Method)
	s.mu.Unlock()
	var result any
	switch req.Method {
	case "initialize":
		var params map[string]any
		_ = json.Unmarshal(req.Params, &params)
		s.mu.Lock()
		s.initParams = params
		s.mu.Unlock()
		result = map[string]any{"protocolVersion": params["protocolVersion"], "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "custom-tools", "version": "1.0.0"}}
	case "tools/list":
		result = map[string]any{"tools": s.tools}
	case "tools/call":
		var params struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"arguments"`
			Meta map[string]any  `json:"_meta"`
		}
		_ = json.Unmarshal(req.Params, &params)
		s.mu.Lock()
		s.calls = append(s.calls, toolCall{params.Name, params.Meta, params.Args})
		s.mu.Unlock()
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": params.Name + "-VALUE"}}}
	case "ping":
		result = map[string]any{}
	default:
		if req.ID == nil {
			return nil, false, nil
		}
		return mustJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}}), true, nil
	}
	if req.ID == nil {
		return nil, false, nil
	}
	return mustJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}), true, nil
}

func (s *bareServer) callList() []toolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]toolCall(nil), s.calls...)
}

func mustJSON(v any) json.RawMessage {
	b, err := marshalFrame(v)
	if err != nil {
		panic(err)
	}
	return b
}

// startLive starts the real child; srv may be nil.
func startLive(t *testing.T, o Options, srv *bareServer) *Process {
	t.Helper()
	var h Handlers
	if srv != nil {
		o.SDKMcpServers = []string{"custom-tools"}
		h.OnMcpMessage = srv.handle
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := Start(ctx, o, h)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		p.Close(ctx)
	})
	return p
}

type toolUse struct{ ID, Name string }

// observed is everything a run emitted, in order.
type observed struct {
	frames      []map[string]any
	init        map[string]any
	result      map[string]any
	toolUses    []toolUse
	toolResults []map[string]any
	assistants  []map[string]any
	events      []map[string]any
	closed      bool // stdout ended before stop was satisfied
}

// at walks nested maps (string keys) and slices (int keys); nil when absent.
func at(v any, keys ...any) any {
	for _, k := range keys {
		switch c := v.(type) {
		case map[string]any:
			s, _ := k.(string)
			v = c[s]
		case []any:
			i, ok := k.(int)
			if !ok || i < 0 || i >= len(c) {
				return nil
			}
			v = c[i]
		default:
			return nil
		}
	}
	return v
}

func str(v any) string { s, _ := v.(string); return s }

func isResult(f map[string]any) bool { return f["type"] == "result" }
func isInit(f map[string]any) bool   { return f["type"] == "system" && f["subtype"] == "init" }

// collect reads frames until stop accepts one (nil stop means the result frame) or stdout ends.
func collect(t *testing.T, p *Process, stop func(map[string]any) bool) *observed {
	t.Helper()
	if stop == nil {
		stop = isResult
	}
	o := &observed{}
	timeout := time.After(liveTimeout)
	for {
		select {
		case raw, ok := <-p.Frames():
			if !ok {
				o.closed = true
				err, _ := boundedWait(p)
				t.Logf("stdout ended before the awaited frame; exit: %v; stderr tail: %s", err, p.StderrTail())
				return o
			}
			var f map[string]any
			if json.Unmarshal(raw, &f) != nil {
				continue
			}
			o.frames = append(o.frames, f)
			switch f["type"] {
			case "system":
				if isInit(f) {
					o.init = f
				}
			case "assistant":
				o.assistants = append(o.assistants, f)
				for _, b := range at(f, "message", "content").([]any) {
					if at(b, "type") == "tool_use" {
						o.toolUses = append(o.toolUses, toolUse{str(at(b, "id")), str(at(b, "name"))})
					}
				}
			case "user":
				blocks, _ := at(f, "message", "content").([]any)
				for _, b := range blocks {
					if m, ok := b.(map[string]any); ok && m["type"] == "tool_result" {
						o.toolResults = append(o.toolResults, m)
					}
				}
			case "stream_event":
				if e, ok := f["event"].(map[string]any); ok {
					o.events = append(o.events, e)
				}
			case "result":
				o.result = f
			}
			if stop(f) {
				return o
			}
		case <-timeout:
			t.Fatalf("timed out after %s waiting for a frame", liveTimeout)
		}
	}
}

// run writes one prompt and collects through the result frame.
func run(t *testing.T, p *Process, prompt string) *observed {
	t.Helper()
	if err := p.WriteUser(UserMessage{Content: prompt}); err != nil {
		t.Fatal(err)
	}
	return collect(t, p, nil)
}

// initOnly runs only far enough to read system/init.
func initOnly(t *testing.T, p *Process) map[string]any {
	t.Helper()
	if err := p.WriteUser(UserMessage{Content: "Say OK."}); err != nil {
		t.Fatal(err)
	}
	o := collect(t, p, isInit)
	if o.init == nil {
		t.Fatal("no system/init message arrived")
	}
	return o.init
}

// rawInit spawns the child without the client so ENABLE_CLAUDEAI_MCP_SERVERS can be left unset, which
// BuildEnv does not allow. cloudVar "" leaves it unset.
func rawInit(t *testing.T, o Options, cloudVar string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.Executable, BuildArgs(o)...)
	cmd.Dir = o.Cwd
	for _, kv := range BuildEnv(os.Environ(), o) {
		if !strings.HasPrefix(kv, "ENABLE_CLAUDEAI_MCP_SERVERS=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	if cloudVar != "" {
		cmd.Env = append(cmd.Env, "ENABLE_CLAUDEAI_MCP_SERVERS="+cloudVar)
	}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	user, _ := marshalFrame(UserMessage{Content: "Say OK."})
	fmt.Fprintf(stdin, "%s\n%s\n", BuildInitialize(o, "init1"), user)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var f map[string]any
		if json.Unmarshal(sc.Bytes(), &f) == nil && isInit(f) {
			return f
		}
	}
	t.Fatal("no system/init message arrived")
	return nil
}

func newUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func assistantText(o *observed) string {
	var sb strings.Builder
	for _, a := range o.assistants {
		for _, b := range at(a, "message", "content").([]any) {
			if at(b, "type") == "text" {
				sb.WriteString(str(at(b, "text")))
			}
		}
	}
	return sb.String()
}

const parallelPrompt = "Call both the alpha tool and the beta tool at the same time, in a single assistant message with two parallel tool calls. Then report both values."

// --- the MCP tool-call pairing scheme ---

func TestToolsCallCarriesToolUseIdMeta(t *testing.T) {
	// Parallel calls are the case that matters: the id must be the call's own tool_use id, not the first
	// block's, or every result is mispaired silently.
	srv := &bareServer{tools: []mcpTool{noArgTool("alpha"), noArgTool("beta")}}
	o := run(t, startLive(t, providerOptions(t), srv), parallelPrompt)
	calls := srv.callList()
	if len(calls) < 1 {
		t.Fatal("CC dispatched no tool call; the test proved nothing")
	}
	for _, call := range calls {
		id, ok := call.Meta[toolUseIDMeta].(string)
		if !ok {
			t.Fatalf("tools/call for %s has no _meta[%q]: %v", call.Name, toolUseIDMeta, call.Meta)
		}
		var match *toolUse
		for i := range o.toolUses {
			if o.toolUses[i].ID == id {
				match = &o.toolUses[i]
			}
		}
		if match == nil {
			t.Fatalf("_meta id %s matches no tool_use CC emitted: %v", id, o.toolUses)
		}
		if match.Name != "mcp__custom-tools__"+call.Name {
			t.Errorf("tools/call %s was stamped with the id of %s; results would be mispaired", call.Name, match.Name)
		}
	}
}

func TestToolsCallNamesTheBareToolNotTheMcpAlias(t *testing.T) {
	srv := &bareServer{tools: []mcpTool{noArgTool("alpha")}}
	run(t, startLive(t, providerOptions(t), srv), "Call the alpha tool once, then stop.")
	var names []string
	for _, c := range srv.callList() {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"alpha"}) {
		t.Fatalf("tools/call names = %v, want [alpha]", names)
	}
}

// --- `tools: []` and the unserved-tool premise ---

func TestToolsEmptyExposesNoBuiltinTools(t *testing.T) {
	srv := &bareServer{tools: []mcpTool{noArgTool("alpha")}}
	init := initOnly(t, startLive(t, providerOptions(t), srv))
	if got := fmt.Sprint(init["tools"]); got != "[mcp__custom-tools__alpha]" {
		t.Fatalf("CC exposed tools beyond our MCP server: %v", init["tools"])
	}
}

func TestUnservedToolUseIsAnsweredByCCNeverDispatched(t *testing.T) {
	// Whether the model takes the bait is up to the model: "never dispatched" is asserted unconditionally,
	// the rejection shape only when the bait was taken.
	srv := &bareServer{tools: []mcpTool{{"bash", "Run a shell command.", json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}}}
	o := run(t, startLive(t, providerOptions(t), srv), "You have a tool registered under the plain name `Bash`. Invoke `Bash` (exactly that name) with command `echo hi`. Do not use any mcp__ prefixed name. Try it even if you doubt it exists.")
	dispatched := map[string]bool{}
	for _, c := range srv.callList() {
		if c.Name != "bash" {
			t.Errorf("CC dispatched a tool we do not serve: %s", c.Name)
		}
		id, _ := c.Meta[toolUseIDMeta].(string)
		dispatched[id] = true
	}
	var bogus []toolUse
	for _, u := range o.toolUses {
		if !strings.HasPrefix(u.Name, "mcp__custom-tools__") {
			bogus = append(bogus, u)
		}
	}
	if len(bogus) == 0 {
		t.Log("model declined the bait; nothing more to pin")
		return
	}
	for _, u := range bogus {
		var rejection map[string]any
		for _, r := range o.toolResults {
			if r["tool_use_id"] == u.ID {
				rejection = r
			}
		}
		if rejection == nil || rejection["is_error"] != true || !strings.Contains(fmt.Sprint(rejection["content"]), "No such tool available") {
			t.Errorf("CC did not self-answer the unserved tool_use %s [%s]: %v", u.Name, u.ID, rejection)
		}
		if dispatched[u.ID] {
			t.Errorf("rejected tool_use %s was also dispatched", u.ID)
		}
	}
}

func TestDisallowedToolsRemovesBash(t *testing.T) {
	// Pins the --disallowedTools= form AskClaude uses: with Bash disallowed and every builtin tool enabled
	// by default, a request for a shell command must not produce a Bash tool use.
	o := providerOptions(t)
	o.ToolsSet, o.Tools = false, nil
	o.DisallowedTools = []string{"Bash"}
	p := startLive(t, o, nil)
	obs := run(t, p, "Run the shell command `echo hi` with the Bash tool and tell me its output.")
	if obs.result == nil {
		t.Fatal("no result frame")
	}
	for _, u := range obs.toolUses {
		if u.Name == "Bash" {
			t.Fatalf("Bash was used despite --disallowedTools=Bash: %v", obs.toolUses)
		}
	}
	tools, _ := obs.init["tools"].([]any)
	if !slicesContain(tools, "Read") { // positive control: the builtin set is listed at all
		t.Fatalf("init.tools lacks the builtin Read, so the Bash check proves nothing: %v", tools)
	}
	if slicesContain(tools, "Bash") {
		t.Fatalf("init.tools still lists Bash: %v", tools)
	}
}

// boundedWait waits up to 15s for the child to exit; exited is false on timeout.
func boundedWait(p *Process) (err error, exited bool) {
	select {
	case <-p.Done():
		return p.Wait(), true
	case <-time.After(15 * time.Second):
		return nil, false
	}
}

func slicesContain(list []any, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- result shapes ---

func TestIsErrorCanBeTrueOnAResultWhoseSubtypeIsStillSuccess(t *testing.T) {
	// CC reports API-level failures (prompt-too-long here) as a success-shaped result with is_error, after a
	// <synthetic> assistant message carrying the same text and with no content stream_events before it.
	o := providerOptions(t)
	o.MaxTurns, o.NoSessionPersistence, o.IncludePartialMessages = 1, true, true
	p := startLive(t, o, nil)
	obs := run(t, p, "Summarize this in one word:\n"+strings.Repeat("banana ", 220_000))

	if obs.result == nil {
		t.Fatal("no result message arrived")
	}
	if obs.result["subtype"] != "success" {
		t.Errorf("expected a success-shaped failure, got subtype=%v", obs.result["subtype"])
	}
	if obs.result["is_error"] != true {
		t.Errorf("is_error = %v, want true", obs.result["is_error"])
	}
	tooLong := regexp.MustCompile(`(?i)too long`)
	if !tooLong.MatchString(str(obs.result["result"])) {
		t.Errorf("expected the API's own text in result.result, got: %v", obs.result["result"])
	}
	// The TypeScript SDK then rejects its generator with "too long"; the client analogue is the exit status:
	// the CLI exits non-zero without repeating the text.
	_ = p.EndInput()
	exitErr, exited := boundedWait(p)
	switch {
	case !exited:
		t.Error("the child did not exit within 15s of stdin ending")
	case exitErr == nil:
		t.Error("the child exited cleanly after the failure result; the recorded non-zero exit changed")
	case tooLong.MatchString(exitErr.Error()) || tooLong.MatchString(p.StderrTail()):
		t.Errorf("the exit now reports the failure text: %v; stderr: %s", exitErr, p.StderrTail())
	}

	if len(obs.assistants) == 0 {
		t.Fatal("no assistant message preceded the failure result")
	}
	for _, e := range obs.events {
		if typ := str(e["type"]); typ != "message_start" && typ != "ping" {
			t.Errorf("stream_event %q preceded the failure", typ)
		}
	}
	for _, a := range obs.assistants {
		if m := str(at(a, "message", "model")); m != "<synthetic>" {
			t.Errorf("failure report is not synthetic: model=%s", m)
		}
		found := false
		for _, b := range at(a, "message", "content").([]any) {
			found = found || at(b, "type") == "text" && tooLong.MatchString(str(at(b, "text")))
		}
		if !found {
			t.Errorf("synthetic message lost the failure text: %v", at(a, "message", "content"))
		}
	}
}

func TestResultModelUsageReportsTheServedContextWindow(t *testing.T) {
	o := providerOptions(t)
	o.MaxTurns, o.NoSessionPersistence = 1, true
	obs := run(t, startLive(t, o, nil), "Reply with just: OK")
	usage, _ := at(obs.result, "modelUsage").(map[string]any)
	if len(usage) == 0 {
		t.Fatalf("result carried no modelUsage: %v", obs.result)
	}
	for model, u := range usage {
		if _, ok := at(u, "contextWindow").(float64); !ok {
			t.Errorf("modelUsage[%s] missing contextWindow: %v", model, u)
		}
	}
}

// --- streaming ---

func TestIncludePartialMessagesYieldsTheStreamEventShapesProcessStreamEventDestructures(t *testing.T) {
	o := providerOptions(t)
	o.IncludePartialMessages, o.Effort = true, "medium"
	srv := &bareServer{tools: []mcpTool{noArgTool("alpha")}}
	obs := run(t, startLive(t, o, srv), "Think briefly about what 17 * 23 is, then call the alpha tool once, then state the number.")
	events, blocks, deltas := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range obs.events {
		events[str(e["type"])] = true
		if s := str(at(e, "content_block", "type")); s != "" {
			blocks[s] = true
		}
		if s := str(at(e, "delta", "type")); s != "" {
			deltas[s] = true
		}
	}
	for _, want := range []string{"message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop"} {
		if !events[want] {
			t.Errorf("no %s stream_event arrived; got %v", want, events)
		}
	}
	for _, want := range []string{"text", "thinking", "tool_use"} {
		if !blocks[want] {
			t.Errorf("no %s content_block; got %v", want, blocks)
		}
	}
	for _, want := range []string{"text_delta", "thinking_delta", "signature_delta", "input_json_delta"} {
		if !deltas[want] {
			t.Errorf("no %s delta; got %v", want, deltas)
		}
	}
}

func TestMessageDeltaUsageCarriesThinkingTokensNestedUnderOutputTokensDetails(t *testing.T) {
	// The bridge maps this onto pi's usage.reasoning. The nesting is the whole contract: a flat
	// thinking_tokens, or a reasoning_tokens, would make the bridge report nothing on every thinking turn.
	o := providerOptions(t)
	o.IncludePartialMessages, o.Effort, o.MaxTurns, o.NoSessionPersistence = true, "medium", 1, true
	obs := run(t, startLive(t, o, nil), "Think briefly about what 17 * 23 is, then state just the number.")
	var usages []map[string]any
	for _, e := range obs.events {
		if e["type"] == "message_delta" {
			if u, ok := e["usage"].(map[string]any); ok {
				usages = append(usages, u)
			}
		}
	}
	if len(usages) == 0 {
		t.Fatal("no message_delta carried usage")
	}
	nested := 0
	for _, u := range usages {
		thinking, ok := at(u, "output_tokens_details", "thinking_tokens").(float64)
		if !ok {
			continue
		}
		nested++
		if thinking <= 0 {
			t.Errorf("a thinking turn must report a positive count: %v", u)
		}
		if out, _ := u["output_tokens"].(float64); thinking > out {
			t.Errorf("thinking_tokens must stay a subset of output_tokens: %v", u)
		}
		if _, flat := u["thinking_tokens"]; flat {
			t.Errorf("a flat thinking_tokens would mean the shape changed: %v", u)
		}
		if _, never := u["reasoning_tokens"]; never {
			t.Errorf("reasoning_tokens has never existed; a value means the shape changed: %v", u)
		}
	}
	if nested == 0 {
		t.Fatalf("no message_delta reported output_tokens_details.thinking_tokens: %v", usages)
	}
}

func TestAStreamedPromptKeepsTheQueryOpenPastResultUntilInputEnds(t *testing.T) {
	// The CLI does not exit on its own after result while stdin is open: that parked process is what lets the
	// bridge write a steer mid-turn, and why it must end input itself or the child never terminates.
	o := providerOptions(t)
	o.MaxTurns, o.NoSessionPersistence = 1, true
	p := startLive(t, o, nil)
	if obs := run(t, p, "Reply with just: OK"); obs.result == nil {
		t.Fatal("no result message arrived")
	}
	select {
	case <-p.Done():
		t.Fatalf("the child exited on its own after result with stdin open: %v", p.Wait())
	case <-time.After(5 * time.Second):
	}
	if err := p.EndInput(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the child did not exit after stdin ended")
	}
}

// --- the in-process MCP server ---

func TestMcpMessageMethodsStayWithinTheServedSet(t *testing.T) {
	// Go analogue of "the SDK treats our McpServer as an opaque endpoint": the Node SDK half has no
	// counterpart here, but the server may be minimal only while the CLI sends nothing beyond these methods.
	srv := &bareServer{tools: []mcpTool{noArgTool("alpha")}}
	run(t, startLive(t, providerOptions(t), srv), "Call the alpha tool once, then stop.")
	if len(srv.callList()) == 0 {
		t.Fatal("no tool call happened; the server was never exercised")
	}
	allowed := map[string]bool{"initialize": true, "notifications/initialized": true, "tools/list": true, "tools/call": true}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, m := range srv.methods {
		if !allowed[m] {
			t.Errorf("the CLI now sends mcp_message method %q beyond the served set; all: %v", m, srv.methods)
		}
	}
}

func TestInitializeResultAcceptedByCLI(t *testing.T) {
	// S2: the CLI sends the MCP initialize before it answers our control initialize; the reply echoing
	// protocolVersion with capabilities {"tools":{}} is accepted, so the tool appears in init and is callable.
	srv := &bareServer{tools: []mcpTool{noArgTool("alpha")}}
	o := run(t, startLive(t, providerOptions(t), srv), "Call the alpha tool once, then stop.")
	srv.mu.Lock()
	params := srv.initParams
	srv.mu.Unlock()
	if params == nil {
		t.Fatal("the CLI never sent an MCP initialize")
	}
	if params["protocolVersion"] != "2025-11-25" || at(params, "clientInfo", "name") != "claude-code" {
		t.Errorf("MCP initialize params changed: %v", params)
	}
	t.Logf("MCP initialize params: %v", params)
	if !slicesContain(at(o.init, "tools").([]any), "mcp__custom-tools__alpha") {
		t.Errorf("init.tools lacks the served tool: %v", o.init["tools"])
	}
	for _, s := range at(o.init, "mcp_servers").([]any) {
		if at(s, "name") == "custom-tools" && at(s, "status") != "connected" {
			t.Errorf("custom-tools status = %v, want connected", at(s, "status"))
		}
	}
	if len(srv.callList()) == 0 {
		t.Error("the accepted server was never called")
	}
}

func TestJSONSchemaIsServedVerbatimNestedObjectsAndAnyOfConstSurvive(t *testing.T) {
	schema := `{"type":"object","properties":{"edit":{"type":"object","properties":{"path":{"type":"string"},"replacement":{"type":"object","properties":{"from":{"type":"string"},"to":{"type":"string"}},"required":["from","to"]}},"required":["path","replacement"]},"kind":{"anyOf":[{"const":"fast"},{"const":"slow"}]}},"required":["edit"]}`
	srv := &bareServer{tools: []mcpTool{{"apply_edit", "Apply a structured edit.", json.RawMessage(schema)}}}
	run(t, startLive(t, providerOptions(t), srv), "Call apply_edit with edit.path='a.txt', edit.replacement.from='X', edit.replacement.to='Y', kind='fast'.")
	calls := srv.callList()
	if len(calls) != 1 {
		t.Fatalf("expected one apply_edit call, got %d", len(calls))
	}
	var got, want any
	_ = json.Unmarshal(calls[0].Args, &got)
	_ = json.Unmarshal([]byte(`{"edit":{"path":"a.txt","replacement":{"from":"X","to":"Y"}},"kind":"fast"}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the nested schema did not reach the model intact: %s", calls[0].Args)
	}
}

// --- session transcripts and --resume ---

func resumeOptions(t *testing.T, cwd, sessionID string) Options {
	o := providerOptions(t)
	o.Cwd, o.Resume, o.MaxTurns = cwd, sessionID, 2
	return o
}

func TestResumeRereadsTheJSONLFromDiskOnEveryCall(t *testing.T) {
	// syncSharedSession's rebuild path overwrites the session file in place under the same UUID; that is
	// only correct if CC caches nothing by UUID.
	cwd, id := scratchDir(t), newUUID(t)
	seed := func(token string) {
		s, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: liveModel, SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddUserMessage("Please remember: the token is " + token + "."); err != nil {
			t.Fatal(err)
		}
		if err := s.AddAssistantMessage([]ccsession.Block{ccsession.TextBlock{Type: "text", Text: "Got it, the token is " + token + "."}}, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	ask := func() string {
		o := resumeOptions(t, cwd, id)
		o.ToolsSet, o.Tools = true, nil
		return assistantText(run(t, startLive(t, o, nil), "What token did I ask you to remember? Reply with just the word."))
	}
	seed("FOXGLOVE")
	if first := ask(); !strings.Contains(strings.ToLower(first), "foxglove") {
		t.Fatalf("first resume did not see FOXGLOVE: %q", first)
	}
	seed("SANDPIPER")
	second := strings.ToLower(ask())
	if !strings.Contains(second, "sandpiper") {
		t.Errorf("rewrite between resumes was ignored: %q", second)
	}
	if strings.Contains(second, "foxglove") {
		t.Errorf("CC served stale content for the same UUID: %q", second)
	}
}

func TestArbitrarySanitizedToolUseIdsInAnImportedTranscriptResumeFine(t *testing.T) {
	// convert.SanitizeToolID only strips characters outside [A-Za-z0-9_-]; the result keeps pi's id shape
	// rather than CC's toolu_* form.
	cwd, id := scratchDir(t), newUUID(t)
	s, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: liveModel, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	err = s.ImportMessages([]ccsession.Message{
		{Role: "user", Content: "Read the vault file."},
		{Role: "assistant", Content: []ccsession.Block{ccsession.ToolUseBlock{Type: "tool_use", ID: "pi_call_0__weird", Name: "Read", Input: map[string]any{"file_path": "/tmp/vault.txt"}}}},
		{Role: "user", Content: []ccsession.Block{ccsession.ToolResultBlock{Type: "tool_result", ToolUseID: "pi_call_0__weird", Content: "The vault code is PLATYPUS."}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	o := resumeOptions(t, cwd, id)
	answer := assistantText(run(t, startLive(t, o, nil), "What was the vault code from the file you read? One word."))
	if !strings.Contains(strings.ToLower(answer), "platypus") {
		t.Fatalf("CC did not resume a transcript with non-toolu_ ids: %q", answer)
	}
}

func TestCCWritesEachToolResultOfAParallelTurnAsItsOwnTranscriptRecord(t *testing.T) {
	// Corrects the premise recorded in convert: CC's own transcript is one record per result, so the
	// single-message shape ConvertPiMessages emits is a RepairToolPairing requirement, not a copy of CC.
	o := providerOptions(t)
	srv := &bareServer{tools: []mcpTool{noArgTool("alpha"), noArgTool("beta")}}
	obs := run(t, startLive(t, o, srv), parallelPrompt)
	if n := len(srv.callList()); n != 2 {
		t.Fatalf("expected two parallel tool calls, got %d", n)
	}
	data, err := os.ReadFile(ccsession.SessionPath("", o.Cwd, str(obs.init["session_id"])))
	if err != nil {
		t.Fatal(err)
	}
	var perRecord []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["type"] != "user" {
			continue
		}
		blocks, _ := at(rec, "message", "content").([]any)
		n := 0
		for _, b := range blocks {
			if at(b, "type") == "tool_result" {
				n++
			}
		}
		if n > 0 {
			perRecord = append(perRecord, n)
		}
	}
	if !reflect.DeepEqual(perRecord, []int{1, 1}) {
		t.Fatalf("CC's transcript layout for parallel results changed: %v", perRecord)
	}
}

func TestRepairToolPairingKeepsEveryResultOnlyWhenTheyShareOneUserMessage(t *testing.T) {
	claudeOnPath(t) // part of the live suite; needs no child itself
	ids := []string{"t1", "t2", "t3"}
	var uses []ccsession.Block
	var combined, results []ccsession.Block
	split := []ccsession.Message{{Role: "user", Content: "go"}}
	for _, id := range ids {
		uses = append(uses, ccsession.ToolUseBlock{Type: "tool_use", ID: id, Name: "Read", Input: map[string]any{}})
		r := ccsession.ToolResultBlock{Type: "tool_result", ToolUseID: id, Content: "RESULT-" + id}
		combined, results = append(combined, r), append(results, r)
	}
	assistant := ccsession.Message{Role: "assistant", Content: uses}
	split = append(split, assistant)
	for _, r := range results {
		split = append(split, ccsession.Message{Role: "user", Content: []ccsession.Block{r}})
	}
	single := []ccsession.Message{{Role: "user", Content: "go"}, assistant, {Role: "user", Content: combined}}
	resultsOf := func(msgs []ccsession.Message) (out []string) {
		for _, m := range ccsession.RepairToolPairing(msgs) {
			blocks, _ := m.Content.([]ccsession.Block)
			for _, b := range blocks {
				if r, ok := b.(ccsession.ToolResultBlock); ok {
					out = append(out, fmt.Sprint(r.Content))
				}
			}
		}
		return
	}
	if got := resultsOf(single); !reflect.DeepEqual(got, []string{"RESULT-t1", "RESULT-t2", "RESULT-t3"}) {
		t.Errorf("single message: %v", got)
	}
	placeholder := false
	for _, c := range resultsOf(split) {
		placeholder = placeholder || !strings.HasPrefix(c, "RESULT-")
	}
	if !placeholder {
		t.Errorf("RepairToolPairing now tolerates split results (%v); ConvertPiMessages could stop collecting them", resultsOf(split))
	}
}

// --- environment suppression ---

func TestEnableClaudeaiMcpServersZeroSuppressesClaudeAiCloudMcpServers(t *testing.T) {
	o := providerOptions(t)
	o.MaxTurns, o.StrictMcpConfig = 1, false
	isCloud := func(init map[string]any) (out []any) {
		servers, _ := init["mcp_servers"].([]any)
		for _, s := range servers {
			if strings.HasPrefix(str(at(s, "name")), "claude.ai ") {
				out = append(out, s)
			}
		}
		return
	}
	if cloud := isCloud(rawInit(t, o, "")); len(cloud) == 0 {
		t.Skip("no claude.ai cloud MCP servers configured for this account; nothing to suppress")
	}
	if after := isCloud(rawInit(t, o, "0")); len(after) != 0 {
		t.Fatalf("cloud MCP servers survived the env gate: %v", after)
	}
}

func TestStrictMcpConfigSuppressesFilesystemMcpServers(t *testing.T) {
	o := providerOptions(t)
	o.MaxTurns, o.StrictMcpConfig = 1, false
	before := initOnly(t, startLive(t, o, nil))
	if servers, _ := before["mcp_servers"].([]any); len(servers) == 0 {
		t.Skip("no filesystem MCP servers configured; nothing to suppress")
	}
	o.StrictMcpConfig = true
	after := initOnly(t, startLive(t, o, nil))
	if servers, _ := after["mcp_servers"].([]any); len(servers) != 0 {
		t.Fatalf("filesystem MCP servers survived --strict-mcp-config: %v", servers)
	}
}

func TestThinkingDisplaySummarizedIsStillAnAcceptedFlagValue(t *testing.T) {
	// A liveness check: an invalid value makes the CLI exit 1 (`full` does).
	o := providerOptions(t)
	o.Effort, o.MaxTurns, o.NoSessionPersistence = "medium", 1, true
	summarized := "summarized"
	o.ExtraArgs = []ExtraArg{{Key: "thinking-display", Value: &summarized}}
	obs := run(t, startLive(t, o, nil), "Reply with just: OK")
	if obs.result == nil || obs.result["subtype"] != "success" {
		t.Fatalf("CC rejected --thinking-display summarized: %v", obs.result)
	}
}

// --- captured request contracts ---

// stubAPI answers every /v1/messages with a canned "OK" stream and records the request bodies.
type stubAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
}

func newStubAPI(t *testing.T) *stubAPI {
	s := &stubAPI{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.requests = append(s.requests, body)
		n := len(s.requests)
		s.mu.Unlock()
		event := func(name string, v map[string]any) string {
			return fmt.Sprintf("event: %s\ndata: %s\n\n", name, mustJSON(v))
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w,
			event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_stub_%d", n), "type": "message", "role": "assistant", "content": []any{}, "model": body["model"], "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 1, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}})+
				event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})+
				event("ping", map[string]any{"type": "ping"})+
				event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "OK"}})+
				event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})+
				event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}})+
				event("message_stop", map[string]any{"type": "message_stop"}))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubAPI) last() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	return s.requests[len(s.requests)-1]
}

// presetRequests keeps only our own requests, by a preset anchor, so background side requests cannot be
// compared by mistake.
func (s *stubAPI) presetRequests() (out []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if strings.Contains(systemText(r), "You are a Claude agent, built on Anthropic's Claude Agent SDK") {
			out = append(out, r)
		}
	}
	return
}

func systemText(body map[string]any) string {
	var sb strings.Builder
	blocks, _ := body["system"].([]any)
	for _, b := range blocks {
		sb.WriteString(str(at(b, "text")))
		sb.WriteString("\n")
	}
	return sb.String()
}

// sansCacheControl strips cache_control markers: breakpoint directives, not cache-keyed content.
func sansCacheControl(v any) any {
	switch c := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range c {
			if k != "cache_control" {
				out[k] = sansCacheControl(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(c))
		for i, x := range c {
			out[i] = sansCacheControl(x)
		}
		return out
	}
	return v
}

func count(v any, sub string) int {
	return strings.Count(string(mustJSON(map[string]any{"v": v})), sub)
}

func TestClaudeMdExcludesPreventsNativeAgentsMdFromDuplicatingForwardedInstructions(t *testing.T) {
	api := newStubAPI(t)
	marker := "project-instructions-" + newUUID(t)
	for _, excluded := range []bool{false, true} {
		o := providerOptions(t)
		if err := os.WriteFile(filepath.Join(o.Cwd, "AGENTS.md"), []byte(marker), 0o644); err != nil {
			t.Fatal(err)
		}
		o.MaxTurns, o.NoSessionPersistence = 1, true
		o.Env = []string{"ANTHROPIC_BASE_URL=" + api.URL}
		globs := []any{"**/CLAUDE.md", "**/.claude/rules/**"}
		if excluded {
			globs = append(globs, "**/AGENTS.md")
		}
		o.Settings = map[string]any{"claudeMdExcludes": globs}
		o.AppendSystemPrompt = marker // the claude_code preset plus an appended block, as the bridge sends it
		obs := run(t, startLive(t, o, nil), "Reply OK.")
		if obs.result == nil || obs.result["subtype"] != "success" {
			t.Fatalf("excluded=%v: result = %v", excluded, obs.result)
		}
		req := api.last()
		if req == nil {
			t.Fatal("CC sent no request")
		}
		if n := count(req["system"], marker); n != 1 {
			t.Errorf("excluded=%v: forwarded instructions appear %d times in system, want 1", excluded, n)
		}
		want := 1
		if excluded {
			want = 0
		}
		if n := count(req["messages"], marker); n != want {
			t.Errorf("excluded=%v: AGENTS.md marker appears %d times in messages, want %d", excluded, n, want)
		}
	}
}

func TestIncludeGitInstructionsFalseStripsGitStatusAndKeepsThePresetStaticAcrossGitTransitions(t *testing.T) {
	// The preset embeds a gitStatus snapshot, and the bridge re-invokes CC every turn, so a git transition
	// rewrites it and busts the prompt cache. Pin both sides: without the setting the snapshot is present
	// and a transition moves it; with it the snapshot is gone and the system blocks plus the leading user
	// message are identical across a transition.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	api := newStubAPI(t)
	repo := scratchDir(t)
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=probe", "GIT_AUTHOR_EMAIL=p@x", "GIT_COMMITTER_NAME=probe", "GIT_COMMITTER_EMAIL=p@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "--allow-empty", "-qm", "init")
	opts := func(settings map[string]any, resume string) Options {
		o := providerOptions(t)
		o.Cwd, o.MaxTurns, o.Settings, o.Resume = repo, 1, settings, resume
		o.Env = []string{"ANTHROPIC_BASE_URL=" + api.URL}
		return o
	}
	turn := func(o Options) *observed { return run(t, startLive(t, o, nil), "Reply OK.") }
	firstMsg := func(r map[string]any) any { return sansCacheControl(at(r, "messages", 0)) }

	// Control: the default preset carries the snapshot in the leading user message and a transition moves it.
	turn(opts(nil, ""))
	if req := api.last(); req == nil || !strings.Contains(string(mustJSON(req)), "gitStatus") {
		t.Fatal("the preset no longer carries a gitStatus block; this test's negative side is obsolete")
	}
	if err := os.WriteFile(filepath.Join(repo, "ctrl-new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn(opts(nil, ""))
	c := api.presetRequests()
	c1, c2 := c[len(c)-2], c[len(c)-1]
	if !strings.Contains(string(mustJSON(at(c1, "messages", 0))), "gitStatus") || !strings.Contains(string(mustJSON(at(c2, "messages", 0))), "gitStatus") {
		t.Fatal("the git snapshot no longer rides in the leading user message; re-point this control")
	}
	if reflect.DeepEqual(firstMsg(c1), firstMsg(c2)) {
		t.Fatal("a git transition did not move the git snapshot without the setting; the break this test guards no longer exists")
	}

	// With the setting: baseline, a git transition, then resume.
	off := map[string]any{"includeGitInstructions": false}
	base := turn(opts(off, ""))
	if base.result == nil || base.result["subtype"] != "success" {
		t.Fatalf("baseline result = %v", base.result)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn(opts(off, str(base.result["session_id"])))
	p := api.presetRequests()
	t1, t2 := p[len(p)-2], p[len(p)-1]
	for _, body := range []map[string]any{t1, t2} {
		if strings.Contains(string(mustJSON(map[string]any{"system": body["system"], "messages": body["messages"]})), "gitStatus") {
			t.Error("a gitStatus block survived includeGitInstructions:false")
		}
	}
	// Block 0 of system is a per-request billing header, hence [1:].
	s1, _ := sansCacheControl(t1["system"]).([]any)
	s2, _ := sansCacheControl(t2["system"]).([]any)
	if len(s1) < 1 || len(s2) < 1 || !reflect.DeepEqual(s1[1:], s2[1:]) {
		t.Error("system prompt changed across a git transition despite includeGitInstructions:false")
	}
	if !reflect.DeepEqual(firstMsg(t1), firstMsg(t2)) {
		t.Error("leading user message changed across a git transition despite includeGitInstructions:false")
	}
}
