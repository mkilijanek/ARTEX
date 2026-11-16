package db

import (
	"fmt"
	"testing"
	"time"
)

func TestEndpointModeProfileRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p := &LLMProfile{Name: fmt.Sprintf("sdk-endpoint-%d", time.Now().UnixNano()), Format: "ollama", Model: "fixture", BaseURL: "http://localhost:11434/relay%2Fv2/?x=a%2Fb", EndpointMode: "fixed", UserAgent: "fixture/1", APIKey: "fixture-key"}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
	p.ID = id
	for _, key := range []string{"", "updated-fixture-key"} {
		p.APIKey = key
		if _, err := d.SaveProfile(p); err != nil {
			t.Fatal(err)
		}
		got, err := d.ProfileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.EndpointMode != p.EndpointMode || got.BaseURL != p.BaseURL || got.UserAgent != p.UserAgent || got.APIKey == "" {
			t.Fatalf("roundtrip=%+v err=%v", got, err)
		}
	}
}
