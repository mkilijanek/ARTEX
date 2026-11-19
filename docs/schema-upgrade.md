# Schema upgrade validation

ARTEX has no versioned migrations. `artex` re-runs the embedded `db/schema.sql` on every
start (`CREATE ... IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`, CHECK constraints dropped and
re-added, triggers recreated), then seeds the built-in rows. "Restart equals migrate."

Phase D.2 stage 1 does not change that. It adds **tests, fixtures and a gate** that show,
on temporary databases, what that mechanism does to an existing database, and that make
every later schema change pass a recorded procedure. Decision:
[adr/0003-schema-upgrade-validation.md](adr/0003-schema-upgrade-validation.md). Stage 2
(a versioned migration runner) is only proposed:
[adr/0004-versioned-migrations-stage-2.md](adr/0004-versioned-migrations-stage-2.md).

Stage 1 refers to issue #24 and does **not** close it: the issue asks for migration
order and checksums, and there are no versioned migrations to order or checksum yet.

## What the validation covers

Starting points ("shapes"), each loaded into a temporary PostgreSQL 16 database with a data
fixture of synthetic rows:

| Shape | What it is |
|---|---|
| `r0509` | `db/schema.sql` as released in 0.5.9m, 0.5.10m and 0.5.11m (identical bytes) |
| `r0512` | `db/schema.sql` as released in 0.5.12m (the current schema adds `findings.ticket_id` to it) |
| `legacy` | **synthetic**: `r0509` plus `legacy-mutations.sql`, which puts back the old forms that the upgrade branches of `schema.sql` look for. It is not a recorded release |

Checks (test names are in `db/`, `db/dbtest/` and `server/`):

- **Fixtures are what they claim to be.** The sha256 of each recorded schema is a constant in
  test source and in `db/testdata/upgrade/manifest.json`; a test fails if a hashed file holds a
  CR byte, and `TestGitHistoryFixtures` compares each fixture with
  `git show <commit>:db/schema.sql` by commit id.
- **Fresh database** after startup (`Open`, `EnsureLLMRecordsTable`, `EnsureLLMUsageTable`)
  has the catalog listed in `db/testdata/upgrade/reference-catalog-pg16.txt`.
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
  the Go DDL constants and the reference catalog are pinned by hash.
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
- Nothing is detected **at runtime**: an older binary on a newer database, a foreign database, or an object
  that kept its name but has another definition (`IF NOT EXISTS` checks names only). Restore the pre-upgrade dump
  before running an older binary.
- The dry run on a restored copy of a real database (below) has not been run on real data yet.
- The first hosted CI run on this change is still to come. The git-history comparison needs a `.git` directory
  and a full-depth checkout; the container run behind the numbers in the change did not have one.

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
- `scripts/upgrade-gate.sh` runs the upgrade tests with `go test -json`, and fails unless `go test` exits 0, at
  least `MIN_UPGRADE_TESTS` top-level tests passed and **none** skipped (a skipped test reads as passed
  in plain `go test`). It then runs `TestGitHistoryFixtures`. Without a `.git` directory the comparison is
  reported `NOT RUN`; `UPGRADE_GATE_REQUIRE_GIT=1` turns that into a failure (hosted CI sets it). Raise
  `MIN_UPGRADE_TESTS` with the tests, never lower it to make the gate pass.
- CI runs the gate as the step "Schema upgrade test gate" after `go test -p 1 ./...`.

Whole suite: `go test -p 1 ./...` with the same variable. Four `db` tests
(`TestCompanyScopeAttribution`, `TestPoolProfilesOrder`, `TestProfileRetryRoundTrip`,
`TestProfileRetryClampedOnSave`) fail on a second run against the same database. That is older than this change; use a
fresh database for each full run.

## Changing the schema

Any change to `db/schema.sql`, the Go DDL constants (`llmRecordsSchema`, `llmRecordsMigrate`, `llmUsageSchema`)
or the reference catalog fails `TestUpgradeSchemaChangeGate` until this procedure is done (it is the failure
message of that test):

1. Record the previous release's `db/schema.sql` as a new fixture by commit id:
   `git show <commit>:db/schema.sql > db/testdata/upgrade/schema-<sha12>.sql`. Add it to `recordedShapes` in
   `db/upgrade_fixture_test.go` and to `manifest.json`. Never edit a fixture. Skip this step when that release
   is already a recorded shape (its schema hash is already in `recordedShapes`) and the schema has not changed
   since it was released.
2. Extend the data fixtures and `expected-changes.json` for the new shape. Run `go test ./db -run
   '^TestUpgradeRowsPreserved'`: it lists every difference that has no entry. A new column with a constant
   default needs one `value` entry (`"column_added": true`) per row of that table in the fixture of each shape
   (for `findings.ticket_id`: 2 rows, 3 shapes, 6 entries); a data fixture needs a change only when a table has
   no rows yet.
3. Regenerate the Go DDL fixture and the reference catalog in a PostgreSQL 16 container:
   `ARTEX_UPGRADE_REGENERATE=1 go test ./db -run TestRegenerateUpgradeReference`
   (with `ARTEX_PG_DSN` set as above; the test logs the new hashes). The files are written into the working
   tree the test runs in: from a container, copy them back out.
4. Update the three sha256 constants in `db/schema_gate_test.go` and the `current` entry of `manifest.json`.
5. Do what the schema change needs beyond the matrix, none of which the gate failure names:
   - A new NOT NULL column of a table in a task archive needs an entry in `archiveRestoreDefaults`
     (`db/upgrade_archive_test.go`) naming the normaliser of the restore path that fills it (`TestUpgradeArchiveColumnsSurviveUpgrade`
     fails otherwise), and the digest difference "column ... added" is listed in `archiveKnownUpgradeDiffs`.
   - A test that opens a database of its own must take it from `dbtest.TempDatabase` (name
     `<DB>_upg_<case>`), so that the guard covers it and the leftover check finds it.
   - Re-check the `file:line` causes in `expected-changes.json` that point into `db/schema.sql` below the edited line.
   - Raise `MIN_UPGRADE_TESTS` in `scripts/upgrade-gate.sh` when tests were added, and update the counts and shape
     descriptions in this file and in the CHANGELOG entry.

Then run the gate. Review the regenerated reference as a diff: it is the new definition of "a correct database".

The suite has been exercised by one real schema change, `findings.ticket_id` (merged from `main` before 0.5.13m).
The procedure required: no new fixture (0.5.12m was already recorded), one regenerated reference line,
three pinned hashes, six expected-changes entries, one archive restore default, and moving the one test that
opened its own database onto `dbtest.TempDatabase`.

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
table before and after and the catalog differences against the reference **by object name**. It prints no row value.
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

## What stage 2 will add

A versioned migration runner with a ledger table, checksums and ordering, a drift report against the reference
catalog, and a warning or refusal for a database last opened by a newer or foreign binary. Until then there is no
runtime detection of any of these. Design: [adr/0004-versioned-migrations-stage-2.md](adr/0004-versioned-migrations-stage-2.md).
It needs its own plan and approval.
