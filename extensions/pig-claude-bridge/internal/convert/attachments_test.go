package convert

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
)

func rec(t *testing.T, v map[string]any) ccsession.RawRecord {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func userRec(t *testing.T, uuid, text string) ccsession.RawRecord {
	return rec(t, map[string]any{"type": "user", "uuid": uuid, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}})
}

func toolResultUserRec(t *testing.T, uuid string) ccsession.RawRecord {
	return rec(t, map[string]any{"type": "user", "uuid": uuid, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}}})
}

func attachRec(t *testing.T, uuid, parent, kind, filename string) ccsession.RawRecord {
	att := map[string]any{"type": kind}
	if filename != "" {
		att["filename"] = filename
	}
	return rec(t, map[string]any{"type": "attachment", "uuid": uuid, "parentUuid": parent, "attachment": att})
}

func filenames(carried []CarriedAttachment) []any {
	out := []any{}
	for _, c := range carried {
		out = append(out, c.Attachment["filename"])
	}
	return out
}

func TestCollectCarriedAttachments(t *testing.T) {
	t.Run("keeps content-bearing kinds and drops the ones CC regenerates", func(t *testing.T) {
		carried := CollectCarriedAttachments([]ccsession.RawRecord{
			userRec(t, "u1", "review @a.js"),
			attachRec(t, "a1", "u1", "file", "/a.js"),
			attachRec(t, "a2", "u1", "skill_listing", ""),
			attachRec(t, "a3", "u1", "task_reminder", ""),
			attachRec(t, "a4", "u1", "edited_text_file", "/b.js"),
		})
		if got := filenames(carried); !reflect.DeepEqual(got, []any{"/a.js"}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("counts ordinals over prompts only, skipping tool-result user records", func(t *testing.T) {
		carried := CollectCarriedAttachments([]ccsession.RawRecord{
			userRec(t, "u1", "first"),
			toolResultUserRec(t, "u2"),
			userRec(t, "u3", "review @a.js"),
			attachRec(t, "a1", "u3", "file", "/a.js"),
		})
		if len(carried) != 1 || carried[0].UserOrdinal != 1 || carried[0].Text != "review @a.js" {
			t.Fatalf("got %#v", carried)
		}
	})

	t.Run("ignores an attachment whose parent is not a prompt", func(t *testing.T) {
		carried := CollectCarriedAttachments([]ccsession.RawRecord{
			userRec(t, "u1", "first"),
			attachRec(t, "a1", "missing-uuid", "file", "/a.js"),
		})
		if len(carried) != 0 {
			t.Fatalf("got %#v", carried)
		}
	})
}

func TestPlaceCarriedAttachments(t *testing.T) {
	carried := []CarriedAttachment{{Attachment: map[string]any{"type": "file", "filename": "/a.js"}, UserOrdinal: 1, Text: "review @a.js"}}
	text := func(s string) []ccsession.Block { return []ccsession.Block{ccsession.TextBlock{Type: "text", Text: s}} }

	t.Run("resolves the ordinal to an index in the array being imported", func(t *testing.T) {
		attachments, skipped := PlaceCarriedAttachments(carried, []ccsession.Message{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: text("ok")},
			{Role: "user", Content: text("review @a.js")},
		})
		if len(skipped) != 0 {
			t.Fatalf("skipped %v", skipped)
		}
		want := []ccsession.ImportAttachment{{AfterIndex: 2, Attachment: carried[0].Attachment}}
		if !reflect.DeepEqual(attachments, want) {
			t.Fatalf("got %#v", attachments)
		}
	})

	t.Run("drops it when that prompt changed rather than guessing", func(t *testing.T) {
		attachments, skipped := PlaceCarriedAttachments(carried, []ccsession.Message{
			{Role: "user", Content: "first"},
			{Role: "user", Content: "something else entirely"},
		})
		if len(attachments) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "changed") {
			t.Fatalf("got %v %v", attachments, skipped)
		}
	})

	t.Run("drops it when history no longer reaches that prompt", func(t *testing.T) {
		attachments, skipped := PlaceCarriedAttachments(carried, []ccsession.Message{{Role: "user", Content: "first"}})
		if len(attachments) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "no longer in history") {
			t.Fatalf("got %v %v", attachments, skipped)
		}
	})
}

// The Go session has no API for uuid-less audit records, so the test appends
// them to the file between saves, which is what Claude Code does on disk.
func TestSessionRequestAuditRecords(t *testing.T) {
	t.Run("keeps attachments and the next parent across uuid-less audit records", func(t *testing.T) {
		dir := t.TempDir()
		const id = "11111111-2222-4333-8444-555555555555"
		s, err := ccsession.Create(ccsession.Options{SessionID: id, ProjectPath: dir, ClaudeDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		audit := func(kind string) {
			t.Helper()
			f, err := os.OpenFile(s.Path(), os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(`{"type":"` + kind + `"}` + "\n"); err != nil {
				t.Fatal(err)
			}
		}
		reopen := func() {
			t.Helper()
			if s, err = ccsession.Open(id, dir, dir); err != nil {
				t.Fatal(err)
			}
		}
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(s.AddUserMessage([]ccsession.Block{ccsession.TextBlock{Type: "text", Text: "review @a.js"}}))
		must(s.Save())
		audit("api-request")
		reopen()
		must(s.AddAttachment(map[string]any{"type": "file", "filename": "/a.js"}))
		must(s.Save())
		audit("api-request-shape")
		reopen()
		must(s.AddAssistantMessage([]ccsession.Block{ccsession.TextBlock{Type: "text", Text: "done"}}, ""))
		must(s.Save())
		lastUUID := recordField(t, s.Records()[len(s.Records())-1], "uuid")
		audit("api-request-blob")
		reopen()

		got := CollectCarriedAttachments(s.Records())
		want := []CarriedAttachment{{Attachment: map[string]any{"type": "file", "filename": "/a.js"}, UserOrdinal: 0, Text: "review @a.js"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v", got)
		}
		must(s.AddUserMessage("next"))
		must(s.Save())
		records := s.Records()
		if parent := recordField(t, records[len(records)-1], "parentUuid"); parent != lastUUID {
			t.Fatalf("parentUuid = %v, want %v", parent, lastUUID)
		}
	})
}

func recordField(t *testing.T, raw ccsession.RawRecord, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m[key]
}

func TestAttachmentsChainedToOtherAttachments(t *testing.T) {
	t.Run("inherits the ordinal up a run so the whole run keys to one prompt", func(t *testing.T) {
		carried := CollectCarriedAttachments([]ccsession.RawRecord{
			userRec(t, "u1", "first"),
			userRec(t, "u2", "edit the files"),
			attachRec(t, "a1", "u2", "file", "/a.js"),
			attachRec(t, "a2", "a1", "edited_text_file", "/b.js"),
			attachRec(t, "a3", "a2", "file", "/c.js"),
		})
		if got := filenames(carried); !reflect.DeepEqual(got, []any{"/a.js", "/c.js"}) {
			t.Fatalf("got %v", got)
		}
		if carried[0].UserOrdinal != 1 || carried[1].UserOrdinal != 1 {
			t.Fatalf("ordinals %d %d", carried[0].UserOrdinal, carried[1].UserOrdinal)
		}
	})

	t.Run("resolves through a kind it does not carry", func(t *testing.T) {
		carried := CollectCarriedAttachments([]ccsession.RawRecord{
			userRec(t, "u1", "go"),
			attachRec(t, "a1", "u1", "skill_listing", ""),
			attachRec(t, "a2", "a1", "file", "/a.js"),
		})
		if got := filenames(carried); !reflect.DeepEqual(got, []any{"/a.js"}) || carried[0].UserOrdinal != 0 {
			t.Fatalf("got %#v", carried)
		}
	})
}
