package server

import (
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"

	"github.com/mkilijanek/artex/sandbox"
)

// checkMCPEnvKeys refuses, at save, the environment keys of an MCP row that the drop chain would
// refuse at spawn. For http and sse the same column holds request headers (X-API-Key, Authorization)
// that never reach a process, so nothing is checked there; any other transport is treated as
// stdio, where the keys become variables of a process (contradiction 12).
//
// The error names the key and never a value. The refusal is logged by name too, so an attempt to
// save a library path or a loader variable is visible.
func checkMCPEnvKeys(name, transport string, env json.RawMessage) error {
	if transport == "http" || transport == "sse" {
		return nil
	}
	if t := strings.TrimSpace(string(env)); t == "" || t == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(env, &m); err != nil {
		return errors.New("env must be an object")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := sandbox.ValidateVarName(k); err != nil {
			log.Printf("[mcp] refused to save server %s: %v", logSafe(name), err)
			return err
		}
	}
	return nil
}
