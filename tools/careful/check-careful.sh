#!/usr/bin/env bash
# check-careful.sh — PreToolUse(Bash) guardrail for Nexus. Thin shim.
#
# The matcher lives in check-careful.py next to this file. This shim exists only so
# .claude/settings.json can keep invoking `bash .../check-careful.sh`, and so that a
# missing/broken python3 degrades to ASK rather than to silent allow.
#
# Ported from gstack /careful (MIT, github.com/garrytan/gstack); telemetry dropped,
# Nexus Go/MySQL/migrate/docker patterns added.
#
# Test: bash .claude/hooks/check-careful.test.sh
# (Staged in tools/careful/ until a human moves it to .claude/hooks/: the running guard refuses
# agent writes under .claude/hooks/, so the move and the settings.json registration are manual.)

INPUT=$(cat 2>/dev/null || true)

# Nothing on stdin: nothing to inspect, allow.
if [ -z "$INPUT" ]; then echo '{}'; exit 0; fi

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OUT=$(printf '%s' "$INPUT" | python3 "$DIR/check-careful.py" 2>/dev/null)
RC=$?

if [ $RC -eq 0 ] && [ -n "$OUT" ]; then
  printf '%s\n' "$OUT"
  exit 0
fi

# The matcher could not run (no python3, syntax error, crash). ASK — do NOT allow.
# A guard that silently disappears when its interpreter is missing is the failure mode
# this whole file was rewritten to end.
printf '%s\n' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"[careful] the guardrail could not run (check-careful.py failed or python3 is missing) — asking instead of allowing."}}'
exit 0
