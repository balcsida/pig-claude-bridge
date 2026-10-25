package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

// debugf appends a timestamped line to the debug log when Deps.Debug is set.
// Failures to write are ignored: logging must never fail a turn.
func (b *Bridge) debugf(format string, args ...any) {
	if !b.deps.Debug {
		return
	}
	path := config.DebugLogPath(b.deps.AgentDir)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", b.deps.Now().UTC().Format("2006-01-02T15:04:05.000Z"), fmt.Sprintf(format, args...))
}

// cliDebugOptions returns the file the Claude CLI should write its own debug log
// to and the handler that forwards its stderr lines into the bridge log, or "" and
// nil when debugging is off (the SDK adds no automatic debug file).
func (b *Bridge) cliDebugOptions(tag string) (debugFile string, onStderr func(chunk string)) {
	if !b.deps.Debug {
		return "", nil
	}
	seq := b.nextCLIDebugSeq.Add(1)
	ts := strings.NewReplacer(":", "-", ".", "-").Replace(b.deps.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	dir := config.CLIDebugLogDir(b.deps.AgentDir)
	_ = os.MkdirAll(dir, 0o755)
	debugFile = filepath.Join(dir, ts+"-"+tag+"-"+strconv.FormatInt(seq, 10)+".log")
	b.debugf("cli-debug: %s #%d → %s", tag, seq, debugFile)
	return debugFile, func(chunk string) {
		for _, line := range strings.Split(strings.ReplaceAll(chunk, "\r\n", "\n"), "\n") {
			if line != "" {
				b.debugf("[cli-stderr %s#%d] %s", tag, seq, line)
			}
		}
	}
}

// diagDump writes an unconditional diagnostic entry for "should never happen" paths.
func (b *Bridge) diagDump(label string, data map[string]any) {
	entry := map[string]any{"ts": b.deps.Now().UTC().Format(time.RFC3339Nano), "label": label}
	for k, v := range data {
		entry[k] = v
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	path := config.DiagLogPath(b.deps.AgentDir)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
	b.debugf("DIAG: %s (see %s)", label, path)
}
