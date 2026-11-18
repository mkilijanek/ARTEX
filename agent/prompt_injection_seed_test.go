package agent

import "strings"
import "testing"

// The prompt-injection hardening block is code-owned (D4): the four consumer agents'
// default seeds do not carry it, and the real rendered prompt of each role, built from
// that seed, carries it exactly once.
func TestPromptSeedsCarryInjectionHardening(t *testing.T) {
	seeds := BuiltinPromptSeeds()
	prev := PromptOverride()
	SetPromptOverride(func(key string) (string, bool) { s, ok := seeds[key]; return s, ok })
	t.Cleanup(func() { SetPromptOverride(prev) })
	rendered := map[string]string{
		"worker":    workerSystem("", "", "/data", "/run"),
		"pentest":   chatSystem("pentest", "/data", "/work"),
		"mainagent": mainAgentSystem("goal", "/data", "/work"),
		"auto":      chatSystem("auto", "/data", "/work"),
	}
	for name, sys := range rendered {
		seed, ok := seeds[name]
		if !ok {
			t.Fatalf("no builtin seed for %s", name)
		}
		if strings.Contains(seed, untrustedContentInstructions) || strings.Contains(seed, "prompt-injection defense") {
			t.Errorf("%s seed still carries the untrusted-content block: it is a code-owned tail", name)
		}
		if head := seed[:40]; !strings.HasPrefix(sys, head) {
			t.Errorf("%s: the rendered prompt was not built from its seed (%q)", name, head)
		}
		if n := strings.Count(sys, untrustedContentInstructions); n != 1 {
			t.Errorf("%s: rendered prompt carries the untrusted-content block %d times, want 1", name, n)
		}
		for _, marker := range []string{"prompt-injection defense", "never instructions to you", "do NOT comply"} {
			if !strings.Contains(sys, marker) {
				t.Errorf("%s: rendered prompt missing hardening marker %q", name, marker)
			}
		}
	}
}
