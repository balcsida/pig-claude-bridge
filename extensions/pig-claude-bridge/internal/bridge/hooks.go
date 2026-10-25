package bridge

import (
	"fmt"
	"runtime/debug"
	"slices"
	"sort"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/promptcapture"
)

// Event entry points the extension forwards from PiG (index.ts default export).
// Each takes the per-call host and never lets a panic reach it.

// promptOptions are the pi prompt options stashed at before_agent_start. They are
// pi config, stable across a turn; only the auto-generated tool list in the
// rendered prompt varies, so the later boundaries reuse them.
type promptOptions struct {
	input promptcapture.Input
	// selectedTools is nil when the options named none (every tool counts as selected).
	selectedTools []string
	hasSelected   bool
}

// parsePromptOptions reads systemPromptOptions. Sections arrive ordered
// ([]promptcapture.Section) or as a map, which is taken in name order.
func parsePromptOptions(options map[string]any) *promptOptions {
	if options == nil {
		return nil
	}
	p := &promptOptions{input: promptcapture.Input{Custom: str(options["customPrompt"]), Append: str(options["appendSystemPrompt"])}}
	switch files := options["contextFiles"].(type) {
	case []promptcapture.ContextFile:
		p.input.ContextFiles = files
	case []any:
		for _, raw := range files {
			f := obj(raw)
			p.input.ContextFiles = append(p.input.ContextFiles, promptcapture.ContextFile{Path: str(f["path"]), Content: str(f["content"])})
		}
	}
	switch skills := options["skills"].(type) {
	case []map[string]any:
		p.input.Skills = skills
	case []any:
		for _, raw := range skills {
			if s := obj(raw); s != nil {
				p.input.Skills = append(p.input.Skills, s)
			}
		}
	}
	switch sections := options["sections"].(type) {
	case []promptcapture.Section:
		p.input.Sections = sections
	case map[string]string:
		names := make([]string, 0, len(sections))
		for name := range sections {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			p.input.Sections = append(p.input.Sections, promptcapture.Section{Name: name, Text: sections[name]})
		}
	case map[string]any:
		for _, name := range sortedKeys(sections) {
			p.input.Sections = append(p.input.Sections, promptcapture.Section{Name: name, Text: str(sections[name])})
		}
	}
	switch tools := options["selectedTools"].(type) {
	case []string:
		p.selectedTools, p.hasSelected = tools, true
	case []any:
		p.hasSelected = true
		for _, t := range tools {
			p.selectedTools = append(p.selectedTools, str(t))
		}
	}
	return p
}

// recordSystemPrompt keys the prompt pi rendered, with the stashed options, at one
// capture boundary.
func (b *Bridge) recordSystemPrompt(source, systemPrompt string, options *promptOptions) {
	if systemPrompt == "" {
		return
	}
	var in promptcapture.Input
	if options != nil {
		in = options.input
		// Skills are read through the read tool; with it deselected there is no way to load them.
		if options.hasSelected && !slices.Contains(options.selectedTools, "read") {
			in.Skills = nil
		}
	}
	b.captures.Record(systemPrompt, in, source)
}

func (b *Bridge) stashedPromptOptions() *promptOptions {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastPromptOptions
}

// recoverHandler contains a panic from an event handler: it becomes a debug line.
func (b *Bridge) recoverHandler(name string) {
	if r := recover(); r != nil {
		b.debugf("%s: recovered from panic: %v\n%s", name, r, debug.Stack())
	}
}

// hostSessionID is the pi session the host serves, "" when it cannot say.
func (b *Bridge) hostSessionID(host Host, event string) string {
	id, err := host.SessionID()
	if err != nil {
		b.debugf("%s: cannot read the session id: %v", event, err)
	}
	return id
}

// OnSessionStart remembers the session's cwd, and drops every mirror when the
// session is a fresh conversation.
func (b *Bridge) OnSessionStart(host Host, reason string) {
	defer b.recoverHandler("session_start")
	b.rememberSessionCwd(b.hostSessionID(host, "session_start"), host.Cwd())
	switch reason {
	case "new", "resume", "fork":
		b.clearSessions("session_start:" + reason)
	}
}

// OnSessionBeforeCompact marks the session whose compaction summary runs next.
func (b *Bridge) OnSessionBeforeCompact(host Host) {
	defer b.recoverHandler("session_before_compact")
	b.rememberSummarySession(b.hostSessionID(host, "session_before_compact"))
}

// OnSessionBeforeTree marks the session whose branch summary runs next.
func (b *Bridge) OnSessionBeforeTree(host Host) {
	defer b.recoverHandler("session_before_tree")
	b.rememberSummarySession(b.hostSessionID(host, "session_before_tree"))
}

// OnBeforeAgentStart records the rendered prompt and stashes the options it was
// rendered from. systemPromptOptions carries custom, append, contextFiles, skills,
// sections and selectedTools.
func (b *Bridge) OnBeforeAgentStart(host Host, systemPrompt string, options map[string]any) {
	defer b.recoverHandler("before_agent_start")
	parsed := parsePromptOptions(options)
	b.mu.Lock()
	b.lastPromptOptions = parsed
	b.mu.Unlock()
	b.recordSystemPrompt("before_agent_start", systemPrompt, parsed)
}

// OnAgentStart records the fully widened prompt: MCP tool descriptions merge into
// it only after their servers connect, which is after before_agent_start. A
// subagent embeds that widened prompt verbatim, so it must be a capture key too.
func (b *Bridge) OnAgentStart(host Host) {
	defer b.recoverHandler("agent_start")
	b.recordHostPrompt(host, "agent_start")
}

// OnTurnStart re-keys the prompt at each turn: it fires after the turn's prompt is
// final, so a mid-run re-render keeps exact-match alive. The stashed options can lag
// a mid-run tool-loadout change; a stale skills list beats failing the turn.
func (b *Bridge) OnTurnStart(host Host) {
	defer b.recoverHandler("turn_start")
	b.recordHostPrompt(host, "turn_start")
}

func (b *Bridge) recordHostPrompt(host Host, source string) {
	prompt, err := host.SystemPrompt()
	if err != nil {
		b.debugf("%s: cannot read the system prompt: %v", source, err)
		return
	}
	b.recordSystemPrompt(source, prompt, b.stashedPromptOptions())
}

// OnSessionCompact and OnSessionTree: pi mutated its messages out from under the
// bridge. The next sync must REBUILD rather than REUSE, and a query parked at a tool
// boundary is discarded when its tool result arrives (the mark arms it). The
// rewrite belongs to the compacting session; others stay untouched. tag is the
// debug-log prefix, session_compact:<reason>:willRetry=<bool> as the event reports it.
func (b *Bridge) OnSessionCompact(host Host, tag string) {
	defer b.recoverHandler("session_compact")
	b.historyRewrittenBy(host, tag)
}

func (b *Bridge) OnSessionTree(host Host) {
	defer b.recoverHandler("session_tree")
	b.historyRewrittenBy(host, "session_tree")
}

func (b *Bridge) historyRewrittenBy(host Host, event string) {
	id := b.hostSessionID(host, event)
	b.markRebuildForSession(id, event)
	b.forgetSummarySession(id)
}

// OnSessionShutdown ends a pi session: its cwd, its sync lock and every mirror go.
func (b *Bridge) OnSessionShutdown(host Host) {
	defer b.recoverHandler("session_shutdown")
	id := b.hostSessionID(host, "session_shutdown")
	b.forgetSessionCwd(id)
	b.mu.Lock()
	delete(b.syncLocks, sessionKey(id))
	b.mu.Unlock()
	b.reportLeaks("session_shutdown")
	b.clearSessions("session_shutdown")
}

// clearSessions drops every mirror: children never emit session_shutdown (only
// runtime teardown does), so there is no per-entry removal, and a top-level
// transition takes every mirror with it.
func (b *Bridge) clearSessions(event string) {
	b.mu.Lock()
	n := len(b.sessions)
	b.sessions = map[string]SessionState{}
	b.historyRewritten = map[string]struct{}{}
	b.mu.Unlock()
	plural := "s"
	if n == 1 {
		plural = ""
	}
	b.debugf("%s", fmt.Sprintf("%s: clearing %d shared session%s", event, n, plural))
}
