package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// UpdatePlannerContext serializes checkpoint updates across processes. The
// exploration row is the lock even before the first checkpoint exists. The
// callback must not perform database work; returning an error leaves it intact.
func (s *ExplorationStore) UpdatePlannerContext(ctx context.Context, update func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM explorations WHERE id=$1 FOR NO KEY UPDATE`, s.expID).Scan(&id); err != nil {
		return nil, err
	}
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT checkpoint FROM planner_context_checkpoints WHERE exploration_id=$1`, s.expID).Scan(&previous)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	next, err := update(previous)
	if err != nil {
		return nil, err
	}
	if !json.Valid(next) {
		return nil, fmt.Errorf("invalid planner context checkpoint")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO planner_context_checkpoints(exploration_id,checkpoint) VALUES($1,$2::jsonb)
ON CONFLICT(exploration_id) DO UPDATE SET checkpoint=excluded.checkpoint`, s.expID, string(next)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}
