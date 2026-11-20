# Schema upgrades: migrations and validation

ARTEX applies the embedded `db/schema.sql` on every start (`CREATE ... IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`,
CHECK constraints dropped and re-added, triggers recreated), then applies the numbered migrations of
`db/migrations/` that are not yet recorded in the table `schema_migrations`, then seeds the built-in rows.
`db/schema.sql` is **frozen** at its 0.5.13m bytes: every later schema change is a migration file.

Two parts, two decisions:

- **Validation** (Phase D.2 stage 1): tests, fixtures and a gate that show, on temporary databases, what startup does
  to an existing database, and that make every schema change pass a recorded procedure.
  [adr/0003-schema-upgrade-validation.md](adr/0003-schema-upgrade-validation.md).
- **Runner** (Phase D.2 stage 2, pull request S2-A): the ledger, the baseline adoption, the refusals and the
  override. [adr/0004-versioned-migrations-stage-2.md](adr/0004-versioned-migrations-stage-2.md). The stage 1 suite
  now runs on the runner.

This page starts with what an operator needs ("Upgrading to a release with migrations"), then what the validation
covers, how to run it, how to change the schema, and what is still missing.

Issue #24 asks for migration order and checksums. S2-A delivers both, but ships no migration that changes the
schema (the baseline files are no-ops on every database a release has started). The issue is not closed by this
change; the pull request with the first real migration (upstream `0004`) is planned to close it.

## Upgrading to a release with migrations

### Before you start

1. **Take a full dump** (`scripts/backup.sh`; see the README, "Backups"). The first start writes to the database and
   that write is not undone by going back to the old binary (see "Rollback"). Run `umask 077` first or `chmod 600`
   the dump: it holds `auth.password_hash`.
2. **Stop every older ARTEX instance** that uses the database, and make sure **no `pg_dump` is running** against it.
   The migrations take short table locks (`ACCESS EXCLUSIVE` on `llm_records` and `llm_usage`, `SHARE` on
   `task_scope` until the transaction ends). A session that holds a conflicting lock makes the start fail after 60
   seconds (see "Lock timeout").
3. Read the release notes in the CHANGELOG for migrations that state `downgrade-safe: no`.

### What the first start does

On a database that an earlier release started (no ledger), the start does everything that earlier releases did
(`schema.sql`, the seeding of the built-in rows), and in addition writes **one new table, `schema_migrations`, with
four rows** (`0001`, `0002`, `0003`, `5001`), in one transaction. The four migration files are written so that they
change nothing on such a database: each statement is `IF NOT EXISTS`. If that transaction fails, neither the table
nor a row remains, and the next start tries again.

The start reads the ledger first and refuses a database it does not understand **before it writes anything**,
including before `schema.sql` runs. The order is: read the ledger, `schema.sql`, adopt the baseline (when there is
no ledger), apply later pending migrations (each in its own transaction together with its ledger row), seed the
built-in rows. All of it holds the schema advisory lock (key `7337741001`), so a second instance waits and then finds
a complete ledger.

The write of `schema_migrations` is **not undone when you roll the binary back**. Binaries up to 0.5.13m never read
the table and still start (see "Rollback").

A release that ships migration `5002` (Tool RAG observation, Phase D.3) then applies it in its own transaction, so
after a start the ledger holds **five** rows, the last one `5002`, and the database has one more table,
`tool_rag_observations`. See "Migration 5002" below.

A failure of an `llm_*` table migration (`0001`, `0002`, `5001`) now **stops the start**. Earlier releases only
logged it and went on.

What the log shows (content-free lines: versions and file names, no data):

| Line | When |
|---|---|
| `[schema] schema.sql sha256=<12 hex>` | every start |
| `[migrate] ledger absent: adopting baseline (4)`, then `[migrate] adopted baseline in <ms>ms` | the first start |
| `[migrate] applied <file> in <ms>ms` | each later migration |
| `[migrate] up to date (upstream=<version>, fork=<version>)` | a successful start |
| `[migrate] refused: <label> (version <NNNN>, "<file>")` | a refusal (table below) |
| `[migrate] WARNING: ARTEX_SCHEMA_ALLOW_NEWER=<NNNN>; unknown migration <NNNN> ("<file>")` | every start while the override applies |
| `[migrate] lock timeout on <table> after 60s: another session holds a lock: ...` | a lock timeout |
| `[migrate] deadlock, retry <n>/4` | a deadlock that is being retried |

### When the start refuses

The start fails with `migration preflight: <message>` and the process exits. Nothing was written. There is **no
override** except for the "unknown migration" case.

| Log label | Message | What it means | Recovery |
|---|---|---|---|
| `ledger was edited` | `schema_migrations exists but is empty: the ledger was edited; restore the pre-upgrade dump` | The table exists with no rows. The runner never leaves an empty ledger, so someone deleted its rows. | Restore the pre-upgrade dump (full dump, fresh database). Do not insert rows by hand. |
| `ledger created by another binary` | `schema_migrations was created by another binary: it has no migration in the fork range 5000-9999, so this database was last used by an upstream build; this binary does not run on it, restore a dump made by this fork` | The ledger holds no fork migration, so an upstream build created it. | Restore a dump made by this fork, or use the upstream build on this database. |
| `applied migration changed` | `migration NNNN: database records file "<name>" but this binary has <name>; published migrations must not be renamed` or `migration NNNN (<name>): checksum changed since it was applied (database "<12 hex>", file <12 hex>); published migrations must not be edited` | A migration file of this binary differs from the one that was applied. | Run the release that applied it (the binary you have was built from a changed file). Do not edit `schema_migrations`: that is unsupported. |
| `unknown migration applied` | `migration NNNN ("<name>") is applied in the database but missing from this binary; run the release that applied it, or set ARTEX_SCHEMA_ALLOW_NEWER=NNNN to start anyway (see docs/schema-upgrade.md)` | A newer release applied a migration this binary does not ship (a downgrade). | Run the release that applied it, or restore the pre-upgrade dump. Only to keep the service up meanwhile: the override below. |
| `migration out of order` | `migration <file> (NNNN) is older than the applied migration "<file>" (NNNN); migrations must be appended, never inserted` | This binary ships a migration that is not applied and is numbered below one that is, in the same range. | Run a binary built from `main` (or the release that applied the newer one). Never renumber a shipped file. |

The checks run in this order and the first that matches wins: empty ledger, no fork-range row, changed name or
checksum, unknown version, out of order. The order rule applies **inside each numbering range** (`0001`-`4999`
upstream, `5000`-`9999` fork), and an unknown version counts as applied: a pending migration below an unknown
applied one of its range is refused even when the override is set.

An upstream-only ledger (for example `0001` to `0005` and no fork row) is refused as "created by another binary"
even with the override set.

### The override `ARTEX_SCHEMA_ALLOW_NEWER`

`ARTEX_SCHEMA_ALLOW_NEWER=<highest version accepted>` (four digits, for example `5002`) lets the start continue on a
database in which **unknown** migrations are applied, as long as every unknown version is at most the value.

- It permits only the "unknown migration" case. It does not permit an empty or foreign ledger, a changed
  name or checksum, or an out-of-order migration.
- **It expires at the next newer version:** an unknown version above the value is refused again, so a forgotten
  setting stops working when the next release adds a migration.
- This binary then runs its own `schema.sql` and its own pending migrations, leaves the unknown rows alone, and
  logs one warning per unknown version **on every start** while the override applies.
- Unset or blank means not set. Any other value that is not four digits (a sign, zero, letters, a different length)
  stops the start before the database is touched with `ARTEX_SCHEMA_ALLOW_NEWER must be a migration version of four
  digits, for example 5002`.
- **Risk.** This binary's `schema.sql` re-creates its triggers and functions, re-adds its CHECK constraints and
  re-runs its conditional data statements over a database that a newer release changed. If the newer release changed
  one of those objects, this start reverts it, and this binary's code may fail on newer columns (for example a NOT
  NULL column without a default).
- **Use it only to keep the service up** while the right binary or a restore is being prepared. Remove it after.

### Lock timeout

Each statement of a migration transaction waits at most **60 seconds** for a lock. After that it fails with
SQLSTATE `55P03`, and the error ends with a hint that names the table:
`another session holds a lock on <table>: a running ARTEX instance of an older release, pg_dump, or an open psql
transaction; stop it and restart`. Stop that session and start again; the failed transaction was rolled back. A
deadlock (SQLSTATE `40P01`) is retried up to 4 times, after 100 ms, 250 ms, 500 ms and 1 s.

`schema.sql` itself, and the wait for the schema advisory lock, still wait **without a limit**.

### Rollback

Redeploy the previous binary. Nothing else is needed for 0.5.13m and older: they never read `schema_migrations`, run
their own `schema.sql` (same bytes as 0.5.13m) and still start. The ledger stays valid if the new binary returns.

To remove the table (only to restore the exact catalog of a database that never had it), stop every ARTEX instance and
run the cleanup SQL below on the database that was rolled back. It drops the ledger **only when it holds exactly the
baseline versions** (1, 2, 3, 5001) and otherwise raises an error and changes nothing: a later migration has run, or
the ledger was edited, and then the right way back is the pre-upgrade dump. Once `5002` has been applied this recipe
refuses: run the `5002` drop first (see "Migration 5002"). The next start of the new binary adopts the
baseline again.

```sql
BEGIN;
SELECT pg_advisory_xact_lock(7337741001);
DO $$
BEGIN
  IF (SELECT array_agg(version ORDER BY version) FROM schema_migrations) IS DISTINCT FROM ARRAY[1,2,3,5001] THEN
    RAISE EXCEPTION 'schema_migrations is not the baseline set: do not drop it; restore the pre-upgrade dump';
  END IF;
END $$;
DROP TABLE schema_migrations;
COMMIT;
```

Save it as `rollback-cleanup-ledger.sql` and run it with `ON_ERROR_STOP` (the same text is
`db/testdata/upgrade/rollback-cleanup-ledger.sql` in the source tree):

```bash
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 < rollback-cleanup-ledger.sql
```

Do **not** drop `schema_migrations` by hand on a database that has later migrations: they would run again.

**Downgrade rule for future releases.** A binary older than the newest applied migration is refused ("unknown
migration applied") unless the override is set. Each fork migration states in its first line whether an older binary
is safe on a database that has it (`-- downgrade-safe: yes|no: <reason>`), and the CHANGELOG repeats it. After a
`no`, restore the pre-upgrade dump.

### Migration 5002 (Tool RAG observation)

`db/migrations/5002_tool_rag_observations.sql` creates the table `tool_rag_observations` and one index
(`CREATE ... IF NOT EXISTS`, first line `-- downgrade-safe: yes: additive table, older binaries do not read it`). The
table references `tasks(id) ON DELETE CASCADE`, so the migration takes a short lock on `tasks`. Design and runbook:
[adr/0005-tool-rag-observation.md](adr/0005-tool-rag-observation.md), [tool-rag-observation.md](tool-rag-observation.md).

- **Ledger.** After a start the ledger ends with `5002`: `0001`, `0002`, `0003`, `5001`, `5002`. The reference catalog
  has one more table (59 tables).
- **Older binary.** A binary that does not ship `5002` refuses the database ("unknown migration applied") unless it is
  started with `ARTEX_SCHEMA_ALLOW_NEWER=5002`. The table is then ignored.
- **Drop recipe.** To remove the table after going back, stop every ARTEX instance and run
  `db/testdata/upgrade/rollback-drop-5002.sql` with `ON_ERROR_STOP`. It acts only when `5002` is the newest fork
  migration in the ledger, and raises, changing nothing, on any other ledger (then restore the pre-upgrade dump):

  ```sql
  BEGIN;
  SELECT pg_advisory_xact_lock(7337741001);
  DO $$
  BEGIN
    IF (SELECT max(version) FROM schema_migrations WHERE version >= 5000) IS DISTINCT FROM 5002 THEN
      RAISE EXCEPTION '5002 is not the newest fork migration: restore the pre-upgrade dump instead';
    END IF;
  END $$;
  DELETE FROM schema_migrations WHERE version = 5002 AND name = '5002_tool_rag_observations.sql';
  DROP TABLE IF EXISTS tool_rag_observations;
  COMMIT;
  ```

- **Order.** Run this drop **before** the ledger cleanup above: the cleanup refuses while `5002` is recorded.
- **Archives.** Task archiving deletes the task's rows of the table; restoring an archive does not bring them back.

### Restores

Restore **only a full dump** (all of `scripts/backup.sh`'s output) into a **freshly created database**. Data and
ledger then come from the same moment, and the pending migrations run once. A partial restore, a data-only restore
and manual edits of `schema_migrations` are **unsupported**: the ledger would no longer describe the data, and a
migration could run twice or be skipped.

**Check the database name after a restore.** If the DSN names a database that does not exist on the server, ARTEX
creates it and starts as a fresh installation: an empty schema, a new ledger, the first-run setup. It does not
refuse. This is older behaviour, not new in this release, but after a restore into a database with another name it
looks like data loss. If the console asks for first-run setup after an upgrade or a restore, stop and compare the
database name in `ARTEX_PG_DSN` (or `POSTGRES_DB` with Docker Compose) with the one you restored into.

With Docker Compose, `ARTEX_SCHEMA_ALLOW_NEWER` is read from `.env` and passed to the container by
`docker-compose.yml`; an empty value means it is not set.

## What the validation covers

Starting points ("shapes"), each loaded into a temporary PostgreSQL 16 database with a data
fixture of synthetic rows:

| Shape | What it is |
|---|---|
| `r0509` | `db/schema.sql` as released in 0.5.9m, 0.5.10m and 0.5.11m (identical bytes) |
| `r0512` | `db/schema.sql` as released in 0.5.12m (0.5.13m adds `findings.ticket_id` to it) |
| `r0513` | `db/schema.sql` as released in 0.5.13m: the schema that is now frozen, and the last release without the migration ledger |
| `legacy` | **synthetic**: `r0509` plus `legacy-mutations.sql`, which puts back the old forms that the upgrade branches of `schema.sql` look for. It is not a recorded release |

Checks (test names are in `db/`, `db/dbtest/` and `server/`):

- **Fixtures are what they claim to be.** The sha256 of each recorded schema is a constant in
  test source and in `db/testdata/upgrade/manifest.json`; a test fails if a hashed file holds a
  CR byte, and `TestGitHistoryFixtures` compares each fixture with
  `git show <commit>:db/schema.sql` by commit id.
- **Fresh database** after startup (`Open` alone: it adopts the baseline migrations and creates the ledger table
  `schema_migrations`) has the catalog listed in `db/testdata/upgrade/reference-catalog-pg16.txt` and the ledger
  versions 0001, 0002, 0003, 5001 and 5002.
- **Upgrade keeps rows.** For each shape, startup removes no row and changes no value except
  what `expected-changes.json` lists exactly (see below). The fixture must have rows in every table or
  a stated exemption. Sequences do not go backwards.
- **Convergence.** After startup, each shape has the same catalog as a fresh database.
- **Repeated startup** changes no catalog object and no row, except `agents.updated_at` of the built-in agents.
- **Atomic schema apply.** A failing last statement, and a row that violates a re-added CHECK, leave the
  catalog, the table digests and the sequence values as they were.
- **Locking.** Four concurrent `Open` calls give the result of one; `Open` waits while a
  transaction holds the advisory lock; the key is `7337741001`.
- **Archives.** A format-3 task archive built on an `r0509` database restores after the upgrade (the restore
  path fills the new NOT NULL column `findings.ticket_id` with `''`); a
  column guard fails when an archived column is dropped, renamed, retyped, or made NOT NULL without a
  restore default.
- **Startup writers outside `schema.sql`** (`server/upgrade_seed_test.go`): the seeders that run
  at server start, run twice on an upgraded database.
- **Schema-change gate** (`TestUpgradeSchemaChangeGate`, no database needed): `db/schema.sql`,
  the reference catalog and the list of embedded migrations are pinned by hash. `TestMigrationPins` pins every
  migration file (name, sha256, downgrade-safe flag, and for upstream files the git blob id).
- **Runner** (`TestMigration*`, `TestLedger*`, `TestLoadMigrations*`, on temporary databases): the refusal order and
  each refusal, the override, the baseline adoption and its rollback on failure, lock timeout, deadlock retry,
  concurrent starts, the load-time lint, the transaction marker, and error and log text free of row content. Also in
  `db/` but outside the gate's name pattern: `TestEnsureWrappersRunEmbeddedMigrations` (the `EnsureLLM*Table` wrappers
  produce the shape of a full `Open`) and `TestBaselineSetIsFrozen`.
- **Rollback** (`TestUpgradeRollbackCleanupDropsABaselineLedger`, `TestUpgradeRollbackCleanupRefusesAnyOtherLedger`,
  `TestUpgradeRollbackToTheReleaseWithoutTheLedgerAndForward`): the guarded cleanup SQL, and the way back to a
  database without the ledger and forward again.
- **Test safety.** A guard (`db/dbtest/guard.go`) runs before every create, load and drop. Fixture rows
  carry the marker `CANARY-D2-ROW`; tests check that neither it nor the server password reaches any output.

### What it does not cover

- Only linux/amd64 and PostgreSQL 16 were run. Other platforms and PostgreSQL versions are untested.
- Releases older than 0.5.9m are not recorded shapes. `legacy` approximates them and is reverse-engineered
  from the upgrade branches of `schema.sql`.
- The archive test uses the current snapshot code on old table shapes, not an old binary.
- The startup-writer test uses the current seeders on a state rewound to 0.5.10m, not an old binary.
- The catalog listing leaves out owners, grants, comments, views, user types, tablespaces, row-level security,
  rewrite rules, extended statistics, event triggers, storage parameters and sequence persistence
  (the full list is the comment above `catalogQueries` in `db/dbtest/catalog.go`).
- Atomicity covers the schema DDL only. `seedBuiltins` runs outside that transaction, and PostgreSQL does
  not roll sequences back.
- **No drift report.** The runner detects a database it does not understand by its ledger (an unknown, changed,
  out-of-order, empty or foreign ledger). It does **not** detect an object that kept its name but has another
  definition (`IF NOT EXISTS` checks names only), a hand-made object or a missing trigger. A drift report against the
  reference catalog was deferred to the backlog (ADR 0004). Only the recorded-shape suite and the dry run on a copy
  show such differences.
- **Migration `5002` is the first fork migration after the baseline** (Phase D.3). The baseline files change nothing on
  a database a release has started; `5002` adds a table. Upstream `0004` (an index on `llm_usage`) is a separate pull
  request. Upstream `0005` is not ported.
- **The load-time lint is textual, not a SQL parser.** It does not catch transaction control inside a procedure that is
  called (`CALL p()` where `p` commits; PostgreSQL itself refuses that inside a transaction block), a statement
  built with `EXECUTE`, a file written for `standard_conforming_strings = off`, or `psql` meta-commands. The
  runtime marker (see "Changing the schema") is the second line and detects a file that ended the transaction after it
  ran; the file's effects up to that point then stay committed.
- Local gate evidence is on linux/amd64 with PostgreSQL 16; the last gate run (Phase D.3, with migration `5002`)
  printed `upgrade-gate: tests passed=130 skipped=0 failed=0 (minimum passed: 130), go test exit 0`. In that run the
  git-history comparison and the migration tag check were `NOT RUN` (the container had no `.git`); they were run
  separately on the host, where the tag check found no release tag with a migration yet. The recorded shapes are
  `r0509`, `r0512`, `r0513` and the synthetic `legacy`.
- The dry run on a restored copy of a real database (below) was run once for stage 1, on 2026-11-19, on a copy of one
  database that predates 0.5.12m (51 tables). Startup created the six missing tables, changed no row count in any
  existing table, left no catalog difference to the reference, and a second startup changed nothing. It compares
  row counts and the catalog, not row values, and one database is not every database. The run for stage 2 (the
  ledger and a rollback rehearsal) is described in the dry-run section.
- The rollback test simulates the published 0.5.13m binary (the 0.5.13m schema plus the current seeding); the
  rehearsal with the real binary is part of the dry run below.
- The first hosted CI run on this change is still to come. The git-history comparison and the migration tag check need
  a `.git` directory and a full-depth checkout with tags.

## Running it

The tests read the database **only** from `ARTEX_PG_DSN`; the config file is never used by a test binary.

```bash
ARTEX_PG_DSN='postgres://USER:PASS@HOST:5432/DB?sslmode=disable' scripts/upgrade-gate.sh
```

- The DSN is a `postgres://` URL naming a base database `DB` made of `[a-z0-9_]` that does not contain `_upg_`.
  Use a throw-away PostgreSQL 16 server (for example the `postgres:16-alpine` image, as in
  `.github/workflows/ci.yml`), never the database of a real installation.
- The role needs `CREATEDB` and `CREATEROLE`: the tests create and drop temporary databases and a canary login role.
- Each package adds its suffix to `DB` (`_db`, `_dbtest`, `_server`). Temporary databases are named
  `<DB>_<package>_upg_<case>` and canary roles `<DB>_<package>_upgrole_<8 hex>`; the tests touch no other name.
- **One run per base database at a time.** The temporary names are stable and a test drops a database of its
  name before it creates it, so two runs on one server and one `DB` destroy each other's databases. Give a
  second run its own `DB`.
- With `ARTEX_PG_DSN` unset the existing database tests skip and the upgrade tests **fail**; nothing falls back
  to the config file.
- `scripts/upgrade-gate.sh` runs the upgrade tests and the migration runner tests (`TestUpgrade*`, `TestMigration*`,
  `TestLedger*`, `TestLoadMigrations*`) with `go test -json`, and fails unless `go test` exits 0, at
  least `MIN_UPGRADE_TESTS` top-level tests passed and **none** skipped (a skipped test reads as passed
  in plain `go test`). It then runs `TestGitHistoryFixtures` and the migration tag check: every file of
  `db/migrations/` that a release tag (`v*`) contains must exist byte-identical at `HEAD`, and the upstream files
  (numbered below 5000) must equal the pinned upstream commit `278de8b2e09a3702f77bbbd065ef35e4b7fdb1d0` when
  that object is in the repository (it is never fetched). Without a `.git` directory these comparisons are
  reported `NOT RUN`; `UPGRADE_GATE_REQUIRE_GIT=1` turns that into a failure (hosted CI sets it). Raise
  `MIN_UPGRADE_TESTS` with the tests, never lower it to make the gate pass. `scripts/upgrade-gate.sh
  --migration-tags [REPO_DIR]` runs only the tag check, with no database; `TestUpgradeGateMigrationTagCheck`
  tries it on synthetic repositories.
- CI runs the gate as the step "Schema upgrade test gate" after `go test -p 1 ./...`.

The gate currently expects **130** passing top-level tests and zero skips.

Whole suite: `go test -p 1 ./...` with the same variable. Four `db` tests
(`TestCompanyScopeAttribution`, `TestPoolProfilesOrder`, `TestProfileRetryRoundTrip`,
`TestProfileRetryClampedOnSave`) fail on a second run against the same database. That is older than this change; use a
fresh database for each full run.

## Changing the schema

`db/schema.sql` is **frozen** at the bytes of release 0.5.13m (sha256 `951f3d0c...f963a`). Every later schema change is
a numbered migration in `db/migrations/`, recorded in the ledger table `schema_migrations` with its sha256 (see
`db/migrate.go` for the runner and its rules). A change to `db/schema.sql`, a migration or the reference catalog
fails `TestUpgradeSchemaChangeGate` or the migration pin tests until this procedure is done (it is the failure
message of the gate test):

1. Add `db/migrations/5NNN_<slug>.sql` with the next free number **on `main`** (never reserve numbers on a branch;
   a collision is resolved by renumbering the unmerged file). Upstream files keep their bytes in `0001`-`4999`; fork
   files use `5000`-`9999`. First line of a fork file: `-- downgrade-safe: yes|no: <reason>`. Idempotent where
   natural (only the baseline set must be). A migration that is on `main` is never edited: a new one corrects it. A
   migration must not change an object that `schema.sql` re-creates on every start (the CHECK pairs, the triggers,
   the four functions, the data statements) unless step 3 is done in the same commit. The loader checks every file
   when the program starts (`loadMigrations`) and rejects:
   - `CONCURRENTLY` and `VACUUM` anywhere outside comments (also inside a string or a `DO` block; a `VACUUM` in a `--`
     comment inside a dollar-quoted body is reported too). They cannot run in the migration transaction.
   - A transaction-control statement at statement level: `COMMIT`, `ROLLBACK`, `SAVEPOINT`, `RELEASE`, `ABORT`,
     `BEGIN`, `END`, `START TRANSACTION`, `PREPARE TRANSACTION`. The check looks at the first word or two of each
     statement, split at `;`, outside comments, string literals, quoted identifiers and dollar-quoted bodies. A
     PL/pgSQL `BEGIN ... END` inside a `DO` block or a function body is fine, and so is the `BEGIN ATOMIC ... END` body of
     an SQL-standard function. The error reads `migration file <name>: COMMIT is not allowed (a migration runs inside
     the runner's transaction and must not control the transaction)` and stops the start before the database is touched.
   - Gaps of the check: transaction control inside a called procedure, a statement built by `EXECUTE`, a file that needs
     `standard_conforming_strings = off`, `psql` meta-commands, and a transaction statement that does not start a
     statement (for example after a label).
   - Second line, at run time: the runner sets `SET LOCAL artex.migration_tx = 'on'` before each file and checks it after.
     A file that ended the transaction anyway fails with "the file ended the migration transaction ... the ledger row
     was not written" and no ledger row, but what the file did before its `COMMIT` stays committed. A migration must not
     define a setting named `artex.migration_tx`.
2. Add the file to `migrationPins` (name, sha256, downgrade-safe flag) in `db/migrate_pins_test.go` and to
   `current.migrations` in `manifest.json`. Upstream files (`0001`-`4999`) also get their blob id.
3. Only for an object that `schema.sql` re-creates on every start: edit that statement in `schema.sql` in the **same
   commit** as the migration that changes it (the only allowed edit of `schema.sql`), update the `schema.sql` sha256
   (`db/schema_gate_test.go` and `manifest.json` `current`) and change `TestUpgradeFrozenSchemaIsTheNewestRecordedShape`,
   which holds the equality of `schema.sql` with the `r0513` fixture while the schema is frozen.
4. Record the previous release as a shape if its state is not recorded yet:
   `git show <commit>:db/schema.sql > db/testdata/upgrade/schema-<sha12>.sql` and, for a release that ran the runner,
   its applied migrations. A shape of a release without the runner also carries the historical Go DDL fixture
   (`goddl_*` in its `manifest.json` entry, never regenerated: the Go DDL constants and their pin are gone, the
   same DDL is now in `db/migrations/`). Add the shape to `recordedShapes` in
   `db/upgrade_fixture_test.go` and to `manifest.json`. Never edit a fixture. Skip this step when that release is
   already a recorded shape.
5. Extend the data fixtures and `expected-changes.json` for each shape, the added `schema_migrations` rows included.
   Run `go test ./db -run '^TestUpgradeRowsPreserved'`: it lists every difference that has no entry. A new column
   with a constant default needs one `value` entry (`"column_added": true`) per row of that table in the fixture of
   each older shape; a data fixture needs a change only when a table has no rows yet.
6. Regenerate the reference catalog in a PostgreSQL 16 container:
   `ARTEX_UPGRADE_REGENERATE=1 go test ./db -run TestRegenerateUpgradeReference`
   (with `ARTEX_PG_DSN` set as above; the test logs the new hash). The file is written into the working
   tree the test runs in: from a container, copy it back out. Review it as a diff: it is the new definition of "a
   correct database". Update the reference sha256 in `db/schema_gate_test.go` and the `current` entry of
   `manifest.json`.
7. Do what the schema change needs beyond the matrix, none of which the gate failure names:
   - A new NOT NULL column of a table in a task archive needs an entry in `archiveRestoreDefaults`
     (`db/upgrade_archive_test.go`) naming the normaliser of the restore path that fills it (`TestUpgradeArchiveColumnsSurviveUpgrade`
     fails otherwise), and the digest difference "column ... added" is listed in `archiveKnownUpgradeDiffs`.
   - A test that opens a database of its own must take it from `dbtest.TempDatabase` (name
     `<DB>_upg_<case>`), so that the guard covers it and the leftover check finds it.
   - Re-check the `file:line` causes in `expected-changes.json` that point into `db/schema.sql` or `db/migrate.go`
     below an edited line (the `schema_migrations` entries cite `db/migrate.go:681; db/migrate.go:690-701`, the ledger
     insert and `adoptBaseline`).
   - Raise `MIN_UPGRADE_TESTS` in `scripts/upgrade-gate.sh` when tests were added, and update the counts and shape
     descriptions in this file and in the CHANGELOG entry.
8. The CHANGELOG lists the migration with its downgrade-safe line.

Then run the gate.

**What the first fork migration after the baseline (`5002`) showed that the steps above do not say:**

- A table that task archiving must clear goes into the delete list in `db/task_archives_restore.go` (the one that holds
  `skill_usage` and `tool_usage`) when its rows hang on a task, because archiving keeps the `tasks` row and so a
  foreign-key cascade does not fire. Add a test that archives and restores.
- A new table raises the table count of the reference (`expectedTables` in `db/upgrade_matrix_test.go`: 58 to 59), and
  each recorded shape needs an exemption for a table that does not exist before startup.
- Ship a guarded drop recipe with the migration (`db/testdata/upgrade/rollback-drop-5002.sql`), test it, run it before
  the ledger cleanup, and list it in `.github/CODEOWNERS` next to the cleanup SQL.
- Tests that assumed a ledger of only the baseline rows must use the embedded set (`requireEmbeddedLedger`); the
  ledger cleanup refuses once a later migration is applied, so rollback tests run the drop first.
- Synthetic migrations in tests must stay clear of real fork numbers (they were renamed `5902` and `5903` because the
  real `5002` collided).
- Raise `MIN_UPGRADE_TESTS` for the new migration tests (128 to 130).

Rollback of the first release with the ledger needs no step (see "Rollback" above). The cleanup SQL is
`db/testdata/upgrade/rollback-cleanup-ledger.sql`, run by `TestUpgradeRollbackCleanupDropsABaselineLedger` and
`TestUpgradeRollbackCleanupRefusesAnyOtherLedger`, and rehearsed end to end by
`TestUpgradeRollbackToTheReleaseWithoutTheLedgerAndForward`.

The suite has been exercised by two real schema changes: `findings.ticket_id` (merged from `main` before 0.5.13m: no new
fixture, one regenerated reference line, three pinned hashes, six expected-changes entries, one archive restore default,
and moving the one test that opened its own database onto `dbtest.TempDatabase`) and the adoption of the migration
ledger itself (a new shape `r0513`, the table `schema_migrations` in the reference, four `added` entries per shape, 58
tables in the reference).

**Integrity controls for migration files.** A released file is checked by: the pins test (`TestMigrationPins`, no
database); `.gitattributes` (`db/migrations/*.sql -text`: no line-ending conversion, because the checksum is over
bytes); the gate's tag check (a file that a `v*` tag holds must be unchanged, undeleted and unrenamed at `HEAD`, and the
upstream files must equal the pinned upstream commit); and `.github/CODEOWNERS`, which names the owner for
`db/migrations/`, `db/migrate.go`, `db/db.go`, `db/schema.sql`, `db/migrate_pins_test.go`, `scripts/upgrade-gate.sh`,
`.github/workflows/`, `.gitattributes`, `.github/CODEOWNERS`, `db/upgrade_gatescript_test.go` and the cleanup SQL. One
pull request could still change a file, its pin and the gate together. **What the repository cannot enforce is the
owner's own setting:** required review by code owners on `main`, and protection of `v*` tags (a deleted or moved tag
weakens the tag check). Neither was verified from the repository.

## The expected-changes file

`db/testdata/upgrade/expected-changes.json` lists, per shape, **every** difference startup may make to the rows of an
existing database. The file is read strictly (unknown fields fail). There are no whole-table and no whole-column
entries.

| Kind | Fields | Meaning |
|---|---|---|
| `value` | `table`, `pk`, `column`, `before`, `after` | one column of one row changes. `before`/`after` are the text form of the value; JSON `null` is SQL NULL |
| `deleted` | `table`, `pk` | the row is removed |
| `added` | `table` and `pk` (one named row), or `table` and `count` (a number of rows that no `pk` entry names) | rows are added |

Every entry has:

- `cause`: `file:line` references (several separated by `; `).
- `class`: `intended`, `accepted` or `defect`.
- `note`: required for `accepted`: the decision and who took it.

Rows added to a table that does not exist before startup (the four rows of `schema_migrations`) are `added` entries
with a `pk` each (`{"version": "1"}`, `{"version": "2"}`, `{"version": "3"}`, `{"version": "5001"}`), and one more, `{"version": "5002"}`,
for migration `5002`.

Special forms: `"after": "@startup"` stands for a `timestamptz` not earlier than the start of startup (the `set_updated_at`
trigger); `"column_added": true` on a `value` entry means the column does not exist before startup, so there is no
`before`.

A **`defect` entry fails the gate** ("awaiting decision") until the owner decides: fix the code, accept the
difference (change the class to `accepted` and record the decision in `note`), or re-plan. The implementer never
picks `intended` or `accepted` alone. An unexpected difference is reported as a candidate defect, not added to the file.

## Accepted behaviours

Decided by the owner on 2026-11-19 and recorded as `accepted` entries.

1. **Startup restores the code's name and description of a built-in agent.** The product does not let a built-in agent be
   renamed (`db/config.go:610` updates custom agents only), so only an out-of-band SQL edit is reverted
   (`db/db.go:258`).
2. **Startup re-creates a missing `task_asset_links` row** when `assets.task_ids` lists a live task
   (`db/schema.sql:712-717`). It only inserts (source `legacy`) and never deletes; `task_ids` is the source of truth.
3. **Restoring a task archive sets `tasks.updated_at` and `explorations.updated_at` to the restore time.** The
   `set_updated_at` triggers override the archived value. All other archived columns round-trip.

Other facts that the runs established and that an operator may notice:

- Every start advances some sequences (`agents_id_seq`, `agent_prompt_vars_id_seq`, `mcp_servers_id_seq`) and rewrites
  `agents.updated_at` of the six built-in agents. Nothing else changes on a repeated start.
- Startup writes 25 one-time settings keys: the 19 `const flag` keys listed in `flags-v0.5.*.txt`, and six more listed
  in `flags-README.txt`.
- Session-level and transaction-level advisory locks share the key `7337741001`: `Open` waits while an archive
  transaction holds it.

## Dry run on a restored copy of a real database

`TestDryRunRestoredCopy` runs startup twice on a copy of a real database and prints a report with row counts per
table before and after, the state of the migration ledger before and after (versions only, never a name or a checksum)
and the catalog differences against the reference **by object name**. It prints no row value. On a copy of a database
that has no ledger the report contains these lines (taken from a run on a synthetic copy):

```
table schema_migrations: absent -> 4 rows
catalog: no difference to the reference
ledger schema_migrations: absent -> present, versions 0001 0002 0003 5001
```

and on the second start `ledger schema_migrations: present, versions 0001 0002 0003 5001 -> present, versions 0001 0002 0003 5001`.
The test fails unless the ledger is exactly the baseline set after the first start of a database without a ledger,
holds at least the baseline otherwise, and is unchanged by the second start.
It is not part of the gate: it needs its own server and does nothing unless `ARTEX_UPGRADECHECK_DSN` is set. It
fails if startup removes rows from a table or if the second start changes a row count. One removal is expected and
reported as such: the obsolete prompt variables (`EngagementTitle`, `CoverageGaps`, `Now`) that every start deletes
from `agent_prompt_vars`; exactly the rows present before startup may disappear.

The check refuses to run unless the server holds only `template0`, `template1`, `postgres` and databases named
`*_upgradecheck`, so it cannot be pointed at a shared server.

1. Take the dump. `scripts/backup.sh` gives the file the caller's umask, and the dump holds
   `auth.password_hash`: run `umask 077` first, or `chmod 600` the file.
2. Start a dedicated container with its **own volume and port** (the names below are examples; use `podman` the same
   way if you prefer). Use the role name of the source database as `POSTGRES_USER`, so that ownership statements
   in the dump find their role:
   ```bash
   docker run -d --name artex-upgradecheck -e POSTGRES_USER=artex -e POSTGRES_PASSWORD='CHOOSE-A-PASSWORD' \
     -p 127.0.0.1:55432:5432 -v artex-upgradecheck-pg:/var/lib/postgresql/data postgres:16-alpine
   ```
3. Create the target database, drop the one the image made, and check the inventory. A stock postgres image also
   creates a database named after the user (`artex` here), and it must be dropped:
   ```bash
   docker exec artex-upgradecheck psql -U artex -d postgres -c 'CREATE DATABASE restored_upgradecheck'
   docker exec artex-upgradecheck psql -U artex -d postgres -c 'DROP DATABASE artex'
   docker exec artex-upgradecheck psql -U artex -d postgres -Atc 'SELECT datname FROM pg_database ORDER BY 1'
   ```
   The last command must list only `postgres`, `restored_upgradecheck`, `template0`, `template1`.
4. Restore the dump into it:
   ```bash
   gunzip -c backups/artex-db-<timestamp>.sql.gz | docker exec -i artex-upgradecheck \
     psql -U artex -d restored_upgradecheck -v ON_ERROR_STOP=1
   ```
5. Run the check from a checkout of the commit under test (Go installed; from a container instead, put it on the
   same Docker network and use the container name and port 5432 in the address):
   ```bash
   ARTEX_UPGRADECHECK_DSN='postgres://artex:CHOOSE-A-PASSWORD@127.0.0.1:55432/restored_upgradecheck?sslmode=disable' \
     go test ./db -run TestDryRunRestoredCopy -v -count=1
   ```
6. Read the report. Then **remove the container and the volume**, and the dump copy, because the dump holds the
   password hash:
   ```bash
   docker rm -f artex-upgradecheck && docker volume rm artex-upgradecheck-pg
   ```

### Rollback rehearsal

The dry run for stage 2 also rehearses the way back with the **published 0.5.13m binary** on the same copy (the test
`TestUpgradeRollbackToTheReleaseWithoutTheLedgerAndForward` only simulates that binary). Steps:

1. Run the dry run (above). The copy now holds the ledger.
2. Start the published 0.5.13m binary against the copy: the release archive, checked against its `SHA256SUMS`, in a
   container on an internal network where only the database container is reachable, with its own data directory.
   Stop it as soon as it logs that it is listening: its start-up writes to the database and it would resume tasks.
3. Run the dry run again. The log must show `[migrate] up to date` and the report no change.
4. Remove the containers, the volume, the network and the dump copy.

Result (2026-11-19, one database older than 0.5.12m, 51 tables, restored into a dedicated local container):

| Step | Ledger | Change in the database |
|---|---|---|
| after the restore | absent | |
| 1. dry run with this release | `0001 0002 0003 5001` | seven tables added: six missing tables with no rows, and `schema_migrations` with four rows; no row count changed in any existing table; catalog equal to the reference; adoption took about 0.1 s |
| 2. published 0.5.13m binary | unchanged | it started, loaded its tasks and listened; its own start-up added 3 rows to `server_logs`, 1 to `settings` and 4 to `tools` |
| 3. dry run with this release again | unchanged | `[migrate] up to date`; no row count changed |

So going back to 0.5.13m needs no step, and coming forward again is clean. Limits: row counts and the catalog were
compared, not row values; one database; the old binary ran for a few seconds only. The cleanup SQL was not part of
this rehearsal; it is covered by tests on recorded shapes.

## What is still missing

- **A drift report** (a same-named object with another definition, a hand-made object, a missing trigger). Deferred to
  the backlog on 2026-11-19; the recorded-shape suite and the dry run are the only evidence until then.
- **Upstream `0004`** (an index on `llm_usage`) is a separate pull request; fork migration `5002` is in. Upstream `0005`
  is not ported; it is planned for when the fork next ports an upstream migration numbered above it or
  the traffic-rewrite feature, whichever comes first.
- Platforms other than linux/amd64 and PostgreSQL versions other than 16, and Windows checkouts (a CRLF conversion
  would change a checksum; `.gitattributes` forbids it).

Design: [adr/0004-versioned-migrations-stage-2.md](adr/0004-versioned-migrations-stage-2.md).
