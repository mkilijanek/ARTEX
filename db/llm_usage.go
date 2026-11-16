package db

import (
	"encoding/json"
	"sort"
	"strconv"
	"time"
)

// LLMUsage is one lightweight LLM-call metering row — the always-on usage ledger,
// distinct from llm_records (which stores full request/response bodies and is a
// gated debug feature). One row per completion call, written on both success and
// error, so token accounting is complete even for interrupted/failed runs. Carries
// only the dimensions needed to slice token spend (model / profile / task / agent),
// never any prompt or response content.
type ToolBytes struct {
	Schema int `json:"schema"`
	Result int `json:"result"`
}

type LLMUsage struct {
	CallID    string               `json:"call_id"`
	Round     int                  `json:"round"`
	Purpose   string               `json:"purpose"`
	ToolBytes map[string]ToolBytes `json:"tool_bytes"` // catalog tool-name keys; no content or arguments

	Provider        string `json:"provider"`
	IntentID        int64  `json:"intent_id"`     // 0 = not an intent session
	Attempt         int    `json:"attempt"`       // 0-based runtime attempt, includes profile failover
	Reason          string `json:"reason"`        // completed | error | cancelled | consumer_stopped
	RequestBytes    int    `json:"request_bytes"` // provider-neutral JSON, not wire bytes or tokens
	SystemBytes     int    `json:"system_bytes"`  // raw UTF-8 across system segments
	MessageBytes    int    `json:"message_bytes"` // JSON; includes tool results
	ToolSchemaBytes int    `json:"tool_schema_bytes"`
	ToolResultBytes int    `json:"tool_result_bytes"` // JSON subset of MessageBytes
	SizeKnown       bool   `json:"size_known"`        // false if request serialization failed

	TaskID        string `json:"task_id"`        // task registry id (matches llm_records.task_id)
	ExplorationID int64  `json:"exploration_id"` // exploration id parsed from the session (0 = unknown/non-task)
	Worker        string `json:"worker"`         // agent lane: worker / planner / mainagent / goals
	Model         string `json:"model"`
	ProfileName   string `json:"profile_name"`
	LatencyMs     int    `json:"latency_ms"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	CacheRead     int    `json:"cache_read"`
	CacheWrite    int    `json:"cache_write"`
	Status        string `json:"status"` // ok | error
}

const llmUsageSchema = `
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
`

// EnsureLLMUsageTable creates the llm_usage metering table if it does not exist.
func (d *DB) EnsureLLMUsageTable() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(llmUsageSchema); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertLLMUsage appends one metering row. Best-effort: callers log and continue on
// error (a lost metering row must never break the LLM call).
func (d *DB) InsertLLMUsage(u *LLMUsage) error {
	var expID any
	if u.ExplorationID > 0 {
		expID = u.ExplorationID
	}
	toolBytes, err := json.Marshal(u.ToolBytes)
	if err != nil {
		return err
	}
	if u.ToolBytes == nil {
		toolBytes = []byte("{}")
	}
	_, err = d.Exec(`
INSERT INTO llm_usage(task_id, exploration_id, worker, model, profile_name, latency_ms, input_tokens, output_tokens, cache_read, cache_write, status, provider, intent_id, attempt, reason, request_bytes, system_bytes, message_bytes, tool_schema_bytes, tool_result_bytes, size_known, call_id, round_no, purpose, tool_bytes)
VALUES (NULLIF($1,''),$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25::jsonb)`,
		u.TaskID, expID, u.Worker, u.Model, u.ProfileName,
		u.LatencyMs, u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheWrite, u.Status, u.Provider, u.IntentID, u.Attempt, u.Reason, u.RequestBytes, u.SystemBytes, u.MessageBytes, u.ToolSchemaBytes, u.ToolResultBytes, u.SizeKnown, u.CallID, u.Round, u.Purpose, string(toolBytes))
	return err
}

// TokenByModel aggregates a task's LLM token usage grouped by model, most-used
// first, from the always-on llm_usage ledger. taskID is the task registry id.
// Accurate even with per-agent model bindings, pool rotation/failover, and
// interrupted runs, since every call (success or error) is metered.
func (d *DB) TokenByModel(taskID string) ([]ModelTokenStat, error) {
	rows, err := d.Query(`
SELECT COALESCE(NULLIF(model,''),'(unknown)') AS model, COUNT(*) AS calls,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
WHERE COALESCE(task_id,'') = $1
GROUP BY model
ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC, model`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelTokenStat{}
	for rows.Next() {
		var m ModelTokenStat
		if err := rows.Scan(&m.Model, &m.Calls, &m.InputTokens, &m.OutputTokens,
			&m.CacheReadTokens, &m.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProfileUsage aggregates the whole ledger's token spend for one LLM profile
// (global, all tasks). Powers the dashboard's per-profile token card (new source).
type ProfileUsage struct {
	ProfileName      string `json:"profile_name"`
	Calls            int    `json:"calls"`
	Tasks            int    `json:"tasks"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
	// CostEur is set only when the profile carries at least one price; nil = not priced.
	CostEur *float64 `json:"cost_eur,omitempty"`
}

// UsageByProfile returns global token spend grouped by profile name, most-used
// first. profile_name may be empty for calls made on env/non-persisted configs.
func (d *DB) UsageByProfile() ([]ProfileUsage, error) {
	rows, err := d.Query(`
SELECT COALESCE(profile_name,'') AS profile_name, COUNT(*) AS calls,
       COUNT(DISTINCT task_id) AS tasks,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
GROUP BY profile_name
ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileUsage{}
	for rows.Next() {
		var p ProfileUsage
		if err := rows.Scan(&p.ProfileName, &p.Calls, &p.Tasks,
			&p.InputTokens, &p.OutputTokens, &p.CacheReadTokens, &p.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]ProfileUsage, len(out))
	for _, current := range out {
		byName[current.ProfileName] = current
	}
	for _, aggregate := range archived {
		for _, cold := range aggregate.TokenProfiles {
			current := byName[cold.ProfileName]
			current.ProfileName = cold.ProfileName
			current.Calls += cold.Calls
			current.Tasks += cold.Tasks
			current.InputTokens += cold.InputTokens
			current.OutputTokens += cold.OutputTokens
			current.CacheReadTokens += cold.CacheReadTokens
			current.CacheWriteTokens += cold.CacheWriteTokens
			byName[cold.ProfileName] = current
		}
	}
	out = out[:0]
	for _, current := range byName {
		out = append(out, current)
	}
	if err := applyCostEur(d, out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		left := out[i].InputTokens + out[i].OutputTokens
		right := out[j].InputTokens + out[j].OutputTokens
		if left != right {
			return left > right
		}
		return out[i].ProfileName < out[j].ProfileName
	})
	return out, nil
}

// ProfileDayUsage is one (profile, UTC calendar day) token bucket for the daily
// chart. Unlike the activity-based chart, ts is the real call time, so this is
// actual per-day consumption rather than tokens bucketed by task creation date.
type ProfileDayUsage struct {
	ProfileName     string `json:"profile_name"`
	Date            string `json:"date"` // YYYY-MM-DD (UTC)
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheReadTokens int    `json:"cache_read_tokens"`
}

// UsageDaily returns per-(profile, day) token buckets for the past `days` days
// (default 365 when days<=0), so the dashboard can slice by profile + range.
func (d *DB) UsageDaily(days int) ([]ProfileDayUsage, error) {
	if days <= 0 {
		days = 365
	}
	rows, err := d.Query(`
SELECT COALESCE(profile_name,'') AS profile_name,
       to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read),0)
FROM llm_usage
WHERE ts >= now() - ($1 * interval '1 day')
GROUP BY profile_name, day
ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileDayUsage{}
	for rows.Next() {
		var p ProfileDayUsage
		if err := rows.Scan(&p.ProfileName, &p.Date, &p.InputTokens, &p.OutputTokens, &p.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	byKey := make(map[string]ProfileDayUsage, len(out))
	for _, current := range out {
		byKey[current.ProfileName+"\x00"+current.Date] = current
	}
	for _, aggregate := range archived {
		for _, cold := range aggregate.TokenDaily {
			if cold.Date < cutoff {
				continue
			}
			key := cold.ProfileName + "\x00" + cold.Date
			current := byKey[key]
			current.ProfileName = cold.ProfileName
			current.Date = cold.Date
			current.InputTokens += cold.InputTokens
			current.OutputTokens += cold.OutputTokens
			current.CacheReadTokens += cold.CacheReadTokens
			byKey[key] = current
		}
	}
	out = out[:0]
	for _, current := range byKey {
		out = append(out, current)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].ProfileName < out[j].ProfileName
	})
	return out, nil
}

// JudgeDayUsage is one UTC-day token bucket for the intercept fallback judge,
// for the config page's recent-spend sparkline.
type JudgeDayUsage struct {
	Date         string `json:"date"` // YYYY-MM-DD (UTC)
	Calls        int    `json:"calls"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// JudgeUsage is the cumulative token spend of the intercept fallback judge
// (worker='judge' rows in the always-on ledger), plus a recent daily series.
type JudgeUsage struct {
	Calls            int             `json:"calls"`
	InputTokens      int             `json:"input_tokens"`
	OutputTokens     int             `json:"output_tokens"`
	CacheReadTokens  int             `json:"cache_read_tokens"`
	CacheWriteTokens int             `json:"cache_write_tokens"`
	Daily            []JudgeDayUsage `json:"daily"`
}

// JudgeUsageStats returns the fallback judge's all-time token totals and a
// per-day series over the past `days` days (default 30). Sourced from the
// always-on llm_usage ledger, so it is accurate across pool rotation and
// interrupted/failed judge calls. days only bounds the daily series; totals are
// all-time.
func (d *DB) JudgeUsageStats(days int) (JudgeUsage, error) {
	if days <= 0 {
		days = 30
	}
	var u JudgeUsage
	err := d.QueryRow(`
SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
WHERE worker = 'judge'`).Scan(&u.Calls, &u.InputTokens, &u.OutputTokens,
		&u.CacheReadTokens, &u.CacheWriteTokens)
	if err != nil {
		return JudgeUsage{}, err
	}
	rows, err := d.Query(`
SELECT to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
       COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
FROM llm_usage
WHERE worker = 'judge' AND ts >= now() - ($1 * interval '1 day')
GROUP BY day
ORDER BY day`, days)
	if err != nil {
		return JudgeUsage{}, err
	}
	defer rows.Close()
	u.Daily = []JudgeDayUsage{}
	for rows.Next() {
		var day JudgeDayUsage
		if err := rows.Scan(&day.Date, &day.Calls, &day.InputTokens, &day.OutputTokens); err != nil {
			return JudgeUsage{}, err
		}
		u.Daily = append(u.Daily, day)
	}
	if err := rows.Err(); err != nil {
		return JudgeUsage{}, err
	}
	return u, nil
}

// ParseExpID turns the exploration-id segment parsed from a session string into an
// int64 (0 when empty/non-numeric, e.g. chat sessions keyed by conversation id).
func ParseExpID(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// applyCostEur fills CostEur on each profile bucket from the stored per-profile
// prices (EUR per 1M tokens). Profiles without any price keep CostEur nil. The
// join is by profile name (llm_usage records the name, not the id); the first
// matching profile wins, and duplicate names are a configuration error the
// operator sees as a weird number, not a crash.
func applyCostEur(d *DB, out []ProfileUsage) error {
	rows, err := d.Query(`SELECT name, COALESCE(price_input_per_m,0), COALESCE(price_output_per_m,0), COALESCE(price_cache_read_per_m,0), COALESCE(price_cache_write_per_m,0) FROM llm_profiles`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type prices struct{ in, outp, cr, cw float64 }
	byName := map[string]prices{}
	seen := map[string]bool{}
	for rows.Next() {
		var n string
		var pr prices
		if err := rows.Scan(&n, &pr.in, &pr.outp, &pr.cr, &pr.cw); err != nil {
			return err
		}
		if !seen[n] {
			byName[n] = pr
			seen[n] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range out {
		pr, ok := byName[out[i].ProfileName]
		if !ok || (pr.in == 0 && pr.outp == 0 && pr.cr == 0 && pr.cw == 0) {
			continue
		}
		// InputTokens includes both cached subsets (Anthropic and OpenAI usage
		// semantics), so the fresh share is input minus cache reads and writes; charging
		// both buckets at full input price double-counts the cache. Clamped at
		// zero for gateways that report the buckets disjointly.
		fresh := out[i].InputTokens - out[i].CacheReadTokens - out[i].CacheWriteTokens
		if fresh < 0 {
			fresh = 0
		}
		cost := float64(fresh)/1e6*pr.in + float64(out[i].OutputTokens)/1e6*pr.outp +
			float64(out[i].CacheReadTokens)/1e6*pr.cr + float64(out[i].CacheWriteTokens)/1e6*pr.cw
		c := cost
		out[i].CostEur = &c
	}
	return nil
}

// UsageDimensions returns a bounded, cursor-paged physical-attempt ledger. CallID
// groups router/pool attempts; round is a per-session-instance invocation ordinal.
func (d *DB) UsageDimensions(task, purpose string, after int64, limit int) ([]UsageDimension, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := d.Query(`SELECT id, call_id, purpose, round_no, attempt, COALESCE(task_id,''),
 COALESCE(profile_name,''), tool_bytes, input_tokens, output_tokens, cache_read, cache_write,
 COALESCE(status,''), reason FROM llm_usage
 WHERE id > $1 AND ($2 = '' OR task_id = $2) AND ($3 = '' OR purpose = $3)
 ORDER BY id LIMIT $4`, after, task, purpose, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageDimension{}
	for rows.Next() {
		var row UsageDimension
		var tools []byte
		if err := rows.Scan(&row.ID, &row.CallID, &row.Purpose, &row.Round, &row.Attempt,
			&row.TaskID, &row.ProfileName, &tools, &row.InputTokens, &row.OutputTokens,
			&row.CacheRead, &row.CacheWrite, &row.Status, &row.Reason); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(tools, &row.ToolBytes); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UsageDimension contains only columns actually selected by UsageDimensions.
// Unselected counters must never appear as measured zeros in the response.
type UsageDimension struct {
 ID int64 `json:"id"`
 CallID string `json:"call_id"`
 Purpose string `json:"purpose"`
 Round int `json:"round"`
 Attempt int `json:"attempt"`
 TaskID string `json:"task_id"`
 ProfileName string `json:"profile_name"`
 ToolBytes map[string]ToolBytes `json:"tool_bytes"`
 InputTokens int `json:"input_tokens"`
 OutputTokens int `json:"output_tokens"`
 CacheRead int `json:"cache_read"`
 CacheWrite int `json:"cache_write"`
 Status string `json:"status"`
 Reason string `json:"reason"`
}
