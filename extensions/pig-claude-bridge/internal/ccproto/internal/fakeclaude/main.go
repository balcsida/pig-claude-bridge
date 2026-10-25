// Command fakeclaude is a test double for the claude CLI's stream-json protocol. It is driven by environment
// variables: FAKECLAUDE_OUT (directory for argv.json, env.json, stdin.jsonl, stdin_closed, term_received),
// FAKECLAUDE_SCRIPT (JSONL frames emitted after initialize is answered; {"__wait_for_user":true} blocks until a
// user frame arrives, {"__sleep_ms":N} sleeps, a non-JSON line is emitted verbatim), FAKECLAUDE_MCP_CALL (JSON-RPC
// message sent as an mcp_message request with id "mcp1"), FAKECLAUDE_MCP_BEFORE_INIT=1 (send an mcp_message
// "initialize" request with id "m0" and wait for its reply before answering initialize), FAKECLAUDE_ECHO=1
// (echo every control_response received back on stdout), FAKECLAUDE_IGNORE_TERM=1 (record and ignore SIGTERM and
// keep running after stdin closes), FAKECLAUDE_HOLD=1 (keep running after stdin closes), FAKECLAUDE_STDERR
// (text written to stderr at startup), FAKECLAUDE_EXIT_CODE (exit code once stdin closes),
// FAKECLAUDE_EXIT_BEFORE_INIT=1 (exit with that code right after the stderr text, before reading stdin),
// FAKECLAUDE_STOP_READING=1 (stop reading stdin once initialize is read), FAKECLAUDE_GRANDCHILD=1 (start a
// helper that inherits stdout and stderr and lingers for 3 s).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

var (
	outMu sync.Mutex
	out   = bufio.NewWriter(os.Stdout)
)

func emit(line []byte) {
	outMu.Lock()
	defer outMu.Unlock()
	out.Write(line)
	out.WriteByte('\n')
	out.Flush()
}

func emitJSON(v any) {
	b, _ := json.Marshal(v)
	emit(b)
}

func respond(id string, payload any) {
	emitJSON(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": payload}})
}

func main() {
	dir := os.Getenv("FAKECLAUDE_OUT")
	save := func(name string, v any) {
		if dir == "" {
			return
		}
		b, _ := json.Marshal(v)
		os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	mark := func(name string) {
		if dir != "" {
			os.WriteFile(filepath.Join(dir, name), nil, 0o644)
		}
	}
	save("argv.json", os.Args[1:])
	save("env.json", os.Environ())
	if s := os.Getenv("FAKECLAUDE_STDERR"); s != "" {
		fmt.Fprint(os.Stderr, s)
	}
	if os.Getenv("FAKECLAUDE_GRANDCHILD") == "child" {
		time.Sleep(3 * time.Second)
		return
	}
	if os.Getenv("FAKECLAUDE_GRANDCHILD") == "1" {
		gc := exec.Command(os.Args[0])
		gc.Env = append(os.Environ(), "FAKECLAUDE_GRANDCHILD=child")
		gc.Stdout, gc.Stderr = os.Stdout, os.Stderr
		if err := gc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fakeclaude:", err)
			os.Exit(2)
		}
	}
	if os.Getenv("FAKECLAUDE_EXIT_BEFORE_INIT") == "1" {
		code, _ := strconv.Atoi(os.Getenv("FAKECLAUDE_EXIT_CODE"))
		os.Exit(code)
	}
	hold := os.Getenv("FAKECLAUDE_IGNORE_TERM") == "1" || os.Getenv("FAKECLAUDE_HOLD") == "1"
	if os.Getenv("FAKECLAUDE_IGNORE_TERM") == "1" {
		sig := make(chan os.Signal, 4)
		signal.Notify(sig, syscall.SIGTERM)
		go func() {
			for range sig {
				mark("term_received")
			}
		}()
	}

	var log *os.File
	if dir != "" {
		log, _ = os.OpenFile(filepath.Join(dir, "stdin.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	initReq := make(chan string, 1)
	responses := make(chan string, 64)
	userCh := make(chan struct{}, 64)
	eof := make(chan struct{})
	echo := os.Getenv("FAKECLAUDE_ECHO") == "1"
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(nil, 1<<24)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			if log != nil {
				log.Write(append(line, '\n'))
			}
			var f struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Request   struct {
					Subtype string `json:"subtype"`
				} `json:"request"`
				Response struct {
					RequestID string `json:"request_id"`
				} `json:"response"`
			}
			if json.Unmarshal(line, &f) != nil {
				continue
			}
			switch f.Type {
			case "control_request":
				switch f.Request.Subtype {
				case "initialize":
					initReq <- f.RequestID
					if os.Getenv("FAKECLAUDE_STOP_READING") == "1" {
						time.Sleep(time.Hour)
					}
				case "interrupt":
					respond(f.RequestID, map[string]any{"still_queued": []string{}})
				default:
					respond(f.RequestID, map[string]any{})
				}
			case "control_response":
				if echo {
					emit(line)
				}
				responses <- f.Response.RequestID
			case "user":
				userCh <- struct{}{}
			}
		}
		mark("stdin_closed")
		close(eof)
	}()

	waitResponse := func(id string) {
		for got := range responses {
			if got == id {
				return
			}
		}
	}
	id := <-initReq
	if os.Getenv("FAKECLAUDE_MCP_BEFORE_INIT") == "1" {
		emitJSON(map[string]any{"type": "control_request", "request_id": "m0", "request": map[string]any{
			"subtype": "mcp_message", "server_name": "srv",
			"message": map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}}}})
		waitResponse("m0")
	}
	respond(id, map[string]any{"commands": []string{}})
	if call := os.Getenv("FAKECLAUDE_MCP_CALL"); call != "" {
		emitJSON(map[string]any{"type": "control_request", "request_id": "mcp1", "request": map[string]any{
			"subtype": "mcp_message", "server_name": "srv", "message": json.RawMessage(call)}})
		waitResponse("mcp1")
	}
	if path := os.Getenv("FAKECLAUDE_SCRIPT"); path != "" {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakeclaude:", err)
			os.Exit(2)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<24)
		for sc.Scan() {
			var d struct {
				WaitForUser bool `json:"__wait_for_user"`
				SleepMs     *int `json:"__sleep_ms"`
			}
			json.Unmarshal(sc.Bytes(), &d)
			switch {
			case d.WaitForUser:
				<-userCh
			case d.SleepMs != nil:
				time.Sleep(time.Duration(*d.SleepMs) * time.Millisecond)
			default:
				emit(append([]byte(nil), sc.Bytes()...))
			}
		}
		f.Close()
	}
	<-eof
	if hold {
		time.Sleep(time.Hour)
	}
	code, _ := strconv.Atoi(os.Getenv("FAKECLAUDE_EXIT_CODE"))
	os.Exit(code)
}
