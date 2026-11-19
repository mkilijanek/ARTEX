# ADR 0004: Versioned migrations and numbering for the fork (Stage 2)

- **Status:** Proposed. **Not built.** Nothing in this ADR exists in the code: there is no `db/migrate.go`,
  no `db/migrations/` and no `schema_migrations` table. It needs its own plan and the owner's approval.
- **Date:** 2026-11-19
- **Depends on:** [0003-schema-upgrade-validation.md](0003-schema-upgrade-validation.md)
- **Plan:** `docs/phase-d2-plan.md` (not tracked), section 5.6

## Context

Upstream's runner (`1da0d07`, with the ordering rule of `f3b9f6f`) applies numbered `.sql` files after
`schema.sql`, one transaction per migration with its ledger row, and refuses an unknown applied version, a
changed name, a changed checksum, and a pending migration older than the newest applied one. The fork differs:

- `llm_usage` has 14 more columns and 2 more indexes than upstream's `0002`.
- `schema.sql` re-applies CHECK pairs, triggers and functions on every start, so a migration that changes
  such an object would be undone on the next start.
- The file-name rule `^([0-9]{4})_[a-z0-9_]+\.sql$` accepts `5001_...`.
- `.gitattributes` has no rule for migration files, and checksums are over bytes.

## Decision (proposed)

1. Port `db/migrate.go` and its tests, **not verbatim**: the order check applies inside each numbering range
   (below). Keep the advisory-lock coordination. Order in `Open`: `schema.sql`, migrations, drift report,
   `seedBuiltins`.
2. **One `schema_migrations` table**, as upstream's, with two ranges:
   - `0001`-`4999`: upstream migrations, byte-identical to upstream (blob ids asserted by a test).
     `0003` is a no-op on fork databases; `db/schema.sql` keeps its index line.
   - `5000`-`9999`: fork migrations.
3. The first fork migration is `5001_llm_usage_fork_columns.sql`, holding the extra `llm_usage` columns and indexes.
4. **Every fork statement is idempotent** (`IF NOT EXISTS` or equivalent): existing databases adopt the ledger by
   re-running them.
5. **A `schema.sql` change goes with the migration, in the same commit.** If a migration touches a CHECK, trigger or
   function that `schema.sql` re-applies, the matching `schema.sql` lines change in that commit. A test asserts that
   a start after a migration leaves the catalog unchanged.
6. `.gitattributes` gets `db/migrations/*.sql -text` (no line-ending conversion, as for the schema fixtures).
7. Also in Stage 2, moved out of Stage 1: a drift report against the reference catalog; a warning or refusal for a
   database last opened by a newer or foreign binary; `EnsureLLM*Table` failures become startup failures. Whether a
   separate schema ledger is still needed next to `schema_migrations` (it would record the `schema.sql`
   fingerprint) is decided in the Stage 2 plan.

## Alternatives rejected

- Renumbering fork migrations into upstream's sequence: loses byte identity with upstream.
- One ledger table per range, or a track column: diverges from upstream's table.
- Replacing `schema.sql` by a baseline migration: forbidden by the port matrix, and irreversible for existing databases.
- A third-party migration tool: a new dependency and a ledger unrelated to upstream's.

## Consequences

- A database last opened by an upstream binary that has migrations the fork lacks is refused.
- Startup gains fail-closed conditions (unknown version, changed name or checksum, out-of-order version).
- The Stage 1 harness becomes the regression suite: it must pass with only the new table and rows as differences, and
  every Stage 1 shape must adopt the ledger without a row change.
- Entry gate: Stage 1 green on `main`, the dry run run or waived, every `defect` entry resolved.
