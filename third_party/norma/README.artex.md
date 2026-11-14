# Norma message localization

This directory is the production Go-source snapshot of the previously pinned
`github.com/BaaRai/Norma v0.4.4`, retaining its Apache-2.0 license. There is no SDK
feature upgrade. The local module replacement is necessary because the SDK has
no localization hook for these compiled message literals.

The translation patch changes only string literals in:

- `tool/shell_tools.go`: interactive-shell descriptions, schema help and errors;
- `tool/shell_session.go`: session and monitor notices;
- `tool/task.go`: background-task completion/failure/limit notices;
- `noa/archive.go`: archive headings and navigation labels.

It does not translate command output, captured evidence, user text, identifiers,
JSON field names, enum values or stored records. Existing ARTEX defaults are
already English; no database prompt/schema migration is required. Historical
messages remain historical data.

`SOURCE.json` records the upstream module checksum and hashes of every original
file. The first commit is an unchanged source snapshot; the following commit is
the reviewable translation delta. Regression tests verify that other snapshot
files remain unchanged and production string literals contain no Chinese text.

When updating Norma, import the new pinned source, reapply this four-file string
patch, update SOURCE.json to that original source identity and rerun both SDK
integration and ARTEX tests. Do not mix a feature upgrade into this translation PR.

Validation for issue #38: full ARTEX suite against disposable PostgreSQL passes
929 test events with five pre-existing skips; focused localization/evidence tests
pass under race detection. `go build ./...` and `go vet ./...` pass. The 57 Chinese
literals plus the remaining Chinese full stop are translated; no live model or
external target is needed to exercise these compiled messages.
