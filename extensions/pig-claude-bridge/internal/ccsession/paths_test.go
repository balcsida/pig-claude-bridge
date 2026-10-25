package ccsession

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestProjectPathToHash(t *testing.T) {
	t.Run("short path replaced char by char", func(t *testing.T) {
		if got, want := ProjectPathToHash("/Users/me/my.proj_1"), "-Users-me-my-proj-1"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
	t.Run("exactly 200 chars has no hash suffix", func(t *testing.T) {
		p := strings.Repeat("a", 200)
		if got := ProjectPathToHash(p); got != p {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("201 chars gives 200 prefix, dash and base36 djb2", func(t *testing.T) {
		p := strings.Repeat("a", 201)
		var h int32
		for _, c := range p {
			h = h<<5 - h + int32(c)
		}
		abs := int64(h)
		if abs < 0 {
			abs = -abs
		}
		want := strings.Repeat("a", 200) + "-" + strconv.FormatInt(abs, 36)
		if got := ProjectPathToHash(p); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
		// Independently computed (Python, 32-bit wrap) for 201 x "a".
		if want != strings.Repeat("a", 200)+"-rkvsv5" {
			t.Fatalf("djb2 pin mismatch: %s", want)
		}
	})
	// Pins from the library: node -e 'import("<clone>/node_modules/cc-session-io/dist/index.js").then(m => console.log(m.projectPathToHash(process.argv[1])))' -- <path>
	t.Run("negative 32-bit hash uses its absolute value", func(t *testing.T) {
		if got, want := ProjectPathToHash("/"+strings.Repeat("b", 200)+"/proj"), "-"+strings.Repeat("b", 199)+"-ohp29v"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
	t.Run("astral character past the cut hashes over UTF-16 units", func(t *testing.T) {
		if got, want := ProjectPathToHash("/"+strings.Repeat("b", 205)+"\U0001F600x"), "-"+strings.Repeat("b", 199)+"-jkib26"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
	t.Run("hash separates long paths sharing a prefix", func(t *testing.T) {
		a := ProjectPathToHash(strings.Repeat("a", 200) + "x")
		b := ProjectPathToHash(strings.Repeat("a", 200) + "y")
		if a == b {
			t.Fatal("expected different hashes")
		}
	})
	t.Run("non-ASCII counts UTF-16 units", func(t *testing.T) {
		if got, want := ProjectPathToHash("/é😀"), "----"; got != want { // / + é (1 unit) + 😀 (2 units)
			t.Fatalf("got %q want %q", got, want)
		}
	})
}

func TestNormalizeProjectPathNFC(t *testing.T) {
	decomposed := "/nonexistent/cafe\u0301"
	got := NormalizeProjectPath(decomposed)
	if got != norm.NFC.String(decomposed) || got != "/nonexistent/caf\u00e9" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeProjectPathResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got := NormalizeProjectPath(link); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestClaudeDirPrecedence(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/env/claude")
	if got := ClaudeDir("/override"); got != "/override" {
		t.Fatalf("got %q", got)
	}
	if got := ClaudeDir(""); got != "/env/claude" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, _ := os.UserHomeDir()
	if got := ClaudeDir(""); got != filepath.Join(home, ".claude") {
		t.Fatalf("got %q", got)
	}
}

func TestSessionPath(t *testing.T) {
	root := t.TempDir()
	proj := t.TempDir()
	want := filepath.Join(root, "projects", ProjectPathToHash(NormalizeProjectPath(proj)), "sid.jsonl")
	if got := SessionPath(root, proj, "sid"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := ProjectDir(root, proj); got != filepath.Dir(want) {
		t.Fatalf("got %q", got)
	}
}
