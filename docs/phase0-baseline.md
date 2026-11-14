# Phase 0 baseline and implementation order

Captured 2026-11-14 before source changes for GitHub issues #11–13.

## Source identity

- Branch: `token-opt/phase0-baseline`.
- HEAD and origin/main: `fc35dddee8286d584f71ba751a02c48bc60835a3` (v0.5.0).
- Fetched upstream/main: `481dd1c3f299c73eea2f33ed1ab186482effad2f`.
- `git rev-list --left-right --count HEAD...upstream/main`: `0 195`.
- Tracked working tree: clean. Local `docs/token-audit.md` and
  `docs/upstream-port-matrix.md` were ignored drafts; they are inputs to this
  review, not baseline measurements. Explicit ignore exceptions now retain the
  program's documents without exposing other local notes.
- origin: `git@github.com:mkilijanek/ARTEX.git`;
  upstream: `git@github.com:BaaRai/ARTEX.git`.

These are fetched ref identities, not a claim that the remote remains unchanged.
No fetch, reset, upstream merge, production database access, or live model call
was needed to establish this baseline.

## Manifests and configuration

| File | SHA-256 before changes |
| --- | --- |
| go.mod | `377d18c2d5c1f36879e38e15efdce24bab5bdf732a63d11b44e21f5276991bc1` |
| go.sum | `d5bc9615444dadc97936ad95465c89f58a865ca5dad20a1e1c51bbe110894917` |
| web/package.json | `bda9836eee82e8e9ae4d5a86dcf61f5b5d97c5f1905555e5ca9f27469a176b13` |
| web/package-lock.json | `517c4b6b5dc0fb4dff99cd8bf88cf9217fd359bb550a88a496a31db7254cdeff` |
| config.example.json | `e71aec93b7ff47e8952aed0dcc5e27599afea69f07c7ecb3dbcca4bb5e76c68f` |
| .env.example | `a7a54da421e3fef2b5ee5e6ea32e457fde6d1f4c7779bd3d7d520183b1e4145d` |

Go requires 1.26.10. Norma's declared v0.4.3 is replaced by
`github.com/BaaRai/Norma v0.4.4`; upstream uses v0.4.8 and additionally
`mvdan.cc/sh/v3 v3.14.1`. Do not bump the SDK as an incidental optimization.
The frontend is Next 16 / React 19; CI uses Node 22. Initially the host had
Node 18.19.1 and no Go on PATH. Go 1.26.10 was downloaded to `/tmp/go` for
verification. Caches were initially in `/tmp`; after root disk exhaustion,
validation used `/opt/data/artex-review-validation-20261114` for toolchain,
module/build caches and temporary build files.

Only example configuration was inventoried. No operator credentials or private
configuration values belong in this document. Runtime configuration is mutable:
database DSN comes from environment/config; LLM profiles and prompts can come
from the database. Example environment leaves LLM provider/model/endpoint unset.

## GitHub review and order

All 33 issues and 10 milestones were open when reviewed. The executable program
is milestones 4–10 (Phase 0, A, B, C, D, E, F). Milestones 1–3 predate it and
overlap it; do not implement the same work twice.

1. **Phase 0 (#11–13):** immutable source/config inventory, dependency matrix,
   test inventory and an honest measurement protocol.
2. **Phase A (#14–16):** extend the existing always-on recorder, retain provider
   usage independently of local byte estimates, attribute hidden calls/retries,
   and test cache semantics before changing pricing.
3. **Phase D security prerequisite (#23–24):** review SDK untrusted boundaries,
   provenance, redirect/DNS pinning and existing-database compatibility before
   changing the context representation used at those boundaries.
4. **Phase B (#17–20):** trace slices and response bounds first; then scoped
   worker context; planner deltas last because each round currently starts a
   fresh session. A delta without a durable/resynchronized snapshot loses state.
5. **Phase C (#21–22):** measure compaction and stabilize cache prefixes while
   preserving instruction priority, evidence and tool-result pairing.
6. **Remaining D (#25–26):** tool retrieval in observation mode, budgets with
   explicit restart behavior. Neither should silently remove tool permissions.
7. **Phase E (#27–29):** build replay fixtures alongside each change, then compare
   A/B/C on identical inputs. Use the full scenario suite before rollout.
8. **Phase F (#30–33):** update measured results, acceptance/rollback and the
   production comparison procedure. No percentage savings until measured.

Older #1/#2 map to B.2, #3 to C.2, #7 to D.1, #10 to a separately tested
performance port. #4 requires SDK thinking-roundtrip fixtures; #5 needs endpoint
and credential-destination tests. #6 is provider promotion with no demonstrated
code requirement: reject as an implementation dependency. #8 (additional live
benchmarks) and #9 (Telegram approvals) are deferred beyond this program.

This order moves security review ahead of context reduction and starts fixtures
early. It does not mark downstream milestones complete or alter GitHub state.

## Verification inventory

| Area | Existing evidence | Remaining gap |
| --- | --- | --- |
| Overview | `agent/tools_overview_test.go`, `agent/blackboard_inheritance_test.go` | Whole rendered byte budget, large graph and resync/delta fixtures |
| Trace detail | `TestGetWorkerTraceStepIDsDegradeGracefully` | Five-step cap is not a byte cap; slice/pagination coverage needed |
| Compaction | `agent/coldgraph.go`, `agent/coldgraph_test.go`, `sidequestion/context_test.go` | Record actual compactor calls and repeated unchanged content |
| Usage | `llmrec/llmrec_test.go`, `agent/capture_usage_test.go`, `server/llmrec_raw_test.go` | Recording-disabled, failed/early-stop, request bytes and dimensions |
| Pricing | `server/cost_ledger_test.go`, `db/llm_usage.go` | Provider/cache fixtures and historical pricing policy |
| Retry/failover | `server/task_llm_test.go`, `llmpool/pool_test.go` | Attempt attribution separate from logical model rounds |
| Side questions | `sidequestion/*test.go`, `server/side_questions_test.go` | Distinguish answer vs context-summary cost |
| Security | `agent/prompt_injection_seed_test.go`, guard/intercept suites | Upstream boundary/DNS/redirect regression tests before ports |

CI builds and vets Go, runs `go test -p 1 ./...`, a selected race suite,
shellcheck, TypeScript and static frontend build. PostgreSQL-backed checks need
an isolated DSN. Several baseline tests explicitly skip (including side usage
cancellation, capture approval and company-scope overview); a skip is not a pass.
See [token-benchmark.md](token-benchmark.md) for commands, results and exclusions.
