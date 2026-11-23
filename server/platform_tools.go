package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/intercept"
	"gopkg.in/yaml.v3"
)

// Platform operation tools (for the built-in Auto agent): create/edit skills,
// custom tools, MCP. All are host tools, seeded into the tools table, bound to auto by default, and injected via hostTools. They reuse the existing db/filesystem logic.

func (s *Server) platformTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolMCPStatus(),
		s.toolAskOperator(),
		s.toolCreateSkill(),
		s.toolUpdateSkillFile(),
		s.toolCreateCustomTool(),
		s.toolUpdateCustomTool(),
		s.toolCreateMCP(),
		s.toolUpdateMCP(),
		s.toolDeleteAssetsByHost(),
	}
}

// platformToolKeys are the tool keys the Auto agent gets bound by default.
var platformToolKeys = []string{
	"create_skill", "update_skill_file",
	"create_custom_tool", "update_custom_tool",
	"create_mcp", "update_mcp",
	"delete_assets_by_host",
}

// ---- human approval for the six tools that change skills, custom tools or MCP servers ----

// platformApprovalTimeoutSeconds is how long a pending approval waits for the operator.
// A timeout rejects the call. A variable so tests can shorten it.
var platformApprovalTimeoutSeconds = 1800

// platformApprovalMessage is the reason shown on the approval record.
const platformApprovalMessage = "This platform tool changes skills, custom tools or MCP servers. A human must approve exactly the content shown."

// requirePlatformApproval blocks until a human approves the call, and is the only gate
// for the six tools. It runs inside the tool, so it holds in every session type (worker,
// chat, planner, main agent) and does not depend on the interceptor's per-session hooks,
// its enabled-tool list, its rules or the AI judge: it calls HandleAsk directly with a
// code-built "ask" decision and never calls Match or Judge. record is the exact content
// to be written; it is stored whole as the approval's tool input (no truncation), so the
// operator sees what will happen. It returns a tool error result and false when the call
// was rejected, timed out, was cancelled, or could not be recorded: the caller then writes
// nothing. The result carries no call content.
func (s *Server) requirePlatformApproval(ctx context.Context, tool string, record map[string]any) (actool.Result, bool) {
	if s == nil || s.m == nil || s.m.interceptor == nil {
		return actool.Errorf("approval is unavailable: " + tool + " was not run and nothing was changed"), false
	}
	rendered, err := json.Marshal(record)
	if err != nil {
		return actool.Errorf("cannot render the approval record: " + tool + " was not run and nothing was changed"), false
	}
	dec := intercept.Decision{
		Action:         "ask",
		Message:        platformApprovalMessage,
		TimeoutEnabled: true,
		TimeoutSeconds: platformApprovalTimeoutSeconds,
		TimeoutAction:  "deny",
	}
	if !s.m.interceptor.HandleAsk(ctx, intercept.ConvIDFromContext(ctx), dec, tool, rendered) {
		return actool.Errorf(tool + " was not approved by the operator (rejected, timed out, cancelled or not recorded); nothing was changed"), false
	}
	return actool.Result{}, true
}

// ---- assets ----

// toolDeleteAssetsByHost hard-deletes every asset tied to one host (exact match).
// Platform-level (not a per-task tool): operates on the global, cross-task asset library.
func (s *Server) toolDeleteAssetsByHost() actool.CoreTool {
	return wrTool("delete_assets_by_host",
		"Delete assets by exact host: removes the host's domains/subdomains plus the services (service) and endpoints (endpoint) under them.\n"+
			"host must match exactly (lowercased, whitespace-trimmed); no fuzzy or wildcard matching.\n"+
			"Passing a root domain (e.g. example.com) also deletes its subdomains and their services/endpoints; passing a subdomain (e.g. a.example.com) or an IP deletes only that host itself and its services/endpoints.\n"+
			"⚠️ Hard delete, applies to the global asset library (shared across tasks), irreversible.",
		objSchema(map[string]any{
			"host": strParam("The host to delete: domain/subdomain/IP. Exact match, e.g. example.com, a.example.com, or 1.2.3.4"),
		}, "host"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			as := s.assetStore()
			if as == nil {
				return actool.Errorf("asset library not initialized"), nil
			}
			var a struct {
				Host string `json:"host"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Host) == "" {
				return actool.Errorf("host must not be empty"), nil
			}
			counts, err := as.DeleteByHost(a.Host)
			if err != nil {
				return actool.Errorf("delete failed: " + err.Error()), nil
			}
			var total int64
			for _, n := range counts {
				total += n
			}
			return jsonResult(map[string]any{
				"host":            a.Host,
				"deleted":         total,
				"deleted_by_type": counts,
			})
		})
}

// ---- skills ----

// reCreateSkillName is the strict pattern for names the model may create. It is narrower
// than validSkillName (which also admits non-ASCII letters for operator-made skills).
var reCreateSkillName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// skillFrontmatter is what create_skill writes. It is rendered by a YAML encoder, so
// no value can add a key or end the frontmatter.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// renderSkillMD returns the exact bytes of the new SKILL.md.
func renderSkillMD(name, description, instructions string) (string, error) {
	head, err := yaml.Marshal(skillFrontmatter{Name: name, Description: description})
	if err != nil {
		return "", fmt.Errorf("render frontmatter: %w", err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(head)
	b.WriteString("---\n")
	if strings.TrimSpace(instructions) != "" {
		b.WriteString(instructions)
	} else {
		fmt.Fprintf(&b, "## %s\n\n1. \n2. \n3. \n", name)
	}
	return b.String(), nil
}

func (s *Server) toolCreateSkill() actool.CoreTool {
	return wrTool("create_skill",
		"Create a new skill (writes SKILL.md, agentskills.io spec). name is lowercase letters/digits/hyphens. A human operator must approve the file before it is written.",
		objSchema(map[string]any{
			"name":         strParam("skill name (starts with a lowercase letter; letters/digits/hyphens)"),
			"description":  strParam("skill description (required; what it does / when to use it)"),
			"instructions": strParam("Markdown body instructions (optional)"),
		}, "name", "description"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, Description, Instructions string }
			_ = json.Unmarshal(in, &a)
			if !reCreateSkillName.MatchString(a.Name) || !validSkillName(a.Name) {
				return actool.Errorf("invalid skill name (must start with a lowercase letter, only lowercase letters/digits/hyphens, max 64)"), nil
			}
			if strings.TrimSpace(a.Description) == "" {
				return actool.Errorf("description is required"), nil
			}
			if !utf8.ValidString(a.Description) || !utf8.ValidString(a.Instructions) {
				return actool.Errorf("description and instructions must be valid UTF-8"), nil
			}
			md, err := renderSkillMD(a.Name, a.Description, a.Instructions)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := os.MkdirAll(s.skillDir, 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			root, err := os.OpenRoot(s.skillDir)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			defer root.Close()
			if _, err := root.Lstat(a.Name); err == nil {
				return actool.Errorf("skill already exists: " + a.Name), nil
			}
			if res, ok := s.requirePlatformApproval(ctx, "create_skill", map[string]any{
				"action":       "create a new skill",
				"skill":        a.Name,
				"path":         a.Name + "/SKILL.md",
				"file_content": md,
			}); !ok {
				return res, nil
			}
			// Mkdir fails when the name exists by now, so an existing skill is never clobbered.
			if err := root.Mkdir(a.Name, 0o755); err != nil {
				if errors.Is(err, fs.ErrExist) {
					return actool.Errorf("skill already exists: " + a.Name), nil
				}
				return actool.Errorf(err.Error()), nil
			}
			if err := writeSkillFile(root, a.Name, "SKILL.md", []byte(md), os.O_WRONLY|os.O_CREATE|os.O_EXCL); err != nil {
				_ = root.RemoveAll(a.Name)
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill created: " + a.Name), nil
		})
}

// writeSkillFile writes one file inside the skill directory name of root. Both opens stay
// inside root (os.Root refuses ".." and links that leave it).
func writeSkillFile(root *os.Root, name, rel string, content []byte, flags int) error {
	sub, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer sub.Close()
	f, err := sub.OpenFile(rel, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// checkSkillTarget refuses a skill directory or a path inside it that is, or passes
// through, a link or any other non-regular entry. It reports whether the file exists.
// clean is the output of skillRelPath. The caller holds sub, rooted at the skill directory.
func checkSkillTarget(sub *os.Root, clean string) (exists bool, err error) {
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for i := range parts {
		rel := strings.Join(parts[:i+1], "/")
		fi, err := sub.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return false, errors.New("path passes through a link")
		}
		last := i == len(parts)-1
		switch {
		case !last && !fi.IsDir():
			return false, errors.New("a parent of the path is not a directory")
		case last && !fi.Mode().IsRegular():
			return false, errors.New("target is not a regular file")
		}
	}
	return true, nil
}

// openSkillDir opens the skill directory name below the skills root without following a
// link at the skill's own name. The caller closes both roots.
func (s *Server) openSkillDir(name string) (root, sub *os.Root, err error) {
	root, err = os.OpenRoot(s.skillDir)
	if err != nil {
		return nil, nil, fmt.Errorf("skills directory unavailable: %w", err)
	}
	fi, err := root.Lstat(name)
	if err != nil {
		root.Close()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, errors.New("skill does not exist: " + name)
		}
		return nil, nil, err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		root.Close()
		return nil, nil, errors.New("skill is not a plain directory: " + name)
	}
	sub, err = root.OpenRoot(name)
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	return root, sub, nil
}

func (s *Server) toolUpdateSkillFile() actool.CoreTool {
	return wrTool("update_skill_file",
		"Write/overwrite one file inside a skill (SKILL.md by default). Used to modify skill content or add scripts/references. A human operator must approve the content before it is written.",
		objSchema(map[string]any{
			"name":    strParam("skill name"),
			"file":    strParam("relative path (optional, SKILL.md by default, e.g. scripts/run.py)"),
			"content": strParam("full file content"),
		}, "name", "content"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, File, Content string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("invalid skill name"), nil
			}
			rel := strings.TrimSpace(a.File)
			if rel == "" {
				rel = "SKILL.md"
			}
			clean, msg := skillRelPath(rel)
			if msg != "" {
				return actool.Errorf("illegal path: " + msg), nil
			}
			root, sub, err := s.openSkillDir(a.Name)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			exists, err := checkSkillTarget(sub, clean)
			sub.Close()
			root.Close()
			if err != nil {
				return actool.Errorf("illegal path: " + err.Error()), nil
			}
			if res, ok := s.requirePlatformApproval(ctx, "update_skill_file", map[string]any{
				"action":              "write a file inside a skill",
				"skill":               a.Name,
				"path":                a.Name + "/" + clean,
				"overwrites_existing": exists,
				"file_content":        a.Content,
			}); !ok {
				return res, nil
			}
			// The check is repeated on fresh handles: the tree may have changed while the
			// approval was pending.
			root, sub, err = s.openSkillDir(a.Name)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			defer root.Close()
			defer sub.Close()
			if _, err := checkSkillTarget(sub, clean); err != nil {
				return actool.Errorf("illegal path: " + err.Error()), nil
			}
			if dir := filepath.Dir(clean); dir != "." {
				if err := sub.MkdirAll(dir, 0o755); err != nil {
					return actool.Errorf(err.Error()), nil
				}
			}
			f, err := sub.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if _, err := f.WriteString(a.Content); err != nil {
				_ = f.Close()
				return actool.Errorf(err.Error()), nil
			}
			if err := f.Close(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill file written: " + a.Name + "/" + clean), nil
		})
}

// ---- custom tools ----

type customToolToolInput struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Exec        json.RawMessage `json:"exec"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Deferred    bool            `json:"deferred"`
	Enabled     *bool           `json:"enabled"`
}

func customToolSchema(keyDesc string) map[string]any {
	return objSchema(map[string]any{
		"key":         strParam(keyDesc),
		"description": strParam("description shown to the model"),
		"kind":        strParam("shell | command | script (Python only) | http. shell = a bash environment declaration (merely tells the model this tool can be called directly in bash, no exec/schema needed); the other three require exec"),
		"exec":        map[string]any{"type": "object", "description": "execution spec (not needed for the shell kind): command→{command}; script→{code}; http→{method,url,headers,body,proxy,use_recording_proxy}"},
		"schema":      map[string]any{"type": "object", "description": "parameter JSON-Schema (optional for shell/command/script; required for http and must contain properties)"},
		"agents":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "agent keys to bind (optional)"},
		"deferred":    map[string]any{"type": "boolean", "description": "whether to defer (no effect for the shell kind; enable only for rarely used command/script/http tools)"},
		"enabled":     map[string]any{"type": "boolean", "description": "whether enabled (default true)"},
	}, "key", "kind")
}

func toDBTool(a customToolToolInput) *db.Tool {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	return &db.Tool{
		Key: a.Key, Description: a.Description, Schema: a.Schema, Agents: a.Agents,
		Enabled: enabled, Kind: a.Kind, Exec: a.Exec, Deferred: a.Deferred,
	}
}

// customToolRecord is the resulting tools row, in full: the command template or script
// included. The approval shows exactly this and the tool writes exactly this.
func customToolRecord(action string, t *db.Tool) map[string]any {
	return map[string]any{
		"action":      action,
		"key":         t.Key,
		"kind":        t.Kind,
		"description": t.Description,
		"exec":        t.Exec,
		"schema":      t.Schema,
		"agents":      t.Agents,
		"deferred":    t.Deferred,
		"enabled":     t.Enabled,
	}
}

func (s *Server) toolCreateCustomTool() actool.CoreTool {
	return wrTool("create_custom_tool", "[Important] When installing tools the platform does not have, call this tool to put the installed tool into the platform so the platform can call it! Creates a custom tool (shell/command/script/http). shell = a bash environment declaration, needing only key+description+agents, no exec/schema. A human operator must approve the tool before it is created.",
		customToolSchema("tool key (starts with a lowercase letter; letters/digits/underscores)"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			a.Key = strings.TrimSpace(a.Key)
			if !reToolKey.MatchString(a.Key) {
				return actool.Errorf("key must start with a lowercase letter and contain only lowercase letters/digits/underscores"), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind must be shell / command / script / http"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http tools must provide a parameter JSON Schema (cannot be empty)"), nil
			}
			if s.m == nil || s.m.pg == nil {
				return actool.Errorf("database unavailable"), nil
			}
			if exist, _ := s.m.pg.GetTool(a.Key); exist != nil {
				return actool.Errorf("this key already exists: " + a.Key), nil
			}
			t := toDBTool(a)
			if res, ok := s.requirePlatformApproval(ctx, "create_custom_tool", customToolRecord("create a custom tool", t)); !ok {
				return res, nil
			}
			if err := s.m.pg.CreateCustomTool(t); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool created: " + a.Key), nil
		})
}

func (s *Server) toolUpdateCustomTool() actool.CoreTool {
	return wrTool("update_custom_tool", "Update an existing custom tool (by key). Every field is overwritten by the values given. A human operator must approve the resulting tool before it is changed.",
		customToolSchema("the key of the custom tool to modify"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			if s.m == nil || s.m.pg == nil {
				return actool.Errorf("database unavailable"), nil
			}
			existing, _ := s.m.pg.GetTool(a.Key)
			if existing == nil || existing.System {
				return actool.Errorf("only custom tools can be modified: " + a.Key), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind must be shell / command / script / http"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http tools must provide a parameter JSON Schema (cannot be empty)"), nil
			}
			t := toDBTool(a)
			if res, ok := s.requirePlatformApproval(ctx, "update_custom_tool", customToolRecord("overwrite an existing custom tool", t)); !ok {
				return res, nil
			}
			if err := s.m.pg.UpdateCustomTool(t); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool updated: " + a.Key), nil
		})
}

// ---- MCP health ----

// toolMCPStatus reports each MCP server's last connect outcome so a worker can
// notice a missing server (and say so) instead of silently degrading when the
// catalog is cached but the server is unreachable.
func (s *Server) toolMCPStatus() actool.CoreTool {
	return wrTool("mcp_status",
		"Report the health of every configured MCP server: name, transport, enabled, cached tool count, and the outcome of the last connect attempt (time, ok/error). Use this when tools you expect are missing or before relying on a skill-gated server.",
		map[string]any{"type": "object", "properties": map[string]any{}},
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			servers, err := s.m.pg.ListMCP()
			if err != nil {
				return actool.Errorf("Error: " + err.Error()), nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%d MCP server(s):\n", len(servers))
			for _, m := range servers {
				status := "never connected"
				switch {
				case m.LastConnectOK != nil && *m.LastConnectOK:
					status = "connected ok"
				case m.LastConnectOK != nil && !*m.LastConnectOK:
					status = "DOWN: " + strings.TrimSpace(coalesceStr(m.LastConnectErr, "connect failed"))
				}
				fmt.Fprintf(&b, "- %s [%s, %s, %d cached tools]: %s", m.Name, m.Transport, enabledWord(m.Enabled), len(m.Tools), status)
				if m.LastConnectAt != nil {
					fmt.Fprintf(&b, " (last attempt %s)", m.LastConnectAt.UTC().Format(time.RFC3339))
				}
				b.WriteByte('\n')
			}
			b.WriteString("If a server you need is DOWN, tell the operator in your final report instead of working around it.")
			return actool.Text(b.String()), nil
		})
}

func coalesceStr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func enabledWord(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// ---- MCP ----

type mcpToolInput struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Command   string          `json:"command"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	URL       string          `json:"url"`
	Enabled   *bool           `json:"enabled"`
	Insecure  *bool           `json:"insecure"`
}

func mcpSchema(withID bool) map[string]any {
	props := map[string]any{
		"name":      strParam("MCP server name"),
		"transport": strParam("stdio | http / sse"),
		"command":   strParam("launch command for stdio (e.g. npx)"),
		"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "array of command arguments"},
		"env":       map[string]any{"type": "object", "description": "environment variables {KEY:VALUE}"},
		"url":       strParam("URL for http/sse"),
		"enabled":   map[string]any{"type": "boolean", "description": "whether enabled (default true)"},
		"insecure":  map[string]any{"type": "boolean", "description": "http: skip TLS certificate verification (set true for self-signed certificates, default false)"},
	}
	required := []string{"name", "transport"}
	if withID {
		props["id"] = map[string]any{"type": "integer", "description": "id of the MCP server to modify"}
		required = []string{"id", "name", "transport"}
	}
	return objSchema(props, required...)
}

func (a mcpToolInput) toDB() *db.MCPServer {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	insecure := false
	if a.Insecure != nil {
		insecure = *a.Insecure
	}
	return &db.MCPServer{
		ID: a.ID, Name: a.Name, Transport: a.Transport, Command: a.Command,
		Args: a.Args, Env: a.Env, URL: a.URL, Enabled: enabled, Insecure: insecure,
	}
}

// maskedValue replaces the value of a secret-looking environment key in the approval record.
const maskedValue = "(value masked)"

// secretKeyWords are the words (a key split on _ - . and case changes, compared case-insensitively)
// that mark a key as secret. A bare substring match would hide SSH_ASKPASS, SESSION_TIMEOUT or
// KEYBOARD_LAYOUT, which carry nothing secret, so only whole words count.
var secretKeyWords = map[string]bool{
	"TOKEN": true, "SECRET": true, "PASSWORD": true, "PASSWD": true, "PWD": true, "APIKEY": true,
	"CREDENTIAL": true, "CREDENTIALS": true, "COOKIE": true, "BEARER": true, "DSN": true, "PRIVATE": true,
	"AUTHORIZATION": true,
}

// secretKeyPrefixes are the words that make a following KEY word secret (API_KEY, SECRET_KEY, ...).
var secretKeyPrefixes = map[string]bool{
	"API": true, "ACCESS": true, "SECRET": true, "PRIVATE": true, "SIGNING": true, "ENCRYPTION": true,
	"CLIENT": true, "AUTH": true,
}

// secretQueryParts are the substrings (case-insensitive) that mark a URL query parameter as secret.
var secretQueryParts = []string{"token", "key", "secret", "sig", "signature", "password", "auth", "code"}

// keyWords splits a key into upper-case words at _, - and . and at a lower-to-upper case change
// (apiKey -> API, KEY).
func keyWords(key string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToUpper(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range key {
		switch {
		case r == '_' || r == '-' || r == '.':
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return words
}

// secretKey reports whether the value of an environment key must not be shown.
func secretKey(key string) bool {
	words := keyWords(key)
	for i, w := range words {
		if secretKeyWords[w] {
			return true
		}
		if w == "KEY" && (i == len(words)-1 || (i > 0 && secretKeyPrefixes[words[i-1]])) {
			return true
		}
	}
	return false
}

// maskedQuery replaces the value of every secret-looking parameter of a raw query with
// "(masked)", keeping the order and the other parameters as they were.
func maskedQuery(raw string) string {
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		name, _, found := strings.Cut(p, "=")
		if !found {
			continue
		}
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		low := strings.ToLower(decoded)
		for _, s := range secretQueryParts {
			if strings.Contains(low, s) {
				parts[i] = name + "=(masked)"
				break
			}
		}
	}
	return strings.Join(parts, "&")
}

// redactedURL is u for the approval record: the password of the userinfo and the values of
// secret-looking query parameters are masked.
func redactedURL(u *url.URL) string {
	c := *u
	c.RawQuery = maskedQuery(c.RawQuery)
	return c.Redacted() // writes the "(masked)" values verbatim
}

// shownValue is the text the approval record shows for one environment value: masked when the
// key is a secret one, with the password of a URL userinfo and the secret query parameters
// masked, and in full otherwise.
func shownValue(key string, raw json.RawMessage) string {
	if secretKey(key) {
		return maskedValue
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		v = strings.TrimSpace(string(raw)) // a number, boolean or other JSON value
	}
	if u, err := url.Parse(v); err == nil && u.Scheme != "" && (u.User != nil || u.RawQuery != "") {
		if u.User != nil {
			if _, has := u.User.Password(); !has {
				return maskedValue // a bare user part is often the token itself
			}
		}
		return redactedURL(u)
	}
	return v
}

// prepare validates the parts of the input that become columns and normalises them, so
// the row shown to the operator is byte for byte the row that is saved.
func (a mcpToolInput) prepare() (*db.MCPServer, error) {
	m := a.toDB()
	m.TimeoutSeconds = 120 // what SaveMCP stores when the tool gives none
	if t := strings.TrimSpace(string(m.Args)); t == "" || t == "null" {
		m.Args = json.RawMessage("[]")
	}
	if t := strings.TrimSpace(string(m.Env)); t == "" || t == "null" {
		m.Env = json.RawMessage("{}")
	}
	var args []string
	if err := json.Unmarshal(m.Args, &args); err != nil {
		return nil, errors.New("args must be an array of strings")
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(m.Env, &env); err != nil {
		return nil, errors.New("env must be an object")
	}
	if m.URL != "" {
		if _, err := url.Parse(m.URL); err != nil {
			return nil, errors.New("url is not valid")
		}
	}
	// A stdio row's keys become variables of a process, so the names the drop chain refuses are
	// refused here, before a human is asked.
	if err := checkMCPEnvKeys(m.Name, m.Transport, m.Env); err != nil {
		return nil, err
	}
	return m, nil
}

// mcpRecord is the resulting mcp_servers row: command and args in full, environment keys
// with values (secret-looking ones masked), and a URL without a password. It carries every column the tool
// writes, so an omitted field that resets one shows as the reset value.
func mcpRecord(action string, m *db.MCPServer) map[string]any {
	var args []string
	_ = json.Unmarshal(m.Args, &args) // validated by prepare
	var env map[string]json.RawMessage
	_ = json.Unmarshal(m.Env, &env)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	shown := make(map[string]string, len(env))
	for _, k := range keys {
		shown[k] = shownValue(k, env[k])
	}
	shownURL := ""
	if u, err := url.Parse(m.URL); err == nil {
		shownURL = redactedURL(u)
	}
	rec := map[string]any{
		"action":          action,
		"name":            m.Name,
		"transport":       m.Transport,
		"command":         m.Command,
		"args":            args,
		"env_keys":        keys,
		"env":             shown,
		"url":             shownURL,
		"enabled":         m.Enabled,
		"insecure":        m.Insecure,
		"timeout_seconds": m.TimeoutSeconds,
	}
	if m.ID != 0 {
		rec["id"] = m.ID
	}
	return rec
}

func (s *Server) toolCreateMCP() actool.CoreTool {
	return wrTool("create_mcp", "Create an MCP server (stdio/http/sse). Its tools must be authorized per agent visibility after creation. A human operator must approve the server before it exists; a stdio server runs its command as soon as it is approved and enabled.",
		mcpSchema(false),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			a.ID = 0
			if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Transport) == "" {
				return actool.Errorf("name / transport are required"), nil
			}
			m, err := a.prepare()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if s.m == nil || s.m.pg == nil {
				return actool.Errorf("database unavailable"), nil
			}
			// No row exists, so nothing can spawn the server, until a human approves.
			if res, ok := s.requirePlatformApproval(ctx, "create_mcp", mcpRecord("create an MCP server", m)); !ok {
				return res, nil
			}
			id, err := s.m.pg.SaveMCP(m)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp created: id=%d name=%s", id, a.Name)), nil
		})
}

func (s *Server) toolUpdateMCP() actool.CoreTool {
	return wrTool("update_mcp", "Update an existing MCP server (by id). Every column is overwritten: an omitted args or env becomes empty. A human operator must approve the resulting row before it is changed.",
		mcpSchema(true),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			if a.ID == 0 {
				return actool.Errorf("id is required"), nil
			}
			m, err := a.prepare()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if s.m == nil || s.m.pg == nil {
				return actool.Errorf("database unavailable"), nil
			}
			servers, err := s.m.pg.ListMCP()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			found := false
			for _, e := range servers {
				if e.ID == a.ID {
					found = true
					break
				}
			}
			if !found {
				return actool.Errorf(fmt.Sprintf("mcp server %d does not exist", a.ID)), nil
			}
			if res, ok := s.requirePlatformApproval(ctx, "update_mcp", mcpRecord("overwrite an existing MCP server", m)); !ok {
				return res, nil
			}
			if _, err := s.m.pg.SaveMCP(m); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp updated: id=%d", a.ID)), nil
		})
}
