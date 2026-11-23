package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// passEnv is the only part of the server's environment that reaches a tool process. Every
// name comes from the server's own environment, the SDK defaults or ARTEX's own BashEnv, so
// none of them is model-chosen. A secret has no place here; the list is closed on purpose.
var passEnv = []string{
	// recording proxy and egress
	"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy",
	"NO_PROXY", "no_proxy", "NODE_USE_ENV_PROXY",
	// CA bundles of the recording proxy
	"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "NODE_EXTRA_CA_CERTS",
	// non-interactive defaults
	"PAGER", "GIT_PAGER", "SYSTEMD_PAGER", "DEBIAN_FRONTEND",
	// terminal and locale
	"TERM", "LANG", "LC_ALL", "TZ", "COLUMNS", "LINES",
	"PLAYWRIGHT_BROWSERS_PATH",
}

// proxyVars are the variables that name an egress proxy. A value with user information in it is
// a credential, and none reaches a tool: the server hands tools the credential-free forwarder
// instead, and anything else that carries a credential (a proxy variable the operator set for the
// server's own use) is dropped here rather than passed on.
var proxyVars = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "http_proxy": true, "https_proxy": true,
	"ALL_PROXY": true, "all_proxy": true,
}

// carriesCredential reports whether a proxy value may hold user:password. An "@" anywhere is
// enough: a value without a scheme ("user:pw@host:3128") does not parse as a URL with user
// information, and a proxy address never needs the character otherwise.
func carriesCredential(v string) bool { return strings.Contains(v, "@") }

// ToolEnv builds the environment of the next stage from the one the root stage received: the
// allow-list, then the fixed values. A later duplicate wins, as it would for the child.
func ToolEnv(environ []string) []string {
	allowed := make(map[string]bool, len(passEnv))
	for _, k := range passEnv {
		allowed[k] = true
	}
	last := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && allowed[k] {
			if proxyVars[k] && carriesCredential(v) {
				delete(last, k) // an earlier value of the same name must not come back either
				continue
			}
			last[k] = v
		}
	}
	out := make([]string, 0, len(last)+5)
	for _, k := range passEnv { // fixed order, so the result is reproducible
		if v, ok := last[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return append(out,
		"HOME="+ToolHome,
		"USER="+ToolUser,
		"LOGNAME="+ToolUser,
		"PATH="+ToolPath,
		"TMPDIR=/tmp",
	)
}

var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// deniedVarPrefixes and deniedVarNames can change how the operator's program loads code or
// where it looks for it. They are harmless after the drop; refusing them keeps a planted or
// model-written entry from steering the program.
var (
	deniedVarPrefixes = []string{"LD_", "GCONV_", "GLIBC_", "MALLOC_", "PYTHON", "PERL5", "RUBY", "GIT_"}
	deniedVarNames    = map[string]bool{"NODE_OPTIONS": true, "BASH_ENV": true, "ENV": true, "PATH": true, "HOME": true, "IFS": true, "SHELL": true, "TMPDIR": true}
)

// DeniedVarError names the key a per-call variable set was refused for. It never carries a
// value.
type DeniedVarError struct{ Key string }

func (e *DeniedVarError) Error() string { return "denied variable name " + e.Key }

// ValidateVarName reports why a per-call variable name is refused, or nil.
func ValidateVarName(k string) error {
	if !varName.MatchString(k) {
		return &DeniedVarError{Key: k}
	}
	if deniedVarNames[k] {
		return &DeniedVarError{Key: k}
	}
	for _, p := range deniedVarPrefixes {
		if strings.HasPrefix(k, p) {
			return &DeniedVarError{Key: k}
		}
	}
	return nil
}

// ValidateVars checks every key and returns the first refusal in key order, so the log line is
// stable.
func ValidateVars(vars map[string]string) error {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := ValidateVarName(k); err != nil {
			return err
		}
		if strings.ContainsRune(vars[k], 0) {
			return fmt.Errorf("variable %s: value contains a NUL byte", k)
		}
	}
	return nil
}

// MaxVarsBytes bounds a variables file.
const MaxVarsBytes = 64 * 1024

// ParseVars reads the JSON object of a variables file: string keys and string values, every
// key valid, at most MaxVarsBytes.
func ParseVars(r io.Reader) (map[string]string, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxVarsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read variables: %w", err)
	}
	if len(data) > MaxVarsBytes {
		return nil, errors.New("variables file is larger than the limit")
	}
	var vars map[string]string
	if err := json.Unmarshal(data, &vars); err != nil {
		return nil, errors.New("variables file is not a JSON object of strings")
	}
	if err := ValidateVars(vars); err != nil {
		return nil, err
	}
	return vars, nil
}

// ApplyVars returns env with the per-call variables set (they replace an earlier entry of the
// same name) and SHELL fixed: the account's login shell is nologin and no tool may derive a
// shell from it.
func ApplyVars(env []string, vars map[string]string) []string {
	drop := map[string]bool{"SHELL": true}
	for k := range vars {
		drop[k] = true
	}
	out := make([]string, 0, len(env)+len(vars)+1)
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+vars[k])
	}
	return append(out, "SHELL=/bin/bash")
}

// WriteVarsFile validates vars and writes them to a new file with a random name directly in
// dir (mode 0600, created exclusively and without following links). It returns the file name,
// which is the only part that appears on a command line. The caller removes the file if the
// spawn fails.
func WriteVarsFile(dir string, vars map[string]string) (string, error) {
	if err := ValidateVars(vars); err != nil {
		return "", err
	}
	body, err := json.Marshal(vars)
	if err != nil {
		return "", fmt.Errorf("encode variables: %w", err)
	}
	if len(body) > MaxVarsBytes {
		return "", errors.New("variables are larger than the limit")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("open variables directory: %w", err)
	}
	defer root.Close()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("random file name: %w", err)
	}
	name := "v-" + hex.EncodeToString(raw[:])
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return "", fmt.Errorf("create variables file: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = root.Remove(name)
		return "", fmt.Errorf("write variables file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(name)
		return "", fmt.Errorf("write variables file: %w", err)
	}
	return name, nil
}

// OpenVarsFile opens a variables file for the root stage. The file must be a regular file
// directly in dir (the name has no separator), opened without following a final link, owned
// by wantUID with mode 0600. It is unlinked as soon as it is open, so only this descriptor
// can read it.
func OpenVarsFile(dir, name string, wantUID uint32) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return nil, errors.New("variables file name is not a plain file name")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open variables directory: %w", err)
	}
	defer root.Close()
	f, err := root.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("open variables file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat variables file: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		f.Close()
		return nil, errors.New("variables file is not a regular file with mode 0600")
	}
	if uid, ok := fileUID(fi); !ok || uid != wantUID {
		f.Close()
		return nil, errors.New("variables file has the wrong owner")
	}
	if err := root.Remove(name); err != nil {
		f.Close()
		return nil, fmt.Errorf("unlink variables file: %w", err)
	}
	return f, nil
}
