#!/usr/bin/env bash
# Runs the whole live suite into one work directory and prints a results table:
# verify.sh, every int-*.sh, the Go RPC tests (tests/live), then the deadlock check over the log dir.
# Spends real Claude quota. Exit status is non-zero when anything failed.
source "$(dirname "$0")/lib.sh"
set +e

mkdir -p "$WORK" "$LOGDIR"
check_deadlock_logs --selftest || exit 1
echo "work directory: $WORK"

ROWS=""
FAILED=0
row() { # name status seconds
  ROWS="$ROWS$(printf '%-34s | %-16s | %s' "$1" "$2" "$3")
"
  [[ "$2" == FAIL* ]] && FAILED=1
}

run_script() { # script
  local name start status skipped
  name=$(basename "$1" .sh); start=$SECONDS
  "$1" 2>&1 | env -u CLAUDE_BRIDGE_TEST_WORK_DIR tee "$LOGDIR/$name.run.txt"
  if [ "${PIPESTATUS[0]}" -eq 0 ]; then status=PASS; else status=FAIL; fi
  skipped=$(sed -nE 's/.*Skipped: ([0-9]+).*/\1/p' "$LOGDIR/$name.run.txt" | tail -1)
  [ "${skipped:-0}" -eq 0 ] || status="$status ($skipped skipped)"
  row "$name" "$status" "$((SECONDS - start))"
}

run_script "$LIVE_DIR/verify.sh"
for script in "$LIVE_DIR"/int-*.sh; do run_script "$script"; done

(cd "$ROOT/tests/live" && go test -race -tags live -count=1 -v ./...) 2>&1 | env -u CLAUDE_BRIDGE_TEST_WORK_DIR tee "$LOGDIR/go-test.run.txt"
[ "${PIPESTATUS[0]}" -eq 0 ] || FAILED=1
while read -r verdict name secs; do
  row "go: $name" "${verdict%:}" "${secs//[()s]/}"
done < <(grep -E '^--- (PASS|FAIL|SKIP): ' "$LOGDIR/go-test.run.txt" | sed -E 's/^--- //')

start=$SECONDS
if check_deadlock_logs "$LOGDIR"; then row "deadlock check" PASS "$((SECONDS - start))"; else row "deadlock check" FAIL "$((SECONDS - start))"; fi

echo
echo "name                               | result           | seconds"
printf '%s' "$ROWS"
exit "$FAILED"
