package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillsAreLoadedOnlyFromASnapshot guards the four call sites: skill.LoadDir follows links
// as root, so server code must go through sandbox.LoadSkills.
func TestSkillsAreLoadedOnlyFromASnapshot(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	calls, configured := 0, false
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "skill.LoadDir(") {
			t.Errorf("%s calls skill.LoadDir; use sandbox.LoadSkills", f)
		}
		if f == "server.go" && strings.Contains(string(b), "sandbox.ConfigureSkillSnapshot(") {
			configured = true
		}
		calls += strings.Count(string(b), "sandbox.LoadSkills(")
	}
	if !configured {
		t.Error("server.go must call sandbox.ConfigureSkillSnapshot at start")
	}
	if calls != 4 {
		t.Errorf("sandbox.LoadSkills call sites = %d, want 4", calls)
	}
}
