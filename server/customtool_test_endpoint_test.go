package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
)

// postToolTest drives POST /api/tools/custom/test the way the tool editor does: an
// unsaved http exec spec taken straight from the request body, with no task or session.
func postToolTest(t *testing.T, urlTemplate string) toolOut {
	t.Helper()
	body, err := json.Marshal(map[string]any{"kind": "http", "exec": httpSpec(urlTemplate, nil), "params": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: &db.DB{}, dir: t.TempDir()}}
	rec := httptest.NewRecorder()
	s.pgTestCustomTool(rec, httptest.NewRequest(http.MethodPost, "/api/tools/custom/test", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, credTrunc(rec.Body.String()))
	}
	var out struct {
		Output  string `json:"output"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return toolOut{Text: out.Output, IsError: out.IsError}
}

// The editor's test endpoint is the request-body-to-outbound-request path that static
// analysis reports (go/request-forgery). It must run under the same destination policy
// as a real tool call: the control-plane denylist and the global block rules apply with
// no task, and no registered policy refuses the request.
func TestCustomToolTestEndpointDestinationPolicy(t *testing.T) {
	t.Run("cloud metadata address: refused", func(t *testing.T) {
		setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
		requireRefusal(t, postToolTest(t, "http://169.254.169.254/latest/meta-data/"), agent.TargetControlPlane)
	})
	t.Run("ARTEX's own port: refused, nothing sent", func(t *testing.T) {
		self := newCredLab(t, "127.0.0.1:0", false)
		setCredPolicy(t, credStaticRules(nil, nil), self.port, nil)
		requireRefusal(t, postToolTest(t, self.URL()+"/api/settings"), agent.TargetControlPlane)
		requireNoHits(t, "the control plane", self)
	})
	t.Run("global block rule: refused, nothing sent", func(t *testing.T) {
		lab := newCredLab(t, "127.0.0.2:0", false)
		setCredPolicy(t, credStaticRules([]db.AssetInterceptRule{credRule("exact_ip", "127.0.0.2")}, nil), "1", nil)
		requireRefusal(t, postToolTest(t, lab.URL()+"/echo"), "TARGET_BLOCKED")
		requireNoHits(t, "the blocked host", lab)
	})
	t.Run("no policy registered: refused, nothing sent", func(t *testing.T) {
		lab := newCredLab(t, "127.0.0.2:0", false)
		agent.SetTargetPolicy(nil, agent.ControlPlane{}, false)
		requireRefusal(t, postToolTest(t, lab.URL()+"/echo"), agent.TargetUnavailable)
		requireNoHits(t, "the host", lab)
	})
	t.Run("task allow rules are not consulted without a task", func(t *testing.T) {
		lab := newCredLab(t, "127.0.0.2:0", false)
		var asked []int64
		loader := func(_ context.Context, taskID int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
			asked = append(asked, taskID)
			return nil, nil, nil
		}
		setCredPolicy(t, loader, "1", nil)
		got := postToolTest(t, lab.URL()+"/echo")
		requireRan(t, got)
		if len(lab.Hits()) != 1 || !strings.Contains(got.Text, `"status":200`) {
			t.Fatalf("positive control did not reach the lab: hits=%d text=%q", len(lab.Hits()), credTrunc(got.Text))
		}
		for _, id := range asked {
			if id != 0 {
				t.Fatalf("rule loader was asked for task %d; the test endpoint has no task", id)
			}
		}
		if len(asked) == 0 {
			t.Fatal("the rule loader was never consulted: the policy did not run")
		}
	})
}
