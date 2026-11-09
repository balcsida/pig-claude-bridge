package digest

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The fixture holds the digests pi-claude-bridge's src/message-digests.ts computes
// (scripts/dev/gen-digests.mjs); the Go port must agree on every byte since digests
// are persisted in the session state.
func TestDigestsMatchTheTypeScript(t *testing.T) {
	data, err := os.ReadFile("../../../../tests/fixtures/message-digests.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string         `json:"name"`
		Message map[string]any `json:"message"`
		Digest  string         `json:"digest"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("fixture has only %d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := Message(c.Message)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.Digest {
				t.Fatalf("got %s want %s", got, c.Digest)
			}
		})
	}
}

func TestJSNumber(t *testing.T) {
	cases := map[float64]string{1: "1", 1.5: "1.5", -2: "-2", 1e21: "1e+21", 1e-7: "1e-7", 0.000001: "0.000001",
		123456789012345680000: "123456789012345680000", 0: "0", 3.0: "3", 1e100: "1e+100", 2.5e-10: "2.5e-10", 123456.789: "123456.789"}
	for in, want := range cases {
		if got := jsNumber(in); got != want {
			t.Errorf("jsNumber(%v) = %q, want %q", in, got, want)
		}
	}
	if got := jsNumber(math.Copysign(0, -1)); got != "0" {
		t.Errorf("-0 = %q", got)
	}
}

func TestMessageRejectsSystemRoles(t *testing.T) {
	if _, err := Message(map[string]any{"role": "system"}); err == nil {
		t.Fatal("a system message has no digest")
	}
	if _, err := Messages([]map[string]any{{"role": "user", "content": "a"}, {"role": "bogus"}}); err == nil {
		t.Fatal("Messages must report the failing message")
	}
}
