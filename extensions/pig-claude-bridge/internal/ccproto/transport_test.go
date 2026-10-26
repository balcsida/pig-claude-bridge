package ccproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeclaude")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakeclaude")
	if out, err := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakeclaude").CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// startFake starts the fake binary with the given FAKECLAUDE_* knobs and returns the process and its output dir.
func startFake(t *testing.T, knobs map[string]string, h Handlers) (*Process, string) {
	t.Helper()
	out := t.TempDir()
	env := []string{"FAKECLAUDE_OUT=" + out}
	for k, v := range knobs {
		if k == "FAKECLAUDE_SCRIPT" {
			v = filepath.Join("testdata", "scripts", v)
			if abs, err := filepath.Abs(v); err == nil {
				v = abs
			}
		}
		env = append(env, k+"="+v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Start(ctx, Options{Executable: fakeBin, Cwd: t.TempDir(), Env: env}, h)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p.Close(ctx)
	})
	return p, out
}

func shortGraces(t *testing.T) {
	t.Helper()
	cg, kg := CloseGrace, KillGrace
	CloseGrace, KillGrace = 100*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { CloseGrace, KillGrace = cg, kg })
}

// waitStdin polls the fake's stdin log until ok accepts the parsed lines.
func waitStdin(t *testing.T, out string, ok func([]map[string]any) bool) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var lines []map[string]any
		data, _ := os.ReadFile(filepath.Join(out, "stdin.jsonl"))
		for _, l := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var m map[string]any
			if json.Unmarshal(l, &m) == nil {
				lines = append(lines, m)
			}
		}
		if ok(lines) {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for stdin lines; have %v", lines)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func responseFor(lines []map[string]any, id string) map[string]any {
	for _, l := range lines {
		if l["type"] == "control_response" {
			if r, _ := l["response"].(map[string]any); r["request_id"] == id {
				return r
			}
		}
	}
	return nil
}

func TestInitializeIsFirstFrameAndAwaited(t *testing.T) {
	p, out := startFake(t, nil, Handlers{})
	if err := p.WriteUser(UserMessage{Content: "hi", UUID: "u-1"}); err != nil {
		t.Fatal(err)
	}
	lines := waitStdin(t, out, func(l []map[string]any) bool { return len(l) >= 2 })
	req, _ := lines[0]["request"].(map[string]any)
	if lines[0]["type"] != "control_request" || req["subtype"] != "initialize" {
		t.Fatalf("first frame = %v", lines[0])
	}
	if lines[1]["type"] != "user" || lines[1]["uuid"] != "u-1" {
		t.Fatalf("second frame = %v", lines[1])
	}
	argv, _ := os.ReadFile(filepath.Join(out, "argv.json"))
	if !strings.Contains(string(argv), "--input-format") {
		t.Fatalf("argv = %s", argv)
	}
}

func TestMcpMessageServedDuringInitialize(t *testing.T) {
	type call struct{ server, message string }
	calls := make(chan call, 1)
	_, out := startFake(t, map[string]string{"FAKECLAUDE_MCP_BEFORE_INIT": "1"}, Handlers{
		OnMcpMessage: func(_ context.Context, server string, msg json.RawMessage) (json.RawMessage, bool, error) {
			calls <- call{server, string(msg)}
			return json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`), true, nil
		},
	})
	got := <-calls // Start returned, so the handler has run
	lines := waitStdin(t, out, func(l []map[string]any) bool { return len(l) >= 2 })
	if req, _ := lines[0]["request"].(map[string]any); req["subtype"] != "initialize" {
		t.Fatalf("first frame = %v", lines[0])
	}
	r := responseFor(lines[1:], "m0")
	inner, _ := r["response"].(map[string]any)
	if r["subtype"] != "success" || inner["mcp_response"] == nil {
		t.Fatalf("mcp reply = %v", lines[1])
	}
	if got.server != "srv" || !strings.Contains(got.message, `"method":"initialize"`) {
		t.Fatalf("handler saw %+v", got)
	}
}

func TestEchoedControlResponseIsDropped(t *testing.T) {
	p, out := startFake(t, map[string]string{
		"FAKECLAUDE_MCP_BEFORE_INIT": "1", "FAKECLAUDE_ECHO": "1", "FAKECLAUDE_SCRIPT": "frames_with_keepalive.jsonl",
	}, Handlers{
		OnMcpMessage: func(context.Context, string, json.RawMessage) (json.RawMessage, bool, error) {
			return json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`), true, nil
		},
	})
	_ = p.EndInput()
	frames := 0
	for f := range p.Frames() {
		frames++
		if strings.Contains(string(f), "control_response") {
			t.Fatalf("echoed control_response leaked into Frames: %s", f)
		}
	}
	// The echo went out before initialize was answered, ahead of the script's frames.
	if _, err := os.Stat(filepath.Join(out, "echoed")); err != nil || frames != 3 {
		t.Fatalf("vacuous run: echoed marker %v, %d script frames", err, frames)
	}
}

// The SDK parses a line and switches on its type, so a frame whose other fields have
// unexpected types is still a message; only an unreadable control frame is dropped.
func TestFramesWithUnexpectedFieldTypesAreDelivered(t *testing.T) {
	p, _ := startFake(t, map[string]string{"FAKECLAUDE_SCRIPT": "odd_frames.jsonl"}, Handlers{})
	_ = p.EndInput()
	var got []string
	for f := range p.Frames() {
		got = append(got, string(f))
	}
	want := []string{`{"type":"assistant","request_id":7,"n":1}`, `{"type":5,"n":2}`, `{"type":"result","response":"x","n":3}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("frames:\n%s", strings.Join(got, "\n"))
	}
}

func TestFramesDeliverStdoutInOrderAndSkipKeepAlive(t *testing.T) {
	p, _ := startFake(t, map[string]string{"FAKECLAUDE_SCRIPT": "frames_with_keepalive.jsonl"}, Handlers{})
	_ = p.EndInput()
	var types []string
	for f := range p.Frames() {
		var h struct {
			Type string `json:"type"`
			N    int    `json:"n"`
		}
		if err := json.Unmarshal(f, &h); err != nil {
			t.Fatal(err)
		}
		types = append(types, h.Type)
	}
	if got := strings.Join(types, ","); got != "assistant,system,result" {
		t.Fatalf("frames = %s", got)
	}
}

func TestCloseEndsStdinThenTermThenKill(t *testing.T) {
	shortGraces(t)
	p, out := startFake(t, map[string]string{"FAKECLAUDE_IGNORE_TERM": "1"}, Handlers{})
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"stdin_closed", "term_received"} {
		if _, err := os.Stat(filepath.Join(out, marker)); err != nil {
			t.Errorf("%s: %v", marker, err)
		}
	}
	if err := p.Wait(); err == nil || err.Error() != "Claude Code process terminated by signal SIGKILL" {
		t.Fatalf("Wait = %v", err)
	}
}

func TestCloseWithAnExpiredContextStillStopsTheChild(t *testing.T) {
	p, _ := startFake(t, map[string]string{"FAKECLAUDE_IGNORE_TERM": "1"}, Handlers{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the child outlived a Close whose context had expired")
	}
	if err := p.Wait(); err == nil || err.Error() != "Claude Code process terminated by signal SIGKILL" {
		t.Fatalf("Wait = %v", err)
	}
}

func TestShutdownKillsEveryTrackedChild(t *testing.T) {
	p, _ := startFake(t, map[string]string{"FAKECLAUDE_HOLD": "1"}, Handlers{})
	_ = p.EndInput() // the fake keeps running after its input ends, as Claude Code parked at a tool call does
	isTracked := func() bool {
		trackedMu.Lock()
		defer trackedMu.Unlock()
		_, ok := tracked[p]
		return ok
	}
	if !isTracked() {
		t.Fatal("a running child is not tracked")
	}
	Shutdown()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown left the child running")
	}
	if err := p.Wait(); err == nil || err.Error() != "Claude Code process terminated by signal SIGKILL" {
		t.Fatalf("Wait = %v", err)
	}
	if isTracked() {
		t.Fatal("an exited child is still tracked")
	}
}

func TestSignalNameFallsBackToTheNumber(t *testing.T) {
	if got := signalName(syscall.SIGPIPE); got != "SIGPIPE" {
		t.Errorf("SIGPIPE = %q", got)
	}
	if got := signalName(syscall.Signal(99)); got != "99" {
		t.Errorf("signal 99 = %q", got)
	}
}

func TestStderrTailKeepsTheLast2048Characters(t *testing.T) {
	stderr := strings.Repeat("a", 1000) + strings.Repeat("é", 2048)
	p, _ := startFake(t, map[string]string{"FAKECLAUDE_STDERR": stderr}, Handlers{})
	_ = p.EndInput()
	_ = p.Wait()
	if got := p.StderrTail(); got != strings.Repeat("é", 2048) {
		t.Fatalf("tail has %d runes, starts %q", len([]rune(got)), got[:min(len(got), 8)])
	}
}

func TestCloseGracefulExit(t *testing.T) {
	p, _ := startFake(t, nil, Handlers{})
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait = %v", err)
	}
}

func TestWaitExitErrorTexts(t *testing.T) {
	var forwarded strings.Builder
	p, _ := startFake(t, map[string]string{"FAKECLAUDE_EXIT_CODE": "3", "FAKECLAUDE_STDERR": "boom"}, Handlers{
		OnStderr: func(chunk string) { forwarded.WriteString(chunk) },
	})
	_ = p.EndInput()
	if err := p.Wait(); err == nil || err.Error() != "Claude Code process exited with code 3. stderr: boom" {
		t.Fatalf("Wait = %v", err)
	}
	if p.StderrTail() != "boom" || forwarded.String() != "boom" {
		t.Fatalf("tail %q forwarded %q", p.StderrTail(), forwarded.String())
	}

	shortGraces(t)
	h, _ := startFake(t, map[string]string{"FAKECLAUDE_HOLD": "1"}, Handlers{})
	_ = h.Close(context.Background())
	if err := h.Wait(); err == nil || err.Error() != "Claude Code process terminated by signal SIGTERM" {
		t.Fatalf("Wait = %v", err)
	}
}

func TestWriteAfterCloseErrors(t *testing.T) {
	p, _ := startFake(t, nil, Handlers{})
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteUser(UserMessage{Content: "late"}); err == nil {
		t.Fatal("WriteUser after Close succeeded")
	}
}

func TestUserMessageShape(t *testing.T) {
	b, _ := json.Marshal(UserMessage{Content: "hi"})
	if string(b) != `{"type":"user","session_id":"","message":{"role":"user","content":"hi"},"parent_tool_use_id":null}` {
		t.Fatalf("got %s", b)
	}
}

func TestStartEarlyExitReportsExitTextAndStderr(t *testing.T) {
	_, err := Start(context.Background(), Options{Executable: fakeBin, Cwd: t.TempDir(), Env: []string{
		"FAKECLAUDE_EXIT_BEFORE_INIT=1", "FAKECLAUDE_EXIT_CODE=3", "FAKECLAUDE_STDERR=bad flag",
	}}, Handlers{})
	if err == nil || !strings.Contains(err.Error(), "Claude Code process exited with code 3. stderr: bad flag") {
		t.Fatalf("Start error = %v", err)
	}
}

func TestCloseReturnsWhileWriteIsBlocked(t *testing.T) {
	shortGraces(t)
	p, _ := startFake(t, map[string]string{"FAKECLAUDE_STOP_READING": "1"}, Handlers{})
	wrote := make(chan error, 1)
	go func() { wrote <- p.WriteUser(UserMessage{Content: strings.Repeat("x", 4<<20)}) }()
	// The write holds wmu once the frame is encoded (slow under -race); the 4 MB frame cannot fit the pipe.
	for deadline := time.Now().Add(5 * time.Second); p.wmu.TryLock(); {
		p.wmu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the write never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-wrote:
		t.Fatalf("the write was not blocked: %v", err)
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatalf("Close = %v", err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("child still running after Close")
	}
	// A pipe error, not "claude process is closed": the write was under way when Close ran.
	if err := <-wrote; err == nil || !strings.HasPrefix(err.Error(), "write to claude: ") {
		t.Fatalf("blocked WriteUser = %v", err)
	}
}

func TestWaitIsBoundedWhenGrandchildHoldsPipes(t *testing.T) {
	cg, dg := CloseGrace, DrainGrace
	CloseGrace, DrainGrace = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { CloseGrace, DrainGrace = cg, dg })
	p, out := startFake(t, map[string]string{"FAKECLAUDE_GRANDCHILD": "1"}, Handlers{})
	_ = p.EndInput()
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed while a grandchild holds the pipes")
	}
	if argv, _ := os.ReadFile(filepath.Join(out, "argv.json")); !strings.Contains(string(argv), "--input-format") {
		t.Fatalf("the grandchild overwrote the fake's argv.json: %s", argv)
	}
	select {
	case _, ok := <-p.Frames():
		if ok {
			t.Fatal("unexpected frame")
		}
	case <-time.After(time.Second):
		t.Fatal("Frames not closed")
	}
}

func TestStartCloseCyclesDoNotLeakDescriptors(t *testing.T) {
	count := func() int {
		entries, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Skipf("no /dev/fd: %v", err)
		}
		return len(entries)
	}
	cycle := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := Start(ctx, Options{Executable: fakeBin, Cwd: t.TempDir()}, Handlers{})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Close(ctx); err != nil {
			t.Fatal(err)
		}
		<-p.Done()
	}
	cycle() // warm up runtime-owned descriptors
	first := count()
	for i := 0; i < 10; i++ {
		cycle()
	}
	last := count()
	t.Logf("open descriptors: first %d, last %d", first, last)
	if last > first+2 {
		t.Fatalf("descriptors grew from %d to %d over 10 cycles", first, last)
	}
}

func TestPanickingStderrHandlerIsContained(t *testing.T) {
	called := make(chan struct{}, 4)
	p, _ := startFake(t, map[string]string{"FAKECLAUDE_STDERR": "boom"}, Handlers{OnStderr: func(string) {
		called <- struct{}{}
		panic("stderr handler exploded")
	}})
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("OnStderr never ran")
	}
	if _, err := p.Interrupt(context.Background()); err != nil {
		t.Fatalf("the transport stopped serving after the panic: %v", err)
	}
}
