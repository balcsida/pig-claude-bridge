package convert

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
)

const fixturePath = "../../../../tests/fixtures/pi-history-310.jsonl"

func loadFixture(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var msgs []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return msgs
}

// volatile are the record fields that legitimately differ between two builds
// of the same history: identity and clock, none of which reaches the prompt.
var volatile = []string{"uuid", "parentUuid", "sessionId", "timestamp", "slug", "requestId"}

// transcript is the whole write path — ConvertPiMessages, RepairToolPairing,
// Session.ImportMessages — reduced to the record content the prompt cache is
// keyed on (port of diag/lib/write-path.mjs).
func transcript(t *testing.T, piMessages []map[string]any) []string {
	t.Helper()
	anthropic, _, _, err := ConvertPiMessages(piMessages, nil)
	if err != nil {
		t.Fatal(err)
	}
	repaired := ccsession.RepairToolPairing(anthropic)
	// A fixed project path, since the cwd field is not volatile; nothing is saved.
	session, err := ccsession.Create(ccsession.Options{ProjectPath: "/work", ClaudeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) > 0 {
		if err := session.ImportMessages(repaired, nil); err != nil {
			t.Fatal(err)
		}
	}
	var out []string
	for _, raw := range session.Records() {
		var r map[string]any
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		for _, k := range volatile {
			delete(r, k)
		}
		if msg, ok := r["message"].(map[string]any); ok {
			delete(msg, "id")
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// settledPrefixes are the prefix lengths at which every tool call issued so far
// has its result. Truncating mid-turn leaves the repair no choice but to stand
// in a synthetic stub, so only settled prefixes can be expected to extend cleanly.
func settledPrefixes(messages []map[string]any) []int {
	var lengths []int
	pending := map[string]bool{}
	for i, msg := range messages {
		if msg["role"] == "assistant" {
			blocks, _ := msg["content"].([]any)
			for _, b := range blocks {
				if bm, _ := b.(map[string]any); bm["type"] == "toolCall" {
					pending[bm["id"].(string)] = true
				}
			}
		}
		if msg["role"] == "toolResult" {
			delete(pending, msg["toolCallId"].(string))
		}
		if len(pending) == 0 {
			lengths = append(lengths, i+1)
		}
	}
	return lengths
}

func assertPrefix(t *testing.T, shorter, longer []string, label string) {
	t.Helper()
	for i := range shorter {
		if i >= len(longer) || longer[i] != shorter[i] {
			t.Fatalf("%s: record %d of %d diverged", label, i, len(shorter))
		}
	}
}

// Parallel calls, a steer landing between their results, an image result, an
// error result, empty content, exotic tool ids, thinking with and without a
// signature, and thinking from providers whose signatures we may not replay.
const exoticJSON = `[
	{"role":"user","content":"start"},
	{"role":"assistant","provider":"claude-bridge","content":[
		{"type":"thinking","thinking":"planning","thinkingSignature":"sigA"},
		{"type":"text","text":"reading two files"},
		{"type":"toolCall","id":"functions.read:0","name":"read","arguments":{"path":"a"}},
		{"type":"toolCall","id":"functions.read:1","name":"read","arguments":{"path":"b"}}]},
	{"role":"toolResult","toolCallId":"functions.read:0","content":"body a"},
	{"role":"user","content":"actually also check c"},
	{"role":"toolResult","toolCallId":"functions.read:1","content":[
		{"type":"text","text":"shot"},
		{"type":"image","data":"BASE64","mimeType":"image/png"}]},
	{"role":"assistant","provider":"openrouter","content":[
		{"type":"thinking","thinking":"foreign","thinkingSignature":"sigB"},
		{"type":"toolCall","id":"tool call#2@x","name":"my_custom_tool","arguments":{}}]},
	{"role":"toolResult","toolCallId":"tool call#2@x","content":"","isError":true},
	{"role":"assistant","provider":"claude-bridge","content":[{"type":"thinking","thinking":"unsigned"}]},
	{"role":"user","content":""},
	{"role":"user","content":[{"type":"image","data":"IMG","mimeType":"image/jpeg"}]},
	{"role":"assistant","provider":"claude-bridge","content":[{"type":"toolCall","id":"t9","name":"bash","arguments":{"cmd":"ls"}}]},
	{"role":"toolResult","toolCallId":"t9","content":[{"type":"document"}]},
	{"role":"assistant","provider":"claude-bridge","content":[{"type":"text","text":"done"}]}
]`

func TestConversionIsDeterministic(t *testing.T) {
	corpus := loadFixture(t)
	exotic := decodeMessages(t, exoticJSON)

	t.Run("a real 310-message session converts identically twice", func(t *testing.T) {
		if !reflect.DeepEqual(transcript(t, corpus), transcript(t, corpus)) {
			t.Fatal("two conversions differ")
		}
	})

	t.Run("parallel calls, images, errors and thinking blocks convert identically twice", func(t *testing.T) {
		if !reflect.DeepEqual(transcript(t, exotic), transcript(t, exotic)) {
			t.Fatal("two conversions differ")
		}
	})
}

func TestRebuildsExtendTheTranscriptInsteadOfRewritingIt(t *testing.T) {
	corpus := loadFixture(t)
	exotic := decodeMessages(t, exoticJSON)

	t.Run("every settled prefix of a real session is a prefix of the full rebuild", func(t *testing.T) {
		full := transcript(t, corpus)
		lengths := settledPrefixes(corpus)
		if len(lengths) <= 100 {
			t.Fatalf("expected many settled prefixes, got %d", len(lengths))
		}
		for _, n := range lengths {
			assertPrefix(t, transcript(t, corpus[:n]), full, fmt.Sprintf("history[0..%d]", n))
		}
	})

	t.Run("a settled prefix survives every longer history, mid-turn truncation included", func(t *testing.T) {
		for _, n := range settledPrefixes(exotic) {
			shorter := transcript(t, exotic[:n])
			for m := n + 1; m <= len(exotic); m++ {
				assertPrefix(t, shorter, transcript(t, exotic[:m]), fmt.Sprintf("exotic[0..%d] vs [0..%d]", n, m))
			}
		}
	})
}

func TestRepairToolPairingIsIdempotent(t *testing.T) {
	anthropic, _, _, err := ConvertPiMessages(loadFixture(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	once := ccsession.RepairToolPairing(anthropic)
	if !reflect.DeepEqual(ccsession.RepairToolPairing(once), once) {
		t.Fatal("a second repair changed the messages")
	}
}

func TestRealSessionRoundTripsWithEveryToolResultIntact(t *testing.T) {
	corpus := loadFixture(t)
	anthropic, _, _, err := ConvertPiMessages(corpus, nil)
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs, wantIDs []string
	for _, m := range ccsession.RepairToolPairing(anthropic) {
		blocks, _ := m.Content.([]ccsession.Block)
		for _, b := range blocks {
			if tr, ok := b.(ccsession.ToolResultBlock); ok {
				gotIDs = append(gotIDs, tr.ToolUseID)
				if strings.Contains(fmt.Sprint(tr.Content), "no tool result recorded") {
					t.Fatalf("stubbed result for %s", tr.ToolUseID)
				}
			}
		}
	}
	for _, m := range corpus {
		if m["role"] == "toolResult" {
			wantIDs = append(wantIDs, m["toolCallId"].(string))
		}
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("tool result ids differ: got %d, want %d", len(gotIDs), len(wantIDs))
	}
}

func TestMapPiToolNameToSdk(t *testing.T) {
	custom := map[string]string{"read": "mcp__custom-tools__read", "kagi_search": "mcp__custom-tools__kagi_search"}
	cases := []struct {
		name   string
		in     string
		custom map[string]string
		want   string
	}{
		{"builtin without a map", "bash", nil, "Bash"},
		{"builtin matched case-insensitively", "Read", nil, "Read"},
		{"custom without a map is pascal-cased", "my_custom_tool", nil, "MyCustomTool"},
		{"served tool takes its map entry", "read", custom, "mcp__custom-tools__read"},
		{"unserved tool gets the prefix, never a builtin name", "bash", custom, "mcp__custom-tools__bash"},
		{"empty name", "", custom, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := MapPiToolNameToSdk(c.in, c.custom); err != nil || got != c.want {
				t.Fatalf("got %q, %v, want %q", got, err, c.want)
			}
		})
	}

	t.Run("refuses an already-converted name", func(t *testing.T) {
		_, err := MapPiToolNameToSdk("mcp__custom-tools__read", nil)
		const want = `mapPiToolNameToSdk: "mcp__custom-tools__read" is already an SDK tool name — pi history holds pi tool names`
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v", err)
		}
		_, _, _, err = ConvertPiMessages(decodeMessages(t, `[{"role":"assistant","content":[{"type":"toolCall","id":"x","name":"mcp__custom-tools__read","arguments":{}}]}]`), nil)
		if err == nil || err.Error() != want {
			t.Fatalf("ConvertPiMessages err = %v", err)
		}
	})
}

// Expected values are what change-case's pascalCase returns for each input.
func TestPascalCaseMatchesChangeCase(t *testing.T) {
	cases := map[string]string{
		"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash",
		"my_custom_tool": "MyCustomTool", "kagi_search": "KagiSearch", "get_subagent_result": "GetSubagentResult",
		"fetch": "Fetch", "Agent": "Agent", "AskClaude": "AskClaude", "web-search": "WebSearch",
		"HTTPServer": "HttpServer", "foo2bar": "Foo2bar", "get2Thing": "Get2Thing", "a_1b": "A_1b",
		"  spaced out  ": "SpacedOut", "": "", "___": "",
	}
	for in, want := range cases {
		if got := pascalCase(in); got != want {
			t.Errorf("pascalCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeToolID(t *testing.T) {
	cases := map[string]string{
		"toolu_01-ok": "toolu_01-ok", "functions.read:0": "functions_read_0", "tool call#2@x": "tool_call_2_x", "é": "_", "😀": "__",
	}
	for in, want := range cases {
		if got := SanitizeToolID(in); got != want {
			t.Errorf("SanitizeToolID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertPiMessages(t *testing.T) {
	t.Run("drops an aborted turn and counts it", func(t *testing.T) {
		msgs, _, dropped, err := ConvertPiMessages(decodeMessages(t, `[{"role":"user","content":"hi"},{"role":"assistant","provider":"claude-bridge","content":[]}]`), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 || dropped.AbortedTurns != 1 {
			t.Fatalf("got %d messages, dropped %+v", len(msgs), dropped)
		}
	})

	t.Run("keeps a slot when every block was filtered, and counts what went", func(t *testing.T) {
		msgs, _, dropped, err := ConvertPiMessages(decodeMessages(t, `[{"role":"assistant","provider":"other","content":[{"type":"thinking","thinking":"x","thinkingSignature":"s"},{"type":"mystery"}]}]`), nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []ccsession.Message{{Role: "assistant", Content: []ccsession.Block{ccsession.TextBlock{Type: "text", Text: "[incompatible content omitted]"}}}}
		if !reflect.DeepEqual(msgs, want) {
			t.Fatalf("got %#v", msgs)
		}
		if dropped.Thinking != 1 || !reflect.DeepEqual(dropped.Providers, []string{"other"}) || dropped.Other["mystery"] != 1 {
			t.Fatalf("dropped %+v", dropped)
		}
	})

	t.Run("names a thinking block's provider as the TypeScript's ?? does", func(t *testing.T) {
		_, _, dropped, err := ConvertPiMessages(decodeMessages(t, `[
			{"role":"assistant","provider":"","content":[{"type":"thinking","thinking":"x"}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"x"}]}]`), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(dropped.Providers, []string{"", "unknown"}) {
			t.Fatalf("providers = %q", dropped.Providers)
		}
	})

	t.Run("reads assistant content in either slice type", func(t *testing.T) {
		msgs, _, dropped, err := ConvertPiMessages([]map[string]any{
			{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "hi"}}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []ccsession.Message{{Role: "assistant", Content: []ccsession.Block{ccsession.TextBlock{Type: "text", Text: "hi"}}}}
		if !reflect.DeepEqual(msgs, want) || dropped.AbortedTurns != 0 {
			t.Fatalf("got %#v, dropped %+v", msgs, dropped)
		}
	})

	t.Run("collects parallel results into one user message after their assistant message", func(t *testing.T) {
		msgs, ids, _, err := ConvertPiMessages(decodeMessages(t, `[
			{"role":"assistant","provider":"claude-bridge","content":[{"type":"toolCall","id":"a:1","name":"read","arguments":{}},{"type":"toolCall","id":"b:2","name":"read","arguments":{}}]},
			{"role":"toolResult","toolCallId":"a:1","content":"A"},
			{"role":"user","content":"steer"},
			{"role":"toolResult","toolCallId":"b:2","content":"B","isError":true}]`), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 || msgs[1].Role != "user" || msgs[2].Content != "steer" {
			t.Fatalf("got %#v", msgs)
		}
		want := []ccsession.Block{
			ccsession.ToolResultBlock{Type: "tool_result", ToolUseID: "a_1", Content: "A"},
			ccsession.ToolResultBlock{Type: "tool_result", ToolUseID: "b_2", Content: "B", IsError: true},
		}
		if !reflect.DeepEqual(msgs[1].Content, want) {
			t.Fatalf("got %#v", msgs[1].Content)
		}
		if ids["a:1"] != "a_1" || ids["b:2"] != "b_2" {
			t.Fatalf("ids %v", ids)
		}
	})
}
