package server

import (
	"slices"
	"testing"

	"github.com/mkilijanek/artex/sandbox"
)

// The tool environment allow-list passes the API Recon variable through when the server has it
// set, and nothing else of the family; without it the variable is absent.
func TestToolEnvPassesUserAgentOnlyWhenSet(t *testing.T) {
	with := sandbox.ToolEnv([]string{toolUAEnv + "=Mozilla/5.0 test", "SECRET_X=1"})
	if !slices.Contains(with, toolUAEnv+"=Mozilla/5.0 test") || slices.Contains(with, "SECRET_X=1") {
		t.Fatalf("env = %v", with)
	}
	for _, kv := range sandbox.ToolEnv([]string{"LANG=C"}) {
		if len(kv) >= len(toolUAEnv) && kv[:len(toolUAEnv)] == toolUAEnv {
			t.Fatalf("unset must stay absent: %v", kv)
		}
	}
}
