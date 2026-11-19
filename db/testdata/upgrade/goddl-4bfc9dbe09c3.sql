-- llmRecordsSchema

CREATE TABLE IF NOT EXISTS llm_records (
    id            BIGSERIAL PRIMARY KEY,
    ts            TIMESTAMPTZ DEFAULT now(),
    model         TEXT,
    profile_name  TEXT,
    session_id    TEXT,
    task_id       TEXT,
    worker        TEXT,
    latency_ms    INTEGER,
    input_tokens  INTEGER,
    output_tokens INTEGER,
    cache_read    INTEGER,
    cache_write   INTEGER,
    status        TEXT,
    error         TEXT,
    request_body  TEXT,
    response_body TEXT,
    raw_request   TEXT,
    raw_response  TEXT
);
CREATE INDEX IF NOT EXISTS idx_llm_records_ts ON llm_records(ts);
CREATE INDEX IF NOT EXISTS idx_llm_records_session ON llm_records(session_id);
-- llmRecordsMigrate

ALTER TABLE llm_records ADD COLUMN IF NOT EXISTS task_id TEXT;
ALTER TABLE llm_records ADD COLUMN IF NOT EXISTS worker TEXT;
ALTER TABLE llm_records ADD COLUMN IF NOT EXISTS profile_name TEXT;
ALTER TABLE llm_records ADD COLUMN IF NOT EXISTS raw_request TEXT;
ALTER TABLE llm_records ADD COLUMN IF NOT EXISTS raw_response TEXT;
-- llmUsageSchema

CREATE TABLE IF NOT EXISTS llm_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    task_id        TEXT,
    exploration_id BIGINT,
    worker         TEXT,
    model          TEXT,
    profile_name   TEXT,
    latency_ms     INTEGER,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read     INTEGER NOT NULL DEFAULT 0,
    cache_write    INTEGER NOT NULL DEFAULT 0,
    status         TEXT
);
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS intent_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS reason TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS request_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS system_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS message_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS tool_schema_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS tool_result_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS size_known BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS call_id TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS round_no INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_usage ADD COLUMN IF NOT EXISTS tool_bytes JSONB NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS idx_llm_usage_call ON llm_usage(call_id) WHERE call_id <> '';
CREATE INDEX IF NOT EXISTS idx_llm_usage_purpose ON llm_usage(purpose, ts);
CREATE INDEX IF NOT EXISTS idx_llm_usage_task  ON llm_usage(task_id);
CREATE INDEX IF NOT EXISTS idx_llm_usage_model ON llm_usage(task_id, model);
CREATE INDEX IF NOT EXISTS idx_llm_usage_exp   ON llm_usage(exploration_id);
