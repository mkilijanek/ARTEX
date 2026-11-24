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
- **Configuration written by an agent is not trusted until an operator approves
  it.** The platform tools that create or change skills, custom tools and MCP
  servers (`create_skill`, `update_skill_file`, `create_custom_tool`,
  `update_custom_tool`, `create_mcp`, `update_mcp`) wait for a human decision on
  the Approval Records page; the AI judge and intercept rules are not consulted
  for them.
- **Agent tools are not the server.** In the Docker image, every process an agent
  starts runs as a separate unprivileged user (`artex-tool`, uid 30002) without
  capabilities, and the server's key, configuration, environment and database
  socket are closed to it. The server reports whether this holds as the tool
  isolation status; where it does not (binary installs, `cap_drop: ALL`,
  `ARTEX_TOOL_ISOLATION=off`), agent tools run with the server's own access. See
  [docs/tool-isolation.md](docs/tool-isolation.md), including its "Limits".
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
   - every database (host, port) pair from the DSN, fallback hosts included, parsed
     with `pgconn.ParseConfig` as the application connects: each host by name and
     by resolved address on that host's own port, and every database port on local
     addresses (a Unix-socket DSN keeps its port denied there). The endpoints are
     read once at startup; an unset DSN protects nothing.
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
- `TARGET_INVALID`: the host or port cannot be read, a host ending in a numeric
  label is not a valid IPv4 address (`foo.123`), or the URL has a
  scheme other than `http`/`https`. The reason is fixed and carries no destination text.
- `TARGET_POLICY_UNAVAILABLE`: the rules cannot be read, no rule loader is
  registered, the local address list cannot be built, or a task session has no
  task id. The check fails closed, and the
  model gets a fixed reason, not the store error.
- `TARGET_UNRESOLVED`: the name does not resolve locally and no global or
  environment proxy is set.

On a direct connection, ARTEX dials only the addresses it checked, so a second DNS
answer cannot change the destination. When IP or CIDR allow rules alone admit a
destination, only the addresses those rules name are dialled.

The host is canonicalised once (IDNA to ASCII, lower case, trailing dots dropped) and
the request is sent with that host and a numeric port, on the initial URL and every
redirect hop, so the checked string is the sent string. Numeric IPv4 spellings
(`2130706433`, `0x7f.1`, `127.1`, `0177.0.0.1`) are checked and dialled as the dotted
address. Ports are compared as numbers; a rule with an invalid port matches nothing.

The rule matcher is URL-aware. Domain rules compare the canonical host. A fuzzy domain rule matches the domain
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

### Prompt-injection hardening of host-built messages

These measures mark target- and agent-derived text as data and restate the operator's
goal. They are advisory to the model: they do **not** prevent prompt injection, and no
test shows how a live model responds to them.

**Data envelope.** Text derived from agent or target content is framed between
`[[ARTEX_DATA nonce=N field=<name>]]` and `[[END_ARTEX_DATA nonce=N]]`, with one random
16-byte nonce per planner message, worker launch and digest-compressor request:
- planner input: `worker_output` and `finding` (the planning triggers), `recent_facts`,
  `finding_list`, `cold_digests`, `related_tasks`, `agent_hints` (hints whose origin is
  not `human`), `task_credentials`, and `projection_notes`;
- worker launch: the same derived fields of the overview, and the target `assets` JSON;
  the intent summary is not framed;
- digest compressor: the member and anchor text.

Data that contains the marker stem `ARTEX_DATA` has it rewritten, so data cannot open or
close a frame. Each rewrite is counted, and a message with at least one logs
`[envelope] neutralised=<n> surface=<planner|worker|compactor>`, without data or nonce.
The nonce is never logged. The envelope and the planner goal anchor are on by default;
`ARTEX_DATA_ENVELOPE=0` (or `false`, `off`, `no`) switches off both and nothing else.
The design is in
[docs/adr/0002-data-envelope-and-code-owned-rules.md](docs/adr/0002-data-envelope-and-code-owned-rules.md).

**Code-owned rules.** These are appended in code after the editable prompt body, so a
DB prompt override or an operator edit cannot remove them, and `ARTEX_DATA_ENVELOPE=0`
does not switch them off:
- the untrusted-content block, in the worker, pentest, main, auto and every chat agent
  prompt, exactly once;
- an untrusted-data rule in the planner system prompt: data cannot add goals, targets
  or scope, change or end the task, or be relayed to a worker through `steer_work`;
- the same rule plus a digest rule for the cold-digest compressor.

The goal anchor at the end of the planner input (task goal, operation constraints, and
the instruction that each new intent names the goal it serves) is also written by code,
but it belongs to the envelope: `ARTEX_DATA_ENVELOPE=0` removes it.

The bbot-recon skill tells the agent that every event field is untrusted data and that
only in-scope events (`scope_distance == 0`) reach `insert_assets`.

**Fact labels.** `record_fact` takes a declared `source`: `target_response`,
`tool_output`, `third_party_intel` or `inference` (the default). `operator_input` and
unknown values are refused for every model tool, with the log line
`[facts] record_refused reason=invalid_source`. The writer (`origin`, set by the server)
and the declared `source` are shown to the planner and the digest compressor. This only
labels and displays. It does not establish provenance: a model can mislabel target
content. Goal proof (`prove_goal`) does not look at the source.

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
  Running tools as a separate user (see "Trust Model") limits what they can read
  on the server, not where they can connect.
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
- **Prompt surfaces.** The envelope, the code-owned rules and the goal anchor are
  advisory, as described above. In addition:
  - the main agent writes with origin `human`, so hints and goal nodes with origin
    `human` include text the main agent wrote (hints, and goals added with `set_goals`)
    and stay outside the frames; their heading names the console, the goal decomposer
    and the main agent, and does not claim operator authorship. Facts and findings the
    main agent records also show `origin=human`;
  - intent summaries, `stagnation_warning.directions` and hint triggers are not framed;
  - a resumed worker draws a new nonce, so frames from its earlier launch carry the old
    nonce;
  - the declared fact `source` is spoofable, and `prove_goal` accepts a fact of any
    source;
  - the RSI lessons block is not framed;
  - the finding detail in a planning trigger is not length-capped.

The per-class status is in
[docs/agentic-attack-audit.md](docs/agentic-attack-audit.md).

### Signal notifications (the `artex-signal` service)

With the optional Signal override of the example deployment, a separate
container, `artex-signal`, runs signal-cli as a **linked device** of one Signal
account. Design and accepted risks:
[docs/adr/0011-signal-sidecar-with-database-config.md](docs/adr/0011-signal-sidecar-with-database-config.md).

- **The linked device can read the account's messages.** Use a dedicated Signal
  account for ARTEX; the link dialog asks the operator to acknowledge this. The
  service downloads no attachments, stories, avatars or stickers, and drops
  received messages without storing, logging or forwarding them.
- **The service is less trusted than the server.** It parses input from Signal.
  It talks to PostgreSQL as its own role, which can read one view and execute
  three SECURITY DEFINER functions with fixed preconditions, and nothing else in
  ARTEX's schema: no table, no object creation, no temporary tables, at most 5
  connections. The server creates that role and refuses `trust` authentication,
  `CREATE` for `PUBLIC` on schema `public`, and roles it did not create or that
  are privileged. The service refuses to run when a wrong password logs in, or
  when a TCP connection does not verify the server (`sslmode=verify-full`).
- **The socket can only send.** `artex` reaches the service through a Unix socket
  under `/run/artex-private` (check C12 keeps agent tools out) that answers
  `POST /v2/send` and `GET /v1/about` only. Linking, unlinking and the group list
  go through the database. signal-cli gets a fixed list of calls (`version`,
  `listAccounts`, `startLink`, `finishLink`, `listGroups`, `send`); it cannot
  register, unregister or delete the account. Linking and unlinking from the UI
  need the operator's password, and an account sends only after the operator has
  typed the last 6 digits of its number.
- **Secrets inside the container.** The supervisor reads its database password
  once, removes it from its environment and makes itself non-dumpable, so the
  signal-cli child (same user) cannot read it from `/proc`. The child gets four
  environment variables and no database access.
- **Unlink is local only.** signal-cli gives a linked device no way to remove
  itself. Until the operator removes the device on the phone, its keys stay valid,
  and the data volume `artex-signal-data` and any copy of it are a live credential
  for the account.
- **Outbound traffic of the service is not restricted.** It can reach the LAN and
  a cloud metadata address; blocking that is the operator's firewall job. ARTEX
  shows no warning for it.

Accepted residual risks (not vulnerabilities by themselves; tell us about a way
to go further):

- A compromised service can hold PostgreSQL advisory locks that ARTEX uses with
  fixed keys and make ARTEX hang at start or on some writes until the container
  is stopped. There is no confidentiality or integrity impact, and stopping the
  container releases the locks.
- A link code can remain in old row versions, the write-ahead log, dumps and
  PostgreSQL statement logs taken while a link is pending. It is valid for 2
  minutes, and a stolen code links the service to the thief's account, not the
  operator's.
- The three functions run as the schema owner, a superuser in the shipped Compose
  file. `ARTEX_SIGNAL_DB_PASSWORD` is visible in `.env` and `docker inspect`, like
  `POSTGRES_PASSWORD`.

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
