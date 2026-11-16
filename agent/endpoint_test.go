package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestNormaEndpointModesAcrossProtocols(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-must-not-leak")
	for _, format := range []string{"anthropic", "openai", "openai-responses", "ollama"} {
		for _, mode := range []string{"base", "fixed"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", format, mode, stream), func(t *testing.T) {
					var uri, ua, sid, key string
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						uri, ua, sid, key = r.RequestURI, r.UserAgent(), r.Header.Get("X-Norma-Session"), r.Header.Get("Authorization")
						var body map[string]any
						_ = json.NewDecoder(r.Body).Decode(&body)
						if body["stream"] != stream {
							t.Errorf("wire stream=%v", body["stream"])
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							switch format {
							case "anthropic":
								fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
							case "openai-responses":
								fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
							default:
								fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
							}
						} else {
							w.Header().Set("Content-Type", "application/json")
							switch format {
							case "anthropic":
								fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`)
							case "openai-responses":
								fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`)
							default:
								fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
							}
						}
					}))
					defer srv.Close()
					base, want := srv.URL, map[string]string{"anthropic": "/v1/messages", "openai": "/chat/completions", "openai-responses": "/responses", "ollama": "/v1/chat/completions"}[format]
					if mode == "fixed" {
						want = "/relay%2Fv2/messages/?route=a%2Fb"
						base += want
					}
					apiKey := "fixture-key"
					if format == "ollama" {
						apiKey = ""
					}
					cfg := ConfigFromEndpoint(format, "fixture", base, apiKey, "", mode)
					cfg.UserAgent = "artex-sdk-fixture/1"
					cfg.SessionHeaderKey = "X-Norma-Session"
					p, err := cfg.NewProvider()
					if err != nil {
						t.Fatal(err)
					}
					ctx := transcript.WithSessionID(t.Context(), "stable-session")
					req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}}
					text := ""
					if stream {
						acc := llm.NewAccumulator()
						for ev, e := range p.Stream(ctx, req) {
							if e != nil {
								t.Fatal(e)
							}
							acc.Add(ev)
						}
						text = acc.Message().Text()
					} else {
						m, _, _, e := p.Complete(ctx, req)
						if e != nil {
							t.Fatal(e)
						}
						text = m.Text()
					}
					if uri != want || ua != cfg.UserAgent || sid != "stable-session" || text != "OK" {
						t.Fatalf("uri=%q want=%q ua=%q session=%q text=%q", uri, want, ua, sid, text)
					}
					if format == "ollama" && key != "Bearer ollama" {
						t.Fatalf("Ollama key fallback: %q", key)
					}
				})
			}
		}
	}
}

func TestNormaFixedEndpointValidationAndRedirect(t *testing.T) {
	for _, raw := range []string{"", "example.com/v2/messages", "ftp://example.com/a", "https://user:pass@example.com/a", "https://example.com/a#fragment"} {
		if _, err := (Config{EndpointMode: EndpointFixed, BaseURL: raw}).NewProvider(); err == nil {
			t.Errorf("accepted fixed endpoint %q", raw)
		}
	}
	if err := ValidateEndpoint("typo", "https://example.com"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	p, err := ConfigFromEndpoint("openai", "fixture", srv.URL+"/exact", "fixture", "", EndpointFixed).NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = p.Complete(t.Context(), llm.CompletionRequest{}); err == nil {
		t.Fatal("redirect treated as completion")
	}
	if redirected.Load() != 0 {
		t.Fatal("fixed endpoint followed a credential-bearing redirect")
	}
}
