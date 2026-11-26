#!/usr/bin/env bash
# Checks (and repairs) the file:line anchors in Markdown documents.
#
# Usage:
#   scripts/check-doc-anchors.sh FILE...            check only, never writes
#   scripts/check-doc-anchors.sh --update FILE...   repair failing anchors
#
# Paths in anchors are relative to the repository root (the parent of scripts/). ANCHOR_ROOT
# replaces that root (the test script uses it).
#
# Anchor kinds (the backticks are part of the syntax):
#   `path:N` `Symbol`           declaration anchor; also `Type.Method`. Line N must be a declaration:
#                               func (with or without receiver, also generic), type, const, var, or an
#                               indented name inside a const ( / var ( block. A call site fails.
#                               With Type.Method the receiver must name Type. A bare name that is
#                               declared more than once in the file is ambiguous.
#   `path:N` `"substring"`      site anchor; line N must contain the exact substring.
#   `path:N-M` `Sym1`..`Sym2`   range anchor; Sym1 at N and Sym2 at M, both as declarations.
# An anchor with none of these tokens fails.
#
# --update rewrites only failing anchors: the line moves to the single matching line of the current
# file (declaration or exact substring); a range moves both ends by the same delta and Sym2 is
# checked again at M. Every rewrite is printed as "old -> new". If any anchor has zero or several
# matches, nothing is written.
#
# Exit codes: 0 all anchors pass (or were repaired), 1 at least one anchor fails (check mode),
# 2 ambiguous or unresolved anchor, or a usage error.
# shellcheck disable=SC2016  # the backticks of the anchor syntax are literal
set -euo pipefail

ROOT="${ANCHOR_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
UPDATE=0
if [ "${1:-}" = "--update" ]; then
	UPDATE=1
	shift
fi
if [ "$#" -eq 0 ]; then
	echo "usage: $0 [--update] FILE..." >&2
	exit 2
fi
for f in "$@"; do
	if [ ! -f "$f" ]; then
		echo "error: document not found: $f" >&2
		exit 2
	fi
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

CHECKED=0
FAILING=0
UNRESOLVED=0
MSG=""

# decl_lines FILE SYMBOL: print the line numbers where SYMBOL is declared (one per line).
decl_lines() {
	awk -v sym="$2" '
	BEGIN {
		n = split(sym, p, ".")
		typ = ""
		name = sym
		if (n == 2) { typ = p[1]; name = p[2] }
		blk = 0
	}
	/^(const|var) \($/ { blk = 1; next }
	blk && /^\)/ { blk = 0; next }
	{
		hit = 0
		if (typ == "") {
			if ($0 ~ ("^func (\\([^)]*\\) )?" name "[\\[(]")) hit = 1
			else if ($0 ~ ("^type " name "([^A-Za-z0-9_]|$)")) hit = 1
			else if ($0 ~ ("^(const|var) " name "([^A-Za-z0-9_]|$)")) hit = 1
			else if (blk && $0 ~ ("^\t" name "([^A-Za-z0-9_]|$)")) hit = 1
		} else {
			if ($0 ~ ("^func \\((([^)]*[^A-Za-z0-9_])|)" typ "([^A-Za-z0-9_][^)]*)?\\) " name "[\\[(]")) hit = 1
		}
		if (hit) print NR
	}' "$1"
}

# site_lines FILE SUBSTRING: print the line numbers that contain SUBSTRING.
site_lines() {
	grep -n -F -- "$2" "$1" | cut -d: -f1 || true
}

# lines_of KIND FILE TARGET: matching line numbers for a declaration or a site target.
lines_of() {
	if [ "$1" = "sub" ]; then site_lines "$2" "$3"; else decl_lines "$2" "$3"; fi
}

# check_end KIND FILE LINE TARGET: 0 pass, 1 fail, 2 ambiguous. Sets MSG.
check_end() {
	local kind="$1" file="$2" line="$3" target="$4" hits count
	hits="$(lines_of "$kind" "$file" "$target")"
	if [ "$kind" = "sub" ]; then
		if [ "$line" -ge 1 ] && [ "$(sed -n "${line}p" "$file" | grep -c -F -- "$target" || true)" -ge 1 ]; then
			return 0
		fi
		MSG="line $line does not contain \"$target\""
		return 1
	fi
	count="$(printf '%s\n' "$hits" | grep -c . || true)"
	if [ "$count" -gt 1 ]; then
		MSG="ambiguous: $target is declared $count times ($(printf '%s' "$hits" | tr '\n' ' ')); use Type.Method"
		return 2
	fi
	if printf '%s\n' "$hits" | grep -q -x -- "$line"; then
		return 0
	fi
	MSG="line $line is not a declaration of $target"
	return 1
}

# resolve_end KIND FILE TARGET: sets NEWN to the single matching line; returns 1 otherwise (sets MSG).
resolve_end() {
	local hits count
	hits="$(lines_of "$1" "$2" "$3")"
	count="$(printf '%s\n' "$hits" | grep -c . || true)"
	if [ "$count" -eq 1 ]; then
		NEWN="$hits"
		return 0
	fi
	if [ "$count" -eq 0 ]; then
		MSG="unresolved: no match for $3"
	else
		MSG="unresolved: several matches for $3 (lines $(printf '%s' "$hits" | tr '\n' ' '))"
	fi
	return 1
}

# check_anchor PATH N M KIND TARGET TARGET2: 0 pass, 1 fail, 2 ambiguous. Sets MSG.
check_anchor() {
	local path="$1" n="$2" m="$3" kind="$4" t1="$5" t2="$6" file="$ROOT/$1" rc
	if [ ! -f "$file" ]; then
		MSG="file not found: $path"
		return 1
	fi
	if [ -z "$kind" ]; then
		MSG="anchor has neither a symbol nor a substring"
		return 1
	fi
	rc=0
	check_end "$kind" "$file" "$n" "$t1" || rc=$?
	if [ "$rc" -ne 0 ]; then return "$rc"; fi
	if [ -n "$t2" ]; then
		check_end sym "$file" "$m" "$t2" || rc=$?
	fi
	return "$rc"
}

# repair_anchor PATH N M KIND TARGET TARGET2: sets NEWN and NEWM; returns 1 when unresolved (sets MSG).
repair_anchor() {
	local path="$1" n="$2" m="$3" kind="$4" t1="$5" t2="$6" file="$ROOT/$1" delta
	if [ ! -f "$file" ] || [ -z "$kind" ]; then
		MSG="unresolved: $MSG"
		return 1
	fi
	resolve_end "$kind" "$file" "$t1" || return 1
	NEWM=""
	if [ -n "$t2" ]; then
		delta=$((NEWN - n))
		NEWM=$((m + delta))
		if ! check_end sym "$file" "$NEWM" "$t2"; then
			MSG="unresolved: after moving $path:$n-$m by $delta, $MSG"
			return 1
		fi
	fi
	return 0
}

# process_doc DOC INDEX: check every anchor; in update mode write the rewritten text to $WORK/out.INDEX.
process_doc() {
	local doc="$1" idx="$2" line rest out head after tok full lineno=0 rc path n m kind t1 t2 newhead eol
	local head_re='`([A-Za-z0-9_./-]*\.[A-Za-z0-9_]+):([0-9]+)(-([0-9]+))?`'
	local tok_re='^[[:space:]]+`("[^`]*"|[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?)`(\.\.`([A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?)`)?'
	local changed=0
	: >"$WORK/out.$idx"
	while IFS= read -r line || [ -n "$line" ]; do
		lineno=$((lineno + 1))
		rest="$line"
		out=""
		while [[ $rest =~ $head_re ]]; do
			head="${BASH_REMATCH[0]}"
			path="${BASH_REMATCH[1]}"
			n="${BASH_REMATCH[2]}"
			m="${BASH_REMATCH[4]}"
			out+="${rest%%"$head"*}"
			after="${rest#*"$head"}"
			kind=""
			t1=""
			t2=""
			tok=""
			if [[ $after =~ $tok_re ]]; then
				tok="${BASH_REMATCH[0]}"
				t1="${BASH_REMATCH[1]}"
				t2="${BASH_REMATCH[4]}"
				if [[ $t1 == \"* ]]; then
					kind="sub"
					t1="${t1#\"}"
					t1="${t1%\"}"
				else
					kind="sym"
				fi
				if [ "$kind" = "sub" ] && { [ -n "$m" ] || [ -n "$t2" ]; }; then
					kind=""
				fi
				if [ -n "$m" ] && [ -z "$t2" ]; then kind=""; fi
				if [ -z "$m" ] && [ -n "$t2" ]; then kind=""; fi
			fi
			full="$head$tok"
			CHECKED=$((CHECKED + 1))
			MSG=""
			rc=0
			check_anchor "$path" "$n" "$m" "$kind" "$t1" "$t2" || rc=$?
			if [ "$kind" = "" ] && [ -n "$tok" ] && [ "$rc" -eq 1 ] && [ -f "$ROOT/$path" ]; then
				MSG="malformed anchor (a range needs two symbols, a substring cannot be a range)"
			elif [ "$kind" = "" ] && [ -f "$ROOT/$path" ]; then
				MSG="anchor has neither a symbol nor a substring (\`$path:$n\`)"
			fi
			if [ "$rc" -ne 0 ]; then
				FAILING=$((FAILING + 1))
				if [ "$UPDATE" -eq 0 ]; then
					[ "$rc" -eq 2 ] && UNRESOLVED=$((UNRESOLVED + 1))
					echo "$doc:$lineno: $path:$n${m:+-$m} $t1${t2:+..$t2}: $MSG" >&2
				elif repair_anchor "$path" "$n" "$m" "$kind" "$t1" "$t2"; then
					newhead="\`$path:$NEWN${NEWM:+-$NEWM}\`"
					echo "$doc:$lineno: $path:$n${m:+-$m} -> $path:$NEWN${NEWM:+-$NEWM}"
					full="$newhead$tok"
					changed=1
				else
					UNRESOLVED=$((UNRESOLVED + 1))
					echo "$doc:$lineno: $path:$n${m:+-$m} $t1${t2:+..$t2}: $MSG" >&2
					full="$head$tok"
				fi
			fi
			out+="$full"
			rest="${after#"$tok"}"
		done
		out+="$rest"
		eol=$'\n'
		printf '%s%s' "$out" "$eol" >>"$WORK/out.$idx"
	done <"$doc"
	# A document without a final newline keeps it that way.
	if [ -s "$doc" ] && [ -n "$(tail -c1 "$doc")" ]; then
		truncate -s -1 "$WORK/out.$idx"
	fi
	echo "$changed" >"$WORK/changed.$idx"
}

i=0
for doc in "$@"; do
	i=$((i + 1))
	process_doc "$doc" "$i"
done

if [ "$UPDATE" -eq 1 ]; then
	if [ "$UNRESOLVED" -gt 0 ]; then
		echo "$UNRESOLVED anchors unresolved ($CHECKED checked): nothing written" >&2
		exit 2
	fi
	i=0
	for doc in "$@"; do
		i=$((i + 1))
		if [ "$(cat "$WORK/changed.$i")" = "1" ]; then
			cat "$WORK/out.$i" >"$doc"
		fi
	done
	echo "$CHECKED anchors checked, $FAILING repaired"
	exit 0
fi

echo "$CHECKED anchors checked, $FAILING failing, $UNRESOLVED ambiguous"
if [ "$UNRESOLVED" -gt 0 ]; then exit 2; fi
if [ "$FAILING" -gt 0 ]; then exit 1; fi
exit 0
