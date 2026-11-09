package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/digest"
)

// SyncResult is the outcome of syncSharedSession (index.ts SyncResult).
type SyncResult struct {
	// SessionID is the Claude Code session to resume, "" for a fresh launch.
	SessionID string
	// TargetSessionID is the session the query writes: SessionID when reused, else
	// the one this sync minted (a fork's target, a prewritten import, a clean start).
	TargetSessionID string
	Created         bool
	// ForkAt is the uuid to fork SessionID at into TargetSessionID; "" otherwise.
	ForkAt   string
	Recorded Recording
	// State is the identity token of the mirror this sync read, retained until
	// progress or completion commits this query; nil when the session had none.
	State *SessionState
	// Path is the decision, for the debug log and tests: reuse, fork, prefix-write,
	// rebuild or clean-start.
	Path string
}

// turnStart is the index of the first message of the current user turn: the
// trailing run of user messages not yet written into the Claude Code session.
func turnStart(messages []map[string]any) int {
	i := len(messages)
	for i > 0 && messages[i-1]["role"] == "user" {
		i--
	}
	return i
}

func rawRecords(records []ccsession.Record) []ccsession.RawRecord {
	out := make([]ccsession.RawRecord, len(records))
	for i, r := range records {
		out[i] = r.Raw
	}
	return out
}

// syncSharedSession compares pi's values with the session's recording, then keeps
// Claude Code's own records up to the last valid cut: reuse when equal, fork when
// the cut is the whole prior history, a prefix write when it is shorter, a full
// import otherwise. promptDigest is the provider's system prompt append; a change
// takes a full import, the only path on which CC records a new one. host may be
// nil, in which case verification warnings only reach the logs.
func (b *Bridge) syncSharedSession(host Host, piSessionID string, messages []map[string]any, custom map[string]string, cwd, modelID, promptDigest string) (SyncResult, error) {
	all := make([]any, len(messages))
	for i, m := range messages {
		all[i] = m
	}
	history := convert.NonSystemMessages(all)
	priors := history[:turnStart(history)]
	unlock := b.lockSync(piSessionID)
	defer unlock()
	state := b.sessionStateFor(piSessionID)
	before := state
	priorDigests, err := digest.Messages(priors)
	if err != nil {
		return SyncResult{}, fmt.Errorf("sync: %w", err)
	}
	compared := Comparison{}
	if state != nil {
		compared = CompareHistory(priorDigests, state.Recording)
	}
	missingFile := false
	if state != nil {
		if _, err := os.Stat(ccsession.SessionPath("", state.Cwd, state.CCSessionID)); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return SyncResult{}, fmt.Errorf("sync: %w", err)
			}
			missingFile = true
			b.debugf("sync: missing transcript %s in recorded cwd=%s; importing pi history", state.CCSessionID, state.Cwd)
			b.setSessionStateFor(piSessionID, nil)
			if err := DeleteSessionState(b.deps.AgentDir, piSessionID); err != nil {
				b.debugf("sync: delete persisted state: %v", err)
			}
			state = nil
		}
	}
	cwdChanged := state != nil && safeRealpath(state.Cwd) != safeRealpath(cwd)
	promptChanged := state != nil && state.PromptDigest != promptDigest
	if promptChanged {
		b.debugf("sync: system prompt append changed; importing pi history piSession=%s", piSessionID)
	}
	var row int
	switch {
	case piSessionID == "":
		row = 0
	case state == nil && len(priors) > 0:
		row = 2
	case state == nil:
		row = 1
	case state.Model != modelID || promptChanged:
		row = 3
	case cwdChanged:
		row = 5
	case compared.Equal && !state.DirtyTail:
		row = 4
	case compared.CutCount == 0:
		row = 5
	case compared.CutCount == len(priors):
		row = 6
	default:
		row = 7
	}
	reason := "history-difference"
	switch {
	case piSessionID == "":
		reason = "unattributed-history"
	case missingFile:
		reason = "missing-transcript"
	case state == nil:
		reason = "no-state"
	case state.Model != modelID:
		reason = "model-changed"
	case promptChanged:
		reason = "prompt-changed"
	case cwdChanged:
		reason = "cwd-changed"
	case row == 4:
		reason = "equal-digests"
	case state.DirtyTail && compared.Equal:
		reason = "dirty-tail"
	}
	decision := func(path, after string) {
		from := "none"
		if before != nil {
			from = before.CCSessionID
		}
		b.debugf("syncResult: path=%s sessionId=%s from=%s priors=%d commonPrefix=%d cutCount=%d row=%d reason=%s piSession=%s",
			path, after, from, len(priors), compared.CommonPrefix, compared.CutCount, row, reason, orNone(piSessionID))
	}
	if row == 4 {
		decision("reuse", state.CCSessionID)
		return SyncResult{SessionID: state.CCSessionID, TargetSessionID: state.CCSessionID, Recorded: state.Recording.Copy(), State: state, Path: "reuse"}, nil
	}
	session, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: modelID})
	if err != nil {
		return SyncResult{}, fmt.Errorf("sync: create session: %w", err)
	}
	target := session.ID()
	var recorded Recording
	records := 0
	path := "rebuild"
	if row == 6 || row == 7 {
		transcript, err := ccsession.ReadTranscript(state.CCSessionID, state.Cwd, "")
		if err != nil {
			return SyncResult{}, fmt.Errorf("sync: %w", err)
		}
		cut, err := ResolveRecordingCut(transcript, state.Recording, compared.CutCount)
		if err != nil {
			return SyncResult{}, fmt.Errorf("sync: %w", err)
		}
		recorded = state.Recording.Prefix(compared.CutCount)
		if row == 6 {
			decision("fork", target)
			return SyncResult{SessionID: state.CCSessionID, TargetSessionID: target, Created: true, ForkAt: cut.UUID, Recorded: recorded, State: state, Path: "fork"}, nil
		}
		rest := priors[compared.CutCount:]
		replaced := transcript[cut.Index+1:]
		results, err := RecordedToolResults(replaced, state.Digests, rest)
		if err != nil {
			return SyncResult{}, fmt.Errorf("sync: %w", err)
		}
		suffix, err := ImportHistory(session, rest, custom, convert.CollectCarriedAttachments(rawRecords(replaced)), results)
		if err != nil {
			return SyncResult{}, fmt.Errorf("sync: %w", err)
		}
		recorded = Recording{Digests: append(recorded.Digests, suffix.Digests...), UUIDs: append(recorded.UUIDs, suffix.UUIDs...), Cuttable: append(recorded.Cuttable, suffix.Cuttable...)}
		if records, err = ccsession.SavePrefixSession(session, transcript[:cut.Index+1], cut.UUID); err != nil {
			return SyncResult{}, fmt.Errorf("sync: %w", err)
		}
		path = "prefix-write"
	} else {
		// Nothing is kept, but the old transcript's @file expansions and tool results
		// still belong to messages pi kept unchanged.
		var transcript []ccsession.Record
		var digests []string
		if state != nil {
			if transcript, err = ccsession.ReadTranscript(state.CCSessionID, state.Cwd, ""); err != nil {
				return SyncResult{}, fmt.Errorf("sync: %w", err)
			}
			digests = state.Digests
		}
		results, err := RecordedToolResults(transcript, digests, priors)
		if err != nil {
			return SyncResult{}, fmt.Errorf("sync: %w", err)
		}
		if recorded, err = ImportHistory(session, priors, custom, convert.CollectCarriedAttachments(rawRecords(transcript)), results); err != nil {
			return SyncResult{}, fmt.Errorf("sync: %w", err)
		}
		if err := session.Save(); err != nil {
			return SyncResult{}, fmt.Errorf("sync: save session %s: %w", short(target), err)
		}
		records = len(session.Records())
		if records == 0 {
			path = "clean-start"
		}
	}
	if records > 0 {
		b.verifyWrittenSession(host, session.Path(), target, records, cwd)
	}
	decision(path, target)
	b.debugSessionPaths(short(target), cwd, session.Path())
	resume := ""
	if records > 0 {
		resume = target
	}
	return SyncResult{SessionID: resume, TargetSessionID: target, Created: true, Recorded: recorded, State: state, Path: path}, nil
}

// importForAskClaude writes pi's prior history (everything before the trailing user
// turn) into a fresh Claude Code session for AskClaude's shared mode and returns its
// id, "" when there is nothing to import. The provider's mirror is not touched (D8).
func (b *Bridge) importForAskClaude(host Host, messages []map[string]any, cwd, modelID string) (string, error) {
	all := make([]any, len(messages))
	for i, m := range messages {
		all[i] = m
	}
	history := convert.NonSystemMessages(all)
	priors := history[:turnStart(history)]
	if len(priors) == 0 {
		return "", nil
	}
	session, err := ccsession.Create(ccsession.Options{ProjectPath: cwd, Model: modelID})
	if err != nil {
		return "", fmt.Errorf("askclaude: create session: %w", err)
	}
	if _, err := ImportHistory(session, priors, nil, nil, nil); err != nil {
		return "", fmt.Errorf("askclaude: %w", err)
	}
	if err := session.Save(); err != nil {
		return "", fmt.Errorf("askclaude: save session %s: %w", short(session.ID()), err)
	}
	b.verifyWrittenSession(host, session.Path(), session.ID(), len(session.Records()), cwd)
	return session.ID(), nil
}

// verifyWrittenSession sanity-checks the file just written. It warns instead of
// failing: Claude Code may be more tolerant than our checks.
func (b *Bridge) verifyWrittenSession(host Host, jsonlPath, sessionID string, recordCount int, cwd string) {
	for _, msg := range ccsession.VerifyWrittenSession(jsonlPath, sessionID, recordCount) {
		b.debugf("WARNING session verify: %s", msg)
		cfgDir := envOr("CLAUDE_CONFIG_DIR", "(unset)")
		hint := " (rerun with debug enabled to capture a debug log)"
		if b.deps.Debug {
			hint = " and attach the debug log"
		}
		if host != nil {
			host.Notify(fmt.Sprintf("Session file issue: %s\ncwd=%s realpath=%s CLAUDE_CONFIG_DIR=%s\n"+
				"Please copy and paste this message into a new issue at https://github.com/balcsida/pig-claude-bridge/issues/new%s",
				msg, cwd, safeRealpath(cwd), cfgDir, hint), "warning")
		}
		b.diagDump("session_verify_fail", map[string]any{"msg": msg, "jsonlPath": jsonlPath, "cwd": cwd, "realpath": safeRealpath(cwd), "claudeConfigDir": cfgDir})
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func safeRealpath(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "<failed: " + err.Error() + ">"
	}
	return r
}

// debugSessionPaths logs where a session file was just written, to catch pi and
// Claude Code disagreeing about the project directory (symlinks, CLAUDE_CONFIG_DIR).
func (b *Bridge) debugSessionPaths(label, cwd, jsonlPath string) {
	if !b.deps.Debug {
		return
	}
	realCwd := safeRealpath(cwd)
	size := ""
	exists := false
	if st, err := os.Stat(jsonlPath); err == nil {
		exists, size = true, fmt.Sprintf(" size=%d", st.Size())
	}
	b.debugf("%s: cwd=%s", label, cwd)
	if realCwd != cwd {
		b.debugf("%s: realpath(cwd)=%s (DIFFERS — symlink-resolved path is what CC uses)", label, realCwd)
	}
	b.debugf("%s: jsonlPath=%s", label, jsonlPath)
	b.debugf("%s: fileExists=%t%s", label, exists, size)
	b.debugf("%s: env.CLAUDE_CONFIG_DIR=%s HOME=%s", label, envOr("CLAUDE_CONFIG_DIR", "(unset)"), envOr("HOME", "(unset)"))
}
