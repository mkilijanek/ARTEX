package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// The database layer of detection engineering (docs/detector-plan.md sections 5 and 6.8): runs, rules, steps, the
// queue, recording, recovery, the deadline sweep, query accounting, lists and delete. The tables are created by
// migration 5003. Nothing here is reachable from a query that agent tools use (section 11): the finding queries
// and DBFinding know nothing of these tables.

// FindingDetectionAgentKey is the agent key of a detector conversation (path 'agent'). The agent row is seeded by a
// later pull request; the key is only stored here.
const FindingDetectionAgentKey = "detector"

// Limits kept by code, in the transaction that holds the lock (section 5), and the byte budget of the snapshot.
const (
	MaxDetectionRunsPerFinding = 100
	MaxDetectionRulesPerRun    = 24
	// DetectionSnapshotBudget is the budget of a stored snapshot, measured as jsonb::text by the database; the
	// column's own check allows 1 MiB.
	DetectionSnapshotBudget = 768 << 10
	// DetectionSweepDeadline is the age of claimed_at after which the sweep fails an engine run (the run timeout
	// is 10 or 15 minutes, so a live worker never reaches it).
	DetectionSweepDeadline = 20 * time.Minute

	detectionSnapshotCap     = 1 << 20
	detectionManifestCap     = 65536
	detectionStageReportCap  = 16384
	detectionDetailCap       = 65536
	detectionKeepFullSnaps   = 5
	maxDetectionNotesChars   = 4000
	maxDetectionAutoAttempts = 10
)

var (
	// ErrDetectionNotRunning is returned by every write for a run that was stopped, finished or deleted (or never
	// claimed): the worker ends quietly.
	ErrDetectionNotRunning = errors.New("this detection run has ended or has not started; please start a new run from the finding details")
	// ErrDetectionNotFound is returned for a run that does not exist or was deleted (a tombstone is not found).
	ErrDetectionNotFound = errors.New("detection run does not exist")
	// ErrDetectionArchiveActive is returned by a start for a finding whose task has an archive job that is queued or in
	// progress (section 6.10): the purge would delete the finding under the run.
	ErrDetectionArchiveActive = errors.New("this finding's task is being archived; a detection run cannot start until the archive job ends")
	// ErrDetectionActive is returned when an active run is deleted.
	ErrDetectionActive = errors.New("this detection run is still active; stop it before deleting it")
	// ErrDetectionRunLimit is returned by a create when the finding holds the maximum number of runs.
	ErrDetectionRunLimit = fmt.Errorf("this finding already has %d detection runs; delete some of them from its history to start another", MaxDetectionRunsPerFinding)
	// ErrDetectionRuleLimit is returned when a run would hold more than MaxDetectionRulesPerRun rules.
	ErrDetectionRuleLimit = fmt.Errorf("a detection run holds at most %d rules", MaxDetectionRulesPerRun)
	// ErrDetectionQueryCap is returned by IncrementDetectionQueries when the run used its queries up.
	ErrDetectionQueryCap = errors.New("the query limit of this detection run is used up")
	// ErrDetectionAutoRunExists is returned by a create of an automatic run when the attempt number already
	// exists for the finding (a deleted run keeps it as a tombstone) or an automatic run of the finding is
	// active or completed.
	ErrDetectionAutoRunExists = errors.New("an automatic detection run of this attempt already exists, or one is active or completed for this finding")
)

// DetectionLimitError names the field that is over its limit. It never carries the value.
type DetectionLimitError struct {
	Field string
	Limit int
	Unit  string // "characters" or "bytes"
}

func (e *DetectionLimitError) Error() string {
	return fmt.Sprintf("detection %s is too large: at most %d %s", e.Field, e.Limit, e.Unit)
}

// Enumerations of the checks of migration 5003, so a wrong value is refused with a clear message before the
// database refuses it (the checks stay as the backstop for direct writes).
var (
	detectionSteps    = map[string]bool{"context": true, "reconstruct": true, "synthesise": true, "compile": true, "validate": true, "repair": true, "record": true}
	detectionStepStat = map[string]bool{"ok": true, "failed": true, "skipped": true}
	detectionStages   = map[string]bool{"attempt": true, "success": true, "post_exploitation": true, "agent_behavior": true}
	detectionFormats  = map[string]bool{"sigma": true, "kql": true, "suricata": true, "waf_imperva": true, "waf_f5": true, "yara": true}
	detectionValids   = map[string]bool{"unvalidated": true, "validated": true, "noisy": true, "no_hit": true, "telemetry_gap": true, "syntax_error": true, "inconclusive": true}
	detectionOutcomes = map[string]bool{"": true, "validated": true, "partial": true, "telemetry_gap": true, "unvalidated": true, "no_rules": true}
	detectionWindows  = map[string]bool{"attack": true, "baseline_1d": true, "baseline_7d": true, "liveness": true}
	detectionQStates  = map[string]bool{"ok": true, "error": true, "rejected": true}
)

// FindingDetection is one detection run. The snapshot is never part of it: lists and single runs name their
// columns, and only ClaimFindingDetection and FindingDetectionSnapshot read it.
type FindingDetection struct {
	ID               int64           `json:"id"`
	FindingID        int64           `json:"finding_id"`
	ConversationID   *int64          `json:"conversation_id"`
	Path             string          `json:"path"`
	TriggerKind      string          `json:"trigger_kind"`
	AutoAttempt      int             `json:"auto_attempt"`
	StartedByKind    string          `json:"started_by_kind"`
	StartedBySubject string          `json:"started_by_subject"`
	Status           string          `json:"status"`
	Phase            string          `json:"phase"`
	NotBefore        *time.Time      `json:"not_before"`
	ClaimedAt        *time.Time      `json:"claimed_at"`
	Attempts         int             `json:"attempts"`
	QueriesUsed      int             `json:"queries_used"`
	Outcome          string          `json:"outcome"`
	EndCode          string          `json:"end_code"`
	Notes            string          `json:"notes"`
	Summary          string          `json:"summary"`
	Error            string          `json:"error"`
	CreatedAt        time.Time       `json:"created_at"`
	StartedAt        *time.Time      `json:"started_at"`
	FinishedAt       *time.Time      `json:"finished_at"`
	DeletedAt        *time.Time      `json:"-"`
	InputManifest    json.RawMessage `json:"input_manifest,omitempty"` // only on a single run (GetFindingDetection)
	StageReport      json.RawMessage `json:"stage_report,omitempty"`   // only on a single run
}

// detectionListCols is the column list of every run query. It names its columns and has no snapshot, no input
// manifest and no stage report: a snapshot is up to 768 KiB and a history can hold 100 runs.
const detectionListCols = `id, finding_id, conversation_id, path, trigger_kind, auto_attempt, started_by_kind, started_by_subject,
	status, phase, not_before, claimed_at, attempts, queries_used, outcome, end_code, notes, summary, error,
	created_at, started_at, finished_at, deleted_at`

func scanDetection(row interface{ Scan(...any) error }) (*FindingDetection, error) {
	r := &FindingDetection{}
	err := row.Scan(&r.ID, &r.FindingID, &r.ConversationID, &r.Path, &r.TriggerKind, &r.AutoAttempt, &r.StartedByKind, &r.StartedBySubject,
		&r.Status, &r.Phase, &r.NotBefore, &r.ClaimedAt, &r.Attempts, &r.QueriesUsed, &r.Outcome, &r.EndCode, &r.Notes, &r.Summary, &r.Error,
		&r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// DetectionRule is one rule of a run.
type DetectionRule struct {
	ID               int64           `json:"id"`
	DetectionID      int64           `json:"detection_id"`
	FindingID        int64           `json:"finding_id"`
	ParentID         *int64          `json:"parent_id"`
	Stage            string          `json:"stage"`
	Format           string          `json:"format"`
	DataSource       string          `json:"data_source"`
	Technology       string          `json:"technology"`
	AttackTechniques []string        `json:"attack_techniques"`
	Title            string          `json:"title"`
	Body             string          `json:"body"`
	Coverage         string          `json:"coverage"`
	Validation       string          `json:"validation"`
	PositiveHits     *int            `json:"positive_hits"`
	BaselineHits     *int            `json:"baseline_hits"`
	ValidationDetail json.RawMessage `json:"validation_detail"`
	CreatedAt        time.Time       `json:"created_at"`
}

const detectionRuleCols = `id, detection_id, finding_id, parent_id, stage, format, data_source, technology, to_jsonb(attack_techniques),
	title, body, coverage, validation, positive_hits, baseline_hits, validation_detail, created_at`

func scanDetectionRule(row interface{ Scan(...any) error }) (*DetectionRule, error) {
	r := &DetectionRule{}
	var techniques []byte
	if err := row.Scan(&r.ID, &r.DetectionID, &r.FindingID, &r.ParentID, &r.Stage, &r.Format, &r.DataSource, &r.Technology, &techniques,
		&r.Title, &r.Body, &r.Coverage, &r.Validation, &r.PositiveHits, &r.BaselineHits, &r.ValidationDetail, &r.CreatedAt); err != nil {
		return nil, err
	}
	r.AttackTechniques = []string{}
	if err := json.Unmarshal(techniques, &r.AttackTechniques); err != nil {
		return nil, fmt.Errorf("decode attack techniques: %w", err)
	}
	return r, nil
}

// DetectionStep is one line of the run log: codes and sizes only, never target, model or rule text.
type DetectionStep struct {
	ID          int64     `json:"id"`
	DetectionID int64     `json:"detection_id"`
	Seq         int       `json:"seq"`
	Step        string    `json:"step"`
	Attempt     int       `json:"attempt"`
	Status      string    `json:"status"`
	Code        string    `json:"code"`
	CallID      string    `json:"call_id"`
	InputBytes  int       `json:"input_bytes"`
	OutputBytes int       `json:"output_bytes"`
	StartedAt   time.Time `json:"started_at"`
	DurationMS  int       `json:"duration_ms"`
}

// DetectionQueryResult is the result of one workspace query, keyed by (run, sha256 of the rule text, window).
type DetectionQueryResult struct {
	BodySHA256 string          `json:"body_sha256"`
	WindowKind string          `json:"window_kind"`
	State      string          `json:"state"`
	Hits       *int            `json:"hits"`
	Detail     json.RawMessage `json:"detail"`
	QueriedAt  time.Time       `json:"queried_at"`
}

// DetectionRuleInput is what a rule is recorded with. There is no run and no finding in it: the run is the
// parameter of the recording function and the finding is taken from the run (rule 4 of the specification).
type DetectionRuleInput struct {
	ParentID         *int64 // a child rule (a kql rule compiled from a sigma rule) of the same run
	Stage            string
	Format           string
	DataSource       string
	Technology       string
	AttackTechniques []string
	Title            string
	Body             string
	Coverage         string
	Validation       string // "" means unvalidated
	PositiveHits     *int
	BaselineHits     *int
	ValidationDetail json.RawMessage // nil means {}
}

// CreateDetectionInput starts a run. Trigger "" is manual; an automatic run carries its attempt number (1 to 10).
type CreateDetectionInput struct {
	FindingID        int64
	Path             string // "engine" or "agent"
	Notes            string
	TriggerKind      string
	AutoAttempt      int
	StartedByKind    string // "" means operator
	StartedBySubject string
}

func limitChars(field, s string, max int) error {
	if utf8.RuneCountInString(s) > max {
		return &DetectionLimitError{Field: field, Limit: max, Unit: "characters"}
	}
	return nil
}

func limitOctets(field, s string, max int) error {
	if len(s) > max {
		return &DetectionLimitError{Field: field, Limit: max, Unit: "bytes"}
	}
	return nil
}

func validDetectionPath(p string) bool { return p == "engine" || p == "agent" }

// cutUTF8 cuts s to at most max bytes at a character boundary and reports whether it cut.
func cutUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	if max < 0 {
		max = 0
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

type detectionQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// jsonFits asks the database whether the JSON text, as jsonb::text, is within the cap. It is used where the
// size is needed before the write (the snapshot shrink loop); the writes themselves carry the same expression
// in their WHERE.
func jsonFits(ctx context.Context, q detectionQuerier, raw []byte, limit int) (int, bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT octet_length($1::jsonb::text)`, string(raw)).Scan(&n); err != nil {
		return 0, false, err
	}
	return n, n <= limit, nil
}

func jsonOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// ---- create ---------------------------------------------------------------------------------------------------

// CreateFindingDetection starts a run for a finding. In one transaction that locks the finding row it returns the
// active run of either path when there is one (created=false), applies the limits, builds the snapshot to a byte
// budget, inserts the run and, for the agent path, creates the conversation and its first message. The returned
// conversation is nil for the engine path and when an active run was returned.
func (d *DB) CreateFindingDetection(ctx context.Context, in CreateDetectionInput) (*FindingDetection, *Conversation, bool, error) {
	if !validDetectionPath(in.Path) {
		return nil, nil, false, fmt.Errorf("detection path must be engine or agent")
	}
	notes := strings.TrimSpace(in.Notes)
	if err := limitChars("notes", notes, maxDetectionNotesChars); err != nil {
		return nil, nil, false, err
	}
	if err := limitChars("started_by_subject", in.StartedBySubject, 200); err != nil {
		return nil, nil, false, err
	}
	trigger := in.TriggerKind
	if trigger == "" {
		trigger = "manual"
	}
	switch trigger {
	case "manual":
		if in.AutoAttempt != 0 {
			return nil, nil, false, errors.New("a manual detection run has no automatic attempt number")
		}
	case "auto":
		if in.AutoAttempt < 1 || in.AutoAttempt > maxDetectionAutoAttempts {
			return nil, nil, false, fmt.Errorf("an automatic detection attempt is numbered 1 to %d", maxDetectionAutoAttempts)
		}
	default:
		return nil, nil, false, errors.New("detection trigger must be manual or auto")
	}
	byKind := in.StartedByKind
	if byKind == "" {
		byKind = "operator"
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, err
	}
	defer tx.Rollback()

	f, err := lockDetectionFinding(ctx, tx, in.FindingID)
	if err != nil {
		return nil, nil, false, err
	}
	// The finding row is held: a purge that got the findings first has made us wait and find no finding, one that
	// comes later sees the run we make. An archive job that is already queued or running must not get a new run.
	var archiving bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM task_archives
		WHERE task_id=$1 AND state IN ('archive_queued','archiving'))`, f.TaskID).Scan(&archiving); err != nil {
		return nil, nil, false, fmt.Errorf("check the task's archive job: %w", err)
	}
	if archiving {
		return nil, nil, false, ErrDetectionArchiveActive
	}
	active, err := scanDetection(tx.QueryRowContext(ctx, `SELECT `+detectionListCols+` FROM finding_detections
		WHERE finding_id=$1 AND status IN ('pending','running')`, in.FindingID))
	if err != nil {
		return nil, nil, false, err
	}
	if active != nil {
		return active, nil, false, nil
	}
	var runs int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM finding_detections WHERE finding_id=$1 AND deleted_at IS NULL`, in.FindingID).Scan(&runs); err != nil {
		return nil, nil, false, err
	}
	if runs >= MaxDetectionRunsPerFinding {
		return nil, nil, false, ErrDetectionRunLimit
	}

	snapshot, err := buildDetectionSnapshot(ctx, tx, f)
	if err != nil {
		return nil, nil, false, fmt.Errorf("build detection snapshot: %w", err)
	}

	var conv *Conversation
	var convID *int64
	if in.Path == "agent" {
		title := detectionConversationTitle(f)
		c, err := scanConv(tx.QueryRowContext(ctx, `INSERT INTO conversations(agent_key,title) VALUES ($1,$2) RETURNING `+convCols,
			FindingDetectionAgentKey, title))
		if err != nil {
			return nil, nil, false, err
		}
		conv, convID = &c, &c.ID
	}

	run, err := insertDetectionRun(ctx, tx, detectionRunRow{
		FindingID: in.FindingID, ConversationID: convID, Path: in.Path, Trigger: trigger, AutoAttempt: in.AutoAttempt,
		StartedByKind: byKind, StartedBySubject: in.StartedBySubject, Notes: notes,
	}, snapshot)
	if err != nil {
		return nil, nil, false, err
	}

	// Only the newest five runs of a finding keep their snapshot and manifest; the rows and the rules stay.
	if _, err := tx.ExecContext(ctx, `UPDATE finding_detections SET snapshot='{}', input_manifest='{}'
		WHERE finding_id=$1 AND deleted_at IS NULL AND status IN ('completed','failed','stopped')
		  AND (snapshot <> '{}'::jsonb OR input_manifest <> '{}'::jsonb)
		  AND id NOT IN (SELECT id FROM finding_detections WHERE finding_id=$1 AND deleted_at IS NULL ORDER BY id DESC LIMIT $2)`,
		in.FindingID, detectionKeepFullSnaps); err != nil {
		return nil, nil, false, fmt.Errorf("compact old detection snapshots: %w", err)
	}

	if conv != nil {
		msg := detectionInitialMessage(in.FindingID, notes)
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_activities(conversation_id,worker,kind,summary,detail) VALUES ($1,$2,'user',$3,$4)`,
			conv.ID, FindingDetectionAgentKey, fmt.Sprintf("Please write detection rules for vulnerability #%d", in.FindingID), msg); err != nil {
			return nil, nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, false, err
	}
	return run, conv, true, nil
}

// detectionRunRow is the creator-chosen part of a new run row.
type detectionRunRow struct {
	FindingID        int64
	ConversationID   *int64
	Path             string
	Trigger          string
	AutoAttempt      int
	StartedByKind    string
	StartedBySubject string
	Notes            string
}

// insertDetectionRun inserts the row of a new run. The snapshot size is measured by the database in the same
// statement (the check of the column stays as the backstop); a snapshot over the cap is a DetectionLimitError.
func insertDetectionRun(ctx context.Context, tx *sql.Tx, r detectionRunRow, snapshot []byte) (*FindingDetection, error) {
	run, err := scanDetection(tx.QueryRowContext(ctx, `INSERT INTO finding_detections
		(finding_id, conversation_id, path, trigger_kind, auto_attempt, started_by_kind, started_by_subject, notes, snapshot)
		SELECT $1::bigint, $2::bigint, $3::text, $4::text, $5::integer, $6::text, $7::text, $8::text, $9::jsonb
		WHERE octet_length($9::jsonb::text) <= `+fmt.Sprint(detectionSnapshotCap)+`
		RETURNING `+detectionListCols,
		r.FindingID, r.ConversationID, r.Path, r.Trigger, r.AutoAttempt, r.StartedByKind, r.StartedBySubject, r.Notes, string(snapshot)))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			(pgErr.ConstraintName == "idx_finding_detections_auto_attempt" || pgErr.ConstraintName == "idx_finding_detections_auto_done") {
			return nil, ErrDetectionAutoRunExists
		}
		return nil, fmt.Errorf("insert detection run: %w", err)
	}
	if run == nil {
		return nil, &DetectionLimitError{Field: "snapshot", Limit: detectionSnapshotCap, Unit: "bytes"}
	}
	return run, nil
}

func detectionConversationTitle(f *detectionFindingRow) string {
	name := f.Name
	if name == "" {
		name = f.VulnClass
	}
	if name == "" {
		name = "Uncategorized"
	}
	title := []rune(name)
	if len(title) > 100 {
		title = title[:100]
	}
	return fmt.Sprintf("Detection #%d · %s", f.ID, string(title))
}

func detectionInitialMessage(findingID int64, notes string) string {
	msg := fmt.Sprintf("Please write detection rules for vulnerability #%d.", findingID)
	if notes != "" {
		msg += "\n\nAdditional notes for this run:\n" + notes
	}
	return msg
}

// ---- snapshot -------------------------------------------------------------------------------------------------

// Caps of the snapshot, in bytes of UTF-8 (section 6.8).
const (
	snapSummaryCap     = 8 << 10
	snapEvidenceCap    = 64 << 10
	snapReportCap      = 64 << 10
	snapIdentCap       = 1 << 10 // name, vulnclass, severity, status, worker: not capped by the plan's list, bounded here
	snapAssetsMax      = 50
	snapURLCap         = 2 << 10
	snapTechMax        = 50
	snapTechCap        = 128
	snapServiceCap     = 256
	snapExtraCap       = 4 << 10
	snapConstraintsCap = 16 << 10
	snapOriginCap      = 256
	snapTrafficMax     = 200
	snapMethodCap      = 64
)

type detectionFindingRow struct {
	ID              int64     `json:"id"`
	TaskID          *int64    `json:"task_id"`
	NodeID          *int64    `json:"node_id"`
	VulnClass       string    `json:"vulnclass"`
	Name            string    `json:"name"`
	Severity        string    `json:"severity"`
	Status          string    `json:"status"`
	Worker          string    `json:"worker"`
	CreatedAt       time.Time `json:"created_at"`
	EvidenceVersion int64     `json:"evidence_version"`
	Summary         string    `json:"summary"`
	Evidence        string    `json:"evidence"`
	Report          string    `json:"report"`
}

type snapAsset struct {
	ID           int64    `json:"id"`
	URL          string   `json:"url"`
	Technologies []string `json:"technologies"`
	ServiceName  string   `json:"service_name"`
	ServiceType  string   `json:"service_type"`
	Extra        string   `json:"extra"`
}

type snapConstraint struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Origin string `json:"origin"`
}

type snapTraffic struct {
	BindingID  int64  `json:"binding_id"`
	SnapshotID string `json:"snapshot_id"`
	Role       string `json:"role"`
	Position   int    `json:"position"`
	CapturedAt int64  `json:"captured_at"`
	Method     string `json:"method"`
	URL        string `json:"url"`
	Status     int    `json:"status"`
	ReqLen     int64  `json:"req_len"`
	RespLen    int64  `json:"resp_len"`
	ReqHash    string `json:"req_hash"`
	RespHash   string `json:"resp_hash"`
}

// detectionSnapshot is the stored shape: named fields only, no ticket_id, no whole-row serialisation.
type detectionSnapshot struct {
	Finding      *detectionFindingRow `json:"finding"`
	Assets       []snapAsset          `json:"assets"`
	Constraints  []snapConstraint     `json:"constraints"`
	Traffic      []snapTraffic        `json:"traffic"`
	TrafficTotal int                  `json:"traffic_total"`
	Truncated    []string             `json:"truncated"`
}

func (s *detectionSnapshot) flag(name string) {
	for _, have := range s.Truncated {
		if have == name {
			return
		}
	}
	s.Truncated = append(s.Truncated, name)
}

// cut cuts *p to max bytes at a character boundary and flags name when it cut.
func (s *detectionSnapshot) cut(p *string, max int, name string) {
	if v, did := cutUTF8(*p, max); did {
		*p = v
		s.flag(name)
	}
}

func lockDetectionFinding(ctx context.Context, tx *sql.Tx, findingID int64) (*detectionFindingRow, error) {
	f := &detectionFindingRow{}
	// left() bounds what is read into memory; the cut to the byte cap is made below.
	err := tx.QueryRowContext(ctx, `SELECT f.id, f.task_id, f.node_id, left(f.vulnclass, $2::int), left(f.name, $2::int), left(f.severity, $2::int),
		left(f.status, $2::int), left(f.worker, $2::int), f.created_at, f.evidence_version,
		left(f.summary, $3::int), left(f.evidence, $4::int), left(f.report, $5::int)
		FROM findings f WHERE f.id=$1 FOR UPDATE OF f`,
		findingID, snapIdentCap+1, snapSummaryCap+1, snapEvidenceCap+1, snapReportCap+1).
		Scan(&f.ID, &f.TaskID, &f.NodeID, &f.VulnClass, &f.Name, &f.Severity, &f.Status, &f.Worker, &f.CreatedAt, &f.EvidenceVersion,
			&f.Summary, &f.Evidence, &f.Report)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFindingNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// buildDetectionSnapshot assembles the snapshot in Go and shrinks it, in the order of section 6.8, until the
// database measures it inside DetectionSnapshotBudget. It must run in the transaction that holds the finding.
func buildDetectionSnapshot(ctx context.Context, tx *sql.Tx, f *detectionFindingRow) ([]byte, error) {
	s := &detectionSnapshot{Finding: f, Assets: []snapAsset{}, Constraints: []snapConstraint{}, Traffic: []snapTraffic{}, Truncated: []string{}}
	s.cut(&f.VulnClass, snapIdentCap, "finding.vulnclass")
	s.cut(&f.Name, snapIdentCap, "finding.name")
	s.cut(&f.Severity, snapIdentCap, "finding.severity")
	s.cut(&f.Status, snapIdentCap, "finding.status")
	s.cut(&f.Worker, snapIdentCap, "finding.worker")
	s.cut(&f.Summary, snapSummaryCap, "finding.summary")
	s.cut(&f.Evidence, snapEvidenceCap, "finding.evidence")
	s.cut(&f.Report, snapReportCap, "finding.report")

	if err := loadSnapAssets(ctx, tx, s); err != nil {
		return nil, err
	}
	if f.TaskID != nil {
		if err := loadSnapConstraints(ctx, tx, s, *f.TaskID); err != nil {
			return nil, err
		}
	}
	if err := loadSnapTraffic(ctx, tx, s); err != nil {
		return nil, err
	}

	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	size, fits, err := jsonFits(ctx, tx, raw, DetectionSnapshotBudget)
	if err != nil {
		return nil, err
	}
	for _, st := range snapshotShrinkSteps {
		for !fits && st.apply(s) {
			s.flag(st.name)
			if raw, err = json.Marshal(s); err != nil {
				return nil, err
			}
			if size, fits, err = jsonFits(ctx, tx, raw, DetectionSnapshotBudget); err != nil {
				return nil, err
			}
		}
		if fits {
			break
		}
	}
	if !fits {
		return nil, fmt.Errorf("snapshot is %d bytes after every shrink step, over the budget of %d", size, DetectionSnapshotBudget)
	}
	return raw, nil
}

func loadSnapAssets(ctx context.Context, tx *sql.Tx, s *detectionSnapshot) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.id, COALESCE(left(a.url, $2::int),''), to_jsonb(COALESCE(a.technologies,'{}')),
		COALESCE(left(a.service_name, $3::int),''), COALESCE(a.service_type,''), COALESCE(left(a.extra::text, $4::int),'')
		FROM assets a
		WHERE a.id IN (SELECT e::text::bigint FROM jsonb_array_elements((SELECT asset_ids FROM findings WHERE id=$1)) e
		               WHERE jsonb_typeof(e)='number' AND e::text ~ '^[0-9]{1,18}$')
		ORDER BY a.id LIMIT $5::int`,
		s.Finding.ID, snapURLCap+1, snapServiceCap+1, snapExtraCap+1, snapAssetsMax+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a snapAsset
		var techs []byte
		if err := rows.Scan(&a.ID, &a.URL, &techs, &a.ServiceName, &a.ServiceType, &a.Extra); err != nil {
			return err
		}
		if err := json.Unmarshal(techs, &a.Technologies); err != nil {
			return fmt.Errorf("decode asset technologies: %w", err)
		}
		if len(s.Assets) == snapAssetsMax {
			s.flag("assets")
			continue
		}
		s.cut(&a.URL, snapURLCap, "assets.url")
		s.cut(&a.ServiceName, snapServiceCap, "assets.service_name")
		s.cut(&a.Extra, snapExtraCap, "assets.extra")
		if len(a.Technologies) > snapTechMax {
			a.Technologies = a.Technologies[:snapTechMax]
			s.flag("assets.technologies")
		}
		for i := range a.Technologies {
			s.cut(&a.Technologies[i], snapTechCap, "assets.technologies")
		}
		s.Assets = append(s.Assets, a)
	}
	return rows.Err()
}

func loadSnapConstraints(ctx context.Context, tx *sql.Tx, s *detectionSnapshot, taskID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT c.id, c.kind, left(c.text, $2::int), COALESCE(left(c.origin, $3::int),'')
		FROM task_constraints c JOIN tasks t ON t.exploration_id=c.exploration_id WHERE t.id=$1 ORDER BY c.id`,
		taskID, snapConstraintsCap+1, snapOriginCap+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	used := 0
	for rows.Next() {
		var c snapConstraint
		if err := rows.Scan(&c.ID, &c.Kind, &c.Text, &c.Origin); err != nil {
			return err
		}
		s.cut(&c.Origin, snapOriginCap, "constraints")
		room := snapConstraintsCap - used - len(c.Kind) - len(c.Origin)
		if room <= 0 {
			s.flag("constraints")
			continue
		}
		s.cut(&c.Text, room, "constraints")
		used += len(c.Kind) + len(c.Origin) + len(c.Text)
		s.Constraints = append(s.Constraints, c)
	}
	return rows.Err()
}

func loadSnapTraffic(ctx context.Context, tx *sql.Tx, s *detectionSnapshot) error {
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM finding_traffic_bindings WHERE finding_id=$1`, s.Finding.ID).Scan(&s.TrafficTotal); err != nil {
		return err
	}
	// Selection order: proof, verification, supporting, baseline, then position and id, so a cut from the end
	// keeps the proof.
	rows, err := tx.QueryContext(ctx, `SELECT b.id, b.snapshot_id, b.role, b.position, e.captured_at, left(e.method, $2::int), left(e.url, $3::int),
		e.status, e.req_len, e.resp_len, e.req_hash, e.resp_hash
		FROM finding_traffic_bindings b JOIN traffic_evidence_snapshots e ON e.id=b.snapshot_id WHERE b.finding_id=$1
		ORDER BY CASE b.role WHEN 'proof' THEN 0 WHEN 'verification' THEN 1 WHEN 'supporting' THEN 2 ELSE 3 END, b.position, b.id
		LIMIT $4::int`,
		s.Finding.ID, snapMethodCap+1, snapURLCap+1, snapTrafficMax)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t snapTraffic
		if err := rows.Scan(&t.BindingID, &t.SnapshotID, &t.Role, &t.Position, &t.CapturedAt, &t.Method, &t.URL, &t.Status, &t.ReqLen, &t.RespLen, &t.ReqHash, &t.RespHash); err != nil {
			return err
		}
		s.cut(&t.Method, snapMethodCap, "traffic.method")
		s.cut(&t.URL, snapURLCap, "traffic.url")
		s.Traffic = append(s.Traffic, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if s.TrafficTotal > len(s.Traffic) {
		s.flag("traffic")
	}
	return nil
}

// snapshotShrinkSteps are applied in this order while the snapshot is over the budget; a step that changes
// nothing is skipped and not named. A step that can repeat (the halving) is applied until it changes nothing.
var snapshotShrinkSteps = []struct {
	name  string
	apply func(*detectionSnapshot) bool
}{
	{"shrink:traffic_100", func(s *detectionSnapshot) bool { return cutTraffic(s, 100) }},
	{"shrink:traffic_50", func(s *detectionSnapshot) bool { return cutTraffic(s, 50) }},
	{"shrink:traffic_12", func(s *detectionSnapshot) bool { return cutTraffic(s, 12) }},
	{"shrink:assets_extra_removed", func(s *detectionSnapshot) bool {
		changed := false
		for i := range s.Assets {
			if s.Assets[i].Extra != "" {
				s.Assets[i].Extra, changed = "", true
			}
		}
		return changed
	}},
	{"shrink:assets_10", func(s *detectionSnapshot) bool {
		if len(s.Assets) > 10 {
			s.Assets = s.Assets[:10]
			return true
		}
		return false
	}},
	{"shrink:technologies_10", func(s *detectionSnapshot) bool {
		changed := false
		for i := range s.Assets {
			if len(s.Assets[i].Technologies) > 10 {
				s.Assets[i].Technologies, changed = s.Assets[i].Technologies[:10], true
			}
		}
		return changed
	}},
	{"shrink:constraints_removed", func(s *detectionSnapshot) bool {
		if len(s.Constraints) > 0 {
			s.Constraints = []snapConstraint{}
			return true
		}
		return false
	}},
	{"shrink:report_evidence_halved", func(s *detectionSnapshot) bool {
		a := halve(&s.Finding.Report)
		b := halve(&s.Finding.Evidence)
		return a || b
	}},
	{"shrink:summary_1k", func(s *detectionSnapshot) bool {
		v, did := cutUTF8(s.Finding.Summary, 1<<10)
		s.Finding.Summary = v
		return did
	}},
	{"shrink:urls_256", func(s *detectionSnapshot) bool {
		changed := false
		for i := range s.Assets {
			var did bool
			if s.Assets[i].URL, did = cutUTF8(s.Assets[i].URL, 256); did {
				changed = true
			}
		}
		for i := range s.Traffic {
			var did bool
			if s.Traffic[i].URL, did = cutUTF8(s.Traffic[i].URL, 256); did {
				changed = true
			}
		}
		return changed
	}},
}

func cutTraffic(s *detectionSnapshot, n int) bool {
	if len(s.Traffic) > n {
		s.Traffic = s.Traffic[:n]
		return true
	}
	return false
}

// halve halves *p, never below 4 KiB, at a character boundary.
func halve(p *string) bool {
	const floor = 4 << 10
	if len(*p) <= floor {
		return false
	}
	target := len(*p) / 2
	if target < floor {
		target = floor
	}
	v, _ := cutUTF8(*p, target)
	*p = v
	return true
}

// ---- queue ----------------------------------------------------------------------------------------------------

// ClaimedDetection is what a worker needs to run a claimed run: the one place besides FindingDetectionSnapshot
// that reads the snapshot.
type ClaimedDetection struct {
	ID        int64
	FindingID int64
	Phase     string
	Snapshot  json.RawMessage
	Notes     string
}

// ClaimFindingDetection claims the oldest claimable engine run, or returns nil when there is none. A run with a
// not_before in the future is not claimable. Concurrent claims never return the same run (FOR UPDATE SKIP LOCKED).
func (d *DB) ClaimFindingDetection(ctx context.Context) (*ClaimedDetection, error) {
	c := &ClaimedDetection{}
	err := d.QueryRowContext(ctx, `UPDATE finding_detections
		SET status='running', started_at=COALESCE(started_at, now()), claimed_at=now(), attempts=attempts+1
		WHERE id = (SELECT id FROM finding_detections
		             WHERE path='engine' AND status='pending' AND (not_before IS NULL OR not_before <= now())
		             ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id, finding_id, phase, snapshot, notes`).Scan(&c.ID, &c.FindingID, &c.Phase, &c.Snapshot, &c.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// FindingDetectionSnapshot reads the snapshot of a run, for the worker and the context tool.
func (d *DB) FindingDetectionSnapshot(ctx context.Context, id int64) (json.RawMessage, error) {
	var snap json.RawMessage
	err := d.QueryRowContext(ctx, `SELECT snapshot FROM finding_detections WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&snap)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDetectionNotFound
	}
	return snap, err
}

// notRunning explains a write that changed no row: the run is not running (or is gone). When it is running the
// write was refused by a limit, and nil is returned for the caller to name it.
func (d *DB) notRunning(ctx context.Context, id int64) error {
	var running bool
	err := d.QueryRowContext(ctx, `SELECT status='running' FROM finding_detections WHERE id=$1`, id).Scan(&running)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !running) {
		return ErrDetectionNotRunning
	}
	return err
}

// SetDetectionManifest stores the content-free manifest of the input selection.
func (d *DB) SetDetectionManifest(ctx context.Context, id int64, manifest json.RawMessage) error {
	res, err := d.ExecContext(ctx, `UPDATE finding_detections SET input_manifest=$2::jsonb
		WHERE id=$1 AND status='running' AND octet_length($2::jsonb::text) <= `+fmt.Sprint(detectionManifestCap), id, jsonOrEmpty(manifest))
	if err != nil {
		return fmt.Errorf("set detection manifest: %w", err)
	}
	return d.zeroRows(ctx, res, id, "input_manifest", detectionManifestCap)
}

// zeroRows turns an update that changed no row into the error that explains it.
func (d *DB) zeroRows(ctx context.Context, res sql.Result, id int64, field string, limit int) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if err := d.notRunning(ctx, id); err != nil {
		return err
	}
	return &DetectionLimitError{Field: field, Limit: limit, Unit: "bytes"}
}

// AppendDetectionStep adds a line to the run log of a running run; the sequence number is assigned here.
func (d *DB) AppendDetectionStep(ctx context.Context, id int64, s DetectionStep) error {
	if !detectionSteps[s.Step] {
		return errors.New("detection step name is not one of the known steps")
	}
	if !detectionStepStat[s.Status] {
		return errors.New("detection step status must be ok, failed or skipped")
	}
	if err := limitChars("step code", s.Code, 64); err != nil {
		return err
	}
	if err := limitChars("step call_id", s.CallID, 64); err != nil {
		return err
	}
	attempt := s.Attempt
	if attempt == 0 {
		attempt = 1
	}
	started := s.StartedAt
	if started.IsZero() {
		started = time.Now().UTC()
	}
	// The run row is held for the statement, so a delete of the run cannot slip in between the check and the insert.
	res, err := d.ExecContext(ctx, `WITH r AS (SELECT id FROM finding_detections WHERE id=$1 AND status='running' FOR KEY SHARE)
		INSERT INTO detection_steps(detection_id, seq, step, attempt, status, code, call_id, input_bytes, output_bytes, started_at, duration_ms)
		SELECT r.id, COALESCE((SELECT max(seq) FROM detection_steps WHERE detection_id=r.id), 0) + 1, $2::text, $3::integer, $4::text, $5::text, $6::text,
		       $7::integer, $8::integer, $9::timestamptz, $10::integer FROM r`,
		id, s.Step, attempt, s.Status, s.Code, s.CallID, s.InputBytes, s.OutputBytes, started, s.DurationMS)
	if err != nil {
		return fmt.Errorf("append detection step: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrDetectionNotRunning
	}
	return nil
}

func validateDetectionRule(in *DetectionRuleInput) error {
	if !detectionStages[in.Stage] {
		return errors.New("detection rule stage is not one of the known stages")
	}
	if !detectionFormats[in.Format] {
		return errors.New("detection rule format is not one of the known formats")
	}
	if in.Validation == "" {
		in.Validation = "unvalidated"
	}
	if !detectionValids[in.Validation] {
		return errors.New("detection rule validation is not one of the known states")
	}
	if in.PositiveHits != nil && *in.PositiveHits < 0 || in.BaselineHits != nil && *in.BaselineHits < 0 {
		return errors.New("detection rule hit counts must not be negative")
	}
	if in.Format == "sigma" && in.ParentID != nil {
		return errors.New("a sigma rule has no parent")
	}
	if len(in.AttackTechniques) > 16 {
		return &DetectionLimitError{Field: "attack_techniques", Limit: 16, Unit: "entries"}
	}
	if n := len(in.Body); n < 1 || n > 65536 {
		if n < 1 {
			return errors.New("detection rule body must not be empty")
		}
		return &DetectionLimitError{Field: "body", Limit: 65536, Unit: "bytes"}
	}
	for _, c := range []struct {
		field string
		v     string
		max   int
	}{{"data_source", in.DataSource, 64}, {"technology", in.Technology, 128}, {"title", in.Title, 300}} {
		if err := limitChars(c.field, c.v, c.max); err != nil {
			return err
		}
	}
	return limitOctets("coverage", in.Coverage, 2048)
}

// insertDetectionRuleTx inserts one rule of a running run whose row is locked by the caller. The finding comes
// from the run row. The detail size is measured by the database in the same statement.
func insertDetectionRuleTx(ctx context.Context, tx *sql.Tx, id, findingID int64, in DetectionRuleInput) (int64, error) {
	techniques, err := json.Marshal(append([]string{}, in.AttackTechniques...))
	if err != nil {
		return 0, err
	}
	var ruleID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO detection_rules
		(detection_id, finding_id, parent_id, stage, format, data_source, technology, attack_techniques, title, body, coverage,
		 validation, positive_hits, baseline_hits, validation_detail)
		SELECT $1::bigint, $2::bigint, $3::bigint, $4::text, $5::text, $6::text, $7::text,
		       ARRAY(SELECT jsonb_array_elements_text($8::jsonb)), $9::text, $10::text, $11::text, $12::text, $13::integer, $14::integer, $15::jsonb
		WHERE octet_length($15::jsonb::text) <= `+fmt.Sprint(detectionDetailCap)+`
		RETURNING id`,
		id, findingID, in.ParentID, in.Stage, in.Format, in.DataSource, in.Technology, string(techniques), in.Title, in.Body, in.Coverage,
		in.Validation, in.PositiveHits, in.BaselineHits, jsonOrEmpty(in.ValidationDetail)).Scan(&ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, &DetectionLimitError{Field: "validation_detail", Limit: detectionDetailCap, Unit: "bytes"}
	}
	if err != nil {
		return 0, fmt.Errorf("insert detection rule: %w", err)
	}
	return ruleID, nil
}

// lockRunningDetection locks the row of a running run and returns its finding. ErrDetectionNotRunning when the
// run is not running or does not exist.
func lockRunningDetection(ctx context.Context, tx *sql.Tx, id int64) (int64, error) {
	var findingID int64
	err := tx.QueryRowContext(ctx, `SELECT finding_id FROM finding_detections WHERE id=$1 AND status='running' FOR UPDATE`, id).Scan(&findingID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrDetectionNotRunning
	}
	return findingID, err
}

func detectionRuleCount(ctx context.Context, tx *sql.Tx, id int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM detection_rules WHERE detection_id=$1`, id).Scan(&n)
	return n, err
}

// RecordDetectionRule records one rule of a running run and returns its id. The run is the parameter and the
// finding is taken from the run: no caller, and no model output, can name another finding. The 25th rule of a
// run is refused.
func (d *DB) RecordDetectionRule(ctx context.Context, id int64, in DetectionRuleInput) (int64, error) {
	if err := validateDetectionRule(&in); err != nil {
		return 0, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	findingID, err := lockRunningDetection(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	have, err := detectionRuleCount(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if have >= MaxDetectionRulesPerRun {
		return 0, ErrDetectionRuleLimit
	}
	ruleID, err := insertDetectionRuleTx(ctx, tx, id, findingID, in)
	if err != nil {
		return 0, err
	}
	return ruleID, tx.Commit()
}

// DetectionFinish is the end of a run written by the worker: the status (completed or failed), the code-computed
// outcome and summary, a fixed English error sentence, the stage report and the rules (step 7, "record").
type DetectionFinish struct {
	Status      string // "completed" or "failed"
	Outcome     string
	EndCode     string
	Summary     string
	Error       string
	StageReport json.RawMessage // nil means {}
	Rules       []DetectionRuleInput
}

// FinishFindingDetection ends a running run. The rules and the outcome are written in one transaction: when any
// part is refused nothing is stored and the run stays running (the worker then finishes it as failed). Zero
// affected rows is ErrDetectionNotRunning.
func (d *DB) FinishFindingDetection(ctx context.Context, id int64, f DetectionFinish) error {
	if f.Status != "completed" && f.Status != "failed" {
		return errors.New("a detection run is finished as completed or failed")
	}
	if !detectionOutcomes[f.Outcome] {
		return errors.New("detection outcome is not one of the known outcomes")
	}
	if err := limitChars("end_code", f.EndCode, 64); err != nil {
		return err
	}
	if err := limitOctets("summary", f.Summary, 16384); err != nil {
		return err
	}
	if err := limitOctets("error", f.Error, 2000); err != nil {
		return err
	}
	if len(f.Rules) > MaxDetectionRulesPerRun {
		return ErrDetectionRuleLimit
	}
	for i := range f.Rules {
		if err := validateDetectionRule(&f.Rules[i]); err != nil {
			return err
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	findingID, err := lockRunningDetection(ctx, tx, id)
	if err != nil {
		return err
	}
	have, err := detectionRuleCount(ctx, tx, id)
	if err != nil {
		return err
	}
	if have+len(f.Rules) > MaxDetectionRulesPerRun {
		return ErrDetectionRuleLimit
	}
	for _, r := range f.Rules {
		if _, err := insertDetectionRuleTx(ctx, tx, id, findingID, r); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE finding_detections
		SET status=$2, outcome=$3, end_code=$4, summary=$5, error=$6, stage_report=$7::jsonb, finished_at=now()
		WHERE id=$1 AND status='running' AND octet_length($7::jsonb::text) <= `+fmt.Sprint(detectionStageReportCap),
		id, f.Status, f.Outcome, f.EndCode, f.Summary, f.Error, jsonOrEmpty(f.StageReport))
	if err != nil {
		return fmt.Errorf("finish detection run: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return &DetectionLimitError{Field: "stage_report", Limit: detectionStageReportCap, Unit: "bytes"}
	}
	return tx.Commit()
}

// StopFindingDetection stops a pending or running run as the operator's request. It reports whether it changed
// the run: a run that already ended is not an error.
func (d *DB) StopFindingDetection(ctx context.Context, id int64) (bool, error) {
	res, err := d.ExecContext(ctx, `UPDATE finding_detections
		SET status='stopped', end_code='operator_stop', error='Stopped by the operator.', finished_at=now()
		WHERE id=$1 AND status IN ('pending','running')`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecoverFindingDetections runs at start, after RecoverFindingRetests: runs that were running, and pending runs
// of the agent path, are stopped with end_code 'restart'; pending engine runs stay queued. It returns the number
// of runs stopped and the number still queued, and changes nothing when run twice.
func (d *DB) RecoverFindingDetections() (stopped, queued int64, err error) {
	err = d.QueryRow(`WITH s AS (
		UPDATE finding_detections SET status='stopped', end_code='restart',
		       error='Server restarted; the detection run was interrupted, please start it again', finished_at=now()
		WHERE status='running' OR (status='pending' AND path='agent') RETURNING 1)
		SELECT (SELECT count(*) FROM s), (SELECT count(*) FROM finding_detections WHERE path='engine' AND status='pending')`).Scan(&stopped, &queued)
	return stopped, queued, err
}

// SweepFindingDetections fails engine runs whose claimed_at is older than olderThan (DetectionSweepDeadline in
// production) with end_code 'deadline', and returns how many it failed. A fresh run, a pending run and a run of
// the agent path are left alone. It is the net under the worker's recover(): a worker that is gone cannot leave a
// run running for ever.
func (d *DB) SweepFindingDetections(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := d.ExecContext(ctx, `UPDATE finding_detections
		SET status='failed', end_code='deadline', error='The detection run exceeded its time limit.', finished_at=now()
		WHERE path='engine' AND status='running' AND claimed_at < now() - make_interval(secs => $1::float8)`, olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- query accounting -----------------------------------------------------------------------------------------

// IncrementDetectionQueries counts one workspace query of a running run against cap and returns the new count.
// The increment is one statement, so concurrent callers succeed exactly cap times; the next gets
// ErrDetectionQueryCap, and a run that is not running gets ErrDetectionNotRunning.
func (d *DB) IncrementDetectionQueries(ctx context.Context, id int64, cap int) (int, error) {
	var used int
	err := d.QueryRowContext(ctx, `UPDATE finding_detections SET queries_used=queries_used+1
		WHERE id=$1 AND status='running' AND queries_used < $2 RETURNING queries_used`, id, cap).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		if err := d.notRunning(ctx, id); err != nil {
			return 0, err
		}
		return 0, ErrDetectionQueryCap
	}
	return used, err
}

// RecordDetectionQueryResult stores the result of a query of a running run under (run, sha256, window). The same
// key written again replaces the earlier result: there is one row per key.
func (d *DB) RecordDetectionQueryResult(ctx context.Context, id int64, r DetectionQueryResult) error {
	if len(r.BodySHA256) != 64 {
		return errors.New("detection query result needs the 64-character sha256 of the rule text")
	}
	if !detectionWindows[r.WindowKind] {
		return errors.New("detection query window is not one of the known windows")
	}
	if !detectionQStates[r.State] {
		return errors.New("detection query state must be ok, error or rejected")
	}
	if r.Hits != nil && *r.Hits < 0 {
		return errors.New("detection query hits must not be negative")
	}
	res, err := d.ExecContext(ctx, `WITH r AS (SELECT id FROM finding_detections WHERE id=$1 AND status='running' FOR KEY SHARE)
		INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state, hits, detail)
		SELECT r.id, $2::text, $3::text, $4::text, $5::integer, $6::jsonb FROM r WHERE octet_length($6::jsonb::text) <= `+fmt.Sprint(detectionDetailCap)+`
		ON CONFLICT (detection_id, body_sha256, window_kind)
		DO UPDATE SET state=EXCLUDED.state, hits=EXCLUDED.hits, detail=EXCLUDED.detail, queried_at=now()`,
		id, r.BodySHA256, r.WindowKind, r.State, r.Hits, jsonOrEmpty(r.Detail))
	if err != nil {
		return fmt.Errorf("record detection query result: %w", err)
	}
	return d.zeroRows(ctx, res, id, "query result detail", detectionDetailCap)
}

// FindDetectionQueryResult returns the stored result of (run, sha256, window), or nil. A result of another run
// is never returned.
func (d *DB) FindDetectionQueryResult(ctx context.Context, id int64, bodySHA256, window string) (*DetectionQueryResult, error) {
	r := &DetectionQueryResult{}
	err := d.QueryRowContext(ctx, `SELECT body_sha256, window_kind, state, hits, detail, queried_at FROM detection_query_results
		WHERE detection_id=$1 AND body_sha256=$2 AND window_kind=$3`, id, bodySHA256, window).
		Scan(&r.BodySHA256, &r.WindowKind, &r.State, &r.Hits, &r.Detail, &r.QueriedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ---- lists ----------------------------------------------------------------------------------------------------

// ListFindingDetections is the run history of a finding, newest first, without snapshots, manifests and stage
// reports and without tombstones.
func (d *DB) ListFindingDetections(ctx context.Context, findingID int64) ([]*FindingDetection, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+detectionListCols+` FROM finding_detections
		WHERE finding_id=$1 AND deleted_at IS NULL ORDER BY id DESC`, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*FindingDetection{}
	for rows.Next() {
		r, err := scanDetection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetFindingDetection returns one run with its input manifest and stage report (never the snapshot), or nil when
// the run does not exist or is a tombstone.
func (d *DB) GetFindingDetection(ctx context.Context, id int64) (*FindingDetection, error) {
	r, err := scanDetection(d.QueryRowContext(ctx, `SELECT `+detectionListCols+` FROM finding_detections WHERE id=$1 AND deleted_at IS NULL`, id))
	if err != nil || r == nil {
		return r, err
	}
	err = d.QueryRowContext(ctx, `SELECT input_manifest, stage_report FROM finding_detections WHERE id=$1`, id).Scan(&r.InputManifest, &r.StageReport)
	return r, err
}

// ListDetectionSteps is the run log of a run in order.
func (d *DB) ListDetectionSteps(ctx context.Context, id int64) ([]DetectionStep, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, detection_id, seq, step, attempt, status, code, call_id, input_bytes, output_bytes, started_at, duration_ms
		FROM detection_steps WHERE detection_id=$1 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DetectionStep{}
	for rows.Next() {
		var s DetectionStep
		if err := rows.Scan(&s.ID, &s.DetectionID, &s.Seq, &s.Step, &s.Attempt, &s.Status, &s.Code, &s.CallID, &s.InputBytes, &s.OutputBytes, &s.StartedAt, &s.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListDetectionRules returns the rules of one run, oldest first.
func (d *DB) ListDetectionRules(ctx context.Context, id int64) ([]*DetectionRule, error) {
	return d.listRules(ctx, `SELECT `+detectionRuleCols+` FROM detection_rules WHERE detection_id=$1 ORDER BY id`, id)
}

// ListFindingDetectionRules returns the rules of a finding, the latest run first.
func (d *DB) ListFindingDetectionRules(ctx context.Context, findingID int64) ([]*DetectionRule, error) {
	return d.listRules(ctx, `SELECT `+detectionRuleCols+` FROM detection_rules WHERE finding_id=$1 ORDER BY detection_id DESC, id`, findingID)
}

func (d *DB) listRules(ctx context.Context, query string, arg int64) ([]*DetectionRule, error) {
	rows, err := d.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*DetectionRule{}
	for rows.Next() {
		r, err := scanDetectionRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- delete ---------------------------------------------------------------------------------------------------

// DeleteFindingDetection deletes a finished run. A manual run is removed with its rules, steps and query results
// (by cascade). An automatic run loses its content and keeps its row as a tombstone, so its attempt number is
// never free again (tombstoned=true). An active run is refused (ErrDetectionActive); a run that does not exist,
// or is already a tombstone, is ErrDetectionNotFound.
func (d *DB) DeleteFindingDetection(ctx context.Context, id int64) (tombstoned bool, err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var trigger, status string
	var deleted *time.Time
	err = tx.QueryRowContext(ctx, `SELECT trigger_kind, status, deleted_at FROM finding_detections WHERE id=$1 FOR UPDATE`, id).Scan(&trigger, &status, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrDetectionNotFound
	}
	if err != nil {
		return false, err
	}
	if deleted != nil {
		return false, ErrDetectionNotFound
	}
	if status == "pending" || status == "running" {
		return false, ErrDetectionActive
	}
	if trigger == "manual" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM finding_detections WHERE id=$1 AND trigger_kind='manual' AND status NOT IN ('pending','running')`, id); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	for _, q := range []string{
		`DELETE FROM detection_rules WHERE detection_id=$1`,
		`DELETE FROM detection_steps WHERE detection_id=$1`,
		`DELETE FROM detection_query_results WHERE detection_id=$1`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return false, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE finding_detections
		SET deleted_at = now(), snapshot = '{}', input_manifest = '{}', stage_report = '{}', summary = '', notes = '', error = ''
		WHERE id = $1 AND trigger_kind = 'auto' AND status NOT IN ('pending','running') AND deleted_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n != 1 {
		return false, fmt.Errorf("tombstone of detection run: %d rows changed, want 1", n)
	}
	return true, tx.Commit()
}

// taskHasDetectionRuns reports whether a task's findings have at least one detection run that is not a tombstone.
// It is what the archive refusal asks (section 6.10); q is the transaction that holds the task or its findings.
func taskHasDetectionRuns(ctx context.Context, q detectionQuerier, taskID int64) (bool, error) {
	var has bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM finding_detections d JOIN findings f ON f.id=d.finding_id
		WHERE f.task_id=$1 AND d.deleted_at IS NULL)`, taskID).Scan(&has)
	return has, err
}
