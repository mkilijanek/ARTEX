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

Issue #24 asks for migration order and checksums. S2-A (release 0.5.15m) delivered both, but shipped no migration that
changes the schema (the baseline files are no-ops on every database a release has started). The pull request S2-C
ships the first real migration, upstream `0004` (the index `idx_llm_usage_ts`), and closes the issue.

## Upgrading to a release with migrations

### Before you start

1. **Take a full dump** (`scripts/backup.sh`; see the README, "Backups"). The first start writes to the database and
   that write is not undone by going back to the old binary (see "Rollback"). Run `umask 077` first or `chmod 600`
   the dump: it holds `auth.password_hash`.
2. **Stop every older ARTEX instance** that uses the database, and make sure **no `pg_dump` is running** against it.
   The baseline migrations take short table locks (`ACCESS EXCLUSIVE` on `llm_records` and `llm_usage`, `SHARE` on
   `task_scope` until the transaction ends). A session that holds a conflicting lock makes the start fail after 60
   seconds (see "Lock timeout"). Migration `0004` takes a longer lock: see "The index build of `0004`" below.
3. **Estimate the size of `llm_usage`.** This release builds an index on it at the first start (next section). Run
   this read-only query on the database:
   ```sql
   SELECT pg_size_pretty(pg_total_relation_size('llm_usage')) AS size,
          (SELECT reltuples::bigint FROM pg_class WHERE oid = 'llm_usage'::regclass) AS estimated_rows;
   ```
   `estimated_rows` is the planner's estimate and reads `-1` when the table was never analysed. The build time grows with
   the table and is not bounded. For a large table, build the index beforehand (see "Pre-building the index").
4. Read the release notes in the CHANGELOG for migrations that state `downgrade-safe: no`.

**`psql` with the shipped Compose file.** From the release after 0.5.17m on, the bundled PostgreSQL has no network and
is reached only through a Unix socket, so every command on this page goes through `docker compose exec`. A database
volume created by that release requires the password for every connection, also inside the container (a volume
created earlier keeps its `trust` lines until you change them; `.env.example` has the steps). Pass the password
through the environment, never on the command line: prefix each `docker compose exec ... psql` command below with
`PGPASSWORD="$POSTGRES_PASSWORD"` and add `-e PGPASSWORD` after `-T`, for example:

```bash
PGPASSWORD="$POSTGRES_PASSWORD" docker compose exec -T -e PGPASSWORD postgres \
  psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc 'SELECT version, name FROM schema_migrations ORDER BY version'
```

### What the first start does

On a database that an earlier release started (no ledger), the start does everything that earlier releases did
(`schema.sql`, the seeding of the built-in rows), and in addition writes **one new table, `schema_migrations`, with
four rows** (`0001`, `0002`, `0003`, `5001`), in one transaction. The four migration files are written so that they
change nothing on such a database: each statement is `IF NOT EXISTS`. If that transaction fails, neither the table
nor a row remains, and the next start tries again. After that, this release applies `0004` (a fifth row, its own
transaction): it **builds the index `idx_llm_usage_ts`**, the first migration that changes the catalog. A database that
already has the ledger from 0.5.15m gets only that one migration.

The start reads the ledger first and refuses a database it does not understand **before it writes anything**,
including before `schema.sql` runs. The order is: read the ledger, `schema.sql`, adopt the baseline (when there is
no ledger), apply later pending migrations (each in its own transaction together with its ledger row), seed the
built-in rows. All of it holds the schema advisory lock (key `7337741001`), so a second instance waits and then finds
a complete ledger.

The write of `schema_migrations` is **not undone when you roll the binary back**. Binaries up to 0.5.14m never read
the table and still start (see "Rollback").

A release that ships migration `5002` (Tool RAG observation, Phase D.3) then applies it in its own transaction, so
after a start the ledger holds **six** rows (`0001 0002 0003 0004 5001 5002`), the last one `5002`, and the database has one more table,
`tool_rag_observations`. See "Migration 5002" below.

A release that ships migration `5003` (detection storage, pull request 1a) applies it the same way, so after a start the
ledger holds **seven** rows (`0001 0002 0003 0004 5001 5002 5003`), the last one `5003`, and the database has four more
tables (`finding_detections`, `detection_rules`, `detection_steps`, `detection_query_results`), a function and a trigger.
They are empty and unused until a later pull request. See "Migration 5003" below.

A failure of an `llm_*` table migration (`0001`, `0002`, `5001`) now **stops the start**. Earlier releases only
logged it and went on.

What the log shows (content-free lines: versions and file names, no data):

| Line | When |
|---|---|
| `[schema] schema.sql sha256=<12 hex>` | every start |
| `[migrate] ledger absent: adopting baseline (4)`, then `[migrate] adopted baseline in <ms>ms` | the first start |
| `[migrate] applied <file> in <ms>ms` | each later migration; for `0004`: `[migrate] applied 0004_llm_usage_ts_index.sql in <ms>ms`. It is written **after** the commit, so nothing is logged while the index builds |
| `[migrate] up to date (upstream=<version>, fork=<version>)` | a successful start |
| `[detect] recovered stopped=<n> queued=<n>` | every start, after the migrations (release with `5003`): detection runs the recovery stopped or left queued; `0` and `0` without runs |
| `[migrate] refused: <label> (version <NNNN>, "<file>")` | a refusal (table below) |
| `[migrate] WARNING: ARTEX_SCHEMA_ALLOW_NEWER=<NNNN>; unknown migration <NNNN> ("<file>")` | every start while the override applies |
| `[migrate] lock timeout on <table> after 60s: another session holds a lock: ...` | a lock timeout |
| `[migrate] deadlock, retry <n>/4` | a deadlock that is being retried |

### The index build of `0004`

This release builds `idx_llm_usage_ts` on `llm_usage (ts)` at the first start (the migration file is
`db/migrations/0004_llm_usage_ts_index.sql`). The daily usage figures and the daily series of the judge statistics
filter `llm_usage` by `ts`; the table gains a row per LLM call and is never purged, so they scanned all of it. What to
expect:

- The build is a plain `CREATE INDEX`. It takes a `SHARE` lock on `llm_usage` for the **whole build**: reads continue,
  every `INSERT`, `UPDATE` and `DELETE` on that table waits. An ARTEX instance that still writes usage rows (an older
  release you did not stop, a second instance) **stalls for the duration**. Stop it first (step 2 above).
- The 60 second lock timeout bounds only the **wait to get the lock**, not the build. The runner sets no statement
  timeout, so the build time is not bounded by ARTEX (a timeout set on the role or the database applies).
- The build runs inside the database open step, **before the HTTP server listens**. On a large table the start looks
  hung: no web page and no further log line. When it ends the log shows `[migrate] applied 0004_llm_usage_ts_index.sql in <ms>ms`
  and then `[migrate] up to date (upstream=0004, fork=5001)`. Wait for them; do not restart the container, that
  cancels the build and the next start begins again. Another session can show the build in `pg_stat_activity` (a
  `CREATE INDEX` statement on `llm_usage`).
- If the build fails, its transaction rolls back: no index, no ledger row, and the next start tries again.

### Pre-building the index

Optional, for a table that is large enough for the lock above to matter. The old release keeps running while you do
it: `CREATE INDEX CONCURRENTLY` does not block writes. Do it **before** the upgrade, with no upgrade in progress, using
`psql` **outside a transaction** (no `-1`, no `BEGIN`, one statement per `-c`):

```bash
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
  -c 'CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_llm_usage_ts ON llm_usage (ts)'
```

Then **check that the index is valid**. A concurrent build that fails (a cancelled statement, a deadlock, a crash)
leaves an `INVALID` index under the same name. The migration's `IF NOT EXISTS` would skip it, the ledger would record
`0004` as applied, and the index would never be used.

```bash
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc \
  "SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass('idx_llm_usage_ts')"
```

It must print `t`. If it prints `f`, drop the index and build it again, and repeat until the check prints `t`:

```bash
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
  -c 'DROP INDEX CONCURRENTLY idx_llm_usage_ts'
```

With a valid index in place, `0004` finds it, does nothing, and the first start of the new release is quick (the
ledger row is still written).

**After any upgrade, confirm the index.** The query below lists the index with its state. One row with `valid = t` is
correct; no row means the index is missing, `f` means it is invalid (drop it as above and build it again):

```sql
SELECT c.relname AS index, i.indisvalid AS valid, i.indisready AS ready
FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
WHERE c.relname = 'idx_llm_usage_ts';
```

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

Releases up to 0.5.14m never read `schema_migrations`, run their own `schema.sql` (same bytes as 0.5.13m) and start on
a database that has the ledger, with or without `0004`. The ledger stays valid if the new binary returns.

**Going back to 0.5.15m** (it reads the ledger and refuses a database with `0004`: "unknown migration applied"). Pick
one of two ways. The index is harmless to the older binary, so both are safe for the data. On a database that also has
`5002` (see "Migration 5002"), the override must be `ARTEX_SCHEMA_ALLOW_NEWER=5002` (it covers both rows); on a
database that also has `5003` (see "Migration 5003") it must be `ARTEX_SCHEMA_ALLOW_NEWER=5003` (it covers all three
rows). The recipe for `0004` comes after the drops of `5003` and `5002`: see "Order of the recipes" below.

1. **Set `ARTEX_SCHEMA_ALLOW_NEWER=0004`** and start 0.5.15m. It starts and logs one warning per start. The override
   has a general risk (the older `schema.sql` runs over a database a newer release changed): read "The override
   `ARTEX_SCHEMA_ALLOW_NEWER`" above. `0004` only adds an index, which the older binary never touches, but the setting stays until you remove it.
2. **Run the guarded `0004` recipe**, then start 0.5.15m with no override. Stop every ARTEX instance first.

```sql
BEGIN;
SELECT pg_advisory_xact_lock(7337741001);
DO $$
BEGIN
  IF (SELECT array_agg(version ORDER BY version) FROM schema_migrations) IS DISTINCT FROM ARRAY[1,2,3,4,5001]
     OR NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 4 AND name = '0004_llm_usage_ts_index.sql') THEN
    RAISE EXCEPTION 'schema_migrations is not baseline + 0004: restore the pre-upgrade dump';
  END IF;
END $$;
DELETE FROM schema_migrations WHERE version = 4 AND name = '0004_llm_usage_ts_index.sql';
DROP INDEX IF EXISTS idx_llm_usage_ts;
COMMIT;
```

Save it as `rollback-0004-index.sql` and run it with `ON_ERROR_STOP` (the same text is
`db/testdata/upgrade/rollback-0004-index.sql` in the source tree, with a comment header):

```bash
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 < rollback-0004-index.sql
```

The recipe drops the index and deletes the ledger row of `0004` **only when the ledger is exactly the baseline plus
`0004` and the row of `0004` has the name of the shipped file**. Otherwise it raises the error above and changes
nothing: a later migration has run (it refuses while `5002` or `5003` is recorded: drop them first), or the ledger was edited. **A second run raises that error too, and changes
nothing**, because the row is already gone. That is expected, not a fault. The next start of this release builds the
index again.

**Removing the ledger table** (only to restore the exact catalog of a database that never had it). Order: the drop of
`5002` first (when it is recorded), then the `0004` recipe, then the cleanup below (and before all of them, the drop of `5003` when it is recorded). Stop every ARTEX instance and run the cleanup SQL on the database that was rolled
back. It drops the ledger **only when it holds exactly the baseline versions** (1, 2, 3, 5001) and otherwise raises an
error and changes nothing: it **refuses while `0004`, `5002` or `5003` is recorded**, and so when a later migration has run or the ledger
was edited; then the right way back is the pre-upgrade dump. The next start of the new binary adopts the baseline
again.

**Order of the recipes.** On a database that this release started (ledger `0001 0002 0003 0004 5001 5002 5003`) run them in
this order, each only after the one before it succeeded, and stop at the first one that raises:

| Step | Recipe | Ledger it accepts | What it refuses |
|---|---|---|---|
| 1 | drop `5003` (`rollback-drop-5003.sql`, see "Migration 5003") | any ledger whose newest fork version (5000 and above) is `5003` and whose `5003` row has the shipped name; on a started database `1 2 3 4 5001 5002 5003`, afterwards `1 2 3 4 5001 5002` | a ledger without `5003`, with a fork version above it, or with `5003` recorded under another name |
| 2 | drop `5002` (`rollback-drop-5002.sql`, see "Migration 5002") | any ledger whose newest fork version (5000 and above) is `5002` and whose `5002` row has the shipped name; afterwards `1 2 3 4 5001` | a ledger without `5002`, with a fork version above it (so `5003` recorded), or with `5002` recorded under another name |
| 3 | `0004` (`rollback-0004-index.sql`) | exactly `1 2 3 4 5001`, `0004` under the shipped name; afterwards `1 2 3 5001` | `5002` or `5003` recorded, `0004` not recorded, or anything else |
| 4 | ledger cleanup (`rollback-cleanup-ledger.sql`) | exactly `1 2 3 5001`; afterwards no ledger table | `0004`, `5002` or `5003` recorded, or anything else |

Steps 3 and 4 are only needed to go back without the override; 0.5.14m and older start with none of them. A
database that has `5002` but not `0004` (it never ran a release with the index) skips step 3. A database whose
newest migration is `5002` (a start of 0.5.17m) skips step 1.

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
Releases up to 0.5.14m start without any of these steps.

**Downgrade rule for future releases.** A binary older than the newest applied migration is refused ("unknown
migration applied") unless the override is set. Each fork migration states in its first line whether an older binary
is safe on a database that has it (`-- downgrade-safe: yes|no: <reason>`), and the CHANGELOG repeats it. After a
`no`, restore the pre-upgrade dump.

### Migration 5002 (Tool RAG observation)

`db/migrations/5002_tool_rag_observations.sql` creates the table `tool_rag_observations` and one index
(`CREATE ... IF NOT EXISTS`, first line `-- downgrade-safe: yes: additive table, older binaries do not read it`). The
table references `tasks(id) ON DELETE CASCADE`, so the migration takes a short lock on `tasks`. Design and runbook:
[adr/0005-tool-rag-observation.md](adr/0005-tool-rag-observation.md), [tool-rag-observation.md](tool-rag-observation.md).

- **Ledger.** After a start the ledger holds six rows and ends with `5002`: `0001`, `0002`, `0003`, `0004`, `5001`,
  `5002`. The reference catalog has one more table (59 tables).
- **Older binary.** A binary that does not ship `5002` refuses the database ("unknown migration applied") unless it is
  started with `ARTEX_SCHEMA_ALLOW_NEWER=5002`. The table is then ignored.
- **Drop recipe.** To remove the table after going back, stop every ARTEX instance and run
  `db/testdata/upgrade/rollback-drop-5002.sql` with `ON_ERROR_STOP`. It acts only when `5002` is the newest fork
  migration in the ledger and its row has the shipped name, and raises, changing nothing, on any other ledger,
  including `5002` recorded under another name (then restore the pre-upgrade dump):

  ```sql
  BEGIN;
  SELECT pg_advisory_xact_lock(7337741001);
  DO $$
  BEGIN
    IF (SELECT max(version) FROM schema_migrations WHERE version >= 5000) IS DISTINCT FROM 5002 THEN
      RAISE EXCEPTION '5002 is not the newest fork migration: restore the pre-upgrade dump instead';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 5002 AND name = '5002_tool_rag_observations.sql') THEN
      RAISE EXCEPTION '5002 is recorded under another name: restore the pre-upgrade dump instead';
    END IF;
  END $$;
  DELETE FROM schema_migrations WHERE version = 5002 AND name = '5002_tool_rag_observations.sql';
  DROP TABLE IF EXISTS tool_rag_observations;
  COMMIT;
  ```

- **Order.** Run this drop before the `0004` recipe and the ledger cleanup above, and **after** the drop of `5003`
  when `5003` is recorded (see "Order of the recipes"): it refuses a ledger with a fork version above `5002`, and the
  `0004` recipe and the cleanup both refuse while `5002` is recorded. This recipe accepts a ledger with `0004`
  recorded (`1 2 3 4 5001 5002`) and one without it.
- **Archives.** Task archiving deletes the task's rows of the table; restoring an archive does not bring them back.

### Migration 5003 (detection storage)

`db/migrations/5003_finding_detections.sql` (113 lines; first line `-- downgrade-safe: yes: additive tables, one function
and one trigger on conversations; older binaries do not read them`) is the storage of the detection work. It creates four tables, `finding_detections`, `detection_rules`, `detection_steps`
and `detection_query_results`, their indexes and CHECK constraints, the function `stop_deleted_conversation_detection()`
and the trigger `trg_conversation_detection_delete` on `conversations`. **Nothing writes to these tables in this
release except the recovery at start (below): there is no API, no UI and no worker yet, and no setting.** The first
start of this release applies the migration; no step is needed from the operator.

- **Ledger.** After a start the ledger holds seven rows and ends with `5003`: `0001`, `0002`, `0003`, `0004`, `5001`,
  `5002`, `5003`. The reference catalog has four more tables (63 tables).
- **Downgrade-safe: yes.** Older binaries never read the tables, and the trigger only acts when a conversation that a
  detection run points to is deleted (it then stops that run if it is pending or running); with no run it does nothing. A binary that does not
  ship `5003` refuses the database ("unknown migration applied") unless it is started with
  `ARTEX_SCHEMA_ALLOW_NEWER=5003`. This is the way back to 0.5.17m (and to 0.5.16m and 0.5.15m, which
  need the override anyway for `5002` or `0004`): set it in `.env`, restart, remove it when the right binary runs. Read
  the risk in "The override" above. Detection rows written meanwhile stay, and the older binary has **no archive
  refusal** (next bullet).
- **Archives (behaviour change of this release).** Task archives do not carry detection runs yet. Queuing an archive
  job for a task whose findings have at least one detection run is refused, and so is the purge of such a task if a run
  appeared after the job was queued (the job then ends as a failed archive and nothing is deleted). The API answers
  `409 Conflict` with the error text
  `this task has detection results, which task archives do not carry yet; delete them from the findings or upgrade to a release that archives them`.
  Deleted runs (tombstones, `deleted_at` set) do not count; deleting the task is not refused and removes its runs with
  it. Because nothing creates detection runs in this release, an operator meets the refusal only on a database that
  already has runs, for example one that ran a later release and came back with the override. To proceed: delete the
  detection runs of the task's findings (a later release has the route), or wait for the release that carries
  detections in archives. An older binary does not know the refusal and archives such a task, which **deletes its
  detections** (they are not in the archive).
- **Start log.** Every start prints `[detect] recovered stopped=<n> queued=<n>` after the migrations: how many
  detection runs the recovery stopped (a run that was running when the server stopped) and how many it left queued. On
  this release both are `0` unless the database has runs. If the recovery fails the start fails with
  `recover finding detections: <error>`.
- **Drop recipe (destructive).** To remove the tables after going back, stop every ARTEX instance, take a dump, and run
  `db/testdata/upgrade/rollback-drop-5003.sql` with `ON_ERROR_STOP`. **It destroys every detection run, rule, step and
  query result.** It acts only when `5003` is the newest fork migration and its row has the shipped name, and raises,
  changing nothing, on any other ledger (then restore the pre-upgrade dump). It is optional: the older binary ignores the
  tables. The text is the same as the file, which has a comment header:

  ```sql
  BEGIN;
  SELECT pg_advisory_xact_lock(7337741001);
  DO $$
  BEGIN
    IF (SELECT max(version) FROM schema_migrations WHERE version >= 5000) IS DISTINCT FROM 5003 THEN
      RAISE EXCEPTION '5003 is not the newest fork migration: restore the pre-upgrade dump instead';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 5003 AND name = '5003_finding_detections.sql') THEN
      RAISE EXCEPTION '5003 is recorded under another name: restore the pre-upgrade dump instead';
    END IF;
  END $$;
  DELETE FROM schema_migrations WHERE version = 5003 AND name = '5003_finding_detections.sql';
  DROP TRIGGER IF EXISTS trg_conversation_detection_delete ON conversations;
  DROP FUNCTION IF EXISTS stop_deleted_conversation_detection();
  DROP TABLE IF EXISTS detection_query_results;
  DROP TABLE IF EXISTS detection_steps;
  DROP TABLE IF EXISTS detection_rules;
  DROP TABLE IF EXISTS finding_detections;
  COMMIT;
  ```

- **Order.** Run this drop **first** of all the recipes (see "Order of the recipes"). The drops of `5002`, the `0004`
  recipe and the cleanup all refuse while `5003` is recorded.

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
| `r0513` | `db/schema.sql` as released in 0.5.13m: the schema that is now frozen, and the same bytes as in 0.5.14m (no ledger) |
| `s2a` | release 0.5.15m, the first release with the ledger: the frozen `schema.sql`, the four baseline migrations `0001`, `0002`, `0003`, `5001` and their ledger rows (the manifest records it as shape `s2a`, tag `v0.5.15m`, with a `migrations` list). Its data fixture is the `r0513` one |
| `s3a` | release 0.5.17m (`v0.5.17m`), the last release before `5003`: the same frozen `schema.sql` as `s2a` (fixture `schema-951f3d0c8859.sql`), the ledger `0001 0002 0003 0004 5001 5002` and the table `tool_rag_observations`. As the manifest names it: shape `s3a`, tag `v0.5.17m`, commit `024abf634efb`, with a `migrations` list of those six rows. Its data fixture is the `r0513` one; startup adds only the four detection tables and the ledger row `5003`. `db/testdata/upgrade/flags-v0.5.17m.txt` records the startup seeds it ran |
| `legacy` | **synthetic**: `r0509` plus `legacy-mutations.sql`, which puts back the old forms that the upgrade branches of `schema.sql` look for. It is not a recorded release |

Checks (test names are in `db/`, `db/dbtest/` and `server/`):

- **Fixtures are what they claim to be.** The sha256 of each recorded schema is a constant in
  test source and in `db/testdata/upgrade/manifest.json`; a test fails if a hashed file holds a
  CR byte, and `TestGitHistoryFixtures` compares each fixture with
  `git show <commit>:db/schema.sql` by commit id.
- **Fresh database** after startup (`Open` alone: it adopts the baseline migrations and creates the ledger table
  `schema_migrations`) has the catalog listed in `db/testdata/upgrade/reference-catalog-pg16.txt` (it now contains the index
  `idx_llm_usage_ts`, the table `tool_rag_observations` and the four detection tables, the function and the trigger of `5003`) and the ledger versions 0001, 0002, 0003, 0004, 5001, 5002 and 5003.
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
- **Runner** (`TestMigration*`, `TestLedger*`, `TestLoadMigrations*`, on temporary databases; the database tests of the tool RAG
  table, `TestToolRag*`, are in the gate's pattern too): the refusal order and
  each refusal, the override, the baseline adoption and its rollback on failure, lock timeout, deadlock retry,
  concurrent starts, the load-time lint, the transaction marker, and error and log text free of row content. Also in
  `db/` but outside the gate's name pattern: `TestEnsureWrappersRunEmbeddedMigrations` (the `EnsureLLM*Table` wrappers
  produce the shape of a full `Open`) and `TestBaselineSetIsFrozen`.
- **Rollback** (`TestUpgradeRollbackCleanupDropsABaselineLedger`, `TestUpgradeRollbackCleanupRefusesAnyOtherLedger`,
  `TestUpgradeRollbackToTheReleaseWithoutTheLedgerAndForward`): the guarded cleanup SQL, and the way back to a
  database without the ledger and forward again (the first and the third start from a database this release
  started and run the drop of `5002`, the `0004` recipe and the cleanup in that order, because each later recipe
  correctly refuses a ledger that still holds the earlier migration; the second one starts from the release before
  `0004`).
- **Migration `0004`** (`db/upgrade_migration0004_test.go`, `db/upgrade_rollback0004_test.go`): the file is
  byte-identical to upstream and not in the baseline set; from `s2a` it is applied after the fork migration `5001`
  (the order rule accepts it, as it works inside each range) and logs `[migrate] applied 0004_llm_usage_ts_index.sql in
  <ms>ms`; a binary without `0004` refuses the database unless `ARTEX_SCHEMA_ALLOW_NEWER=0004` (one warning per start,
  ledger row and index kept); a simulated 0.5.13m start works on a database that has it; the guarded `0004` recipe
  drops the index and the row, refuses six other ledgers (no `0004`, `5002` recorded, a later fork migration, no `0003`, no `5001`, empty) and
  a `0004` row under another name, and a second run raises and changes nothing.
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
- **The time of the index build was measured on one database only.** On a restored copy of a real database
  with 73 104 rows in `llm_usage` the build took 35 ms (2026-11-20; log line
  `[migrate] applied 0004_llm_usage_ts_index.sql in 35ms`). No test builds `idx_llm_usage_ts` on a large table; the
  lock behaviour above is PostgreSQL's, documented, not measured here, and the path with a failed concurrent
  pre-build (an INVALID index) was not exercised.
- **No drift report.** The runner detects a database it does not understand by its ledger (an unknown, changed,
  out-of-order, empty or foreign ledger). It does **not** detect an object that kept its name but has another
  definition (`IF NOT EXISTS` checks names only), a hand-made object or a missing trigger. A drift report against the
  reference catalog was deferred to the backlog (ADR 0004). Only the recorded-shape suite and the dry run on a copy
  show such differences.
- **Upstream `0005` is not ported.** The first non-baseline migration, `0004` (the index `idx_llm_usage_ts`), is in this
  release, and so is the first fork migration after the baseline, `5002` (the table `tool_rag_observations`). The
  `EnsureLLM*Table` wrappers re-run the baseline files only, so they do not create that index; only a full `Open` does.
- **The load-time lint is textual, not a SQL parser.** It does not catch transaction control inside a procedure that is
  called (`CALL p()` where `p` commits; PostgreSQL itself refuses that inside a transaction block), a statement
  built with `EXECUTE`, a file written for `standard_conforming_strings = off`, or `psql` meta-commands. The
  runtime marker (see "Changing the schema") is the second line and detects a file that ended the transaction after it
  ran; the file's effects up to that point then stay committed.
- Local gate evidence is on linux/amd64 with PostgreSQL 16; the last gate run (detection storage, pull request 1a, with
  migrations `0004`, `5002` and `5003`) printed `upgrade-gate: tests passed=166 skipped=0 failed=0 (minimum passed: 166)`
  and `upgrade-gate: OK`, and `go test -p 1 ./db` was green (raw output in the pull request). In the container runs the migration tag check was `NOT RUN` (no `.git`) unless
  `UPGRADE_GATE_REQUIRE_GIT=1` and a history were present; it was run separately on the host. The recorded shapes are
  `r0509`, `r0512`, `r0513`, `s2a`, `s3a` and the synthetic `legacy`.
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
  `TestLedger*`, `TestLoadMigrations*`, and `TestToolRag*`) with `go test -json`, and fails unless `go test` exits 0, at
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

The gate currently expects **166** passing top-level tests and zero skips (150 before migration `5003`; 110 of them in
`./db`, 51 in `./db/dbtest`, 1 in `./agent`, 4 in `./server`). It consists of 100 upgrade tests (`TestUpgrade*`,
`TestConfigFileDSNIsNotUsed*`), the 53 migration runner tests of `./db` (`TestMigration*`, `TestLedger*`,
`TestLoadMigrations*`; 14 of them are the tests of `5003`, `TestMigrationDetection*`, and 2 check that the drop recipes
of `5002` and `5003` refuse a ledger row recorded under another name) and the 13 database tests of the tool RAG table
and its report (`TestToolRag*`: 5 of the table, 8 of the report). The breakdown is the comment above `MIN_UPGRADE_TESTS` in `scripts/upgrade-gate.sh`.

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
   files use `5000`-`9999`. An **upstream file** is a port, not a new file: it keeps upstream's number and **bytes**
   (copy it, check the sha256, git blob id, size and that it has no CR; never add a header), so it carries no
   `downgrade-safe` line and the flag lives only in its pin (step 2). Choose `yes` only when an older binary can run on
   a database that has it, since only the runner's refusal, not a schema incompatibility, stops that binary (`0004`:
   `yes`; the override or the rollback recipe lifts the refusal). A file that sorts before a fork file shifts the
   positional indices that `TestMigrationPinsDetectChanges` uses. First line of a fork file: `-- downgrade-safe: yes|no: <reason>`. Idempotent where
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
   already a recorded shape. For a release that ran the runner, the manifest entry has a `migrations` list (the ledger it
   left; `buildShape` loads the schema, the baseline files, the ledger table and its rows, then the data fixture), and
   the release needs a `flags-<tag>.txt` (the `const flag` keys at that tag; the tests read it). Then adapt the tests
   that assumed the previous newest shape or a database without a ledger:
   `TestUpgradeFrozenSchemaIsTheNewestRecordedShape`, `TestUpgradeLedgerAdoptsEveryShape` and the skip list of
   `TestUpgradeLegacyBranchesWereEntered`. A shape with a ledger has no `schema_migrations` exemption.
5. Extend the data fixtures and `expected-changes.json` for each shape, the added `schema_migrations` rows included (a
   migration that writes a ledger row adds one `added` entry per shape that did not have it; the cause is
   `db/migrate.go:681; db/migrate.go:715-733`, the ledger insert and `applyPending`).
   Run `go test ./db -run '^TestUpgradeRowsPreserved'`: it lists every difference that has no entry. A new column
   with a constant default needs one `value` entry (`"column_added": true`) per row of that table in the fixture of
   each older shape; a data fixture needs a change only when a table has no rows yet.
6. Regenerate the reference catalog in a PostgreSQL 16 container:
   `ARTEX_UPGRADE_REGENERATE=1 go test ./db -run TestRegenerateUpgradeReference`
   (with `ARTEX_PG_DSN` set as above; the test logs the new hash). The file is written into the working
   tree the test runs in: from a container, copy it back out (the container does not do it; encoding the file with
   `base64` and writing it in the work tree worked). For `0004` the diff was one line, `index idx_llm_usage_ts`. Review it as a diff: it is the new definition of "a
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
   - Tests that hard-code the ledger content break when a second ledger-writing migration exists: the helpers
     `requireBaselineLedger` (baseline only), `requireLedgerOf` and `requireEmbeddedLedger` (the embedded set), the log
     line `up to date (upstream=<v>, fork=<v>)`, tests that insert a version by hand, the synthetic migration of
     `TestMigrationOrderPerRange`, the rollback tests (they start from the previous release) and the comparison of the
     `EnsureLLM*Table` wrappers with a full `Open` (it excludes the index of `0004`).
   - Raise `MIN_UPGRADE_TESTS` in `scripts/upgrade-gate.sh` when tests were added (and the counts in its comments), and
     update the counts and shape descriptions in this file and in the CHANGELOG entry.
8. The CHANGELOG lists the migration with its downgrade-safe line.

Then run the gate.

**What the first fork migration after the baseline (`5002`) showed that the steps above do not say:**

- A table that task archiving must clear goes into the delete list in `db/task_archives_restore.go` (the one that holds
  `skill_usage` and `tool_usage`) when its rows hang on a task, because archiving keeps the `tasks` row and so a
  foreign-key cascade does not fire. Add a test that archives and restores.
- A new table raises the table count of the reference (`expectedTables` in `db/schema_fresh_test.go`: 58 to 59), and
  each recorded shape needs an exemption for a table that does not exist before startup.
- Ship a guarded drop recipe with the migration (`db/testdata/upgrade/rollback-drop-5002.sql`), test it, run it before
  the recipe for `0004` and the ledger cleanup, and list it in `.github/CODEOWNERS` next to the cleanup SQL.
- Tests that assumed a ledger of only the baseline rows must use the embedded set (`requireEmbeddedLedger`); the
  ledger cleanup refuses once a later migration is applied, so rollback tests run the drop first.
- Synthetic migrations in tests must stay clear of real fork numbers (they were renamed `5902` and `5903` because the
  real `5002` collided).
- The tests that a new migration adds are only counted by the gate when their names match its pattern (the `db`
  tests of `5002` are `TestToolRag*`, which the pattern lists); raise `MIN_UPGRADE_TESTS` to the new count.

**What the next fork migration (`5003`, four tables, one function, one trigger) added to those steps:**

- Record the release before it as a new shape (`s3a`, release 0.5.17m: fixture, manifest entry with its `migrations`
  list, `flags-v0.5.17m.txt`, a case in the matrix with exemptions for the four tables that do not exist before
  startup) and add the new ledger row as an `added` entry for every shape that lacks it (`5003` in all of them; `5002`
  is not added to `s3a`, which has it).
- Four tables raise `expectedTables` from 59 to 63 and the reference catalog; the function and the trigger are catalog
  objects too (`reference-catalog-pg16.txt`).
- The drop recipe has **two** guards: the newest fork version, and the row of the migration under its shipped name
  (`5002` got the second guard in the same pull request). The drops of `5002` and `5003`, the `0004` recipe and the
  cleanup run in that order, and each later recipe refuses while an earlier migration is recorded. Tests that assumed
  that `5002` is the newest migration (rollback tests) run the drop of `5003` first.
- The new recipe goes into `.github/CODEOWNERS` next to its siblings.
- A migration whose rows hang on `findings` needs no entry in the archive delete list, but archiving must not lose
  them silently: see "Migration 5003", "Archives". A test helper that builds an old archive fixture must create the
  tables the purge now reads (`finding_detections`).
- Test databases: the cleanup of a temporary database that a role owns waits for a server-user session to end before
  the drop (`db/dbtest/server.go`).
- Raise `MIN_UPGRADE_TESTS` (now 166) and the counts in its comment.

Rollback of the first release with the ledger needs no step (see "Rollback" above). The `0004` recipe is
`db/testdata/upgrade/rollback-0004-index.sql`, run by `TestUpgradeRollback0004RecipeDropsTheIndexAndTheLedgerRow` and
`TestUpgradeRollback0004RecipeRefusesAnyOtherLedger`; the drop of `5002` runs before it (`TestMigrationToolRagDropRecipeIsGuardedAndReversible`); the drop of `5003` runs before that
(`db/detection_migration_test.go`, `db/upgrade_rollback_test.go`). The cleanup SQL is
`db/testdata/upgrade/rollback-cleanup-ledger.sql`, run by `TestUpgradeRollbackCleanupDropsABaselineLedger` and
`TestUpgradeRollbackCleanupRefusesAnyOtherLedger`, and rehearsed end to end by
`TestUpgradeRollbackToTheReleaseWithoutTheLedgerAndForward`.

The suite has been exercised by two real schema changes: `findings.ticket_id` (merged from `main` before 0.5.13m: no new
fixture, one regenerated reference line, three pinned hashes, six expected-changes entries, one archive restore default,
and moving the one test that opened its own database onto `dbtest.TempDatabase`) and the adoption of the migration
ledger itself (a new shape `r0513`, the table `schema_migrations` in the reference, four `added` entries per shape, 58
tables in the reference), and by the first real migration, `0004`: an upstream file (a shape `s2a` with a ledger, one
reference line, one more `added` entry per shape, seven more gate tests).

**Integrity controls for migration files.** A released file is checked by: the pins test (`TestMigrationPins`, no
database); `.gitattributes` (`db/migrations/*.sql -text`: no line-ending conversion, because the checksum is over
bytes); the gate's tag check (a file that a `v*` tag holds must be unchanged, undeleted and unrenamed at `HEAD`, and the
upstream files must equal the pinned upstream commit); and `.github/CODEOWNERS`, which names the owner for
`db/migrations/`, `db/migrate.go`, `db/db.go`, `db/schema.sql`, `db/migrate_pins_test.go`, `scripts/upgrade-gate.sh`,
`.github/workflows/`, `.gitattributes`, `.github/CODEOWNERS`, `db/upgrade_gatescript_test.go` and the rollback
recipes (`rollback-cleanup-ledger.sql`, `rollback-0004-index.sql`, `rollback-drop-5002.sql`, `rollback-drop-5003.sql`). One
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

Rows added to a table that does not exist before startup (the rows of `schema_migrations`) are `added` entries
with a `pk` each (`{"version": "1"}`, `{"version": "2"}`, `{"version": "3"}`, `{"version": "5001"}`), one more for
`{"version": "4"}` in every shape except `s2a` (which already holds the four baseline rows and gets only `4`), and one
more, `{"version": "5002"}`, in every shape except `s3a` (which has it); `{"version": "5003"}` is added in every shape.

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
ledger schema_migrations: absent -> present, versions 0001 0002 0003 0004 5001
```

and on the second start `ledger schema_migrations: present, versions 0001 0002 0003 0004 5001 -> present, versions 0001 0002 0003 0004 5001`.
The test fails unless the ledger holds exactly the versions this release ships after the first start of a database
without a ledger (the baseline, adopted, plus every later migration, applied), holds at least the baseline
otherwise, and is unchanged by the second start.
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
- **Upstream `0005`** is not ported; it is planned for when the fork next ports an upstream migration numbered above it or
  the traffic-rewrite feature, whichever comes first.
- Platforms other than linux/amd64 and PostgreSQL versions other than 16, and Windows checkouts (a CRLF conversion
  would change a checksum; `.gitattributes` forbids it).

Design: [adr/0004-versioned-migrations-stage-2.md](adr/0004-versioned-migrations-stage-2.md).
