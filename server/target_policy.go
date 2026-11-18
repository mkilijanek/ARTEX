package server

import (
	"context"
	"errors"
	"log"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
)

// registerTargetPolicy installs the WebFetch egress policy (design D1): the rule loader
// backed by the asset intercept tables, ARTEX's own addresses, the database endpoint
// from the DSN, and whether a global egress proxy is set (which disables WebFetch's
// direct retry). Called from New and again whenever the global proxy changes; agents
// pick the proxy flag up when they are rebuilt.
func (s *Server) registerTargetPolicy() {
	cp := agent.ControlPlane{ListenAddr: s.listenAddr, RecordingProxyAddr: s.recordingProxyAddr}
	// The database endpoint is captured once, at startup: ScrubSecretEnv removes
	// ARTEX_PG_DSN afterwards, so a later re-registration could not read it again.
	s.tpMu.Lock()
	if !s.dbCaptured {
		if dsn, _, err := db.DSN(); err == nil {
			if eps, err := agent.DatabaseEndpoint(dsn); err != nil {
				// Not fatal: the denylist then reads the DSN again when a fetch starts.
				log.Printf("[webfetch-policy] database endpoint unavailable: %v", err)
			} else {
				s.dbEndpoints, s.dbCaptured = eps, true
			}
		}
	}
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
