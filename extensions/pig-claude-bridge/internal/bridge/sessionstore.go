package bridge

// Port of pi-claude-bridge src/session-store.ts: the persisted pointer from a pi
// session to the Claude Code session its turns run on, so a restart or /reload
// forks that session at its last valid cut instead of importing pi's history.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf16"
)

const stateFormatVersion = 1

// SessionState mirrors the Claude Code conversation one pi session's turns run on.
type SessionState struct {
	CCSessionID string
	Cwd         string
	Model       string
	// PromptDigest is the digest of the system prompt append CC recorded for this
	// session, which it replays on every resume until a full import.
	PromptDigest string
	Recording
	// DirtyTail: the recording may not cover the tail of the file (an abort, a
	// discard, a loaded pointer), so the next sync compares content rather than reusing.
	DirtyTail bool
}

func sessionsDir(agentDir string) string { return filepath.Join(agentDir, "claude-bridge", "sessions") }

// stateFileName hashes the pi session id as the TypeScript does (sha256 over UTF-16LE).
func stateFileName(piSessionID string) string {
	units := utf16.Encode([]rune(piSessionID))
	buf := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[2*i:], u)
	}
	sum := sha256.Sum256(buf)
	return "s-" + hex.EncodeToString(sum[:]) + ".json"
}

func statePath(agentDir, piSessionID string) string {
	return filepath.Join(sessionsDir(agentDir), stateFileName(piSessionID))
}

type storedState struct {
	Version      int       `json:"version"`
	CCSessionID  string    `json:"ccSessionId"`
	Cwd          string    `json:"cwd"`
	Model        string    `json:"model"`
	PromptDigest string    `json:"promptDigest"`
	Digests      []string  `json:"digests"`
	UUIDs        []*string `json:"uuids"`
	Cuttable     []bool    `json:"cuttable"`
}

// LoadSessionState reads the persisted state for piSessionID: nil with no error when
// there is none, nil with a reason when the file is rejected (invalid, another
// version), the state marked dirty otherwise. An I/O error is returned as is.
func LoadSessionState(agentDir, piSessionID string) (state *SessionState, rejected string, err error) {
	data, err := os.ReadFile(statePath(agentDir, piSessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return nil, "invalid JSON", nil
		}
		return nil, "expected an object", nil
	}
	if raw == nil {
		return nil, "expected an object", nil
	}
	if _, ok := raw["version"]; !ok {
		return nil, "missing version", nil
	}
	var version int
	if json.Unmarshal(raw["version"], &version) != nil || version != stateFormatVersion {
		return nil, fmt.Sprintf("unsupported version; expected %d", stateFormatVersion), nil
	}
	for _, key := range []string{"ccSessionId", "cwd", "model", "promptDigest", "digests", "uuids", "cuttable"} {
		if _, ok := raw[key]; !ok {
			return nil, "missing " + key, nil
		}
	}
	var stored storedState
	for _, key := range []string{"ccSessionId", "cwd", "model", "promptDigest"} {
		var s string
		if json.Unmarshal(raw[key], &s) != nil {
			return nil, key + " must be a string", nil
		}
		if s == "" {
			return nil, key + " must not be empty", nil
		}
	}
	if err := json.Unmarshal(data, &stored); err != nil || stored.Digests == nil || stored.UUIDs == nil || stored.Cuttable == nil {
		return nil, "digests, uuids, and cuttable must be arrays", nil
	}
	rec := Recording{Digests: stored.Digests, UUIDs: make([]string, len(stored.UUIDs)), Cuttable: stored.Cuttable}
	for i, u := range stored.UUIDs {
		if u != nil {
			if *u == "" {
				return nil, fmt.Errorf("%w: slot %d", ErrInvalidRecording, i).Error(), nil
			}
			rec.UUIDs[i] = *u
		}
	}
	if err := rec.Validate(); err != nil {
		return nil, err.Error(), nil
	}
	return &SessionState{CCSessionID: stored.CCSessionID, Cwd: stored.Cwd, Model: stored.Model, PromptDigest: stored.PromptDigest, Recording: rec, DirtyTail: true}, "", nil
}

// SaveSessionState writes state atomically (a fresh temp file renamed over the
// target), readable by the user only.
func SaveSessionState(agentDir, piSessionID string, state SessionState) error {
	for key, v := range map[string]string{"ccSessionId": state.CCSessionID, "cwd": state.Cwd, "model": state.Model, "promptDigest": state.PromptDigest} {
		if v == "" {
			return fmt.Errorf("%s must be a non-empty string", key)
		}
	}
	if err := state.Validate(); err != nil {
		return err
	}
	dir := sessionsDir(agentDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	stored := storedState{Version: stateFormatVersion, CCSessionID: state.CCSessionID, Cwd: state.Cwd, Model: state.Model,
		PromptDigest: state.PromptDigest, Digests: state.Digests, UUIDs: make([]*string, len(state.UUIDs)), Cuttable: state.Cuttable}
	for i := range state.UUIDs {
		if state.UUIDs[i] != "" {
			u := state.UUIDs[i]
			stored.UUIDs[i] = &u
		}
	}
	contents, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+newUUID()+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(contents); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, statePath(agentDir, piSessionID))
}

// DeleteSessionState removes the persisted state; a missing file is fine.
func DeleteSessionState(agentDir, piSessionID string) error {
	err := os.Remove(statePath(agentDir, piSessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
