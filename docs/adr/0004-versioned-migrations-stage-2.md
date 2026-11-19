# ADR 0004: Versioned migrations and numbering for the fork (Stage 2)

- **Status:** Accepted for the part built (pull request S2-A: the runner, the ledger and the baseline). Not built, and
  not accepted here: the drift report (backlog), the first non-baseline migration (the next pull request, upstream
  `0004`) and upstream `0005`.
- **Date:** 2026-11-19
- **Depends on:** [0003-schema-upgrade-validation.md](0003-schema-upgrade-validation.md)
- **Plan:** `docs/phase-d2-stage2-plan.md` (not tracked), sections 3, 4, 9 and 12
- **Runbook:** [schema-upgrade.md](../schema-upgrade.md), "Upgrading to a release with migrations"

## Context

Upstream's runner (`1da0d07`, with the ordering rule of `f3b9f6f`) applies numbered `.sql` files after
`schema.sql`, one transaction per migration with its ledger row, and refuses an unknown applied version, a
changed name, a changed checksum, and a pending migration older than the newest applied one. The fork differs:

- `llm_usage` has 14 more columns and 2 more indexes than upstream's `0002`.
- `schema.sql` re-applies CHECK pairs, triggers and functions on every start, so a migration that changes
  such an object would be undone on the next start.
- The file-name rule `^([0-9]{4})_[a-z0-9_]+\.sql$` accepts `5001_...`.
- `.gitattributes` has no rule for migration files, and checksums are over bytes.

## Decision

1. Port `db/migrate.go` and its tests, **not verbatim**: the order check applies inside each numbering range
   (below). Keep the advisory-lock coordination.
2. **One `schema_migrations` table**, as upstream's, with two ranges:
   - `0001`-`4999`: upstream migrations, byte-identical to upstream (blob ids asserted by a test). `0003` is a
     no-op on fork databases; `db/schema.sql` keeps its index line.
   - `5000`-`9999`: fork migrations, numbered on `main` only, never reserved on a branch.
3. The first fork migration is `5001_llm_usage_fork_columns.sql`, holding the extra `llm_usage` columns and indexes.
4. **The baseline set is idempotent; later migrations run once.** Existing databases adopt the ledger by running the
   baseline files (`0001`, `0002`, `0003`, `5001`), which are `IF NOT EXISTS` and no-ops on a database that a release
   without the ledger started.
5. **A `schema.sql` change goes with the migration, in the same commit**, and only for an object that `schema.sql`
   re-applies. That lockstep edit is the only allowed change of the frozen `schema.sql` (see "As built").
6. `.gitattributes` gets `db/migrations/*.sql -text` (no line-ending conversion).
7. `EnsureLLM*Table` failures become startup failures.

## As built (S2-A)

**Preflight.** `Open` holds the schema advisory lock (key `7337741001`) and does, in this order: read the ledger and
refuse a database it does not understand (`checkLedger`, read only); `schema.sql`; on a database without a ledger,
adopt the baseline in one transaction; apply each later pending migration in its own transaction with its ledger row;
`seedBuiltins`. A refusal happens before anything is written, so an older or foreign binary's `schema.sql` has not
run over the database. The checks run in a fixed order and the first match wins:

| # | Ledger state | Action |
|---|---|---|
| 0 | table absent | adopt the baseline |
| 0a | table exists, no rows | refuse: the ledger was edited (no override) |
| 1 | rows exist, none in `5000`-`9999` | refuse: created by another binary (no override) |
| 2 | a known version with another name or checksum | refuse (no override) |
| 3 | a version this binary does not ship | refuse, unless every such version is at most `ARTEX_SCHEMA_ALLOW_NEWER` |
| 4 | a pending version below the newest applied one of its range | refuse (no override) |

Check 1 runs before check 3, so an upstream ledger is refused even with the override set.

**Frozen baseline.** The baseline set is `baselineVersions = {1, 2, 3, 5001}`, pinned by `TestBaselineSetIsFrozen`: it
never grows. A database without a ledger gets the table, the four files and the four rows in one transaction; a
failure leaves neither. An empty existing ledger is refused, not adopted, because adoption never leaves one: an empty
table means a manual `DELETE` or `TRUNCATE`, and adopting it would re-run later migrations. Drift is not checked at
adoption.

**`schema.sql` is frozen** at its 0.5.13m bytes (sha256 `951f3d0c...f963a`). Every later change is a migration. A
migration may change an object that `schema.sql` re-creates on every start (CHECK pairs, triggers, the four functions,
the data statements) only if the same commit edits that `schema.sql` statement.

**Order rule per range.** A pending migration whose version is below the newest applied version **of its range** is
refused. Unknown versions count as applied for this rule, so the override cannot be used to slide a pending migration
under an unknown one. Pending migrations apply in ascending version, so a later upstream migration runs after fork
`5xxx` on an existing database and before it on a fresh one; the convergence test catches an order dependency. The
upstream range is a contiguous prefix of upstream's sequence.

**Override.** `ARTEX_SCHEMA_ALLOW_NEWER=<NNNN>` is the highest unknown version accepted. It covers check 3 only, and
it expires at the next newer version. A malformed value (anything but four digits, nonzero) stops the start before the
database is touched. One warning per unknown version is logged on every start. The risk is stated in the runbook:
this binary's `schema.sql` re-creates triggers, functions and CHECK constraints and re-runs its data statements over
data that a newer release wrote. A read-only start was rejected (the application writes at start).

**Lock timeout and retry.** Each adoption and migration transaction runs `SET LOCAL lock_timeout = '60s'`; a `55P03`
error names the migration and the table and the likely holders. `schema.sql` and the wait for the advisory lock keep
their unbounded wait. A deadlock (`40P01`) is retried up to 4 times with the delays of the `schema.sql` apply.

**Transaction control.** A migration runs inside the runner's transaction, so a `COMMIT` in the file would drop
`lock_timeout` and write the ledger row in autocommit. `loadMigrations` therefore rejects, besides `CONCURRENTLY` and
`VACUUM`, any statement-level `COMMIT`, `ROLLBACK`, `SAVEPOINT`, `RELEASE`, `ABORT`, `BEGIN`, `END`, `START TRANSACTION`
or `PREPARE TRANSACTION`, after removing comments and replacing string literals, quoted identifiers and dollar-quoted
bodies. The check is textual: procedures that commit, `EXECUTE`, `standard_conforming_strings = off` and `psql`
meta-commands are not caught. The second line is a runtime marker: `SET LOCAL artex.migration_tx = 'on'` before each
file and a check after it; a file that ended the transaction fails without a ledger row (its effects before the
`COMMIT` stay). A parser was not used: no new dependency.

**Ledger-derived text.** Names and checksums read from the database are printed quoted (`%q`), and errors of a
migration step carry the SQLSTATE and object names only (message text only for classes 23, 40, 42, 55, 57), never row
content or the DSN.

**Ensure wrappers.** The DDL of the `llm_*` tables lives in `0001`, `0002` and `5001`; the Go DDL constants are gone.
`EnsureLLMRecordsTable` and `EnsureLLMUsageTable` remain as wrappers that re-run the embedded files in one
transaction under the schema lock, write no ledger row and take the same 60 s table-lock timeout. A failure of
these migrations in `Open` stops the start.

**Immutability controls.** `TestMigrationPins` (name, sha256, downgrade-safe flag, blob id for upstream files),
`.gitattributes -text`, the gate's tag check against `v*` tags and the pinned upstream commit `278de8b`, and
`.github/CODEOWNERS`. Required code-owner review and protection of `v*` tags are the repository owner's settings.

**Rollback.** Binaries up to 0.5.13m ignore the table and start. The guarded cleanup SQL drops the ledger only when it
holds exactly the baseline versions. A fork migration declares in its first line whether an older binary is safe on it.

## Deferred

- **Drift report** against the reference catalog (planned as S2-D): moved to the backlog by the owner. Without it a
  same-named object with another definition is not detected at runtime.
- **Upstream `0004`** (an index on `llm_usage`): the next pull request, through the rewritten procedure.
- **Upstream `0005`**: decided to be ported byte-identical, as a table without fork code, when the fork next ports an
  upstream migration numbered above it or the traffic-rewrite feature, whichever comes first. Rejected: a pinned list of
  skipped upstream versions exempt from the order rule (a new mechanism and a permanent divergence).
- No separate `schema_ledger` for the `schema.sql` fingerprint: `schema.sql` is frozen and pinned instead.

## Decisions of the owner, 2026-11-19

1. Freeze `schema.sql`; every later schema change is a migration.
2. Fail closed on a database the binary does not understand, with a capped override
   (`ARTEX_SCHEMA_ALLOW_NEWER`) for the unknown-version case only.
3. The drift report goes to the backlog.
4. A rollback rehearsal with the published 0.5.13m binary on a restored copy of a real database belongs to the
   release checks (result: see the runbook).

## Alternatives rejected

- Renumbering fork migrations into upstream's sequence: loses byte identity with upstream.
- One ledger table per range, or a track column: diverges from upstream's table. A single global order rule was
  rejected too: fork `5001` would block upstream `0004` and later for ever.
- Replacing `schema.sql` by a baseline migration: forbidden by the port matrix, and irreversible for existing databases.
- Marking the baseline applied after a catalog check, or by object presence: the reference is PostgreSQL 16 specific
  and partial, and presence checks names only.
- A third-party migration tool: a new dependency and a ledger unrelated to upstream's.

## Consequences

- The first start of a release with the runner writes one table with four rows. It is not undone by rolling the
  binary back; old binaries still start.
- A database last opened by an upstream binary that has migrations the fork lacks is refused.
- Startup gains fail-closed conditions (empty or foreign ledger, changed name or checksum, unknown version,
  out-of-order version) and a 60 second wait limit on migration table locks.
- A failed `llm_*` migration stops the start where it was only logged before.
- A migration that fails on some database cannot be corrected under the "never edit" rule; mitigation: the dry run on a
  copy before each release with migrations.
- Partial restores and manual edits of `schema_migrations` are unsupported.
- The Stage 1 harness is the regression suite: it passes with only the `schema_migrations` table and its four rows as
  differences (gate: 128 tests, zero skips).
