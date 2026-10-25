#!/usr/bin/env bash
# Cross-resume check, forward direction: a conversation recorded by the TypeScript bridge, resumed by this port.
# tests/fixtures/pi-history-310.jsonl is the 310 messages (placeholder text such as "t0:128") that pi held at the
# end of a real claude-bridge session; it is a bare message list, so this script wraps it into a pi session file
# (header + one tree entry per message) in the temp project and resumes that with `pig --session`.
# The bridge has no Claude Code session for the new session id, so its first turn must rebuild one from pi's history.
# Requires: pig, Claude Code, jq.

source "$(dirname "$0")/lib.sh"

echo "=== int-cross-resume.sh ==="

require_command pig jq timeout

setup_test_env "cross-resume" none

TIMEOUT=240
FAIL=0
trap 'kill_descendants; exit_check; [ -z "${CC_PROJECT_DIR:-}" ] || rm -rf "$CC_PROJECT_DIR"' EXIT

check() { # check <description> <command...>
  local desc="$1"; shift
  if "$@" >/dev/null; then echo "PASS: $desc"; else echo "FAIL: $desc"; FAIL=$((FAIL + 1)); fi
}

# Claude Code's directory for this project: every non-alphanumeric character of the resolved path becomes "-".
CC_PROJECT_DIR="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects/$(pwd -P | sed 's/[^A-Za-z0-9]/-/g')"

SESSION="$WORK/cross-resume-session.jsonl"
{
  jq -nc --arg cwd "$(pwd -P)" '{type:"session",version:3,id:"cross-resume-310",timestamp:"2026-01-15T14:00:00Z",cwd:$cwd}'
  jq -c -s '. as $m | range(0; length) | {type:"message", id:("m" + tostring), parentId:(if . == 0 then null else "m" + ((. - 1) | tostring) end),
    timestamp:"2026-01-15T14:00:01Z", message:($m[.] + {timestamp:(1768485601000 + .)})}' "$ROOT/tests/fixtures/pi-history-310.jsonl"
} > "$SESSION"
echo "Session file: $SESSION ($(wc -l < "$SESSION") lines)"

# A rerun in the same work dir must not read the previous run's lines.
: > "$DEBUG_LOG"
OUT="$LOGDIR/cross-resume.ndjson"
echo "Resuming with pig --session..."
timeout "$TIMEOUT" pig -ne -e "$ROOT" --model "claude-bridge/claude-haiku-4-5" --session "$SESSION" --mode json \
  -p "What was the exact text of my very first message in this conversation? Reply with just that text." \
  > "$OUT" 2> "$OUT.err" || { echo "FAIL: pig exited with $?"; head -c 2000 "$OUT.err"; exit 1; }

ANSWER=$(jq -rs '[.[] | select(.type == "message_update") | .assistantMessageEvent | select(.type == "text_end") | .content] | join(" ")' "$OUT")
echo "Answer: $ANSWER"
check "the answer quotes the first message of the recorded history (t0:128)" grep -qF 't0:128' <<<"$ANSWER"

FIRST_SYNC=$(grep -o 'syncResult: path=[a-z-]*' "$DEBUG_LOG" | head -1 || true)
echo "First sync: ${FIRST_SYNC:-none}"
check "the first turn rebuilt a Claude Code session (syncResult: path=rebuild)" test "$FIRST_SYNC" = "syncResult: path=rebuild"

SESSION_ID=$(grep -o 'query done, session=[0-9a-f]*' "$DEBUG_LOG" | tail -1 | cut -d= -f2 || true)
SESSION_FILE=$(compgen -G "$CC_PROJECT_DIR/${SESSION_ID:-none}*.jsonl" | head -1 || true)
echo "Claude Code session: ${SESSION_FILE:-not found}"
check "the rebuilt Claude Code session file exists under the project hash" test -n "$SESSION_FILE"
if [ -n "$SESSION_FILE" ]; then
  RECORDS=$(grep -c . "$SESSION_FILE")
  echo "Records: $RECORDS, first type: $(head -1 "$SESSION_FILE" | jq -r .type)"
  check "the first record is a user record" test "$(head -1 "$SESSION_FILE" | jq -r .type)" = "user"
  check "the session holds at least the 310 recorded messages plus the new turn (>= 311 records)" test "$RECORDS" -ge 311
  # Claude Code appends records of its own (queue-operation, attachments) after the rebuilt ones, so the check is
  # that the first record is a root and every other parentUuid names an earlier record.
  check "the records chain in order (first record is a root, every parentUuid names an earlier record)" \
    jq -es '. as $a | $a[0].parentUuid == null and all(range(1; length); . as $i | $a[$i].parentUuid == null or ($a[:$i] | map(.uuid) | index($a[$i].parentUuid)) != null)' "$SESSION_FILE"
fi

echo ""
echo "Failed: $FAIL"
[ "$FAIL" -eq 0 ] || exit 1
