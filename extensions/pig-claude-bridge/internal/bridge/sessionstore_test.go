package bridge

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSessionStore(t *testing.T) {
	agentDir := t.TempDir()
	state := SessionState{CCSessionID: "cc-1", Cwd: "/work", Model: "claude-sonnet-5-5[1m]", PromptDigest: "pd",
		Recording: Recording{Digests: []string{"a", "b"}, UUIDs: []string{"u1", ""}, Cuttable: []bool{true, false}}}

	t.Run("round-trips through an atomic, user-only file, loading dirty", func(t *testing.T) {
		if err := SaveSessionState(agentDir, "pi-1", state); err != nil {
			t.Fatal(err)
		}
		loaded, rejected, err := LoadSessionState(agentDir, "pi-1")
		if err != nil || rejected != "" || loaded == nil {
			t.Fatalf("load: %v %q %v", loaded, rejected, err)
		}
		want := state
		want.DirtyTail = true
		if !reflect.DeepEqual(*loaded, want) {
			t.Fatalf("got %+v want %+v", *loaded, want)
		}
		info, err := os.Stat(statePath(agentDir, "pi-1"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, %v", info.Mode(), err)
		}
		if dir, _ := os.Stat(sessionsDir(agentDir)); dir.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v", dir.Mode())
		}
		if leftovers, _ := filepath.Glob(filepath.Join(sessionsDir(agentDir), ".*.tmp")); len(leftovers) != 0 {
			t.Fatalf("temp files left: %v", leftovers)
		}
		data, _ := os.ReadFile(statePath(agentDir, "pi-1"))
		if !strings.Contains(string(data), `"uuids":["u1",null]`) {
			t.Fatalf("a null uuid must persist as JSON null: %s", data)
		}
	})

	t.Run("the file name is the TypeScript's sha256 over the UTF-16LE id", func(t *testing.T) {
		// node -e 'console.log(require("crypto").createHash("sha256").update("pi-1","utf16le").digest("hex"))'
		if got := stateFileName("pi-1"); got != "s-31e63129e995f7cb29f919f6f216503077e01ca5485bf8edb5e5fefc686d9d03.json" {
			t.Fatalf("name = %s", got)
		}
	})

	t.Run("a missing file is no state and no error", func(t *testing.T) {
		loaded, rejected, err := LoadSessionState(agentDir, "nobody")
		if loaded != nil || rejected != "" || err != nil {
			t.Fatalf("%v %q %v", loaded, rejected, err)
		}
		if err := DeleteSessionState(agentDir, "nobody"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("invalid files are rejected with a reason and treated as no state", func(t *testing.T) {
		cases := map[string]string{
			`{bad`:          "invalid JSON",
			`[]`:            "expected an object",
			`{}`:            "missing version",
			`{"version":2}`: "unsupported version; expected 1",
			`{"version":1,"ccSessionId":"c","cwd":"/w","model":"m","promptDigest":"p","digests":[],"uuids":[]}`:                          "missing cuttable",
			`{"version":1,"ccSessionId":"","cwd":"/w","model":"m","promptDigest":"p","digests":[],"uuids":[],"cuttable":[]}`:             "ccSessionId must not be empty",
			`{"version":1,"ccSessionId":"c","cwd":"/w","model":"m","promptDigest":"p","digests":["a"],"uuids":[null],"cuttable":[true]}`: "invalid session recording: slot 0",
			`{"version":1,"ccSessionId":"c","cwd":"/w","model":"m","promptDigest":"p","digests":["a"],"uuids":[],"cuttable":[]}`:         "invalid session recording: arrays must be present and aligned",
		}
		for contents, want := range cases {
			if err := os.WriteFile(statePath(agentDir, "pi-bad"), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, rejected, err := LoadSessionState(agentDir, "pi-bad")
			if err != nil || loaded != nil || rejected != want {
				t.Errorf("%s: loaded=%v rejected=%q err=%v, want %q", contents, loaded, rejected, err, want)
			}
		}
	})

	t.Run("save refuses an incomplete state", func(t *testing.T) {
		if err := SaveSessionState(agentDir, "pi-2", SessionState{Cwd: "/w", Model: "m", PromptDigest: "p", Recording: emptyRecording()}); err == nil || !strings.Contains(err.Error(), "ccSessionId") {
			t.Fatalf("err = %v", err)
		}
		if err := SaveSessionState(agentDir, "pi-2", SessionState{CCSessionID: "c", Cwd: "/w", Model: "m", PromptDigest: "p"}); err == nil {
			t.Fatal("a zero recording is not a valid one")
		}
	})

	t.Run("delete removes the file", func(t *testing.T) {
		if err := DeleteSessionState(agentDir, "pi-1"); err != nil {
			t.Fatal(err)
		}
		if loaded, _, _ := LoadSessionState(agentDir, "pi-1"); loaded != nil {
			t.Fatal("still loads")
		}
	})
}
