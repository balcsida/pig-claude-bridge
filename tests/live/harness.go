//go:build live

// Package live holds the RPC-driven live integration tests: they run the real pig with the extension
// loaded and the real Claude Code behind it, so they spend quota and are never part of a normal test run.
package live

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Event is one decoded JSON line from pig's stdout (a response or an event).
type Event = map[string]any

// Options mirror the TypeScript createRpcHarness options.
type Options struct {
	Name           string   // used for log file names
	Args           []string // extra pig CLI args, after --mode rpc
	Env            map[string]string
	Cwd            string        // default: <work>/project
	DefaultTimeout time.Duration // default 30s
}

type listener struct {
	id int
	fn func(Event)
}

// Harness drives one `pig --mode rpc` process.
type Harness struct {
	Dir      string // repository root, the extension that pig loads
	LogDir   string
	RPCLog   string
	DebugLog string

	t    *testing.T
	opts Options

	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	exited      chan struct{}
	rpcLog      *os.File
	listeners   []listener
	nextID      int
	reqID       int
	stopped     bool
	startedOnce bool
}

var (
	envOnce  sync.Once
	rootDir  string
	workDir  string
	logDir   string
	envError error
)

// liveEnv resolves the repository root, work directory and log directory once per test process, creates
// the run's PiG home and probes that Claude Code's config dir is writable.
func liveEnv() (root, work, logs string, err error) {
	envOnce.Do(func() { rootDir, workDir, logDir, envError = initLiveEnv() })
	return rootDir, workDir, logDir, envError
}

func initLiveEnv() (root, work, logs string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "scripts", "live", "lib.sh")); statErr == nil {
			root = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", "", errors.New("repository root (scripts/live/lib.sh) not found above the working directory")
		}
		dir = parent
	}
	work = os.Getenv("CLAUDE_BRIDGE_TEST_WORK_DIR")
	if work == "" {
		work = filepath.Join(root, "scripts", "live", "work", time.Now().Format("20060102-150405"))
	}
	logs = os.Getenv("CLAUDE_BRIDGE_TEST_LOG_DIR")
	if logs == "" {
		logs = filepath.Join(work, "logs")
	}
	agentDir := filepath.Join(work, "home", "pig", "agent")
	for _, d := range []string{logs, agentDir, filepath.Join(work, "project")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", "", "", err
		}
	}
	settings := filepath.Join(agentDir, "settings.json")
	if _, statErr := os.Stat(settings); statErr != nil {
		if err := os.WriteFile(settings, []byte(`{"defaultProvider":"claude-bridge","defaultModel":"claude-haiku-4-5"}`+"\n"), 0o644); err != nil {
			return "", "", "", err
		}
	}
	return root, work, logs, probeClaudeDir()
}

// probeClaudeDir mirrors the TypeScript harness: Claude Code persists session state under its config dir,
// and a sandbox that blocks those writes fails the next turn's --resume, which reads like a bridge bug.
func probeClaudeDir() error {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".claude")
	}
	probe := filepath.Join(dir, ".int-test-write-probe-"+strconv.Itoa(os.Getpid()))
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = os.WriteFile(probe, nil, 0o644)
	}
	if err == nil {
		err = os.Remove(probe)
	}
	if err != nil {
		code := err.Error()
		var errno syscall.Errno
		if errors.As(err, &errno) {
			switch errno {
			case syscall.EACCES:
				code = "EACCES"
			case syscall.EPERM:
				code = "EPERM"
			case syscall.EROFS:
				code = "EROFS"
			case syscall.ENOENT:
				code = "ENOENT"
			}
		}
		return fmt.Errorf("Integration tests need write access to %s for Claude Code session state (got %s). "+
			"Re-run outside the sandbox, or point CLAUDE_CONFIG_DIR at a writable directory.", dir, code)
	}
	return nil
}

// New builds a harness and registers Stop as a test cleanup.
func New(t *testing.T, opts Options) *Harness {
	t.Helper()
	root, work, logs, err := liveEnv()
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultTimeout == 0 {
		opts.DefaultTimeout = 30 * time.Second
	}
	if opts.Cwd == "" {
		opts.Cwd = filepath.Join(work, "project")
	}
	h := &Harness{
		Dir:      root,
		LogDir:   logs,
		RPCLog:   filepath.Join(logs, opts.Name+".log"),
		DebugLog: filepath.Join(logs, opts.Name+"-debug.log"),
		t:        t,
		opts:     opts,
	}
	t.Cleanup(h.Stop)
	return h
}

// Pig returns the pig process command (nil before Start).
func (h *Harness) Pig() *exec.Cmd {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cmd
}

func (h *Harness) environ() []string {
	_, work, logs, _ := liveEnv()
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	delete(env, "PI_CODING_AGENT_DIR")
	delete(env, "PIG_USE_PI_DIRS")
	env["PIG_HOME"] = filepath.Join(work, "home", "pig")
	env["PIG_CODING_AGENT_DIR"] = filepath.Join(work, "home", "pig", "agent")
	env["NO_COLOR"] = "1"
	env["TERM"] = "dumb"
	env["CLAUDE_BRIDGE_DEBUG"] = "1"
	env["CLAUDE_BRIDGE_DEBUG_PATH"] = h.DebugLog
	env["CLAUDE_BRIDGE_TEST_WORK_DIR"] = work
	env["CLAUDE_BRIDGE_TEST_LOG_DIR"] = logs
	for k, v := range h.opts.Env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Start spawns pig. The debug log is cleared on the first start only, so markers from an earlier start of
// the same harness (e.g. after a child crash) survive for the final log gate.
func (h *Harness) Start() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.startedOnce {
		if err := os.WriteFile(h.DebugLog, nil, 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.startedOnce = true
	h.stopped = false
	var err error
	if h.rpcLog, err = os.OpenFile(h.RPCLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		h.t.Fatal(err)
	}
	args := append([]string{"--no-session", "-ne", "-e", h.Dir, "--mode", "rpc"}, h.opts.Args...)
	cmd := exec.Command("pig", args...)
	cmd.Dir = h.opts.Cwd
	cmd.Env = h.environ()
	// Own pipes rather than StdoutPipe: Wait must return when pig exits even if a leaked descendant still
	// holds the write end, and the readers then end only at EOF.
	outR, outW, err := os.Pipe()
	if err != nil {
		h.t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		h.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if h.stdin, err = cmd.StdinPipe(); err != nil {
		h.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start pig: %v", err)
	}
	outW.Close()
	errW.Close()
	h.cmd = cmd
	h.exited = make(chan struct{})
	exited := h.exited
	go func() { _ = cmd.Wait(); close(exited) }()
	go h.readStderr(errR)
	go h.readStdout(outR)
}

func (h *Harness) logf(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rpcLog != nil {
		fmt.Fprintf(h.rpcLog, format, args...)
	}
}

func (h *Harness) readStderr(r *os.File) {
	defer r.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.mu.Lock()
			if !h.stopped && h.rpcLog != nil {
				h.rpcLog.Write(buf[:n])
			}
			h.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// readStdout decodes JSON lines; a line that is not JSON is dropped, as in the TypeScript harness.
func (h *Harness) readStdout(r *os.File) {
	defer r.Close()
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			var msg Event
			if json.Unmarshal([]byte(line), &msg) == nil {
				h.mu.Lock()
				if h.stopped {
					h.mu.Unlock()
					return
				}
				if h.rpcLog != nil {
					fmt.Fprintf(h.rpcLog, "< %s\n", line)
				}
				ls := append([]listener(nil), h.listeners...)
				h.mu.Unlock()
				for _, l := range ls {
					l.fn(msg)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// StartAndWait starts pig and gives it d (default 2s) to come up.
func (h *Harness) StartAndWait(d time.Duration) {
	h.t.Helper()
	if d == 0 {
		d = 2 * time.Second
	}
	h.Start()
	time.Sleep(d)
}

// Stop records pig's descendants, terminates pig, waits for it, and fails the test when any recorded
// descendant is still alive 10 s later. Safe to call twice.
func (h *Harness) Stop() {
	h.mu.Lock()
	cmd, exited := h.cmd, h.exited
	if cmd == nil || h.stopped {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	recorded := Descendants(cmd.Process.Pid)
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}

	delete(recorded, cmd.Process.Pid)
	deadline := time.Now().Add(10 * time.Second)
	var alive map[int]string
	for {
		alive = map[int]string{}
		for pid, command := range recorded {
			if cmdline, ok := ProcessCommand(pid); ok && cmdline == command {
				alive[pid] = command
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for pid, command := range alive {
		h.t.Errorf("process survived pig's shutdown: pid %d: %s", pid, command)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	h.mu.Lock()
	if h.rpcLog != nil {
		h.rpcLog.Close()
		h.rpcLog = nil
	}
	h.mu.Unlock()
}

// Descendants returns pid -> command line for root and everything below it.
func Descendants(root int) map[int]string {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return map[int]string{}
	}
	children := map[int][]int{}
	commands := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
		commands[pid] = strings.Join(f[2:], " ")
	}
	result := map[int]string{}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if _, seen := result[pid]; seen {
			continue
		}
		result[pid] = commands[pid]
		queue = append(queue, children[pid]...)
	}
	return result
}

// ProcessCommand returns the command line of a live process.
func ProcessCommand(pid int) (string, bool) {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false
	}
	cmdline := strings.Join(strings.Fields(string(out)), " ")
	return cmdline, cmdline != ""
}

// AddListener registers fn for every decoded line; fn runs on the reader goroutine and must not block.
func (h *Harness) AddListener(fn func(Event)) (remove func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	h.listeners = append(h.listeners, listener{id, fn})
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for i, l := range h.listeners {
			if l.id == id {
				h.listeners = append(h.listeners[:i], h.listeners[i+1:]...)
				return
			}
		}
	}
}

// ClearListeners drops every listener.
func (h *Harness) ClearListeners() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listeners = nil
}

func (h *Harness) exitedChan() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exited
}

// await waits for ch, the timeout or pig's exit; timeoutMsg is the error text on timeout.
func (h *Harness) await(ch <-chan Event, timeout time.Duration, timeoutMsg string) (Event, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case msg := <-ch:
		return msg, nil
	case <-timer.C:
		return nil, errors.New(timeoutMsg)
	case <-h.exitedChan():
		return nil, fmt.Errorf("%s (pig exited)", timeoutMsg)
	}
}

// listenFor registers a listener that forwards the first line pred accepts.
func (h *Harness) listenFor(pred func(Event) bool) (<-chan Event, func()) {
	ch := make(chan Event, 1)
	remove := h.AddListener(func(msg Event) {
		if pred(msg) {
			select {
			case ch <- msg:
			default:
			}
		}
	})
	return ch, remove
}

// Send writes one command with a fresh id and returns the matching response's data. A response with
// success:false is an error.
func (h *Harness) Send(cmd map[string]any, timeout time.Duration) (any, error) {
	if timeout == 0 {
		timeout = h.opts.DefaultTimeout
	}
	h.mu.Lock()
	h.reqID++
	id := fmt.Sprintf("req_%d", h.reqID)
	h.mu.Unlock()
	full := map[string]any{}
	for k, v := range cmd {
		full[k] = v
	}
	full["id"] = id
	line, err := json.Marshal(full)
	if err != nil {
		return nil, err
	}
	typ, _ := cmd["type"].(string)
	ch, remove := h.listenFor(func(m Event) bool { return m["type"] == "response" && m["id"] == id })
	defer remove()
	h.logf("> %s\n", line)
	h.mu.Lock()
	stdin := h.stdin
	h.mu.Unlock()
	if stdin == nil {
		return nil, fmt.Errorf("%s: pig not started", typ)
	}
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("%s: %w", typ, err)
	}
	msg, err := h.await(ch, timeout, "Timeout: "+typ)
	if err != nil {
		return nil, err
	}
	if ok, _ := msg["success"].(bool); !ok {
		return nil, fmt.Errorf("%s: %v", typ, msg["error"])
	}
	return msg["data"], nil
}

// WaitForEvent waits for the next line whose type is typ.
func (h *Harness) WaitForEvent(typ string, timeout time.Duration) (Event, error) {
	return h.WaitForMatch(func(m Event) bool { return m["type"] == typ }, typ, timeout)
}

// WaitForMatch waits for the next line pred accepts.
func (h *Harness) WaitForMatch(pred func(Event) bool, description string, timeout time.Duration) (Event, error) {
	if timeout == 0 {
		timeout = h.opts.DefaultTimeout
	}
	ch, remove := h.listenFor(pred)
	defer remove()
	return h.await(ch, timeout, "Timeout waiting for "+description)
}

// CollectText accumulates assistant text deltas until the returned stop func is called.
func (h *Harness) CollectText() (stop func() string) {
	var mu sync.Mutex
	var text strings.Builder
	remove := h.AddListener(func(msg Event) {
		if msg["type"] != "message_update" {
			return
		}
		ae, _ := msg["assistantMessageEvent"].(map[string]any)
		if ae["type"] == "text_delta" {
			delta, _ := ae["delta"].(string)
			mu.Lock()
			text.WriteString(delta)
			mu.Unlock()
		}
	})
	return func() string {
		remove()
		mu.Lock()
		defer mu.Unlock()
		return text.String()
	}
}

// PromptAndWait sends a prompt and returns the streamed assistant text once agent_end arrives.
func (h *Harness) PromptAndWait(message string, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = h.opts.DefaultTimeout
	}
	stop := h.CollectText()
	// Listen before sending so an agent_end that follows the response immediately cannot be missed.
	end, remove := h.listenFor(func(m Event) bool { return m["type"] == "agent_end" })
	defer remove()
	if _, err := h.Send(map[string]any{"type": "prompt", "message": message}, timeout); err != nil {
		stop()
		return "", err
	}
	_, err := h.await(end, timeout, "Timeout waiting for agent_end")
	return stop(), err
}
