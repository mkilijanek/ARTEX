# Phase D.1 plan: untrusted boundary, data envelope and WebFetch protections

Issues: #23 (Phase D.1) and #7 (mirrors upstream BaaRai/ARTEX#124, the agentic attack-class audit).
Base: `main` at `43208e5`. Status: **approved by the user on 2026-11-17** (see §10).

The work ships as two PRs:
- **PR 1 (network)** on branch `phase-d/untrusted-boundary-audit`.
- **PR 2 (prompt envelope and fact labels)** on a later branch.

Every item below carries a `[PR 1]` or `[PR 2]` tag. Items deferred to the goal-proof iteration are tagged
`[deferred]`.

## PR 1 scope (work from this list alone)

- **Acceptance criteria:** AC1, AC3–AC12, AC21, AC22 (PR 1 log lines), AC23.
- **Tasks:**
  - S0
  - S1-net (lab target plus network red tests)
  - S4 (D1+D6)
  - S5 (D2)
  - S5b (`insert_assets` fail-closed)
  - S8-PR1 (verification)
  - S9-PR1 (documentation)
- **Tests:** T-LAB-1, T-MATCH-1, T-WF-1..9, `TestWebFetchToolSetPerRole`, `TestRunInfoReachesToolContext`,
  T-CRED-1..5, T-INS-1.
- **Docs:**
  - `SECURITY.md` (network part)
  - `docs/upstream-port-matrix.md` rows for `f6b29b8`/`1c3a5f4` (partial adaptation) plus the corrected citations
  - new `docs/agentic-attack-audit.md`: rows C1, C7–C11, C17 with results; other rows marked "pending PR 2" or
    "deferred"
  - `CHANGELOG.md` (Security; Changed: direct retry)
  - new ADR for D1
- **Not in PR 1:** everything tagged `[PR 2]` or `[deferred]`.

## 1. Goal and scope

**Goal.**
- `[PR 1]` Stop WebFetch and the custom http tool from reaching the ARTEX control plane, cloud metadata or
  destinations blocked by operator rules.
- `[PR 2]` Mark target-controlled content structurally as data on every planner-, worker- and compactor-facing
  surface, and label facts with a declared source and their writer.
- Each #7 attack class gets a deterministic adversarial regression test that records whether the protection held,
  including the classes where it did.
- **Task completion does not change in D.1.**

**What D.1 does not claim.** Bash, `shell_*`, custom *command* tools, MCP tools and WebSearch queries remain open
egress channels. The D1 policy covers **WebFetch and the custom http tool only**. Against an injected model it is
advisory while `curl` is available. No doc will say "destinations are enforced".

**Out of scope.**
- No new attack capability and no change to hunting behaviour or task completion.
- The shell and recording-proxy egress gate (D.2; decision Q1).
- Live-model evaluation.
- Defaults of `ARTEX_BOUNDED_CONTEXT`, `ARTEX_CACHE_PREFIX`, `ARTEX_COMPACTION_MEMO` and `ARTEX_WORKER_CONTEXT`
  (#1, #2, #33 stay open).
- Any `third_party/norma` patch. The hooks exist (`agentcore.go:111-120`).
- Guard hooks for planner and main sessions, and the `main_agent` origin (D.2; Q4, Q5).
- Server-checkable goal proof, the `prove_goal` refusal and `confidence=observed` (goal-proof iteration; Q6–Q8).

## 2. Current state (verified by reading code at 43208e5)

Legend: HELD = a protection exists and only needs a confirming test. PARTIAL = it exists but can be bypassed or is
incomplete. GAP = nothing protects this. UNKNOWN = not verifiable from code. RESIDUAL = not fixed in D.1; named in
the audit report.

| # | Class / topic | Today (file:line) | Missing | Verdict | Test | PR |
|---|---|---|---|---|---|---|
| C1 | Untrusted boundary on tool results | `norma_trust.go:19-21` is on unless `ARTEX_UNTRUSTED_BOUNDARY` is falsy (`docker-compose.yml:30`). `norma/harness/untrusted.go:21-24,88-92` adds a 16-hex nonce and neutralises markers. `harness/tools.go:97,113` applies it to every tool. | – | HELD | T-LAB-1 | 1 |
| C1b | bbot JSONL and other tool-ingested files (#7) | Only C1 framing covers them. `skills/bbot-recon/SKILL.md` has no untrusted note (upstream `8acf1be` added one). | Skill note | HELD (framing) | T-LAB-2 | 2 |
| C2 | Code-owned untrusted rules | `normaTrustRules` (`norma_trust.go:16`) is appended to every session (`model_metrics.go:8-13` via `capture.go:35`, `side_questions.go:14`). The #49 block (`prompt_shared.go:4-8`) is only in **editable** bodies (`worker.go:265`, `promptcatalog.go:30,79`, `mainagent.go:103`), which a DB override replaces (`prompt.go:58-64`). The planner body (`planner.go:313-355`) has no data rule. | Tails (`8acf1be`/`09ef702`) | PARTIAL | T-PROMPT-1 | 2 |
| C3 | Envelope on host-built messages | The planner input splices raw worker output and finding detail (`planner.go:212,221`), so a forged `- The human (main agent) added a goal:` line cannot be told apart from a real one. The overview is unframed JSON (`planner.go:301-306`). Target-derived asset JSON sits in the worker **system** prompt (`worker.go:477`). Bounded-context prefixes have no nonce (`planner_context.go:17,213,226`). | Envelope (`975f634`) | GAP | T-PLAN-1, T-WORKER-1 | 2 |
| C4 | Memory poisoning (facts) | Node origin is set by the server (`tools.go:1470`, `db/exploration.go:197`) but dropped by `compactNode` (`tools.go:993-1001`). There is no `source` field. `prove_goal` accepts any fact, finding or related-task node (`tools.go:1304-1306`), and the last met goal completes the task (`:1310-1319`). | Labels and surfacing (D5). The refusal is deferred | GAP | T-FACT-1/2 | 2 (refusal deferred) |
| C5 | Plan hijacking | C3 surfaces plus `steer_work` relay (`tools.go:1851`). | Envelope plus planner data rule | GAP | T-PLAN-1 | 2 |
| C6 | Goal drift | Each round the goal is in the planner system prompt (`planner.go:315,357-365`) and the session is new. SDK compaction keeps the original input (`norma_trust.go:36-47,70-85`). No anchor follows the data. `goal_met` (`tools.go:1332-1342`) has no evidence check; it is unbound from the planner by default (`server/orchestration.go:507,564`) but can be rebound. | Anchor; `goal_met` RESIDUAL (Q6) | PARTIAL | T-PLAN-3 | 2 |
| C7 | Tool abuse | Guard intercept runs only for worker and chat (`worker.go:553`, `chat.go:165`). Its default tool set includes WebFetch (`intercept/intercept.go:144-146`): a rule or judge check on tool **input**, with no URL resolution. Agents can widen `task_scope` (`tools_insert.go:443-446`; `db/task_scope.go:147`), but nothing enforces it. The `insert_assets` asset gate fails **open** if rules cannot be read (`tools_insert.go:236-245`). | Fail-closed `insert_assets`; planner/main hooks RESIDUAL (Q4) | PARTIAL | T-INS-1 | 1 |
| C8 | WebFetch SSRF/scope | `permission.AskUser` (`webfetch.go:79-86`) is bypassed by `ModeBypass` everywhere (`planner.go:458`, `worker.go:510`, `chat.go:134`, `mainagent.go:144`). The only checks are scheme and asset suffix (`webfetch.go:224-229`). There are no host `WebFetchCheckRedirect`/`WebFetchDialContext`. The asset-gate fuzzy rules are raw substring matches that allow on any candidate (`db/asset_intercept_match.go:72-91,193-203`), so they cannot serve as an egress matcher. | D1 | GAP | T-MATCH-1, T-WF-1..9 | 1 |
| C9 | Redirects | Default policy: 10 hops to any host (`webfetch.go:97`). | Per-hop check plus a re-implemented 10-hop cap | GAP | T-WF-2, T-WF-8 | 1 |
| C10 | Credential leakage | HELD: provider (`agent/provider_redirect_test.go:9`), WebSearch (`norma/tool/websearch_redirect_test.go:9`), WebFetch sends no credentials (`webfetch.go:236`). The custom http tool renders `{param}` unescaped into the URL (`server/customtool.go:427,497-503`), so `https://api.vendor.com{path}` with `path=@attacker.net/x` sends operator headers (`:441-443`) to attacker.net on the **first** request. Its plain client (`:444`) has no `CheckRedirect` and no pinned dial. | D2 | GAP (http tool) | T-CRED-1..5 | 1 |
| C11 | Direct retry bypasses proxies | `webfetch.go:143-148,155-175,238-250` retries directly on `tls:`/`eof`/reset errors, which a self-signed target triggers. With capture off, the WebFetch proxy is the global egress proxy (`server/manager.go:765-777`), so the retry leaks the source IP. With capture on, the retry skips recording and the upstream global proxy (`traffic/traffic.go:213-221`). | D6 | GAP | T-WF-6, T-WF-7 | 1 |
| C12 | Cold-digest laundering | The compressor (`agent/compaction.go:540`, prompt `:687`) has no data rule, and its output is shown as `cold_digests`. | Rule plus frames | GAP | T-DIGEST-1 | 2 |
| C13 | NOA summaries | Named as data in `normaTrustRules`; framing not verified. | – | UNKNOWN | audit report | 2 |
| C14 | Main agent treated as human | ToolSet origin is `"human"` (`mainagent.go:115`). Hints are stored as `human` (`tools.go:1568,1686`). Goals and hints are labelled "The human (main agent)" (`planner.go:195-205`). Constraints reach the worker system prompt (`worker.go:464`, `constraints.go:14`). | D.2 (Q5) | RESIDUAL | – | – |
| C15 | Planner-written intent summary in the worker system prompt, unframed (`worker.go:347-349,468`) | It is the worker's assignment; the planner data rule forbids relaying data. | – | RESIDUAL | – | – |
| C16 | RSI lessons (cross-task poisoning) | Opt-in `ARTEX_RSI_LESSONS=inject` (`server/lessons.go:26`). Flattened, capped, headed "not instructions" (`db/lessons.go:245-271`); no nonce. | – | PARTIAL / RESIDUAL | – | – |
| C17 | MCP tools and WebSearch queries as exfil channels | Destinations are operator-trusted (SECURITY.md:49-52); argument and query text is model-chosen. | – | RESIDUAL | – | 1 (doc) |

**Upstream status (corrected).**
- Port matrix rows `docs/upstream-port-matrix.md:11-13` mark `8c3bae4`/`c73e287`, `975f634` and `f6b29b8`/`1c3a5f4` as PORT. Only the boundary is delivered (#47, `:46`).
- Inventory: `975f634`, `1c3a5f4`, `f6b29b8`, `c73e287`, `8c3bae4`, `8acf1be`, `09ef702` are `dependency-review` (`tsv:46,82,84,90,91,106,125`). `62c9b4a` is `dependency-review` (`tsv:103`). `bc33db2` is `defer-independent` (`tsv:104`).
- Matrix `:19` says ADAPT `bc33db2`. Matrix `:20` cites `f6b29b8` only as the norma v0.4.8 dependency row.
- `8acf1be` and `09ef702` appear only in the inventory.

## 3. Acceptance criteria (numbers are stable)

1. **AC1 [PR 1] (C1)**: In a real harness session with the real WebFetch tool against an `httptest` lab target,
   payloads in body, HTML comment, `X-Powered-By` header and a forged `<<<END_UNTRUSTED_OUTPUT …>>>>` arrive inside
   exactly one frame, and the forgery is neutralised.
2. **AC2 [PR 2] (C1b)**: Read of a JSONL file holding an injection payload returns it inside one frame.
3. **AC3 [PR 1] (C8, matcher)**: Domain rules match only the normalised parsed hostname (lowercase, trailing dot
   stripped, IDNA→ASCII). Fuzzy domain means a label-boundary suffix. URL rules match on scheme+host+port+path
   prefix. Table cases: `http://allowed.example@attacker.net/`, `http://attacker.net/?allowed.example`,
   `allowed.example.attacker.net`, `BLOCKED.example.`, and the punycode form of a Unicode rule.
4. **AC4 [PR 1] (C8)**: A host matched by an enabled global or task block rule returns `TARGET_BLOCKED`, and the
   lab server receives nothing.
5. **AC5 [PR 1] (C8)**: With task allow rules, a host outside them returns `TARGET_NOT_ALLOWED`. A domain allow rule
   is satisfied by host or URL alone. Block rules and the denylist apply to host, URL and every resolved IP.
6. **AC6 [PR 1] (C9)**: A redirect to `169.254.169.254` or to a blocked or not-allowed host stops at that hop, and
   the next hop is never dialled. In-policy redirects work. An 11th hop fails like net/http's default.
7. **AC7 [PR 1] (C8, control plane)**: Resolver and local-interface enumerator are injected. These are refused:
   `127.0.0.1:<artex port>`, `<fake iface addr>:<db port>`, `<resolved DSN host>:<db port>`,
   `::ffff:169.254.169.254`, `64:ff9b::a9fe:a9fe`, `[::]:8787`. A loopback lab target on another port is allowed,
   and so is dialling the configured proxy.
8. **AC8 [PR 1] (C8, dial)**: The direct path dials exactly the checked IP, so rebinding on the second lookup cannot
   change it. With `HTTP_PROXY` set, the wrapper and hop checks still refuse a blocked host before any request.
9. **AC9 [PR 1] (C8, fail closed)**:
   - A rule-read error returns `TARGET_POLICY_UNAVAILABLE`.
   - `ExplorationID>0` with `TaskID==0` fails closed.
   - With no loader registered, only the denylist applies.
   - Chat sessions (`TaskID 0`, `runinfo.go:17`) apply global rules plus the denylist.
   - RunInfo provably reaches the tool context in planner and worker runs.
10. **AC10 [PR 1] (C8, assembly)**: For every role (planner, worker, chat, main), the tool-name set is unchanged and
    WebFetch appears once, wrapped by `guardPanic`. A panic becomes a tool error.
11. **AC11 [PR 1] (C11)**: With a global proxy configured, in either capture mode, a `tls:`-class proxy failure
    produces **no** direct request. With capture on and no global proxy, the retry stays and is logged
    (`[webfetch] direct_retry`).
12. **AC12 [PR 1] (C10)**: For the custom http tool:
    - Templates whose authority has no placeholder refuse any other rendered origin (userinfo `@`, host, scheme,
      port).
    - If the authority holds a placeholder and the tool has operator-set headers, the request is refused with
      `TEMPLATED_ORIGIN_NOT_ALLOWED` unless the rendered parsed host matches an enabled task allow rule. Templates
      without operator headers run under D1 only.
    - The initial URL and every hop pass D1.
    - The connection uses D1's pinned `DialContext` (rebinding-safe; proxy exempt).
    - Cross-origin hops (exact scheme+host+port; https→http is cross-origin) drop all operator-set headers plus
      Authorization/Cookie.
    - Cross-origin 307/308 with a body is refused.
    - The 10-hop cap holds.
13. **AC13 [PR 2] (C3/C5)**: The planner input frames worker output, finding detail, `recent_facts`,
    `finding_list`, `cold_digests`, `related_tasks` and agent hints (hint nodes with origin ≠ `human`; empty today,
    C14). Each sits in an `[[ARTEX_DATA nonce=N field=…]]` frame and forged stems are neutralised. Frames are
    outside the `plannerSnapshotPrefix` JSON and within the `plannerPacketBudget` checks. With an unchanged goal, the
    planner system prompt is byte-identical across rounds, with and without `ARTEX_CACHE_PREFIX=1`.
14. **AC14 [PR 2] (C3)**: The worker launch frames the asset JSON and overview, not the intent summary (C15). The
    worker system prompt is ≤14000 bytes (`worker_launch_context_test.go:131`).
15. **AC15 [PR 2] (C2)**: Under a DB override without the untrusted block, worker, pentest, main, auto and chat
    carry the untrusted rule exactly once, and planner and compactor the data rule once. This holds also with
    `ARTEX_DATA_ENVELOPE=0`.
16. **AC16 [PR 2] (C6)**: The planner input ends with the goal anchor. With `ARTEX_DATA_ENVELOPE=0`, envelope and
    anchor are absent and the input is byte-identical to today's.
17. **AC17 [PR 2] (C4)**: `record_fact` accepts `source ∈ {target_response, tool_output, third_party_intel,
    inference}` (empty → `inference`). It refuses `operator_input` and unknown values from any model tool. `origin`
    and `source` appear in `compactNode`, `list_facts` and `recent_facts`. `prove_goal` behaviour is unchanged.
18. **AC18 [deferred to the goal-proof iteration]**: `prove_goal` refusal of `target_response` facts. Not built in
    D.1.
19. **AC19 [PR 2] (C4)**: The schema refresh, run twice, adds `source` once and keeps operator-edited text.
20. **AC20 [PR 2] (C12)**: The compressor request carries the compactor rule and frames member text.
21. **AC21 [PR 1] (C7)**: The `insert_assets` asset gate fails closed when rules cannot be read.
22. **AC22 [PR 1 / PR 2] (observability)**: The §7 log lines of each PR are content-free: no URL, host, body or
    header values.
23. **AC23 [PR 1 and PR 2] (regression)**: The §6 command set passes with no new failures, and
    `scripts/verify-norma-source.py` shows no SDK change.

**Deferred to the goal-proof iteration** (decisions Q6–Q8):
- AC18
- T-FACT-3 `TestProveGoalRefusesTargetResponse` and T-FACT-4 `TestProveGoalAcceptsFindingAndLegacyFact`
- the `prove_goal` description change
- the completion-behaviour risk and log line
- server-checkable proof
- `confidence=observed`

## 4. Design

**D1 [PR 1]. Egress policy for WebFetch (host-side, unconditional). Files: `agent/webfetch_policy.go`,
`db/egress_match.go`, `agent/model_metrics.go`, `cmd/artex/main.go`, `server/server.go`.**

*Wiring.*
- `attachWebFetchPolicy(opts)` runs in `attachModelMetrics`. When `EnableWebFetch` is set, it clears it and appends
  `guardPanic(policyWebFetch{tool.NewWebFetch(cfg)})`. Name, schema and description are unchanged; only the position
  moves (AC10).
- `cfg` sets the existing `CheckRedirect` and `DialContext` options.
- `DisableDirectRetry` is true whenever a global egress proxy is configured (D6).

*Checks.* They run in the wrapper (initial URL) and in `CheckRedirect` (every hop, with the 10-hop cap):
- Parse with `net/url` and normalise the host. Resolve once through an injectable resolver.
- Apply `netip` `Unmap` and decode embedded IPv4 (`::a.b.c.d`, `64:ff9b::/96`, `2002::/16`).
- Check the denylist, then the block rules, then the allow rules.
- The checked IPs ride in the request context. On the direct path, `DialContext` dials exactly one checked IP.
- The dial hook exempts only the configured proxy address, or the address `http.ProxyFromEnvironment` returns
  **for that specific request**, and nothing looser.
- Resolver and `net.InterfaceAddrs` enumeration are injectable.
- When a proxy resolves the name, pre-resolution is advisory, and rebinding inside that proxy is RESIDUAL.

*Matcher.* `db.EvaluateEgress(block, allow, parsedURL, ips)` uses URL-aware semantics (AC3). `EvaluateAssetGate`
and `insert_assets` matching are unchanged.

*Denylist (decision Q2).*
- Always blocked: metadata and link-local ranges, namely `169.254.0.0/16` (incl. ECS `169.254.170.2`), `fe80::/10`,
  `fd00:ec2::254`, `100.100.100.200`, `168.63.129.16`.
- Always blocked as (local address, port): ARTEX listen port (`cmd/artex/main.go:52`) and recording-proxy port
  (`:54`). Local addresses are `127.0.0.0/8`, `::1`, `0.0.0.0/8`, `::` and every local interface address.
- DB pairs, both blocked:
  - (every IP the DSN host resolves to at check time, DB port), from `db.DSN()` (`db/db.go:96`); the compose DB host
    is `postgres` (`docker-compose.yml:2`);
  - (local addresses, DB port).
  A lab target on that exact IP and port is unreachable, which is accepted.
- Other loopback and private ports are allowed.
- Registration: `main.go` passes `*addr` and `*proxy`, and the server calls
  `agent.SetTargetPolicy(loader, controlPlane, globalProxySet)`, a synchronized hook.
- `task_scope` is not used, because agents write it.

*Relation to guard intercept.* The operator's intercept rules and judge still run first on worker and chat
(`intercept.go:144`). D1 is a separate destination check inside the tool, for every role.

Rejected:
- `EvaluateAssetGate`: substring bypasses.
- A PreToolUse hook: no hooks on planner or main sessions.
- An SDK patch: not needed.
- Blocking all loopback: breaks labs and the proxy dial.

**D2 [PR 1]. Custom http tool (unconditional). File: `server/customtool.go`.** Implements AC12:
- Origin pinning from the template prefix before the first placeholder.
- Templated authority: operator headers only to a host matched by an enabled task allow rule; otherwise the request
  is refused. Global rules are block-only (`db/asset_intercept.go:16-17`). In chat sessions (task id 0) no task
  allow rules exist, so such a tool is **always refused there**. The audit doc and SECURITY.md say so.
- D1 policy on the initial URL and every hop.
- D1 pinned `DialContext` on the transport, replacing the plain client at `:444` and the `httpProxyTransport`
  transport.
- `CheckRedirect` with exact-origin header stripping, refusal of 307/308 replays that carry a body, and the 10-hop
  cap.

Rejected:
- Escaping parameters: breaks path/query templates.
- Silently stripping headers: gives confusing authentication failures.

**D3 [PR 2]. Data envelope (default ON, opt-out `ARTEX_DATA_ENVELOPE=0`).** Port `975f634` to:
- `renderTriggers` (`planner.go:187-226`)
- the planner overview (`:306`) and the journal path, with frames outside the JSON prefix
- the worker overview and asset JSON (`worker.go:361-375,477`)

The nonce is per user message. The planner system prompt is nonce-free, and a worker gets one nonce per launch.
The nonce source is injectable. Default ON per #23, following the `ARTEX_UNTRUSTED_BOUNDARY` precedent. Overhead is
measured.

Rejected:
- XML tags.
- Framing the intent summary (C15).

**D4 [PR 2]. Code-owned tails, data rules, goal anchor.** Port `8acf1be`/`09ef702`:
- `untrustedContentTail` for worker, pentest, main, auto and chat.
- `untrustedDataRules` in `plannerSystem`.
- `compactorDataRule`.
- The bbot-recon SKILL.md note and `server/shipped_skills_test.go`.

Tails and rules are unconditional. `ARTEX_DATA_ENVELOPE=0` switches off only D3 and `plannerGoalAnchor`.

**D5 [PR 2]. Fact labels (unconditional; limited claim).**
- Port `62c9b4a`/`bc33db2` without the `prove_goal` change:
  - `source` enum (AC17)
  - `origin`/`source` surfaced to the planner
  - `operator_input` refused for every model tool (the main agent's origin is `human`, C14; the value stays
    reserved for a future UI/API path)
  - idempotent schema refresh (AC19)
- What it enforces: no model can claim operator provenance, and the planner sees the writer and the declared source.
- What it does **not** do: establish provenance (a model can label target data `tool_output`), or change goal
  proof or task completion. Those are deferred (Q6–Q8).

**D6 [PR 1]. Direct retry (decision Q3).** Disabled when a global egress proxy is configured, in either capture
mode. With capture on and no global proxy, it is kept and logged: the dial wrapper sees a non-proxy dial while a
proxy is configured.

**D7 [D.2]. Recording-proxy enforcement (UNKNOWN; decision Q1).** go-mitmproxy v1.9.2 (`go.mod:11`) carries Bash
traffic when capture is on. ARTEX uses `SetUpstreamProxy` (`traffic/traffic.go:221`) and the addon
`Response`/`RequestError` hooks (`:392,403`). Whether a request-phase addon can short-circuit a request is UNKNOWN,
because the module source is not on this host. This is the first D.2 item, starting with a spike.

## 5. Tasks (vertical slices; red tests first)

The branch accepts red intermediate commits. Each PR merges only when green.

| # | PR | Slice | Files | Red tests | Done when |
|---|---|---|---|---|---|
| S0 | 1 | Inventory | read-only | – | Raw list of host-built messages with derived text, and of non-test `http.Client{` sites |
| S1-net | 1 | Lab target and network red tests | `agent/adversarial_lab_test.go`, `agent/webfetch_policy_test.go`, `db/egress_match_test.go`, `server/customtool_redirect_test.go`, `agent/insert_assets_failclosed_test.go` | T-LAB-1 (expected green), T-MATCH-1, T-WF-1..9, T-CRED-1..5, T-INS-1, per-role and RunInfo tests | Red output mapped to C-rows |
| S4 | 1 | D1 + D6 | `db/egress_match.go`, `agent/webfetch_policy.go`, `agent/model_metrics.go`, `cmd/artex/main.go`, `server/server.go` | S1-net WebFetch and matcher tests | Green; `-race` |
| S5 | 1 | D2 | `server/customtool.go` | T-CRED-1..5 | Green; `TestRunHTTPTool` unchanged |
| S5b | 1 | `insert_assets` fail-closed | `agent/tools_insert.go` | T-INS-1 | Green |
| S8-PR1 | 1 | Verification | – | AC1, AC3–AC12, AC21–AC23 | Raw local container output |
| S9-PR1 | 1 | Docs | §8 PR 1 items | – | DOCUMENT gate evidence |
| S2 | 2 | Prompt red tests | `agent/untrusted_surfaces_test.go`, `agent/data_envelope_test.go`, `agent/adversarial_lab_test.go` (T-LAB-2) | T-LAB-2, T-PLAN-1..3, T-WORKER-1, T-PROMPT-1, T-DIGEST-1 | Red output |
| S3 | 2 | Fact red tests (PG) | `agent/fact_provenance_test.go`, `server/fact_source_schema_test.go` | T-FACT-1/2, T-SCHEMA-1 | Red, no SKIP |
| S6 | 2 | D3 + D4 | `agent/data_envelope.go`, `agent/untrusted_prompt.go`, `agent/planner.go`, `agent/worker.go`, `agent/compaction.go`, `agent/chat.go`, `agent/mainagent.go`, `agent/promptcatalog.go`, `agent/tools.go` (overview), `skills/bbot-recon/SKILL.md`, `server/shipped_skills_test.go` | S2 | Green; `cache_prefix_test.go:107-131`, `worker_launch_context_test.go:128-131`, `prompt_injection_seed_test.go`, `planner_context_test.go:65,87,239-244` pass; overhead printed |
| S7 | 2 | D5 (after S6) | `agent/tools.go` (`compactNode`, `recordFact`; **not** `proveGoal`), default prompt text, `server/orchestration.go` | S3 | Green on PG; `prove_goal` diff empty |
| S8-PR2 | 2 | Verification | – | AC2, AC13–AC17, AC19, AC20, AC22, AC23 | Raw output |
| S9-PR2 | 2 | Docs | §8 PR 2 items | – | DOCUMENT gate evidence |

## 6. Test plan

Primary evidence comes from local runs: a `golang:1.26` container plus `postgres:16-alpine`, source streamed in
with tar, commit recorded. CI only corroborates. Platform: linux/amd64. Others are UNKNOWN.

The same command set gates both PRs (AC23):
1. `go test -race ./agent/... ./server/... ./db/... ./cmd/...`
2. `go test -race ./...` from the module root
3. `cd third_party/norma && go test ./...`
4. `python3 scripts/verify-norma-source.py`

**PR 1 tests:**
- T-LAB-1 `TestAdversarialLabBoundaryFramesAllPayloads`
- T-MATCH-1 `TestEgressMatchParsedHostOnly` (userinfo, query, label suffix, trailing dot, case, punycode)
- T-WF-1 `TestWebFetchPolicyBlockRule`
- T-WF-2 `TestWebFetchPolicyRedirectHopBlocked`
- T-WF-3 `TestWebFetchPolicyAllowList` (domain allow rule keeps its resolved IP)
- T-WF-4 `TestWebFetchPolicyControlPlaneAndMetadata` (injected resolver and interface enumerator; mapped and NAT64
  forms; DSN host `postgres` via the fake resolver; loopback lab port allowed)
- T-WF-5 `TestWebFetchPolicyFailsClosed` (read error; `ExplorationID` without `TaskID`; no loader; chat)
- T-WF-6 `TestWebFetchNoDirectRetryWithGlobalProxy`
- T-WF-7 `TestWebFetchDirectRetryLogged`
- T-WF-8 `TestWebFetchRedirectHopCap`
- T-WF-9 `TestWebFetchDialPinnedAgainstRebinding` (also with `HTTP_PROXY` set)
- `TestWebFetchToolSetPerRole` (AC10, incl. panic → tool error)
- `TestRunInfoReachesToolContext`
- T-CRED-1 `TestCustomHTTPOriginPinned`
- T-CRED-2 `TestCustomHTTPCrossOriginStripsHeaders` (incl. https→http)
- T-CRED-3 `TestCustomHTTP307BodyRefused`
- T-CRED-4 `TestCustomHTTPInitialURLPolicy` (incl. rebinding: second lookup `127.0.0.1:<artex port>`, the dial uses
  the checked IP)
- T-CRED-5 `TestCustomHTTPTemplatedAuthorityHeaders` (operator header: refused without a task allow rule, sent with
  one, refused in a task-id-0 session; a header-less template runs)
- T-INS-1 `TestInsertAssetsGateFailsClosed`

**PR 2 tests:**
- T-LAB-2 `TestAdversarialLabJSONLReadFramed`
- T-PLAN-1 `TestPlannerTriggersFramedAndForgeryNeutralised`
- T-PLAN-2 `TestPlannerSystemStableAcrossRounds`
- T-PLAN-3 `TestPlannerGoalAnchorIsLastAndOptOut`
- T-WORKER-1 `TestWorkerLaunchFramesAssets`
- T-PROMPT-1 `TestCodeOwnedTailsSurviveOverrideAndOptOut`
- T-DIGEST-1 `TestDigestCompressorDataRule`
- T-FACT-1 `TestFactSourceEnum`
- T-FACT-2 `TestFactOriginSurfaced`
- T-SCHEMA-1 `TestFactSourceSchemaIdempotent`

**Deferred:** T-FACT-3, T-FACT-4.

PG-backed tests must not SKIP during a gate. Network tests use loopback `httptest` with an injected resolver.

**What these tests prove, and what they do not.** They prove structure: framing, ordering, server-side refusals,
and network policy on the initial request, every hop and every direct dial. They do **not** prove that a model
ignores injected text. They do not cover Bash, MCP or WebSearch egress, or resolution inside a remote proxy.

## 7. Risks, rollback, observability

| PR | Risk | Mitigation / acceptance |
|---|---|---|
| 1 | A lab target is blocked | Narrow denylist; loopback lab test |
| 1 | Bash/curl/MCP/WebSearch bypass | **Accepted residual**; D.2 (D7) |
| 1 | Rebinding inside an env or remote proxy | **Accepted residual**; stated in docs |
| 1 | Fetches through a global proxy fail without a direct fallback | Intended: no source-IP leak |
| 1 | Templated-host http tools with operator headers refused in chat or without allow rules | Intended; error explains the needed task allow rule; documented |
| 1 | Fail-closed WebFetch or `insert_assets` while the DB is down | Intended; logged with a code |
| 1 | Tool-order shift moves the prompt-cache prefix | One-time; per-role test; token-audit note |
| 1 | `SetTargetPolicy` races | Synchronized hook; `-race` |
| 2 | Envelope overhead shifts the #1/#2/#33 baselines | Deterministic byte counts; opt-out; 14000-byte budget test |
| 2 | Schema refresh clobbers operator edits | Additive, idempotent (T-SCHEMA-1); database-reviewer |
| 2 | Declared `source` spoofed | **Accepted**; D5 claims only labelling and display |

**Rollback.**
- Each slice is one commit (`git revert`), and each PR can be reverted as a whole.
- PR 2's schema refresh only adds an optional property that older code ignores.
- `ARTEX_DATA_ENVELOPE=0` disables D3 and the anchor.
- D1, D2, D4, D5 and S5b roll back only by revert.
- Rehearsal: run the pre-merge commit against a refreshed DB.

**Observability.** Content-free log lines, watched for 14 days after each PR:
- `[PR 1]` `[webfetch-policy] decision=<TARGET_BLOCKED|TARGET_NOT_ALLOWED|TARGET_CONTROL_PLANE|TARGET_POLICY_UNAVAILABLE> hop=<n> task=<id>`
- `[PR 1]` `[customtool-http] decision=<ORIGIN_MISMATCH|TEMPLATED_ORIGIN_NOT_ALLOWED|…> tool=<name>`
- `[PR 1]` `[webfetch] direct_retry task=<id>` (capture on, no global proxy)
- `[PR 1]` `[assets] gate_unavailable`
- `[PR 2]` `[envelope] neutralised=<n> surface=<…>`
- `[PR 2]` `[facts] record_refused reason=<code>`

Failure signals: operators report blocked labs, `POLICY_UNAVAILABLE` bursts, refused custom http tools.

## 8. Documentation impact

**PR 1:**
- **`SECURITY.md`**: what is checked. WebFetch and custom http destinations against the denylist and operator rules,
  hops, header hygiene, templated-host refusal including the chat case. Residuals: shell, MCP and WebSearch egress,
  proxy-side resolution, model compliance, C14–C17. Never "destinations enforced".
- **`docs/upstream-port-matrix.md`**: `f6b29b8`/`1c3a5f4` partially adapted (WebFetch and http tool via own
  matcher; shell gate deferred), plus the corrected citations.
- **New `docs/agentic-attack-audit.md`**:
  - C1, C7–C11, C17 with results; negative results C1 HELD and provider/WebSearch redirect HELD;
  - note that in chat sessions (task id 0) there are no task allow rules, so a custom http tool with a templated
    host and operator headers is always refused there;
  - other rows marked "pending PR 2" or "deferred (goal-proof iteration)";
  - what fixtures cannot prove.
- **`CHANGELOG.md`**: Security (D1, D2, S5b); Changed (direct retry off with a global proxy; templated-host http
  tools need a task allow rule).
- **New ADR (`docs/adr/`, new directory)**: D1 uses its own URL-aware matcher plus the control-plane denylist, not
  `task_scope` or `EvaluateAssetGate`.
- **`docs/token-audit.md`**: the WebFetch tool-order note.

**PR 2:**
- **`SECURITY.md`**: envelope, code-owned rules, the D5 limited claim.
- **`docs/upstream-port-matrix.md`**: rows for `975f634`, `8acf1be`/`09ef702`, `62c9b4a`/`bc33db2`, with the
  refusal deferred.
- **`docs/agentic-attack-audit.md`**: rows C1b–C6, C12–C16.
- **`docs/token-audit.md`**: envelope overhead.
- **`README.md` and `.env.example`**: `ARTEX_DATA_ENVELOPE`.
- **`CHANGELOG.md`**: the corresponding entries.

**Both:** `third_party/norma/README.artex.md` is unchanged. `.gitignore:28` ignores `/docs/*`, so new docs need
`git add -f` or an exception (orchestrator decision).

## 9. Staffing plan

| # | PR | Task | Role | Tier | Effort | Why | Group | Gate | Next rung |
|---|---|---|---|---|---|---|---|---|---|
| S0 | 1 | inventory | minor | haiku | low | mechanical | 1A | raw list | – |
| S1-net | 1 | lab and network red tests | implementer | sonnet | medium | spec'd, test-only | 1A | red mapped | sonnet high |
| S4 | 1 | D1+D6 | implementer | sonnet | high | security-critical, 4 packages | 1B | green; security-reviewer **opus**; silent-failure-hunter sonnet | implementer opus |
| S5 | 1 | D2 | implementer | sonnet | medium | origin/redirect subtleties | 1B (after S4 matcher API) | green; security-reviewer (combined with S4) | sonnet high |
| S5b | 1 | `insert_assets` fail-closed | implementer | haiku | low | one hunk, test given | 1B | T-INS-1 | sonnet low |
| S8-PR1 | 1 | TEST/RETEST | qa-engineer + minor | sonnet / haiku | medium / low | mapping / runs | 1C | raw output | – |
| S9-PR1 | 1 | docs | architect | opus | medium | owns docs | 1D | skeptic sonnet; minor link/grep | – |
| S2, S3 | 2 | prompt and fact red tests | implementer | sonnet | medium | spec'd | 2A | red, no SKIP | sonnet high |
| S6 | 2 | D3+D4 | implementer | sonnet | high | multi-surface, cache/budget tests | 2B | green; skeptic sonnet | implementer opus |
| S7 | 2 | D5 (after S6) | implementer | sonnet | medium | upstream reference | 2C | green; database-reviewer sonnet | sonnet high |
| S8-PR2, S9-PR2 | 2 | verification, docs | qa-engineer / architect | sonnet / opus | medium | as PR 1 | 2D | as PR 1 | – |
| G | both | PLAN, TEST, DOCUMENT gates | advisor | opus | high | judgment | – | APPROVE | – |

In group 1B, S4 owns `db/egress_match.go`, `agent/webfetch_policy.go`, `model_metrics.go`, `cmd/artex/main.go` and
`server/server.go`; S5 owns `customtool.go` and starts once S4's matcher API exists; S5b owns `tools_insert.go`. In
PR 2, S7 runs after S6 because both touch `tools.go`, `planner.go` and `worker.go`. Top tier is used only for gates,
the S4/S5 security review and the docs.

## 10. Decisions (approved by the user, 2026-11-17)

| Q | Question | Decision |
|---|---|---|
| Q1 | D.2 scope for shell and captured-traffic egress | A go-mitmproxy addon spike first (D7); the upstream target gate (`f6b29b8`/`1c3a5f4`) only if the proxy route fails; planned at deep tier |
| Q2 | Denylist breadth | Metadata ranges plus ARTEX's own ports only (§4 D1); other loopback and private allowed |
| Q3 | Direct retry with capture on and no global proxy | Kept and logged; disabled whenever a global proxy is configured |
| Q4 | Guard hooks on planner and main sessions | Deferred to D.2 |
| Q5 | `main_agent` origin instead of `human` | Deferred to D.2 (data/UI note) |
| Q6 | Server-checkable goal proof (retest "reproduced" or operator confirmation) | Separate goal-proof iteration |
| Q7 | `confidence=observed` rule for `prove_goal` | In the goal-proof iteration |
| Q8 | `prove_goal` refusal of `target_response` facts | Moved out of D.1 to the goal-proof iteration. D.1 keeps the `source` enum, `origin`/`source` display, the `operator_input` refusal and the schema refresh. Task completion does not change |
| Delivery | One PR or two | Two PRs, network first: PR 1 on this branch (S0, S1-net, S4, S5, S5b plus tests, docs, audit rows); PR 2 later (S2, S3, S6, S7) |

## 10a. PR 1 as built (deviations from this plan)

Recorded at DOCUMENT from the working tree of `phase-d/untrusted-boundary-audit`. The
sections above are kept as approved; where they differ, this list and the code are
authoritative.

- **New code `TARGET_UNRESOLVED`** (`agent/webfetch_policy.go`). A name that does not
  resolve locally is refused when no global or environment proxy is set. With such a
  proxy, only name checks apply (residual).
- **No loader fails closed.** AC9 said "with no loader registered, only the denylist
  applies". As built, the denylist still answers first, and everything that passes it
  is refused with `TARGET_POLICY_UNAVAILABLE`. A failure to enumerate local interfaces
  is refused the same way.
- **`REDIRECT_BODY_REFUSED`** is the code for the refused cross-origin 307/308 body
  replay (AC12). Custom http refusals log
  `[customtool-http] decision=<code> hop=<n> task=<id>`, not `tool=<name>` (§7).
  Refusals by the shared policy also log the `[webfetch-policy]` line.
- **Schemeless URL block rules.** A pattern starting with `/` is a path rule:
  `exact_url` is a case-insensitive string prefix of the cleaned path (so `/admin` also
  blocks `/administrator`), and `fuzzy_url` is a substring of path plus query. A pattern
  starting with a host (`evil.com/admin`) needs that host on a label boundary plus the
  path. Schemeless allow rules never match. A URL-type block rule that is only a host name
  (`evil.com`, `[::1]:8080`) blocks that host on a label boundary (see ADR 0001).
- **Sticky header stripping.** Once a chain leaves the original origin, operator
  headers, `Authorization` and `Cookie` stay stripped on every later hop, including a
  return to the origin. Keep-alives are off on the custom http transport.
- **Fixed error text.** Policy refusals carry a fixed reason, never a store error.
  Custom http transport errors are `request failed: <class>` (`timeout`, `canceled`,
  `dns`, `tls`, `connect`, `redirect: stopped after 10 redirects`, `other`).
  Placeholder names containing `{ } / ? #` are not substituted.
- **Proxied-path residual wording.** This covers every proxy, including the default
  in-process recording proxy and per-tool proxies, not only remote or environment
  proxies: addresses are checked at check time only, and rebinding and IP allow-rule
  pinning are not enforced on the proxy leg.
- **Server wiring.** `server.WithControlPlane(listenAddr, recordingProxyAddr)` is the
  option `cmd/artex` passes. `registerTargetPolicy` (`server/target_policy.go`) captures
  the database endpoint once at startup, before the secret scrub, and runs again when
  the global proxy changes.
- **IP allow rules.** When only IP or CIDR allow rules admit a destination, only the
  addresses those rules name are dialled.

Verification evidence for PR 1:

Local run on 2026-11-18 of the tree committed as `1d88411` (on `main` `43208e5`), Go 1.26.11 in a
`golang:1.26` container with a fresh PostgreSQL 16 for each command set:

- `gofmt -l` on every changed and new Go file: no output.
- `go build ./...`: exit 0. `go vet ./...`: exit 0.
- `go test -p 1 -count=1 -v ./...`: exit 0; 1367 passing test and subtest results, no
  failures, 7 skips. Five skips exist on `main`; the other two are
  `TestWebFetchEnvProxyChild` and `TestCustomHTTPSchemeChild`, helper processes that
  skip unless their parent test starts them.
- `go test -race -p 1 -count=1 ./...`: exit 0, no data race reported.
- `go test ./...` in `third_party/norma`: exit 0. No file under `third_party/` changed.
- `python3 scripts/verify-norma-source.py`: exit 0 ("Verified upstream module sum and
  all original file hashes").

Before the hardening, the 17 new network tests failed on assertions and
`TestAdversarialLabBoundaryFramesAllPayloads` and `TestRunInfoReachesToolContext`
passed. Hosted CI has not run on this change yet.

Not part of this change: four `db` tests (`TestCompanyScopeAttribution`,
`TestPoolProfilesOrder`, `TestProfileRetryRoundTrip`, `TestProfileRetryClampedOnSave`)
fail when the `db` package runs a second time against the same database. They fail
the same way on `main` `43208e5`.

## 11. Changes after PLAN review

Every finding was checked against the code before it was accepted.
- **Security lens, accepted.** H1 (own URL-aware matcher). H2 (D6). H3 (origin pin and initial check). M1. M2 a–e.
  M3 (concrete default). L1. L2 (S5b). Surfaces C14–C17. Residual wording.
- **Skeptic lens, accepted.** 1–10: S7 serialised; file lists completed; goal residuals; policy semantics; `main.go`
  and `db.DSN`; hop cap; budgets and prefixes; hint definition; proxy-aware retry detection; citations; guard
  relation and T-LAB-2.
- **Accepted in part.** M4. `operator_input` is removed from model input instead of renaming the origin (Q5).
  Server-verifiable proof went to Q6.
- **Advisor gate 1, accepted.**
  1. Pinned dial for the http tool (T-CRED-4 rebinding).
  2. Templated authority needs a task allow rule (T-CRED-5).
  3. Q8.
  4. Injectable interface enumeration.
  5. Exact environment-proxy exemption; DB pairs; `main.go:52/:54`.
  Also S5b on haiku and the split option.
- **User approval (2026-11-17).** Decisions in §10.
  - The `prove_goal` refusal was removed from the goal, D5, AC18 (kept as a deferred number), T-FACT-3/4, the risks,
    the log lines and the CHANGELOG.
  - T-CRED range corrected to 1..5.
  - Everything is tagged PR 1 or PR 2, and the "PR 1 scope" list was added.
  - Chat/task-id-0 note for the audit doc.
- **Unverified.** D7 (go-mitmproxy request blocking): UNKNOWN.
- **Rejected.** None.
