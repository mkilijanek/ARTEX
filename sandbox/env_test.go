package sandbox

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// The root stage keeps only the allow-list and adds the fixed values: everything else,
// including every secret-shaped name and every loader variable, is dropped.
func TestToolEnvKeepsOnlyAllowListAndFixedValues(t *testing.T) {
	in := []string{
		"HTTP_PROXY=http://127.0.0.1:8788", "https_proxy=http://127.0.0.1:8788", "NO_PROXY=localhost",
		"SSL_CERT_FILE=/ca.pem", "NODE_EXTRA_CA_CERTS=/ca.pem", "PAGER=cat", "DEBIAN_FRONTEND=noninteractive",
		"TERM=xterm", "LANG=C.UTF-8", "TZ=UTC", "PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright",
		// never passed
		"ARTEX_PG_DSN=postgres://u:pw@h/db", "ANTHROPIC_API_KEY=k", "ARTEX_LLM_PROXY=http://u:p@x",
		"LD_PRELOAD=/tmp/x.so", "LD_LIBRARY_PATH=/tmp", "NODE_OPTIONS=--require /x", "BASH_ENV=/x", "PYTHONPATH=/x",
		"PATH=/tmp/evil", "HOME=/root", "TMPDIR=/run/artex-tmp", "SHELL=/bin/zsh", "USER=root", "PGPASSWORD=x",
		"MALFORMED",
	}
	got := envMap(ToolEnv(in))
	for _, k := range []string{"HTTP_PROXY", "https_proxy", "NO_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "PAGER", "DEBIAN_FRONTEND", "TERM", "LANG", "TZ", "PLAYWRIGHT_BROWSERS_PATH"} {
		if _, ok := got[k]; !ok {
			t.Errorf("allow-listed %s was dropped", k)
		}
	}
	want := map[string]string{"HOME": ToolHome, "USER": ToolUser, "LOGNAME": ToolUser, "PATH": ToolPath, "TMPDIR": "/tmp"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want the fixed %q", k, got[k], v)
		}
	}
	allowed := map[string]bool{}
	for _, k := range passEnv {
		allowed[k] = true
	}
	for k := range want {
		allowed[k] = true
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("name %s is outside the allow-list and the fixed set", k)
		}
	}
	for _, k := range []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "ARTEX_LLM_PROXY", "LD_PRELOAD", "LD_LIBRARY_PATH", "NODE_OPTIONS", "BASH_ENV", "PYTHONPATH", "PGPASSWORD", "SHELL", "MALFORMED"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s reached the tool environment", k)
		}
	}
}

// A later duplicate wins, as it would for the child, and nothing is emitted twice.
func TestToolEnvDuplicatesLastWins(t *testing.T) {
	out := ToolEnv([]string{"TZ=A", "TZ=B", "LANG=x"})
	n := 0
	for _, kv := range out {
		if strings.HasPrefix(kv, "TZ=") {
			n++
			if kv != "TZ=B" {
				t.Errorf("got %q, want the last value", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("TZ appears %d times", n)
	}
}

func TestValidateVarName(t *testing.T) {
	for _, k := range []string{"FOO", "_x", "A1_b", "NODE_ENV", "PLAYWRIGHT_MCP_HEADLESS"} {
		if err := ValidateVarName(k); err != nil {
			t.Errorf("%q refused: %v", k, err)
		}
	}
	for _, k := range []string{
		"", "1A", "A-B", "A B", "A=B", "A\x00", "LD_PRELOAD", "LD_AUDIT", "GCONV_PATH", "GLIBC_TUNABLES", "MALLOC_CONF",
		"PYTHONPATH", "PYTHONSTARTUP", "PERL5OPT", "RUBYOPT", "GIT_SSH_COMMAND", "GIT_EXEC_PATH",
		"NODE_OPTIONS", "BASH_ENV", "ENV", "PATH", "HOME", "IFS", "SHELL", "TMPDIR",
	} {
		if err := ValidateVarName(k); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
}

// The error names the key and never a value.
func TestValidateVarsNamesTheKeyOnly(t *testing.T) {
	err := ValidateVars(map[string]string{"OK": "v", "LD_PRELOAD": "secret-value-xyz"})
	var de *DeniedVarError
	if !errors.As(err, &de) || de.Key != "LD_PRELOAD" {
		t.Fatalf("got %v, want a DeniedVarError for LD_PRELOAD", err)
	}
	if strings.Contains(err.Error(), "secret-value-xyz") {
		t.Fatal("the error carries a value")
	}
}

func TestParseVarsLimitsAndShape(t *testing.T) {
	got, err := ParseVars(strings.NewReader(`{"FOO":"1","BAR":"two"}`))
	if err != nil || !reflect.DeepEqual(got, map[string]string{"FOO": "1", "BAR": "two"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := ParseVars(strings.NewReader(`{"LD_PRELOAD":"x"}`)); err == nil {
		t.Error("a denied key was accepted")
	}
	if _, err := ParseVars(strings.NewReader(`{"FOO":1}`)); err == nil {
		t.Error("a non-string value was accepted")
	}
	if _, err := ParseVars(strings.NewReader(`not json`)); err == nil {
		t.Error("invalid JSON was accepted")
	}
	if _, err := ParseVars(strings.NewReader(`{"FOO":"a\u0000b"}`)); err == nil {
		t.Error("a NUL in a value was accepted")
	}
	big := `{"FOO":"` + strings.Repeat("x", MaxVarsBytes) + `"}`
	if _, err := ParseVars(strings.NewReader(big)); err == nil {
		t.Error("an oversized file was accepted")
	}
}

func TestApplyVarsOverridesAndFixesShell(t *testing.T) {
	out := ApplyVars([]string{"TZ=UTC", "FOO=old"}, map[string]string{"FOO": "new", "BAR": "x"})
	m := envMap(out)
	if m["FOO"] != "new" || m["BAR"] != "x" || m["TZ"] != "UTC" || m["SHELL"] != "/bin/bash" {
		t.Fatalf("got %v", out)
	}
}

func TestVarsFileRoundTripAndUnlink(t *testing.T) {
	dir := t.TempDir()
	name, err := WriteVarsFile(dir, map[string]string{"FOO": "canary-value"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "canary") {
		t.Fatalf("unsafe file name %q", name)
	}
	fi, err := os.Lstat(filepath.Join(dir, name))
	if err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("mode %v err %v, want a regular 0600 file", fi.Mode(), err)
	}
	f, err := OpenVarsFile(dir, name, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("the file was not unlinked at once: %v", err)
	}
	body, _ := io.ReadAll(f)
	if !strings.Contains(string(body), "canary-value") {
		t.Fatalf("content lost: %q", body)
	}
}

// Server side: a denied key is refused before any file exists, naming the key only.
func TestWriteVarsFileRefusesDeniedKeyBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteVarsFile(dir, map[string]string{"LD_PRELOAD": "/tmp/x.so"})
	var de *DeniedVarError
	if !errors.As(err, &de) {
		t.Fatalf("got %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("a file was left behind: %v", ents)
	}
}

func TestOpenVarsFileRefusesUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	uid := uint32(os.Geteuid())
	write := func(name string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"A":"b"}`), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("loose", 0o644)
	write("group", 0o660)
	write("ok", 0o600)
	if err := os.Symlink(filepath.Join(dir, "ok"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deep"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		uid  uint32
	}{
		{"loose", uid}, {"group", uid}, {"link", uid}, {"sub", uid}, {"sub/deep", uid}, {"../ok", uid}, {"", uid},
		{"missing", uid},
		{"ok", uid + 1}, // wrong owner
	} {
		f, err := OpenVarsFile(dir, tc.name, tc.uid)
		if err == nil {
			f.Close()
			t.Errorf("%q (owner %d) was accepted", tc.name, tc.uid)
		}
	}
	// the refusals did not consume the good file
	if _, err := os.Lstat(filepath.Join(dir, "ok")); err != nil {
		t.Fatalf("a refused open removed a file: %v", err)
	}
}

// AC2 (egress proxy part): a proxy variable whose value carries credentials is dropped, however it
// is spelled and wherever it came from; clean values stay.
func TestToolEnvDropsProxyValuesWithCredentials(t *testing.T) {
	in := []string{
		"HTTP_PROXY=http://canaryuser:canarypw@proxy.example:3128",
		"http_proxy=http://canaryuser@proxy.example:3128",
		"HTTPS_PROXY=socks5://canaryuser:canarypw@proxy.example:1080",
		"https_proxy=canaryuser:canarypw@proxy.example:3128", // no scheme
		"ALL_PROXY=http://127.0.0.1:8789",
		"all_proxy=http://127.0.0.1:8789", "all_proxy=http://u:p@x", // a later credential value wins, so it is dropped
		"NO_PROXY=localhost",
	}
	got := envMap(ToolEnv(in))
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "all_proxy"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s=%s was passed", k, v)
		}
	}
	if got["ALL_PROXY"] != "http://127.0.0.1:8789" || got["NO_PROXY"] != "localhost" {
		t.Errorf("a clean value was lost: %v", got)
	}
	for _, kv := range ToolEnv(in) {
		if strings.Contains(kv, "canary") {
			t.Errorf("credential in %q", kv)
		}
	}
}
