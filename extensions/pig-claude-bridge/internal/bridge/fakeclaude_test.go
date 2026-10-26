package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
)

// fakeClaude runs ccproto's fake Claude Code (internal/ccproto/testdata/fakeclaude) in
// place of the configured binary, so a test drives the real transport.
type fakeClaude struct {
	bin, out string
	knobs    []string
	started  chan *ccproto.Process
}

// newFakeClaude builds the fake; knobs are its FAKECLAUDE_* settings.
func newFakeClaude(t *testing.T, knobs ...string) *fakeClaude {
	t.Helper()
	f := &fakeClaude{bin: filepath.Join(t.TempDir(), "fakeclaude"), out: t.TempDir(), knobs: knobs, started: make(chan *ccproto.Process, 8)}
	if out, err := exec.Command("go", "build", "-o", f.bin, "../ccproto/testdata/fakeclaude").CombinedOutput(); err != nil {
		t.Fatalf("build fakeclaude: %v\n%s", err, out)
	}
	return f
}

// spawn is a Deps.Spawn that starts the fake with the bridge's options.
func (f *fakeClaude) spawn(ctx context.Context, o ccproto.Options, h ccproto.Handlers) (ccproto.Client, error) {
	o.Executable = f.bin
	o.Env = append(append([]string{}, o.Env...), append([]string{"FAKECLAUDE_OUT=" + f.out}, f.knobs...)...)
	p, err := ccproto.Start(ctx, o, h)
	if err != nil {
		return nil, err
	}
	f.started <- p
	return p, nil
}

// awaitLaunch waits until the fake process has started (it records its argv first).
func (f *fakeClaude) awaitLaunch(t *testing.T) {
	t.Helper()
	waitFor(t, "fakeclaude to start", func() bool {
		_, err := os.Stat(filepath.Join(f.out, "argv.json"))
		return err == nil
	})
}

// gone fails unless p has exited within a few seconds.
func gone(t *testing.T, p *ccproto.Process) {
	t.Helper()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the Claude Code child is still running")
	}
}
