package agent

import (
	"strings"
	"testing"
)

func TestCachePrefixPreservesPromptAndStableRolePrefix(t *testing.T) {
	t.Setenv("ARTEX_CACHE_PREFIX", "1")
	for _, role := range []string{"worker", "planner", "mainagent", "chat"} {
		one := []string{role + " rules\n\nNow: 2026-11-15 00:00:01 UTC\n\nartifact rules", "deferred"}
		two := []string{role + " rules\n\nNow: 2026-11-15 00:00:02 UTC\n\nartifact rules", "deferred"}
		a, bound := cachePrefix(one, 2)
		b, _ := cachePrefix(two, 2)
		if bound != 3 || a[0] != b[0] || strings.Join(a, "\n\n") != strings.Join(one, "\n\n") {
			t.Fatalf("%s: changed prompt or unstable prefix", role)
		}
		t.Logf("%s: full bytes=%d stable prefix=%d", role, len(strings.Join(a, "\n\n")), len(a[0]))
	}
	s := []string{"Now: 2026-11-15 00:00:01 UTC without a prior paragraph"}
	out, b := cachePrefix(s, 0)
	if b != 0 || len(out) != 1 || out[0] != s[0] {
		t.Fatal("unsafe template split")
	}
}

func TestRenderedAgentPromptByteInventory(t *testing.T) {
	t.Setenv("ARTEX_CACHE_PREFIX", "1")
	prompts := map[string]string{
		"worker":    workerSystem("", "", "/data", "/data/tasks/1/i1"),
		"planner":   plannerSystem("test goal", "/data", "/data/tasks/1"),
		"mainagent": mainAgentSystem("test goal", "/data", "/data/tasks/1"),
		"chat":      chatSystem("assistant", "/data", "/data/sessions/test"),
	}
	for role, prompt := range prompts {
		parts, b := cachePrefix([]string{prompt}, 0)
		if role == "planner" && len(parts[0]) < 8000 {
			t.Fatalf("planner stable prefix too short: %d", len(parts[0]))
		}
		if strings.Join(parts, "\n\n") != prompt {
			t.Fatalf("%s changed instructions", role)
		}
		budgets := map[string]int{"worker": 10400, "planner": 13500, "mainagent": 4500, "chat": 600}
		if len(prompt) > budgets[role] {
			t.Fatalf("%s prompt grew beyond reviewed byte ceiling: %d > %d", role, len(prompt), budgets[role])
		}
		t.Logf("%s system bytes=%d prefix bytes=%d boundary=%d", role, len(prompt), len(parts[0]), b)
	}
}

func TestRenderedPrefixesAcrossGoalsAndDirectories(t *testing.T) {
	t.Setenv("ARTEX_CACHE_PREFIX", "1")
	for _, render := range []func(string, string, string) string{plannerSystem, mainAgentSystem} {
		one := render("goal one", "/data", "/data/tasks/1")
		two := render("entirely changed goal", "/data", "/data/tasks/2")
		a, _ := cachePrefix([]string{one}, 0)
		b, _ := cachePrefix([]string{two}, 0)
		if a[0] != b[0] || strings.Join(a, "\n\n") != one || strings.Join(b, "\n\n") != two {
			t.Fatal("real prompt prefix depends on goal or changes instructions")
		}
	}
}

func TestRenderedWorkerAndChatPrefixesAcrossDirectories(t *testing.T) {
	t.Setenv("ARTEX_CACHE_PREFIX", "1")
	pairs := [][2]string{{workerSystem("", "", "/data", "/data/tasks/1/i1"), workerSystem("", "", "/data", "/data/tasks/2/i2")}, {chatSystem("assistant", "/data", "/data/sessions/one"), chatSystem("assistant", "/data", "/data/sessions/two")}}
	for _, pair := range pairs {
		a, _ := cachePrefix([]string{pair[0]}, 0)
		b, _ := cachePrefix([]string{pair[1]}, 0)
		if a[0] != b[0] || strings.Join(a, "\n\n") != pair[0] {
			t.Fatal("real role prefix changed with directory")
		}
	}
}
