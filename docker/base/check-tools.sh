#!/bin/bash
# Checks the tools of the ARTEX tools-base image from inside the image.
#
#   check-tools.sh [slim|full]   run every row, print PASS or FAIL per row, exit 1 on any FAIL
#                                (the variant defaults to $ARTEX_VARIANT)
#   check-tools.sh scan-libs     print the shared libraries every ELF file of the tools loads
#                                (LIB <path>) and the ones that cannot be resolved (MISSING <file> <lib>);
#                                the image build uses it to keep the libraries the tools need
#
# CHECK_VERBOSE=1 also prints the whole output of every passing row. CHECK_BUILD_SANDBOX=1 is for
# the image build only (see ping_unavailable_in_build).
#
# Run it as root. Rows marked "root" run as root; rows marked "tool" run as uid 30002 through
# the same privilege drop the server uses for model-driven commands: setgroups([]), the id
# change, empty inheritable, ambient and bounding sets and no_new_privs (sandbox/stages.go,
# SetprivArgv), with the environment the server builds for a tool process (sandbox/env.go,
# ToolEnv) and the process limit its root stage sets. The server's own binary is not part of
# this image, so the chain here is setpriv only.
#
# The files that sit next to this script are read from there: setuid-expected.txt.

set -u
export LC_ALL=C

TOOL_UID=30002
TOOL_GID=30002
TOOL_HOME=/home/artex-tool
TOOL_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
TOOL_NPROC=4096
BBOT_HOME=/opt/bbot-home
BROWSERS=${PLAYWRIGHT_BROWSERS_PATH:-/opt/ms-playwright}
# bbot dry-run modules of the tool matrix, and the one only the full variant has
DRY_MODULES=wafw00f,paramminer_getparams,lightfuzz,sslcert,gowitness,nuclei,legba,medusa

# ---------------------------------------------------------------- library scan ---

is_elf() {
  local magic
  IFS= read -r -N4 magic <"$1" || return 1
  [[ $magic == $'\x7f'ELF ]]
}

# Every ELF file the tools use: the tool binaries, bbot's venv, its tool and build trees, the
# browsers and the global node modules. Plain find: a link is not followed, its target is found
# where it lives.
scan_libs() {
  local -a dirs=()
  local d f out line n=0
  for d in /usr/local/bin /opt/bbot /opt/bbot-home/.bbot /opt/ms-playwright /usr/lib/node_modules; do
    [ -d "$d" ] && dirs+=("$d")
  done
  while IFS= read -r -d '' f; do
    is_elf "$f" || continue
    n=$((n + 1))
    out=$(ldd "$f" 2>&1) || true
    # a static or relocatable file is not a dynamic executable: nothing to resolve
    case $out in *"not a dynamic executable"* | *"statically linked"*) continue ;; esac
    while IFS= read -r line; do
      case $line in
        *"=> not found"*) echo "MISSING $f ${line%% =>*}" | tr -d '\t' ;;
        *"=> /"*) line=${line#*=> }; echo "LIB ${line%% (*}" ;;
        *linux-vdso* | */ld-linux-*) ;;
        *) echo "UNPARSED $f $line" ;;
      esac
    done <<<"$out"
  done < <(find "${dirs[@]}" -type f -print0)
  echo "scan-libs: $n ELF files scanned" >&2
}

# ---------------------------------------------------------------- identities ---

# run_root and run_tool start the check function (or program) named by their arguments in a
# clean environment. The script runs itself with __exec so that its own functions are available
# in both identities.
run_root() {
  env -i HOME=/root PATH="$TOOL_PATH" TMPDIR=/tmp PLAYWRIGHT_BROWSERS_PATH="$BROWSERS" TERM=dumb \
    CT_WORK="$WORK" CT_PORT="$PORT" CT_VARIANT="$VARIANT" CT_WHO=root \
    bash "$WORK/check-tools.sh" __exec "$@"
}

run_tool() {
  prlimit --nproc="$TOOL_NPROC:$TOOL_NPROC" \
    setpriv --reuid="$TOOL_UID" --regid="$TOOL_GID" --clear-groups \
    --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
    env -i HOME="$TOOL_HOME" USER=artex-tool LOGNAME=artex-tool PATH="$TOOL_PATH" TMPDIR=/tmp \
    PLAYWRIGHT_BROWSERS_PATH="$BROWSERS" TERM=dumb \
    CT_WORK="$WORK" CT_PORT="$PORT" CT_VARIANT="$VARIANT" CT_WHO=tool \
    bash "$WORK/check-tools.sh" __exec "$@"
}

tools_dir() { if [ "${CT_WHO:-}" = tool ]; then echo "$TOOL_HOME/.bbot/tools"; else echo "$BBOT_HOME/.bbot/tools"; fi; }

# ---------------------------------------------------------------- rows ---

pass=0
fail=0
skip=0

report() { # report <PASS|FAIL> <who> <name> <output>
  local first
  first=$(printf '%s\n' "$4" | grep -m1 . | cut -c1-140)
  printf '%-4s %-4s %s\n' "$1" "$2" "$3"
  if [ "$1" = FAIL ]; then
    printf '%s\n' "$4" | tail -n 40 | sed 's/^/       | /'
  elif [ -n "${CHECK_VERBOSE:-}" ]; then
    printf '%s\n' "$4" | sed 's/^/       > /'
  elif [ -n "$first" ]; then
    printf '       > %s\n' "$first"
  fi
}

# row <who> <name> <command...>: passes when the command exits 0.
row() {
  local who=$1 name=$2 out rc
  shift 2
  out=$("run_$who" "$@" 2>&1) && rc=0 || rc=$?
  if [ "$rc" -eq 0 ]; then pass=$((pass + 1)); report PASS "$who" "$name" "$out"
  else fail=$((fail + 1)); report FAIL "$who" "$name (exit $rc)" "$out"; fi
}

# absent <who> <name> <command...>: passes when the command exits non-zero (the tool is not there).
absent() {
  local who=$1 name=$2 out rc
  shift 2
  out=$("run_$who" "$@" 2>&1) && rc=0 || rc=$?
  if [ "$rc" -ne 0 ]; then pass=$((pass + 1)); report PASS "$who" "$name" "exit $rc"
  else fail=$((fail + 1)); report FAIL "$who" "$name (found)" "$out"; fi
}

# skip <who> <name> <reason>: a row that cannot run here. It is listed and counted, never a PASS.
skip() {
  skip=$((skip + 1))
  printf '%-4s %-4s %s\n       > %s\n' SKIP "$1" "$2" "$3"
}

# The build sandbox of docker build has no net.ipv4.ping_group_range, so an unprivileged ping
# (no capability, the way tools run) cannot open an ICMP socket there. CHECK_BUILD_SANDBOX=1,
# set by the image build only, lets that one row skip when the range excludes the tool group.
ping_unavailable_in_build() {
  local lo hi
  [ -n "${CHECK_BUILD_SANDBOX:-}" ] || return 1
  read -r lo hi </proc/sys/net/ipv4/ping_group_range || return 1
  [ "$TOOL_GID" -lt "$lo" ] || [ "$TOOL_GID" -gt "$hi" ]
}

both() { local name=$1; shift; row root "$name" "$@"; row tool "$name" "$@"; }

# ---------------------------------------------------------------- check functions ---
# These run inside run_root / run_tool (through __exec), so they see CT_WORK, CT_PORT, CT_WHO.

f_id_chain() {
  local want="uid=$TOOL_UID gid=$TOOL_GID groups=$TOOL_GID" got k v
  got="uid=$(id -u) gid=$(id -g) groups=$(id -G)"
  [ "$got" = "$want" ] || { echo "identity is '$got', want '$want'"; return 1; }
  grep -q '^NoNewPrivs:[[:space:]]*1$' /proc/self/status || { echo "no_new_privs is not set"; return 1; }
  while read -r k v; do
    case $k in
      CapInh: | CapPrm: | CapEff: | CapBnd: | CapAmb:)
        [ -z "${v//0/}" ] || { echo "$k $v is not empty"; return 1; } ;;
    esac
  done </proc/self/status
  echo "$got NoNewPrivs=1, capability sets empty"
}

f_bbot_dry() { # f_bbot_dry <modules>: dry run, no dependency install, no error lines
  local log="$CT_WORK/tool/bbot-dry-$$.log"
  ( cd "$HOME" && timeout -k 10 300 bbot --dry-run --no-deps -y -t example.com -m "$1" --no-color ) >"$log" 2>&1 \
    || { cat "$log"; return 1; }
  if grep -E 'Installing|ERRR|CRIT' "$log"; then echo "error lines in the dry run"; return 1; fi
  echo "dry run ok: $1"
  rm -f "$log"
}

f_ldd_clean() {
  local out
  out=$(scan_libs 2>&1) || return 1
  if printf '%s\n' "$out" | grep -E '^(MISSING|UNPARSED)'; then return 1; fi
  printf '%s\n' "$out" | grep -E '^scan-libs'
}

f_dlopen_pcap() { # libpcap is opened by masscan at scan time, so ldd never lists it
  ldconfig -p | grep -q 'libpcap\.so\.0\.8 ' || { echo "libpcap.so.0.8 is not in the loader cache"; return 1; }
  echo "libpcap.so.0.8 resolvable"
}

f_node_playwright() { NODE_PATH=$(npm root -g) node -e "require('playwright'); console.log('playwright module loads')"; }

f_nmap() { nmap -sT -p 80 127.0.0.1; }

f_setuid() {
  local expected got
  expected=$(sort "$CT_WORK/setuid-expected.txt") || return 1
  got=$(find / -xdev -perm /6000 -type f | sort)
  if [ "$got" != "$expected" ]; then
    echo "setuid/setgid files differ from setuid-expected.txt (< expected, > found):"
    diff <(printf '%s\n' "$expected") <(printf '%s\n' "$got")
    return 1
  fi
  printf '%s\n' "$got" | wc -l | sed 's/$/ setuid\/setgid files, as expected/'
}

f_chrome_link() {
  local l=$BBOT_HOME/.bbot/tools/chrome-linux target
  [ -L "$l" ] || { echo "$l is not a symlink"; return 1; }
  target=$(readlink -f "$l")
  case $target in "$BROWSERS"/chromium-*/chrome-linux*) ;; *) echo "$l points to $target"; return 1 ;; esac
  [ -x "$target/chrome" ] || { echo "$target/chrome is not executable"; return 1; }
  echo "$l -> $target"
}

f_tool_bin() { local d; d=$(tools_dir); "$d/$1" "${@:2}"; }

# f_tool_usage <regex> <tool> <args...>: for a tool whose help exits non-zero (masscan --help
# exits 1). The row still fails when the tool does not start: a loader error does not match.
f_tool_usage() {
  local re=$1 out
  out=$(f_tool_bin "${@:2}" 2>&1)
  printf '%s\n' "$out" | grep -qE "$re" || { printf '%s\n' "$out" | head -5; return 1; }
  printf '%s\n' "$out" | grep -m1 -E "$re"
}

f_chrome_version() { "$(tools_dir)/chrome-linux/chrome" --version; }

f_nuclei_link() {
  local l=$BBOT_HOME/.bbot/tools/nuclei
  [ -L "$l" ] && [ "$(readlink "$l")" = /usr/local/bin/nuclei ] || { echo "$l is not a link to /usr/local/bin/nuclei"; return 1; }
  echo "$l -> /usr/local/bin/nuclei"
}

f_not_writable() {
  local out
  out=$(find "$BROWSERS" "$BBOT_HOME" /opt/bbot /usr/local/bin -writable 2>&1)
  [ -z "$out" ] || { echo "writable for uid $(id -u):"; printf '%s\n' "$out" | head -20; return 1; }
  echo "nothing under $BROWSERS, $BBOT_HOME, /opt/bbot, /usr/local/bin is writable for uid $(id -u)"
}

f_hello() { # full: gcc builds and runs a program
  local d="$CT_WORK/tool/hello"
  mkdir -p "$d" && cd "$d" || return 1
  printf '#include <stdio.h>\nint main(void){puts("hello from gcc");return 0;}\n' >hello.c
  gcc -o hello hello.c && ./hello
}

# gowitness screenshot of the page the check serves, with bbot's gowitness and the chromium it is
# linked to. The command line of the chromium main process is read from /proc while it runs.
f_gowitness() {
  local gw chrome d="$CT_WORK/tool/gw" log="$CT_WORK/tool/gw.log" cmdlines="$CT_WORK/tool/gw.cmdlines" pid c main
  gw=$(tools_dir)/gowitness
  chrome=$(tools_dir)/chrome-linux/chrome
  rm -rf "$d"; mkdir -p "$d"; : >"$cmdlines"
  timeout -k 10 180 "$gw" scan single --url "http://127.0.0.1:$CT_PORT/" --screenshot-path "$d" \
    --chrome-path "$chrome" --screenshot-format png --write-none --delay 1 >"$log" 2>&1 &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    for c in /proc/[0-9]*/cmdline; do
      # a process can end between the glob and the read: that is the only error this hides
      { tr '\0' ' ' <"$c"; } 2>/dev/null | grep -E 'chrome-linux/chrome' >>"$cmdlines" || true
    done
    sleep 0.05
  done
  wait "$pid" || { echo "gowitness failed:"; cat "$log"; return 1; }
  main=$(grep -v -- '--type=' "$cmdlines" | sort -u | head -1)
  [ -n "$main" ] || { echo "no chromium main process command line was captured"; cat "$log"; return 1; }
  echo "chromium sandbox flags: $(printf '%s\n' "$main" | grep -oE -- '--(no-sandbox|disable-setuid-sandbox|disable-gpu-sandbox|no-zygote)' | sort -u | tr '\n' ' ')"
  echo "chromium command line: $main"
  find "$d" -name '*.png' -size +0 | grep -q . || { echo "no non-empty PNG in $d"; cat "$log"; return 1; }
  echo "screenshot: $(find "$d" -name '*.png' -size +0 -exec ls -l {} + | head -1)"
}

f_mcp() { # Playwright MCP over stdio: initialize, tools/list, browser_navigate, browser_snapshot
  local d="$CT_WORK/tool/mcp/browser"
  rm -rf "$d"; mkdir -p "$d" && cd "$d" || return 1
  timeout -k 10 240 python3 "$CT_WORK/mcp-driver.py" "http://127.0.0.1:$CT_PORT/" "$MARKER"
}

MARKER=artex-check-marker-5f3a

write_mcp_driver() {
  cat >"$WORK/mcp-driver.py" <<'PY'
import json, os, queue, signal, subprocess, sys, threading, time

url, marker = sys.argv[1], sys.argv[2]
cmd = ["npx", "@playwright/mcp", "--headless", "--browser", "chromium"]
p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                     text=True, bufsize=1, start_new_session=True)
out_q, err_lines = queue.Queue(), []
threading.Thread(target=lambda: [out_q.put(l) for l in p.stdout], daemon=True).start()
threading.Thread(target=lambda: err_lines.extend(p.stderr), daemon=True).start()

def stop():
    try:
        os.killpg(p.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass

def fail(msg):
    stop()
    print("FAIL:", msg)
    print("stderr:", "".join(err_lines)[-2000:])
    sys.exit(1)

def send(obj):
    p.stdin.write(json.dumps(obj) + "\n")
    p.stdin.flush()

def call(rid, method, params, timeout=90):
    send({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            line = out_q.get(timeout=1)
        except queue.Empty:
            if p.poll() is not None:
                fail(f"{method}: server exited with {p.returncode}")
            continue
        msg = json.loads(line)
        if msg.get("id") != rid:
            continue
        if "error" in msg:
            fail(f"{method}: {msg['error']}")
        res = msg.get("result", {})
        if res.get("isError"):
            fail(f"{method}: {json.dumps(res)[:500]}")
        return res
    fail(f"{method}: no answer within {timeout}s")

init = call(1, "initialize", {"protocolVersion": "2024-11-05", "capabilities": {},
                              "clientInfo": {"name": "check-tools", "version": "0"}})
print("initialize:", init.get("serverInfo"))
send({"jsonrpc": "2.0", "method": "notifications/initialized"})
tools = [t["name"] for t in call(2, "tools/list", {}).get("tools", [])]
for need in ("browser_navigate", "browser_snapshot"):
    if need not in tools:
        fail(f"tools/list has no {need}: {tools}")
print("tools/list:", len(tools), "tools")
call(3, "tools/call", {"name": "browser_navigate", "arguments": {"url": url}})
print("browser_navigate: ok")
snap = call(4, "tools/call", {"name": "browser_snapshot", "arguments": {}})
text = json.dumps(snap)
if marker not in text:
    fail(f"browser_snapshot does not contain {marker}: {text[:500]}")
print("browser_snapshot: contains the marker text")
stop()
PY
}

# ---------------------------------------------------------------- driver ---

main_checks() {
  local who

  echo "== recon binaries"
  both "nuclei -version" nuclei -version
  both "httpx -version" httpx -version
  both "subfinder -version" subfinder -version
  both "katana -version" katana -version
  both "vulnx --help" vulnx --help
  both "ffuf -V" ffuf -V
  both "dalfox --version" dalfox --version
  both "sqlmap --version" sqlmap --version

  echo "== bbot and its tools"
  both "bbot --version" bbot --version
  row tool "bbot dry run ($DRY_MODULES)" f_bbot_dry "$DRY_MODULES"
  if [ "$VARIANT" = full ]; then row tool "bbot dry run (jadx)" f_bbot_dry jadx; fi
  both "legba on PATH" command -v legba
  both "legba --version" legba --version
  for who in root tool; do
    # the tools are not on the server's PATH; bbot finds them in its tools directory
    row "$who" "masscan --help" f_tool_usage '^usage: masscan' masscan --help
    row "$who" "massdns --help" f_tool_bin massdns --help
    row "$who" "medusa -V" f_tool_bin medusa -V
  done
  row root "libpcap.so.0.8 resolvable (masscan opens it at scan time)" f_dlopen_pcap
  row tool "gowitness screenshot (bbot's gowitness, chromium via chrome-linux)" f_gowitness
  row tool "gowitness binary and chromium both run" f_chrome_version

  echo "== browser"
  both "playwright-cli --help" playwright-cli --help
  both "node require('playwright') from the global prefix" f_node_playwright
  row tool "Playwright MCP session (initialize, tools/list, navigate, snapshot)" f_mcp

  echo "== base tools"
  row tool "nmap -sT -p 80 127.0.0.1" f_nmap
  if ping_unavailable_in_build; then
    skip tool "ping -c1 127.0.0.1" "net.ipv4.ping_group_range='$(cat /proc/sys/net/ipv4/ping_group_range)' excludes gid $TOOL_GID in this build sandbox; run check-tools.sh in a container"
  else
    row tool "ping -c1 127.0.0.1" ping -c1 127.0.0.1
  fi
  both "git --version" git --version
  both "node -v" node -v
  both "python3 -V" python3 -V
  both "rg --version" rg --version
  both "curl -V" curl -V
  both "wget -V" wget -V
  both "jq --version" jq --version
  both "dig -v" dig -v
  both "nc -h (OpenBSD netcat usage)" bash -c 'nc -h 2>&1 | grep -q "OpenBSD netcat"'
  both "whois --version" whois --version
  both "vim --version" vim --version
  both "unzip -v" unzip -v
  both "ps aux" ps aux
  both "pgrep -x bash || true (pgrep runs; no match is fine)" bash -c 'pgrep -x bash || true'
  both "pkill --version" pkill --version
  both "killall --version" killall --version
  both "free -m" free -m

  echo "== variant rows ($VARIANT)"
  if [ "$VARIANT" = slim ]; then
    for who in root tool; do
      absent "$who" "absent: java" command -v java
      absent "$who" "absent: jadx" command -v jadx
      absent "$who" "absent: gcc" command -v gcc
      absent "$who" "absent: make" command -v make
      absent "$who" "absent: cc" command -v cc
      absent "$who" "absent: bbot jadx tree" test -e /opt/bbot-home/.bbot/tools/jadx
    done
  else
    both "java -version" java -version
    both "jadx --version" f_tool_bin jadx/bin/jadx --version
    row tool "gcc builds and runs a program" f_hello
    both "gcc --version" gcc --version
    both "make --version" make --version
  fi

  echo "== libraries"
  both "ldd: no unresolved library in the tool trees" f_ldd_clean

  echo "== isolation"
  row tool "drop chain: uid, gid, groups, no_new_privs, capability sets" f_id_chain
  row root "setuid/setgid files equal setuid-expected.txt" f_setuid
  row root "tools/chrome-linux is a symlink to Playwright's chromium" f_chrome_link
  row root "tools/nuclei is a link to /usr/local/bin/nuclei" f_nuclei_link
  row tool "browser and tool trees are not writable by uid $TOOL_UID" f_not_writable
}

if [ "${1:-}" = __exec ]; then
  shift
  # functions and shell builtins run in this shell; a program replaces it, so that the
  # process the caller started is the program itself
  case $(type -t "$1") in
    function | builtin) "$@"; exit ;;
    *) exec "$@" ;;
  esac
fi

if [ "${1:-}" = scan-libs ]; then
  scan_libs
  exit
fi

VARIANT=${1:-${ARTEX_VARIANT:-}}
case $VARIANT in slim | full) ;; *) echo "usage: $0 [slim|full] | scan-libs (variant '$VARIANT')" >&2; exit 2 ;; esac
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 2; }

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d /tmp/check-tools.XXXXXX)
HTTP_PID=
cleanup() {
  [ -z "$HTTP_PID" ] || kill "$HTTP_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

chmod 0755 "$WORK"
install -m 0644 "$0" "$WORK/check-tools.sh"
install -m 0644 "$HERE/setuid-expected.txt" "$WORK/setuid-expected.txt"
install -d -o "$TOOL_UID" -g "$TOOL_GID" -m 0700 "$WORK/tool"
install -d -m 0755 "$WORK/www"
printf '<!doctype html><title>check</title><p>%s</p>\n' "$MARKER" >"$WORK/www/index.html"
write_mcp_driver
chmod 0644 "$WORK/mcp-driver.py"

PORT=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
# the page the gowitness and MCP rows load, served by the tool user inside the container
run_tool python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$WORK/www" >"$WORK/http.log" 2>&1 &
HTTP_PID=$!
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null && break
  sleep 0.1
done
curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" || { echo "the check's web server did not start:"; cat "$WORK/http.log"; exit 1; }

echo "check-tools: variant=$VARIANT host_uid=$(id -u) tool_uid=$TOOL_UID"
main_checks
echo "check-tools: $pass passed, $fail failed, $skip skipped (variant $VARIANT)"
[ "$fail" -eq 0 ]
