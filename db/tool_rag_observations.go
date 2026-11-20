package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// Limits the database layer enforces on one observation (Phase D.3 plan, 3.3 and 3.6). The caller sanitises names
// (over-long or control-character names become "_unknown") and counts overflow under "_overflow"; the checks here
// refuse a row that breaks the rule instead of silently changing it.
const (
	ToolRAGMaxNameBytes = 128
	ToolRAGMaxRanking   = 256
	// ToolRAGMaxCalled is the 256 names of the name rule plus the "_overflow" entry.
	ToolRAGMaxCalled = ToolRAGMaxRanking + 1
)

// ErrToolRAGObservationInvalid marks a row refused before any SQL ran.
var ErrToolRAGObservationInvalid = errors.New("invalid tool RAG observation")

// ToolRAGCall is one entry of ToolRAGObservation.Called: how often the tool was called and its class
// (candidate, keep, deferred_global, deferred_locked, other).
type ToolRAGCall struct {
	N     int    `json:"n"`
	Class string `json:"class"`
}

// ToolRAGObservation is one row of tool_rag_observations. It holds names and counts only: never the query,
// descriptions, arguments or results. ExplorationID and IntentID are 0 when unknown and stored as NULL.
type ToolRAGObservation struct {
	TaskID            int64
	ExplorationID     int64
	IntentID          int64
	AgentKey          string
	Scorer            string
	Mode              string
	Outcome           string
	K                 int
	HostListCount     int
	KeepCount         int
	CandidateCount    int
	NonzeroCount      int
	WouldHideCount    int
	DeferredCount     int
	LockedAtStart     int
	UnlockedDuringRun int
	QueryTokens       int
	QueryTruncated    bool
	DurationUS        int
	Ranking           []string
	Called            map[string]ToolRAGCall
}

func toolRAGName(kind, name string) error {
	if name == "" || len(name) > ToolRAGMaxNameBytes || !utf8.ValidString(name) {
		return fmt.Errorf("%w: %s name is empty, over %d bytes or not UTF-8", ErrToolRAGObservationInvalid, kind, ToolRAGMaxNameBytes)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s name has a control character", ErrToolRAGObservationInvalid, kind)
		}
	}
	return nil
}

func (o *ToolRAGObservation) validate() error {
	if o.TaskID <= 0 {
		return fmt.Errorf("%w: task id is required", ErrToolRAGObservationInvalid)
	}
	if len(o.Ranking) > ToolRAGMaxRanking {
		return fmt.Errorf("%w: ranking has %d names, at most %d", ErrToolRAGObservationInvalid, len(o.Ranking), ToolRAGMaxRanking)
	}
	if len(o.Called) > ToolRAGMaxCalled {
		return fmt.Errorf("%w: called has %d entries, at most %d", ErrToolRAGObservationInvalid, len(o.Called), ToolRAGMaxCalled)
	}
	for _, name := range o.Ranking {
		if err := toolRAGName("ranking", name); err != nil {
			return err
		}
	}
	for name := range o.Called {
		if err := toolRAGName("called", name); err != nil {
			return err
		}
	}
	return nil
}

// InsertToolRAGObservation appends one observation row. The caller supplies the context (the writer gives each
// insert its own timeout). A row for a task that no longer exists fails with SQLSTATE 23503 (foreign key), which
// the caller counts as "task gone". Error text is passed through unchanged for the caller to reduce to the SQLSTATE.
func (d *DB) InsertToolRAGObservation(ctx context.Context, o *ToolRAGObservation) error {
	if err := o.validate(); err != nil {
		return err
	}
	ranking := o.Ranking
	if ranking == nil {
		ranking = []string{}
	}
	called := o.Called
	if called == nil {
		called = map[string]ToolRAGCall{}
	}
	calledJSON, err := json.Marshal(called)
	if err != nil {
		return fmt.Errorf("encode tool RAG called map: %w", err)
	}
	_, err = d.ExecContext(ctx, `
INSERT INTO tool_rag_observations(
  task_id, exploration_id, intent_id, agent_key, scorer, mode, outcome, k, host_list_count, keep_count,
  candidate_count, nonzero_count, would_hide_count, deferred_count, locked_at_start, unlocked_during_run,
  query_tokens, query_truncated, duration_us, ranking, called)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21::jsonb)`,
		o.TaskID, nullIfZero(o.ExplorationID), nullIfZero(o.IntentID), o.AgentKey, o.Scorer, o.Mode, o.Outcome,
		o.K, o.HostListCount, o.KeepCount, o.CandidateCount, o.NonzeroCount, o.WouldHideCount, o.DeferredCount,
		o.LockedAtStart, o.UnlockedDuringRun, o.QueryTokens, o.QueryTruncated, o.DurationUS, ranking, string(calledJSON))
	return err
}
