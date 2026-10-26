#!/usr/bin/env bash
# Quick live gate: the package validates, the smoke script passes, and one idle pig instance
# (pig plus the extension runner, no prompt sent) has a measured footprint.
# Usage: scripts/live/verify.sh   (spends a little claude-haiku-4-5 / claude-sonnet-4-6 quota via int-smoke.sh)
source "$(dirname "$0")/lib.sh"

command -v pig >/dev/null || { echo "pig is not installed" >&2; exit 1; }
require_command jq claude
mkdir -p "$WORK" "$LOGDIR"
init_pig_home
echo "PiG $(pig --version) · claude $(claude --version) · work directory $WORK"

fail() { echo "verify.sh: FAIL: $*" >&2; exit 1; }
trap 'exec 3>&- || true; kill_descendants; exit_check' EXIT

# 1. The package validates and registers the claude-bridge provider.
VALIDATE=$(cd "$WORK" && pig install "$ROOT" --validate-only --json 2>&1) || fail "validate-only exited $?: $VALIDATE"
jq -e '.valid == true and (.providers | index("claude-bridge"))' <<<"$VALIDATE" >/dev/null || fail "validate-only: $VALIDATE"
echo "1. validate-only: valid, provider claude-bridge registered"

# 2. The smoke script.
"$LIVE_DIR/int-smoke.sh" || fail "int-smoke.sh failed"
echo "2. int-smoke.sh: ok"

# 3. Idle footprint of one instance: pig plus the extension runner. stdin is a fifo that stays open until
# the measurement is taken, then closes so pig shuts down by itself.
mkdir -p "$WORK/project"
FIFO="$WORK/idle.fifo"; rm -f "$FIFO"; mkfifo "$FIFO"
export CLAUDE_BRIDGE_DEBUG=1 CLAUDE_BRIDGE_DEBUG_PATH="$LOGDIR/verify-idle-debug.log"
( cd "$WORK/project" && exec pig --no-session -ne -e "$ROOT" --mode rpc < "$FIFO" > "$LOGDIR/verify-idle.log" 2>&1 ) &
PIG_PID=$!
exec 3<>"$FIFO" # read-write, so the open cannot block on a reader that died before opening it
for _ in $(seq 1 120); do # the first run may still be compiling the extension
  [ "$(descendants "$PIG_PID" | wc -l)" -ge 2 ] && break
  sleep 0.5
done
sleep 3
TOTAL_KB=0; PROCS=0; DETAIL=""
for pid in $(descendants "$PIG_PID"); do
  if command -v footprint >/dev/null; then
    kb=$(footprint -p "$pid" --noCategories -f bytes 2>/dev/null | awk '/phys_footprint:/{print int($2/1024); exit}')
  else
    kb=$(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')
  fi
  if [ -n "${kb:-}" ]; then
    TOTAL_KB=$((TOTAL_KB + kb)); PROCS=$((PROCS + 1))
    DETAIL="$DETAIL
   pid $pid $((kb / 1024)) MiB $(ps -o comm= -p "$pid" | xargs basename)"
  fi
done
exec 3>&-
for _ in $(seq 1 60); do
  kill -0 "$PIG_PID" 2>/dev/null || break
  sleep 0.5
done
kill -0 "$PIG_PID" 2>/dev/null && fail "pig did not exit within 30 s of its stdin closing (see $LOGDIR/verify-idle.log)"
wait "$PIG_PID" 2>/dev/null || true
[ "$PROCS" -ge 2 ] || fail "expected pig and its extension runner, saw $PROCS process(es) (see $LOGDIR/verify-idle.log)"
echo "3. idle footprint: $((TOTAL_KB / 1024)) MiB across $PROCS processes (pig + extension runner)$DETAIL"

echo "verify.sh: ok"
