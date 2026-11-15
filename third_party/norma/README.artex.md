# Norma source and integration patches

ARTEX requires `github.com/Autumn-27/norma v0.4.8` and replaces it with this
local snapshot of `github.com/BaaRai/Norma v0.4.8` (Apache-2.0).
`SOURCE.json` retains the upstream module checksum and original SHA-256 of each
production source file. The local replacement provides compiled message
localization and the reviewed integration fixes below.

## Localization — issue #38, separate PR #39

Only system string literals change in `tool/shell_tools.go`,
`tool/shell_session.go`, `tool/task.go` and `noa/archive.go`. Raw command output,
evidence, user text, field names and historical records remain unchanged.
The translation-only branch remains based on v0.4.4; those four original files
are identical between v0.4.4 and v0.4.8. Existing ARTEX defaults are English;
no stored prompt translation migration is needed.

## Integration — milestone #11

Local production patches beyond localization:

- `noaadapter/compactor.go`: learn the provider-reported overflow window.
- `noaadapter/project.go`: retain signed/redacted thinking; frame synthetic
  summaries as JSON data while retaining the code-owned summary header and
  archive navigation reconstructed from trusted state.
- `noaadapter/session.go` and new `noaadapter/budget.go`: measure the final view,
  including nudge/ref overhead, and shorten outgoing tool-result copies when
  compression is ignored. Stored originals and tool pairs remain intact.
- `noaadapter/materialize.go`: retain archive navigation when NOA is disabled.
- `skill/skill.go`: JSON-quote model-supplied args inside privileged skill Extra;
  operator-installed procedure text remains host guidance.
- `llm/types.go`: optional ContextTokens metadata distinguishes the latest
  physical context footprint from additive retry billing counters.
- `harness/query.go`: use that footprint for compaction and retain reported
  atomic-call usage even when the model call fails.

The ARTEX source-hash test permits exactly these reviewed production patches
and the four localized files; all other original source must match the manifest.

The imported upstream contract tests run as a separate Go module in CI.
Test-only adaptations initialize the HTTP client in a direct doStream fixture,
use English archive labels, and replace assertions that expected old NOA bugs
with affirmative bounded-view/tier-consolidation assertions.

For an upgrade, import the new pinned original source and checksum manifest,
reapply the listed patches, review changed contracts, and run both nested SDK
and ARTEX integration/race checks. See `docs/norma-sdk-audit.md` in ARTEX for
version decisions, scope, accounting exclusions and validation evidence.
