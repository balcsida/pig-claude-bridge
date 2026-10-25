#!/usr/bin/env bash
# Runs the whole live suite into one work directory and prints a results table:
# verify.sh, every int-*.sh, the Go RPC tests (tests/live), then the deadlock check over the log dir.
# Spends real Claude quota. Exit status is non-zero when anything failed.
source "$(dirname "$0")/lib.sh"
set +e

ROWS=""
FAILED=0
row() { # name status seconds
  ROWS="$ROWS$(printf '%-34s | %-16s | %s' "$1" "$2" "$3")
"
  [[ "$2" == FAIL* ]] && FAILED=1
}

# One row for the script, then one SKIP row per "<check>   SKIP (...)" line it printed.
script_rows() { # name status seconds output-file
  local line
  row "$1" "$2" "$3"
  while IFS= read -r line; do
    line=${line%% SKIP (*}
    row "$1: ${line%"${line##*[![:space:]]}"}" SKIP 0
  done < <(grep -E ' SKIP \(' "$4")
}

# One row per "--- PASS: TestX (22.00s)" line of go test -v output.
go_rows() { # output-file
  local verdict name secs
  while read -r verdict name secs; do
    row "go: $name" "${verdict%:}" "${secs//[()s]/}"
  done < <(grep -E '^--- (PASS|FAIL|SKIP): ' "$1" | sed -E 's/^--- //')
}

if [ "${1:-}" = "--selftest" ]; then
  tmp=$(mktemp -d)
  printf 'a: check   PASS\nb: skipped check                 SKIP (X unset)\n' > "$tmp/skip.txt"
  printf -- '--- PASS: TestX (22.00s)\n' > "$tmp/go.txt"
  : > "$tmp/empty.txt"
  script_rows ok PASS 5 "$tmp/empty.txt"; script_rows smoke PASS 7 "$tmp/skip.txt"; script_rows bad FAIL 3 "$tmp/empty.txt"
  go_rows "$tmp/go.txt"
  want=$(printf '%-34s | %-16s | %s\n' ok PASS 5 smoke PASS 7 "smoke: b: skipped check" SKIP 0 bad FAIL 3 "go: TestX" PASS 22.00)
  got=${ROWS%$'\n'}
  rm -rf "$tmp"
  [ "$got" = "$want" ] && [ "$FAILED" = 1 ] || { echo "run-all.sh --selftest: FAIL" >&2; printf 'got:\n%s\nwant:\n%s\n' "$got" "$want" >&2; exit 1; }
  echo "run-all.sh --selftest: ok"
  exit 0
fi

mkdir -p "$WORK" "$LOGDIR"
check_deadlock_logs --selftest || exit 1
echo "work directory: $WORK"

run_script() { # script
  local name start status
  name=$(basename "$1" .sh); start=$SECONDS
  "$1" 2>&1 | env -u CLAUDE_BRIDGE_TEST_WORK_DIR tee "$LOGDIR/$name.run.txt"
  if [ "${PIPESTATUS[0]}" -eq 0 ]; then status=PASS; else status=FAIL; fi
  script_rows "$name" "$status" "$((SECONDS - start))" "$LOGDIR/$name.run.txt"
}

run_script "$LIVE_DIR/verify.sh"
for script in "$LIVE_DIR"/int-*.sh; do run_script "$script"; done

(cd "$ROOT/tests/live" && go test -race -tags live -count=1 -v ./...) 2>&1 | env -u CLAUDE_BRIDGE_TEST_WORK_DIR tee "$LOGDIR/go-test.run.txt"
[ "${PIPESTATUS[0]}" -eq 0 ] || FAILED=1
go_rows "$LOGDIR/go-test.run.txt"

start=$SECONDS
if check_deadlock_logs "$LOGDIR"; then row "deadlock check" PASS "$((SECONDS - start))"; else row "deadlock check" FAIL "$((SECONDS - start))"; fi

echo
echo "name                               | result           | seconds"
printf '%s' "$ROWS"
exit "$FAILED"
