package server

import (
	"os"

	"github.com/mkilijanek/artex/agent"
)

// secretEnv lists the credentials read from the environment only at startup, by cmd/artex's main:
// ARTEX_PG_DSN (the database), the two provider API keys and the LLM proxy URL (it may carry
// credentials), all read once by agent.FromEnv and db.DSN; and the password of the Signal service's
// database role, which main hands to role provisioning. ARTEX_LLM_BASE_URL is deliberately not
// here: the SDK's provider constructor reads it again, and it is not on the tool allow-list, so
// tools never see it.
var secretEnv = []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ARTEX_LLM_PROXY", "ARTEX_SIGNAL_DB_PASSWORD"}

// SecretEnvNames returns the names ScrubSecretEnv removes. Isolation start-up refuses to go on
// while any of them is still set.
func SecretEnvNames() []string { return append([]string(nil), secretEnv...) }

// ScrubSecretEnv removes secretEnv from the process environment once startup has read it. Child
// processes (agent shells, custom tools, MCP stdio servers) copy os.Environ() when they are
// spawned, so they no longer inherit these values.
//
// main calls it right after the single read and before isolation start-up, the first thing that
// can start a tool process.
func ScrubSecretEnv() {
	for _, k := range secretEnv {
		_ = os.Unsetenv(k)
	}
}

// Option configures New. Options keep the test call sites unchanged.
type Option func(*Server)

// WithControlPlane tells the WebFetch egress policy which addresses are ARTEX's own:
// the HTTP listen address and the traffic recording proxy address (cmd/artex -addr and
// -proxy). Without it only the cloud-metadata ranges and the database stay denied.
func WithControlPlane(listenAddr, recordingProxyAddr string) Option {
	return func(s *Server) { s.listenAddr, s.recordingProxyAddr = listenAddr, recordingProxyAddr }
}

// WithDatabaseEndpoints hands the server the database endpoints main parsed from the DSN it read
// once. The WebFetch policy denies them. An empty list is a valid capture (a socket-only DSN);
// without this option the endpoints are not captured and WebFetch refuses every fetch.
func WithDatabaseEndpoints(eps []agent.DBEndpoint) Option {
	return func(s *Server) { s.dbEndpoints, s.dbCaptured = eps, true }
}

// WithEnvLLM hands the server the provider config main read from the environment once, before the
// keys were removed from it. The server uses it only when no persisted profile exists.
func WithEnvLLM(cfg agent.Config, ok bool) Option {
	return func(s *Server) { s.envLLM, s.envLLMOK = cfg, ok }
}
