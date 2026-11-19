package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Versioned migrations (docs/phase-d2-stage2-plan.md, docs/schema-upgrade.md).
//
// Schema rules:
//
//   - schema.sql is the idempotent base that runs on every start. It is FROZEN at its 0.5.13m
//     bytes (pinned by the schema gate tests): every later change is a numbered migration in
//     db/migrations/.
//   - Migrations are recorded in schema_migrations, upstream's ledger table, with the sha256 of
//     the file. 0001-4999 are upstream's files, byte-identical; 5000-9999 are the fork's. A
//     released migration is never edited: a new one corrects it. A fork file's first line is
//     "-- downgrade-safe: yes|no: <reason>". No CONCURRENTLY and no VACUUM: they cannot run in
//     the migration transaction, and loadMigrations rejects them outside comments.
//   - Open, holding the schema advisory lock, (1) reads the ledger and refuses a database it does
//     not understand BEFORE anything is written (checkLedger); (2) runs schema.sql; (3) on a
//     database without a ledger adopts the frozen baseline set (baselineVersions) in one
//     transaction; (4) applies every later pending migration, each in its own transaction with its
//     ledger row; (5) seeds the built-in agents.
//   - A migration must not change an object that schema.sql re-creates on every start (the CHECK
//     pairs, the triggers, the four functions, the data statements) unless the same commit changes
//     that schema.sql statement; that lockstep edit is the only allowed change of schema.sql.
//   - A later migration runs exactly once per database; only the baseline set must be idempotent.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// Embedded migration files that EnsureLLMRecordsTable / EnsureLLMUsageTable re-run on demand.
const (
	migrationLLMRecords   = "0001_llm_records.sql"
	migrationLLMUsage     = "0002_llm_usage.sql"
	migrationLLMUsageFork = "5001_llm_usage_fork_columns.sql"
)

// migrationSource is a directory of migration files: the embedded one, or a synthetic one in tests.
type migrationSource = fs.FS

// migrationForkMin is the first version of the fork range; versions below it are upstream's.
const migrationForkMin = 5000

// baselineVersions is the frozen adoption set: the migrations a database without a ledger gets, in
// one transaction, together with their four ledger rows. Each of them is idempotent and a no-op on
// a database that a release without the ledger started. The set never grows: a later migration is
// applied on its own, once, in its own transaction (pinned by TestBaselineSetIsFrozen).
var baselineVersions = []int{1, 2, 3, 5001}

// migrationLockTimeout bounds how long a statement of a migration transaction waits for a table lock.
// A variable so that a test can shorten it.
var migrationLockTimeout = 60 * time.Second

// migrationLockTimeoutText is the value of SET LOCAL lock_timeout.
func migrationLockTimeoutText() string {
	return strconv.FormatInt(migrationLockTimeout.Milliseconds(), 10) + "ms"
}

// embeddedMigrations is the embedded migrations directory, rooted so file names are the bare
// NNNN_slug.sql names.
func embeddedMigrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory exists
	}
	return sub
}

const migrationLedgerTable = `
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT,
    checksum   TEXT,
    applied_at TIMESTAMPTZ DEFAULT now()
);
`

var migrationNameRE = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

type migration struct {
	version  int
	name     string // file name, e.g. 0001_llm_records.sql
	sql      string
	checksum string // hex SHA-256 of the file content
}

func migrationRange(version int) int {
	if version >= migrationForkMin {
		return 1
	}
	return 0
}

// scanMigrationSQL makes two views of a migration file, both without comments (-- to the end of the
// line, and nested /* */):
//
//	code  everything else verbatim, string literals and dollar-quoted bodies included (the lint for
//	      CONCURRENTLY and VACUUM looks here, so that a word inside a DO block is still seen)
//	top   the same with every string literal, quoted identifier and dollar-quoted body replaced by one
//	      space, so that only the statements of the file itself are left (the transaction-control
//	      lint looks here, where a PL/pgSQL BEGIN ... END cannot be taken for a transaction BEGIN)
//
// A dollar-quoted body ($$ ... $$ or $tag$ ... $tag$) is scanned the way PostgreSQL does: up to the
// closing tag, with no comment, string or nesting rules, so a "--" inside it does not hide anything
// after it. Not handled: a standard_conforming_strings = off file (backslash escapes in plain
// '...' strings; only E'...' strings are read with them), and psql meta-commands (the runner does
// not use psql).
func scanMigrationSQL(text string) (code, top string) {
	var c, t strings.Builder
	isIdent := func(b byte) bool {
		return b == '_' || b == '$' || b >= 0x80 || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
	}
	blockDepth := 0
	for i := 0; i < len(text); i++ {
		ch := text[i]
		switch {
		case blockDepth > 0:
			if ch == '/' && i+1 < len(text) && text[i+1] == '*' {
				blockDepth++
				i++
			} else if ch == '*' && i+1 < len(text) && text[i+1] == '/' {
				blockDepth--
				i++
				if blockDepth == 0 {
					c.WriteByte(' ')
					t.WriteByte(' ')
				}
			}
		case ch == '\'' || ch == '"':
			// A string literal or a quoted identifier; '' and "" inside are two quotes in a row, which
			// this loop reads as one literal ending and the next beginning.
			escapes := ch == '\'' && i > 0 && (text[i-1] == 'E' || text[i-1] == 'e') && (i < 2 || !isIdent(text[i-2]))
			j := i + 1
			for j < len(text) && text[j] != ch {
				if escapes && text[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(text) {
				j = len(text) - 1
			}
			c.WriteString(text[i : j+1])
			t.WriteByte(' ')
			i = j
		case ch == '$' && (i == 0 || !isIdent(text[i-1])):
			j := i + 1
			if j < len(text) && !(text[j] == '$' || text[j] == '_' || text[j] >= 0x80 || (text[j] >= 'a' && text[j] <= 'z') || (text[j] >= 'A' && text[j] <= 'Z')) {
				c.WriteByte(ch)
				t.WriteByte(ch)
				continue
			}
			for j < len(text) && text[j] != '$' && isIdent(text[j]) {
				j++
			}
			if j >= len(text) || text[j] != '$' {
				c.WriteByte(ch)
				t.WriteByte(ch)
				continue
			}
			tag := text[i : j+1]
			end := strings.Index(text[j+1:], tag)
			stop := len(text) - 1
			if end >= 0 {
				stop = j + 1 + end + len(tag) - 1
			}
			c.WriteString(text[i : stop+1])
			t.WriteByte(' ')
			i = stop
		case ch == '-' && i+1 < len(text) && text[i+1] == '-':
			for i < len(text) && text[i] != '\n' {
				i++
			}
			c.WriteByte('\n')
			t.WriteByte('\n')
		case ch == '/' && i+1 < len(text) && text[i+1] == '*':
			blockDepth = 1
			i++
		default:
			c.WriteByte(ch)
			t.WriteByte(ch)
		}
	}
	return c.String(), t.String()
}

// migrationCodeOnly is the "code" view of scanMigrationSQL: comments removed, the rest verbatim.
func migrationCodeOnly(text string) string {
	code, _ := scanMigrationSQL(text)
	return code
}

// migrationForbiddenRE matches the statements that cannot run inside a transaction block.
var migrationForbiddenRE = regexp.MustCompile(`(?i)\b(concurrently|vacuum)\b`)

var (
	migrationWordRE   = regexp.MustCompile(`[A-Za-z_]+`)
	migrationAtomicRE = regexp.MustCompile(`(?i)\bBEGIN\s+ATOMIC\b`)
)

// migrationTransactionControl returns the first transaction-control statement of the file's own
// statements (the "top" view of scanMigrationSQL), or "". The file is one simple-protocol Exec
// inside the runner's transaction: a COMMIT or ROLLBACK ends that transaction (the SET LOCAL
// lock_timeout is lost and the ledger row is written in autocommit), and a BEGIN, SAVEPOINT or the
// like changes what the runner's own COMMIT means.
//
// It looks at the first word or two of every statement (split at ';') outside comments, string
// literals, quoted identifiers and dollar-quoted bodies: COMMIT, ROLLBACK, SAVEPOINT, RELEASE, ABORT,
// BEGIN, END, START TRANSACTION and PREPARE TRANSACTION. A PL/pgSQL BEGIN ... END is inside a dollar
// body or a string and is not looked at; the BEGIN ATOMIC ... END body of an SQL-standard function
// is skipped up to its END. What it does not catch: transaction control run from inside a body or
// a procedure (CALL p() where p COMMITs; PostgreSQL itself refuses that inside a transaction block),
// a statement built by EXECUTE, and a statement hidden behind a standard_conforming_strings = off
// string. The runtime check in execMigration is the second line.
func migrationTransactionControl(top string) string {
	atomic := false
	for _, stmt := range strings.Split(top, ";") {
		words := migrationWordRE.FindAllString(stmt, 3)
		if len(words) == 0 {
			continue
		}
		first := strings.ToUpper(words[0])
		second := ""
		if len(words) > 1 {
			second = strings.ToUpper(words[1])
		}
		if migrationAtomicRE.MatchString(stmt) {
			atomic = true // CREATE FUNCTION ... BEGIN ATOMIC <statements>; END
			continue
		}
		switch first {
		case "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "ABORT", "BEGIN":
			return first
		case "END":
			if atomic {
				atomic = false
				continue
			}
			return first
		case "START", "PREPARE":
			if second == "TRANSACTION" {
				return first + " TRANSACTION"
			}
		}
	}
	return ""
}

// loadMigrations reads every *.sql file at the root of fsys, sorted by version. A .sql file that
// does not follow NNNN_slug.sql, a duplicated version, or a statement that cannot run inside the
// migration transaction (CONCURRENTLY, VACUUM, outside comments) or that ends or nests the transaction
// (COMMIT, BEGIN and the like, see migrationTransactionControl) is an error, so a typo cannot silently
// skip a migration and a migration cannot fail only on a real database or leave a half-applied file.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		m := migrationNameRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %s: name must be NNNN_slug.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		if version == 0 {
			return nil, fmt.Errorf("migration file %s: version must be > 0", e.Name())
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migration version %04d is used by both %s and %s", version, prev, e.Name())
		}
		seen[version] = e.Name()
		content, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		code, top := scanMigrationSQL(string(content))
		if word := migrationForbiddenRE.FindString(code); word != "" {
			return nil, fmt.Errorf("migration file %s: %s is not allowed (a migration runs inside a transaction)", e.Name(), strings.ToUpper(word))
		}
		if stmt := migrationTransactionControl(top); stmt != "" {
			return nil, fmt.Errorf("migration file %s: %s is not allowed (a migration runs inside the runner's transaction and must not control the transaction)", e.Name(), stmt)
		}
		sum := sha256.Sum256(content)
		out = append(out, migration{version: version, name: e.Name(), sql: string(content), checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// ---- error text ----

// migrationErrorText reduces an error to what may be printed. A PostgreSQL error becomes its SQLSTATE
// and the names of the schema, table, column and constraint it carries; its message is added only for
// the classes whose messages name objects (23 integrity, 40 rollback, 42 syntax or name, 55 object
// state, 57 operator intervention). Never the Detail or Where (they quote row values), never the
// message of class 22 (it quotes the input value), never the DSN: a connection error and any error of
// another kind are reduced to a fixed text.
func migrationErrorText(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		parts := []string{"SQLSTATE " + pgErr.Code}
		if pgErr.SchemaName != "" {
			parts = append(parts, "schema "+pgErr.SchemaName)
		}
		if pgErr.TableName != "" {
			parts = append(parts, "table "+pgErr.TableName)
		}
		if pgErr.ColumnName != "" {
			parts = append(parts, "column "+pgErr.ColumnName)
		}
		if pgErr.ConstraintName != "" {
			parts = append(parts, "constraint "+pgErr.ConstraintName)
		}
		text := strings.Join(parts, ", ")
		if pgErr.Message != "" && len(pgErr.Code) >= 2 {
			switch pgErr.Code[:2] {
			case "23", "40", "42", "55", "57":
				text += ": " + pgErr.Message
			}
		}
		return text
	}
	if errors.Is(err, errMigrationEndedTx) {
		return err.Error() // a fixed text of this package
	}
	var connErr *pgconn.ConnectError
	var netErr *net.OpError
	if errors.As(err, &connErr) || errors.As(err, &netErr) {
		return "no SQLSTATE: connection error (details withheld)"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, sql.ErrConnDone) {
		return "no SQLSTATE: " + err.Error()
	}
	return "no SQLSTATE: driver or I/O error (details withheld)"
}

// migrationError is an error of a migration step. Its text is content-free (migrationErrorText) and
// it unwraps to the cause, so errors.As still finds the PostgreSQL error.
type migrationError struct {
	what string // "migration 5003 (5003_x.sql)"
	mig  *migration
	err  error
	hint string
}

func newMigrationError(what string, err error) error { return &migrationError{what: what, err: err} }

func newMigrationErrorFor(m migration, err error) error {
	return &migrationError{what: migrationLabel(m), mig: &m, err: err}
}

func (e *migrationError) Error() string {
	text := e.what + ": " + migrationErrorText(e.err)
	if e.hint != "" {
		text += "; " + e.hint
	}
	return text
}

func (e *migrationError) Unwrap() error { return e.err }

func migrationLabel(m migration) string { return fmt.Sprintf("migration %04d (%s)", m.version, m.name) }

// ---- the preflight ----

// Kinds of refusal, in the order checkLedger tries them.
const (
	migrationRefusalEmpty   = "empty"   // schema_migrations exists and has no rows
	migrationRefusalForeign = "foreign" // rows exist and none is in the fork range
	migrationRefusalChanged = "changed" // a known version with another name or checksum
	migrationRefusalUnknown = "unknown" // a version this binary does not ship
	migrationRefusalOrder   = "order"   // a pending version below the newest applied one in its range
)

var migrationRefusalLabels = map[string]string{
	migrationRefusalEmpty:   "ledger was edited",
	migrationRefusalForeign: "ledger created by another binary",
	migrationRefusalChanged: "applied migration changed",
	migrationRefusalUnknown: "unknown migration applied",
	migrationRefusalOrder:   "migration out of order",
}

// migrationRefusal is the error of a database this binary refuses to start on. It is returned
// before anything is written.
type migrationRefusal struct {
	kind    string
	reason  string // the full, actionable text
	version int    // 0 when the refusal concerns the ledger as a whole
	file    string
}

func (e *migrationRefusal) Error() string { return e.reason }

// logLine is the content-free log line: label, version and file only. The file name is quoted: for
// an unknown version it is the ledger's, which the database controls.
func (e *migrationRefusal) logLine() string {
	line := "[migrate] refused: " + migrationRefusalLabels[e.kind]
	if e.version != 0 {
		line += fmt.Sprintf(" (version %04d, %q)", e.version, e.file)
	}
	return line
}

type ledgerRow struct {
	version        int
	name, checksum string
}

// ledgerPlan is what checkLedger found.
type ledgerPlan struct {
	absent  bool              // no schema_migrations table: adopt the baseline set
	applied map[int]ledgerRow // rows of the ledger
	unknown []ledgerRow       // applied versions this binary does not ship, all within the override
}

// ledgerQueryer is what checkLedger needs; *sql.DB, *sql.Conn and *sql.Tx satisfy it.
type ledgerQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func shortChecksum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// checkLedger is the read-only preflight. It runs before schema.sql, so that a database this binary
// does not understand is refused before anything is written (not even by schema.sql, which an older
// or foreign binary would otherwise have run over). The checks run in a fixed order and the first
// that matches wins:
//
//	table absent                          adopt (plan.absent)
//	0a  table exists, no rows             refuse: the ledger was edited
//	1   rows, none in 5000-9999           refuse: created by another binary (no override)
//	2   known version, other name or sum  refuse (no override)
//	3   version this binary does not ship refuse, unless every such version <= allowNewer
//	4   pending version below the newest applied one of its range: refuse (no override)
//
// allowNewer is the value of ARTEX_SCHEMA_ALLOW_NEWER, 0 when it is not set.
func checkLedger(ctx context.Context, q ledgerQueryer, ms []migration, allowNewer int) (ledgerPlan, error) {
	plan := ledgerPlan{applied: map[int]ledgerRow{}}
	exists, err := ledgerTableExists(ctx, q)
	if err != nil {
		return plan, err
	}
	if !exists {
		plan.absent = true
		return plan, nil
	}

	rows, err := q.QueryContext(ctx, `SELECT version, COALESCE(name,''), COALESCE(checksum,'') FROM schema_migrations ORDER BY version`)
	if err != nil {
		return plan, newMigrationError("read schema_migrations", err)
	}
	var ledger []ledgerRow
	for rows.Next() {
		var row ledgerRow
		if err := rows.Scan(&row.version, &row.name, &row.checksum); err != nil {
			rows.Close()
			return plan, newMigrationError("read schema_migrations", err)
		}
		ledger = append(ledger, row)
		plan.applied[row.version] = row
	}
	if err := rows.Close(); err != nil {
		return plan, newMigrationError("read schema_migrations", err)
	}
	if err := rows.Err(); err != nil {
		return plan, newMigrationError("read schema_migrations", err)
	}

	if len(ledger) == 0 {
		return plan, &migrationRefusal{kind: migrationRefusalEmpty,
			reason: "schema_migrations exists but is empty: the ledger was edited; restore the pre-upgrade dump"}
	}
	hasFork := false
	for _, row := range ledger {
		hasFork = hasFork || migrationRange(row.version) == 1
	}
	if !hasFork {
		return plan, &migrationRefusal{kind: migrationRefusalForeign,
			reason: "schema_migrations was created by another binary: it has no migration in the fork range 5000-9999, " +
				"so this database was last used by an upstream build; this binary does not run on it, restore a dump made by this fork"}
	}

	byVersion := map[int]migration{}
	for _, m := range ms {
		byVersion[m.version] = m
	}
	for _, row := range ledger {
		m, ok := byVersion[row.version]
		if !ok {
			continue
		}
		if row.name != m.name {
			return plan, &migrationRefusal{kind: migrationRefusalChanged, version: row.version, file: m.name,
				reason: fmt.Sprintf("migration %04d: database records file %q but this binary has %s; published migrations must not be renamed", row.version, row.name, m.name)}
		}
		if row.checksum != m.checksum {
			return plan, &migrationRefusal{kind: migrationRefusalChanged, version: row.version, file: m.name,
				reason: fmt.Sprintf("migration %04d (%s): checksum changed since it was applied (database %q, file %s); published migrations must not be edited",
					row.version, m.name, shortChecksum(row.checksum), shortChecksum(m.checksum))}
		}
	}
	for _, row := range ledger {
		if _, ok := byVersion[row.version]; ok {
			continue
		}
		if allowNewer == 0 || row.version > allowNewer {
			return plan, &migrationRefusal{kind: migrationRefusalUnknown, version: row.version, file: row.name,
				reason: fmt.Sprintf("migration %04d (%q) is applied in the database but missing from this binary; run the release that applied it, "+
					"or set ARTEX_SCHEMA_ALLOW_NEWER=%04d to start anyway (see docs/schema-upgrade.md)", row.version, row.name, row.version)}
		}
		plan.unknown = append(plan.unknown, row)
	}

	// The newest applied version of each range, unknown versions included.
	var newest [2]ledgerRow
	for _, row := range ledger {
		if rng := migrationRange(row.version); row.version > newest[rng].version {
			newest[rng] = row
		}
	}
	for _, m := range ms {
		if _, done := plan.applied[m.version]; done {
			continue
		}
		if n := newest[migrationRange(m.version)]; m.version < n.version {
			return plan, &migrationRefusal{kind: migrationRefusalOrder, version: m.version, file: m.name,
				reason: fmt.Sprintf("migration %s (%04d) is older than the applied migration %q (%04d); migrations must be appended, never inserted",
					m.name, m.version, n.name, n.version)}
		}
	}
	return plan, nil
}

func ledgerTableExists(ctx context.Context, q ledgerQueryer) (bool, error) {
	r, err := q.QueryContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`)
	if err != nil {
		return false, newMigrationError("read schema_migrations", err)
	}
	defer r.Close()
	if !r.Next() {
		err := r.Err()
		if err == nil {
			err = errors.New("no row")
		}
		return false, newMigrationError("read schema_migrations", err)
	}
	var exists bool
	if err := r.Scan(&exists); err != nil {
		return false, newMigrationError("read schema_migrations", err)
	}
	return exists, nil
}

// allowNewerVersion reads ARTEX_SCHEMA_ALLOW_NEWER: the highest unknown migration version the
// operator accepts, four digits. Unset or blank is "not set". Any other value is an error, so that a
// typo cannot turn the override into a silent no-op or an accept-all.
func allowNewerVersion() (int, bool, error) {
	v := strings.TrimSpace(os.Getenv("ARTEX_SCHEMA_ALLOW_NEWER"))
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || len(v) != 4 || v[0] == '-' || v[0] == '+' || n == 0 {
		return 0, false, errors.New("ARTEX_SCHEMA_ALLOW_NEWER must be a migration version of four digits, for example 5002")
	}
	return n, true, nil
}

// ---- applying ----

// migrationBeginner is what withMigrationRetry needs; *sql.Conn and *sql.DB satisfy it.
type migrationBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// migrationConn is the connection that holds the schema advisory lock (see Open); every statement of
// a migration runs on it, so nothing here waits on that lock again.
type migrationConn interface {
	migrationBeginner
	ledgerQueryer
}

// runMigrationTx runs fn in one transaction whose statements wait at most migrationLockTimeout for a
// lock (SET LOCAL ends with the transaction).
func runMigrationTx(ctx context.Context, conn migrationBeginner, fn func(*sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return newMigrationError("begin transaction", err)
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '"+migrationLockTimeoutText()+"'"); err != nil {
		return rollbackAfter(tx, newMigrationError("set lock_timeout", err))
	}
	if err := fn(tx); err != nil {
		return rollbackAfter(tx, err)
	}
	if err := tx.Commit(); err != nil {
		return newMigrationError("commit", err)
	}
	return nil
}

// rollbackAfter rolls tx back and returns cause, joined with the rollback's own error when it has one.
func rollbackAfter(tx *sql.Tx, cause error) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return errors.Join(cause, newMigrationError("rollback", err))
	}
	return cause
}

// withMigrationRetry runs fn in a migration transaction and, like applySchemaWithRetry, retries it
// when PostgreSQL reports a deadlock (40P01) after rolling the failed attempt back, with the same
// delays. Any other error is returned at once.
func withMigrationRetry(ctx context.Context, conn migrationBeginner, sleep func(time.Duration), fn func(*sql.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := runMigrationTx(ctx, conn, fn)
		if err == nil {
			return nil
		}
		if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
			return err
		}
		log.Printf("[migrate] deadlock, retry %d/%d", attempt+1, len(schemaDeadlockRetryDelays))
		sleep(schemaDeadlockRetryDelays[attempt])
	}
}

// migrationTxMarker is a custom setting that execMigration sets with SET LOCAL before a file runs and
// reads after it.
const migrationTxMarker = "artex.migration_tx"

// errMigrationEndedTx is returned when a migration file ended the transaction it runs in.
var errMigrationEndedTx = errors.New("the file ended the migration transaction (COMMIT, ROLLBACK or the like); the ledger row was not written")

// execMigration runs one migration file and its ledger row.
func execMigration(ctx context.Context, tx *sql.Tx, m migration) error {
	// The marker lives as long as the transaction (SET LOCAL). The loader rejects files that control the
	// transaction; this is the second line: if the file ended it anyway, the marker is gone and the
	// ledger row is not written in autocommit.
	if _, err := tx.ExecContext(ctx, "SET LOCAL "+migrationTxMarker+" = 'on'"); err != nil {
		return newMigrationErrorFor(m, err)
	}
	// No args: pgx sends it over the simple protocol, which allows several statements.
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return newMigrationErrorFor(m, err)
	}
	var open bool
	if err := tx.QueryRowContext(ctx, "SELECT current_setting('"+migrationTxMarker+"', true) = 'on'").Scan(&open); err != nil {
		return newMigrationErrorFor(m, fmt.Errorf("check transaction: %w", err))
	}
	if !open {
		return newMigrationErrorFor(m, errMigrationEndedTx)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum) VALUES ($1, $2, $3)`, m.version, m.name, m.checksum); err != nil {
		return newMigrationErrorFor(m, fmt.Errorf("record: %w", err))
	}
	return nil
}

// adoptBaseline creates the ledger and applies the baseline migrations, with their ledger rows, in
// ONE transaction: a failure leaves neither the ledger nor a row, and the database as after a start
// of a release without the ledger.
func adoptBaseline(ctx context.Context, conn migrationConn, baseline []migration) error {
	err := withMigrationRetry(ctx, conn, time.Sleep, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, migrationLedgerTable); err != nil {
			return newMigrationError("create schema_migrations", err)
		}
		for _, m := range baseline {
			if err := execMigration(ctx, tx, m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		var me *migrationError
		if errors.As(err, &me) {
			me.what = "adopt baseline: " + me.what
		}
		return explainLockTimeout(ctx, conn, err)
	}
	return nil
}

// applyPending applies, in ascending version order, every migration of ms that plan.applied has no
// row for, each in its own transaction together with its ledger row. A failure rolls that one back
// and stops, leaving the earlier ones applied.
func applyPending(ctx context.Context, conn migrationConn, ms []migration, plan ledgerPlan) error {
	for _, m := range ms {
		if _, done := plan.applied[m.version]; done {
			continue
		}
		start := time.Now()
		err := withMigrationRetry(ctx, conn, time.Sleep, func(tx *sql.Tx) error {
			return execMigration(ctx, tx, m)
		})
		if err != nil {
			return explainLockTimeout(ctx, conn, err)
		}
		log.Printf("[migrate] applied %s in %dms", m.name, time.Since(start).Milliseconds())
	}
	return nil
}

// explainLockTimeout adds, to a lock timeout (55P03) of a migration transaction, which table is held
// and by whom. PostgreSQL does not name the table in the error, so it is looked up among the tables
// the migration mentions that another session has locked now.
func explainLockTimeout(ctx context.Context, conn ledgerQueryer, err error) error {
	var pgErr *pgconn.PgError
	var me *migrationError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || !errors.As(err, &me) || me.mig == nil {
		return err
	}
	table := pgErr.TableName
	if table == "" {
		table = lockedTables(ctx, conn, *me.mig)
	}
	me.hint = fmt.Sprintf("another session holds a lock on %s: a running ARTEX instance of an older release, pg_dump, or an open psql transaction; stop it and restart", table)
	log.Printf("[migrate] lock timeout on %s after %gs: another session holds a lock: an older ARTEX instance, pg_dump or an open psql transaction; stop it and restart",
		table, migrationLockTimeout.Seconds())
	return me
}

// lockedTables names the tables, among those the migration's text mentions, that another session of
// this database holds a lock on. It is only a hint, so a failed lookup is reported as such in the text.
func lockedTables(ctx context.Context, conn ledgerQueryer, m migration) string {
	rows, err := conn.QueryContext(ctx, `
SELECT DISTINCT c.relname
  FROM pg_locks l
  JOIN pg_class c ON c.oid = l.relation AND c.relkind = 'r'
  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
 WHERE l.locktype = 'relation' AND l.granted AND l.pid <> pg_backend_pid()
   AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
 ORDER BY 1`)
	if err != nil {
		return "a table of this migration (lookup failed: " + migrationErrorText(err) + ")"
	}
	defer rows.Close()
	code := migrationCodeOnly(m.sql)
	var held []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "a table of this migration (lookup failed)"
		}
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(code) {
			held = append(held, name)
		}
	}
	if err := rows.Err(); err != nil {
		return "a table of this migration (lookup failed: " + migrationErrorText(err) + ")"
	}
	if len(held) == 0 {
		return "a table of this migration (the lock was released meanwhile)"
	}
	return strings.Join(held, ", ")
}

// baselineMigrations selects the baseline set from ms, ascending. A baseline version this binary does
// not ship is an error.
func baselineMigrations(ms []migration) ([]migration, error) {
	byVersion := map[int]migration{}
	for _, m := range ms {
		byVersion[m.version] = m
	}
	versions := append([]int(nil), baselineVersions...)
	sort.Ints(versions)
	out := make([]migration, 0, len(versions))
	for _, v := range versions {
		m, ok := byVersion[v]
		if !ok {
			return nil, fmt.Errorf("baseline migration %04d is not shipped by this binary", v)
		}
		out = append(out, m)
	}
	return out, nil
}

// applyMigrations adopts the baseline when plan says there is no ledger, then applies the later
// pending migrations, and logs the resulting state. conn is the connection that holds the schema lock.
func applyMigrations(ctx context.Context, conn migrationConn, ms, baseline []migration, plan ledgerPlan) error {
	if plan.absent {
		log.Printf("[migrate] ledger absent: adopting baseline (%d)", len(baseline))
		start := time.Now()
		if err := adoptBaseline(ctx, conn, baseline); err != nil {
			return err
		}
		log.Printf("[migrate] adopted baseline in %dms", time.Since(start).Milliseconds())
		plan.applied = map[int]ledgerRow{}
		for _, m := range baseline {
			plan.applied[m.version] = ledgerRow{m.version, m.name, m.checksum}
		}
	}
	if err := applyPending(ctx, conn, ms, plan); err != nil {
		return err
	}
	return logLedgerState(ctx, conn)
}

// logLedgerState logs the newest applied version of each range.
func logLedgerState(ctx context.Context, q ledgerQueryer) error {
	rows, err := q.QueryContext(ctx, `SELECT max(version) FILTER (WHERE version < $1), max(version) FILTER (WHERE version >= $1) FROM schema_migrations`, migrationForkMin)
	if err != nil {
		return newMigrationError("read schema_migrations", err)
	}
	defer rows.Close()
	var upstream, fork sql.NullInt64
	if !rows.Next() {
		return newMigrationError("read schema_migrations", errors.New("no row"))
	}
	if err := rows.Scan(&upstream, &fork); err != nil {
		return newMigrationError("read schema_migrations", err)
	}
	show := func(v sql.NullInt64) string {
		if !v.Valid {
			return "none"
		}
		return fmt.Sprintf("%04d", v.Int64)
	}
	log.Printf("[migrate] up to date (upstream=%s, fork=%s)", show(upstream), show(fork))
	return nil
}

// logUnknownVersions warns about each applied version this binary does not ship, accepted by the
// ARTEX_SCHEMA_ALLOW_NEWER override.
func logUnknownVersions(plan ledgerPlan, allowNewer int) {
	for _, row := range plan.unknown {
		log.Printf("[migrate] WARNING: ARTEX_SCHEMA_ALLOW_NEWER=%04d; unknown migration %04d (%q)", allowNewer, row.version, row.name)
	}
}

// ---- Open ----

// migrationStartup is what Open loads before it connects: the migration set, the baseline subset
// and the ARTEX_SCHEMA_ALLOW_NEWER override. A bad set or a bad override stops the start before
// the database is touched.
type migrationStartup struct {
	all, baseline []migration
	allowNewer    int
}

func loadMigrationStartup(fsys migrationSource) (*migrationStartup, error) {
	all, err := loadMigrations(fsys)
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	baseline, err := baselineMigrations(all)
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	allowNewer, _, err := allowNewerVersion()
	if err != nil {
		return nil, err
	}
	return &migrationStartup{all: all, baseline: baseline, allowNewer: allowNewer}, nil
}

// run returns what Open does while it holds the schema advisory lock, on the lock's connection:
// preflight (read only), schema.sql, baseline adoption and pending migrations, seedBuiltins.
func (s *migrationStartup) run(d *DB) func(*sql.Conn) error {
	return func(conn *sql.Conn) error {
		ctx := context.Background()
		plan, err := checkLedger(ctx, conn, s.all, s.allowNewer)
		if err != nil {
			var refusal *migrationRefusal
			if errors.As(err, &refusal) {
				log.Print(refusal.logLine())
			}
			return fmt.Errorf("migration preflight: %w", err)
		}
		logUnknownVersions(plan, s.allowNewer)
		sum := sha256.Sum256([]byte(schemaSQL))
		log.Printf("[schema] schema.sql sha256=%s", hex.EncodeToString(sum[:])[:12])
		if err := applySchemaWithRetry(ctx, conn, time.Sleep); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := applyMigrations(ctx, conn, s.all, s.baseline, plan); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		if err := d.seedBuiltins(); err != nil {
			return fmt.Errorf("seed builtins: %w", err)
		}
		return nil
	}
}

// ---- the Ensure wrappers ----

// execEmbeddedMigrations re-runs embedded (idempotent) migration files in one transaction under the
// schema lock. It does not touch schema_migrations. The lock is taken on a pooled connection, so this
// must never be called from inside Open's withSchemaMigrationLock callback: it would wait on the lock
// that callback's connection holds.
func (d *DB) execEmbeddedMigrations(names ...string) error {
	tx, err := d.Begin()
	if err != nil {
		return newMigrationError("embedded migrations: begin transaction", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return newMigrationError("embedded migrations: schema lock", err)
	}
	// Set after the schema lock, so that waiting for that lock keeps its old, unbounded behaviour (a
	// slow start of another instance must not fail an Ensure call); it bounds the table locks of the DDL.
	if _, err := tx.Exec("SET LOCAL lock_timeout = '" + migrationLockTimeoutText() + "'"); err != nil {
		return newMigrationError("embedded migrations: set lock_timeout", err)
	}
	for _, name := range names {
		content, err := fs.ReadFile(embeddedMigrations(), name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(content)); err != nil {
			return newMigrationError("embedded migration "+name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return newMigrationError("embedded migrations: commit", err)
	}
	return nil
}
