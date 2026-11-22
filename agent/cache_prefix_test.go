package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
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
		budgets := map[string]int{"worker": 10400, "planner": 13500, "mainagent": 4500, "chat": 1500} // chat 600 -> 1500: the code-owned untrusted-content tail (D4) is now appended to the chat prompt
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

func TestPlannerDefaultOrderChangesOnlyWithCacheOptIn(t *testing.T) {
	for _, flag := range []string{"0", "1"} {
		t.Setenv("ARTEX_CACHE_PREFIX", flag)
		prompt := plannerSystem("unique goal fixture", "/data", "/data/tasks/1")
		goal := strings.Index(prompt, "Task goal: unique goal fixture")
		rules := strings.Index(prompt, "**How many intents")
		if goal < 0 || rules < 0 || (flag == "0" && goal > rules) || (flag == "1" && goal < rules) {
			t.Fatalf("flag=%s goal=%d rules=%d", flag, goal, rules)
		}
	}
}

// Measure the provider-facing system, including SDK trust framing and deferred
// guidance, rather than only logging sizes of the host template.
func TestProviderFacingPromptBudgetsWithTrafficAndDeferredTools(t *testing.T) {
	t.Setenv("ARTEX_UNTRUSTED_BOUNDARY", "1")
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	for _, cache := range []string{"0", "1"} {
		t.Run("cache="+cache, func(t *testing.T) {
			t.Setenv("ARTEX_CACHE_PREFIX", cache)
			for _, recording := range []bool{false, true} {
				t.Run(fmt.Sprint("recording=", recording), func(t *testing.T) {
					cert := ""
					if recording {
						cert = "/fixture/ca.pem"
					}
					prompts := map[string]string{"worker": workerSystem("127.0.0.1:8080", cert, "/data", "/data/tasks/1/i1"), "planner": plannerSystem("fixture goal", "/data", "/data/tasks/1"), "mainagent": mainAgentSystem("fixture goal", "/data", "/data/tasks/1"), "chat": chatSystem("assistant", "/data", "/data/sessions/test")}
					budgets := map[string]int{"worker": 14000, "planner": 16000, "mainagent": 7000, "chat": 3500}
					for role, body := range prompts {
						def := DeferredInfo{GlobalNames: []string{"mcp__fixture__read"}, FindingGuidance: findingIDGuidance}
						system, boundary := deferredSystem(body, def)
						var request llm.CompletionRequest
						provider := &plannerContextFixtureProvider{receive: func(req llm.CompletionRequest) { request = req }}
						opts := agentcore.Options{Provider: provider, SystemPrompt: system, DynamicBoundary: boundary, Tools: noTools, NonStreaming: true, MaxTurns: 1}
						if _, _, err := captureRun(t.Context(), opts, "fixture only", nil); err != nil {
							t.Fatal(err)
						}
						rendered := strings.Join(request.System, "\n\n")
						if len(rendered) > budgets[role] {
							t.Fatalf("%s actual system=%d > %d", role, len(rendered), budgets[role])
						}
						for _, block := range []string{normaTrustRules, findingIDGuidance, "<available-deferred-tools>"} {
							if strings.Count(rendered, block) != 1 {
								t.Fatalf("%s duplicated/missing block %q", role, block)
							}
						}
						if role == "worker" && strings.Contains(rendered, "**Traffic tools**") != recording {
							t.Fatal("traffic guidance mismatches tool availability")
						}
						if role == "worker" || role == "mainagent" {
							if strings.Count(rendered, untrustedContentInstructions) != 1 {
								t.Fatal("shared untrusted rules missing/duplicated", role)
							}
						}
						t.Logf("cache=%s recording=%v %s provider system=%d", cache, recording, role, len(rendered))
					}
				})
			}
		})
	}
}
