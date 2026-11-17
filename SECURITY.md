# Security Policy

This repository is a fork of [BaaRai/ARTEX](https://github.com/BaaRai/ARTEX), an
autonomous penetration-testing platform. This policy covers the fork's own code
and releases.

## Supported Versions

Security fixes are provided for the latest fork release (tags of the form
`vMAJOR.MINOR.PATCHm`) and for the `main` branch. Older releases do not receive
fixes; please upgrade before reporting.

If the issue also exists in the upstream project, it should be reported there as
well, through the upstream repository's own security policy.

## Reporting a Vulnerability

**Do not open a public issue or pull request for a security vulnerability.**

Report privately through GitHub: **Security** tab → **Report a vulnerability** in
this repository. This is the only reporting channel.

Please include:

- A description of the vulnerability and its impact.
- The ARTEX version or commit hash.
- The runtime environment (OS, Docker or local, PostgreSQL version, LLM provider).
- Minimal reproduction steps or a proof of concept.
- Logs, screenshots or sample traffic with sensitive data removed.
- A suggested fix, if you have one.

Do not send real third-party data, credentials, tokens or unredacted traffic.

## Authorized Testing

ARTEX is an offensive-security tool. Test only against your own ARTEX deployment
in a lab or local environment, systems you own, or targets you are explicitly
authorized in writing to test. Reports based on unauthorized testing of
third-party systems may be closed without action.

## Trust Model

Knowing what ARTEX treats as trusted helps decide whether something is a
vulnerability.

- **The operator is trusted.** An authenticated operator can run tools and
  commands on the host by design. ARTEX is intended to run on an isolated host or
  network, not to be exposed to untrusted users.
- **Operator configuration is trusted.** LLM provider URLs, MCP server URLs and
  commands, custom tools, proxies and skill directories are set by the operator.
  Requests to those addresses and the start of those commands are intended
  behaviour.
- **Targets are untrusted.** Content returned by a target, including pages, API
  responses, recorded traffic and anything an agent reads, must never gain
  operator privileges, escape the configured scope, or alter stored evidence. A
  way for target-controlled data to do so is a vulnerability.
- **TLS verification toward targets is relaxed on purpose.** Probes and the
  recording proxy accept self-signed certificates, because test targets commonly
  use them. This does not apply to authentication of the operator to ARTEX.

### Destination checks for WebFetch and the custom http tool

These checks apply to **WebFetch and the custom http tool only**. Other ways an
agent can reach the network are listed under "Known limits" below.

WebFetch is checked in every agent role (planner, worker, main agent, chat). It is
always on; there is no flag to turn it off. The tool's name, schema and description
are unchanged. Before any connection, the initial URL and every redirect hop (at most
10) are checked in this order:

1. **Built-in control-plane denylist** (`TARGET_CONTROL_PLANE`):
   - cloud metadata and link-local ranges: `169.254.0.0/16`, `fe80::/10`,
     `fd00:ec2::254`, `100.100.100.200`, `168.63.129.16`;
   - ARTEX's listen port (`-addr`), the recording-proxy port (`-proxy`) and the
     database port, on local addresses: `127.0.0.0/8`, `::1`, `0.0.0.0/8`, `::` and
     every local interface address;
   - the database host from the DSN, by name and by resolved address, on the
     database port. The database endpoint is read once at startup.
   Addresses are also checked in their embedded IPv4 forms (IPv4-mapped, NAT64
   `64:ff9b::/96`, 6to4 `2002::/16`, IPv4-compatible `::a.b.c.d`).
2. **Operator block rules**, global and task (`TARGET_BLOCKED`). They come from the
   asset intercept rules.
3. **Task allow rules** (`TARGET_NOT_ALLOWED`). When a task has enabled allow rules,
   the destination must match one. Chat sessions have no task, so only the global
   block rules and the denylist apply there.

The built-in global block rules shipped with ARTEX (`.gov`, `.gov.cn`, `.edu`,
`.edu.cn`, enabled by default) are part of these checks, so WebFetch and the custom
http tool refuse those domains in every role until an operator disables the rules.

Other refusals:
- `TARGET_POLICY_UNAVAILABLE`: the rules cannot be read, no rule loader is
  registered, the local address list cannot be built, or a task session has no
  task id. The check fails closed, and the
  model gets a fixed reason, not the store error.
- `TARGET_UNRESOLVED`: the name does not resolve locally and no global or
  environment proxy is set.

On a direct connection, ARTEX dials only the addresses it checked, so a second DNS
answer cannot change the destination. When IP or CIDR allow rules alone admit a
destination, only the addresses those rules name are dialled.

The rule matcher is URL-aware. Domain rules compare the parsed host after lowercasing,
dropping a trailing dot and IDNA conversion. A fuzzy domain rule matches the domain
and its subdomains, on a label boundary. URL rules compare scheme, host, port and
path prefix after resolving dot segments. For domain rules and for URL rules written with a
scheme, text in userinfo, query or fragment does not satisfy a rule. A URL-type block
rule written without a scheme is read as a host or as a path and query fragment, and
such a rule never allows. The full semantics are in
[docs/adr/0001-webfetch-destination-policy.md](docs/adr/0001-webfetch-destination-policy.md).

The custom http tool passes the same checks, and adds the following:
- `ORIGIN_MISMATCH`: a template whose scheme, host and port are fixed text refuses
  any rendered URL with a different origin, for example a parameter value that
  injects `@other.host`.
- `TEMPLATED_ORIGIN_NOT_ALLOWED`: a template with a placeholder in its host that also
  carries operator-set headers runs only against a host that matches an enabled task
  allow rule. In chat sessions there are no task allow rules, so such a tool is
  **always refused**. Templates without operator headers run under the destination
  checks alone.
- Once a redirect chain leaves the original origin, the operator headers plus
  `Authorization` and `Cookie` are removed on that hop and on every later hop, also
  if the chain returns to the original origin.
- `REDIRECT_BODY_REFUSED`: a 307 or 308 redirect that would replay the request body
  to another origin is refused.
- Transport errors are returned as `request failed: <class>`, without the URL or
  host.

`insert_assets` fails closed: when the asset intercept rules cannot be read, the
whole call is refused and nothing is inserted.

The operator's guard intercept rules and judge still run first for worker and chat
sessions. The destination checks are a separate check inside the tool.

### Known limits

These are not fixed, and the destination checks above do not cover them:

- **Proxied requests.** Through any proxy (the default in-process recording proxy,
  a per-tool proxy, a global or environment proxy) the addresses are checked only
  when the check runs. The proxy resolves the name again, so DNS rebinding and
  IP allow-rule pinning are not enforced on that path. Through a global or
  environment proxy, a name that does not resolve locally gets name checks only.
- **Other egress channels.** Bash and shell commands (which run with the proxy
  environment set), custom tools of kind `command` and `script`, MCP tools and
  WebSearch queries can reach any destination. Against an injected model, the
  destination checks are therefore advisory while such tools are available.
- **Operator headers and templated hosts.** A custom http tool whose URL template
  takes the host from a model parameter sends its operator-set headers to any host
  that an enabled task allow rule matches. Those hosts are targets, so a broad allow
  rule (a fuzzy domain or a CIDR) shares the headers with many of them. Through a
  proxy, an IP or CIDR match holds only at check time.
- **Lab targets on loopback and private networks.** Loopback and private
  destinations other than ARTEX's own three ports are allowed by design.
- **Model behaviour.** The regression tests prove the structure and the policy on
  real code paths with scripted model responses. They do not prove that a model
  ignores injected instructions.
- **Prompt surfaces.** Framing of target-derived data in host-built messages,
  code-owned untrusted-content rules, a goal anchor and fact provenance labels are
  not delivered yet.

The per-class status is in
[docs/agentic-attack-audit.md](docs/agentic-attack-audit.md).

## In Scope

Examples of issues we want to hear about:

- Authentication or authorization bypass in the ARTEX server or web UI.
- Target-controlled content that causes command execution, file access outside
  the working directories, scope escape or prompt-driven privilege escalation.
- A way for WebFetch or the custom http tool to reach a destination the checks
  above refuse, other than through a listed known limit.
- Path traversal, archive extraction or deletion flaws that affect files outside
  the intended directory.
- Leakage of credentials, session tokens, API keys or recorded traffic.
- Tampering with findings, evidence or audit records.

## Out of Scope

- Actions available to an authenticated operator by design (see Trust Model).
- Attacks that require an operator to configure a malicious provider, MCP server,
  tool or skill.
- "ARTEX can be used to attack systems" when the user holds valid authorization;
  that is the purpose of the tool.
- Denial of service through large-scale traffic or resource exhaustion.
- Missing headers or hardening settings without a concrete security impact.
- Issues that exist only in third-party dependencies and do not affect ARTEX.
- Social engineering of maintainers or users.

## What to Expect

The fork is maintained on a best-effort basis; no response or fix time is
guaranteed. For an accepted report, coordination happens in a private GitHub
Security Advisory, the release containing the fix is identified there, and
reporters are credited if they wish. If a report is declined, the reason is given.
