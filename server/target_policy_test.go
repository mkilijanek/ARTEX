package server

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// Finding 5: ScrubSecretEnv removes ARTEX_PG_DSN after startup, so a re-registration of
// the policy (the global-proxy handler does it) must reuse the database endpoint captured
// at startup instead of reading the DSN again.
func TestTargetPolicyKeepsDBEndpointOnReRegistration(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN not set")
	}
	t.Setenv("ARTEX_PG_DSN", dsn) // restored after the scrub below
	eps, err := agent.DatabaseEndpoint(dsn)
	if err != nil || len(eps) == 0 {
		t.Fatalf("fixture: no network endpoint in the DSN: %v", err)
	}
	port := eps[0].Port
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("no manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir(), WithSecretScrub())
	t.Cleanup(func() { agent.SetTargetPolicy(nil, agent.ControlPlane{}, false) })
	if os.Getenv("ARTEX_PG_DSN") != "" {
		t.Fatal("fixture: the scrub did not remove the DSN")
	}

	u := mustParseURL(t, "http://127.0.0.1:"+strconv.Itoa(int(port))+"/")
	denied := func(when string) {
		t.Helper()
		_, err := agent.NewTargetGuard("", false).Check(agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: 7, ExplorationID: 1}), u)
		var te *agent.TargetError
		if !errors.As(err, &te) || te.Code != agent.TargetControlPlane {
			t.Fatalf("%s: the database port on a local address is not denied, got %v", when, err)
		}
	}
	denied("after startup")
	s.registerTargetPolicy() // what the global-proxy settings handler does
	denied("after re-registration")
}

// AC4/AC5: the production loader composes global block rules with a task's own block
// and allow rules; a chat session (task id 0) gets the global rules only.
func TestTargetPolicyLoaderComposesGlobalAndTaskRules(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("ARTEX_PG_DSN not set")
	}
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("no manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	t.Cleanup(func() { agent.SetTargetPolicy(nil, agent.ControlPlane{}, false) })

	global, err := m.pg.CreateAssetInterceptRule("exact_ip", "127.0.0.9", "loader test", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteAssetInterceptRule(global.ID) })
	task, err := m.pg.CreateTask("loader composition", "loader composition", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteTask(task.ID) })
	if _, err := m.assets.CreateTaskInterceptRule(task.ID, "block", "exact_ip", "127.0.0.10", "loader test", true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.assets.CreateTaskInterceptRule(task.ID, "allow", "exact_ip", "127.0.0.11", "loader test", true); err != nil {
		t.Fatal(err)
	}

	check := func(taskID int64, host string) string {
		t.Helper()
		ri := agent.RunInfo{TaskID: taskID, ExplorationID: 1}
		if taskID == 0 {
			ri = agent.RunInfo{SessionID: "chat-loader-test"} // a chat session has no exploration
		}
		_, err := agent.NewTargetGuard("", false).Check(agent.WithRunInfo(t.Context(), ri), mustParseURL(t, "http://"+host+":9/"))
		if err == nil {
			return ""
		}
		var te *agent.TargetError
		if !errors.As(err, &te) {
			t.Fatalf("%s: unexpected error %v", host, err)
		}
		return te.Code
	}
	for _, tc := range []struct {
		name   string
		taskID int64
		host   string
		want   string
	}{
		{"task: global block rule applies", task.ID, "127.0.0.9", "TARGET_BLOCKED"},
		{"task: task block rule applies", task.ID, "127.0.0.10", "TARGET_BLOCKED"},
		{"task: allow rule admits its address", task.ID, "127.0.0.11", ""},
		{"task: allow rules gate everything else", task.ID, "127.0.0.12", "TARGET_NOT_ALLOWED"},
		{"chat: global block rule applies", 0, "127.0.0.9", "TARGET_BLOCKED"},
		{"chat: task rules do not apply", 0, "127.0.0.10", ""},
		{"chat: no allow gate", 0, "127.0.0.12", ""},
	} {
		if got := check(tc.taskID, tc.host); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Review finding 3: a DSN may name several hosts and ports (pgx tries them in turn). Every
// (host, port) pair stays denied after the scrub and after re-registration, and every
// database port is denied on local addresses.
func TestTargetPolicyKeepsEveryDBEndpointOnReRegistration(t *testing.T) {
	for _, k := range []string{"PGHOST", "PGPORT", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGPASSFILE"} {
		t.Setenv(k, "")
	}
	t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@10.77.0.9:5432,10.77.0.10:6432/artex?sslmode=disable")
	s := &Server{listenAddr: ":1"} // no manager: only the control-plane part of the policy is exercised
	s.registerTargetPolicy()
	t.Cleanup(func() { agent.SetTargetPolicy(nil, agent.ControlPlane{}, false) })
	t.Setenv("ARTEX_PG_DSN", "") // what ScrubSecretEnv leaves behind

	denied := func(when, dest string) {
		t.Helper()
		_, err := agent.NewTargetGuard("", false).Check(agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: 7, ExplorationID: 1}), mustParseURL(t, dest))
		var te *agent.TargetError
		if !errors.As(err, &te) || te.Code != agent.TargetControlPlane {
			t.Errorf("%s: %s is not refused as control plane, got %v", when, dest, err)
		}
	}
	check := func(when string) {
		for _, dest := range []string{
			"http://10.77.0.9:5432/",  // first host on its port
			"http://10.77.0.10:6432/", // fallback host on its port
			"http://127.0.0.1:5432/",  // local address on every database port
			"http://127.0.0.1:6432/",
			"http://127.0.0.1:06432/",
		} {
			denied(when, dest)
		}
	}
	check("after startup")
	s.registerTargetPolicy() // what the global-proxy settings handler does
	check("after re-registration")
}
