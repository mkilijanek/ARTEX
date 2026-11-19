# ADR 0001: Destination policy for WebFetch and the custom http tool

- **Status:** Accepted (Phase D.1, PR 1, branch `phase-d/untrusted-boundary-audit`)
- **Date:** 2026-11-17
- **Issues:** #23 (Phase D.1), #7 (agentic attack-class audit)
- **Plan:** [phase-d1-plan.md](../phase-d1-plan.md), design D1, D2 and D6

## Context

Before this change, WebFetch could reach any destination in every role. The SDK
permission prompt (`permission.AskUser`) is bypassed because every ARTEX session runs
in `ModeBypass`. The only checks were the URL scheme and a static-asset suffix. No
redirect or dial hook was installed, so a target could redirect WebFetch to cloud
metadata (`169.254.169.254`), to ARTEX's own API or recording proxy, or to the
database.

The custom http tool rendered `{param}` values into the URL without escaping. A
template such as `https://api.vendor.com{path}` with `path=@attacker.net/x` sent the
operator's headers to `attacker.net` on the first request. Its client had no redirect
check and no pinned dial.

There were two candidate rule sources:
- `task_scope`: agents can widen it themselves, so it cannot restrict them.
- The asset intercept rules (`EvaluateAssetGate`): operators already maintain them,
  but their fuzzy kinds are raw substring matches that allow on any candidate string,
  so `http://allowed.example@attacker.net/` would satisfy an allow rule.

## Decision

1. **Rules.** The destination policy uses the operator's asset intercept rules: global
   rules (block only) plus the task's block and allow rules. Chat sessions (task id 0)
   get the global block rules only. `task_scope` is not used.
2. **Own matcher.** `db.EvaluateEgress` (`db/egress_match.go`) is a new URL-aware
   matcher. `EvaluateAssetGate` and `insert_assets` keep their existing semantics.
3. **Built-in denylist.** Cloud metadata and link-local ranges, ARTEX's own listen,
   recording-proxy and every database port on local addresses, and each DSN database
   host (name and resolved addresses) on that host's own port are always refused
   (`TARGET_CONTROL_PLANE`). The database endpoints are every (host, port) pair that
   `pgconn.ParseConfig`, the parser `db.Open` connects with, returns for the DSN,
   fallback hosts included. They are captured at startup, before the environment
   scrub, and kept on re-registration. A Unix-socket DSN keeps its port denied on
   local addresses; an unset DSN protects nothing. Other loopback and
   private destinations stay reachable for lab targets.
4. **Host-side hooks, not an SDK patch.** `agent/webfetch_policy.go` replaces the
   SDK-appended WebFetch with the same SDK tool, built with ARTEX's `CheckRedirect` and
   `DialContext` and wrapped by an initial-URL check and `guardPanic`. Name, schema and
   description are the SDK's. `third_party/norma` is not changed.
5. **Unconditional.** There is no flag. A policy that cannot be evaluated refuses the
   request (`TARGET_POLICY_UNAVAILABLE`).
6. **Pinned direct dials.** The checked addresses travel in the request context. A
   direct dial uses exactly those addresses. The only addresses dialled without a check
   are the configured proxy and the environment proxy for that specific request.
7. **Direct retry.** WebFetch's direct (no-proxy) retry is disabled whenever a global
   egress proxy is configured. With capture on and no global proxy it stays, and is
   logged as `[webfetch] direct_retry task=<id>`.
8. **Custom http tool.** It uses the same guard (`agent.TargetGuard`), plus origin
   pinning, a task-allow-rule requirement for templated hosts with operator headers,
   sticky header stripping after a cross-origin hop, and refusal of cross-origin
   307/308 body replays (`server/customtool.go`).

### Order of checks

For the initial URL and each redirect hop (at most 10):
0. Canonicalise the host once (`db.CanonicalEgressHost`) and the port, and rewrite the
   URL to that form: the request is sent with the canonical ASCII host and a numeric
   port, so the string checked is the string sent. An invalid host, an invalid port, or
   a scheme other than `http`/`https`, with or without a port: `TARGET_INVALID`.
1. Resolve the normalised name locally (IP literals are used as they are). If it does
   not resolve and no global or environment proxy is set: `TARGET_UNRESOLVED`. With
   such a proxy, only the name checks below apply.
2. Denylist on the name (database host) and every resolved address, in every embedded
   IPv4 form: `TARGET_CONTROL_PLANE`.
3. Load the rules once per call. Failure or no loader: `TARGET_POLICY_UNAVAILABLE`.
4. Block rules on host, URL and every resolved address: `TARGET_BLOCKED`.
5. Allow rules, only when at least one is enabled. A host, URL or IP rule is enough:
   otherwise `TARGET_NOT_ALLOWED`.

A task session that has an exploration id but no task id is refused before step 1.

### Matcher semantics (`db.EvaluateEgress`)

Hosts are canonicalised by `db.CanonicalEgressHost`: whitespace trimmed, IDNA to ASCII
for a non-ASCII host, lower case, trailing dots dropped after the mapping (so U+3002
counts as a dot). IP literals are canonicalised (IPv4-mapped unmapped, zone dropped). A
host whose last label is numeric is read as IPv4 with inet_aton/WHATWG rules
(`2130706433`, `0x7f.1`, `127.1`, `0177.0.0.1` and their full-width forms are
127.0.0.1) and is checked and dialled as the dotted address; if it does not parse
(`foo.123`) the destination is refused (`TARGET_INVALID`). Domain patterns and the host
of URL patterns go through the same function, so an `exact_domain` pattern `127.1`
means 127.0.0.1. `exact_ip` and `cidr` patterns are parsed as standard addresses only
(`127.1` there matches nothing), and `fuzzy_ip` stays a text prefix.

Ports are compared as numbers (URL rules, `host:port` block patterns, the 80/443
defaults). A rule with an invalid port matches nothing.

| Rule kind | Matches |
|---|---|
| `exact_domain` | The normalised host equals the normalised pattern. |
| `fuzzy_domain` | The host equals the pattern or ends with `.` + pattern (label boundary). A leading `.` in the pattern is ignored. |
| `exact_ip` | The host literal or a resolved address equals the pattern. |
| `fuzzy_ip` | Octet or group boundary prefix: `10.1` matches `10.1.x.x`, not `10.10.x.x`. |
| `cidr` | The prefix contains the host literal or a resolved address. |
| `exact_url`, `fuzzy_url` with `://` | Same scheme, same normalised host, same port (80/443 implied), and the path equals the rule path or continues it after a `/`. Rule path `/` matches the whole origin. Dot segments and doubled slashes are resolved first. Userinfo, query and fragment are ignored. Block rules compare paths case-insensitively, allow rules case-sensitively. |
| `exact_url` without `://` (block only) | Pattern starts with `/`: a case-insensitive **string** prefix of the cleaned path, so `/admin` also blocks `/administrator`. Pattern starts with a host (`evil.com/admin`): the host must match on a label boundary (and the port, if given), and the rest is a string prefix of the path. |
| `fuzzy_url` without `://` (block only) | As above, but a case-insensitive substring of the cleaned path plus `?` plus query. |

Further rules:
- URL-type allow rules without `://` never match, so a malformed allow rule cannot
  widen what is reachable.
- A URL-type block rule without `://` and without `/` that looks like a host (it
  contains a `.` or a `:` and none of `= ? & #` or a space, for example `evil.com`,
  `evil.com:8443` or `[::1]:8080`) blocks that host on a label boundary, with the port
  if one is given. The path reading still applies as well: `exact_url` `evil.com` also
  blocks the path prefix `/evil.com`, and `fuzzy_url` `evil.com` also blocks URLs whose
  path or query contains `evil.com`. A pattern such as `secret` or `token=` is only a
  path or query fragment. `exact_domain` and `fuzzy_domain` remain the clearest way to
  block a host.
- If a host-first pattern names another host, the whole pattern is tested as a path.
- Block rules test each address in all embedded IPv4 forms. Allow rules test the
  address itself.
- When only IP or CIDR allow rules matched, only the addresses they name may be
  dialled.

## Alternatives rejected

- **`task_scope` as the rule source.** Agents can write to it.
- **`EvaluateAssetGate` as the matcher.** Substring matching allows bypasses through
  userinfo, query text and label suffixes.
- **A PreToolUse guard hook.** Planner and main-agent sessions have no guard hooks
  (deferred to a D.1 follow-up).
- **An SDK patch.** Not needed: the SDK already exposes `CheckRedirect`, `DialContext`
  and `DisableDirectRetry`.
- **Blocking all loopback and private addresses.** This breaks lab targets and the
  proxy dial.
- **Escaping custom http parameters.** This breaks path and query templates.
- **Silently stripping operator headers for templated hosts.** This produces
  confusing authentication failures. The tool refuses the request instead.

## Consequences

- WebFetch now appears at the end of the host tool list instead of being appended by
  the SDK. This is a one-time change in tool order (see
  [token-audit.md](../token-audit.md)).
- Fetches through a global proxy no longer fall back to a direct connection, so a
  broken proxy makes the fetch fail rather than leak the source IP.
- WebFetch and the custom http tool fail when the rule store cannot be read.
- The global block rules seeded by `db/db.go` (`.gov`, `.gov.cn`, `.edu`, `.edu.cn`,
  enabled by default) now apply to WebFetch and the custom http tool in every role,
  chat included. A default install therefore refuses fetches of those domains with
  `TARGET_BLOCKED` until an operator disables the rules.
- A task allow list made only of schemeless URL-type rules refuses every fetch, because
  such rules never allow.
- With a templated host, a custom http tool sends its operator-set headers to any host
  that an enabled task allow rule matches; those hosts are targets.
- In chat sessions, a custom http tool with a templated host and operator headers is
  always refused.
- A lab target on a database host's address and that host's port cannot be reached.
  For remote database hosts the denial is per (host, port) pair; the same address on
  another port stays reachable.
- A model that sends a Unicode or compatibility spelling of a host gets the canonical
  ASCII URL fetched, not the spelling it sent.
- When the global proxy changes, the policy is registered again. Agents pick up the
  new direct-retry setting when they are next built.

### Residuals (not addressed by this decision)

1. Through any proxy (the default in-process recording proxy, a per-tool proxy, a
   global or environment proxy), address checks happen only at check time. The proxy
   resolves the name again, so DNS rebinding and IP allow-rule pinning are not
   enforced there. Through a global or environment proxy, a name that does not resolve
   locally gets name checks only.
2. The policy covers WebFetch and the custom http tool only. Bash and shell commands
   (bypass mode, proxy environment set), custom tools of kind `command` and `script`,
   MCP tools and WebSearch queries can reach any destination. The shell and
   recording-proxy gate is planned as a D.1 follow-up (plan D7, decision Q1).
3. Loopback and private destinations other than ARTEX's own three ports are allowed by
   design.

## Related

- [SECURITY.md](../../SECURITY.md), "Destination checks for WebFetch and the custom
  http tool" and "Known limits"
- [agentic-attack-audit.md](../agentic-attack-audit.md), classes C8 to C11
- [upstream-port-matrix.md](../upstream-port-matrix.md), `f6b29b8` / `1c3a5f4`
