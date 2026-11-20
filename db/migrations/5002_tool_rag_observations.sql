-- downgrade-safe: yes: additive table, older binaries do not read it
CREATE TABLE IF NOT EXISTS tool_rag_observations (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMPTZ NOT NULL DEFAULT now(),
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    exploration_id BIGINT,
    intent_id BIGINT,
    agent_key TEXT NOT NULL,
    scorer TEXT NOT NULL,
    mode TEXT NOT NULL,
    outcome TEXT NOT NULL,
    k INTEGER NOT NULL,
    host_list_count INTEGER NOT NULL,
    keep_count INTEGER NOT NULL,
    candidate_count INTEGER NOT NULL,
    nonzero_count INTEGER NOT NULL,
    would_hide_count INTEGER NOT NULL,
    deferred_count INTEGER NOT NULL,
    locked_at_start INTEGER NOT NULL,
    unlocked_during_run INTEGER NOT NULL,
    query_tokens INTEGER NOT NULL,
    query_truncated BOOLEAN NOT NULL,
    duration_us INTEGER NOT NULL,
    ranking TEXT[] NOT NULL DEFAULT '{}',
    called JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_tool_rag_observations_task ON tool_rag_observations(task_id, ts);
