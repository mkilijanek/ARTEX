//go:build unix

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/sandbox"
)

// hasSkillTool reports whether the packed tool set contains the Skill meta-tool.
func hasSkillTool(tools []actool.CoreTool) bool {
	for _, t := range tools {
		if t.Name() == "Skill" {
			return true
		}
	}
	return false
}

// TestAssembleVisibleSkill verifies a filesystem skill made visible to an agent is
// packed into that agent's tool set (as the single Skill meta-tool) via the wired
// ToolAugment hook. Skills live on disk under skillDir; visibility is a per-agent
// (agent × skill_name) row keyed by the skill's directory name.
func TestAssembleVisibleSkill(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer pg.Close()

	// Filesystem skill fixture: <skillDir>/t-assemble/SKILL.md. The directory name
	// (t-assemble) is the visibility key matched against AgentSkillNames.
	skillDir := t.TempDir()
	const skillName = "t-assemble"
	if err := os.MkdirAll(filepath.Join(skillDir, skillName), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: t-assemble\ndescription: do the thing\n---\ndo the thing"
	if err := os.WriteFile(filepath.Join(skillDir, skillName, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	wireAgentAugment(pg, skillDir, nil)
	t.Cleanup(func() { agent.SetToolAugment(nil) })

	ag, _ := pg.GetAgentByKey("planner")
	if ag == nil {
		t.Fatal("planner agent missing")
	}

	// Isolate this test from whatever skill visibility the planner already has in the
	// shared DB: clear it now, restore it on cleanup.
	orig, _ := pg.AgentSkillNames(ag.ID)
	if err := pg.SetAgentSkillVisibility(ag.ID, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.SetAgentSkillVisibility(ag.ID, orig) })

	// before: skill not visible → no Skill meta-tool
	extra, _, cleanup := agent.ToolAugment()(context.Background(), "planner")
	cleanup()
	if hasSkillTool(extra) {
		t.Fatalf("expected no Skill meta-tool before the skill is made visible")
	}

	// make the skill visible to planner
	if err := pg.ToggleSkillVisibility(ag.ID, skillName, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.ToggleSkillVisibility(ag.ID, skillName, false) })

	// after: the Skill meta-tool is packed in
	extra, _, cleanup = agent.ToolAugment()(context.Background(), "planner")
	defer cleanup()
	if !hasSkillTool(extra) {
		t.Fatalf("expected the Skill meta-tool after making the skill visible")
	}

	// AugmentTools appends extra to base without filtering base
	combined, _, c2 := agent.AugmentTools(context.Background(), "planner", nil)
	defer c2()
	if !hasSkillTool(combined) {
		t.Fatalf("AugmentTools should surface the Skill meta-tool")
	}
}

// fakeMCP is a connected MCP server that offers one tool named after the server.
type fakeMCP struct{ name string }

func (f fakeMCP) Tools(context.Context) ([]actool.CoreTool, error) {
	return []actool.CoreTool{wrTool("fakemcp_"+f.name, "a fake tool", map[string]any{"type": "object"},
		func(context.Context, json.RawMessage) (actool.Result, error) { return actool.Text("ok"), nil })}, nil
}
func (fakeMCP) Close() error { return nil }

func fakeMCPNames(tools []actool.CoreTool) []string {
	var names []string
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return fakeNames(names)
}

func fakeNames(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, "fakemcp_") {
			out = append(out, n)
		}
	}
	return out
}

// When the skills cannot be loaded for an agent that has skills, the MCP servers whose
// visibility depends on a skill cannot be told apart: none may become a global
// (start-of-session) tool, and the failure is reported in the isolation status.
func TestSkillLoadFailureLeavesNoMCPToolGlobal(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer pg.Close()

	iso, _ := activateEnforcedTree(t)
	oldConnect := assemblyConnectMCP
	assemblyConnectMCP = func(_ context.Context, m *db.MCPServer) (mcpClient, error) { return fakeMCP{name: m.Name}, nil }
	t.Cleanup(func() { assemblyConnectMCP = oldConnect })

	ag, _ := pg.GetAgentByKey("planner")
	if ag == nil {
		t.Fatal("planner agent missing")
	}
	origSkills, _ := pg.AgentSkillNames(ag.ID)
	origMCP, _ := pg.AgentVisible(ag.ID, "mcp")
	t.Cleanup(func() {
		_ = pg.SetAgentSkillVisibility(ag.ID, origSkills)
		_ = pg.SetAgentVisibilityKind(ag.ID, "mcp", origMCP)
	})
	id, err := pg.SaveMCP(&db.MCPServer{Name: uniq("failclosed"), Transport: "http", URL: "http://192.0.2.1/mcp", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteMCP(id) })
	if err := pg.SetAgentVisibilityKind(ag.ID, "mcp", []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetAgentSkillVisibility(ag.ID, []string{"some-skill"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.SetToolAugment(nil) })

	// A working loader: the directly visible server is global.
	goodDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(goodDir, "some-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: some-skill\ndescription: x\n---\nx"
	if err := os.WriteFile(filepath.Join(goodDir, "some-skill", "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	oldRoot := sandbox.SkillSnapshotRoot
	sandbox.SkillSnapshotRoot = t.TempDir()
	t.Cleanup(func() { sandbox.SkillSnapshotRoot = oldRoot })
	skillsFailureReported.Store(false)
	t.Cleanup(func() { skillsFailureReported.Store(false) })
	wireAgentAugment(pg, goodDir, nil)
	extra, def, cleanup := agent.ToolAugment()(context.Background(), "planner")
	cleanup()
	if len(fakeMCPNames(extra)) == 0 || len(fakeNames(def.GlobalNames)) == 0 {
		t.Fatalf("control: with a working loader the visible MCP tool must be global; extra=%v global=%v", fakeMCPNames(extra), def.GlobalNames)
	}

	// A failing loader: no MCP tool at all, none global.
	wireAgentAugment(pg, filepath.Join(t.TempDir(), "missing"), nil)
	extra, def, cleanup = agent.ToolAugment()(context.Background(), "planner")
	cleanup()
	if n := fakeMCPNames(extra); len(n) != 0 {
		t.Fatalf("MCP tools offered although the skills failed to load: %v", n)
	}
	if n := fakeNames(def.GlobalNames); len(n) != 0 {
		t.Fatalf("MCP tools in the global set although the skills failed to load: %v", n)
	}
	for _, n := range def.Deferred {
		if strings.HasPrefix(n, "fakemcp_") {
			t.Fatalf("MCP tool %s is deferred-available although the skills failed to load", n)
		}
	}
	if v := iso.View(); !strings.Contains(v.Reason, "S13") {
		t.Fatalf("no S13 status entry after the failure: %+v", v)
	}
}

// A skill snapshot directory that cannot be set up raises the status warning, not only a log line.
func TestSkillSnapshotSetupFailureRaisesTheStatusWarning(t *testing.T) {
	iso, _ := activateEnforcedTree(t)
	reportSkillSnapshot(nil)
	if v := iso.View(); strings.Contains(v.Reason, "S13") {
		t.Fatalf("a successful set-up raised the warning: %+v", v)
	}
	reportSkillSnapshot(errors.New("skill snapshot directory /x is not writable"))
	if v := iso.View(); !strings.Contains(v.Reason, "S13: skills will not load") || !strings.Contains(v.Reason, "not writable") {
		t.Fatalf("view after the failure: %+v", v)
	}
}
