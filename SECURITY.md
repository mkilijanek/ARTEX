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

## In Scope

Examples of issues we want to hear about:

- Authentication or authorization bypass in the ARTEX server or web UI.
- Target-controlled content that causes command execution, file access outside
  the working directories, scope escape or prompt-driven privilege escalation.
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
