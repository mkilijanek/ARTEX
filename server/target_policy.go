package server

import (
	"context"
	"errors"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
)

// registerTargetPolicy installs the WebFetch egress policy (design D1): the rule loader
// backed by the asset intercept tables, ARTEX's own addresses, the database endpoints
// main captured from the DSN, and whether a global egress proxy is set (which disables WebFetch's
// direct retry). Called from New and again whenever the global proxy changes; agents
// pick the proxy flag up when they are rebuilt.
func (s *Server) registerTargetPolicy() {
	cp := agent.ControlPlane{ListenAddr: s.listenAddr, RecordingProxyAddr: s.recordingProxyAddr}
	// The database endpoints were handed over once by main (WithDatabaseEndpoints). There is no
	// fallback to the DSN: the environment no longer holds it, and without a capture the policy
	// refuses every fetch (agent.dsnEndpoints).
	s.tpMu.Lock()
	cp.DBEndpoints, cp.DBCaptured = s.dbEndpoints, s.dbCaptured
	s.tpMu.Unlock()
	m := s.m
	loader := func(_ context.Context, taskID int64) (block, allow []db.AssetInterceptRule, err error) {
		if m == nil || m.pg == nil || m.assets == nil {
			return nil, nil, errors.New("rule store not available")
		}
		// Global rules are block-only; a task adds its own block and allow rules.
		block, err = m.pg.ListAssetInterceptRules()
		if err != nil {
			return nil, nil, err
		}
		if taskID > 0 {
			tb, ta, err := m.assets.TaskInterceptRulesSplit(taskID)
			if err != nil {
				return nil, nil, err
			}
			block = append(block, tb...)
			allow = ta
		}
		return block, allow, nil
	}
	agent.SetTargetPolicy(loader, cp, m != nil && m.GlobalProxy() != "")
}
