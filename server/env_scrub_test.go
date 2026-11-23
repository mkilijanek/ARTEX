package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/agent"
)

// A child process spawned after startup must not see the startup secrets, while
// other variables still pass through.
func TestScrubSecretEnvHidesSecretsFromChildren(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh in PATH")
	}
	for _, k := range secretEnv {
		t.Setenv(k, "leak-"+k)
	}
	t.Setenv("ARTEX_SCRUB_KEEP", "kept")

	ScrubSecretEnv()

	out, err := exec.Command(sh, "-c", "env").Output()
	if err != nil {
		t.Fatal(err)
	}
	env := "\n" + string(out)
	for _, k := range secretEnv {
		if strings.Contains(env, "\n"+k+"=") {
			t.Errorf("%s still reaches child processes", k)
		}
	}
	if !strings.Contains(env, "\nARTEX_SCRUB_KEEP=kept\n") {
		t.Errorf("non-secret variable did not reach the child process")
	}
}

// The scrub list names every credential main reads once at startup, and only those: the SDK
// reads ARTEX_LLM_BASE_URL again, so removing it would silently send requests to the public
// endpoint.
func TestSecretEnvNamesCoverEveryStartupSecret(t *testing.T) {
	got := SecretEnvNames()
	have := map[string]bool{}
	for _, k := range got {
		have[k] = true
	}
	for _, want := range []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ARTEX_LLM_PROXY"} {
		if !have[want] {
			t.Errorf("%s is not scrubbed", want)
		}
	}
	if have["ARTEX_LLM_BASE_URL"] {
		t.Error("ARTEX_LLM_BASE_URL must stay: the SDK's provider constructor reads it again")
	}
	got[0] = "changed"
	if SecretEnvNames()[0] == "changed" {
		t.Error("SecretEnvNames hands out the list itself")
	}
}

// New no longer reads the environment: the provider config and the database endpoints come from
// main, which read them once and removed the secrets before anything could start a process.
func TestNewTakesEnvConfigFromOptionsOnly(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", testDSN())
	if err != nil {
		t.Skipf("no manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	t.Setenv("ANTHROPIC_API_KEY", "must-not-be-read")
	t.Setenv("OPENAI_API_KEY", "must-not-be-read")
	t.Setenv("ARTEX_LLM_PROVIDER", "openai")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	t.Cleanup(func() { agent.SetTargetPolicy(nil, agent.ControlPlane{}, false) })
	if s.envLLMOK {
		t.Fatal("New picked up a provider config without being handed one")
	}
	if s.dbCaptured {
		t.Fatal("New captured database endpoints without being handed them")
	}
}
