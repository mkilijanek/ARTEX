package server

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mkilijanek/artex/useragent"
)

// uaFixture resets the User-Agent settings before and after, so tests do not see each other.
func uaFixture(t *testing.T) (*Server, func(method, path, body string) (int, string)) {
	t.Helper()
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg
	reset := func() {
		pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2,$3)`, settingUAHidden, settingUACustom, settingUAComponents)
		if err := loadUserAgentSettings(pg); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	t.Cleanup(reset)
	return s, func(method, path, body string) (int, string) {
		r := request(method, path, body)
		return r.Code, r.Body.String()
	}
}

func TestUserAgentSettingsDefaultHidden(t *testing.T) {
	s, _ := uaFixture(t)
	p := s.settingsPayload()
	if p["user_agent_hidden"] != true || p["user_agent_custom"] != "" {
		t.Fatalf("defaults: hidden=%v custom=%q", p["user_agent_hidden"], p["user_agent_custom"])
	}
	if got := len(p["user_agent_components"].([]string)); got != 6 {
		t.Fatalf("components default = %d, want all 6 configurable", got)
	}
	if useragent.For("LLM") != "" {
		t.Fatal("hidden by default: For must be empty")
	}
	if v, ok := os.LookupEnv(toolUAEnv); ok {
		t.Fatalf("%s must be unset while hidden, got %q", toolUAEnv, v)
	}
}

func TestUserAgentSettingsRoundTripAndRuntime(t *testing.T) {
	s, req := uaFixture(t)
	code, body := req("PUT", "/api/settings", `{"user_agent_hidden":false}`)
	if code != 200 {
		t.Fatal(code, body)
	}
	var got struct {
		Hidden    bool              `json:"user_agent_hidden"`
		Version   string            `json:"user_agent_version"`
		Effective map[string]string `json:"user_agent_effective"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Hidden || got.Version == "" {
		t.Fatalf("payload = %+v", got)
	}
	// Runtime: the flag changed without a restart, and the tool env follows it.
	want := "ARTEX/" + BuildVersion + " (+LLM)"
	if ua := useragent.For("LLM"); !strings.Contains(ua, want) || got.Effective["LLM"] != ua {
		t.Fatalf("For(LLM) = %q, effective = %q", ua, got.Effective["LLM"])
	}
	if os.Getenv(toolUAEnv) != useragent.For("API Recon") || os.Getenv(toolUAEnv) == "" {
		t.Fatalf("%s = %q", toolUAEnv, os.Getenv(toolUAEnv))
	}
	if got.Effective["WebFetch"] != useragent.WebFetchUA || got.Effective["WebSearch"] != useragent.WebSearchUA {
		t.Fatalf("norma values missing: %v", got.Effective)
	}

	// Custom value for checked components only.
	if code, body = req("PUT", "/api/settings", `{"user_agent_custom":"  Custom/1  ","user_agent_components":["LLM","Tools"]}`); code != 200 {
		t.Fatal(code, body)
	}
	if useragent.For("LLM") != "Custom/1" || useragent.For("Tools") != "Custom/1" {
		t.Fatalf("custom not used: %q", useragent.For("LLM"))
	}
	if useragent.For("MCP") != "" || os.Getenv(toolUAEnv) != "" {
		t.Fatalf("unchecked component must be hidden; MCP=%q env=%q", useragent.For("MCP"), os.Getenv(toolUAEnv))
	}
	if s.settingsPayload()["user_agent_custom"] != "Custom/1" {
		t.Fatal("custom not persisted trimmed")
	}
	// Persisted: a reload from the database gives the same state.
	if err := loadUserAgentSettings(s.m.pg); err != nil {
		t.Fatal(err)
	}
	if useragent.For("Tools") != "Custom/1" || useragent.For("Notify") != "" {
		t.Fatal("reload from the database lost the state")
	}

	// Switching back to hidden wins over everything.
	if code, body = req("PUT", "/api/settings", `{"user_agent_hidden":true}`); code != 200 {
		t.Fatal(code, body)
	}
	if useragent.For("LLM") != "" {
		t.Fatal("hidden must override the custom value")
	}
}

func TestUserAgentSettingsInvalidStoresNothing(t *testing.T) {
	s, req := uaFixture(t)
	for _, body := range []string{
		`{"user_agent_hidden":false,"user_agent_custom":"bad\u0000ua"}`,
		`{"user_agent_hidden":false,"user_agent_custom":"Skaner-\u017c/1"}`,
		`{"user_agent_hidden":false,"user_agent_custom":"` + strings.Repeat("a", 257) + `"}`,
		`{"user_agent_hidden":false,"user_agent_components":["LLM","WebFetch"]}`,
		`{"user_agent_hidden":false,"user_agent_components":["WebSearch"]}`,
		`{"user_agent_hidden":false,"user_agent_components":["nope"]}`,
	} {
		if code, resp := req("PUT", "/api/settings", body); code != 400 {
			t.Fatalf("%s: code %d %s, want 400", body, code, resp)
		}
	}
	for _, k := range []string{settingUAHidden, settingUACustom, settingUAComponents} {
		if _, ok, _ := s.m.pg.GetSetting(k); ok {
			t.Fatalf("%s was stored by a refused request", k)
		}
	}
	if !useragent.Hidden() {
		t.Fatal("a refused request changed the runtime flag")
	}
}

// A stored value that no longer validates must not leave the previous (shown) state in force:
// the update reports the error and the User-Agent is published hidden.
func TestUserAgentReloadFailurePublishesHidden(t *testing.T) {
	s, req := uaFixture(t)
	if code, body := req("PUT", "/api/settings", `{"user_agent_hidden":false}`); code != 200 {
		t.Fatal(code, body)
	}
	if useragent.Hidden() {
		t.Fatal("precondition: shown")
	}
	if err := s.m.pg.SetSetting(settingUACustom, "bad\x7fua"); err != nil {
		t.Fatal(err)
	}
	code, body := req("PUT", "/api/settings", `{"user_agent_hidden":false}`)
	if code != 500 || !strings.Contains(body, "stored "+settingUACustom) {
		t.Fatalf("code %d body %s, want 500 naming the stored value", code, body)
	}
	if !useragent.Hidden() || useragent.For("LLM") != "" || os.Getenv(toolUAEnv) != "" {
		t.Fatalf("after a failed reload the User-Agent must be hidden; hidden=%v env=%q", useragent.Hidden(), os.Getenv(toolUAEnv))
	}
}

// Concurrent PATCHes publish whole states only.
func TestUserAgentConcurrentPatchesAreWhole(t *testing.T) {
	_, req := uaFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"user_agent_hidden":false,"user_agent_custom":"A/1","user_agent_components":["LLM"]}`
			if i%2 == 1 {
				body = `{"user_agent_hidden":true,"user_agent_custom":"","user_agent_components":["MCP","Tools"]}`
			}
			if code, resp := req("PUT", "/api/settings", body); code != 200 {
				t.Errorf("code %d %s", code, resp)
			}
		}(i)
	}
	wg.Wait()
	s := useragent.Snapshot()
	a := !s.Hidden && s.Custom == "A/1" && slices.Equal(s.Components, []string{"LLM"})
	b := s.Hidden && s.Custom == "" && slices.Equal(s.Components, []string{"MCP", "Tools"})
	if !a && !b {
		t.Fatalf("final state is a mix: %+v", s)
	}
}
