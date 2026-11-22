# ADR 0006: Agent tools run as a separate user

- **Status:** Accepted, built (the release after 0.5.17m). Not built here: raw sockets for tools and one user per
  task (a separate tool runner, planned for the next release); a non-root server and an isolated setup for binary
  installs (later).
- **Date:** 2026-11-21
- **Runbook:** [../tool-isolation.md](../tool-isolation.md)

## Context

Agents run commands and read and write files through tools, and the model chooses the command lines and paths. The
design aim is least privilege: tools get what they need (the workspace, targets, the recording proxy) and nothing the
server holds for itself (the session signing key, the configuration file with provider keys, the database password
and the database).

Constraints:

- The vendored SDK (`third_party/norma`) is not edited. It starts shells with `exec.Command("bash", ...)`, resolved
  through the server's `PATH`, and runs the file tools inside the server process.
- Scans must still reach targets and the recording proxy.
- Binary installs, which run as a login user, must keep working.
- No schema change.

## Decision

1. **A dedicated user for every tool process.** uid/gid 30002 (`artex-tool`); 30001, 30003 and 30004 are reserved.
   A root-owned shim directory comes first in the server's `PATH`, so the SDK's `bash` goes through a fixed chain:
   `artex __astool` (root; clean environment from an allow-list; per-call variables in a root-only file, never on the
   command line) → `setpriv` (uid change, empty capability sets, `no_new_privs`) → `artex __toolenv` (applies the
   variables) → the program. ARTEX's own spawns (MCP stdio servers, script tools, the kill helper) use the same
   chain. The server stays root.
2. **File tools out of process.** `Read`, `Write`, `Edit`, `MultiEdit`, `LS`, `Glob` and `Grep` run in an
   `artex __toolfs` helper started through the chain. One guard wraps every session after its options are final and
   refuses tool lists that are not built this way.
3. **The server never follows a path a tool could have changed.** Workspace creation and hand-over, the workspace
   API, task archive packing and restore, uploads, `ask_operator` and skill loading open paths one component at a time
   through `os.Root`, without following links, and check the open descriptor.
4. **Socket-only database.** The bundled PostgreSQL has no network; the server reaches it through a socket under a
   root-only directory. New volumes require scram-sha-256.
5. **A credential-free egress forwarder.** With capture off and a global egress proxy set, tools get
   `http://127.0.0.1:8789`; the server adds the proxy's credentials. Without a global proxy it refuses (`502`).
6. **Warn, do not refuse.** Static preconditions (P1-P5) decide whether the chain is used; a self-check run as the
   tool user (C1-C11) and later failures (S13, E1) can only make the status `degraded`. Every status other than
   `enforced` is reported at every start, at every session start and as a UI banner. `ARTEX_TOOL_ISOLATION=off` is the
   operator's override.
7. **Human approval for the six platform tools** that write skills, custom tools and MCP servers, inside the tools
   themselves, never through the AI judge.
8. **Login throttling per source** (local addresses as one source, IPv6 per /64) with a signed per-device cookie; no
   global lockout.
9. **Kill points.** Tool processes are ended at start, at a graceful stop and whenever no session holds one. A lease
   taken by every spawner keeps the idle sweep from ending a process in use.

## Alternatives rejected

- **Refuse to start, or refuse sessions, when the user change cannot work.** It would stop binary installs without
  making them any safer; the warnings make the state visible instead.
- **Capabilities for tools (for raw sockets) in the server's network namespace.** They would apply to the server's
  network as well. Raw sockets come back with a separate runner in its own namespace.
- **Per-uid firewall rules as the main control.** They need `NET_ADMIN` on every install, and the recording proxy
  connects as the server anyway.
- **`setfsuid` for the file tools inside the server.** Per-thread state in a Go process is fragile; a separate process
  is simple to reason about. The cost is latency (below).
- **Editing the SDK** to start shells differently: it is vendored and pinned; the `PATH` shim needs no edit.

## Consequences

- Raw-socket scans (`nmap -sS`, `-sU`, `-O`) and packet capture do not work until the separate runner.
- All tasks share uid 30002: tools of one task can read another task's workspace while sessions run, and a background
  process survives until the next kill point.
- Each file-tool call starts a process (a `Read` took about 160 ms against about 55 ms before).
- Workspaces are owned by uid 30002 on the host.
- Operators with their own Compose files must carry over the socket layout, `init: true`, `pids_limit: 8192` and Docker's default capabilities.
- Binary installs report `unavailable` and keep running tools with the server's access.

## Rollback

Going back to 0.5.17m (with `ARTEX_SCHEMA_ALLOW_NEWER=5003`, because of migration `5003`) runs agent tools as root in
the server's container again and restores the TCP database layout. `ARTEX_TOOL_ISOLATION=off` turns off only the user
change. Steps: [../tool-isolation.md](../tool-isolation.md), "Rollback".

## Outcome to check

The status stays `enforced` on the shipped image across the observation window; no `[toolfs]` failures other than tool
errors; no reports of broken tools beyond the consequences above.
