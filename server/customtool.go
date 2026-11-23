package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/sandbox"
	"github.com/mkilijanek/artex/useragent"
)

// This file implements the custom tool executor (docs/custom-tool-design.md). tools rows
// with system=false are dispatched by kind: command (render the command → reuse the Bash
// low-level run), script (Python only; write a temp file, pass parameters via stdin JSON
// + env TOOL_*, use the configured interpreter), http (native request + optional proxy). Like traffic/orchestration tools they need no seeding (they already live in the tools table); they are injected via hostTools and filtered by binding.

// ---------- custom tool CRUD ----------

type customToolReq struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Enabled     bool            `json:"enabled"`
	Kind        string          `json:"kind"` // command | script | http
	Exec        json.RawMessage `json:"exec"`
	Deferred    bool            `json:"deferred"`
}

var reToolKey = reAgentKey // same rule as agent key: starts with a lowercase letter, lowercase letters/digits/underscores only

func (s *Server) pgCreateCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req customToolReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if !reToolKey.MatchString(req.Key) {
		writeErr(w, 400, "key must start with a lowercase letter and contain only lowercase letters/digits/underscores")
		return
	}
	if req.Kind != "command" && req.Kind != "script" && req.Kind != "http" && req.Kind != "shell" {
		writeErr(w, 400, "kind must be command / script / http / shell")
		return
	}
	if req.Kind == "http" && !hasSchemaProps(req.Schema) {
		writeErr(w, 400, "http tools must provide a parameter JSON Schema (cannot be left empty)")
		return
	}
	if exist, _ := pg.GetTool(req.Key); exist != nil {
		writeErr(w, 409, "this key already exists (built-in or custom tool)")
		return
	}
	if err := pg.CreateCustomTool(&db.Tool{
		Key: req.Key, Description: req.Description, Schema: req.Schema, Agents: req.Agents,
		Enabled: req.Enabled, Kind: req.Kind, Exec: req.Exec, Deferred: req.Deferred,
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"key": req.Key})
}

func (s *Server) pgUpdateCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	existing, err := pg.GetTool(key)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if existing == nil || existing.System {
		writeErr(w, 400, "only custom tools can be edited")
		return
	}
	var req customToolReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Kind != "command" && req.Kind != "script" && req.Kind != "http" && req.Kind != "shell" {
		writeErr(w, 400, "kind must be command / script / http / shell")
		return
	}
	if req.Kind == "http" && !hasSchemaProps(req.Schema) {
		writeErr(w, 400, "http tools must provide a parameter JSON Schema (cannot be left empty)")
		return
	}
	if err := pg.UpdateCustomTool(&db.Tool{
		Key: key, Description: req.Description, Schema: req.Schema, Agents: req.Agents,
		Enabled: req.Enabled, Kind: req.Kind, Exec: req.Exec, Deferred: req.Deferred,
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) pgDeleteCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	if err := pg.DeleteCustomTool(key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": key})
}

// testToolReq is a dry-run request from the editor: run the given (possibly unsaved)
// exec spec with sample params, without persisting the tool. Same executor path as a
// real tool call — it runs arbitrary command/script/http on the server, which the
// custom-tool feature already allows, so no new capability is granted.
type testToolReq struct {
	Kind   string          `json:"kind"` // command | script | http
	Exec   json.RawMessage `json:"exec"`
	Params map[string]any  `json:"params"`
}

// pgTestCustomTool executes an exec spec once and returns its raw output + error
// flag, so the editor can debug a tool before saving it. Per-kind timeouts still
// apply from the exec spec (with defaults); the outer ceiling is a hard backstop.
func (s *Server) pgTestCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req testToolReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid request body")
		return
	}
	params := req.Params
	if params == nil {
		params = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	// A command or script run starts a tool process outside any session: hold a lease so the idle
	// sweep cannot end it mid-run.
	lease, err := sandbox.Current().Leases().Acquire(ctx)
	if err != nil {
		writeErr(w, 503, "tool processes are being restarted; retry")
		return
	}
	defer lease.Release()
	tc := &actool.ToolContext{WorkingDir: s.m.dir} // run in the project dir, like a real call
	var res actool.Result
	switch req.Kind {
	case "command":
		res, _ = s.runCommandTool(ctx, req.Exec, params, tc)
	case "script":
		res, _ = s.runScriptTool(ctx, "test", req.Exec, params, tc)
	case "http":
		res, _ = s.runHTTPTool(ctx, req.Exec, params, tc)
	case "shell":
		writeErr(w, 400, "shell-kind tools are bash environment declarations with no executable content")
		return
	default:
		writeErr(w, 400, "unknown tool kind: "+req.Kind)
		return
	}
	writeJSON(w, 200, map[string]any{"output": res.Flatten(), "is_error": res.IsError})
}

// ---------- Python interpreter (detect + persist + override) ----------

const settingPythonInterp = "python_interpreter"

// detectPython finds a python interpreter absolute path (python3 preferred).
func detectPython() string {
	for _, c := range []string{"python3", "python"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// pythonInterpreter resolves the interpreter: user-set > stored auto-detect > live
// detect. "" only when truly none found.
func (s *Server) pythonInterpreter() string {
	if v, ok, _ := s.m.pg.GetSetting(settingPythonInterp); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return detectPython()
}

// seedPythonInterpreter stores the auto-detected interpreter on startup if unset
// (never clobbers a user-set value).
func (s *Server) seedPythonInterpreter() {
	if v, ok, _ := s.m.pg.GetSetting(settingPythonInterp); ok && strings.TrimSpace(v) != "" {
		return
	}
	if p := detectPython(); p != "" {
		_ = s.m.pg.SetSetting(settingPythonInterp, p)
		log.Printf("[custom-tool] auto-detected python interpreter: %s", p)
	}
}

// ---------- exec spec ----------

type commandExec struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms"`
}
type scriptExec struct {
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms"`
}
type httpExec struct {
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers"`
	Body              string            `json:"body"`
	TimeoutMs         int               `json:"timeout_ms"`
	Proxy             string            `json:"proxy"`
	UseRecordingProxy bool              `json:"use_recording_proxy"`
}

func timeoutOr(ms, def int) time.Duration {
	if ms <= 0 {
		return time.Duration(def) * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// ---------- common tool construction ----------

// customTools builds CoreTools for every user-defined (system=false) tool row.
// shell-kind tools are environment hints only — they surface in the Bash tool
// description via ToolResolve and do NOT create callable tool entries here.
func (s *Server) customTools() ([]actool.CoreTool, error) {
	rows, err := s.m.pg.ListCustomTools()
	if err != nil {
		return nil, err
	}
	out := make([]actool.CoreTool, 0, len(rows))
	for _, t := range rows {
		if t.Kind == "shell" {
			continue // shell hints are handled by ToolResolve → Bash description
		}
		out = append(out, s.buildCustomTool(t))
	}
	return out, nil
}

// buildCustomTool turns one custom-tool row into a CoreTool. Empty schema → a thin
// {args:string} (thin-wrapper tool), so command/http templates can use {args}.
func (s *Server) buildCustomTool(t *db.Tool) actool.CoreTool {
	schema := ensureSchema(t.Schema)
	key, kind, execRaw := t.Key, t.Kind, t.Exec
	run := func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
		var params map[string]any
		if len(in) > 0 {
			_ = json.Unmarshal(in, &params)
		}
		if params == nil {
			params = map[string]any{}
		}
		switch kind {
		case "command":
			return s.runCommandTool(ctx, execRaw, params, tc)
		case "script":
			return s.runScriptTool(ctx, key, execRaw, params, tc)
		case "http":
			return s.runHTTPTool(ctx, execRaw, params, tc)
		default:
			return actool.Errorf("unknown custom tool kind: " + kind), nil
		}
	}
	return actool.Build(actool.Spec{
		Name: key, Description: t.Description, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: run,
	})
}

// hasSchemaProps reports whether raw is a JSON-Schema object with ≥1 property.
// http tools require an explicit schema (the auto {args} shell can't name the
// {param} placeholders in URL/headers/body), so an empty schema is rejected.
func hasSchemaProps(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	props, _ := m["properties"].(map[string]any)
	return len(props) > 0
}

// ensureSchema returns the tool's schema, or a thin {args:string} when none given.
func ensureSchema(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	props, _ := m["properties"].(map[string]any)
	if len(props) > 0 {
		return m
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{"type": "string", "description": "command/parameters (free text)"},
		},
	}
}

// ---------- command: render the command → reuse the Bash low-level run ----------

func (s *Server) runCommandTool(ctx context.Context, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec commandExec
	_ = json.Unmarshal(execRaw, &spec)
	if strings.TrimSpace(spec.Command) == "" {
		return actool.Errorf("command is empty"), nil
	}
	cmd := renderTemplate(spec.Command, params, shellQuote)
	// Reuse the low-level run Bash also uses (via the Bash CoreTool.Call): it
	// automatically inherits the safety floor/timeout/proxy env/output overflow. The tool is a peer of Bash sharing that low level, not routed through the Bash tool for the model to call.
	bashIn, _ := json.Marshal(map[string]any{"command": cmd})
	if spec.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeoutOr(spec.TimeoutMs, 120000))
		defer cancel()
	}
	return actool.NewBash().Call(ctx, bashIn, tc)
}

// ---------- script (Python only): temp file + stdin JSON + env ----------

func (s *Server) runScriptTool(ctx context.Context, key string, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec scriptExec
	_ = json.Unmarshal(execRaw, &spec)
	if strings.TrimSpace(spec.Code) == "" {
		return actool.Errorf("script code is empty"), nil
	}
	interp := s.pythonInterpreter()
	if interp == "" {
		return actool.Errorf("no python interpreter configured or detected (set it in system settings)"), nil
	}
	workDir := s.m.dir
	var sessionEnv []string
	if tc != nil {
		if tc.WorkingDir != "" {
			workDir = tc.WorkingDir
		}
		sessionEnv = tc.Env
	}
	body, err := execPython(ctx, interp, spec.Code, params, workDir, s.m.dir, sessionEnv, timeoutOr(spec.TimeoutMs, 120000))
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(actool.Capture(tc, body)), nil
}

// scriptWaitDelay is how long after the interpreter ends (or is killed on the timeout) the run
// waits for its output pipe: a child it left behind that holds the pipe open must not keep the
// call, and the lease it holds, waiting for the idle sweep that can only run once the call ends.
const scriptWaitDelay = 3 * time.Second

// execPython runs one script tool. The code goes to a file in the server's script directory
// (<data>/spill/scripts: root 0711, the file 0644 under a random name, made exclusively), never
// into the workspace, and the interpreter starts through the drop chain in the workspace, so
// nothing the model wrote runs as root. The params JSON goes via stdin; scalar params are mirrored
// as TOOL_<NAME> variables, which travel in the root-only variables file and not in the
// environment or arguments of the root stage. Returns combined stdout+stderr (with a
// timeout/exit note). Standalone + testable.
func execPython(ctx context.Context, interp, code string, params map[string]any, workDir, dataDir string, sessionEnv []string, timeout time.Duration) (string, error) {
	vars := make(map[string]string)
	for k, v := range params { // scalar params mirrored as TOOL_<NAME>
		sv, ok := scalarStr(v)
		if !ok {
			continue
		}
		name := "TOOL_" + strings.ToUpper(k)
		if sandbox.ValidateVarName(name) != nil {
			continue // not a name an environment variable can have; stdin still carries the parameter
		}
		vars[name] = sv
	}
	if err := sandbox.ValidateVars(vars); err != nil {
		return "", fmt.Errorf("a parameter cannot be passed as a variable: %w", err)
	}
	pj, _ := json.Marshal(params)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	iso := sandbox.Current()
	lease, err := iso.Leases().Acquire(runCtx)
	if err != nil {
		return "", fmt.Errorf("tool processes are being restarted: %w", err)
	}
	defer lease.Release()

	script, remove, err := writeScriptFile(dataDir, code)
	if err != nil {
		return "", err
	}
	defer remove()

	c, cleanup, err := iso.Command(runCtx, lease, sandbox.Spec{Dir: workDir, Vars: vars, Program: interp, Args: []string{script}})
	if err != nil {
		return "", err
	}
	defer cleanup()
	if iso.On() {
		// The root stage reduces this to its allow-list (the session's proxy variables pass).
		c.Env = append(os.Environ(), sessionEnv...)
	} else {
		c.Env = append(append(os.Environ(), sessionEnv...), varEnv(vars)...)
	}
	c.Stdin = bytes.NewReader(pj) // parameter JSON goes via stdin
	c.WaitDelay = scriptWaitDelay
	out, err := c.CombinedOutput()
	body := string(out)
	if runCtx.Err() == context.DeadlineExceeded {
		body += "\n... [terminated on timeout] ..."
	} else if err != nil {
		body += "\n[exit: " + err.Error() + "]"
	}
	return body, nil
}

// varEnv renders variables as environment entries, in name order.
func varEnv(vars map[string]string) []string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+vars[k])
	}
	return env
}

// writeScriptFile writes code to <data>/spill/scripts/<random>.py and returns its path and a
// function that removes it. The directories are the server's and made without following a link;
// the file is created exclusively, so nothing planted at the name is written through.
func writeScriptFile(dataDir, code string) (string, func(), error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", nil, err
	}
	dir, err := sandbox.OpenServerDir(abs, 0o711, "spill", "scripts")
	if err != nil {
		return "", nil, fmt.Errorf("script directory: %w", err)
	}
	f, name, err := createUnique(dir, "s", ".py")
	if err != nil {
		dir.Close()
		return "", nil, fmt.Errorf("script file: %w", err)
	}
	remove := func() { _ = dir.Remove(name); dir.Close() }
	if _, err := f.WriteString(code); err != nil {
		f.Close()
		remove()
		return "", nil, fmt.Errorf("script file: %w", err)
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		remove()
		return "", nil, fmt.Errorf("script file: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("script file: %w", err)
	}
	return filepath.Join(abs, "spill", "scripts", name), remove, nil
}

// ---------- http: native request + proxy ----------

func (s *Server) runHTTPTool(ctx context.Context, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec httpExec
	_ = json.Unmarshal(execRaw, &spec)
	method := strings.ToUpper(strings.TrimSpace(spec.Method))
	if method == "" {
		method = "GET"
	}
	// Only names the classifier in templateOrigin can see are substituted.
	params = placeholderSafeParams(params)
	rawURL := renderTemplate(spec.URL, params, identity)
	if strings.TrimSpace(rawURL) == "" {
		return actool.Errorf("http url is empty"), nil
	}
	var bodyReader io.Reader
	if spec.Body != "" {
		bodyReader = strings.NewReader(renderTemplate(spec.Body, params, identity))
	}
	runCtx, cancel := context.WithTimeout(ctx, timeoutOr(spec.TimeoutMs, 30000))
	defer cancel()
	taskID := agent.RunInfoFrom(ctx).TaskID
	refuse := func(code, reason string, hop int) (actool.Result, error) {
		log.Printf("[customtool-http] decision=%s hop=%d task=%d", code, hop, taskID)
		return actool.Errorf(code + ": " + reason), nil
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return actool.Errorf("request failed: invalid url"), nil // url.Parse errors quote the URL
	}
	pin, templated := templateOrigin(spec.URL)
	if pin != nil && !sameOrigin(pin, target) {
		return refuse(codeOriginMismatch, "the rendered url leaves the origin fixed by the tool's url template", 0)
	}

	tr, proxyStr := s.httpProxyTransport(spec)
	var guard *agent.TargetGuard
	if tr != nil {
		guard = agent.NewTargetGuard(proxyStr, false)
	} else {
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment}
		guard = agent.NewTargetGuard("", true)
	}
	tr.DialContext = guard.DialContext
	// Direct dials are pinned to the checked IPs per request. Through any proxy only the
	// proxy address is dialled: the proxy resolves the name itself, so DNS rebinding and
	// IP-based allow rules are NOT enforced on the proxy leg (residual).
	tr.DisableKeepAlives = true

	reqCtx, err := guard.Check(runCtx, target)
	if err != nil {
		return refusalResult(err, refuse)
	}
	if templated && len(spec.Headers) > 0 && !guard.TaskAllowMatches(reqCtx, target) {
		return refuse(codeTemplatedOrigin, "a url template with a placeholder in its host may carry operator headers only to hosts matched by a task allow rule", 0)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, target.String(), bodyReader)
	if err != nil {
		return actool.Errorf("request failed: invalid request"), nil
	}
	// ARTEX User-Agent first (when the switch resolves one); a configured header of any case replaces it.
	if ua := useragent.For("Tools"); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, renderTemplate(v, params, identity))
	}
	client := &http.Client{Timeout: timeoutOr(spec.TimeoutMs, 30000), Transport: tr}
	// net/http copies the ORIGINAL request's headers onto every hop, so the strip runs on
	// every hop that is not the original origin, and for good once a hop has left it.
	left := false
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if err := guard.CheckRedirect(r, via); err != nil {
			return err
		}
		if !left && sameOrigin(via[0].URL, r.URL) {
			return nil
		}
		left = true
		if (r.Response != nil && (r.Response.StatusCode == http.StatusTemporaryRedirect || r.Response.StatusCode == http.StatusPermanentRedirect)) &&
			r.Body != nil && r.Body != http.NoBody {
			return &customHTTPRefusal{Code: codeRedirectBody, Reason: "a cross-origin 307/308 redirect would replay the request body", Hop: len(via)}
		}
		for k := range spec.Headers {
			r.Header.Del(k)
		}
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		var re *customHTTPRefusal
		if errors.As(err, &re) || isTargetErr(err) {
			return refusalResult(err, refuse)
		}
		return actool.Errorf("request failed: " + errClass(err)), nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := map[string]any{"status": resp.StatusCode, "body": string(respBody)}
	b, _ := json.Marshal(out)
	return actool.Text(actool.Capture(tc, string(b))), nil
}

// Refusal codes of the custom http tool; the TARGET_* codes come from agent.TargetGuard.
const (
	codeOriginMismatch  = "ORIGIN_MISMATCH"
	codeTemplatedOrigin = "TEMPLATED_ORIGIN_NOT_ALLOWED"
	codeRedirectBody    = "REDIRECT_BODY_REFUSED"
)

// customHTTPRefusal is a refusal made by the tool itself during a redirect.
type customHTTPRefusal struct {
	Code, Reason string
	Hop          int
}

func (e *customHTTPRefusal) Error() string { return e.Code + ": " + e.Reason }

func isTargetErr(err error) bool {
	var te *agent.TargetError
	return errors.As(err, &te)
}

// refusalResult turns a policy refusal into the tool's error result and its one
// content-free log line. Anything else (e.g. an unresolvable name) is a plain request error.
func refusalResult(err error, refuse func(code, reason string, hop int) (actool.Result, error)) (actool.Result, error) {
	var te *agent.TargetError
	if errors.As(err, &te) {
		return refuse(te.Code, te.Reason, te.Hop)
	}
	var re *customHTTPRefusal
	if errors.As(err, &re) {
		return refuse(re.Code, re.Reason, re.Hop)
	}
	return actool.Errorf("request failed: " + errClass(err)), nil
}

// errClass reduces a transport error to a fixed word. The raw text of a *url.Error holds
// the full URL (query keys) and host, which must not reach the model.
func errClass(err error) string {
	var dns *net.DNSError
	var certErr *tls.CertificateVerificationError
	var hdrErr tls.RecordHeaderError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case strings.Contains(err.Error(), "stopped after 10 redirects"):
		return "redirect: stopped after 10 redirects"
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &certErr), errors.As(err, &hdrErr), errors.As(err, &unknownCA), errors.As(err, &hostErr):
		return "tls"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return "connect"
	}
	return "other"
}

// placeholderSafeParams drops parameter names that placeholderRe cannot match, so the
// renderer and the template classifier agree on what a placeholder is.
func placeholderSafeParams(params map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for k, v := range params {
		if !strings.ContainsAny(k, "{}/?#") {
			out[k] = v
		}
	}
	return out
}

// originOf returns scheme, lower-cased hostname, port (default implied) and userinfo.
func originOf(u *url.URL) (scheme, host, port, user string) {
	scheme = strings.ToLower(u.Scheme)
	host = strings.ToLower(u.Hostname())
	port = u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme, host, port, u.User.String()
}

// sameOrigin compares parsed URLs on exact scheme + host + port (and userinfo).
func sameOrigin(a, b *url.URL) bool {
	as, ah, ap, au := originOf(a)
	bs, bh, bp, bu := originOf(b)
	return as == bs && ah == bh && ap == bp && au == bu
}

var placeholderRe = regexp.MustCompile(`\{[^{}/?#]*\}`)

// templateOrigin inspects the url template. pin is the origin fixed by the literal text
// before the first placeholder (nil when the authority is not fixed); templated reports
// that the authority itself contains a placeholder.
func templateOrigin(tmpl string) (pin *url.URL, templated bool) {
	// A brace the renderer will not substitute (e.g. "{a/b}") inside the authority
	// region can never be pinned: treat it as a templated authority.
	region := tmpl
	if i := strings.Index(tmpl, "://"); i >= 0 {
		region = tmpl[i+3:]
	}
	if j := strings.IndexAny(region, "/?#"); j >= 0 {
		region = region[:j]
	}
	if strings.Count(region, "{") != len(placeholderRe.FindAllStringIndex(region, -1)) || (!strings.Contains(tmpl, "://") && strings.Contains(tmpl, "{")) {
		return nil, true
	}
	loc := placeholderRe.FindStringIndex(tmpl)
	if loc == nil {
		return nil, false // no placeholder: nothing to pin or check
	}
	prefix := tmpl[:loc[0]]
	schemeEnd := strings.Index(prefix, "://")
	if schemeEnd < 0 {
		return nil, true // the scheme or the whole authority is templated
	}
	auth := prefix[schemeEnd+3:]
	if i := strings.IndexAny(auth, "/?#"); i >= 0 {
		auth = auth[:i]
		// the authority closed before the first placeholder: fully literal
	} else {
		// The placeholder touches the authority. It only extends the authority when
		// literal host text follows it before the path; a trailing placeholder is
		// treated as path-like and the authority is pinned.
		rest := tmpl[loc[1]:]
		for strings.HasPrefix(rest, "{") {
			if l := placeholderRe.FindStringIndex(rest); l != nil && l[0] == 0 {
				rest = rest[l[1]:]
			} else {
				break
			}
		}
		if rest != "" && !strings.ContainsAny(rest[:1], "/?#") {
			return nil, true
		}
		if auth == "" || strings.HasSuffix(auth, ".") || strings.HasSuffix(auth, ":") || strings.HasSuffix(auth, "@") || strings.HasSuffix(auth, "-") {
			return nil, true
		}
	}
	if auth == "" {
		return nil, true
	}
	pu, err := url.Parse(prefix[:schemeEnd+3] + auth)
	if err != nil || pu.Hostname() == "" {
		return nil, true
	}
	return pu, false
}

// httpProxyTransport builds a Transport for the http tool's proxy config, or nil
// (direct), plus the proxy URL it uses. use_recording_proxy routes through the
// recording proxy + trusts its CA.
func (s *Server) httpProxyTransport(spec httpExec) (*http.Transport, string) {
	proxyStr := strings.TrimSpace(spec.Proxy)
	var caFile string
	if spec.UseRecordingProxy {
		if addr := s.m.ProxyAddr(); addr != "" {
			proxyStr = "http://" + addr
			caFile = s.m.ProxyCACert()
		}
	}
	if proxyStr == "" {
		return nil, ""
	}
	pu, err := url.Parse(proxyStr)
	if err != nil {
		return nil, ""
	}
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	if caFile != "" {
		if pem, err := os.ReadFile(caFile); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
		}
	}
	return tr, proxyStr
}

// ---------- helpers ----------

func identity(s string) string { return s }

// shellQuote single-quotes a value for safe shell interpolation.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// renderTemplate replaces {name} placeholders with each param's rendered value.
func renderTemplate(tmpl string, params map[string]any, quote func(string) string) string {
	out := tmpl
	for k, v := range params {
		out = strings.ReplaceAll(out, "{"+k+"}", quote(valToStr(v)))
	}
	return out
}

// valToStr renders a param value: scalars as-is, arrays/objects as compact JSON.
func valToStr(v any) string {
	if sv, ok := scalarStr(v); ok {
		return sv
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// scalarStr returns (string, true) for scalar values, ("", false) for arrays/objects.
func scalarStr(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return fmt.Sprintf("%t", x), true
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x)), true
		}
		return fmt.Sprintf("%g", x), true
	case nil:
		return "", true
	default:
		return "", false
	}
}
