package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db"
)

// TestSkillTools exercises create_skill and update_skill_file end to end through the
// approval path (the tools now need a human decision; see platform_approval_test.go).
func TestSkillTools(t *testing.T) {
	e := newPlatEnv(t)
	dir := e.dir

	// create_skill writes a spec-compliant SKILL.md.
	in := map[string]any{"name": "my-skill", "description": "does X", "instructions": "## do\n1. a"}
	if res := e.run(e.s.toolCreateSkill(), "create_skill", in, true); res.IsError {
		t.Fatalf("create_skill: %s", res.Flatten())
	}
	md, err := os.ReadFile(filepath.Join(dir, "my-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("SKILL.md not written: %v", err)
	}
	if !strings.Contains(string(md), "name: my-skill") || !strings.Contains(string(md), "description: does X") || !strings.Contains(string(md), "1. a") {
		t.Fatalf("SKILL.md content:\n%s", md)
	}

	// update_skill_file writes a nested script.
	up := map[string]any{"name": "my-skill", "file": "scripts/run.py", "content": "print(1)"}
	if res := e.run(e.s.toolUpdateSkillFile(), "update_skill_file", up, true); res.IsError {
		t.Fatalf("update_skill_file: %s", res.Flatten())
	}
	got, err := os.ReadFile(filepath.Join(dir, "my-skill", "scripts", "run.py"))
	if err != nil || string(got) != "print(1)" {
		t.Fatalf("nested file not written: %v %q", err, got)
	}
}

func TestShownValue(t *testing.T) {
	cases := []struct {
		key, val, want string
	}{
		// secret words, split on _ - . and compared case-insensitively
		{"API_TOKEN", `"v"`, maskedValue}, {"db_password", `"v"`, maskedValue}, {"PASSWD", `"v"`, maskedValue},
		{"DB_PWD", `"v"`, maskedValue}, {"AWS_SECRET_ACCESS", `"v"`, maskedValue}, {"CLIENT_SECRET", `"v"`, maskedValue},
		{"SSH_PRIVATE", `"v"`, maskedValue}, {"Cookie", `"v"`, maskedValue}, {"SENTRY_DSN", `"v"`, maskedValue},
		{"apikey", `"v"`, maskedValue}, {"CREDENTIAL", `"v"`, maskedValue}, {"CREDENTIALS_FILE", `"v"`, maskedValue},
		{"BEARER", `"v"`, maskedValue}, {"Authorization", `"v"`, maskedValue}, {"x-api-token", `"v"`, maskedValue},
		{"db.password", `"v"`, maskedValue}, {"GITHUB-TOKEN", `"v"`, maskedValue},
		// KEY after API, ACCESS, SECRET, PRIVATE, SIGNING, ENCRYPTION, CLIENT or AUTH, or at the end
		{"API_KEY", `1`, maskedValue}, {"Api-Key", `"v"`, maskedValue}, {"AWS_ACCESS_KEY", `"v"`, maskedValue},
		{"AWS_ACCESS_KEY_ID", `"v"`, maskedValue}, {"KEY", `"v"`, maskedValue}, {"KEY_FILE", `"v"`, "v"}, {"SECRET_KEY", `"v"`, maskedValue}, {"PRIVATE_KEY", `"v"`, maskedValue},
		{"SIGNING_KEY", `"v"`, maskedValue}, {"ENCRYPTION_KEY", `"v"`, maskedValue}, {"CLIENT_KEY", `"v"`, maskedValue},
		{"AUTH_KEY", `"v"`, maskedValue}, {"AUTH.KEY", `"v"`, maskedValue}, {"DEPLOY_KEY", `"v"`, maskedValue},
		{"SSH_KEY", `"v"`, maskedValue}, {"apiKey", `"v"`, maskedValue}, {"sessionToken", `"v"`, maskedValue},
		// shown: the old substring list hid these
		{"SSH_ASKPASS", `"/usr/bin/ssh-askpass"`, "/usr/bin/ssh-askpass"},
		{"NODE_PATH", `"/usr/lib/node_modules"`, "/usr/lib/node_modules"},
		{"JAVA_TOOL_OPTIONS", `"-Xmx512m"`, "-Xmx512m"},
		{"SESSION_TIMEOUT", `"30"`, "30"}, {"SESSION_ID", `"abc"`, "abc"},
		{"KEYBOARD_LAYOUT", `"us"`, "us"}, {"MONKEY_COUNT", `3`, "3"},
		{"AUTH_URL", `"https://auth.example.com/login"`, "https://auth.example.com/login"},
		{"HTTP_PROXY", `"http://x:1"`, "http://x:1"}, {"LOG_LEVEL", `"debug"`, "debug"}, {"PORT", `8080`, "8080"},
		// URL values: the userinfo password and the secret-looking query parameters
		{"DB_URL", `"postgres://bob:hunter2@db:5432/x"`, "postgres://bob:xxxxx@db:5432/x"},
		{"REPO", `"https://tok123@host/x"`, maskedValue},
		{"AUTH_URL", `"https://auth.example.com/cb?code=hunter2&state=ok"`, "https://auth.example.com/cb?code=(masked)&state=ok"},
		{"CALLBACK", `"https://h/p?access_token=hunter2&x=1&Signature=hunter2&API_KEY=hunter2&password=hunter2&client_secret=hunter2&auth=hunter2&sig=hunter2"`,
			"https://h/p?access_token=(masked)&x=1&Signature=(masked)&API_KEY=(masked)&password=(masked)&client_secret=(masked)&auth=(masked)&sig=(masked)"},
		{"FEED", `"https://h/p?page=2&q=a%20b"`, "https://h/p?page=2&q=a%20b"},
		{"SSE_URL", `"https://u:hunter2@h/p?token=hunter2"`, "https://u:xxxxx@h/p?token=(masked)"},
	}
	for _, c := range cases {
		got := shownValue(c.key, json.RawMessage(c.val))
		if got != c.want {
			t.Errorf("%s=%s: got %q want %q", c.key, c.val, got, c.want)
		}
		if strings.Contains(got, "hunter2") || strings.Contains(got, "tok123") {
			t.Errorf("%s leaks a secret: %q", c.key, got)
		}
	}
}

// The URL column of the approval record gets the same treatment as a URL-valued variable.
func TestMCPRecordMasksTheURLQuerySecrets(t *testing.T) {
	rec := mcpRecord("create an MCP server", &db.MCPServer{Name: "n", Transport: "http", URL: "https://u:hunter2@h/mcp?api_key=hunter2&mode=x"})
	if got := rec["url"]; got != "https://u:xxxxx@h/mcp?api_key=(masked)&mode=x" {
		t.Fatalf("url=%v", got)
	}
}

func TestMCPApprovalRecordShowsValuesNotSecrets(t *testing.T) {
	e := newPlatEnv(t)
	const canary = "CANARY-s3cr3t-9f2c"
	env := map[string]any{"API_TOKEN": canary, "LOG_LEVEL": "debug", "DB_URL": "postgres://u:" + canary + "@h/x"}
	check := func(raw string) {
		t.Helper()
		if strings.Contains(raw, canary) {
			t.Fatal("a secret value is in the approval record")
		}
		for _, want := range []string{`"LOG_LEVEL": "debug"`, maskedValue, "postgres://u:xxxxx@h/x"} {
			if !strings.Contains(raw, want) {
				t.Fatalf("the approval record lacks %q: %s", want, raw)
			}
		}
	}
	name := uniq("isomask")
	call := e.start(context.Background(), e.s.toolCreateMCP(), "create_mcp", map[string]any{
		"name": name, "transport": "stdio", "command": "x", "env": env})
	check(string(call.row.ToolInput))
	call.decide(false)

	id, err := e.m.pg.SaveMCP(&db.MCPServer{Name: uniq("isomaskupd"), Transport: "stdio", Command: "old"})
	if err != nil {
		t.Fatal(err)
	}
	call = e.start(context.Background(), e.s.toolUpdateMCP(), "update_mcp", map[string]any{
		"id": id, "name": uniq("isomaskupd"), "transport": "stdio", "command": "new", "env": env})
	check(string(call.row.ToolInput))
	call.decide(false)
}
