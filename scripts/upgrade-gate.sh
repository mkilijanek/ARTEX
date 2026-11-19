#!/usr/bin/env bash
# Gate for the schema upgrade tests and the migration runner tests (docs/phase-d2-plan.md AC11,
# section 5.5; docs/phase-d2-stage2-plan.md AC23, AC28).
#
# A green `go test` can hide tests that did not run: a skipped test reads as passed.
# This script runs the upgrade and migration tests with -json and fails unless
#   * go test exits 0,
#   * at least MIN_UPGRADE_TESTS top-level tests passed,
#   * no test, at any level, was skipped,
# then compares the recorded schema fixtures with git history (TestGitHistoryFixtures), and checks the
# migrations against the release tags: every file of db/migrations/ that any release tag (v*) contains
# must exist, byte-identical, at HEAD, and the upstream files (0001-4999) must be byte-identical to the
# pinned upstream commit when that commit is in the repository (check_migration_tags below).
#
# Usage:
#   ARTEX_PG_DSN=postgres://USER:PASS@HOST:5432/DB?sslmode=disable scripts/upgrade-gate.sh
#   scripts/upgrade-gate.sh --migration-tags [REPO_DIR]    only the migration tag check, no database
#
# ARTEX_PG_DSN must name a PostgreSQL 16 test server: the tests create and drop
# temporary databases named <DB>_<package>_upg_<case> on it and nowhere else, and
# create and drop canary roles named <DB>_<package>_upgrole_<8 hex>.
# One run per base database at a time: the temporary database names are stable (the
# leftover check finds them by prefix) and a test drops a database of its name before it
# creates it, so two runs on one server and one <DB> destroy each other's databases.
# Give a second run its own <DB>.
# UPGRADE_GATE_REQUIRE_GIT=1 turns a missing git history into a failure (hosted CI
# sets it, with a full-depth checkout); without it the comparisons are reported as
# NOT RUN where there is no .git. UPGRADE_GATE_UPSTREAM_COMMIT replaces the pinned upstream commit (the
# self-test of the tag check uses it; nothing else should).
#
# The summary prints counts and test names only, no row content, no DSN. The output of a
# failed run is echoed only when it holds no canary marker (see echo_failing_output).
set -euo pipefail
cd "$(dirname "$0")/.."

# Count of the top-level tests selected by PATTERN, from a run with -v: 128 (./db 72, ./db/dbtest 51, ./agent 1,
# ./server 4): 94 upgrade tests (TestUpgrade*, TestConfigFileDSNIsNotUsed*) and the 34 migration runner tests
# of ./db (TestMigration*, TestLedger*, TestLoadMigrations*), so that none of those can skip unseen. Raise the
# count with the tests, never lower it to make the gate pass.
MIN_UPGRADE_TESTS=128
PATTERN='^(TestUpgrade|TestConfigFileDSNIsNotUsed|TestMigration|TestLedger|TestLoadMigrations)'
PACKAGES=(./db ./db/dbtest ./agent ./server)

# The upstream commit the upstream migration files (0001-4999) were taken from; its blob ids are pinned in
# db/migrate_pins_test.go as well.
UPSTREAM_COMMIT="${UPGRADE_GATE_UPSTREAM_COMMIT:-278de8b2e09a3702f77bbbd065ef35e4b7fdb1d0}"
MIGRATIONS_DIR=db/migrations

# check_migration_tags REPO prints the outcome and returns 0 (identical, or not run) or 1 (a violation).
#   1. For every release tag v* and every file the tag holds under db/migrations/: the file must exist at
#      HEAD with the same git blob id (the same bytes; .gitattributes keeps the bytes unconverted). A
#      changed or a deleted tagged file is a violation. Until the first release with migrations exists this
#      compares nothing, and the pins test (TestMigrationPins) is the guard of that window.
#   2. When UPSTREAM_COMMIT is in the repository: every NNNN_*.sql below 5000 at HEAD must have the blob id
#      it has in that commit; a file that commit does not have is a violation. The object is never fetched.
# Without git history the check is NOT RUN, or a failure with UPGRADE_GATE_REQUIRE_GIT=1. It prints file
# names and short blob ids only.
check_migration_tags() {
  local repo="$1" list tag file want got base number tags_seen=0 files_seen=0 bad=0
  if ! command -v git >/dev/null 2>&1 || ! git -C "$repo" rev-parse --git-dir >/dev/null 2>&1; then
    if [ "${UPGRADE_GATE_REQUIRE_GIT:-}" = "1" ]; then
      echo "upgrade-gate: FAIL: migration check: UPGRADE_GATE_REQUIRE_GIT=1 but there is no git history here" >&2
      return 1
    fi
    echo "upgrade-gate: migration tag check: NOT RUN (no git history here; set UPGRADE_GATE_REQUIRE_GIT=1 to make that a failure)"
    return 0
  fi
  list="$(mktemp "${TMPDIR:-/tmp}/upgrade-gate-files.XXXXXX")"

  while IFS= read -r tag; do
    tags_seen=$((tags_seen + 1))
    if ! git -C "$repo" ls-tree -r -z --name-only "$tag" -- "$MIGRATIONS_DIR" >"$list" 2>/dev/null; then
      echo "upgrade-gate: FAIL: cannot list $MIGRATIONS_DIR in tag $tag" >&2
      bad=1
      continue
    fi
    while IFS= read -r -d '' file; do
      files_seen=$((files_seen + 1))
      want="$(git -C "$repo" rev-parse "$tag:$file")"
      if ! got="$(git -C "$repo" rev-parse --verify -q "HEAD:$file" 2>/dev/null)"; then
        echo "upgrade-gate: FAIL: $file is in release tag $tag and is missing at HEAD (a released migration is never deleted or renamed)" >&2
        bad=1
      elif [ "$got" != "$want" ]; then
        echo "upgrade-gate: FAIL: $file differs at HEAD from release tag $tag (blob ${got:0:12}, tag ${want:0:12}); a released migration is never edited, a new one corrects it" >&2
        bad=1
      fi
    done <"$list"
  done < <(git -C "$repo" tag --list 'v*')
  if [ "$files_seen" -eq 0 ]; then
    echo "upgrade-gate: migration tag check: no release tag holds a file of $MIGRATIONS_DIR yet ($tags_seen tag(s) looked at): nothing to compare"
  elif [ "$bad" -eq 0 ]; then
    echo "upgrade-gate: migration tag check: $files_seen tagged file(s) in $tags_seen tag(s) are identical at HEAD"
  fi

  if git -C "$repo" cat-file -e "${UPSTREAM_COMMIT}^{commit}" 2>/dev/null; then
    if ! git -C "$repo" ls-tree -r -z --name-only HEAD -- "$MIGRATIONS_DIR" >"$list" 2>/dev/null; then
      echo "upgrade-gate: FAIL: cannot list $MIGRATIONS_DIR at HEAD" >&2
      bad=1
    fi
    files_seen=0
    while IFS= read -r -d '' file; do
      base="${file##*/}"
      [[ "$base" =~ ^([0-9]{4})_[a-z0-9_]+\.sql$ ]] || continue
      number=$((10#${BASH_REMATCH[1]}))
      [ "$number" -lt 5000 ] || continue
      files_seen=$((files_seen + 1))
      got="$(git -C "$repo" rev-parse "HEAD:$file")"
      if ! want="$(git -C "$repo" rev-parse --verify -q "${UPSTREAM_COMMIT}:$file" 2>/dev/null)"; then
        echo "upgrade-gate: FAIL: $file is numbered in the upstream range (below 5000) and upstream commit ${UPSTREAM_COMMIT:0:12} has no such file" >&2
        bad=1
      elif [ "$got" != "$want" ]; then
        echo "upgrade-gate: FAIL: $file differs from upstream commit ${UPSTREAM_COMMIT:0:12} (blob ${got:0:12}, upstream ${want:0:12}); upstream files are byte-identical" >&2
        bad=1
      fi
    done <"$list"
    [ "$bad" -ne 0 ] || echo "upgrade-gate: migration upstream check: $files_seen upstream-range file(s) equal upstream commit ${UPSTREAM_COMMIT:0:12}"
  else
    echo "upgrade-gate: migration upstream check: NOT RUN (upstream commit ${UPSTREAM_COMMIT:0:12} is not in this repository; it is never fetched here)"
  fi
  rm -f "$list"
  return "$bad"
}

if [ "${1:-}" = "--migration-tags" ]; then
  check_migration_tags "${2:-.}"
  exit $?
fi

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

check_migration_tags . || exit 1
echo "upgrade-gate: OK"
