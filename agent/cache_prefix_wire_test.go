package agent

import (
	"encoding/json"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type wireSystemBlock struct {
	Text  string         `json:"text"`
	Cache map[string]any `json:"cache_control"`
}

func TestCachePrefixPhysicalAnthropicRequestRetainsBothBreakpoints(t *testing.T) {
	requests := make(chan []wireSystemBlock, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System []wireSystemBlock `json:"system"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body.System
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"fixture complete"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	defer server.Close()
	provider, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: server.URL, APIKey: "fixture", Model: "fixture", MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"0", "1"} {
		t.Run("flag="+flag, func(t *testing.T) {
			t.Setenv("ARTEX_CACHE_PREFIX", flag)
			firstPrefix := ""
			for _, goal := range []string{"goal one", "different goal"} {
				body := plannerSystem(goal, "/data", "/data/tasks/1")
				system, boundary := deferredSystem(body, DeferredInfo{GlobalNames: []string{"mcp__fixture__read"}, FindingGuidance: findingIDGuidance})
				opts := agentcore.Options{Provider: provider, SystemPrompt: system, DynamicBoundary: boundary, NonStreaming: true, MaxTurns: 1}
				if _, _, err := captureRun(t.Context(), opts, "Only finish this fixture; no tools.", nil); err != nil {
					t.Fatal(err)
				}
				wire := <-requests
				joined := []string{}
				cached := []string{}
				for _, b := range wire {
					joined = append(joined, b.Text)
					if b.Cache != nil {
						cached = append(cached, b.Text)
					}
				}
				text := strings.Join(joined, "\n\n")
				if strings.Count(text, findingIDGuidance) != 1 || !strings.Contains(text, "mcp__fixture__read") || !strings.Contains(text, goal) || strings.Count(text, normaTrustRules) != 1 {
					t.Fatal("instruction/guidance loss")
				}
				if flag == "1" {
					if len(cached) != 2 || len(cached[0]) < 8000 || strings.Contains(cached[0], goal) {
						t.Fatal("static prefix not cached independently", len(cached))
					}
					if firstPrefix != "" && firstPrefix != cached[0] {
						t.Fatal("physical cached prefix changed with goal")
					}
					firstPrefix = cached[0]
					if !strings.Contains(cached[1], "mcp__fixture__read") {
						t.Fatal("legacy deferred guidance endpoint lost")
					}
				} else if len(cached) != 1 || !strings.Contains(cached[0], goal) || !strings.Contains(cached[0], "mcp__fixture__read") {
					t.Fatal("flag-off cache coverage changed")
				}
				t.Logf("flag=%s goal=%q cached_blocks=%d stable_prefix_bytes=%d full_system_bytes=%d", flag, goal, len(cached), len(cached[0]), len(text))
			}
		})
	}
}
