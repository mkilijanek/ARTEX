# Agentic attack-class audit (#7)

Issue #7 mirrors upstream BaaRai/ARTEX#124. This report records, for each attack
class, what protected ARTEX before Phase D.1, what changed, which test proves it and
the status now. The classes and the "before" verdicts come from §2 of
[phase-d1-plan.md](phase-d1-plan.md), verified against the code at `43208e5`.

This revision covers **PR 1 (network)** on branch `phase-d/untrusted-boundary-audit`.
PR 2 (prompt envelope and fact labels) will update the rows marked PENDING PR 2.

## Status values

- **HELD**: a protection already existed. A test now records that it holds.
- **FIXED in PR 1**: the gap is closed for the scope named in the row.
- **PENDING PR 2**: planned for PR 2 and not built yet.
- **DEFERRED**: moved out of D.1 (goal-proof iteration or D.2).
- **RESIDUAL**: not fixed in D.1. Named here and in [SECURITY.md](../SECURITY.md).
- **UNKNOWN**: cannot be verified from the code.

## Results

| # | Class | Before | What PR 1 changed | Tests | Status after PR 1 |
|---|---|---|---|---|---|
| C1 | Untrusted boundary on tool results | HELD | Nothing. WebFetch is now wrapped by the destination policy, and its results still pass through the same boundary framing. | T-LAB-1 `TestAdversarialLabBoundaryFramesAllPayloads` | HELD |
| C1b | bbot JSONL and other tool-ingested files | HELD (framing) | Nothing | T-LAB-2 (PR 2) | PENDING PR 2 (skill note and test) |
| C2 | Code-owned untrusted rules | PARTIAL | Nothing | T-PROMPT-1 (PR 2) | PENDING PR 2 |
| C3 | Envelope on host-built messages | GAP | Nothing | T-PLAN-1, T-WORKER-1 (PR 2) | PENDING PR 2 |
| C4 | Memory poisoning (facts) | GAP | Nothing | T-FACT-1/2 (PR 2) | PENDING PR 2 (labels). `prove_goal` refusal: DEFERRED (goal-proof iteration) |
| C5 | Plan hijacking | GAP | Nothing | T-PLAN-1 (PR 2) | PENDING PR 2 |
| C6 | Goal drift | PARTIAL | Nothing | T-PLAN-3 (PR 2) | PENDING PR 2 (anchor). `goal_met` without evidence: RESIDUAL (Q6) |
| C7 | Tool abuse | PARTIAL | `insert_assets` fails closed: if the asset intercept rules cannot be read, the whole call is refused and nothing is inserted (`agent/tools_insert.go`). | T-INS-1 `TestInsertAssetsGateFailsClosed` | FIXED in PR 1 (`insert_assets`). Guard hooks on planner and main sessions: DEFERRED (D.2, Q4). `task_scope` is still not enforced. |
| C8 | WebFetch SSRF and scope | GAP | Destination policy for WebFetch in every role, and for the custom http tool: control-plane denylist, operator block and allow rules, URL-aware matcher, pinned direct dials, fail closed (`agent/webfetch_policy.go`, `db/egress_match.go`, `server/target_policy.go`). | T-MATCH-1 `TestEgressMatchParsedHostOnly`; `TestEgressAllowedIPsFollowTheMatchingRule`; T-WF-1 `TestWebFetchPolicyBlockRule`; T-WF-3 `TestWebFetchPolicyAllowList`; T-WF-4 `TestWebFetchPolicyControlPlaneAndMetadata`; T-WF-5 `TestWebFetchPolicyFailsClosed`; T-WF-9 `TestWebFetchDialPinnedAgainstRebinding` (with its re-executed helper `TestWebFetchEnvProxyChild`, which skips when run on its own); `TestWebFetchResolvesNormalisedNameOnEveryPath`; `TestWebFetchIPAllowRuleDialsOnlyMatchingAddress`; `TestWebFetchPolicyUnavailableHidesStoreError`; `TestWebFetchToolSetPerRole`; `TestRunInfoReachesToolContext`; `TestTargetPolicyKeepsDBEndpointOnReRegistration` | FIXED in PR 1 for WebFetch and the custom http tool. Proxied paths: RESIDUAL (see below). |
| C9 | Redirects | GAP | Every hop passes the same policy. The 10-hop cap is re-implemented in `CheckRedirect`. | T-WF-2 `TestWebFetchPolicyRedirectHopBlocked`; T-WF-8 `TestWebFetchRedirectHopCap` | FIXED in PR 1 |
| C10 | Credential leakage | GAP (custom http tool); HELD (provider, WebSearch, WebFetch) | Custom http tool: origin pinning (`ORIGIN_MISMATCH`); templated host with operator headers needs a task allow rule (`TEMPLATED_ORIGIN_NOT_ALLOWED`); sticky stripping of operator headers, `Authorization` and `Cookie` after a cross-origin hop; cross-origin 307/308 body replay refused (`REDIRECT_BODY_REFUSED`); pinned dial; keep-alives off; content-free transport errors. | T-CRED-1 `TestCustomHTTPOriginPinned`; T-CRED-2 `TestCustomHTTPCrossOriginStripsHeaders` (its https-to-http case runs in the re-executed helper `TestCustomHTTPSchemeChild`, which skips when run on its own); T-CRED-3 `TestCustomHTTP307BodyRefused`; T-CRED-4 `TestCustomHTTPInitialURLPolicy`; T-CRED-5 `TestCustomHTTPTemplatedAuthorityHeaders`; `TestCustomHTTPChainStaysStripped`; `TestCustomHTTPOddPlaceholderNames`; `TestCustomHTTPErrorTextIsContentFree`; `TestCustomHTTPRecordingProxyPath` | FIXED in PR 1 (custom http tool). Provider and WebSearch redirects: HELD (existing tests, see below). |
| C11 | Direct retry bypasses proxies | GAP | WebFetch's direct retry is off whenever a global egress proxy is configured, in either capture mode. With capture on and no global proxy, it stays and is logged. | T-WF-6 `TestWebFetchNoDirectRetryWithGlobalProxy`; T-WF-7 `TestWebFetchDirectRetryLogged` | FIXED in PR 1. The retry with capture on and no global proxy is kept by decision (Q3). |
| C12 | Cold-digest laundering | GAP | Nothing | T-DIGEST-1 (PR 2) | PENDING PR 2 |
| C13 | NOA summaries | UNKNOWN | Nothing | audit (PR 2) | PENDING PR 2 (still UNKNOWN) |
| C14 | Main agent treated as human | RESIDUAL | Nothing | – | RESIDUAL (D.2, Q5) |
| C15 | Planner intent summary in the worker system prompt, unframed | RESIDUAL | Nothing | – | RESIDUAL |
| C16 | RSI lessons (cross-task poisoning) | PARTIAL / RESIDUAL | Nothing | – | RESIDUAL |
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
  `TARGET_POLICY_UNAVAILABLE`, `TARGET_UNRESOLVED` (WebFetch and the custom http tool),
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
  - `[webfetch-policy] db_host_unresolved task=<id>`: the DSN host did not resolve
    when the denylist was built. The name and the local-address pair stay denied.
  - `[assets] gate_unavailable task=<id>`: `insert_assets` refused because the rules
    could not be read.

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
5. **Prompt surfaces.** The data envelope, code-owned rules, goal anchor and fact
   provenance (C1b–C6, C12, C13) are pending PR 2.

Also residual: a custom http tool with a templated host sends its operator-set headers
to any host that an enabled task allow rule matches. Those hosts are targets, and a
broad allow rule shares the headers with many of them.

Behaviour change to note: the built-in global block rules (`.gov`, `.gov.cn`, `.edu`,
`.edu.cn`, enabled by default) now apply to WebFetch and the custom http tool in every
role.

## What these tests can and cannot prove

They prove structure on real code paths:
- framing of tool results;
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
