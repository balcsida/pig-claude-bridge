package bridge

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

var noSkills = false

// runAsk is one PromptAndWait call against a scripted process; it returns the answer, the error and the options the process got.
func runAsk(t *testing.T, h *providerHarness, script func(*spawn), r AskRequest) (string, error, *spawn) {
	t.Helper()
	h.script(script)
	r.AppendSkills = &noSkills
	text, err := h.b.PromptAndWait(context.Background(), h.host, r)
	return text, err, h.lastSpawn()
}

func TestPromptAndWaitIsolated(t *testing.T) {
	h := newProviderHarness(t, nil)
	disallowed := []string{"Write", "Bash"}
	text, err, s := runAsk(t, h, replies("the answer"), AskRequest{
		Prompt: "why?", DisallowedTools: disallowed, Isolated: true, Model: "sonnet", Thinking: "high",
		Context: msgs(map[string]any{"role": "user", "content": "earlier"}),
	})
	if err != nil || text != "the answer" {
		t.Fatalf("text = %q, err = %v", text, err)
	}
	o := s.opts
	if o.Resume != "" || !o.NoSessionPersistence {
		t.Fatalf("isolated call resumed %q or persisted (NoSessionPersistence = %t)", o.Resume, o.NoSessionPersistence)
	}
	if !slices.Equal(o.DisallowedTools, disallowed) || o.PermissionMode != "bypassPermissions" || o.ToolsSet {
		t.Fatalf("tool options = %+v", o)
	}
	if o.Effort != "high" || !o.SettingSourcesSet || !slices.Equal(o.SettingSources, []string{"user", "project"}) || o.Skills == nil || len(o.Skills) != 0 || o.SystemPromptSet {
		t.Fatalf("options = %+v", o)
	}
	var args []string
	for _, e := range o.ExtraArgs {
		v := ""
		if e.Value != nil {
			v = "=" + *e.Value
		}
		args = append(args, e.Key+v)
	}
	if !slices.Contains(args, "strict-mcp-config") || !slices.Contains(args, "thinking-display=summarized") || !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "model=claude-sonnet") }) {
		t.Fatalf("extra args = %q", args)
	}
	if o.Settings["claudeMdExcludes"] == nil {
		t.Fatal("claudeMdExcludes missing from settings")
	}
	written, _ := json.Marshal(s.client.written)
	if len(s.client.written) != 1 || !strings.Contains(string(written), "why?") {
		t.Fatalf("written = %s", written)
	}
}

func TestPromptAndWaitContinuesTheRememberedSession(t *testing.T) {
	h := newProviderHarness(t, nil)
	h.b.setSessionStateFor("pi-1", &SessionState{SessionID: "cc-remembered", PiSessionID: "pi-1"})
	_, err, s := runAsk(t, h, replies("ok"), AskRequest{
		Prompt: "next", PiSessionID: "pi-1", Context: msgs(map[string]any{"role": "user", "content": "earlier"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.opts.Resume != "cc-remembered" || s.opts.NoSessionPersistence {
		t.Fatalf("resume = %q, NoSessionPersistence = %t", s.opts.Resume, s.opts.NoSessionPersistence)
	}
}

func TestPromptAndWaitWithoutContextDoesNotResume(t *testing.T) {
	h := newProviderHarness(t, nil)
	h.b.setSessionStateFor("pi-1", &SessionState{SessionID: "cc-remembered", PiSessionID: "pi-1"})
	_, err, s := runAsk(t, h, replies("ok"), AskRequest{Prompt: "next", PiSessionID: "pi-1"})
	if err != nil || s.opts.Resume != "" {
		t.Fatalf("resume = %q, err = %v", s.opts.Resume, err)
	}
}

func TestPromptAndWaitReportsToolUse(t *testing.T) {
	h := newProviderHarness(t, nil)
	type use struct{ id, name, path string }
	var got []use
	_, err, _ := runAsk(t, h, func(s *spawn) {
		s.client.emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "looking"},
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "a.go"}},
		}}})
		replies("done")(s)
	}, AskRequest{Prompt: "q", OnToolUse: func(id, name string, input any) {
		got = append(got, use{id, name, input.(map[string]any)["file_path"].(string)})
	}})
	if err != nil || len(got) != 1 || got[0] != (use{"t1", "Read", "a.go"}) {
		t.Fatalf("tool uses = %+v, err = %v", got, err)
	}
}

func TestPromptAndWaitFailsOnAnErrorResult(t *testing.T) {
	h := newProviderHarness(t, nil)
	text, err, _ := runAsk(t, h, func(s *spawn) {
		s.client.emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "429 rate limited"})
		s.client.exit(0, "")
	}, AskRequest{Prompt: "q"})
	if err == nil || err.Error() != "429 rate limited" || text != "" {
		t.Fatalf("text = %q, err = %v", text, err)
	}
}

func TestPromptAndWaitFailsWhenTheProcessCannotStart(t *testing.T) {
	h := newProviderHarness(t, nil)
	_, err := h.b.PromptAndWait(context.Background(), h.host, AskRequest{Prompt: "q", AppendSkills: &noSkills})
	if err == nil || !strings.Contains(err.Error(), "start Claude Code") {
		t.Fatalf("err = %v", err)
	}
}

// As the TypeScript's promptAndWait: an abort once the process runs ends the call with
// the answer so far (stopReason cancelled), not an error; one before it starts is the error.
func TestPromptAndWaitAborts(t *testing.T) {
	t.Run("mid-run, with the answer so far", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		h.script(func(s *spawn) { // answers, then stays up as a process with input open does
			s.client.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "partial"})
		})
		ctx, cancel := context.WithCancel(context.Background())
		type answer struct {
			text string
			err  error
		}
		done := make(chan answer, 1)
		go func() {
			text, err := h.b.PromptAndWait(ctx, h.host, AskRequest{Prompt: "q", AppendSkills: &noSkills})
			done <- answer{text, err}
		}()
		waitFor(t, "the result to be read", func() bool {
			s := h.spawns()
			return len(s) == 1 && s[0].client.sent.Load() == 1
		})
		cancel()
		if got := receive(t, done, "PromptAndWait to return after the abort"); got.text != "partial" || got.err != nil {
			t.Fatalf("got %q, %v", got.text, got.err)
		}
	})
	t.Run("before the call starts", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := h.b.PromptAndWait(ctx, h.host, AskRequest{Prompt: "q", AppendSkills: &noSkills}); err == nil || err.Error() != "Aborted" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPromptAndWaitSkipsSkillsWhenReadIsBlocked(t *testing.T) {
	h := newProviderHarness(t, nil)
	h.script(replies("ok"))
	// No prompt capture exists, so resolving skills would fail; a mode without Read must not need one.
	if _, err := h.b.PromptAndWait(context.Background(), h.host, AskRequest{Prompt: "q", DisallowedTools: []string{"Read"}}); err != nil {
		t.Fatal(err)
	}
	h.script(replies("ok"))
	if _, err := h.b.PromptAndWait(context.Background(), h.host, AskRequest{Prompt: "q", SystemPrompt: "unknown"}); err == nil {
		t.Fatal("an unresolvable prompt did not fail a call that needs skills")
	}
}

func TestPromptAndWaitHonoursAnAbortWhileClaudeCodeStarts(t *testing.T) {
	fake := newFakeClaude(t, "FAKECLAUDE_STALL_INIT=1")
	h := newProviderHarness(t, func(d *Deps) { d.Spawn = fake.spawn })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.b.PromptAndWait(ctx, h.host, AskRequest{Prompt: "q", AppendSkills: &noSkills})
		done <- err
	}()
	fake.awaitLaunch(t)
	cancel()
	// The query exists once the process is launched, so this is a cancelled call with no answer yet.
	if err := receive(t, done, "PromptAndWait to return after the abort"); err != nil {
		t.Fatalf("err = %v", err)
	}
}
