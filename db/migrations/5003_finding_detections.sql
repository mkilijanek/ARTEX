-- downgrade-safe: yes: additive tables, one function and one trigger on conversations; older binaries do not read them
CREATE TABLE IF NOT EXISTS finding_detections (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    conversation_id BIGINT UNIQUE REFERENCES conversations(id) ON DELETE SET NULL,
    path TEXT NOT NULL CHECK (path IN ('engine','agent')),
    trigger_kind TEXT NOT NULL DEFAULT 'manual' CHECK (trigger_kind IN ('manual','auto')),
    auto_attempt INTEGER NOT NULL DEFAULT 0 CHECK (auto_attempt BETWEEN 0 AND 10),
    started_by_kind TEXT NOT NULL DEFAULT 'operator',
    started_by_subject TEXT NOT NULL DEFAULT '' CHECK (char_length(started_by_subject) <= 200),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','completed','failed','stopped')),
    phase TEXT NOT NULL DEFAULT 'generate' CHECK (phase IN ('generate','validate')),
    not_before TIMESTAMPTZ,
    claimed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0,
    queries_used INTEGER NOT NULL DEFAULT 0 CHECK (queries_used >= 0),
    outcome TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','validated','partial','telemetry_gap','unvalidated','no_rules')),
    end_code TEXT NOT NULL DEFAULT '' CHECK (char_length(end_code) <= 64),
    notes TEXT NOT NULL DEFAULT '' CHECK (char_length(notes) <= 4000),
    snapshot JSONB NOT NULL CHECK (octet_length(snapshot::text) <= 1048576),
    input_manifest JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(input_manifest::text) <= 65536),
    stage_report JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(stage_report::text) <= 16384),
    summary TEXT NOT NULL DEFAULT '' CHECK (octet_length(summary) <= 16384),
    error TEXT NOT NULL DEFAULT '' CHECK (octet_length(error) <= 2000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    CONSTRAINT uq_finding_detections_id_finding UNIQUE (id, finding_id),
    CONSTRAINT ck_finding_detections_engine_conv CHECK (path <> 'engine' OR conversation_id IS NULL),
    CONSTRAINT ck_finding_detections_auto_attempt CHECK ((trigger_kind = 'auto') = (auto_attempt >= 1)),
    CONSTRAINT ck_finding_detections_tombstone
        CHECK (deleted_at IS NULL OR (trigger_kind = 'auto' AND status IN ('completed','failed','stopped')))
);
CREATE INDEX IF NOT EXISTS idx_finding_detections_history ON finding_detections(finding_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_finding_detections_active ON finding_detections(finding_id)
    WHERE status IN ('pending','running');
CREATE UNIQUE INDEX IF NOT EXISTS idx_finding_detections_auto_attempt ON finding_detections(finding_id, auto_attempt)
    WHERE trigger_kind = 'auto';
CREATE UNIQUE INDEX IF NOT EXISTS idx_finding_detections_auto_done ON finding_detections(finding_id)
    WHERE trigger_kind = 'auto' AND status IN ('pending','running','completed');
CREATE INDEX IF NOT EXISTS idx_finding_detections_queue ON finding_detections(id)
    WHERE path = 'engine' AND status = 'pending';

CREATE TABLE IF NOT EXISTS detection_rules (
    id BIGSERIAL PRIMARY KEY,
    detection_id BIGINT NOT NULL,
    finding_id BIGINT NOT NULL,
    parent_id BIGINT,
    stage TEXT NOT NULL CHECK (stage IN ('attempt','success','post_exploitation','agent_behavior')),
    format TEXT NOT NULL CHECK (format IN ('sigma','kql','suricata','waf_imperva','waf_f5','yara')),
    data_source TEXT NOT NULL DEFAULT '' CHECK (char_length(data_source) <= 64),
    technology TEXT NOT NULL DEFAULT '' CHECK (char_length(technology) <= 128),
    attack_techniques TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(attack_techniques) <= 16),
    title TEXT NOT NULL DEFAULT '' CHECK (char_length(title) <= 300),
    body TEXT NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 65536),
    coverage TEXT NOT NULL DEFAULT '' CHECK (octet_length(coverage) <= 2048),
    validation TEXT NOT NULL DEFAULT 'unvalidated'
        CHECK (validation IN ('unvalidated','validated','noisy','no_hit','telemetry_gap','syntax_error','inconclusive')),
    positive_hits INTEGER CHECK (positive_hits IS NULL OR positive_hits >= 0),
    baseline_hits INTEGER CHECK (baseline_hits IS NULL OR baseline_hits >= 0),
    validation_detail JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(validation_detail::text) <= 65536),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_detection_rules_id_detection UNIQUE (id, detection_id),
    CONSTRAINT fk_detection_rules_run FOREIGN KEY (detection_id, finding_id)
        REFERENCES finding_detections(id, finding_id) ON DELETE CASCADE,
    CONSTRAINT fk_detection_rules_parent FOREIGN KEY (parent_id, detection_id)
        REFERENCES detection_rules(id, detection_id) ON DELETE CASCADE,
    CONSTRAINT ck_detection_rules_sigma_root CHECK (format <> 'sigma' OR parent_id IS NULL),
    CONSTRAINT ck_detection_rules_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id)
);
CREATE INDEX IF NOT EXISTS idx_detection_rules_detection ON detection_rules(detection_id, id);
CREATE INDEX IF NOT EXISTS idx_detection_rules_finding ON detection_rules(finding_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_detection_rules_parent ON detection_rules(parent_id) WHERE parent_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS detection_steps (
    id BIGSERIAL PRIMARY KEY,
    detection_id BIGINT NOT NULL REFERENCES finding_detections(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    step TEXT NOT NULL CHECK (step IN ('context','reconstruct','synthesise','compile','validate','repair','record')),
    attempt INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL CHECK (status IN ('ok','failed','skipped')),
    code TEXT NOT NULL DEFAULT '' CHECK (char_length(code) <= 64),
    call_id TEXT NOT NULL DEFAULT '' CHECK (char_length(call_id) <= 64),
    input_bytes INTEGER NOT NULL DEFAULT 0,
    output_bytes INTEGER NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    duration_ms INTEGER NOT NULL DEFAULT 0,
    CONSTRAINT uq_detection_steps_seq UNIQUE (detection_id, seq)
);

CREATE TABLE IF NOT EXISTS detection_query_results (
    id BIGSERIAL PRIMARY KEY,
    detection_id BIGINT NOT NULL REFERENCES finding_detections(id) ON DELETE CASCADE,
    body_sha256 TEXT NOT NULL CHECK (char_length(body_sha256) = 64),
    window_kind TEXT NOT NULL CHECK (window_kind IN ('attack','baseline_1d','baseline_7d','liveness')),
    state TEXT NOT NULL CHECK (state IN ('ok','error','rejected')),
    hits INTEGER CHECK (hits IS NULL OR hits >= 0),
    detail JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(detail::text) <= 65536),
    queried_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_detection_query_results UNIQUE (detection_id, body_sha256, window_kind)
);

CREATE OR REPLACE FUNCTION stop_deleted_conversation_detection() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE finding_detections
       SET status='stopped', end_code='conversation_deleted', error='detection session deleted', finished_at=now()
     WHERE conversation_id=OLD.id AND status IN ('pending','running');
    RETURN OLD;
END;
$$;
CREATE OR REPLACE TRIGGER trg_conversation_detection_delete BEFORE DELETE ON conversations
    FOR EACH ROW EXECUTE FUNCTION stop_deleted_conversation_detection();
