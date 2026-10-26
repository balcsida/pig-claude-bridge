package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
)

// replies is a script: one result frame, then a clean exit.
func replies(text string) func(*spawn) {
	return func(s *spawn) {
		s.client.emit(resultFrame(text))
		s.client.exit(0, "")
	}
}

var isolatedContext = map[string]any{
	"systemPrompt": "You are an advisor.",
	"messages":     msgs(map[string]any{"role": "user", "content": []any{textBlock("Advise.")}, "timestamp": 0}),
}

// The Fable 5.1 model of unit-isolated-effort.mjs: only xhigh and max are named, off is unsupported.
var fableModel = map[string]any{
	"id": "claude-fable-5-1", "api": "claude-bridge", "provider": "claude-bridge", "reasoning": true,
	"thinkingLevelMap": map[string]any{"off": nil, "xhigh": "xhigh", "max": "max"},
}

// runIsolated is the TypeScript's runIsolated: one call, which must succeed, and the options its process got.
func runIsolated(t *testing.T, model map[string]any, options map[string]any) ccproto.Options {
	t.Helper()
	h := newProviderHarness(t, nil)
	h.script(replies("advice"))
	stream, err := h.b.IsolatedStream(context.Background(), h.host, model, isolatedContext, options, "summary", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply := finalOf(t, stream); reply["stopReason"] != "stop" {
		t.Fatalf("reply = %v", reply)
	}
	if len(h.spawns()) != 1 {
		t.Fatalf("spawns = %d", len(h.spawns()))
	}
	return h.lastSpawn().opts
}

// --- unit-isolated-effort.mjs ---

func TestIsolatedEffort(t *testing.T) {
	unmapped := map[string]any{"id": "claude-test-unmapped", "reasoning": true}
	t.Run("effortForReasoning uses the model's thinkingLevelMap when it names the level", func(t *testing.T) {
		if effortForReasoning(fableModel, "max") != "max" || effortForReasoning(fableModel, "xhigh") != "xhigh" {
			t.Fatal("mapped levels")
		}
	})
	t.Run("effortForReasoning falls back to the generic table for levels the map leaves unnamed", func(t *testing.T) {
		if effortForReasoning(fableModel, "high") != "high" || effortForReasoning(unmapped, "xhigh") != "max" || effortForReasoning(unmapped, "max") != "max" {
			t.Fatal("generic levels")
		}
	})
	t.Run("effortForReasoning sends no effort for unsupported or absent levels", func(t *testing.T) {
		if effortForReasoning(fableModel, "off") != "" || effortForReasoning(fableModel, nil) != "" {
			t.Fatal("expected no effort")
		}
	})
	t.Run("isolated path passes the caller's reasoning level to Claude Code", func(t *testing.T) {
		if got := runIsolated(t, fableModel, map[string]any{"cacheRetention": "none", "reasoning": "max"}).Effort; got != "max" {
			t.Fatalf("effort = %q", got)
		}
	})
	t.Run("isolated path leaves effort to Claude Code's default without a reasoning level", func(t *testing.T) {
		if got := runIsolated(t, fableModel, map[string]any{"cacheRetention": "none"}).Effort; got != "" {
			t.Fatalf("effort = %q", got)
		}
	})
}

// --- unit-summary-cwd.mjs (the cwd attribution) ---

// The TypeScript runs two real pi sessions at once ("concurrent native ... queries use
// their owning session's cwd") and keys the pending summary by AbortSignal; Go has no
// such identity (divergence D7), so the most recent pending summary owns the call. The
// adaptation runs each session's operation in turn, which proves attribution only while
// operations do not overlap; an overlapping pair goes to the later one, pinned below.
func TestIsolatedSummaryCwd(t *testing.T) {
	for _, operation := range []string{"compaction", "branch summary"} {
		t.Run("native "+operation+" queries use their owning session's cwd", func(t *testing.T) {
			h := newProviderHarness(t, nil)
			calls := 1
			if operation == "compaction" {
				calls = 2
			}
			for _, id := range []string{"one", "two"} {
				host := &fakeHost{sessionID: id, cwd: t.TempDir()}
				h.b.OnSessionStart(host, "startup")
				if operation == "compaction" {
					h.b.OnSessionBeforeCompact(host)
				} else {
					h.b.OnSessionBeforeTree(host)
				}
				for i := 0; i < calls; i++ {
					h.script(replies("Controlled native summary"))
					stream, _ := h.b.IsolatedStream(context.Background(), host, testModel, isolatedContext, map[string]any{"cacheRetention": "none"}, "summary", nil, nil)
					finalOf(t, stream)
					if got := h.lastSpawn().opts.Cwd; got != host.cwd {
						t.Fatalf("session %s cwd = %q, want %q", id, got, host.cwd)
					}
				}
				if operation == "compaction" {
					h.b.OnSessionCompact(host, "session_compact:manual:willRetry=false")
				} else {
					h.b.forgetSummarySession(id)
				}
			}
			if len(h.spawns()) != 2*calls {
				t.Fatalf("spawns = %d", len(h.spawns()))
			}
		})
	}
	t.Run("overlapping operations: the later one's session owns both sessions' calls (D7)", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		one, two := &fakeHost{sessionID: "one", cwd: t.TempDir()}, &fakeHost{sessionID: "two", cwd: t.TempDir()}
		h.b.OnSessionStart(one, "startup")
		h.b.OnSessionStart(two, "startup")
		h.b.OnSessionBeforeCompact(one)
		h.b.OnSessionBeforeCompact(two)
		for _, host := range []*fakeHost{one, two} {
			h.script(replies("summary"))
			finalOf(t, mustStream(h.b.IsolatedStream(context.Background(), host, testModel, isolatedContext, map[string]any{"cacheRetention": "none"}, "summary", nil, nil)))
		}
		if s := h.spawns(); s[0].opts.Cwd != two.cwd || s[1].opts.Cwd != two.cwd {
			t.Fatalf("cwds = %q, %q, want both %q", s[0].opts.Cwd, s[1].opts.Cwd, two.cwd)
		}
	})
	t.Run("without a pending summary the call's own session id owns the cwd, else the process cwd", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.b.rememberSessionCwd("own", "/agents/own")
		h.script(replies("a"))
		h.script(replies("b"))
		finalOf(t, mustStream(h.b.IsolatedStream(context.Background(), h.host, testModel, isolatedContext, map[string]any{"cacheRetention": "none", "sessionId": "own"}, "summary", nil, nil)))
		finalOf(t, mustStream(h.b.IsolatedStream(context.Background(), h.host, testModel, isolatedContext, map[string]any{"cacheRetention": "none"}, "summary", nil, nil)))
		wd, _ := os.Getwd()
		if s := h.spawns(); s[0].opts.Cwd != "/agents/own" || s[1].opts.Cwd != wd {
			t.Fatalf("cwds = %q, %q", s[0].opts.Cwd, s[1].opts.Cwd)
		}
	})
}

// --- the isolated path itself ---

func mustStream[S any](s S, err error) S {
	if err != nil {
		panic(err)
	}
	return s
}

func TestIsolatedStream(t *testing.T) {
	call := func(h *providerHarness, ctx context.Context, piContext, options map[string]any) map[string]any {
		return finalOf(t, mustStream(h.b.IsolatedStream(ctx, h.host, testModel, piContext, options, "summary", nil, nil)))
	}
	t.Run("returns the result text and spawns a process with no session, tools or MCP server", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(func(s *spawn) {
			s.client.emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "partial"}}}}, resultFrame("final text"))
			s.client.exit(0, "")
		})
		reply := call(h, context.Background(), isolatedContext, map[string]any{"cacheRetention": "none"})
		content, _ := reply["content"].([]any)
		if reply["stopReason"] != "stop" || len(content) != 1 || obj(content[0])["text"] != "final text" {
			t.Fatalf("reply = %v", reply)
		}
		o := h.lastSpawn().opts
		if !o.NoSessionPersistence || !o.StrictMcpConfig || len(o.Tools) != 0 || !o.ToolsSet || len(o.SDKMcpServers) != 0 || o.Resume != "" || o.SystemPrompt != "You are an advisor." {
			t.Fatalf("opts = %+v", o)
		}
		if w := h.lastSpawn().client.writes(); len(w) != 1 {
			t.Fatalf("writes = %v", w)
		}
		if _, closes, _ := h.lastSpawn().client.counts(); closes == 0 {
			t.Fatal("client not closed")
		}
	})
	t.Run("falls back to the assistant text when the result is empty", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(func(s *spawn) {
			s.client.emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "from assistant"}}}}, resultFrame(""))
			s.client.exit(0, "")
		})
		reply := call(h, context.Background(), isolatedContext, nil)
		if content, _ := reply["content"].([]any); len(content) != 1 || obj(content[0])["text"] != "from assistant" {
			t.Fatalf("reply = %v", reply)
		}
	})
	t.Run("an error result is the stream's error", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(func(s *spawn) {
			s.client.emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "overloaded"})
			s.client.exit(0, "")
		})
		reply := call(h, context.Background(), isolatedContext, nil)
		if reply["stopReason"] != "error" || reply["errorMessage"] != "overloaded" {
			t.Fatalf("reply = %v", reply)
		}
	})
	t.Run("an empty summary is an error", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(replies("  "))
		if reply := call(h, context.Background(), isolatedContext, nil); reply["errorMessage"] != "Claude Code summary returned empty text" {
			t.Fatalf("reply = %v", reply)
		}
	})
	t.Run("a process that exits abnormally is an error", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(func(s *spawn) { s.client.exit(1, "boom") })
		if reply := call(h, context.Background(), isolatedContext, nil); reply["stopReason"] != "error" || !strings.Contains(str(reply["errorMessage"]), "boom") {
			t.Fatalf("reply = %v", reply)
		}
	})
	t.Run("a non-zero exit after a successful result is the stream's error", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(func(s *spawn) {
			s.client.emit(resultFrame("fine"))
			s.client.exit(1, "late failure")
		})
		events := eventsOf(t, mustStream(h.b.IsolatedStream(context.Background(), h.host, testModel, isolatedContext, nil, "summary", nil, nil)))
		if len(events) != 1 || events[0]["type"] != "error" || !strings.Contains(str(obj(events[0]["error"])["errorMessage"]), "late failure") {
			t.Fatalf("events = %v", events)
		}
	})
	t.Run("an abort landing with the end of the frames still reports aborted", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			h := newProviderHarness(t, nil)
			spawned := make(chan *fakeClient, 1)
			h.script(func(s *spawn) { spawned <- s.client })
			ctx, cancel := context.WithCancel(context.Background())
			stream := mustStream(h.b.IsolatedStream(ctx, h.host, testModel, isolatedContext, nil, "summary", nil, nil))
			fc := receive(t, spawned, "the spawn")
			cancel()
			fc.exit(0, "")
			if reply := finalOf(t, stream); reply["stopReason"] != "aborted" || reply["errorMessage"] != "Operation aborted" {
				t.Fatalf("reply = %v", reply)
			}
		}
	})
	// Deterministic: the loop sees the frames end with no abort yet, and the abort lands
	// while it waits for the exit, so the check after the loop is what reports it.
	t.Run("an abort after the loop saw the frames end still reports aborted", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		h.script(func(s *spawn) {
			s.client.onWait = cancel
			s.client.exit(0, "")
		})
		if reply := finalOf(t, mustStream(h.b.IsolatedStream(ctx, h.host, testModel, isolatedContext, nil, "summary", nil, nil))); reply["stopReason"] != "aborted" {
			t.Fatalf("reply = %v", reply)
		}
	})
	t.Run("a failed spawn is an error", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		if reply := call(h, context.Background(), isolatedContext, nil); !strings.Contains(str(reply["errorMessage"]), "start Claude Code") {
			t.Fatalf("reply = %v", reply)
		}
	})
	t.Run("a call that is not a one-off must hold exactly one user message", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		two := map[string]any{"messages": msgs(userMsg("a"), assistantMsg([]any{textBlock("b")}, "stop"))}
		reply := call(h, context.Background(), two, nil)
		if want := "isolatedStreamFn: expected exactly 1 user message, got 2 (user,assistant)"; reply["errorMessage"] != want || len(h.spawns()) != 0 {
			t.Fatalf("reply = %v", reply)
		}
		if reply := call(h, context.Background(), map[string]any{"messages": msgs(userMsg(""))}, nil); reply["errorMessage"] != "isolatedStreamFn: summarization prompt is empty" {
			t.Fatalf("reply = %v", reply)
		}
	})
	t.Run("a one-off without a user prompt is an error", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		reply := call(h, context.Background(), map[string]any{"messages": msgs(assistantMsg([]any{textBlock("b")}, "stop"))}, map[string]any{"cacheRetention": "none"})
		if !strings.HasPrefix(str(reply["errorMessage"]), "runIsolatedSummary: one-off summary without a user prompt") {
			t.Fatalf("reply = %v", reply)
		}
	})
	t.Run("an abort ends the stream as aborted and closes the process", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(func(*spawn) {})
		ctx, cancel := context.WithCancel(context.Background())
		stream := mustStream(h.b.IsolatedStream(ctx, h.host, testModel, isolatedContext, nil, "summary", nil, nil))
		waitFor(t, "the spawn", func() bool { return len(h.spawns()) == 1 })
		cancel()
		reply := finalOf(t, stream)
		if reply["stopReason"] != "aborted" || reply["errorMessage"] != "Operation aborted" {
			t.Fatalf("reply = %v", reply)
		}
		waitFor(t, "the close", func() bool { _, closes, _ := h.lastSpawn().client.counts(); return closes > 0 })
	})
	t.Run("the caller's tools and allowed tools reach the process", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(replies("ok"))
		finalOf(t, mustStream(h.b.IsolatedStream(context.Background(), h.host, testModel, isolatedContext, nil, "ask", []string{"Read"}, []string{"Read"})))
		if o := h.lastSpawn().opts; !reflect.DeepEqual(o.Tools, []string{"Read"}) || !reflect.DeepEqual(o.AllowedTools, []string{"Read"}) {
			t.Fatalf("opts = %+v", o)
		}
	})
}

// TestIsolatedOptionsMatchSnapshot pins the Options of the isolated path to what the
// Node SDK produced for the same inputs (the snapshot's isolated-summary entry).
func TestIsolatedOptionsMatchSnapshot(t *testing.T) {
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
	var want = -1
	for i, e := range entries {
		if e.Name == "isolated-summary" {
			want = i
		}
	}
	if want < 0 {
		t.Fatal("no isolated-summary entry")
	}
	e := entries[want]

	// Forced 200k: the 1m-context variant of the model id is not under test.
	h := newProviderHarness(t, func(d *Deps) { d.Config.Provider.ForceTwoHundredK = []string{"claude-opus-5-5"} })
	h.script(replies("ok"))
	model := map[string]any{"id": "claude-opus-5-5", "api": "claude-bridge", "provider": "claude-bridge"}
	piContext := map[string]any{"systemPrompt": "SYSTEM", "messages": msgs(userMsg("PROMPT"))}
	finalOf(t, mustStream(h.b.IsolatedStream(context.Background(), h.host, model, piContext, map[string]any{"cacheRetention": "none", "reasoning": "high"}, "summary", nil, nil)))
	o := h.lastSpawn().opts
	// The snapshot resumed a session; the isolated path has no resume of its own.
	o.Resume = "11111111-1111-1111-1111-111111111111"

	got := ccproto.BuildArgs(o)
	if len(got) != len(e.Argv) {
		t.Fatalf("argv\n got  %q\n want %q", got, e.Argv)
	}
	for i, w := range e.Argv {
		if i > 0 && e.Argv[i-1] == "--settings" {
			var a, b any
			_ = json.Unmarshal([]byte(got[i]), &a)
			_ = json.Unmarshal([]byte(w), &b)
			if !reflect.DeepEqual(a, b) {
				t.Errorf("--settings: got %s want %s", got[i], w)
			}
		} else if got[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], w)
		}
	}
	gotEnv := map[string]string{}
	for _, kv := range ccproto.BuildEnv(nil, o) {
		k, v, _ := strings.Cut(kv, "=")
		gotEnv[k] = v
	}
	// The recorder builds env without process.env, where the TypeScript bridge sets this one.
	delete(gotEnv, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC")
	if !reflect.DeepEqual(gotEnv, e.Env) {
		t.Errorf("env\n got  %v\n want %v", gotEnv, e.Env)
	}
	if init := string(ccproto.BuildInitialize(o, "<id>")); init != compact(t, e.Initialize) {
		t.Errorf("initialize\n got  %s\n want %s", init, compact(t, e.Initialize))
	}
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestIsolatedStreamHonoursAnAbortWhileClaudeCodeStarts(t *testing.T) {
	fake := newFakeClaude(t, "FAKECLAUDE_STALL_INIT=1")
	h := newProviderHarness(t, func(d *Deps) { d.Spawn = fake.spawn })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := mustStream(h.b.IsolatedStream(ctx, h.host, testModel, isolatedContext, nil, "compact-summary", nil, nil))
	fake.awaitLaunch(t)
	cancel()
	if reply := finalOf(t, stream); reply["stopReason"] != "aborted" {
		t.Fatalf("reply = %v", reply)
	}
}

func TestIsolatedWriterPanicIsContained(t *testing.T) {
	h := newProviderHarness(t, func(d *Deps) { d.Debug = true })
	h.script(func(s *spawn) {
		s.client.onWrite = func(ccproto.UserMessage) error { panic("writer exploded") }
		replies("advice")(s)
	})
	finalOf(t, mustStream(h.b.IsolatedStream(context.Background(), h.host, testModel, isolatedContext, nil, "compact-summary", nil, nil)))
	if log := readDebugLog(t, h.agentDir); !strings.Contains(log, "isolated writer: recovered from panic: writer exploded") {
		t.Fatalf("log:\n%s", log)
	}
}
