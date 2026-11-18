# Selective upstream port matrix

Reviewed against fetched `481dd1c3f299c73eea2f33ed1ab186482effad2f`,
195 commits ahead of baseline `fc35dddee8286d584f71ba751a02c48bc60835a3`.
PORT means prioritized for a tested port, not already applied. ADAPT requires
integration with this fork; DEFER leaves runtime unchanged; REJECT excludes the
proposal from this program. This table records the historical selection, not the current implementation status.

| Decision | Upstream evidence | Upstream intent | Files / functions | Dependencies and risk | Token effect / acceptance |
| --- | --- | --- | --- | --- | --- |
| PORT | `8c3bae4`, `c73e287` untrusted boundary | Preserve trust and target boundaries | `agent/untrusted_boundary.go`, assembly/provider hooks | Baseline Norma replace v0.4.4 versus upstream v0.4.8; inspect SDK hook support and opt-out before enabling. Keep operator instructions trusted. | May add tokens; injection resistance takes priority. Boundary-on/off and tool-result tests. |
| PORT | `975f634` derived data envelopes | Preserve trust and target boundaries | `agent/data_envelope.go`, planner/worker/tools | Nonce generation, shared rendering, provenance and prompt ordering must land together. Never truncate away framing. | Measure envelope overhead; test malicious field content and delimiter collisions. |
| PORT | `f6b29b8`, `1c3a5f4` target checks | Preserve trust and target boundaries | `intercept/target_*.go`, `agent/webfetch.go`, `server/customtool.go`, `guard/guard.go` | Target extraction, resolution, scope schema, request approvals, pinned dialer and redirect checks form one dependency chain. `mvdan.cc/sh/v3` added upstream. | No savings expected; test redirect, private address, credentials, disabled gate and every HTTP path. |
| ADAPT | `1da0d07`, `f3b9f6f` versioned migrations | Integrate compatibility or data-model improvements | `db/migrate.go`, `db/migrations`, `db/db.go`, EnsureLLM*Table | Retain advisory-lock coordination and archive restore compatibility; reject checksum mismatch or old pending migration. Do not replace schema.sql wholesale. | None; fresh/existing DB and repeated startup must preserve rows. |
| ADAPT | PR #230 / `30c787d` | Integrate compatibility or data-model improvements | `db/task_scope.go` coverage lookups, `db/migrations/0003_task_scope_icp_index.sql` | Depends on migration runner or explicit idempotent DDL adaptation. Preserve scope ownership and run large-graph fixtures on temporary PostgreSQL. | DB latency/query count, not LLM token savings; addresses #10. |
| DEFER | `f3a20ef` lexical Tool RAG | Add independent runtime capabilities | `agent/toolrag.go`, `agent/assembly.go`, catalog tests | Requires bound catalog, locked tools, permission hooks and fallback behavior. Introduce observation-only measurement before filtering. | Unknown until tool-schema bytes and missing-tool behavior measured. |
| ADAPT | `16ddfc7`, `4cde1a8` budgets | Integrate compatibility or data-model improvements | `agent/budget.go`, `server/engine_budget.go`, `llmrec.WithUsageSink` | Callback must observe failures/retries exactly once. Intent/task limits and settle/revive state reset semantics require explicit tests. | Unknown; retain forced evidence wrap-up and document restart reset. |
| ADAPT | `6f3a59e`, `101c92d` coverage ledger | Integrate compatibility or data-model improvements | `db/coverage_ledger.go`, `agent/coverage_ledger.go`, planner/tools | Schema, provenance, framing and per-run end reason. Untested is distinct from negative evidence. | Adds context; measure before selecting projections. |
| ADAPT | `bc33db2` provenance | Integrate compatibility or data-model improvements | record_fact tool, `db/exploration.go`, `server/server.go` schema update | Existing tool schemas stored in DB must be updated, not just Go descriptions. | Preserve source identity across summaries and archive/restore. |
| ADAPT | `46e81f5` (v0.4.7), `f6b29b8` (v0.4.8) | Integrate compatibility or data-model improvements | `go.mod`, `go.sum`, provider/capture hooks | Test thinking/signature tool roundtrip (#4), Ollama, streaming usage and cancellation; SDK behavior is part of cost semantics. | Rebaseline usage when adapters change; no assumed savings. |
| DEFER | custom User-Agent branch `4acdf3b` / #5 | Add independent runtime capabilities | `agent/provider.go`, config/profile/API/UI | Saved keys must remain scoped to their saved endpoint; full URL mode requires separate transport work. | Compatibility feature, not prerequisite for token optimization. |
| DEFER | secrets-at-rest / verified evidence | Add independent runtime capabilities | `db/secrets.go`, evidence snapshots, API masking | Important follow-up requiring key lifecycle, backup/restore and existing-data tests; unrelated schema not bundled into token changes. | No assumed token savings. |
| DEFER | #8 extra benchmark corpora; #9 Telegram | Add independent runtime capabilities | benchmark scripts; new inbound bot | New infrastructure and external message/approval surface. Replay harness and local tests first. | No comparable baseline yet. |
| REJECT | #6 provider promotion | Broaden the program without attributable evidence | no code change demonstrated | Existing configurable endpoint already supports compatible providers. Do not couple promotion to a technical dependency. | None demonstrated. |
| REJECT | wholesale main merge / full-file copying | Broaden the program without attributable evidence | 304 changed files, ~32k insertions | Pulls unrelated offensive features, credentials, schema and defaults into a measurement program. Port dependency sets with independent evidence instead. | Cannot attribute results to changes. |

Validate exact commit identities with `git show` before implementation; abbreviated
commit IDs identify inspected local history. Security dependency review precedes
context reduction. See [phase0-baseline.md](phase0-baseline.md) for milestone order.

## Selection and current delivery status (2026-11-15)

Current comparison: `d147dce3442531d44eaf06774441dbf47edc024a` against `fc35ddd` (223 commits).
The original 15 rows group dependency sets; they never represented one row per
commit. Selection includes SDK/provider/session, context/compaction/tool schemas,
usage/cost, database compatibility, and security dependencies of those paths.
Independent UI, notification, feature and operational changes are deferred.
[upstream-commit-inventory.tsv](upstream-commit-inventory.tsv) enumerates **every**
commit, its upstream intent (commit subject), touched files and triage category,
including merges. Triage is not a claim that every deferred feature was tested.

| Change | Delivery in this fork | Remaining acceptance |
| --- | --- | --- |
| Cache-write pricing (`6072809`) | Independently fixed by Phase A #35 and SDK #47 | Preserve pricing regression fixtures; no second port needed |
| Stream echo accounting and logical metadata | #35, with separate SDK integration in #47 | #14 reporting and pool-attempt review fixes |
| Context projection and trace slices | #36 | #17–20 review corrections; opt-in until validated |
| Summary memo and cache prefix | #37 | #21–22 incomplete summaries, prefix and measurement review |
| Norma v0.4.8, callbacks, replay, trust boundaries | #47 | #40–43 build/provenance/session/decorator review fixes |
| Custom User-Agent (`13dde2d`, `4acdf3b`) and full URL mode | #47 | Covered by SDK endpoint and credential tests; no longer deferred |
| Runtime/system message translation | #39, prerequisite of #47 | Docker build correction |
| Target gate, migrations runner, budgets, Tool RAG and independent features | Not delivered by #34–37/#47 | Separate dependency review; not part of SDK-only milestone. Target gate: partially adapted in Phase D.1 PR 1 (see below) |

## Phase D.1 PR 1 delivery (2026-11-17)

Branch `phase-d/untrusted-boundary-audit`; plan [phase-d1-plan.md](phase-d1-plan.md);
decision [adr/0001-webfetch-destination-policy.md](adr/0001-webfetch-destination-policy.md).

| Change | Delivery in this fork | Remaining acceptance |
| --- | --- | --- |
| `f6b29b8`, `1c3a5f4` target checks: WebFetch | **Partial adaptation, not a port.** The intent is kept, but none of the upstream files are copied. Host-side hooks in `agent/webfetch_policy.go` install `CheckRedirect` and a pinned `DialContext` on the SDK's WebFetch in every role, with an initial-URL check. The fork has its own URL-aware matcher (`db/egress_match.go`) over the existing asset intercept rules, plus a built-in control-plane denylist. There is no `intercept/target_*.go`, no scope-schema change and no `mvdan.cc/sh/v3`. No SDK patch. | Proxied paths re-resolve names (residual). The PR 1 test evidence is added at the TEST gate |
| `f6b29b8`, `1c3a5f4` target checks: custom http tool | **Partial adaptation.** `server/customtool.go` uses the same guard, plus origin pinning, a task-allow-rule requirement for templated hosts with operator headers, sticky header stripping after a cross-origin hop, and refusal of cross-origin 307/308 body replays. | As above |
| `f6b29b8`, `1c3a5f4` target checks: shell and guard target gate | **Not delivered.** Bash, shell, `command`/`script` custom tools, MCP tools and WebSearch queries are not destination-checked. Plan decision Q1: a go-mitmproxy addon spike first (D.2); the upstream shell target gate only if that route fails. | D.2 |

Corrected citations. These corrections apply to the historical selection table above,
which is not rewritten:
- Rows `8c3bae4`/`c73e287`, `975f634` and `f6b29b8`/`1c3a5f4` are marked PORT. Only
  the untrusted boundary was delivered (#47). `975f634` is planned for Phase D.1 PR 2,
  and `f6b29b8`/`1c3a5f4` are partially adapted as described above.
- In [upstream-commit-inventory.tsv](upstream-commit-inventory.tsv), `975f634`,
  `1c3a5f4`, `f6b29b8`, `c73e287`, `8c3bae4`, `8acf1be`, `09ef702` and `62c9b4a` are
  triaged `dependency-review`, and `bc33db2` is `defer-independent`.
- The row "ADAPT `46e81f5` (v0.4.7), `f6b29b8` (v0.4.8)" refers to the Norma SDK
  dependency, not to the target checks.
- `8acf1be` and `09ef702` (code-owned untrusted-content tails and the planner goal
  anchor) appear only in the inventory. They
  are planned for Phase D.1 PR 2.
