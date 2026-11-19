#!/usr/bin/env bash
# Gate for the schema upgrade tests (docs/phase-d2-plan.md AC11, section 5.5).
#
# A green `go test` can hide tests that did not run: a skipped test reads as passed.
# This script runs the upgrade tests with -json and fails unless
#   * go test exits 0,
#   * at least MIN_UPGRADE_TESTS top-level tests passed,
#   * no test, at any level, was skipped,
# then compares the recorded schema fixtures with git history (TestGitHistoryFixtures).
#
# Usage:
#   ARTEX_PG_DSN=postgres://USER:PASS@HOST:5432/DB?sslmode=disable scripts/upgrade-gate.sh
#
# ARTEX_PG_DSN must name a PostgreSQL 16 test server: the tests create and drop
# temporary databases named <DB>_<package>_upg_<case> on it and nowhere else, and
# create and drop canary roles named <DB>_<package>_upgrole_<8 hex>.
# One run per base database at a time: the temporary database names are stable (the
# leftover check finds them by prefix) and a test drops a database of its name before it
# creates it, so two runs on one server and one <DB> destroy each other's databases.
# Give a second run its own <DB>.
# UPGRADE_GATE_REQUIRE_GIT=1 turns a missing git history into a failure (hosted CI
# sets it, with a full-depth checkout); without it the comparison is reported as
# NOT RUN where there is no .git.
#
# The summary prints counts and test names only, no row content, no DSN. The output of a
# failed run is echoed only when it holds no canary marker (see echo_failing_output).
set -euo pipefail
cd "$(dirname "$0")/.."

# Count of the top-level tests selected by PATTERN after the D.2 fix round, from a run with
# -v: 83 (./db 29, ./db/dbtest 49, ./agent 1, ./server 4). Raise it with the tests, never
# lower it to make the gate pass.
MIN_UPGRADE_TESTS=83
PATTERN='^(TestUpgrade|TestConfigFileDSNIsNotUsed)'
PACKAGES=(./db ./db/dbtest ./agent ./server)

if [ -z "${ARTEX_PG_DSN:-}" ]; then
  echo "upgrade-gate: ARTEX_PG_DSN is not set; the tests would fail (they never read the config file)" >&2
  exit 2
fi

out="$(mktemp "${TMPDIR:-/tmp}/upgrade-gate.XXXXXX")"
trap 'rm -f "$out"' EXIT

# test2json events are one JSON object per line: Time, Action, Package, Test, ...
count() { grep -cE "$1" "$out" || true; }

# The fixed canary strings of db/dbtest/canary.go. A test that leaked one into its output
# would print it again if this script echoed that output: when one is found, a fixed
# message replaces the echo. (A random per-run secret is checked inside the tests.)
CANARY_PATTERN='CANARY-D2-(ROW|PW)'

# echo_failing_output prints the output events of "$out" to stderr, unless they hold a canary
# marker or the password of ARTEX_PG_DSN.
echo_failing_output() {
  local pw
  pw="$(printf '%s' "$ARTEX_PG_DSN" | sed -n 's#^[a-z]*://[^:/@]*:\([^@]*\)@.*$#\1#p')"
  if grep -qE "$CANARY_PATTERN" "$out" || { [ "${#pw}" -ge 8 ] && grep -qF -- "$pw" "$out"; }; then
    echo "upgrade-gate: output withheld: it contains a canary marker or the server password (a test leaked it); rerun the failing test by hand" >&2
    return 0
  fi
  grep -E '"Action":"output"' "$out" | sed -e 's/^.*"Output":"//' -e 's/"}$//' -e 's/\\n$//' -e 's/\\t/\t/g' -e 's/\\"/"/g' >&2 || true
}

status=0
go test -json -count=1 -p 1 -run "$PATTERN" "${PACKAGES[@]}" >"$out" || status=$?

passed=$(count '"Action":"pass","Package":"[^"]*","Test":"[^"/]*"')
failed=$(count '"Action":"fail","Package":"[^"]*","Test":"')
skipped=$(count '"Action":"skip","Package":"[^"]*","Test":"')
echo "upgrade-gate: tests passed=$passed skipped=$skipped failed=$failed (minimum passed: $MIN_UPGRADE_TESTS), go test exit $status"

problem=0
if [ "$status" -ne 0 ]; then
  echo "upgrade-gate: FAIL: go test exited $status; failing output follows" >&2
  echo_failing_output
  problem=1
fi
if [ "$skipped" -ne 0 ]; then
  echo "upgrade-gate: FAIL: $skipped test(s) skipped:" >&2
  grep -E '"Action":"skip","Package":"[^"]*","Test":"' "$out" | sed -e 's/^.*"Package":"\([^"]*\)","Test":"\([^"]*\)".*$/  \1 \2/' >&2
  problem=1
fi
if [ "$passed" -lt "$MIN_UPGRADE_TESTS" ]; then
  echo "upgrade-gate: FAIL: $passed top-level tests passed, at least $MIN_UPGRADE_TESTS expected (tests missing or filtered out)" >&2
  problem=1
fi
[ "$problem" -eq 0 ] || exit 1

# Fixtures against git history, by commit id.
git_status=0
go test -json -count=1 -run '^TestGitHistoryFixtures$' ./db >"$out" || git_status=$?
if [ "$git_status" -ne 0 ]; then
  echo "upgrade-gate: FAIL: fixtures differ from git history, or the history is missing (go test exit $git_status)" >&2
  echo_failing_output
  exit 1
fi
if [ "$(count '"Action":"skip","Package":"[^"]*","Test":"TestGitHistoryFixtures"')" -ne 0 ]; then
  echo "upgrade-gate: git history comparison: NOT RUN (no git history here; set UPGRADE_GATE_REQUIRE_GIT=1 to make that a failure)"
else
  echo "upgrade-gate: git history comparison: fixtures equal git show <commit>:db/schema.sql for every release commit"
fi
echo "upgrade-gate: OK"
