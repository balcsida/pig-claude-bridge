package ccproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// CloseGrace is how long Close waits after ending stdin before SIGTERM; KillGrace is the wait before SIGKILL.
var (
	CloseGrace = 2 * time.Second
	KillGrace  = 5 * time.Second
	// DrainGrace bounds how long the pipes may stay open after the child exits (a grandchild can inherit them).
	DrainGrace = 200 * time.Millisecond
)

const stderrTailChars = 2048

// Handlers are the consumer callbacks; both are optional.
type Handlers struct {
	OnMcpMessage func(ctx context.Context, serverName string, message json.RawMessage) (json.RawMessage, bool, error)
	OnStderr     func(chunk string)
}

// Client is the process surface the bridge consumes.
type Client interface {
	Frames() <-chan json.RawMessage
	WriteUser(msg UserMessage) error
	EndInput() error
	Interrupt(ctx context.Context) (json.RawMessage, error)
	Close(ctx context.Context) error
	Wait() error
	StderrTail() string
	Done() <-chan struct{}
}

// Spawn is the injection point the bridge uses to start a Client.
var Spawn = func(ctx context.Context, o Options, h Handlers) (Client, error) { return Start(ctx, o, h) }

var (
	trackedMu sync.Mutex
	tracked   = map[*Process]struct{}{}
)

// Shutdown kills every child still running. The bridge calls it from session_shutdown, which PiG
// sends before the extension process goes away.
func Shutdown() {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	for p := range tracked {
		_ = p.cmd.Process.Kill()
	}
}

// Process is one running Claude Code child.
//
// Goroutines: readStdout (dispatches frames and control traffic), readStderr, pump (delivers queued frames
// to Frames without ever blocking the reader), and wait (reaps the child, then closes done). One goroutine
// per inbound control request, cancelled when stdout ends.
type Process struct {
	cmd      *exec.Cmd
	handlers Handlers
	ctx      context.Context
	cancel   context.CancelFunc

	wmu        sync.Mutex // serializes stdin writes; held across a blocked Write, so never taken by Close
	fmu        sync.Mutex // guards stdinEnded, closed; never held across I/O
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	stdinEnded bool
	closed     bool

	mu           sync.Mutex // guards pending, inflight, stdoutClosed, tail, waitErr
	pending      map[string]chan controlResult
	inflight     map[string]context.CancelFunc
	stdoutClosed bool
	tail         []rune
	waitErr      error

	qmu       sync.Mutex // guards queue, eof
	queue     []json.RawMessage
	eof       bool
	wake      chan struct{}
	frames    chan json.RawMessage
	abandon   chan struct{}
	abandonMu sync.Once
	closeOnce sync.Once

	stdoutDone, stderrDone chan struct{}
	done                   chan struct{}
}

// Start spawns the child, writes initialize first and returns once its response arrives.
func Start(ctx context.Context, o Options, h Handlers) (*Process, error) {
	name, args, err := commandLine(o.Executable, BuildArgs(o)...)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = o.Cwd
	cmd.Env = BuildEnv(os.Environ(), o)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", o.Executable, err)
	}
	pctx, cancel := context.WithCancel(context.Background())
	p := &Process{
		cmd: cmd, handlers: h, ctx: pctx, cancel: cancel, stdin: stdin, stdout: stdout, stderr: stderr,
		pending: map[string]chan controlResult{}, inflight: map[string]context.CancelFunc{},
		wake: make(chan struct{}, 1), frames: make(chan json.RawMessage), abandon: make(chan struct{}),
		stdoutDone: make(chan struct{}), stderrDone: make(chan struct{}), done: make(chan struct{}),
	}
	trackedMu.Lock()
	tracked[p] = struct{}{}
	trackedMu.Unlock()
	go p.readStdout(stdout)
	go p.readStderr(stderr)
	go p.pump()
	go p.wait()

	id := newRequestID()
	ch, err := p.register(id)
	if err == nil {
		err = p.write(BuildInitialize(o, id))
	}
	if err != nil {
		return nil, p.failStart(err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, p.failStart(r.err)
		}
		return p, nil
	case <-ctx.Done():
		return nil, p.failStart(ctx.Err())
	}
}

// failStart tears the child down and, when it exited on its own, appends its exit text (with the stderr tail).
func (p *Process) failStart(err error) error {
	err = fmt.Errorf("initialize: %w", err)
	if p.abort() {
		return err
	}
	if werr := p.Wait(); werr != nil {
		return fmt.Errorf("%w: %v", err, werr)
	}
	return err
}

// abort gives the child DrainGrace to exit by itself, then kills it and waits for it to be reaped.
// It reports whether it had to kill.
func (p *Process) abort() (killed bool) {
	select {
	case <-p.done:
	case <-time.After(DrainGrace):
		_ = p.cmd.Process.Kill()
		killed = true
		<-p.done
	}
	p.abandonFrames()
	return killed
}

func (p *Process) abandonFrames() { p.abandonMu.Do(func() { close(p.abandon) }) }

func (p *Process) write(line []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.fmu.Lock()
	closed, ended := p.closed, p.stdinEnded
	p.fmu.Unlock()
	if closed {
		return errors.New("claude process is closed")
	}
	if ended {
		return nil // writes to ended stdin are dropped (reference 1.4)
	}
	if _, err := p.stdin.Write(append(line[:len(line):len(line)], '\n')); err != nil {
		return fmt.Errorf("write to claude: %w", err)
	}
	return nil
}

// WriteUser sends one user turn.
func (p *Process) WriteUser(msg UserMessage) error { return p.writeJSON(msg) }

// EndInput closes stdin; later writes are dropped.
func (p *Process) EndInput() error {
	p.fmu.Lock()
	ended := p.stdinEnded
	p.stdinEnded = true
	p.fmu.Unlock()
	if ended {
		return nil
	}
	return p.stdin.Close()
}

// Frames delivers every non-control stdout frame in order and closes on stdout EOF.
func (p *Process) Frames() <-chan json.RawMessage { return p.frames }

// Done is closed once the child has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait blocks until exit and returns the SDK's exit error texts (nil for exit 0).
func (p *Process) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

// StderrTail returns the last 2048 characters of stderr.
func (p *Process) StderrTail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.tail)
}

// Close ends stdin, then escalates to SIGTERM after CloseGrace and SIGKILL after a further KillGrace.
func (p *Process) Close(ctx context.Context) error {
	p.fmu.Lock()
	p.closed = true
	ended := p.stdinEnded
	p.stdinEnded = true
	p.fmu.Unlock()
	if !ended {
		_ = p.stdin.Close() // outside the lock: closing the pipe also unblocks a pending write
	}
	defer p.abandonFrames()

	for _, step := range []struct {
		wait time.Duration
		sig  syscall.Signal
	}{{CloseGrace, syscall.SIGTERM}, {KillGrace, syscall.SIGKILL}} {
		timer := time.NewTimer(step.wait)
		select {
		case <-p.done:
			timer.Stop()
			return nil
		case <-ctx.Done():
			timer.Stop()
			// Nothing escalates once Close returns, so a caller that stops waiting must not leave the child running.
			_ = p.cmd.Process.Kill()
			return ctx.Err()
		case <-timer.C:
			_ = p.cmd.Process.Signal(step.sig)
		}
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Process) readStdout(r io.Reader) {
	defer close(p.stdoutDone)
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			p.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	p.cancel()
	p.failPending()
	p.qmu.Lock()
	p.eof = true
	p.qmu.Unlock()
	p.notify()
}

func (p *Process) dispatch(line []byte) {
	var frame struct {
		Type json.RawMessage `json:"type"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return // non-JSON stdout is ignored (reference 1.4)
	}
	// Only the type is read up front, as the SDK switches on it: a frame whose other fields
	// have unexpected types still reaches Frames. A non-string type is no control frame.
	var typ string
	_ = json.Unmarshal(frame.Type, &typ)
	switch typ {
	case "control_request", "control_response", "control_cancel_request":
		var h frameHead
		if json.Unmarshal(line, &h) != nil {
			return // a control frame whose ids are not strings cannot be paired or answered
		}
		switch typ {
		case "control_request":
			p.serveRequest(h)
		case "control_response":
			p.deliver(h)
		default:
			p.cancelInflight(h.RequestID)
		}
	case "keep_alive", "transcript_mirror":
	default:
		p.qmu.Lock()
		p.queue = append(p.queue, json.RawMessage(line))
		p.qmu.Unlock()
		p.notify()
	}
}

func (p *Process) notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Process) pump() {
	defer close(p.frames)
	for {
		p.qmu.Lock()
		if len(p.queue) > 0 {
			f := p.queue[0]
			p.queue = p.queue[1:]
			p.qmu.Unlock()
			select {
			case p.frames <- f:
			case <-p.abandon:
				return
			}
			continue
		}
		eof := p.eof
		p.qmu.Unlock()
		if eof {
			return
		}
		select {
		case <-p.wake:
		case <-p.abandon:
			return
		}
	}
}

func (p *Process) readStderr(r io.Reader) {
	defer close(p.stderrDone)
	buf := make([]byte, 4096)
	var carry []byte
	for {
		n, err := r.Read(buf)
		data := append(carry, buf[:n]...)
		cut := completeRunes(data)
		chunk := string(data[:cut])
		carry = append([]byte(nil), data[cut:]...)
		if chunk != "" {
			p.mu.Lock()
			p.tail = append(p.tail, []rune(chunk)...)
			if len(p.tail) > stderrTailChars {
				p.tail = append([]rune(nil), p.tail[len(p.tail)-stderrTailChars:]...)
			}
			p.mu.Unlock()
			p.forwardStderr(chunk)
		}
		if err != nil {
			return
		}
	}
}

// forwardStderr hands chunk to the consumer; a panic there is dropped so the reader keeps draining stderr.
// ccproto has no log of its own: the bridge's consumer recovers and logs its own panics first.
func (p *Process) forwardStderr(chunk string) {
	if p.handlers.OnStderr == nil {
		return
	}
	defer func() { _ = recover() }()
	p.handlers.OnStderr(chunk)
}

// wait reaps the child, gives the pipes DrainGrace to reach EOF (a grandchild may hold them open), then
// publishes the exit error and closes done.
func (p *Process) wait() {
	state, err := p.cmd.Process.Wait()
	timer := time.AfterFunc(DrainGrace, p.closeReaders)
	<-p.stdoutDone
	<-p.stderrDone
	timer.Stop()
	p.closeReaders()
	_ = p.stdin.Close()
	p.cancel()
	trackedMu.Lock()
	delete(tracked, p)
	trackedMu.Unlock()
	p.mu.Lock()
	p.waitErr = p.exitError(state, err)
	p.mu.Unlock()
	close(p.done)
}

// closeReaders closes the pipe read ends exactly once (cmd.Wait is not used, so nothing else does).
func (p *Process) closeReaders() {
	p.closeOnce.Do(func() { _ = p.stdout.Close(); _ = p.stderr.Close() })
}

// exitError renders the SDK's texts; the caller holds p.mu.
func (p *Process) exitError(state *os.ProcessState, err error) error {
	if err != nil {
		return fmt.Errorf("wait for claude: %w", err)
	}
	if state.Success() {
		return nil
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return fmt.Errorf("Claude Code process terminated by signal %s", signalName(ws.Signal()))
	}
	return fmt.Errorf("Claude Code process exited with code %d. stderr: %s", state.ExitCode(), string(p.tail))
}

// signalName is the name Node reports for s, or its number for a signal not listed here.
func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGBUS:
		return "SIGBUS"
	}
	return strconv.Itoa(int(s))
}

// completeRunes returns the length of the prefix of b that does not end inside a multi-byte rune.
func completeRunes(b []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return len(b) - i
			}
			break
		}
	}
	return len(b)
}
