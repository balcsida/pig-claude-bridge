//go:build live

package ccproto

// Port of pi-claude-bridge tests/int-attachment-rebuild.mjs (commit 6aa276a): an `@file` expansion survives a
// session rebuild. Claude Code stores the expansion as an attachment record in its own session file; pi never
// sees it, so a rebuild from pi's history must carry it. The question asks for a token that exists only inside
// the attachment; the control rebuilds without the carry and must not produce it.
//
// Kept beside the contract tests because it needs convert and ccsession, which only this module can import.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// Nothing can infer this from the filename, the prompt, or the file on disk: it exists only in the
// attachment payload the rebuild has to carry.
const (
	attachmentToken  = "ZORBLAT-4417-QUUX"
	attachmentPrompt = "Review @fixture.txt and remember it."
	attachmentAsk    = "What is the build token recorded in the file I attached? Reply with just the token."
)

func attachmentFor(t *testing.T, dir string) map[string]any {
	t.Helper()
	filePath := filepath.Join(dir, "fixture.txt")
	content := "# fixture\nbuild-token: " + attachmentToken + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"type":     "file",
		"filename": filePath,
		"content":  map[string]any{"type": "text", "file": map[string]any{"filePath": filePath, "content": content}},
	}
}

func attachmentMessages() []ccsession.Message {
	return []ccsession.Message{
		{Role: "user", Content: attachmentPrompt},
		{Role: "assistant", Content: []ccsession.Block{ccsession.TextBlock{Type: "text", Text: "Noted."}}},
	}
}

func attachmentRecords(s *ccsession.Session) (n int) {
	for _, raw := range s.Records() {
		if regexp.MustCompile(`"type":"attachment"`).Match(raw) {
			n++
		}
	}
	return n
}

// seedWithAttachment is the session as CC left it: prompt, attachment, reply.
func seedWithAttachment(t *testing.T, cwd, id string, attachment map[string]any) *ccsession.Session {
	t.Helper()
	s, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: liveModel, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportMessages(attachmentMessages(), []ccsession.ImportAttachment{{AfterIndex: 0, Attachment: attachment}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s
}

// rebuildWithCarry is what syncSharedSession does on REBUILD, with and without the carry.
func rebuildWithCarry(t *testing.T, cwd, id string, carried []convert.CarriedAttachment) (*ccsession.Session, []string) {
	t.Helper()
	if err := ccsession.Delete(id, cwd, ""); err != nil {
		t.Fatal(err)
	}
	s, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: liveModel, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	msgs := attachmentMessages()
	placed, skipped := convert.PlaceCarriedAttachments(carried, msgs)
	if err := s.ImportMessages(msgs, placed); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s, skipped
}

func askAttachment(t *testing.T, cwd, id string) string {
	t.Helper()
	o := resumeOptions(t, cwd, id)
	o.ToolsSet, o.Tools = true, nil
	return strings.TrimSpace(assistantText(run(t, startLive(t, o, nil), attachmentAsk)))
}

func TestAnAtFileAttachmentSurvivesARebuildAndReachesClaude(t *testing.T) {
	// The fixture lives outside the project dir so CC cannot read it off disk and answer without the attachment.
	cwd, id, dir := scratchDir(t), newUUID(t), t.TempDir()
	t.Cleanup(func() { ccsession.Delete(id, cwd, "") })

	seeded := seedWithAttachment(t, cwd, id, attachmentFor(t, dir))
	if got := attachmentRecords(seeded); got != 1 {
		t.Fatalf("seed did not write the attachment: %d attachment records", got)
	}
	opened, err := ccsession.Open(id, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	carried := convert.CollectCarriedAttachments(opened.Records())
	if len(carried) != 1 {
		t.Fatalf("CollectCarriedAttachments did not find the attachment: %d", len(carried))
	}

	rebuilt, skipped := rebuildWithCarry(t, cwd, id, carried)
	if len(skipped) != 0 {
		t.Fatalf("placement skipped: %s", strings.Join(skipped, "; "))
	}
	if got := attachmentRecords(rebuilt); got != 1 {
		t.Fatalf("rebuilt session has no attachment: %d attachment records", got)
	}

	answer := askAttachment(t, cwd, id)
	if !regexp.MustCompile("(?i)" + regexp.QuoteMeta(attachmentToken)).MatchString(answer) {
		t.Fatalf("Claude could not see the carried attachment. Answer: %s", answer)
	}
}

func TestControlWithoutTheCarryTheSameRebuildLosesTheAttachment(t *testing.T) {
	cwd, id, dir := scratchDir(t), newUUID(t), t.TempDir()
	t.Cleanup(func() { ccsession.Delete(id, cwd, "") })

	seedWithAttachment(t, cwd, id, attachmentFor(t, dir))
	rebuilt, _ := rebuildWithCarry(t, cwd, id, nil)
	if got := attachmentRecords(rebuilt); got != 0 {
		t.Fatalf("control rebuild should carry nothing: %d attachment records", got)
	}

	answer := askAttachment(t, cwd, id)
	if regexp.MustCompile("(?i)" + regexp.QuoteMeta(attachmentToken)).MatchString(answer) {
		t.Fatalf("Control produced the token without the attachment — the positive test proves nothing. Answer: %s", answer)
	}
}
