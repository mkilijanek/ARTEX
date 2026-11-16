# Norma SDK integration audit and implementation

Milestone [#11](https://github.com/mkilijanek/ARTEX/milestone/11), audited
2026-11-15. Implementation branch: `norma-sdk/upgrade-integration`, targeting
the fork's `main`. Every change has its own descriptive commit.

## Baselines and scope

- Fork main: `fc35dddee8286d584f71ba751a02c48bc60835a3`; declared Norma
  requirement v0.4.3, replaced by BaaRai/Norma v0.4.4.
- Upstream ARTEX: `251ab680cce3bf84e0f98c1bbb2b63332a82d59a`; replacement v0.4.8.
- Existing local phase candidate `c932522` was unmerged. It was inspected,
  but its broad changes were not used as this branch's base.
- This branch starts with main and the translation-only commits from
  [PR #39](https://github.com/mkilijanek/ARTEX/pull/39). That PR remains separate
  and should merge first. The effective dependency is an audited v0.4.8 local
  snapshot with the patches listed in `third_party/norma/README.artex.md`.
  `SOURCE.json` retains original module/file hashes; root `go.mod` now declares
  v0.4.8 and replaces it with this directory.

Only SDK source and directly integrated session, provider, tool, compaction,
usage, configuration and validation code are included. General graph selection,
Planner/Worker algorithms, RAG, target-gate policy/schema, egress governance,
coverage/budget systems and other token-optimization phases are excluded.
Existing broad issues #14, #17–23 and PR #35 were inspected for overlap; their
independent features were not imported. Existing issues #4, #5, #15, #16 and
#38 were reused instead of duplicated.

## SDK version decisions

| Transition | Verified upstream change | Decision and verification |
| --- | --- | --- |
| v0.4.4 → v0.4.5 | Bound CaptureOnce's captured-marker shortcut; new spill directories 0700/files 0600 | Adopt; forged-marker, UTF-8, file-mode and full-output recovery contracts |
| v0.4.5 → v0.4.6 | Nonce-tagged untrusted Content framing, reserved output budget, optional host controls | Adopt; default on in ARTEX; `ARTEX_UNTRUSTED_BOUNDARY=0/false/off/no` disables tool frames; summary policy remains active |
| v0.4.6 → v0.4.7 | Explicit empty signed thinking, signature-only accumulation, redacted_thinking replay, thinking-disabled filtering; WebFetch redirect hook | Adopt; Anthropic wire/replay tests and ARTEX transcript/NOA roundtrips |
| v0.4.7 → v0.4.8 | WebFetch dial and disable-direct-retry hooks; cloned default transport for dial-only configuration | Adopt API and mocked proxy/direct/redirect contracts; no independent target-gate policy port |

Sources: pinned module downloads for each version in the above range and the
[Norma repository](https://github.com/BaaRai/Norma). The four localized SDK files
are identical upstream between v0.4.4 and v0.4.8, allowing PR #39's English
literal changes to be retained without translating evidence.

Selective ARTEX upstream ports:

- [37f2573](https://github.com/BaaRai/ARTEX/commit/37f2573a535b066f7dd4605d0d59d0c77d3f385a)
  and [54ff511](https://github.com/BaaRai/ARTEX/commit/54ff51177d96bfc15809fea12be33d939d204a0e):
  synchronized tool/prompt/wrap-up callbacks; call copied functions outside locks.
  Tests for unrelated upstream features were excluded when adapting the ports.
- [13dde2d](https://github.com/BaaRai/ARTEX/commit/13dde2da15e8e1cf136fe72642dac620a103cda5)
  and [4acdf3b](https://github.com/BaaRai/ARTEX/commit/4acdf3bb268d1d9c1bf5fe2d831568812f90a4ff):
  custom User-Agent, persistence/UI and connection-test validation. Exact endpoint
  mode is an additional fork implementation; upstream did not implement that
  half of the existing request.

## Findings, dependencies and review order

All twelve issues belong to milestone #11. P1 covers correctness/reliability;
P2 covers localization and capability/configuration compatibility.

| Order | Issue | Priority | Implementation and acceptance evidence |
| --- | --- | --- | --- |
| 0 | [#38](https://github.com/mkilijanek/ARTEX/issues/38) | P2 | Separate PR #39; four SDK literal patches; English notices and original Chinese tool output tests |
| 1 | [#40](https://github.com/mkilijanek/ARTEX/issues/40), [#4](https://github.com/mkilijanek/ARTEX/issues/4) | P1 | Audited v0.4.8 identity and source guard; native Anthropic empty/signed/redacted thinking, tool replay, disabled-thinking summaries and transcript restoration |
| 2 | [#42](https://github.com/mkilijanek/ARTEX/issues/42) | P1 | Locked callback setters/getters in assembly, tool catalog, prompt and wrap-up; concurrent swap and reentrant replacement tests |
| 3 | [#41](https://github.com/mkilijanek/ARTEX/issues/41) | P1 | Stable worker/planner/main/chat IDs without transcript storage; provider-context identity; checked Resume before calls; corrupt/unreadable originals remain unchanged |
| 4a | [#16](https://github.com/mkilijanek/ARTEX/issues/16) | P1 | MergeEcho for deltas; fresh input excludes cache reads and writes; persisted ledger and mixed-price fixtures |
| 4b | [#44](https://github.com/mkilijanek/ARTEX/issues/44) | P1 | Logical retry/failover usage and cancellation; no replay after opaque thinking/tool output; SDK retains atomic-error usage; ContextTokens separates physical context from billing |
| 4c | [#15](https://github.com/mkilijanek/ARTEX/issues/15) | P1 | Purpose/logical ID/round through QueryDeps and provider fallback; side answer/summary tags; one persisted row per provider attempt; interrupted consumers covered |
| 5 | [#43](https://github.com/mkilijanek/ARTEX/issues/43) | P2 | Forward optional AcceptsRawInput through nested decorators; real SDK strict/repair/default/permission tests |
| 6a | [#46](https://github.com/mkilijanek/ARTEX/issues/46) | P1 | Reproduced NOA failures in imported tests; learned windows, configured host window, final-view budget, explicit irreducible-request failure and opaque replay; archive/fallback tests |
| 6b | [#45](https://github.com/mkilijanek/ARTEX/issues/45) | P1 | Tool frames; code-owned summary rules; JSON summaries and caller args; trusted archive pointers; forced compaction, resume, skill Extra and WebFetch contract tests |
| 7 | [#5](https://github.com/mkilijanek/ARTEX/issues/5) | P2 | Optional UA and exact URL across four protocols, streaming/atomic paths, connection tests, saved profiles and UI; credentials bound to endpoint/mode; keyless Ollama retained |

The raw-input decorator defect was latent: the built-in NOA Compress tool is
currently appended after ARTEX tool augmentation. The audit does not claim
that current Compress calls were broken by that wrapper.

Existing session Close defers and NOA/built-in compactor exclusivity were already
correct. Resume treats a missing file as a normal new session; corruption and
read errors now stop the run. NOA additionally prepares its archive location
before replacing built-in compaction, because upstream Enable creates it lazily.

## Context, trust and accounting contracts

Raw transcript history and archive originals remain recoverable. Request-only
NOA shortening retains call/source references and tool pairing. Signed thinking
text/signatures and retained opaque redacted payloads are not rewritten.
Irreducible NOA requests fail before transport when the estimated message,
system/tool and output-reserve budget cannot fit; estimates are not an exact
provider tokenizer. Reactive recovery learns a smaller provider-reported limit.

Summary framing preserves the code-owned NOA header and reconstructs navigation
from compression state, including materialization with NOA off. Operator task
and system policy retain authority. SDK compaction preserves initial session
input and evidence references inside JSON data; summary-specific instructions
are applied through the actual summary route, not prompt-text classification.

`Result.Extra` is privileged host guidance. Its current production producer is
the operator-installed Skill registry. Skill args supplied by the model are now
JSON-quoted data. MCP responses and raw target/tool results stay in Content.
Nonce/JSON frames and deterministic tests establish transport/data contracts;
they do not prove that a language model resists every prompt injection.

The ledger counts each invocation of the decorated provider's Stream/Complete,
including ARTEX-level retries/failover and failed/interrupted calls with reported
usage. SDK terminal usage aggregates logical request attempts; physical context
metadata prevents that aggregate from triggering unnecessary compaction.
Summary tokens are billed in the ledger even though the SDK's own session
terminal total does not include its separate summarizer calls.

Coverage exclusions are explicit:

- SDK-internal connection and OpenAI empty-response HTTP retries occur inside a
  single provider invocation. They are not separate ledger rows; any usage not
  surfaced by the SDK cannot be reconstructed as billed tokens from this API.
- SDK WebSearch's dedicated DeepSeek client, other search HTTP backends, MCP
  servers and subprocess/external clients do not pass through the configured
  model-provider recorder. Their separate model/API spend is excluded.
- Independent graph digest, review/judge and other direct ARTEX model calls keep
  their existing provider metering; new direct calls have explicit unclassified
  or judge attribution. Their algorithms were not changed by this milestone.
- Historical rows have empty attribution columns and must not be presented as
  known summary-purpose data. Provider-invoice reconciliation is outside these
  local protocol tests.

Purpose reporting without changing existing aggregate API semantics:

```sql
SELECT COALESCE(NULLIF(purpose,''),'historical/unknown') AS purpose,
       COUNT(*) AS provider_calls, COUNT(DISTINCT NULLIF(call_id,'')) AS logical_calls,
       SUM(input_tokens) AS input, SUM(output_tokens) AS output
FROM llm_usage
GROUP BY 1;
```

Fixed endpoint mode preserves escaped path, trailing slash and query exactly,
disables redirects and requires an explicit key when a saved destination/mode
changes. Model listing remains a base-URL operation; fixed-mode users enter a
model name manually. Existing profiles default to legacy mode.

## Validation

Validation uses deterministic local HTTP/SSE/tool fixtures and disposable
PostgreSQL. No paid model or external target is used. Root and nested SDK test
suites run separately; root `./...` does not include the SDK module. CI now runs
both and focused protocol/tool/server-adapter race checks.

Final local results on the milestone implementation:

- ARTEX: `go build ./...`, `go vet ./...`, `go test -json -p 1 ./... -count=1`
  pass against fresh disposable PostgreSQL: **1001 passed test events**, five
  existing skips. Twelve additional role/segment/resumed-unlock test events
  pass with PostgreSQL, including under race detection. No new ARTEX test was disabled.
- Nested SDK: full `go test ./... -count=1 -timeout=240s` passes. The imported
  contract inventory contains 439 passed test events and three existing skips.
- Race detection: ARTEX callback/session/tool/retry/endpoint adapters, plus SDK
  thinking/redacted/framing/raw-input/WebFetch/transcript contracts pass.
- Frontend: Node 22, `tsc --noEmit` and `npm run build:static` pass (31 pages).
- Source hashes/localization and `git diff --check` pass.

Issues remain open for review/merge. Implementation and acceptance-test coverage
of this milestone is complete; opening the final PR does not close the milestone.
The separate translation PR #39 is retained as the review/merge prerequisite.


## Recovering a session that cannot be restored

`session_restore_error` is a local persistence failure, not a provider error. ARTEX
stops before sending a model request, preserves the transcript, reports its path,
and does not run the model-error retry loop. Do not repeatedly redispatch it.

1. Stop/pause the affected session and ensure no process is writing its transcript.
2. Make a protected copy of the exact path in the error, plus the associated NOA
   archive directory; preserve permissions. Never edit the only copy.
3. Restore a known-good complete transcript backup. For a torn final JSONL record,
   inspect a copy: removing only an incomplete final record is safe only after
   verifying the remaining tool-use/results are paired. Otherwise use a complete
   backup. Do not discard signed thinking or rewrite tool IDs.
4. Retry the session once after validation; confirm it resumes the intended
   history. Keep the original and backup until that succeeds. If no valid history
   exists, create a new session explicitly rather than silently overwriting it.

Identity mapping is deliberate: transcript/provider use the session ID; chat NOA
archives use `chat-<sessionID>` for backward-compatible namespacing. Main/worker
archives use their existing role IDs. This is a deterministic mapping, not literal
identity equality across every storage namespace.

## Additional review follow-up (2026-11-16)

Provider and model-list clients refuse redirects in both base and fixed endpoint
modes. Stored key binding includes proxy configuration; profile lookup failures
abort edits. Side-question identity includes endpoint mode and proxy.

Repeated conversation summaries retain the original input without nesting prior
JSON envelopes. Side-question summaries are quoted as untrusted JSON. Activity
presentation strips only complete, nonce-matched SDK wrappers; model history
retains the frames. Marker neutralization performed by the SDK remains visible
in the activity payload; the underlying original artifacts remain authoritative.

NOA retains redacted-only assistant messages. The final request budget is a
conservative local estimate, not a provider tokenizer or proof that a request
would overflow. It can reject a request a provider would accept. Such failures
are identified as local context-budget errors and reported as blocking_limit,
not retried as network/model failures. Increase the configured window only after
checking the model's actual limit, or start a fresh session.

The later review on #15/#16 identified integration work between the
phase A–C stack and this SDK branch; that integration is now complete. Phase A telemetry is now integrated on this branch: numeric dimensions, pool
attempt ordinals and cumulative logical usage are preserved together. Phase C
compatibility has been checked after merging the current main. This PR does not bundle independent
Planner/Worker projection logic. Per-call fresh-input subtotals and unknown cache tariffs are now represented
explicitly; old archives without the new subtotal retain unknown cost. Do not
interpret green CI as acceptance of those unresolved cross-branch concerns.

Review follow-up 2026-11-16: trigger context JSON-quotes all tool event fields;
NOA budget errors are mapped on the actual harness terminal path and tested in
both stream and atomic capture. Search-provider clients refuse redirects with
keys or request bodies, and model-listing key reuse includes proxy identity.
Configure the final HTTPS endpoint explicitly: HTTP-to-HTTPS, base-path and 308
redirects are refused rather than followed with credentials. Custom-tool and MCP
HTTP client policy is outside this SDK-only change.

The snapshot verifier checks the full upstream production file inventory and
original test hashes separately from reviewed local test hashes. Two actual SDK
compactions and actual tool-result activity capture cover summary/data framing.
Phase B/C are now merged into main and included by integration ancestry.
Their context/cache flags remain disabled by default pending rollout measurements.
