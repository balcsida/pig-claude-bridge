//go:build live

package live

import (
	"os"
	"strings"
	"testing"
	"time"
)

// An image sent on another provider must survive the bridge writing pi's history into a Claude Code session,
// and still be visible to Claude after the resume (port of tests/int-image-session.mjs). This is the one place
// the whole image path is exercised together: pi context -> conversion -> session writer -> CC's own resume.
// Needs the alternative provider from the environment only.
func TestIntImageSession(t *testing.T) {
	altProvider, altModel := altProviderModel(t)
	// 32x32 solid crimson PNG — a colour the model can name unambiguously.
	const crimsonPNG = "iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAIAAAD8GO2jAAAAKklEQVR4nGO4I2JDU8QwasGoBaMWjFow" +
		"asGoBaMWjFowasGoBaMWDBULADahsD1ndvqVAAAAAElFTkSuQmCC"
	const timeout = 90 * time.Second

	cwd := t.TempDir()
	h := New(t, Options{
		Name:           "image-session",
		Args:           []string{"--provider", altProvider, "--model", altModel},
		Cwd:            cwd,
		DefaultTimeout: timeout,
	})
	// Both the working dir and the session Claude Code wrote for it are ours.
	t.Cleanup(func() {
		if dir := claudeProjectDir(t, cwd); strings.Contains(dir, "image-session") || strings.Contains(dir, "TestIntImageSession") {
			os.RemoveAll(dir)
		}
	})
	h.StartAndWait(0)

	// Substring matching is not enough: a refusal like "I don't see an image, I made an error" contains "red".
	// Only the first word counts, so a model that cannot see the image has no way to accidentally pass.
	namesTheColour := func(answer string) bool {
		s := strings.TrimLeftFunc(strings.ToLower(strings.TrimSpace(answer)), func(r rune) bool { return r < 'a' || r > 'z' })
		first := strings.FieldsFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z') })
		return len(first) > 0 && (first[0] == "red" || first[0] == "crimson")
	}

	// Turn 1 on the non-bridge provider: puts the image into pi's history, and confirms the fixture is a
	// readable image before the bridge is involved.
	t.Log("Turn 1: send the image to the non-bridge provider...")
	collect := h.CollectText()
	if _, err := h.Send(map[string]any{
		"type":    "prompt",
		"message": "What colour is this image? Reply with just the colour word.",
		"images":  []any{map[string]any{"type": "image", "data": crimsonPNG, "mimeType": "image/png"}},
	}, timeout); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	if _, err := h.WaitForEvent("agent_end", timeout); err != nil {
		failWithLogs(t, h, "%v", err)
	}
	seen := collect()
	t.Logf("  Response: %s", head(strings.TrimSpace(seen), 60))
	if !namesTheColour(seen) {
		// Nothing after this can mean anything if the fixture was never readable.
		failWithLogs(t, h, "turn 1 could not read the image fixture (got: %s)", head(strings.TrimSpace(seen), 80))
	}

	// Switching providers forces the bridge to write pi's history, image included, into a Claude Code session.
	t.Log("Switching to claude-bridge/claude-haiku-4-5...")
	if _, err := h.Send(map[string]any{"type": "set_model", "provider": "claude-bridge", "modelId": "claude-haiku-4-5"}, timeout); err != nil {
		failWithLogs(t, h, "%v", err)
	}

	t.Log("Turn 2: ask Claude about the image through the rebuilt session...")
	answer, err := h.PromptAndWait("Look at the image earlier in this conversation. Reply with exactly one word: "+
		"its colour, or UNKNOWN if no image is present.", timeout)
	if err != nil {
		failWithLogs(t, h, "%v", err)
	}
	t.Logf("  Response: %s", head(strings.TrimSpace(answer), 80))
	if !namesTheColour(answer) {
		failWithLogs(t, h, "image did not survive the session rebuild (got: %s)", head(strings.TrimSpace(answer), 120))
	}
	t.Log("PASS")
}
