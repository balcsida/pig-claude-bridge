package promptcapture

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	piHarness   = "You are an expert coding assistant operating inside pi. Pi documentation: pi packages (docs/packages.md)."
	parentKey   = piHarness + "\n\n<project_context>raw parent context</project_context>\nCurrent working directory: /parent"
	childSuffix = "\n\n<sub_agent_context>child rules</sub_agent_context>\n\n<active_agent name=\"Plan\"/>\n\n# Environment\nWorking directory: /child\n\n<agent_instructions>plan carefully</agent_instructions>"
	childKey    = parentKey + childSuffix + "\nCurrent working directory: /child"
	mcpContent  = "- mcp__github (codemode): GitHub tools"
	mcpSection  = "<mcp_servers>\n" + mcpContent + "\n</mcp_servers>"
	debugPath   = "/agent/claude-bridge.log"
)

func skill(name string, disabled ...bool) map[string]any {
	return map[string]any{
		"name":                   name,
		"description":            name + " description",
		"filePath":               "/skills/" + name + "/SKILL.md",
		"baseDir":                "/skills/" + name,
		"disableModelInvocation": len(disabled) > 0 && disabled[0],
	}
}

func agents(content string) []ContextFile {
	return []ContextFile{{Path: "/AGENTS.md", Content: content}}
}

func project(t *testing.T, c *Captures, key string) string {
	t.Helper()
	found, ok := c.Resolve(key)
	if !ok {
		t.Fatalf("missing capture for %.30s", key)
	}
	out, err := ProjectPromptCapture(found, "mcp", debugPath)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	return out
}

func mustMatch(t *testing.T, pattern, text string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(text) {
		t.Errorf("want match for %q in %q", pattern, text)
	}
}

func mustNotMatch(t *testing.T, pattern, text string) {
	t.Helper()
	if regexp.MustCompile(pattern).MatchString(text) {
		t.Errorf("want no match for %q in %q", pattern, text)
	}
}

func equal[T comparable](t *testing.T, got, want T, msg string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", msg, got, want)
	}
}

func TestPromptCaptures(t *testing.T) {
	t.Run("keeps parent and child captures isolated", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("parent rules")}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix}, "")

		p, _ := c.Resolve(parentKey)
		equal(t, len(p.ContextFiles), 1, "parent context files")
		ch, _ := c.Resolve(childKey)
		equal(t, len(ch.ContextFiles), 0, "child context files")
		if _, ok := c.Resolve("unknown"); ok {
			t.Error("unknown resolved")
		}
		if _, ok := c.Resolve(""); ok {
			t.Error("empty resolved")
		}
	})

	t.Run("derives a transient capture when a later extension wrapped the prompt", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("parent rules"), Skills: []map[string]any{skill("browser")}}, "")

		wrapped := "PREFIX FROM ANOTHER EXTENSION\n\n" + parentKey + "\n\nSUFFIX"
		derived, err := c.ResolveOrDerive(wrapped)
		if err != nil {
			t.Fatal(err)
		}
		projected, err := ProjectPromptCapture(derived, "mcp", debugPath)
		if err != nil {
			t.Fatal(err)
		}
		mustMatch(t, "parent rules", projected)
		mustMatch(t, "browser", projected)
		mustMatch(t, "PREFIX FROM ANOTHER EXTENSION", projected)
		mustMatch(t, "SUFFIX", projected)
		mustNotMatch(t, "Pi documentation", projected)
		if _, ok := c.Resolve(wrapped); ok {
			t.Error("a derived capture is retained")
		}
	})

	t.Run("revives an exact capture whose lookup key was evicted", func(t *testing.T) {
		c := NewCaptures(2, nil)
		c.Record(parentKey, Input{ContextFiles: agents("parent rules")}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix}, "")
		c.Record("unrelated", Input{}, "")
		if _, ok := c.Resolve(parentKey); ok {
			t.Fatal("precondition: the key is gone")
		}

		revived, err := c.ResolveOrDerive(parentKey)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, revived.ContextFiles[0].Content, "parent rules", "revived content")
		equal(t, c.Size(), 2, "size after revival")
	})

	t.Run("keeps a parent alive that is only ever resolved, never re-recorded", func(t *testing.T) {
		c := NewCaptures(4, nil)
		c.Record(parentKey, Input{ContextFiles: agents("parent rules")}, "")
		for i := 0; i < 8; i++ {
			if _, ok := c.Resolve(parentKey); !ok {
				t.Fatalf("parent evicted after %d sub-agents", i)
			}
			c.Record(fmt.Sprintf("sub-agent prompt %d", i), Input{}, "")
		}
		p, ok := c.Resolve(parentKey)
		if !ok {
			t.Fatal("the parent must survive its own sub-agents")
		}
		equal(t, len(p.ContextFiles), 1, "parent context files")
	})

	t.Run("throws rather than silently dropping instructions it cannot account for", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("parent rules")}, "")

		_, err := c.ResolveOrDerive("a prompt sharing nothing with what we recorded")
		if err == nil {
			t.Fatal("want error")
		}
		mustMatch(t, "no capture for this .* system prompt", err.Error())
		got, err := c.ResolveOrDerive("")
		if got != nil || err != nil {
			t.Errorf("empty prompt: got %v, %v", got, err)
		}
	})

	t.Run("reports the closest known capture when a prompt matches nothing", func(t *testing.T) {
		var diagnostics []Diagnostic
		c := NewCaptures(64, func(d Diagnostic) { diagnostics = append(diagnostics, d) })
		c.Record("prefix-common-THE-REST", Input{}, "")

		_, err := c.ResolveOrDerive("prefix-common-WHO-ARE-YOU")
		if err == nil {
			t.Fatal("want error")
		}
		mustMatch(t, `diverges at offset 14 \(22-char key\)`, err.Error())
		equal(t, len(diagnostics), 1, "diagnostic count")
		equal(t, diagnostics[0].SystemPrompt, "prefix-common-WHO-ARE-YOU", "diagnostic prompt")
		equal(t, diagnostics[0].Matches[0].Key, "prefix-common-THE-REST", "closest key")
		equal(t, diagnostics[0].Matches[0].FirstDivergent, 14, "divergence")
	})

	t.Run("calls the diagnostic callback outside the registry lock", func(t *testing.T) {
		var c *Captures
		c = NewCaptures(0, func(d Diagnostic) {
			c.Record("recorded by the callback", Input{}, "")
			c.Resolve(d.SystemPrompt)
		})
		done := make(chan error, 1)
		go func() {
			_, err := c.ResolveOrDerive("nothing like it")
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("want the no-capture error")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a callback that uses the registry deadlocked")
		}
		if _, ok := c.Resolve("recorded by the callback"); !ok {
			t.Fatal("the callback's Record did not land")
		}
	})

	t.Run("names pi#5581 when a prompt is a recorded key with its tail missing", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey+"\n\n<skills>extension additions</skills>", Input{}, "turn_start")

		_, err := c.ResolveOrDerive(parentKey)
		if err == nil {
			t.Fatal("want error")
		}
		mustMatch(t, "pi#5581", err.Error())
		mustMatch(t, "recorded at turn_start with its final 38 chars missing", err.Error())
	})

	t.Run("recursively projects an inherited prompt without Pi's harness", func(t *testing.T) {
		browser := skill("browser")
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("parent rules"), Skills: []map[string]any{browser}}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix, Skills: []map[string]any{browser}}, "")

		parent := project(t, c, parentKey)
		child := project(t, c, childKey)
		if !strings.HasPrefix(child, parent) {
			t.Error("child should retain the parent's projected cache prefix")
		}
		mustNotMatch(t, "operating inside pi|pi packages", child)
		mustMatch(t, "parent rules", child)
		mustMatch(t, `<sub_agent_context>child rules</sub_agent_context>`, child)
		mustMatch(t, `<active_agent name="Plan"/>`, child)
		mustMatch(t, `<agent_instructions>plan carefully</agent_instructions>`, child)
		equal(t, strings.Count(child, "/skills/browser/SKILL.md"), 1, "browser skill count")
	})

	t.Run("leaves direct custom and replace-mode prompts byte-identical", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("direct assembled", Input{Custom: "  direct user instructions\n"}, "")
		c.Record("replace assembled", Input{Custom: "<active_agent name=\"review\"/>\nreplace instructions"}, "")

		equal(t, project(t, c, "direct assembled"), "  direct user instructions\n", "direct")
		equal(t, project(t, c, "replace assembled"), "<active_agent name=\"review\"/>\nreplace instructions", "replace")
	})

	t.Run("uses the longest inherited key for nested agents", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("parent rules")}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix}, "")
		grandSuffix := "\n\n<sub_agent_context>grandchild rules</sub_agent_context>"
		grandKey := childKey + grandSuffix + "\nCurrent working directory: /grandchild"
		c.Record(grandKey, Input{Custom: childKey + grandSuffix}, "")

		grandchild := project(t, c, grandKey)
		mustNotMatch(t, `operating inside pi|Current working directory: /parent|Current working directory: /child`, grandchild)
		equal(t, strings.Count(grandchild, "parent rules"), 1, "parent rules count")
		mustMatch(t, "child rules", grandchild)
		mustMatch(t, "grandchild rules", grandchild)
	})

	t.Run("retains inherited nodes after their lookup keys are evicted", func(t *testing.T) {
		c := NewCaptures(2, nil)
		c.Record(parentKey, Input{ContextFiles: agents("survives eviction")}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix}, "")
		c.Record("unrelated", Input{}, "")

		if _, ok := c.Resolve(parentKey); ok {
			t.Error("parent key should be evicted")
		}
		mustMatch(t, "survives eviction", project(t, c, childKey))
	})

	t.Run("relinks an evicted ancestor through the live inheritance graph", func(t *testing.T) {
		c := NewCaptures(2, nil)
		c.Record(parentKey, Input{ContextFiles: agents("reachable ancestor")}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix}, "")
		c.Record("unrelated", Input{}, "")
		changedKey := "changed child assembled prompt"
		c.Record(changedKey, Input{Custom: parentKey + "\n\nchanged child rules"}, "")

		changed := project(t, c, changedKey)
		mustNotMatch(t, "operating inside pi", changed)
		mustMatch(t, "reachable ancestor", changed)
		mustMatch(t, "changed child rules", changed)
	})

	t.Run("updates descendants through a re-recorded parent node", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("rules v1")}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix}, "")
		c.Record(parentKey, Input{ContextFiles: agents("rules v2")}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix, Append: "child append"}, "")

		child := project(t, c, childKey)
		mustNotMatch(t, "rules v1|operating inside pi", child)
		mustMatch(t, "rules v2", child)
		mustMatch(t, "child append$", child)
	})

	t.Run("projects every non-overlapping occurrence of an inherited prompt", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("repeated rules")}, "")
		c.Record("repeated child", Input{Custom: parentKey + "\nseparator\n" + parentKey}, "")

		result := project(t, c, "repeated child")
		mustNotMatch(t, "operating inside pi", result)
		equal(t, strings.Count(result, "repeated rules"), 2, "repeated rules count")
	})

	t.Run("deduplicates inherited skills and preserves child-only skills", func(t *testing.T) {
		browser, review := skill("browser"), skill("review")
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{Skills: []map[string]any{browser}}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix, Skills: []map[string]any{browser, review}}, "")

		child, _ := c.Resolve(childKey)
		var names []string
		for _, s := range CollectPromptSkills(child) {
			names = append(names, s["name"].(string))
		}
		equal(t, strings.Join(names, ","), "browser,review", "collected skills")
		result, err := ProjectPromptCapture(child, "mcp", debugPath)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, strings.Count(result, "/skills/browser/SKILL.md"), 1, "browser count")
		equal(t, strings.Count(result, "/skills/review/SKILL.md"), 1, "review count")
	})

	t.Run("allows an enabled child skill when the inherited copy is hidden", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{Skills: []map[string]any{skill("browser", true)}}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix, Skills: []map[string]any{skill("browser")}}, "")
		equal(t, strings.Count(project(t, c, childKey), "/skills/browser/SKILL.md"), 1, "browser count")
	})

	t.Run("is bounded and evicts the least-recently-recorded key", func(t *testing.T) {
		c := NewCaptures(3, nil)
		c.Record("a", Input{}, "")
		c.Record("b", Input{}, "")
		c.Record("c", Input{}, "")
		c.Record("a", Input{Custom: "refreshed"}, "")
		c.Record("d", Input{}, "")

		equal(t, c.Size(), 3, "size")
		if _, ok := c.Resolve("b"); ok {
			t.Error("b should be evicted")
		}
		a, _ := c.Resolve("a")
		equal(t, a.Custom, "refreshed", "a custom")
		_, okC := c.Resolve("c")
		_, okD := c.Resolve("d")
		if !okC || !okD {
			t.Error("c and d must remain")
		}
	})

	t.Run("serialises concurrent hook handlers", func(t *testing.T) {
		c := NewCaptures(8, nil)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key := fmt.Sprintf("k%d", i)
				c.Record(key, Input{Custom: "x"}, "agent_start")
				c.Resolve(key)
				_, _ = c.ResolveOrDerive("nothing like it")
			}()
		}
		wg.Wait()
		if c.Size() > 8 {
			t.Errorf("size %d exceeds the limit", c.Size())
		}
	})
}

func TestPromptSections(t *testing.T) {
	t.Run("projects a recorded section after the appended instructions, in pi's wrapper format", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("sectioned prompt", Input{Append: "be careful", Sections: []Section{{"mcp_servers", mcpContent}}}, "")
		equal(t, project(t, c, "sectioned prompt"), "be careful\n\n"+mcpSection, "projection")
	})

	t.Run("renders sections in their recorded order", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("ordered", Input{Sections: []Section{{"zeta", "z"}, {"alpha", "a"}}}, "")
		equal(t, project(t, c, "ordered"), "<zeta>\nz\n</zeta>\n\n<alpha>\na\n</alpha>", "order")
	})

	t.Run("skips an empty section, as pi's builder does", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("one empty section", Input{Sections: []Section{{"mcp_servers", ""}, {"other", "kept"}}}, "")
		equal(t, project(t, c, "one empty section"), "<other>\nkept\n</other>", "projection")
	})

	t.Run("keeps the recorded sections when the caller mutates its own map", func(t *testing.T) {
		sections := []Section{{"mcp_servers", mcpContent}}
		c := NewCaptures(0, nil)
		c.Record("live options", Input{Sections: sections}, "")
		sections[0].Text = "mutated"
		equal(t, project(t, c, "live options"), mcpSection, "projection")
	})

	t.Run("drops a section a later recording no longer carries", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("sectioned prompt", Input{Sections: []Section{{"mcp_servers", mcpContent}}}, "")
		equal(t, project(t, c, "sectioned prompt"), mcpSection, "first")
		c.Record("sectioned prompt", Input{}, "")
		equal(t, project(t, c, "sectioned prompt"), "", "second")
	})

	t.Run("projects an inherited prompt's section through a child, once", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("parent rules"), Sections: []Section{{"mcp_servers", mcpContent}}}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix}, "")

		child := project(t, c, childKey)
		equal(t, strings.Count(child, mcpSection), 1, "section count")
		mustNotMatch(t, "operating inside pi", child)
	})

	t.Run("refuses a section carrying pi's preamble or Anthropic's trigger pair", func(t *testing.T) {
		offending := []Section{
			{"leaked_harness", PiPreamble + ", a coding agent harness."},
			{"triggers", "see docs/custom-provider.md and docs/packages.md"},
		}
		for _, s := range offending {
			c := NewCaptures(0, nil)
			c.Record("guarded prompt", Input{Sections: []Section{s}}, "before_agent_start")
			found, _ := c.Resolve("guarded prompt")
			_, err := ProjectPromptCapture(found, "none", debugPath)
			if err == nil || !strings.Contains(err.Error(), "in the "+s.Name+" section") {
				t.Errorf("the %s section must be checked on its own: %v", s.Name, err)
			}
		}
	})
}

func TestCaptureProvenance(t *testing.T) {
	t.Run("records which boundary last wrote a key", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("key", Input{}, "agent_start")
		got, _ := c.Resolve("key")
		equal(t, got.Source, "agent_start", "first source")
		c.Record("key", Input{}, "turn_start")
		got, _ = c.Resolve("key")
		equal(t, got.Source, "turn_start", "a re-record replaces the earlier source")
	})

	t.Run("names the closest match's boundary in a throw", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("prefix-common-THE-REST", Input{}, "agent_start")
		_, err := c.ResolveOrDerive("prefix-common-WHO-ARE-YOU")
		if err == nil {
			t.Fatal("want error")
		}
		mustMatch(t, "recorded at agent_start", err.Error())
	})
}

func TestPromptGuard(t *testing.T) {
	t.Run("throws when a recorded custom prompt carries pi's preamble at a line start", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("leaking prompt", Input{Custom: PiPreamble + ", a coding agent harness.\n\n<tools>\n- read: Read a file\n</tools>"}, "before_agent_start")
		found, ok := c.Resolve("leaking prompt")
		if !ok {
			t.Fatal("missing capture")
		}

		_, err := ProjectPromptCapture(found, "none", debugPath)
		if err == nil {
			t.Fatal("want error")
		}
		msg := err.Error()
		if !strings.Contains(msg, PiPreamble) {
			t.Error("message lacks the preamble")
		}
		mustMatch(t, `offset 0 of \d+ chars`, msg)
		mustMatch(t, "Capture: before_agent_start", msg)
		mustMatch(t, "Compatibility with other extensions", msg)
		want := "prompt-capture: refusing to send this prompt. Claude Code's Anthropic path reads a request\n" +
			"  carrying pi's harness, or the phrase pair its subscription gate rejects, as a third-party\n" +
			"  app: it fails with 400 or is billed as extra usage.\n"
		if !strings.HasPrefix(msg, want) {
			t.Errorf("refusal header differs: %q", msg)
		}
		wantTail := "  Capture: before_agent_start, 0 inherited capture(s) substituted.\n" +
			"  If this came from an inherited pi prompt, see README \"Compatibility with other extensions\".\n" +
			"  If it is your own text, reword or remove it. CLAUDE_BRIDGE_DEBUG=1 writes the full prompt to\n" +
			"  " + debugPath + "."
		if !strings.HasSuffix(msg, wantTail) {
			t.Errorf("refusal tail differs: %q", msg)
		}
	})

	t.Run("checks the custom text after replacing a matched inherited prompt", func(t *testing.T) {
		c := NewCaptures(0, nil)
		parentPrompt := PiPreamble + ", a coding agent harness.\n\n<tools>\n- read: Read a file\n</tools>"
		c.Record(parentPrompt, Input{ContextFiles: []ContextFile{{"/parent/AGENTS.md", "parent rules"}}}, "agent_start")
		c.Record("child assembled prompt", Input{Custom: "child wrapper\n\n" + parentPrompt + "\n\nchild instructions"}, "agent_start")
		child, _ := c.Resolve("child assembled prompt")
		equal(t, len(child.Inherited), 1, "inherited count")

		projected, err := ProjectPromptCapture(child, "none", debugPath)
		if err != nil {
			t.Fatal(err)
		}
		if projected == "" || strings.Contains(projected, PiPreamble) {
			t.Errorf("projection must be non-empty and harness-free: %q", projected)
		}
		mustMatch(t, "child wrapper", projected)
		mustMatch(t, "parent rules", projected)
	})

	t.Run("throws when a context file carries the trigger pair", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("context-only prompt", Input{ContextFiles: agents("see docs/custom-provider.md and docs/packages.md")}, "before_agent_start")
		found, _ := c.Resolve("context-only prompt")

		_, err := ProjectPromptCapture(found, "none", debugPath)
		if err == nil {
			t.Fatal("want error")
		}
		mustMatch(t, `docs/custom-provider\.md and docs/packages\.md in the project context block`, err.Error())
	})

	t.Run("ignores either trigger path on its own", func(t *testing.T) {
		for _, path := range []string{"docs/custom-provider.md", "docs/packages.md"} {
			c := NewCaptures(0, nil)
			custom := "see " + path
			c.Record("one path", Input{Custom: custom}, "before_agent_start")
			found, _ := c.Resolve("one path")
			got, err := ProjectPromptCapture(found, "none", debugPath)
			if err != nil || got != custom {
				t.Errorf("%s: got %q, %v", path, got, err)
			}
		}
	})

	t.Run("ignores a mid-line mention of pi's preamble", func(t *testing.T) {
		c := NewCaptures(0, nil)
		custom := "note: " + PiPreamble + ", allegedly"
		c.Record("mid-line prompt", Input{Custom: custom}, "before_agent_start")
		found, _ := c.Resolve("mid-line prompt")
		got, err := ProjectPromptCapture(found, "none", debugPath)
		if err != nil || got != custom {
			t.Errorf("got %q, %v", got, err)
		}
	})

	// Adaptation: the TypeScript test imports Pi's rendered prompt from node_modules.
	// The Go bridge cannot import PiG's prompt builder, so this pins the sentences as
	// quoted from PiG (internal/codingagent/prompts/coding.go preamble and
	// docs_section.go docsTopics) and asserts the guard refuses them.
	t.Run("pins the guard's markers against PiG's own harness text", func(t *testing.T) {
		pigPreamble := "You are an expert coding assistant operating inside pig, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files."
		pigDocsTopics := "- When asked about: custom providers (docs/custom-provider.md), adding models (docs/models.md), pig packages (docs/packages.md)"
		c := NewCaptures(0, nil)
		c.Record("pig preamble", Input{Custom: pigPreamble}, "agent_start")
		c.Record("pig docs", Input{Custom: pigDocsTopics}, "agent_start")
		for _, key := range []string{"pig preamble", "pig docs"} {
			found, _ := c.Resolve(key)
			if _, err := ProjectPromptCapture(found, "none", debugPath); err == nil {
				t.Errorf("%s: PiG's harness text must be refused", key)
			}
		}
	})
}

func TestConcurrentProjection(t *testing.T) {
	t.Run("projects a child while its parent is re-recorded", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("rules"), Skills: []map[string]any{skill("browser")}}, "")
		c.Record(childKey, Input{Custom: parentKey + childSuffix}, "")
		derived, err := c.ResolveOrDerive("wrap\n" + parentKey)
		if err != nil {
			t.Fatal(err)
		}
		if derived.mu != &c.mu {
			t.Fatal("a derived capture must share the registry mutex")
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				c.Record(parentKey, Input{ContextFiles: agents(fmt.Sprint("rules ", i)), Skills: []map[string]any{skill("browser")}}, "agent_start")
			}
		}()
		go func() {
			defer wg.Done()
			child, _ := c.Resolve(childKey)
			for i := 0; i < 300; i++ {
				if _, err := ProjectPromptCapture(child, "mcp", debugPath); err != nil {
					t.Error(err)
					return
				}
				_, _ = ProjectPromptCapture(derived, "mcp", debugPath)
				CollectPromptSkills(child)
			}
		}()
		wg.Wait()
	})
}

func TestDiagnosticUnits(t *testing.T) {
	t.Run("counts offsets and lengths in UTF-16 units", func(t *testing.T) {
		// "é" is 2 bytes and 1 unit; "😀" is 4 bytes and 2 units.
		c := NewCaptures(0, nil)
		c.Record("é😀-same-X", Input{}, "")
		_, err := c.ResolveOrDerive("é😀-same-Y!")
		if err == nil {
			t.Fatal("want error")
		}
		mustMatch(t, `this 11-char system prompt`, err.Error())
		mustMatch(t, `diverges at offset 9 \(10-char key`, err.Error())

		c2 := NewCaptures(0, nil)
		c2.Record("é", Input{}, "")
		_, err = c2.ResolveOrDerive("è")
		if err == nil {
			t.Fatal("want error")
		}
		mustMatch(t, `diverges at offset 0 \(1-char key`, err.Error())

		c3 := NewCaptures(0, nil)
		c3.Record("é😀-tail", Input{}, "turn_start")
		_, err = c3.ResolveOrDerive("é😀")
		mustMatch(t, `this 3-char prompt is the 8-char capture.* final 5 chars missing`, err.Error())

		c4 := NewCaptures(0, nil)
		c4.Record("g", Input{Custom: "é😀\n" + PiPreamble}, "x")
		g, _ := c4.Resolve("g")
		_, err = ProjectPromptCapture(g, "none", debugPath)
		mustMatch(t, `at offset 4 of 58 chars`, err.Error())
	})
}

func TestProjectReRecordedKey(t *testing.T) {
	t.Run("projects a capture while the same key is re-recorded", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record(parentKey, Input{ContextFiles: agents("rules")}, "")
		found, _ := c.Resolve(parentKey)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				c.Record(parentKey, Input{ContextFiles: agents(fmt.Sprint("rules ", i))}, "agent_start")
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				if _, err := ProjectPromptCapture(found, "mcp", debugPath); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		wg.Wait()
	})

	t.Run("does not panic when a key starts with a continuation byte", func(t *testing.T) {
		c := NewCaptures(0, nil)
		c.Record("\x80abc", Input{}, "")
		if _, err := c.ResolveOrDerive("zzz"); err == nil {
			t.Fatal("want error")
		}
	})
}
