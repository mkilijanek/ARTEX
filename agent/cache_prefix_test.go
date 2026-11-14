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
		if bound != 1 || a[0] != b[0] || strings.Join(a, "\n\n") != strings.Join(one, "\n\n") {
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
		if strings.Join(parts, "\n\n") != prompt {
			t.Fatalf("%s changed instructions", role)
		}
		t.Logf("%s system bytes=%d prefix bytes=%d boundary=%d", role, len(prompt), len(parts[0]), b)
	}
}
