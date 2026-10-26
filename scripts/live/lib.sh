#!/usr/bin/env bash
# Shared setup for the live integration scripts (port of pi-claude-bridge tests/lib/bash-setup.sh
# and tests/lib/check-deadlock-logs.mjs). Source it at the start of a script. It never reads a
# .env file: the alternative provider and its credentials come from the caller's environment.

set -euo pipefail

LIVE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$LIVE_DIR/../.." && pwd)"

# One work directory per run; run-all.sh and the Go harness share it through the environment.
WORK="${CLAUDE_BRIDGE_TEST_WORK_DIR:-$ROOT/scripts/live/work/$(date +%Y%m%d-%H%M%S)}"
LOGDIR="${CLAUDE_BRIDGE_TEST_LOG_DIR:-$WORK/logs}"
export ROOT WORK LOGDIR CLAUDE_BRIDGE_TEST_WORK_DIR="$WORK" CLAUDE_BRIDGE_TEST_LOG_DIR="$LOGDIR"

# Fresh PiG state for the run. HOME stays real: Claude Code's login lives in ~/.claude and the keychain.
export PIG_HOME="$WORK/home/pig"
export PIG_CODING_AGENT_DIR="$WORK/home/pig/agent"
export NO_COLOR=1 TERM=dumb
unset PI_CODING_AGENT_DIR PIG_USE_PI_DIRS

SURVIVORS=0

# Claude Code persists session state under its config dir. A sandbox that blocks those writes lets the
# first query succeed and then fails the next turn's --resume, which reads like a bridge bug.
probe_claude_dir() {
	local dir="${CLAUDE_CONFIG_DIR:-$HOME/.claude}" probe err
	probe="$dir/.int-test-write-probe-$$"
	if ! err=$({ mkdir -p "$dir" && : > "$probe" && rm -f "$probe"; } 2>&1); then
		echo "Integration tests need write access to $dir for Claude Code session state (got ${err##*: }). Re-run outside the sandbox, or point CLAUDE_CONFIG_DIR at a writable directory." >&2
		exit 1
	fi
}

# Create the run's PiG home once: settings select the bridge's cheapest model.
init_pig_home() {
	mkdir -p "$PIG_CODING_AGENT_DIR" "$LOGDIR"
	[ -f "$PIG_CODING_AGENT_DIR/settings.json" ] ||
		printf '{"defaultProvider":"claude-bridge","defaultModel":"claude-haiku-4-5"}\n' > "$PIG_CODING_AGENT_DIR/settings.json"
}

# Enable AskClaude through the agent-dir config (PiG's project config dir name is host-decided).
enable_ask_claude() {
	printf '{"askClaude":{"enabled":true}}\n' > "$PIG_CODING_AGENT_DIR/claude-bridge.json"
}
disable_ask_claude() {
	rm -f "$PIG_CODING_AGENT_DIR/claude-bridge.json"
}

# A throwaway project with the files the multi-turn prompts read. The TypeScript suite read this
# repo's own package.json, README.md, tsconfig.json and LICENSE; this repo has no such files.
# Usage: make_fixture_project DIR
make_fixture_project() {
	mkdir -p "$1"
	printf '{\n  "name": "pi-claude-bridge",\n  "version": "4.7.2"\n}\n' > "$1/package.json"
	printf '# pi-claude-bridge\n\nA fixture project for the live integration scripts.\n' > "$1/README.md"
	printf '{\n  "compilerOptions": {\n    "target": "ES2022",\n    "module": "ESNext"\n  }\n}\n' > "$1/tsconfig.json"
	cp "$ROOT/LICENSE" "$1/LICENSE"
}

# Setup standard test environment.
# Usage: setup_test_env "test-name" [log-suffix|none]
# Sets: LOGDIR, LOGFILE, DEBUG_LOG, PROJECT; exports CLAUDE_BRIDGE_DEBUG(_PATH); cd's into PROJECT.
setup_test_env() {
	local name="$1"
	local log_suffix="${2:-.log}"

	probe_claude_dir
	init_pig_home

	export CLAUDE_BRIDGE_DEBUG=1
	DEBUG_LOG="$LOGDIR/${name}-debug.log"
	export CLAUDE_BRIDGE_DEBUG_PATH="$DEBUG_LOG"

	if [[ "$log_suffix" != "none" ]]; then
		LOGFILE="$LOGDIR/${name}${log_suffix}"
	else
		LOGFILE=""
	fi

	PROJECT="$WORK/project"
	make_fixture_project "$PROJECT"
	cd "$PROJECT"

	export LOGFILE DEBUG_LOG PROJECT
}

# Pids of this process's descendants (and itself).
descendants() {
	ps -A -o pid=,ppid= | awk -v root="$1" '{pp[$1]=$2} END {s[root]=1; c=1; while (c) {c=0; for (p in pp) if (!(p in s) && (pp[p] in s)) {s[p]=1; c=1}} for (p in s) print p}'
}

# Live processes outside this shell's ancestry that carry this run's work dir in their environment.
# A pig that was killed leaves its runner and Claude Code re-parented to init, so the process tree
# cannot find them; the exported CLAUDE_BRIDGE_TEST_WORK_DIR marks everything that descended from a run.
leaked_processes() {
	local skip=" $$ " p=$$ listing pid
	while [ "$p" -gt 1 ]; do
		p=$(ps -o ppid= -p "$p" | tr -d ' ')
		[ -n "$p" ] || break
		skip="$skip$p "
	done
	listing=$(ps -axeww -o pid=,command= | grep -F "CLAUDE_BRIDGE_TEST_WORK_DIR=$WORK" || true)
	for pid in $(awk '{print $1}' <<<"$listing"); do
		case "$skip" in *" $pid "*) continue ;; esac
		kill -0 "$pid" 2>/dev/null && echo "$pid"
	done
}

# Kill all descendant processes (children, grandchildren, etc.), then fail the script on any survivor.
# Use as: trap 'kill_descendants; exit_check' EXIT
kill_descendants() {
	pkill -P $$ 2>/dev/null || true
	sleep 1
	local pid
	for pid in $(leaked_processes); do
		echo "SURVIVOR: pid $pid $(ps -o command= -p "$pid" 2>/dev/null | cut -c1-120)" >&2
		kill -9 "$pid" 2>/dev/null || true
		SURVIVORS=$((SURVIVORS + 1))
	done
}
exit_check() {
	[ "$SURVIVORS" -eq 0 ] || { echo "FAIL: $SURVIVORS process(es) outlived pig" >&2; exit 1; }
}

# Require an environment variable or exit with error.
# Usage: require_env VARNAME
require_env() {
	local var="$1"
	local val="${!var:-}"
	if [[ -z "$val" ]]; then
		echo "ERROR: $var not set"
		exit 1
	fi
	echo "$val"
}

# Check for required commands or exit with error.
# Usage: require_command cmd1 cmd2 ...
require_command() {
	local cmd
	for cmd in "$@"; do
		if ! command -v "$cmd" >/dev/null 2>&1; then
			echo "ERROR: $cmd is required but not installed"
			exit 1
		fi
	done
}

# The Go bridge's debug line is "[<UTC timestamp>Z] <message>" with no [id] segment.
DEADLOCK_RE='^\[[0-9]{4}-[0-9]{2}-[0-9]{2}T[^]]+\] (BUG:|WARNING: [0-9]+ MCP handlers still waiting)'

# Fail a completed run on bridge-origin deadlock lines in <dir>/*-debug.log.
# Usage: check_deadlock_logs <dir> | --selftest
check_deadlock_logs() {
	if [ "${1:-}" = "--selftest" ]; then
		local f hits
		f=$(mktemp)
		printf '%s\n' \
			'[2026-10-25T12:00:00.000Z] WARNING: 2 MCP handlers still waiting after delivering 1 results' \
			'[2026-10-25T12:00:01.000Z] provider: streamSimple called, activeQuery=false, lastMsgRole=user' \
			'[2026-10-25T12:00:02.000Z] BUG: both maps non-empty! handlers=1 results=1' \
			'[2026-10-25T12:00:03.000Z] provider: reply quoted "BUG: both maps non-empty"' > "$f"
		hits=$(grep -nE "$DEADLOCK_RE" "$f" | cut -d: -f1 | tr '\n' ' ')
		rm -f "$f"
		[ "$hits" = "1 3 " ] || { echo "check_deadlock_logs --selftest: FAIL (matched lines: '${hits}', want '1 3 ')" >&2; return 1; }
		echo "check_deadlock_logs --selftest: ok"
		return 0
	fi
	local dir="${1:-}" file hits findings=""
	[ -n "$dir" ] || { echo "Usage: check_deadlock_logs <integration-log-dir>" >&2; return 2; }
	local files=("$dir"/*-debug.log)
	[ -e "${files[0]}" ] || { echo "No integration debug logs in $dir" >&2; return 1; }
	for file in "${files[@]}"; do
		hits=$(grep -nE "$DEADLOCK_RE" "$file" | sed "s|^|$(basename "$file"):|" || true)
		[ -z "$hits" ] || findings+="$hits"$'\n'
	done
	if [ -n "$findings" ]; then
		echo "Bridge deadlock warnings:" >&2
		echo "$findings" >&2
		return 1
	fi
	echo "No bridge deadlock warnings in $dir"
}
