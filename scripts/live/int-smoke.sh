#!/usr/bin/env bash
# Smoke tests for the pig-claude-bridge provider (port of pi-claude-bridge tests/int-smoke.sh).
# Requires: pig, Claude Code (login), jq.
# The AskClaude check also needs CLAUDE_BRIDGE_TESTING_ALT_PROVIDER / CLAUDE_BRIDGE_TESTING_ALT_MODEL
# (provider credentials from your environment); without them it prints SKIP and counts as skipped.

source "$(dirname "$0")/lib.sh"

echo "=== smoke-test.sh ==="

require_command pig jq timeout

setup_test_env "smoke-test"

ALT_PROVIDER="${CLAUDE_BRIDGE_TESTING_ALT_PROVIDER:-}"
ALT_MODEL="${CLAUDE_BRIDGE_TESTING_ALT_MODEL:-}"

TIMEOUT=60
PASS=0
FAIL=0
SKIP=0

TEST_CWD_PREFIX="$LOGDIR/smoke-cwd."
TEST_CWD=$(mktemp -d "$TEST_CWD_PREFIX"XXXXXX)
cd "$TEST_CWD"
cleanup() {
  if [[ "${TEST_CWD:-}" == "$TEST_CWD_PREFIX"* && ${#TEST_CWD} -gt ${#TEST_CWD_PREFIX} && -d "$TEST_CWD" ]]; then
    rm -rf -- "$TEST_CWD"
  fi
  disable_ask_claude
  kill_descendants
  exit_check
}
trap cleanup EXIT

run() {
  local name="$1"; shift
  local slug=$(echo "$name" | tr ' :,' '-' | tr -cd '[:alnum:]-')
  local logfile="$LOGDIR/$slug.log"
  printf "%-50s " "$name"
  if output=$(timeout "$TIMEOUT" "$@" 2>&1); then
    echo "$output" > "$logfile"
    if [ -n "$output" ]; then
      echo "PASS"
      ((++PASS))
    else
      echo "FAIL (empty output)"
      echo "  Log: $logfile"
      ((++FAIL))
    fi
  else
    local rc=$?
    echo "${output:-}" > "$logfile" 2>/dev/null || true
    echo "FAIL (exit $rc)"
    echo "  Log: $logfile"
    ((++FAIL))
  fi
  kill_descendants
}

# --- Tests ---

run "provider: print mode responds" \
  pig --no-session -ne -e "$ROOT" \
  --model "claude-bridge/claude-sonnet-4-6" \
  -p "Reply with just the word 'yes'"

# --provider on its own is a no-op for model resolution, so assert the model that
# actually answered, not merely that something did.
run "provider: --provider + --model resolve to the bridge" \
  bash -c "pig --no-session -ne -e '$ROOT' --provider claude-bridge --model claude-haiku-4-5 --mode json \
    -p \"Reply with just the word 'yes'\" 2>/dev/null \
    | jq -r 'select(.message.role==\"assistant\") | .message.provider + \"/\" + .message.model' \
    | sort -u | grep -x 'claude-bridge/claude-haiku-4-5'"

run "provider: model list includes provider" \
  bash -c "pig --no-session -ne -e '$ROOT' --list-models 2>&1 | grep claude-bridge"

# The bridge sends Claude Code's own preset system prompt, so the user's prompt
# customisation has to be forwarded explicitly or it silently does nothing.
run "system prompt: --append-system-prompt reaches Claude" \
  bash -c "pig --no-session -ne -e '$ROOT' --model 'claude-bridge/claude-haiku-4-5' \
    --append-system-prompt 'You must end every response with the exact word BANANA.' \
    -p 'What is 2+2? Answer in one short sentence.' 2>&1 | grep -q BANANA && echo ok"

run "system prompt: --system-prompt reaches Claude" \
  bash -c "pig --no-session -ne -e '$ROOT' --model 'claude-bridge/claude-haiku-4-5' \
    --system-prompt 'You are a pirate. You must end every response with the exact word ARRR.' \
    -p 'What is 2+2? Answer in one short sentence.' 2>&1 | grep -q ARRR && echo ok"

# AskClaude only registers when a non-claude-bridge provider is active, and it is enabled through
# the agent-dir claude-bridge.json (the TypeScript smoke wrote a project .pi/claude-bridge.json).
#
# This covers registration too: `"toolName":"AskClaude"` appears only in tool
# execution events, so a match proves pig dispatched a call to a registered tool.
# The prompt names the tool, but the grep keys off the JSON field rather than the
# bare name, so the user message --mode json echoes back cannot false-positive.
# A separate "is it registered" check by asking the model to enumerate its tools
# was dropped as both weaker and flaky — models summarise and omit custom tools.
MISSING_ALT=""
[ -n "$ALT_PROVIDER" ] || MISSING_ALT="CLAUDE_BRIDGE_TESTING_ALT_PROVIDER"
[ -n "$ALT_MODEL" ] || MISSING_ALT="${MISSING_ALT:+$MISSING_ALT / }CLAUDE_BRIDGE_TESTING_ALT_MODEL"
if [ -n "$MISSING_ALT" ]; then
  printf "%-50s " "tool: AskClaude responds"
  echo "SKIP ($MISSING_ALT unset)"
  ((++SKIP))
else
  enable_ask_claude
  run "tool: AskClaude responds" \
    bash -c "pig --no-session -ne -e '$ROOT' --provider '$ALT_PROVIDER' --model '$ALT_MODEL' --mode json \
      -p 'Use the AskClaude tool with prompt=\"What is 2+2? Reply with just the number.\" and then tell me the answer.' 2>&1 \
      | grep -q '\"toolName\":\"AskClaude\"' && echo ok"
fi

# --- Summary ---

echo ""
echo "Passed: $PASS  Failed: $FAIL  Skipped: $SKIP"
[ "$FAIL" -eq 0 ] || exit 1
