package ccproto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveExecutableConfigured(t *testing.T) {
	got, err := ResolveExecutable("/opt/claude")
	if err != nil || got != "/opt/claude" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestResolveExecutableMissing(t *testing.T) {
	t.Setenv("PATH", "")
	_, err := ResolveExecutable("")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"claude", "provider.pathToClaudeCodeExecutable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestVersion(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho '2.1.310 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Version(exe)
	if err != nil || got != "2.1.310" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestVersionUnparsable(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Version(exe); err == nil {
		t.Fatal("expected error")
	}
}

func TestVersionStalledReturnsWithinTheTimeout(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := VersionTimeout
	VersionTimeout = 200 * time.Millisecond
	t.Cleanup(func() { VersionTimeout = old })
	start := time.Now()
	_, err := Version(exe)
	if err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Version took %s", elapsed)
	}
}

func TestOlderThan(t *testing.T) {
	for _, c := range []struct {
		v, min string
		want   bool
	}{
		{"2.1.309", "2.1.310", true},
		{"2.1.310", "2.1.310", false},
		{"2.1.311", "2.1.310", false},
		{"2.0.999", "2.1.310", true},
		{"2.10.0", "2.9.0", false},
		{"1.99.99", "2.0.0", true},
		{"garbage", "2.1.310", false},
	} {
		if got := OlderThan(c.v, c.min); got != c.want {
			t.Errorf("OlderThan(%q,%q)=%v want %v", c.v, c.min, got, c.want)
		}
	}
}
