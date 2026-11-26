#!/usr/bin/env bash
# Tests for scripts/check-doc-anchors.sh. Every case builds its own fixture tree in a temporary
# directory and asserts the exit code and a message fragment.
# Usage: bash scripts/check-doc-anchors_test.sh
# shellcheck disable=SC2016  # the backticks of the anchor syntax are literal
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-doc-anchors.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
n=0
OUT=""
RC=0

# new_case: fresh fixture root with the Go sources the cases share.
new_case() {
	n=$((n + 1))
	ROOT="$WORK/case$n"
	mkdir -p "$ROOT/agent" "$ROOT/docs"
	printf '%s\n' \
		'package agent' \
		'' \
		'func caller() {' \
		'	x := compress(1)' \
		'	_ = x' \
		'}' \
		'' \
		'func compress(n int) int {' \
		'	return n' \
		'}' \
		'' \
		'func Map[T any](v T) T {' \
		'	return v' \
		'}' \
		'' \
		'const (' \
		'	ModeA = 1' \
		'	ModeB = 2' \
		')' \
		'' \
		'func (a *Alpha) Run() {}' \
		'func (b *Beta) Run() {}' \
		'func (a *Alpha) Stop() {}' \
		'' \
		'func start() {' \
		'	log("begin")' \
		'	log("end")' \
		'}' >"$ROOT/agent/a.go"
}

# run_check ARGS...: run the checker against the fixture root, capture output and exit code.
run_check() {
	set +e
	OUT="$(ANCHOR_ROOT="$ROOT" bash "$CHECK" "$@" 2>&1)"
	RC=$?
	set -e
}

# expect NAME WANT_RC FRAGMENT: assert the last run_check result.
expect() {
	local name="$1" want="$2" frag="$3" ok=1
	[ "$RC" -eq "$want" ] || ok=0
	case "$OUT" in *"$frag"*) ;; *) ok=0 ;; esac
	if [ "$ok" -eq 1 ]; then
		pass=$((pass + 1))
		printf 'ok   %s\n' "$name"
	else
		fail=$((fail + 1))
		printf 'FAIL %s: want rc=%s containing [%s], got rc=%s:\n%s\n' "$name" "$want" "$frag" "$RC" "$OUT"
	fi
}

doc() { printf '%s\n' "$@" >"$ROOT/docs/d.md"; }

# --- check mode ---------------------------------------------------------------
new_case
doc 'Declaration: `agent/a.go:8` `compress` here.'
run_check "$ROOT/docs/d.md"
expect "declaration anchor on a declaration passes" 0 ""

new_case
doc 'Call site: `agent/a.go:4` `compress` here.'
run_check "$ROOT/docs/d.md"
expect "declaration anchor on a call site fails" 1 "not a declaration of compress"

new_case
doc 'Grouped: `agent/a.go:17` `ModeA`, `agent/a.go:18` `ModeB`.'
run_check "$ROOT/docs/d.md"
expect "const block member passes" 0 ""

new_case
doc 'Generic: `agent/a.go:12` `Map`.'
run_check "$ROOT/docs/d.md"
expect "generic func passes" 0 ""

new_case
doc 'Bare: `agent/a.go:22` `Run`.'
run_check "$ROOT/docs/d.md"
expect "bare method name on two receivers is ambiguous" 2 "ambiguous"

new_case
doc 'Typed: `agent/a.go:22` `Beta.Run`, `agent/a.go:21` `Alpha.Run`.'
run_check "$ROOT/docs/d.md"
expect "Type.Method resolves per receiver" 0 ""

new_case
doc 'Wrong receiver: `agent/a.go:21` `Beta.Run`.'
run_check "$ROOT/docs/d.md"
expect "Type.Method with another receiver fails" 1 "not a declaration of Beta.Run"

new_case
doc 'Site: `agent/a.go:4` `"x := compress(1)"`.'
run_check "$ROOT/docs/d.md"
expect "site anchor passes when the line contains the substring" 0 ""

new_case
doc 'Site: `agent/a.go:5` `"x := compress(1)"`.'
run_check "$ROOT/docs/d.md"
expect "site anchor fails when the line moved" 1 "does not contain"

new_case
doc 'Missing: `agent/nope.go:3` `compress`.'
run_check "$ROOT/docs/d.md"
expect "missing file fails" 1 "file not found"

new_case
doc 'Bare: `agent/a.go:8` and nothing else.'
run_check "$ROOT/docs/d.md"
expect "anchor without symbol or substring fails" 1 "neither a symbol nor a substring"

new_case
doc 'Past the end: `agent/a.go:999` `compress`.'
run_check "$ROOT/docs/d.md"
expect "line beyond the end of file fails" 1 "not a declaration of compress"

new_case
doc 'Range: `agent/a.go:8-10` `compress`..`Map`.'
run_check "$ROOT/docs/d.md"
expect "range with a wrong end fails" 1 "not a declaration of Map"

new_case
doc 'Range: `agent/a.go:8-12` `compress`..`Map`.'
run_check "$ROOT/docs/d.md"
expect "range passes when both ends match" 0 ""

new_case
doc 'Nothing to check here, `plain` text.'
run_check "$ROOT/docs/d.md"
expect "document without anchors passes" 0 ""

new_case
doc 'Call site: `agent/a.go:4` `compress` here.'
cp "$ROOT/docs/d.md" "$WORK/before.md"
run_check "$ROOT/docs/d.md"
if cmp -s "$ROOT/docs/d.md" "$WORK/before.md"; then
	pass=$((pass + 1))
	printf 'ok   check mode never writes\n'
else
	fail=$((fail + 1))
	printf 'FAIL check mode never writes\n'
fi

# --- update mode ----------------------------------------------------------------
# Lines are shifted by inserting two lines at the top of the Go file.
shift_go() { { printf '// a\n// b\n'; cat "$ROOT/agent/a.go"; } >"$ROOT/agent/a.go.new" && mv "$ROOT/agent/a.go.new" "$ROOT/agent/a.go"; }

new_case
doc 'Decl: `agent/a.go:8` `compress` and site `agent/a.go:26` `"begin"`.'
shift_go
run_check --update "$ROOT/docs/d.md"
expect "update rewrites a moved declaration anchor" 0 "agent/a.go:8 -> agent/a.go:10"
if grep -q '`agent/a.go:10` `compress`' "$ROOT/docs/d.md"; then
	pass=$((pass + 1))
	printf 'ok   update wrote the declaration anchor\n'
else
	fail=$((fail + 1))
	printf 'FAIL update wrote the declaration anchor: %s\n' "$(cat "$ROOT/docs/d.md")"
fi
run_check "$ROOT/docs/d.md"
expect "document passes after update" 0 ""

new_case
doc 'Site: `agent/a.go:26` `"begin"`.'
shift_go
run_check --update "$ROOT/docs/d.md"
expect "update rewrites a moved site anchor" 0 "agent/a.go:26 -> agent/a.go:28"

new_case
doc 'Range: `agent/a.go:8-12` `compress`..`Map`.'
shift_go
run_check --update "$ROOT/docs/d.md"
expect "update shifts both ends of a range by the same delta" 0 "agent/a.go:8-12 -> agent/a.go:10-14"
run_check "$ROOT/docs/d.md"
expect "range passes after update" 0 ""

new_case
doc 'Range: `agent/a.go:8-10` `compress`..`Map`.'
shift_go
cp "$ROOT/docs/d.md" "$WORK/before.md"
run_check --update "$ROOT/docs/d.md"
expect "update refuses a range whose end does not match after the shift" 2 "unresolved"
cmp -s "$ROOT/docs/d.md" "$WORK/before.md" && r=0 || r=1
if [ "$r" -eq 0 ]; then
	pass=$((pass + 1))
	printf 'ok   unresolved range leaves the document untouched\n'
else
	fail=$((fail + 1))
	printf 'FAIL unresolved range leaves the document untouched\n'
fi

# All or nothing: one anchor is repairable, the other has no match anywhere.
new_case
doc 'Good: `agent/a.go:8` `compress`.' 'Gone: `agent/a.go:3` `"does not exist anywhere"`.'
shift_go
cp "$ROOT/docs/d.md" "$WORK/before.md"
run_check --update "$ROOT/docs/d.md"
expect "update with one unresolved anchor exits 2" 2 "unresolved"
if cmp -s "$ROOT/docs/d.md" "$WORK/before.md"; then
	pass=$((pass + 1))
	printf 'ok   update writes nothing when one anchor is unresolved\n'
else
	fail=$((fail + 1))
	printf 'FAIL update writes nothing when one anchor is unresolved: %s\n' "$(cat "$ROOT/docs/d.md")"
fi

# All or nothing across documents.
new_case
printf '%s\n' 'Good: `agent/a.go:8` `compress`.' >"$ROOT/docs/one.md"
printf '%s\n' 'Gone: `agent/a.go:3` `"does not exist anywhere"`.' >"$ROOT/docs/two.md"
shift_go
cp "$ROOT/docs/one.md" "$WORK/before.md"
run_check --update "$ROOT/docs/one.md" "$ROOT/docs/two.md"
if [ "$RC" -eq 2 ] && cmp -s "$ROOT/docs/one.md" "$WORK/before.md"; then
	pass=$((pass + 1))
	printf 'ok   update writes no document when another document is unresolved\n'
else
	fail=$((fail + 1))
	printf 'FAIL update writes no document when another document is unresolved (rc=%s)\n' "$RC"
fi

# Ambiguous: a substring present twice cannot be resolved.
new_case
doc 'Twice: `agent/a.go:3` `"log("`.'
run_check --update "$ROOT/docs/d.md"
expect "update with several matches exits 2" 2 "several"

# Only failing anchors are touched; passing text stays byte-identical.
new_case
doc 'Keep   spacing: `agent/a.go:12` `Map`;  moved: `agent/a.go:8` `compress`.'
# Map is at 12; insert two lines after it is not needed: shift everything and keep Map valid by
# making the passing anchor point at the shifted line while only compress is stale.
shift_go
sed -i 's/a.go:12` `Map`/a.go:14` `Map`/' "$ROOT/docs/d.md"
run_check --update "$ROOT/docs/d.md"
want='Keep   spacing: `agent/a.go:14` `Map`;  moved: `agent/a.go:10` `compress`.'
if [ "$(cat "$ROOT/docs/d.md")" = "$want" ]; then
	pass=$((pass + 1))
	printf 'ok   update touches only the failing anchor\n'
else
	fail=$((fail + 1))
	printf 'FAIL update touches only the failing anchor: %s\n' "$(cat "$ROOT/docs/d.md")"
fi

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
