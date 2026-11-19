# ADR 0003: Validate schema upgrades with tests and fixtures before changing startup

- **Status:** Accepted (Phase D.2 stage 1, branch `phase-d/schema-validation`)
- **Date:** 2026-11-19
- **Issues:** #24 (referenced, not closed)
- **Plan:** `docs/phase-d2-plan.md` (not tracked), sections 0, 5 and 13
- **Runbook:** [schema-upgrade.md](../schema-upgrade.md)
- **Follow-up:** [0004-versioned-migrations-stage-2.md](0004-versioned-migrations-stage-2.md) (Proposed)

## Context

`db.Open` applies the embedded `db/schema.sql` on every start, as one string, under an advisory
lock, and then runs `seedBuiltins`. There is no version table, no checksum and no record of what
was applied. Upstream added a versioned runner (`1da0d07`, `f3b9f6f`), and the port matrix marks
it ADAPT: do not replace `schema.sql` wholesale, keep the lock and archive compatibility, and
preserve rows on fresh, existing and repeated starts.

Releases 0.5.9m to 0.5.12m differ from `main` by one additive table and one additive JSON
property, so the hard question was not "how to order migrations" but "does the existing
every-start mechanism keep data, and will a later change stay safe". Nothing in the repository
could answer that. A runner would also add three startup refusals (unknown version, changed
checksum, out-of-order version) and a numbering rule for a fork, before there was any evidence
about existing databases.

## Decision

1. **Stage 1 is tests, fixtures and gates only. No runtime change:** `Open`, `db/schema.sql`
   and every production code path stay as they are.
2. Record the schema of each release as a fixture, by commit id and sha256, plus a synthetic
   `legacy` shape that reaches the upgrade branches of `schema.sql`.
3. Prove, on temporary PostgreSQL 16 databases: a fresh database matches a committed reference
   catalog; each shape keeps its rows except for differences listed exactly in a committed
   file; each shape converges on the fresh catalog; a repeated start changes nothing; the schema
   apply is atomic; startups coordinate on the advisory lock; task archives restore across the
   upgrade.
4. Pin `db/schema.sql`, the Go DDL constants and the reference catalog by hash. A change needs the
   recorded procedure ([schema-upgrade.md](../schema-upgrade.md), "Changing the schema").
5. A gate script fails when an upgrade test skips or fewer than the expected number ran, and runs
   in CI.
6. Tests read the database only from `ARTEX_PG_DSN`, through a guard that refuses any database
   the tests did not create.
7. Unexpected differences are classified `intended`, `accepted` or `defect`. A `defect` fails the
   gate until the owner decides. Three differences were accepted on 2026-11-19: startup restores the code's
   name and description of a built-in agent; startup re-creates a missing `task_asset_links` row listed
   in `assets.task_ids`; a restored task archive gets `updated_at` set to the restore time on `tasks` and
   `explorations`.
8. The runner is Stage 2, with its own plan and approval.

## Alternatives rejected

- **Port the runner now.** Three startup refusals and a numbering decision before any evidence
  about existing databases; a runtime change in an iteration meant to validate first.
- **Harness only, never a runner.** Upstream migrations would be hand-merged into `schema.sql` for good,
  and a non-idempotent change would have no safe place. Not rejected as an end state, only as the end of the road.
- **Committed `pg_dump` fixtures from old binaries.** `settings` holds `auth.password_hash`; a committed dump
  needs scrubbing on every regeneration. The real-binary gap is addressed by the dry run on the owner's own
  restored copy, outside the repository.
- **A schema ledger or an observe-only drift report in Stage 1.** It is a change to `Open`.
- **Allowlists with whole-table or whole-column entries.** They absorb defects: a citation shows a cause,
  not that the change is wanted.
- **Fixtures only from fresh databases of old tags.** They never enter the legacy branches of `schema.sql`.

## Consequences

- No runtime detection of a downgrade, a foreign database or a mis-defined object until Stage 2. Operators
  keep the pre-upgrade dump.
- Every schema change now needs a fixture, a regenerated reference and an expected-changes review.
  Upstream `0004` and `0005` wait for Stage 2.
- Evidence is limited to linux/amd64 and PostgreSQL 16, to shapes from 0.5.9m on (`legacy` is synthetic), and to the
  current code on old table shapes. The dry run on real data was approved by the owner and is not done yet.
- Stage 1 does not deliver "migration order/checksums" from the issue; the pull request does not close it.
- Test code now fails when `ARTEX_PG_DSN` is unset (upgrade tests) and never reads the config file.
- To judge in review: defects the harness exposed, entries the owner had to decide, and the effort of the next schema change.
