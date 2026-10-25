// Package promptcapture keeps what Pi assembled for one agent so the bridge can
// append only the portable parts after Claude Code's own preset.
package promptcapture

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// PiPreamble is Pi's own preamble, the first section of every prompt Pi renders for
// a session without a custom prompt. Machine-generated, so operator text never
// carries it; forwarding it makes Claude Code's subscription path read the request
// as a third-party app.
const PiPreamble = "You are an expert coding assistant operating inside pi"

// PigHarnessTexts lists PiG's harness sentences that PiPreamble does not already
// match. PiG's preamble ("... operating inside pig, a coding agent harness.",
// internal/codingagent/prompts/coding.go) starts with PiPreamble, and its docs
// section names the same docs/custom-provider.md and docs/packages.md, so it is empty.
var PigHarnessTexts []string

// anthropicThirdPartyTriggers are both doc paths from Pi's documentation-routing
// line. Anthropic's subscription gate rejects a system prompt carrying both, while
// either alone passes (issues #883, #88).
var anthropicThirdPartyTriggers = []string{"docs/custom-provider.md", "docs/packages.md"}

// defaultLimit bounds the lookup keys. It is set well above any plausible working
// set because the costs are lopsided: a capture is tens of KB, while evicting one
// that is still live fails the turn. The bound exists only to cap an extension that
// rebuilds the prompt every turn.
const defaultLimit = 256

// ContextFile is one context file Pi loaded.
type ContextFile struct {
	Path    string
	Content string
}

// Section is a custom prompt section from systemPromptOptions.sections; Pi renders
// it as "<name>\ncontent\n</name>" after the built-in sections.
type Section struct {
	Name string
	Text string
}

// Input is what the bridge observed at one hook boundary.
type Input struct {
	Custom       string
	Append       string
	ContextFiles []ContextFile
	// Skills are Pi skill records: name, description, filePath, disableModelInvocation.
	Skills []map[string]any
	// Sections keep the SDK's insertion order.
	Sections []Section
}

// InheritedPrompt is an exact previously assembled prompt embedded in Custom at [Start, End).
type InheritedPrompt struct {
	Start, End int
	Parent     *Capture
}

// Capture is one recorded prompt with its components.
type Capture struct {
	Input
	AssembledPrompt string
	// Source is the bridge boundary that last recorded this key (before_agent_start | agent_start | turn_start).
	Source    string
	Inherited []InheritedPrompt
}

// Match is one known key sharing a prefix with an unresolved prompt.
type Match struct {
	Key            string
	FirstDivergent int
	Source         string
}

// Diagnostic is everything an unresolved prompt would otherwise lose: a
// fingerprint plus the closest matches' first divergent offsets.
type Diagnostic struct {
	SystemPrompt string
	Matches      []Match
}

// Captures holds captures keyed by the fully assembled prompt Pi sends to a
// provider. A sub-agent's override embeds its parent's assembled prompt verbatim;
// linking exact prior keys recovers the inheritance graph without recognising Pi prose.
type Captures struct {
	mu         sync.Mutex
	byKey      map[string]*Capture
	order      []string // oldest use first
	limit      int
	onDiagnose func(Diagnostic)
}

// NewCaptures returns a registry retaining limit lookup keys (256 when limit <= 0).
// onDiagnose receives the data of a failed resolution; it may be nil.
func NewCaptures(limit int, onDiagnose func(Diagnostic)) *Captures {
	if limit <= 0 {
		limit = defaultLimit
	}
	if onDiagnose == nil {
		onDiagnose = func(Diagnostic) {}
	}
	return &Captures{byKey: map[string]*Capture{}, limit: limit, onDiagnose: onDiagnose}
}

// Size is the number of retained lookup keys.
func (c *Captures) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byKey)
}

// Record stores in under systemPrompt, updating an existing node in place so
// descendants keep a live reference.
func (c *Captures) Record(systemPrompt string, in Input, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing := c.byKey[systemPrompt]
	capture := existing
	if capture == nil {
		capture = &Capture{AssembledPrompt: systemPrompt}
	}
	customChanged := existing == nil || existing.Custom != in.Custom

	capture.Custom = in.Custom
	capture.Append = in.Append
	capture.ContextFiles = append([]ContextFile(nil), in.ContextFiles...)
	capture.Skills = append([]map[string]any(nil), in.Skills...)
	// Copied, not referenced: the caller's options are a live object that later handlers mutate.
	capture.Sections = append([]Section(nil), in.Sections...)
	capture.Source = source
	if customChanged {
		capture.Inherited = c.findInheritedPrompts(systemPrompt, in.Custom)
	}
	c.touch(systemPrompt, capture)
}

// Resolve is an exact lookup only. Callers serving a query want ResolveOrDerive.
func (c *Captures) Resolve(systemPrompt string) (*Capture, bool) {
	if systemPrompt == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	capture, ok := c.byKey[systemPrompt]
	if ok {
		c.touch(systemPrompt, capture)
	}
	return capture, ok
}

// touch makes the key the most recently used and trims to the limit. Recency is by
// use, not just by record: a parent records once and then only resolves.
// Trimming here covers revived keys that were not in the map. Callers hold c.mu.
func (c *Captures) touch(systemPrompt string, capture *Capture) {
	if _, ok := c.byKey[systemPrompt]; ok {
		for i, k := range c.order {
			if k == systemPrompt {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.byKey[systemPrompt] = capture
	c.order = append(c.order, systemPrompt)
	for len(c.byKey) > c.limit {
		delete(c.byKey, c.order[0])
		c.order = c.order[1:]
	}
}

// ResolveOrDerive returns the capture to project for one query. An exact key is the
// normal case. A prompt that only embeds known prompts resolves to a transient
// descendant over the whole prompt, so the text around the embedded captures is
// carried through. It returns (nil, nil) for an empty prompt and an error when a
// prompt can be accounted for by neither route: handing Claude Code a turn with
// none of the user's context would silently discard policy the user wrote down.
func (c *Captures) ResolveOrDerive(systemPrompt string) (*Capture, error) {
	if systemPrompt == "" {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if exact, ok := c.byKey[systemPrompt]; ok {
		c.touch(systemPrompt, exact)
		return exact, nil
	}

	// A capture outlives its lookup key: eviction drops the key while inheritance
	// edges keep the node alive; touching it puts the key back.
	for _, node := range c.reachableCaptures() {
		if node.AssembledPrompt == systemPrompt {
			c.touch(systemPrompt, node)
			return node, nil
		}
	}

	// Inheritance is tried before any adoption route: a child that embeds its
	// parent's prompt verbatim would otherwise be re-keyed onto the parent's capture,
	// dropping the child's wrapper text.
	embedded := c.findInheritedPrompts(systemPrompt, systemPrompt)
	if len(embedded) == 0 {
		matches := c.closestKnown(systemPrompt)
		c.onDiagnose(Diagnostic{SystemPrompt: systemPrompt, Matches: matches})
		// A query that is a recorded key minus its tail is the signature of pi#5581:
		// an extension-triggered idle turn skips before_agent_start.
		for _, m := range matches {
			if m.FirstDivergent == len(systemPrompt) && len(m.Key) > len(systemPrompt) {
				source := m.Source
				if source == "" {
					source = "an unknown boundary"
				}
				return nil, fmt.Errorf("prompt-capture: this %d-char prompt is the %d-char capture recorded at "+
					"%s with its final %d chars missing. "+
					"That shape matches pi#5581: an extension-triggered idle turn (sendMessage with triggerTurn) skips before_agent_start, "+
					"so the prompt lacks that turn's extension additions. The turn fails; the next user-typed turn resolves normally. "+
					"Workaround: when idle, send a user message instead of triggerTurn.",
					len(systemPrompt), len(m.Key), source, len(m.Key)-len(systemPrompt))
			}
		}
		divergent, keyLen, recorded := "?", 0, ""
		if len(matches) > 0 {
			divergent = fmt.Sprint(matches[0].FirstDivergent)
			keyLen = len(matches[0].Key)
			if matches[0].Source != "" {
				recorded = ", last recorded at " + matches[0].Source
			}
		}
		return nil, fmt.Errorf("prompt-capture: no capture for this %d-char system prompt, and it embeds none of the %d known. "+
			"Closest known match diverges at offset %s (%d-char key%s). "+
			"Claude Code would receive none of this turn's context files, skills or custom instructions. "+
			"When the divergence sits at a section's opening tag, the capture and this prompt disagree about that section — "+
			"pi's mcp_servers since pi 0.99.2, or one an extension added. "+
			"Otherwise an extension loaded after claude-bridge rewrote the system prompt from before_agent_start — "+
			"one that wraps it is fine, one that rebuilds or strips it leaves nothing to match — "+
			"or pi rebuilt the prompt outside before_agent_start (a late-registered tool or fresh resource discovery).",
			len(systemPrompt), len(c.byKey), divergent, keyLen, recorded)
	}

	// Custom is the prompt itself and the edges keep their original offsets, so
	// projectCustom substitutes the embedded captures in place.
	return &Capture{AssembledPrompt: systemPrompt, Input: Input{Custom: systemPrompt}, Inherited: embedded}, nil
}

// closestKnown returns the longest shared-prefix matches, best first.
func (c *Captures) closestKnown(systemPrompt string) []Match {
	shared := 0
	var matches []Match
	for _, key := range c.order {
		limit := min(len(key), len(systemPrompt))
		i := 0
		for i < limit && key[i] == systemPrompt[i] {
			i++
		}
		if i >= shared {
			if i > shared {
				shared = i
				matches = matches[:0]
			}
			matches = append(matches, Match{Key: key, FirstDivergent: i, Source: c.byKey[key].Source})
		}
	}
	return matches
}

func (c *Captures) findInheritedPrompts(systemPrompt, custom string) []InheritedPrompt {
	if custom == "" {
		return nil
	}
	type candidate struct {
		InheritedPrompt
		length int
	}
	var candidates []candidate
	for _, parent := range c.reachableCaptures() {
		key := parent.AssembledPrompt
		if key == systemPrompt || key == "" {
			continue
		}
		for from := 0; from <= len(custom); {
			i := strings.Index(custom[from:], key)
			if i < 0 {
				break
			}
			start := from + i
			candidates = append(candidates, candidate{InheritedPrompt{start, start + len(key), parent}, len(key)})
			from = start + len(key)
		}
	}

	// A grandchild contains both its parent's key and the grandparent key nested
	// inside it. Keep the longest exact non-overlapping matches.
	sortStable(candidates, func(a, b candidate) bool {
		if a.length != b.length {
			return a.length > b.length
		}
		return a.Start < b.Start
	})
	var selected []InheritedPrompt
	for _, cand := range candidates {
		overlaps := false
		for _, edge := range selected {
			if cand.Start < edge.End && cand.End > edge.Start {
				overlaps = true
				break
			}
		}
		if !overlaps {
			selected = append(selected, cand.InheritedPrompt)
		}
	}
	sortStable(selected, func(a, b InheritedPrompt) bool { return a.Start < b.Start })
	return selected
}

// reachableCaptures walks every retained capture and its ancestors, once each.
func (c *Captures) reachableCaptures() []*Capture {
	var result []*Capture
	seen := map[*Capture]bool{}
	var visit func(*Capture)
	visit = func(capture *Capture) {
		if seen[capture] {
			return
		}
		seen[capture] = true
		result = append(result, capture)
		for _, edge := range capture.Inherited {
			visit(edge.Parent)
		}
	}
	for _, key := range c.order {
		visit(c.byKey[key])
	}
	return result
}

// sortStable is an insertion sort: candidate lists are tiny and it is stable.
func sortStable[T any](s []T, less func(a, b T) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// CollectPromptSkills returns the skills visible through inherited prompts,
// ancestor first and once per file.
func CollectPromptSkills(capture *Capture) []map[string]any {
	var result []map[string]any
	seenPaths := map[string]bool{}
	visited := map[*Capture]bool{}
	var visit func(*Capture)
	visit = func(node *Capture) {
		if visited[node] {
			return
		}
		visited[node] = true
		for _, edge := range node.Inherited {
			visit(edge.Parent)
		}
		for _, skill := range node.Skills {
			path, _ := skill["filePath"].(string)
			if skillHidden(skill) || seenPaths[path] {
				continue
			}
			seenPaths[path] = true
			result = append(result, skill)
		}
	}
	visit(capture)
	return result
}

func skillHidden(skill map[string]any) bool {
	hidden, _ := skill["disableModelInvocation"].(bool)
	return hidden
}

// promptPart is one piece of the append, named so a refusal can say where it found the text.
type promptPart struct{ label, text string }

// ProjectPromptCapture builds the append text for Claude Code from capture.
// readTool is the skills read-tool mode (mcp, native, none). debugLogPath is named
// in the refusal text. The text is empty when there is nothing to append; err
// carries the refusal for Pi's harness text or both documentation paths.
func ProjectPromptCapture(capture *Capture, readTool string, debugLogPath string) (appendText string, err error) {
	return projectCapture(capture, readTool, debugLogPath, map[*Capture]bool{})
}

var errCyclic = errors.New("Cyclic prompt inheritance")

func projectCapture(capture *Capture, readTool, debugLogPath string, visiting map[*Capture]bool) (string, error) {
	if visiting[capture] {
		return "", errCyclic
	}
	visiting[capture] = true
	defer delete(visiting, capture)

	inheritedSkillPaths := map[string]bool{}
	for _, edge := range capture.Inherited {
		for _, skill := range CollectPromptSkills(edge.Parent) {
			path, _ := skill["filePath"].(string)
			inheritedSkillPaths[path] = true
		}
	}
	ownSkillPaths := map[string]bool{}
	var ownSkills []map[string]any
	for _, skill := range capture.Skills {
		path, _ := skill["filePath"].(string)
		if skillHidden(skill) || inheritedSkillPaths[path] || ownSkillPaths[path] {
			continue
		}
		ownSkillPaths[path] = true
		ownSkills = append(ownSkills, skill)
	}

	custom, err := projectCustom(capture, readTool, debugLogPath, visiting)
	if err != nil {
		return "", err
	}
	var parts []promptPart
	files := make([]map[string]any, len(capture.ContextFiles))
	for i, f := range capture.ContextFiles {
		files[i] = map[string]any{"path": f.Path, "content": f.Content}
	}
	if context, ok := convert.FormatProjectContext(files); ok {
		parts = append(parts, promptPart{"the project context block", context})
	}
	if skills, ok := convert.RenderSkillsBlock(ownSkills, readTool); ok {
		parts = append(parts, promptPart{"the skills block", skills})
	}
	if custom != "" {
		parts = append(parts, promptPart{"the custom prompt", custom})
	}
	if capture.Append != "" {
		parts = append(parts, promptPart{"the appended instructions", capture.Append})
	}
	// Pi's builder renders custom sections last, after cwd — the one built-in section
	// with nothing portable to forward — so they follow the append here.
	for _, s := range capture.Sections {
		if s.Text == "" {
			continue
		}
		parts = append(parts, promptPart{"the " + s.Name + " section", "<" + s.Name + ">\n" + s.Text + "\n</" + s.Name + ">"})
	}
	if err := assertSendablePrompt(parts, capture, debugLogPath); err != nil {
		return "", err
	}
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.text
	}
	return strings.Join(texts, "\n\n"), nil
}

func assertSendablePrompt(parts []promptPart, capture *Capture, debugLogPath string) error {
	var findings []string
	for _, p := range parts {
		for _, marker := range append([]string{PiPreamble}, PigHarnessTexts...) {
			if offset := harnessAtLineStart(p.text, marker); offset != -1 {
				findings = append(findings, fmt.Sprintf("pi's preamble (\"%s\") in %s, at offset %d of %d chars", marker, p.label, offset, len(p.text)))
			}
		}
		// The pair has to co-occur in one part; the two phrases split across parts are not detected.
		both := true
		for _, trigger := range anthropicThirdPartyTriggers {
			both = both && strings.Contains(p.text, trigger)
		}
		if both {
			findings = append(findings, strings.Join(anthropicThirdPartyTriggers, " and ")+" in "+p.label)
		}
	}
	if len(findings) == 0 {
		return nil
	}
	source := capture.Source
	if source == "" {
		source = "unknown"
	}
	lines := []string{
		"prompt-capture: refusing to send this prompt. Claude Code's Anthropic path reads a request",
		"  carrying pi's harness, or the phrase pair its subscription gate rejects, as a third-party",
		"  app: it fails with 400 or is billed as extra usage.",
	}
	for _, f := range findings {
		lines = append(lines, "  Found: "+f+".")
	}
	lines = append(lines,
		fmt.Sprintf("  Capture: %s, %d inherited capture(s) substituted.", source, len(capture.Inherited)),
		"  If this came from an inherited pi prompt, see README \"Compatibility with other extensions\".",
		"  If it is your own text, reword or remove it. CLAUDE_BRIDGE_DEBUG=1 writes the full prompt to",
		"  "+debugLogPath+".")
	return errors.New(strings.Join(lines, "\n"))
}

// harnessAtLineStart is the offset of marker at the start of a line, or -1.
// Mid-line mentions are someone describing Pi's prompt, not Pi's prompt.
func harnessAtLineStart(text, marker string) int {
	for from := 0; from <= len(text); {
		i := strings.Index(text[from:], marker)
		if i < 0 {
			return -1
		}
		offset := from + i
		if offset == 0 || text[offset-1] == '\n' {
			return offset
		}
		from = offset + len(marker)
	}
	return -1
}

func projectCustom(capture *Capture, readTool, debugLogPath string, visiting map[*Capture]bool) (string, error) {
	if capture.Custom == "" || len(capture.Inherited) == 0 {
		return capture.Custom, nil
	}
	var result strings.Builder
	cursor := 0
	for _, edge := range capture.Inherited {
		result.WriteString(capture.Custom[cursor:edge.Start])
		projected, err := projectCapture(edge.Parent, readTool, debugLogPath, visiting)
		if err != nil {
			return "", err
		}
		result.WriteString(projected)
		cursor = edge.End
	}
	result.WriteString(capture.Custom[cursor:])
	return result.String(), nil
}
