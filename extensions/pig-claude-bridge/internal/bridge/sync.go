package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccsession"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// Sync outcomes. CLEAN_START means no Claude Code session exists to resume.
const (
	ActionReuse           = "REUSE"
	ActionAppend          = "APPEND"
	ActionRebuild         = "REBUILD"
	ActionPostAbortRotate = "POST_ABORT_ROTATE"
	ActionCleanStart      = "CLEAN_START"
)

// SyncResult is the outcome of syncSharedSession. SessionID is empty when no
// session should be resumed.
type SyncResult struct {
	Action    string
	SessionID string
	// PreserveSharedSession tells the completion handler not to replace the mirror
	// with the ephemeral session a clean start creates.
	PreserveSharedSession bool
	Cursor                int
	Priors                int
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

// syncSharedSession makes the shared session hold every message before the current
// user turn. It returns the session id to resume, or "" when no resume is needed.
// host may be nil, in which case verification warnings only reach the logs.
func (b *Bridge) syncSharedSession(host Host, piSessionID string, messages []map[string]any, custom map[string]string, cwd, modelID string) (SyncResult, error) {
	all := make([]any, len(messages))
	for i, m := range messages {
		all[i] = m
	}
	history := convert.NonSystemMessages(all)
	priors := history[:turnStart(history)]
	// One sync per key at a time: the file I/O below must not interleave, and the
	// final store must know which marks landed since this snapshot.
	unlock := b.lockSync(piSessionID)
	defer unlock()
	shared := b.sessionStateFor(piSessionID)

	if shared != nil && !shared.NeedsRebuild && len(priors) >= shared.Cursor {
		missed := priors[shared.Cursor:]
		trailingAssistantOnly := len(missed) == 1 && missed[0]["role"] == "assistant"
		if len(missed) == 0 || trailingAssistantOnly {
			res := SyncResult{Action: ActionReuse, SessionID: shared.SessionID, Cursor: shared.Cursor, Priors: len(priors)}
			if trailingAssistantOnly {
				b.advanceCursor(piSessionID, len(priors), cwd)
				res.Action, res.Cursor = ActionAppend, len(priors)
				b.debugf("Case 3: advanced cursor past trailing assistant, resuming session %s, cursor=%d", short(shared.SessionID), len(priors))
			} else {
				b.debugf("Case 3: resuming session %s, cursor=%d", short(shared.SessionID), shared.Cursor)
			}
			b.debugf("syncResult: path=reuse sessionId=%s cursor=%d", shared.SessionID, res.Cursor)
			return res, nil
		}
	}
	// A shorter context cannot continue the cached session: start clean and keep the mirror.
	if shared != nil && !shared.NeedsRebuild && len(priors) < shared.Cursor {
		b.debugf("Case 1 synthetic: clean start for shorter context, preserving shared session %s, cursor=%d", short(shared.SessionID), shared.Cursor)
		b.debugf("syncResult: path=clean-start preserve-shared sessionId=%s cursor=%d", shared.SessionID, shared.Cursor)
		return SyncResult{Action: ActionCleanStart, PreserveSharedSession: true, Cursor: shared.Cursor, Priors: len(priors)}, nil
	}
	if len(priors) == 0 {
		b.debugf("Case 1: clean start, %d total messages", len(history))
		b.debugf("syncResult: path=clean-start")
		return SyncResult{Action: ActionCleanStart}, nil
	}

	var previousID string
	previousCursor := 0
	if shared != nil {
		previousID, previousCursor = shared.SessionID, shared.Cursor
	}
	preserveID := previousID != "" && !shared.ForceRotate
	var carried []convert.CarriedAttachment
	if previousID != "" {
		// Before Delete: it wipes the file the attachments live in.
		carried = b.readCarriedAttachments(previousID, cwd)
	}
	if preserveID {
		if err := ccsession.Delete(previousID, cwd, ""); err != nil {
			return SyncResult{}, fmt.Errorf("sync: delete previous session %s: %w", short(previousID), err)
		}
	}
	opts := ccsession.Options{ProjectPath: cwd, Model: modelID}
	if preserveID {
		opts.SessionID = previousID
	}
	session, err := ccsession.Create(opts)
	if err != nil {
		return SyncResult{}, fmt.Errorf("sync: create session: %w", err)
	}
	if err := b.convertAndImportMessages(session, priors, custom, carried); err != nil {
		return SyncResult{}, err
	}
	if err := session.Save(); err != nil {
		return SyncResult{}, fmt.Errorf("sync: save session %s: %w", short(session.ID()), err)
	}
	// Records, not messages: carried attachments are records too.
	b.verifyWrittenSession(host, session.Path(), session.ID(), len(session.Records()), cwd)
	b.storeRebuilt(piSessionID, shared, SessionState{SessionID: session.ID(), Cursor: len(priors), Cwd: cwd, PiSessionID: piSessionID})

	id, n := short(session.ID()), len(session.Records())
	res := SyncResult{Action: ActionRebuild, SessionID: session.ID(), Cursor: len(priors), Priors: len(priors)}
	flavor := "preserved"
	switch {
	case previousID == "":
		flavor = "first"
		b.debugf("Case 2: first turn with %d prior messages → session %s, %d records", len(priors), id, n)
	case preserveID:
		b.debugf("Case 4: %d missed messages, %d total → rewrote session %s (same id), %d records", len(priors)-previousCursor, len(priors), id, n)
	default:
		flavor = "rotated-post-abort"
		res.Action = ActionPostAbortRotate
		b.debugf("Case 4 post-abort: %d total → new session %s (was %s, rotated to avoid race with orphan writer), %d records", len(priors), id, short(previousID), n)
	}
	b.debugSessionPaths(id, cwd, session.Path())
	b.debugf("syncResult: path=rebuild sessionId=%s priors=%d %s", session.ID(), len(priors), flavor)
	return res, nil
}

// readCarriedAttachments reads Claude Code's @file expansions from the session
// about to be replaced. A killed subprocess may have left a truncated last line;
// carrying none is the pre-existing behaviour, so a failure is logged, not raised.
func (b *Bridge) readCarriedAttachments(sessionID, cwd string) []convert.CarriedAttachment {
	previous, err := ccsession.Open(sessionID, cwd, "")
	if err != nil {
		b.debugf("WARNING: could not read attachments from session %s: %v", short(sessionID), err)
		return nil
	}
	return convert.CollectCarriedAttachments(previous.Records())
}

// convertAndImportMessages converts pi messages and writes them into session.
func (b *Bridge) convertAndImportMessages(session *ccsession.Session, messages []map[string]any, custom map[string]string, carried []convert.CarriedAttachment) error {
	anthropic, sanitized, dropped, err := convert.ConvertPiMessages(messages, custom)
	if err != nil {
		return fmt.Errorf("convert pi messages: %w", err)
	}
	b.debugf("convertAndImportMessages: %d pi msgs → %d anthropic msgs", len(messages), len(anthropic))
	var parts []string
	if dropped.Thinking > 0 {
		providers := append([]string(nil), dropped.Providers...)
		sort.Strings(providers)
		parts = append(parts, fmt.Sprintf("%d thinking (%s)", dropped.Thinking, strings.Join(providers, ", ")))
	}
	if dropped.AbortedTurns > 0 {
		parts = append(parts, fmt.Sprintf("%d aborted turn(s)", dropped.AbortedTurns))
	}
	types := make([]string, 0, len(dropped.Other))
	for t := range dropped.Other {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		parts = append(parts, fmt.Sprintf("%d %s", dropped.Other[t], t))
	}
	if len(parts) > 0 {
		b.debugf("convertAndImportMessages: dropped %s", strings.Join(parts, ", "))
	}
	if len(sanitized) > 0 {
		b.debugf("convertAndImportMessages: sanitized %d tool IDs", len(sanitized))
	}
	repaired := ccsession.RepairToolPairing(anthropic)
	if len(repaired) != len(anthropic) {
		b.debugf("convertAndImportMessages: repairToolPairing %d → %d msgs", len(anthropic), len(repaired))
	}
	var attachments []ccsession.ImportAttachment
	if len(carried) > 0 {
		var skipped []string
		attachments, skipped = convert.PlaceCarriedAttachments(carried, repaired)
		if len(skipped) > 0 {
			b.debugf("convertAndImportMessages: dropped %d carried attachment(s): %s", len(skipped), strings.Join(skipped, "; "))
		}
		if len(attachments) > 0 {
			b.debugf("convertAndImportMessages: carrying %d attachment(s) across the rebuild", len(attachments))
		}
	}
	if len(repaired) == 0 {
		return nil
	}
	if err := session.ImportMessages(repaired, attachments); err != nil {
		return fmt.Errorf("import messages: %w", err)
	}
	return nil
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
