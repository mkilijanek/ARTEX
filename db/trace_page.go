package db

// ActivityTracePageWithSources pages only an owned or directly inherited intent.
// The terminal-state condition stays in SQL so a reopened source cannot leak a
// live trace between ownership resolution and row retrieval.
func (s *ExplorationStore) ActivityTracePageWithSources(nodeID, before int64, q string, limit int) ([]Activity, bool, error) {
	n, err := s.GetNodeWithSources(nodeID)
	if err != nil || n == nil || n.Kind != KindIntent {
		return []Activity{}, false, err
	}
	owner := s
	if n.Inherited {
		sources, e := s.DirectSourceStores()
		if e != nil {
			return nil, false, e
		}
		owner = nil
		for _, src := range sources {
			if src.Task.TaskID == n.SourceTaskID {
				owner = src.Store
				break
			}
		}
		if owner == nil {
			return []Activity{}, false, nil
		}
	}
	limit = max(1, min(limit, 50))
	rows, err := s.db.Query(`SELECT a.id,a.node_id,COALESCE(a.kind,''),COALESCE(a.tool,''),a.is_error,COALESCE(a.summary,'')
 FROM activity a JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
 WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent' AND a.kind NOT IN ('thinking','usage')
 AND (NOT $3 OR n.state IN ('done','blocked','exhausted','stopped'))
 AND ($4<=0 OR a.id<$4) AND ($5='' OR a.summary ILIKE '%'||$5||'%' OR a.detail ILIKE '%'||$5||'%')
 ORDER BY a.id DESC LIMIT $6`, owner.expID, nodeID, n.Inherited, before, q, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Kind, &a.Tool, &a.IsError, &a.Summary); err != nil {
			return nil, false, err
		}
		a.Inherited = n.Inherited
		a.SourceTaskID = n.SourceTaskID
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
