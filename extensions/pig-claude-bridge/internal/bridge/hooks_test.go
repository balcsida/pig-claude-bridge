package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/promptcapture"
)

// --- unit-prompt-blocks.mjs ---

func textBlock(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

func imageBlock(data string) map[string]any {
	return map[string]any{"type": "image", "mimeType": "image/png", "data": data}
}

func base64Image(data string) map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": data}}
}

func blockMsg(blocks ...any) map[string]any { return map[string]any{"role": "user", "content": blocks} }

func TestExtractUserPromptBlocks(t *testing.T) {
	b, _, _ := newTestBridge(t, true)
	t.Run("keeps images and text from the trailing run of user messages", func(t *testing.T) {
		got, err := b.extractUserPromptBlocks(msgs(
			blockMsg(textBlock("describe this"), imageBlock("aW1hZ2U=")),
			map[string]any{"role": "user", "content": "(attachment preview: [#image 1])"},
		))
		want := []any{textBlock("describe this"), base64Image("aW1hZ2U="), textBlock("(attachment preview: [#image 1])")}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("does not reach past the current user turn", func(t *testing.T) {
		got, err := b.extractUserPromptBlocks(msgs(
			blockMsg(imageBlock("b2xk")),
			map[string]any{"role": "assistant", "content": []any{textBlock("done")}},
			map[string]any{"role": "user", "content": "next turn"},
		))
		if err != nil || got != nil {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("skips malformed image blocks instead of throwing", func(t *testing.T) {
		got, err := b.extractUserPromptBlocks(msgs(blockMsg(
			textBlock("look"),
			map[string]any{"type": "image", "mimeType": "image/png"},
			imageBlock("aW1hZ2U="),
		)))
		want := []any{textBlock("look"), base64Image("aW1hZ2U=")}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("throws a legible error on off-type content", func(t *testing.T) {
		_, err := b.extractUserPromptBlocks(msgs(map[string]any{"role": "user", "content": nil}))
		if err == nil || !strings.Contains(err.Error(), "content must be a string or block array, got") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestHistoryPromptSplit(t *testing.T) {
	// The turn boundary is computed once and both halves derive from it, so a message
	// can never be replayed as history and resent as the prompt.
	t.Run("does not replay the current turn as session history", func(t *testing.T) {
		b, claudeDir, _ := newTestBridge(t, false)
		cwd := t.TempDir()
		messages := msgs(
			map[string]any{"role": "user", "content": "earlier question", "timestamp": 1},
			map[string]any{"role": "assistant", "content": []any{textBlock("earlier answer")}, "timestamp": 2},
			// The current turn: the image-bearing message plus an extension's trailing
			// display-only message (issue #34).
			blockMsg(textBlock("describe this"), imageBlock("aW1hZ2U=")),
			map[string]any{"role": "user", "content": "(attachment preview: [#image 1])", "timestamp": 4},
		)
		res, err := b.syncSharedSession(nil, "", messages, nil, cwd, "claude-haiku-4-5")
		if err != nil {
			t.Fatal(err)
		}
		history, err := os.ReadFile(ccsession.SessionPath(claudeDir, cwd, res.SessionID))
		if err != nil {
			t.Fatalf("syncSharedSession should have written a session file: %v", err)
		}
		blocks, _ := b.extractUserPromptBlocks(messages)
		prompt, _ := json.Marshal(blocks)
		if !strings.Contains(string(prompt), "describe this") {
			t.Error("the image-bearing message belongs in the prompt")
		}
		if strings.Contains(string(history), "describe this") {
			t.Error("and must not also be replayed as history")
		}
		if !strings.Contains(string(history), "earlier question") {
			t.Error("genuinely prior turns still become history")
		}
	})
}

// --- unit-prompt-capture-turn-failure.mjs ---

// A system prompt the bridge refuses to forward fails the turn through the returned
// stream instead of escaping from StreamSimple. No query starts and no stream is
// left claimed, so the next turn is unaffected.
func TestAPromptTheBridgeRefuses(t *testing.T) {
	// The failure path writes a diag entry into an agent dir that does not exist yet:
	// a diag write that cannot create it would fail the turn differently.
	newHarness := func(t *testing.T) (*providerHarness, string) {
		agentDir := filepath.Join(t.TempDir(), "agent")
		h := newProviderHarness(t, func(d *Deps) { d.AgentDir = agentDir })
		return h, agentDir
	}
	turn := func(h *providerHarness, systemPrompt string) map[string]any {
		stream, err := h.b.StreamSimple(context.Background(), h.host, testModel,
			map[string]any{"systemPrompt": systemPrompt, "messages": msgs(map[string]any{"role": "user", "content": "hi", "timestamp": 0}), "tools": []any{}},
			map[string]any{"sessionId": "pi-session"})
		if err != nil {
			t.Fatal(err)
		}
		return finalOf(t, stream)
	}

	t.Run("fails the turn on the stream when no capture accounts for the prompt", func(t *testing.T) {
		h, agentDir := newHarness(t)
		result := turn(h, "a system prompt no capture boundary recorded")
		if result["stopReason"] != "error" || !strings.Contains(str(result["errorMessage"]), "prompt-capture: no capture") {
			t.Fatalf("result = %v", result)
		}
		if len(h.spawns()) != 0 || len(h.b.liveContexts()) != 0 {
			t.Fatal("no query may start and no context may stay claimed")
		}
		if _, err := os.Stat(config.DiagLogPath(agentDir)); err != nil {
			t.Fatalf("the diag log: %v", err)
		}
	})

	t.Run("fails the turn on the stream when the sendability guard refuses the capture", func(t *testing.T) {
		h, _ := newHarness(t)
		prompt := promptcapture.PiPreamble + ", recorded with pi's harness in its custom prompt."
		h.b.captures.Record(prompt, promptcapture.Input{Custom: prompt}, "")
		result := turn(h, prompt)
		if result["stopReason"] != "error" || !strings.Contains(str(result["errorMessage"]), "prompt-capture: refusing to send this prompt") {
			t.Fatalf("result = %v", result)
		}
		if len(h.spawns()) != 0 {
			t.Fatal("no query may start")
		}
	})
}

// --- unit-agent-start-capture.mjs ---

const (
	preWiden = "You are pi.\n# Tools\n- read: Read a file\n\npi packages (docs/packages.md)"
	// Same prefix, then the MCP tool descriptions that only appear post-connect.
	widened = "You are pi.\n# Tools\n- read: Read a file\n- Agent: Launch a subagent with a very long description ... \n\npi packages (docs/packages.md)"
)

func mustNotResolve(t *testing.T, b *Bridge, prompt, why string) {
	t.Helper()
	if _, err := b.captures.ResolveOrDerive(prompt); err == nil || !strings.Contains(err.Error(), "no capture") {
		t.Fatalf("%s: err = %v", why, err)
	}
}

func TestAgentStartWidenedPromptCapture(t *testing.T) {
	t.Run("records ctx.getSystemPrompt() so the widened prompt itself resolves", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		b.OnBeforeAgentStart(host, preWiden, map[string]any{})
		// Before agent_start, only the pre-widen prompt is known.
		mustNotResolve(t, b, widened, "the widened prompt must not resolve off the pre-widen record alone")

		host.setPrompt(widened)
		b.OnAgentStart(host)
		if c, err := b.captures.ResolveOrDerive(widened); err != nil || c == nil {
			t.Fatalf("after agent_start the widened prompt resolves exactly: %v", err)
		}
	})

	t.Run("lets a child embedding the widened parent resolve by inheritance", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		b.OnBeforeAgentStart(host, preWiden, map[string]any{})
		host.setPrompt(widened)
		b.OnAgentStart(host)

		// pi-subagents embeds the widened parent prompt verbatim as the child's prefix.
		child := widened + "\n\n<sub_agent_context>be concise</sub_agent_context>\n\n<active_agent name=\"worker\"/>"
		resolved, err := b.captures.ResolveOrDerive(child)
		if err != nil || resolved == nil {
			t.Fatalf("a child embedding the widened parent must resolve, not fall to a side request: %v", err)
		}
		if len(resolved.Inherited) < 1 {
			t.Fatal("resolution must be via an inheritance edge onto the widened parent capture")
		}
	})

	t.Run("carries the stashed portable parts onto the widened key", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		files := []promptcapture.ContextFile{{Path: "/AGENTS.md", Content: "project rules"}}
		b.OnBeforeAgentStart(host, preWiden, map[string]any{"contextFiles": files, "skills": []map[string]any{}, "selectedTools": []string{"read"}})
		host.setPrompt(widened)
		b.OnAgentStart(host)

		capture, ok := b.captures.Resolve(widened)
		if !ok {
			t.Fatal("widened key must be recorded")
		}
		if !reflect.DeepEqual(capture.ContextFiles, files) {
			t.Fatalf("the widened capture must carry the before_agent_start context files, not empty ones: %v", capture.ContextFiles)
		}
	})
}

func TestTurnStartPromptCapture(t *testing.T) {
	t.Run("re-keys a prompt rebuilt between turns with the stashed portable parts", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		files := []promptcapture.ContextFile{{Path: "/AGENTS.md", Content: "project rules"}}
		b.OnBeforeAgentStart(host, preWiden, map[string]any{"contextFiles": files, "skills": []map[string]any{}, "selectedTools": []string{"read"}})
		host.setPrompt(widened)
		b.OnAgentStart(host)

		// A later in-run turn renders a new prompt (mid-run tool-loadout change,
		// section re-render). turn_start re-keys it.
		turnTwo := widened + "\n# Tools\n- git_status: Report repo state\n"
		host.setPrompt(turnTwo)
		b.OnTurnStart(host)

		capture, ok := b.captures.Resolve(turnTwo)
		if !ok {
			t.Fatal("the rebuilt prompt must resolve after turn_start")
		}
		if !reflect.DeepEqual(capture.ContextFiles, files) {
			t.Fatalf("the mid-run capture must carry the stashed portable parts: %v", capture.ContextFiles)
		}
	})

	t.Run("is idempotent when the same prompt is re-fired", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		b.OnBeforeAgentStart(host, preWiden, map[string]any{})
		host.setPrompt(widened)
		b.OnAgentStart(host)

		before := b.captures.Size()
		b.OnTurnStart(host)
		b.OnTurnStart(host)
		if _, ok := b.captures.Resolve(widened); !ok {
			t.Fatal("the prompt still resolves")
		}
		if b.captures.Size() != before {
			t.Fatalf("re-firing an unchanged prompt must not grow the registry: %d → %d", before, b.captures.Size())
		}
	})
}

func TestPromptOptionsReachTheCapture(t *testing.T) {
	t.Run("deselecting read drops the skills, selecting it keeps them", func(t *testing.T) {
		skills := []map[string]any{{"name": "deploy", "description": "Deploy", "filePath": "/s/deploy/SKILL.md"}}
		for _, tc := range []struct {
			selected []string
			want     int
		}{{[]string{"bash"}, 0}, {[]string{"read"}, 1}, {nil, 1}} {
			b, _, _ := newTestBridge(t, false)
			options := map[string]any{"skills": skills}
			if tc.selected != nil {
				options["selectedTools"] = tc.selected
			}
			b.OnBeforeAgentStart(&fakeHost{}, "prompt", options)
			if c, _ := b.captures.Resolve("prompt"); len(c.Skills) != tc.want {
				t.Fatalf("selected %v: %d skills, want %d", tc.selected, len(c.Skills), tc.want)
			}
		}
	})
	t.Run("sections keep their order, or sort by name when they arrive as a map", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		ordered := []promptcapture.Section{{Name: "zeta", Text: "z"}, {Name: "alpha", Text: "a"}}
		b.OnBeforeAgentStart(&fakeHost{}, "ordered", map[string]any{"sections": ordered})
		b.OnBeforeAgentStart(&fakeHost{}, "mapped", map[string]any{"sections": map[string]any{"zeta": "z", "alpha": "a"}})
		o, _ := b.captures.Resolve("ordered")
		m, _ := b.captures.Resolve("mapped")
		if !reflect.DeepEqual(o.Sections, ordered) || !reflect.DeepEqual(m.Sections, []promptcapture.Section{{Name: "alpha", Text: "a"}, {Name: "zeta", Text: "z"}}) {
			t.Fatalf("sections: %v / %v", o.Sections, m.Sections)
		}
	})
	t.Run("an empty prompt records nothing", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		b.OnBeforeAgentStart(&fakeHost{}, "", map[string]any{})
		if b.captures.Size() != 0 {
			t.Fatal("recorded an empty prompt")
		}
	})
}

// --- unit-agent-start-capture-gaps.mjs ---

func section(name, content string) string { return "<" + name + ">\n" + content + "\n</" + name + ">" }

func TestAgentStartCaptureDocumentedGaps(t *testing.T) {
	parentSections := []string{
		promptcapture.PiPreamble + ", a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.",
		section("tools", "- read: Read a file\n- bash: Run commands\n\nIn addition to the tools above, you may have access to other custom tools depending on the project."),
		section("rules", "- Use bash for file operations like ls, rg, find\n- Be concise in your responses"),
		section("docs", "Pi documentation (read only when the user asks about pi itself):\n- Main documentation: /pi/README.md"),
		section("project_context", "Project-specific instructions and guidelines:\n\n<project_instructions path=\"/parent/AGENTS.md\">\nParent rules.\n</project_instructions>"),
		section("skills", "The following skills provide specialized instructions for specific tasks.\n\n<available_skills>\n  <skill>\n    <name>deploy</name>\n    <description>Deploy the application.</description>\n  </skill>\n</available_skills>"),
		section("cwd", "/parent"),
	}
	parentPrompt := strings.Join(parentSections, "\n\n")
	strippedParent := strings.Join(parentSections[:5], "\n\n")
	childWrapper := "<active_agent name=\"Explore\"/>\n\n# Environment\nWorking directory: /child\nGit repository: yes\nBranch: main\nPlatform: darwin"
	strippedChild := strippedParent + "\n\n" + childWrapper

	recordParent := func(b *Bridge, host *fakeHost) {
		b.OnBeforeAgentStart(host, parentPrompt, map[string]any{})
		host.setPrompt(parentPrompt)
		b.OnAgentStart(host)
	}

	t.Run("makes isolated subagent captures resolve via the shared registry (#64)", func(t *testing.T) {
		// The TypeScript re-evaluates the module for an isolated agent and checks both
		// instances share one process-wide registry. A Bridge owns its one registry, so
		// the equivalent is a second session's records landing in the registry the
		// provider path resolves against.
		b, _, _ := newTestBridge(t, false)
		recordParent(b, &fakeHost{})
		isolated := "You are an isolated smoke-test agent. Respond with ZZ_ISO_OK."
		child := &fakeHost{sessionID: "child"}
		b.OnBeforeAgentStart(child, isolated, map[string]any{})
		child.setPrompt(isolated)
		b.OnAgentStart(child)
		if _, ok := b.captures.Resolve(isolated); !ok {
			t.Fatal("the child recorded its own prompt")
		}
		if c, err := b.captures.ResolveOrDerive(isolated); err != nil || c == nil {
			t.Fatalf("the registry the stream resolves against resolves the child's prompt: %v", err)
		}
	})

	t.Run("does not match a child embedding a tail-stripped parent prompt (#88)", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		recordParent(b, &fakeHost{})
		// gotgenes/pi-subagents inheritedIdentity embeds the parent minus the per-session tail.
		mustNotResolve(t, b, strippedChild, "the full assembled prompt is not a substring of its tail-stripped embedding")
	})

	t.Run("fails loudly when a recorded tail-stripped child has no inheritance edge (#88)", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		recordParent(b, host)
		// The child is recorded at both production capture boundaries, but its custom
		// prompt contains only the tail-stripped parent shape, so no edge is matched.
		b.OnBeforeAgentStart(host, strippedChild, map[string]any{"customPrompt": strippedChild})
		host.setPrompt(strippedChild)
		b.OnAgentStart(host)

		capture, ok := b.captures.Resolve(strippedChild)
		if !ok {
			t.Fatal("the tail-stripped child must be recorded before projection")
		}
		if len(capture.Inherited) != 0 {
			t.Fatal("the unsupported embedding must not invent an inheritance edge")
		}
		if _, err := promptcapture.ProjectPromptCapture(capture, "none", config.DebugLogPath(t.TempDir())); err == nil || !strings.Contains(err.Error(), "prompt-capture: refusing to send this prompt") {
			t.Fatalf("an unmatched recorded child must fail before its harness reaches Claude Code: %v", err)
		}
	})

	t.Run("does capture a handler-returned wholesale replacement: it resolves via the agent_start key", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		b.OnBeforeAgentStart(host, "pi rendered prompt", map[string]any{})
		// A before_agent_start handler that returns a system prompt forces it as the
		// request head, and pi renders the host's system prompt as exactly that text.
		const forced = "forced replacement prompt owning the request head"
		host.setPrompt(forced)
		b.OnAgentStart(host)
		if _, ok := b.captures.Resolve(forced); !ok {
			t.Fatal("the forced text becomes a capture key at agent_start")
		}
		// Caveat pinned by design: only the portable parts are projected for Claude Code.
	})

	t.Run("does not rescue a prompt composed outside the before_agent_start pipeline (#102 shape)", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		b.OnBeforeAgentStart(host, "pi rendered prompt", map[string]any{})
		host.setPrompt("pi rendered prompt")
		b.OnAgentStart(host)
		mustNotResolve(t, b, "host-composed prompt owning the request head", "out-of-pipeline composition is neither a recorded key nor an embedding of one")
	})

	t.Run("does not see a prompt replaced between turn_start and the stream call (#91 remaining shape)", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{}
		b.OnBeforeAgentStart(host, "turn-1 prompt", map[string]any{})
		host.setPrompt("turn-1 prompt")
		b.OnAgentStart(host)
		host.setPrompt("turn-2 rendered prompt")
		b.OnTurnStart(host)
		if c, err := b.captures.ResolveOrDerive("turn-2 rendered prompt"); err != nil || c == nil {
			t.Fatalf("the turn_start record resolves: %v", err)
		}
		mustNotResolve(t, b, "replacement head installed by a context handler", "post-turn_start rewrites are seen by no recording boundary")
	})
}

// --- unit-compaction-coexistence.mjs ---

// The TypeScript drives a real pi session, extension runner and native summarizer;
// PiG has no counterpart in this test, so the adaptation asserts the same
// contract at the bridge's boundary: the bridge never starts a provider query for
// a summary, never owns the compaction (its handlers return nothing), and records
// the rewrite only when a compaction completed.
func TestBridgeLeavesCompactionAndBranchSummaryOwnershipToPiAndOtherExtensions(t *testing.T) {
	t.Run("the before-compact and before-tree hooks only remember the session", func(t *testing.T) {
		h := newProviderHarness(t, nil)
		host := &fakeHost{sessionID: "coexistence"}
		h.b.OnSessionBeforeCompact(host)
		if id, ok := h.b.summarySessionFor(); !ok || id != "coexistence" {
			t.Fatalf("summary session = %q %t", id, ok)
		}
		h.b.OnSessionBeforeTree(&fakeHost{sessionID: "tree"})
		if id, _ := h.b.summarySessionFor(); id != "tree" {
			t.Fatalf("summary session = %q", id)
		}
		h.b.OnSessionShutdown(host)
		if len(h.spawns()) != 0 {
			t.Fatal("no process may start")
		}
	})

	owners := []struct {
		name string
		// summaries is how many one-off summarizer calls the compaction makes; completed
		// says whether pi then observes session_compact.
		summaries int
		completed bool
	}{
		{"extension", 0, true},
		{"native", 2, true},
		{"declining-extension", 2, true},
		{"cancel", 0, false},
		{"error", 1, false},
	}
	for _, o := range owners {
		t.Run("compaction owner: "+o.name, func(t *testing.T) {
			h := newProviderHarness(t, nil)
			host := &fakeHost{sessionID: "pi-compaction"}
			h.b.OnSessionBeforeCompact(host)
			for i := 0; i < o.summaries; i++ {
				stream, err := h.b.StreamSimple(context.Background(), host, testModel,
					map[string]any{"systemPrompt": "summarization prompt", "messages": msgs(userMsg("summarize"))},
					map[string]any{"sessionId": "pi-compaction", "cacheRetention": "none"})
				if err != nil {
					t.Fatal(err)
				}
				finalOf(t, stream)
			}
			if o.completed {
				h.b.OnSessionCompact(host)
			}
			if len(h.spawns()) != 0 {
				t.Fatalf("spawns = %d, a summary never goes through the provider query", len(h.spawns()))
			}
			if got := h.b.historyRewrittenFor("pi-compaction"); got != o.completed {
				t.Fatalf("history rewrite recorded = %t, want %t", got, o.completed)
			}
			_, pending := h.b.summarySessionFor()
			if pending == o.completed {
				t.Fatalf("summary session pending = %t after a compaction that completed = %t", pending, o.completed)
			}
		})
	}
}

// --- the event hooks ---

func TestEventHooks(t *testing.T) {
	t.Run("session_start remembers the session's cwd", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		host := &fakeHost{sessionID: "s1", cwd: "/agents/Martin"}
		b.OnSessionStart(host, "startup")
		if got := b.sessionCwdFor("s1"); got != "/agents/Martin" {
			t.Fatalf("cwd = %q", got)
		}
	})
	t.Run("a new, resumed or forked session drops every mirror; a startup does not", func(t *testing.T) {
		for reason, dropped := range map[string]bool{"new": true, "resume": true, "fork": true, "startup": false, "reload": false} {
			b, _, _ := newTestBridge(t, false)
			b.setSessionStateFor("pi-a", &SessionState{SessionID: "cc-a"})
			b.markRebuildForSession("pi-a", "test")
			b.OnSessionStart(&fakeHost{sessionID: "pi-b"}, reason)
			if gone := b.sessionStateFor("pi-a") == nil && !b.historyRewrittenFor("pi-a"); gone != dropped {
				t.Fatalf("%s: dropped = %t, want %t", reason, gone, dropped)
			}
		}
	})
	t.Run("session_compact and session_tree arm the rebuild for the rewriting session only", func(t *testing.T) {
		for _, fire := range []func(*Bridge, Host){(*Bridge).OnSessionCompact, (*Bridge).OnSessionTree} {
			b, _, _ := newTestBridge(t, false)
			b.setSessionStateFor("pi-a", &SessionState{SessionID: "cc-a"})
			b.setSessionStateFor("pi-b", &SessionState{SessionID: "cc-b"})
			b.rememberSummarySession("pi-a")
			fire(b, &fakeHost{sessionID: "pi-a"})
			if !b.sessionStateFor("pi-a").NeedsRebuild || b.sessionStateFor("pi-b").NeedsRebuild || !b.historyRewrittenFor("pi-a") {
				t.Fatal("only the compacting session is marked")
			}
			if _, pending := b.summarySessionFor(); pending {
				t.Fatal("the summary operation is over")
			}
		}
	})
	t.Run("session_compact arms a parked query so its tool result is discarded", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q := b.addContext("pi-a")
		b.OnSessionCompact(&fakeHost{sessionID: "pi-a"})
		if !b.isStale(q) {
			t.Fatal("the parked query must be armed")
		}
	})
	t.Run("session_shutdown forgets the cwd and sync lock, clears mirrors and names a leak", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		host := &fakeHost{sessionID: "s9", cwd: "/x"}
		b.OnSessionStart(host, "startup")
		b.setSessionStateFor("s9", &SessionState{SessionID: "cc"})
		b.lockSync("s9")()
		b.addContext("s9")
		b.OnSessionShutdown(host)
		if got := b.sessionCwdFor("s9"); got == "/x" {
			t.Fatal("the cwd must be forgotten")
		}
		b.mu.Lock()
		_, lock := b.syncLocks["s9"]
		b.mu.Unlock()
		if lock || b.sessionStateFor("s9") != nil {
			t.Fatal("lock entry and mirror must be gone")
		}
		if log := readDebugLog(t, agentDir); !strings.Contains(log, "WARNING: session_shutdown left state behind — contexts=1 pendingToolCalls=0 promptStreams=0") {
			t.Fatalf("log:\n%s", log)
		}
	})
	t.Run("a clean shutdown reports no leak", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		b.OnSessionShutdown(&fakeHost{sessionID: "clean"})
		if log := readDebugLog(t, agentDir); strings.Contains(log, "left state behind") {
			t.Fatalf("log:\n%s", log)
		}
	})
	t.Run("a panicking host never escapes an entry point", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		host := explodingHost{}
		b.OnSessionStart(host, "new")
		b.OnBeforeAgentStart(host, "p", nil)
		b.OnAgentStart(host)
		b.OnTurnStart(host)
		b.OnSessionBeforeCompact(host)
		b.OnSessionBeforeTree(host)
		b.OnSessionCompact(host)
		b.OnSessionTree(host)
		b.OnSessionShutdown(host)
		if log := readDebugLog(t, agentDir); strings.Count(log, "recovered from panic: boom") < 7 {
			t.Fatalf("log:\n%s", log)
		}
	})
}

// explodingHost panics on every call.
type explodingHost struct{}

func (explodingHost) Notify(string, string)         { panic("boom") }
func (explodingHost) SessionID() (string, error)    { panic("boom") }
func (explodingHost) Cwd() string                   { panic("boom") }
func (explodingHost) AgentDir() string              { panic("boom") }
func (explodingHost) ConfigDirName() string         { panic("boom") }
func (explodingHost) SystemPrompt() (string, error) { panic("boom") }
