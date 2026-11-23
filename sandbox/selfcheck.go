package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// SelfCheckInput names what the check looks at. Nothing in it is a secret value: only paths,
// ids and endpoints.
type SelfCheckInput struct {
	ServerUID int // uid of the server process (os.Geteuid)
	ServerPID int
	Key       string // the signing key file
	// ConfigPath is checked only when Config says the file exists.
	ConfigPath string
	Config     bool
	DataDir    string
	VarsDir    string
	SkillsDir  string
	Skills     bool     // the skills directory exists
	Symlinks   []string // top-level entries of the data directory that are symbolic links
	Sockets    []string // database socket files
	TCP        []string // database endpoints reached over TCP, host:port

	// SignalSocket is the Signal bridge socket file (ARTEX_SIGNAL_API). Empty: no C12 probe.
	SignalSocket string

	// WorkDirParent is where the check creates its own workspace, a fresh tool-owned directory.
	WorkDirParent string
	// FallbackDir is where the shell runs when that workspace cannot be created (a full disk):
	// C4 is then reported failed and the other checks still run. Empty: no fallback.
	FallbackDir string
}

// dockerSocket is the Docker socket path the check looks for.
const dockerSocket = "/var/run/docker.sock"

// metadataAddr is the cloud metadata endpoint.
const metadataHost, metadataPort = "169.254.169.254", "80"

// shq quotes a string for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeProbeHelpers writes the shell functions every probe uses.
func writeProbeHelpers(w func(format string, a ...any)) {
	w(`can_read() { head -c1 "$1" >/dev/null 2>&1; }`)
	w(`can_list() { ls "$1" >/dev/null 2>&1; }`)
	w(`probe_read() { if can_read "$2"; then echo "$1=readable"; else echo "$1=denied"; fi; }`)
	w(`probe_list() { if can_list "$2"; then echo "$1=readable"; else echo "$1=denied"; fi; }`)
	// connect to a unix socket with python; unknown when python is missing. -I -S: the working
	// directory (a tool-owned workspace), PYTHON* variables and site packages cannot change the
	// answer.
	w(`probe_unix() {`)
	w(`  if ! command -v python3 >/dev/null 2>&1; then echo "$1=unknown"; return; fi`)
	w(`  if python3 -I -S -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.settimeout(2); s.connect(sys.argv[1])' "$2" >/dev/null 2>&1; then echo "$1=connected"; else echo "$1=refused"; fi`)
	w(`}`)
	// connect over TCP with bash; unknown when timeout or bash is missing
	w(`probe_tcp() {`)
	w(`  if ! command -v timeout >/dev/null 2>&1 || ! command -v bash >/dev/null 2>&1; then echo "$1=unknown"; return; fi`)
	w(`  if timeout 2 bash -c 'exec 3<>/dev/tcp/$0/$1' "$2" "$3" >/dev/null 2>&1; then echo "$1=open"; else echo "$1=closed"; fi`)
	w(`}`)
}

// SelfCheckScript builds the shell script the check runs as the tool user. It prints facts as
// key=value lines and always exits 0; EvaluateSelfCheck decides what they mean.
func SelfCheckScript(in SelfCheckInput) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	writeProbeHelpers(w)
	w(`echo "uid=$(id -u)"`)
	w(`echo "gid=$(id -g)"`)
	w(`echo "groups=$(id -G)"`)
	w(`grep -E '^(CapInh|CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs):' /proc/self/status | tr -d '\t ' | sed 's/:/=/'`)
	w(`echo "envnames=$(env | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' | tr '\n' ' ')"`)
	w(`probe_read c3.proc1 /proc/1/environ`)
	w(`probe_read c3.procself /proc/%d/environ`, in.ServerPID)
	w(`probe_read c3.key %s`, shq(in.Key))
	if in.Config {
		w(`probe_read c3.config %s`, shq(in.ConfigPath))
	}
	w(`probe_list c3.traffic %s`, shq(filepath.Join(in.DataDir, "traffic")))
	w(`probe_list c3.vars %s`, shq(in.VarsDir))
	for i, l := range in.Symlinks {
		w(`probe_read c3.link%d %s`, i, shq(l))
	}
	// a block of data, not an empty file: a full disk still has room for an inode
	w(`if head -c 4096 /dev/zero > "$PWD/.selfcheck-probe" 2>/dev/null; then rm -f "$PWD/.selfcheck-probe"; echo c4.write=ok; else rm -f "$PWD/.selfcheck-probe" 2>/dev/null; echo c4.write=fail; fi`)
	if in.Skills {
		w(`if t=$(mktemp %s 2>/dev/null); then rm -f "$t"; echo c5.skills=writable; else echo c5.skills=notwritable; fi`, shq(filepath.Join(in.SkillsDir, ".selfcheck.XXXXXX")))
	}
	for i, s := range in.Sockets {
		w(`probe_unix c6.%d %s`, i, shq(s))
	}
	for i, ep := range in.TCP {
		host, port := splitEndpoint(ep)
		w(`probe_tcp c7.%d %s %s`, i, shq(host), shq(port))
	}
	w(`if [ -S %s ]; then probe_unix c8.0 %s; else echo c8.0=refused; fi`, shq(dockerSocket), shq(dockerSocket))
	w(`probe_tcp c9.meta %s %s`, shq(metadataHost), shq(metadataPort))
	writeC12Probes(w, in)
	w(`exit 0`)
	return b.String()
}

// writeC12Probes writes the C12 probes: none without a Signal socket, and none while the server's
// own pre-check says the socket is absent (EvaluateSelfCheck reports that without the tool shell).
func writeC12Probes(w func(format string, a ...any), in SelfCheckInput) {
	if in.SignalSocket == "" || c12Precheck(in.SignalSocket) != "" {
		return
	}
	w(`probe_unix c12.sock %s`, shq(in.SignalSocket))
	w(`probe_list c12.dir %s`, shq(filepath.Dir(in.SignalSocket)))
}

// c12Precheck is the root-side half of C12. probe_unix maps ENOENT and EACCES both to "refused", so
// the server, which can see the path, first says whether a socket is there at all. It returns the
// detail of a "not verified" C12, or "" when the path is a socket and the tool user may be probed.
func c12Precheck(path string) string {
	fi, err := os.Lstat(path)
	switch {
	case err == nil && fi.Mode().Type() == os.ModeSocket:
		return ""
	case err == nil, errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return "not verified: socket absent"
	default:
		return "not verified: " + err.Error()
	}
}

// evaluateC12 turns the answers of the C12 probes into a failed check. A tool user that is refused
// and cannot list the parent directory is the pass (ok false). Anything else degrades: it can
// connect, it can list the directory holding the socket, or the probe proved nothing.
func evaluateC12(kv map[string]string, path string) (CheckResult, bool) {
	if p := c12Precheck(path); p != "" {
		return CheckResult{ID: "C12", Detail: p, Degrades: true}, true
	}
	var bad []string
	switch kv["c12.sock"] {
	case "refused":
	case "connected":
		bad = append(bad, "tool user can connect to "+path)
	default: // unknown, or no answer
		bad = append(bad, "the tool user's access to "+path+" could not be verified")
	}
	dir := filepath.Dir(path)
	switch kv["c12.dir"] {
	case "denied":
	case "readable":
		bad = append(bad, "tool user can list "+dir+", which holds "+path)
	default:
		bad = append(bad, "the tool user's access to "+dir+", which holds "+path+", could not be verified")
	}
	if len(bad) == 0 {
		return CheckResult{}, false
	}
	return CheckResult{ID: "C12", Detail: strings.Join(bad, "; "), Degrades: true}, true
}

// parseProbeOutput reads the script's key=value lines.
func parseProbeOutput(out string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func splitEndpoint(ep string) (host, port string) {
	i := strings.LastIndex(ep, ":")
	if i < 0 {
		return ep, "0"
	}
	return ep[:i], ep[i+1:]
}

// fixedEnvNames are the names a tool shell has besides the allow-list: the fixed values
// __astool and __toolenv set, and the ones bash adds.
var fixedEnvNames = []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "SHELL", "PWD", "OLDPWD", "SHLVL", "_"}

// EvaluateSelfCheck reads the script's answers and returns the checks that failed. A probe
// with no answer counts as failed. A probe that could not be run (unknown) or gave no answer
// degrades the status even for the checks that otherwise only warn (C7-C9): it proves nothing.
func EvaluateSelfCheck(out string, in SelfCheckInput) []CheckResult {
	kv := parseProbeOutput(out)
	var failed []CheckResult
	add := func(id, detail string, degrades bool) {
		failed = append(failed, CheckResult{ID: id, Detail: detail, Degrades: degrades})
	}

	// C1: the shell is the dropped one.
	var c1 []string
	uid, uidOK := kv["uid"]
	if !uidOK || uid != strconv.Itoa(ToolUID) {
		c1 = append(c1, "uid is "+orMissing(uid, uidOK))
	}
	if g, ok := kv["gid"]; !ok || g != strconv.Itoa(ToolGID) {
		c1 = append(c1, "gid is "+orMissing(g, ok))
	}
	if g, ok := kv["groups"]; !ok || strings.TrimSpace(g) != strconv.Itoa(ToolGID) {
		c1 = append(c1, "supplementary groups are "+orMissing(g, ok))
	}
	for _, c := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		if v, ok := kv[c]; !ok || strings.Trim(v, "0") != "" {
			c1 = append(c1, c+" is "+orMissing(v, ok))
		}
	}
	if v, ok := kv["NoNewPrivs"]; !ok || v != "1" {
		c1 = append(c1, "no_new_privs is "+orMissing(v, ok))
	}
	if names, ok := kv["envnames"]; !ok {
		c1 = append(c1, "the environment was not reported")
	} else {
		allowed := map[string]bool{}
		for _, n := range append(append([]string(nil), passEnv...), fixedEnvNames...) {
			allowed[n] = true
		}
		var extra []string
		for _, n := range strings.Fields(names) {
			if !allowed[n] {
				extra = append(extra, n)
			}
		}
		if len(extra) > 0 {
			c1 = append(c1, "unexpected environment names: "+strings.Join(extra, " "))
		}
	}
	if len(c1) > 0 {
		add("C1", "the tool shell is not the dropped one: "+strings.Join(c1, "; "), true)
	}

	// C2: the tool user is not the server's user.
	if !uidOK || uid != strconv.Itoa(ToolUID) || in.ServerUID == ToolUID {
		add("C2", fmt.Sprintf("tool uid %s, server uid %d", orMissing(uid, uidOK), in.ServerUID), true)
	}

	// C3: the server's secrets cannot be read.
	var c3 []string
	reach := func(key, what string) {
		if kv[key] != "denied" {
			c3 = append(c3, what)
		}
	}
	reach("c3.proc1", "the environment of process 1")
	reach("c3.procself", "the environment of the server process")
	reach("c3.key", "the signing key")
	if in.Config {
		reach("c3.config", "the configuration file")
	}
	reach("c3.traffic", "the traffic directory")
	reach("c3.vars", "the variables directory")
	for i, l := range in.Symlinks {
		reach("c3.link"+strconv.Itoa(i), "the linked entry "+l)
	}
	if len(c3) > 0 {
		add("C3", "tools can reach: "+strings.Join(c3, ", "), true)
	}

	// C4: a tool-owned workspace is writable.
	if kv["c4.write"] != "ok" {
		add("C4", "a tool-owned directory is not writable", true)
	}

	// C5: the skills directory is not writable by tools.
	if in.Skills && kv["c5.skills"] != "notwritable" {
		add("C5", "the skills directory is writable by tools", true)
	}

	// C6: the database socket cannot be reached.
	var c6 []string
	for i, s := range in.Sockets {
		switch kv["c6."+strconv.Itoa(i)] {
		case "refused":
		case "unknown":
			c6 = append(c6, s+" could not be verified")
		default:
			c6 = append(c6, s+" accepts a connection")
		}
	}
	if len(c6) > 0 {
		add("C6", strings.Join(c6, "; "), true)
	}

	// C7-C9 only warn when the probe ran; an unknown or missing answer degrades.
	var c7, c7unknown []string
	for i, ep := range in.TCP {
		switch v, ok := kv["c7."+strconv.Itoa(i)]; {
		case !ok || v == "unknown":
			c7unknown = append(c7unknown, ep)
		case v != "closed":
			c7 = append(c7, ep)
		}
	}
	switch {
	case len(c7unknown) > 0:
		detail := "the database's TCP endpoints could not be verified: " + strings.Join(c7unknown, ", ")
		if len(c7) > 0 {
			detail += "; reachable over TCP: " + strings.Join(c7, ", ")
		}
		add("C7", detail, true)
	case len(c7) > 0:
		add("C7", "the database is reachable over TCP: "+strings.Join(c7, ", "), false)
	}
	switch v, ok := kv["c8.0"]; {
	case !ok || v == "unknown":
		add("C8", "the Docker socket could not be verified", true)
	case v == "connected":
		add("C8", "the Docker socket is reachable", false)
	}
	switch v, ok := kv["c9.meta"]; {
	case !ok || v == "unknown":
		add("C9", "the cloud metadata address could not be verified", true)
	case v != "closed":
		add("C9", "the cloud metadata address is reachable", false)
	}

	// C12: the Signal bridge socket cannot be reached, or is not there to be verified.
	if in.SignalSocket != "" {
		if c, bad := evaluateC12(kv, in.SignalSocket); bad {
			failed = append(failed, c)
		}
	}
	return failed
}

func orMissing(v string, ok bool) string {
	if !ok {
		return "not reported"
	}
	return v
}

// ScriptRunner runs a shell script as a tool process would run and returns its output. dir is
// the working directory.
type ScriptRunner func(ctx context.Context, dir, script string) (string, error)

// SDKRunner runs the script through the SDK's own Bash tool, so the shim and the drop chain
// are exercised exactly as in a session.
func SDKRunner(ctx context.Context, dir, script string) (string, error) {
	input, err := json.Marshal(map[string]any{"command": script, "timeout_ms": 30000})
	if err != nil {
		return "", err
	}
	res, err := actool.NewBash().Call(ctx, input, &actool.ToolContext{WorkingDir: dir})
	if err != nil {
		return "", err
	}
	out := res.Flatten()
	if res.IsError {
		return out, fmt.Errorf("the check shell failed: %s", firstLine(out))
	}
	return out, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// RunSelfCheck creates a fresh tool-owned workspace, runs the script in it as the tool user and
// returns the failed checks. Every failure it reports is a status of degraded or less: a
// check a tool could make fail never switches isolation off.
func RunSelfCheck(ctx context.Context, run ScriptRunner, in SelfCheckInput) []CheckResult {
	if in.WorkDirParent == "" {
		in.WorkDirParent = filepath.Join(in.DataDir, "tasks")
	}
	dir, err := makeCheckDir(in.WorkDirParent)
	var c4 []CheckResult
	if err != nil {
		c4 = []CheckResult{{ID: "C4", Detail: "cannot create a tool-owned workspace: " + err.Error(), Degrades: true}}
		if in.FallbackDir == "" {
			// without a workspace there is no shell to inspect
			return append(c4, CheckResult{ID: "C1", Detail: "the tool shell could not be started for the check", Degrades: true})
		}
		dir = in.FallbackDir
	} else {
		defer os.RemoveAll(dir)
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := run(cctx, dir, SelfCheckScript(in))
	if err != nil {
		return []CheckResult{
			{ID: "C1", Detail: "the tool shell failed to run: " + err.Error(), Degrades: true},
			{ID: "C2", Detail: "the tool user could not be observed", Degrades: true},
		}
	}
	failed := EvaluateSelfCheck(out, in)
	if c4 != nil {
		kept := failed[:0:0]
		for _, f := range failed {
			if f.ID != "C4" { // the write probe ran in the fallback directory and proves nothing here
				kept = append(kept, f)
			}
		}
		failed = append(kept, c4...)
		sort.SliceStable(failed, func(a, b int) bool { return failed[a].ID < failed[b].ID })
	}
	return failed
}

// c12Script is the script of a C12 re-run: the helpers and the two C12 probes only.
func c12Script(in SelfCheckInput) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	writeProbeHelpers(w)
	writeC12Probes(w, in)
	w(`exit 0`)
	return b.String()
}

// RunSignalCheck runs C12 alone and returns it when it failed (nil when it passed or when there is
// no Signal socket). The root-side pre-check comes first and skips the tool shell when the socket is
// absent. Like RunSelfCheck it never reports anything worse than degraded.
func RunSignalCheck(ctx context.Context, run ScriptRunner, in SelfCheckInput) []CheckResult {
	if in.SignalSocket == "" {
		return nil
	}
	if p := c12Precheck(in.SignalSocket); p != "" {
		return []CheckResult{{ID: "C12", Detail: p, Degrades: true}}
	}
	if in.WorkDirParent == "" {
		in.WorkDirParent = filepath.Join(in.DataDir, "tasks")
	}
	dir, err := makeCheckDir(in.WorkDirParent)
	if err != nil {
		if in.FallbackDir == "" {
			return []CheckResult{{ID: "C12", Detail: "the tool shell could not be started for the check: " + err.Error(), Degrades: true}}
		}
		dir = in.FallbackDir
	} else {
		defer os.RemoveAll(dir)
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := run(cctx, dir, c12Script(in))
	if err != nil {
		return []CheckResult{{ID: "C12", Detail: "the tool shell failed to run: " + err.Error(), Degrades: true}}
	}
	if c, bad := evaluateC12(parseProbeOutput(out), in.SignalSocket); bad {
		return []CheckResult{c}
	}
	return nil
}

// makeCheckDir creates a fresh directory under parent, owned by the tool user, mode 0700.
func makeCheckDir(parent string) (string, error) {
	if err := os.MkdirAll(parent, 0o711); err != nil {
		return "", err
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, ".selfcheck-"+hex.EncodeToString(raw[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Lchown(dir, ToolUID, ToolGID); err != nil {
		os.Remove(dir)
		return "", err
	}
	return dir, nil
}
