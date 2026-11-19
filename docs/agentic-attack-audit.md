# Agentic attack-class audit (#7)

Issue #7 mirrors upstream BaaRai/ARTEX#124. This report records, for each attack
class, what protected ARTEX before Phase D.1, what changed, which test proves it and
the status now. The classes and the "before" verdicts come from §2 of
[phase-d1-plan.md](phase-d1-plan.md), verified against the code at `43208e5`.

PR 1 (network) was delivered on branch `phase-d/untrusted-boundary-audit`. This
revision adds **PR 2 (prompt envelope and fact labels)** on branch
`phase-d/data-envelope-provenance`: rows C1b–C6 and C12–C16 now carry their results.

## Status values

- **HELD**: a protection already existed. A test now records that it holds.
- **FIXED in PR 1**: the gap is closed for the scope named in the row.
- **ADVISORY in PR 2**: PR 2 added the frame, rule or label named in the row. It marks
  data for the model; it cannot stop a model from following injected text.
- **DEFERRED**: moved out of D.1 (goal-proof iteration or a D.1 follow-up).
- **RESIDUAL**: not fixed in D.1. Named here and in [SECURITY.md](../SECURITY.md).
- **UNKNOWN**: cannot be verified from the code.

## Results

| # | Class | Before | What PR 1 or PR 2 changed | Tests | Status after PR 2 |
|---|---|---|---|---|---|
| C1 | Untrusted boundary on tool results | HELD | Nothing. WebFetch is now wrapped by the destination policy, and its results still pass through the same boundary framing. | T-LAB-1 `TestAdversarialLabBoundaryFramesAllPayloads` | HELD |
| C1b | bbot JSONL and other tool-ingested files | HELD (framing) | PR 2: a JSONL file read through the Read tool arrives inside one boundary frame (the C1 framing, unchanged). The bbot-recon skill (`skills/bbot-recon/SKILL.md`) now says every event field is untrusted data, sends only `scope_distance == 0` events to `insert_assets`, never auto-scopes out-of-scope events, and records them as intel facts. | T-LAB-2 `TestAdversarialLabJSONLReadFramed`; `TestBbotSkillIngestionTreatsEventsAsUntrusted` | HELD (framing). Skill note: ADVISORY in PR 2 |
| C2 | Code-owned untrusted rules | PARTIAL | PR 2: the untrusted-content block moved out of the editable worker, pentest, main and auto seeds and is appended in code (`untrustedContentTail`, `agent/untrusted_prompt.go`) to the worker, pentest, main, auto and every chat agent prompt, exactly once, also under a DB prompt override. The planner system prompt gets `untrustedDataRules`, the digest compressor `untrustedDataRules` plus `compactorDataRule`. Not switched off by `ARTEX_DATA_ENVELOPE=0`. | T-PROMPT-1 `TestCodeOwnedTailsSurviveOverrideAndOptOut`; `TestPromptSeedsCarryInjectionHardening` (adapted) | ADVISORY in PR 2 |
| C3 | Envelope on host-built messages | GAP | PR 2: derived text in the planner input and the worker launch sits in `[[ARTEX_DATA nonce=N field=…]]` frames with one random nonce per message; forged stems are neutralised (`agent/data_envelope.go`). Planner: `worker_output`, `finding`, `recent_facts`, `finding_list`, `cold_digests`, `related_tasks`, `agent_hints`, `task_credentials`, `projection_notes`. Worker: the overview's derived fields and the `assets` JSON; the intent summary is not framed (C15). Default on; `ARTEX_DATA_ENVELOPE=0` switches it off. | T-PLAN-1 `TestPlannerTriggersFramedAndForgeryNeutralised`; T-WORKER-1 `TestWorkerLaunchFramesAssets`; `TestTaskCredentialsFramed`; `TestOptOutHintJSONIsPreChangeLiteral`; `TestEnvelopeOptOutDrawsNoNonce`; `TestEnvelopeNeutralisedLogLineIsContentFree`; `TestBoundedPlannerInputBudgetsPerPart` | ADVISORY in PR 2. Not framed: `stagnation_warning.directions` and intent summaries (C15) |
| C4 | Memory poisoning (facts) | GAP | PR 2: `record_fact` takes `source` (`target_response`, `tool_output`, `third_party_intel`, `inference`; empty means `inference`) and refuses `operator_input` and unknown values for every writer, without writing a fact. `origin` (set by the server) and `source` are shown in `list_facts`, `recent_facts`, `compactNode` output and the compressor input. `seedFactSourceSchema` adds the property to existing databases once. This labels; it does not establish provenance. | T-FACT-1 `TestFactSourceEnum`; T-FACT-2 `TestFactOriginSurfaced`; `TestFactRefusalLogContentFree`; T-SCHEMA-1 `TestFactSourceSchemaIdempotent`; `TestProveGoalIgnoresFactSource` | ADVISORY in PR 2 (labels; the declared source is spoofable). `prove_goal` refusal: DEFERRED (goal-proof iteration) |
| C5 | Plan hijacking | GAP | PR 2: worker output and finding details in planning triggers are framed (C3); `untrustedDataRules` tells the planner that data cannot add goals, targets or scope, end the task, or be relayed through `steer_work`. | T-PLAN-1 `TestPlannerTriggersFramedAndForgeryNeutralised`; T-PLAN-2 `TestPlannerSystemStableAcrossRounds` | ADVISORY in PR 2 |
| C6 | Goal drift | PARTIAL | PR 2: the planner input ends with the code-owned goal anchor (`plannerGoalAnchor`): task goal, operation constraints, and the rule that each new intent names the goal it serves. Off with `ARTEX_DATA_ENVELOPE=0`. | T-PLAN-3 `TestPlannerGoalAnchorIsLastAndOptOut` | ADVISORY in PR 2 (anchor). `goal_met` without evidence: RESIDUAL (Q6) |
| C7 | Tool abuse | PARTIAL | `insert_assets` fails closed: if the asset intercept rules cannot be read, the whole call is refused and nothing is inserted (`agent/tools_insert.go`). | T-INS-1 `TestInsertAssetsGateFailsClosed` | FIXED in PR 1 (`insert_assets`). Guard hooks on planner and main sessions: DEFERRED (D.1 follow-up, Q4). `task_scope` is still not enforced. |
| C8 | WebFetch SSRF and scope | GAP | Destination policy for WebFetch in every role, and for the custom http tool: control-plane denylist, operator block and allow rules, URL-aware matcher, pinned direct dials, fail closed (`agent/webfetch_policy.go`, `db/egress_match.go`, `server/target_policy.go`). | T-MATCH-1 `TestEgressMatchParsedHostOnly`; `TestEgressAllowedIPsFollowTheMatchingRule`; T-WF-1 `TestWebFetchPolicyBlockRule`; T-WF-3 `TestWebFetchPolicyAllowList`; T-WF-4 `TestWebFetchPolicyControlPlaneAndMetadata`; T-WF-5 `TestWebFetchPolicyFailsClosed`; T-WF-9 `TestWebFetchDialPinnedAgainstRebinding` (with its re-executed helper `TestWebFetchEnvProxyChild`, which skips when run on its own); `TestWebFetchResolvesNormalisedNameOnEveryPath`; `TestWebFetchIPAllowRuleDialsOnlyMatchingAddress`; `TestWebFetchPolicyUnavailableHidesStoreError`; `TestWebFetchToolSetPerRole`; `TestRunInfoReachesToolContext`; `TestTargetPolicyKeepsDBEndpointOnReRegistration`; `TestTargetPolicyKeepsEveryDBEndpointOnReRegistration`; `TestDatabaseEndpoint`; `TestWebFetchPolicyUsesCapturedDatabaseEndpoints`; `TestWebFetchPolicyDeniesEveryDatabaseEndpoint`; `TestWebFetchPolicyUnresolvedDatabaseHostsStayDenied`; `TestWebFetchPolicySocketDatabaseHostKeepsItsLocalPortDenied`; `TestWebFetchCompatibilityHostSpellingIsBlocked`; `TestWebFetchCompatibilityHostSpellingGoesOutCanonical`; `TestWebFetchZeroPaddedPortKeepsPolicy`; `TestWebFetchPolicyInvalidDestinationIsRefusedAndLogged`; `TestWebFetchNumericIPv4SpellingsAreCheckedAsAddresses` | FIXED in PR 1 for WebFetch and the custom http tool. Proxied paths: RESIDUAL (see below). |
| C9 | Redirects | GAP | Every hop passes the same policy. The 10-hop cap is re-implemented in `CheckRedirect`. | T-WF-2 `TestWebFetchPolicyRedirectHopBlocked`; T-WF-8 `TestWebFetchRedirectHopCap`; `TestWebFetchCompatibilityHostSpellingOnRedirectHop` | FIXED in PR 1 |
| C10 | Credential leakage | GAP (custom http tool); HELD (provider, WebSearch, WebFetch) | Custom http tool: origin pinning (`ORIGIN_MISMATCH`); templated host with operator headers needs a task allow rule (`TEMPLATED_ORIGIN_NOT_ALLOWED`); sticky stripping of operator headers, `Authorization` and `Cookie` after a cross-origin hop; cross-origin 307/308 body replay refused (`REDIRECT_BODY_REFUSED`); pinned dial; keep-alives off; content-free transport errors. | T-CRED-1 `TestCustomHTTPOriginPinned`; T-CRED-2 `TestCustomHTTPCrossOriginStripsHeaders` (its https-to-http case runs in the re-executed helper `TestCustomHTTPSchemeChild`, which skips when run on its own); T-CRED-3 `TestCustomHTTP307BodyRefused`; T-CRED-4 `TestCustomHTTPInitialURLPolicy`; T-CRED-5 `TestCustomHTTPTemplatedAuthorityHeaders`; `TestCustomHTTPChainStaysStripped`; `TestCustomHTTPOddPlaceholderNames`; `TestCustomHTTPErrorTextIsContentFree`; `TestCustomHTTPRecordingProxyPath`; `TestCustomHTTPCompatibilityHostSpellingIsBlocked`; `TestCustomHTTPCompatibilityHostSpellingGoesOutCanonical`; `TestCustomHTTPZeroPaddedPortKeepsPolicy`; `TestCustomHTTPNumericIPv4SpellingsAreCheckedAsAddresses` | FIXED in PR 1 (custom http tool). Provider and WebSearch redirects: HELD (existing tests, see below). |
| C11 | Direct retry bypasses proxies | GAP | WebFetch's direct retry is off whenever a global egress proxy is configured, in either capture mode. With capture on and no global proxy, it stays and is logged. | T-WF-6 `TestWebFetchNoDirectRetryWithGlobalProxy`; T-WF-7 `TestWebFetchDirectRetryLogged` | FIXED in PR 1. The retry with capture on and no global proxy is kept by decision (Q3). |
| C12 | Cold-digest laundering | GAP | PR 2: the compressor request frames member and anchor text with its own nonce, shows each member's `source` and `origin`, and its system prompt carries `untrustedDataRules` plus `compactorDataRule` (a directive in a member is mentioned only as quoted data). The summary memo key ignores the nonce. | T-DIGEST-1 `TestDigestCompressorDataRule`; `TestCompactionMemoHitsWithEnvelopeOn`; `TestCompressionInputOptOutIsPlain` | ADVISORY in PR 2 |
| C13 | NOA summaries | UNKNOWN | Nothing. PR 2 does not change NOA summaries. | – | UNKNOWN |
| C14 | Main agent treated as human | RESIDUAL | PR 2: hints with origin `human` stay outside the frames, but that origin also covers hints the main agent writes. The same holds for goal nodes: the main agent can add goals with `set_goals`, they are stored with origin `human`, shown unframed with the operator-set items, and they resume a finished task. Their heading now reads "entered through the console or the main agent" instead of claiming operator authorship. Hint triggers carry no origin. | `TestPlannerTriggersFramedAndForgeryNeutralised` (agent hints framed) | RESIDUAL (D.1 follow-up, Q5) |
| C15 | Planner intent summary in the worker system prompt, unframed | RESIDUAL | Nothing by design: the intent summary is the planner's own assignment. `stagnation_warning.directions`, derived from intent summaries, is not framed either. | T-WORKER-1 `TestWorkerLaunchFramesAssets` (asserts the summary stays unframed) | RESIDUAL |
| C16 | RSI lessons (cross-task poisoning) | PARTIAL / RESIDUAL | Nothing. The lessons block is appended to the planner and worker system prompts as before, outside any frame. | – | RESIDUAL |
| C17 | MCP tools and WebSearch queries as exfiltration channels | RESIDUAL | Documented in SECURITY.md "Known limits" | – | RESIDUAL |

Test run evidence for PR 1:

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

## Negative results: what already held

PR 1 recorded these protections as already in place. Nothing was changed for them.

- **Tool-result boundary framing (C1).** With the real harness and the real WebFetch
  tool against a loopback lab target, payloads in the body, an HTML comment, the
  `X-Powered-By` header, a JSON API response and a forged
  `<<<END_UNTRUSTED_OUTPUT …>>>` marker each arrive inside exactly one frame, and the
  forged marker is neutralised (`TestAdversarialLabBoundaryFramesAllPayloads`). The
  boundary is on unless `ARTEX_UNTRUSTED_BOUNDARY` is falsy (`agent/norma_trust.go`).
- **Provider redirect credential handling (C10).** Covered by the existing
  `TestProviderRedirectRefusesCredentialForwarding` (`agent/provider_redirect_test.go`).
- **WebSearch redirect credential handling (C10).** Covered by the existing
  `TestSearchClientRefusesCredentialRedirect`
  (`third_party/norma/tool/websearch_redirect_test.go`).
- **RunInfo reaches the tool context in all roles.** For planner, worker, main agent
  and chat, the run identity a tool call receives is the one the role attached. This
  is what the policy uses to select the rules (`TestRunInfoReachesToolContext`).

## Notes on PR 1 behaviour

- **Chat sessions.** Chat sessions have no task id, so only the global block rules and
  the denylist apply. There are no task allow rules in chat. As a result, a custom
  http tool whose URL template has a placeholder in the host and that carries
  operator-set headers is **always refused** in chat with
  `TEMPLATED_ORIGIN_NOT_ALLOWED`. The same template without operator headers runs
  under the destination checks alone.
- **Refusal codes.** `TARGET_BLOCKED`, `TARGET_NOT_ALLOWED`, `TARGET_CONTROL_PLANE`,
  `TARGET_POLICY_UNAVAILABLE`, `TARGET_UNRESOLVED`, `TARGET_INVALID` (WebFetch and the custom http tool),
  and `ORIGIN_MISMATCH`, `TEMPLATED_ORIGIN_NOT_ALLOWED`, `REDIRECT_BODY_REFUSED`
  (custom http tool only). The matcher semantics are in
  [adr/0001-webfetch-destination-policy.md](adr/0001-webfetch-destination-policy.md).
- **Log lines.** These lines carry no URL, host, body or header values:
  - `[webfetch-policy] decision=<code> hop=<n> task=<id>`: every policy refusal, from
    WebFetch and from the custom http tool.
  - `[customtool-http] decision=<code> hop=<n> task=<id>`: every custom http tool
    refusal. A refusal by the shared policy therefore logs both lines.
  - `[webfetch] direct_retry task=<id>`: once per fetch that dials directly while a
    proxy is configured.
  - `[webfetch-policy] db_host_unresolved task=<id>`: a DSN host did not resolve
    when the denylist was built. Its name on its port and the local-address pairs
    stay denied.
  - `[assets] gate_unavailable task=<id>`: `insert_assets` refused because the rules
    could not be read.

- **Canonical destination.** Both tools send the canonical ASCII host and numeric port
  they checked, on every hop (`db.CanonicalEgressHost`, `canonicalizeURL` in
  `agent/webfetch_policy.go`). `TARGET_INVALID` is logged as
  `[webfetch-policy] decision=TARGET_INVALID hop=<n> task=<id>`.
- **Database endpoints.** `agent.DatabaseEndpoint` parses the DSN with
  `pgconn.ParseConfig` and returns every (host, port) pair; for remote hosts the
  denial is per pair.

## Notes on PR 2 behaviour

- **Opt-out.** `ARTEX_DATA_ENVELOPE=0` (or `false`, `off`, `no`) removes the frames, the
  nonce and the goal anchor, and hints carry no `origin` key. The code-owned rules (C2)
  and the fact labels (C4) stay.
- **Log lines.** Neither carries data, the nonce or the refused value:
  - `[envelope] neutralised=<n> surface=<planner|worker|compactor>`: forged marker stems
    were rewritten in one message.
  - `[facts] record_refused reason=invalid_source`: `record_fact` refused a `source`.
- **Test evidence for PR 2** is recorded in [phase-d1-plan.md](phase-d1-plan.md), §10b.
- **Decision record:** [adr/0002-data-envelope-and-code-owned-rules.md](adr/0002-data-envelope-and-code-owned-rules.md).

## Residuals

1. **Proxied requests.** Through any proxy (the default in-process recording proxy, a
   per-tool proxy, a global or environment proxy), address checks happen only at check
   time. The proxy resolves the name again, so DNS rebinding and IP allow-rule pinning
   are not enforced there. Through a global or environment proxy, a name that does not
   resolve locally gets name checks only.
2. **Coverage.** The policy covers WebFetch and the custom http tool only. Bash and
   shell commands (bypass mode, proxy environment set), custom tools of kind `command`
   and `script`, MCP tools and WebSearch queries can reach any destination.
3. **Lab targets.** Loopback and private destinations other than ARTEX's own three
   ports are allowed by design.
4. **No live-model evaluation.** See the next section.
5. **Prompt surfaces (PR 2).** The envelope, the code-owned rules and the goal anchor
   are advisory. They mark data and restate the goal; they do not prevent injection.
   - Hints with origin `human` include text the main agent wrote (C14).
   - Intent summaries and `stagnation_warning.directions` are not framed (C15).
   - Hint triggers carry no origin.
   - A resumed worker draws a new nonce, so frames from its earlier launch in the
     session history carry the old nonce.
   - The declared fact `source` is chosen by the model and can be wrong or spoofed.
     `prove_goal` ignores it (refusal deferred to the goal-proof iteration).
   - The finding detail in a planning trigger is not length-capped.

Also residual: a custom http tool with a templated host sends its operator-set headers
to any host that an enabled task allow rule matches. Those hosts are targets, and a
broad allow rule shares the headers with many of them.

Behaviour change to note: the built-in global block rules (`.gov`, `.gov.cn`, `.edu`,
`.edu.cn`, enabled by default) now apply to WebFetch and the custom http tool in every
role.

## What these tests can and cannot prove

They prove structure on real code paths:
- framing of tool results, and of derived text in the planner, worker and compressor
  messages, with forged markers neutralised;
- the presence and position of the code-owned rules and the goal anchor;
- the `record_fact` source enum, its refusals and the display of `origin` and `source`;
- server-side refusals, with the refusal code and no request reaching the target;
- the network policy on the initial request, on every redirect hop and on every
  direct dial, including a rebinding resolver;
- that each role offers WebFetch once and turns a panic in the wrapper into a tool
  error.

Model responses are scripted provider fixtures. Network tests use loopback `httptest`
targets with an injected resolver and interface list.

They do **not** prove:
- that a model ignores injected instructions;
- anything about Bash, shell, MCP, `command`/`script` custom tools or WebSearch egress;
- what a remote or environment proxy resolves a name to, or whether it is rebound
  there;
- behaviour on platforms other than linux/amd64, which are UNKNOWN.
