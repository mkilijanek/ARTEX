# ADR 0011: Signal through an own sidecar, configured through the database

- **Status:** Accepted, built (approved by the project owner). Amends [ADR 0007](0007-signal-bridge-over-private-socket.md)
  (items 2 and 4; the bridge, the nginx proxy and "account management only on the host" are superseded). Not
  verified yet: delivery from a real Signal account and the 2000-byte message limit (tested with a simulated
  signal-cli; the production image was started with the real signal-cli, without an account).
- **Date:** 2026-11-24
- **Runbooks:** [../../example-deployment/README.md](../../example-deployment/README.md#signal-notifications-optional)
  ("Signal notifications": set-up, database role, health, backup, upgrade from the bridge, rollback, incidents),
  [../tool-isolation.md](../tool-isolation.md) (C12), [../schema-upgrade.md](../schema-upgrade.md) ("Migration 5004")

## Context

ADR 0007 put a third-party signal-cli REST bridge behind an allowlisting nginx proxy and a private socket. The sender
number was server environment (`ARTEX_SIGNAL_NUMBER`), and registering or linking the account, listing group ids and
answering rate-limit challenges were done on the host with `docker compose exec ... curl`. The owner asked for:

- linking a Signal account from the UI, by QR code, as a linked device;
- choosing the group from a list instead of copying a `group.<id>`;
- configuration and status passed through the database, not through new socket endpoints.

What does not change: agent tools share the server's network namespace (ADR 0006), so anything `artex` can reach over
the network, a prompt-injected tool can reach too, and a component that can link a device can take over the account.
Sending is synchronous (`Send` returns delivered count and error, and the channel test waits for the answer). The
service parses hostile input (messages, group names, Signal server answers).

## Decision

1. **Own sidecar `artex-signal`.** A static Go supervisor (`cmd/artex-signal`, package `signalsidecar`) starts
   signal-cli (JVM build, version pinned, tarball and native libraries checked by SHA-256) as one long-lived child in
   `jsonRpc` mode on stdio, multi-account mode, with a fixed command line, no shell, and an environment of `HOME`,
   `PATH`, `LANG` and `TMPDIR` only. The supervisor restarts the child with a backoff, and after three timed-out
   calls in a row. Its image is published with each release under the same tag as the ARTEX image. The signal-cli
   release signature was checked against the key with fingerprint `FA10826A74907F9EC6BBB7FC2BA2CD21B5B09570`, taken
   from two sources (`github.com/AsamK.gpg` and keys.openpgp.org).
2. **Send over the private socket.** The sidecar listens on `api.sock`, which it creates with mode 0600 in its own 0700
   directory (a stale socket is removed at start). The volume is mounted in `artex` under `/run/artex-private`
   (root 0700), and C12 checks that tools are kept out. The socket answers `POST /v2/send` and `GET /v1/about` only,
   in the request and response shapes ARTEX already used, so its parsing, classification and scrubbing code stayed.
   ARTEX opens the path without following symlinks and refuses anything that is not a socket before it dials.
3. **Configuration and status through the database.** Migration `5004` adds a configuration row (written by ARTEX)
   and a status row (written by the sidecar), one view and four functions. The sidecar connects as a dedicated role
   that can `SELECT` from that **one view** and `EXECUTE` **three SECURITY DEFINER functions** (heartbeat, set link,
   set groups), with a fixed `search_path`, static SQL, typed parameters and checks of every precondition; nothing
   else in ARTEX's schema. The fourth function is the grant helper, executable by the schema owner only. The role is
   created and checked by the server at start (`LOGIN NOINHERIT CONNECTION LIMIT 5`, a SCRAM verifier computed in Go,
   an ownership comment), never by the migration; provisioning refuses `trust` authentication, `CREATE` for `PUBLIC`
   on schema `public`, a role it did not create and a privileged one, takes `TEMPORARY` on the database away from
   `PUBLIC` once and records that, and never fails the start. The sidecar polls the view every 2 seconds and reports a
   heartbeat; it does not use LISTEN/NOTIFY. Every ARTEX statement on these tables has a 3-second `lock_timeout`.
4. **Linking from the UI, as a linked device, with a confirmation.** The operator starts a link with the password and
   an acknowledgement that the device can read all messages; one link at a time, valid for 2 minutes, and cancellable.
   The link code is served only by a dedicated `no-store` route, is never logged, and is cleared from the status row
   when the link completes, expires or is cancelled. After the scan the account is not usable
   until the operator types the last 6 digits of its number, which the API never returns; ARTEX and the sidecar both
   refuse to send for an unconfirmed account. Wrong digits are throttled with a progressive back-off. Unlink needs the
   password. The sender account is no longer server environment.
5. **A fixed method allowlist.** The sidecar's JSON-RPC client sends only `version`, `listAccounts`, `startLink`,
   `finishLink`, `listGroups` and `send`, each with parameters built in code, and `listGroups` and `send` always name
   the confirmed account. Calls that change or delete the account (`unregister`, `deleteLocalAccountData`,
   `removeDevice`, PIN and account updates) are refused by the client. Unlink is therefore local only: the data
   directory is wiped and the operator removes the device on the phone.
6. **Receive, but keep nothing.** The child runs with `--receive-mode on-start --ignore-attachments --ignore-stories
   --ignore-avatars --ignore-stickers`; incoming messages are counted and dropped, never parsed beyond the method
   name, logged, stored or forwarded. Lines from the child of any length are read with a fixed buffer, and an oversize
   line is skipped.
7. **Native libraries out of the jar, `/tmp` noexec.** libsignal and SQLite JDBC would unpack their native libraries
   into `/tmp` at run time. The image takes them out of the verified jars at build time (checked by SHA-256, debug
   info stripped, the stripped file checked too), installs them in a root-owned read-only directory and removes them
   from the jars, so the container runs with a read-only root filesystem and `/tmp` mounted `noexec,nosuid`.
8. **A non-dumpable supervisor.** The supervisor reads its settings once, removes them from its environment and sets
   `PR_SET_DUMPABLE` to 0 before it starts anything, so signal-cli, which runs as the same user, cannot read the
   database password from `/proc/<pid>/environ`. The database DSN must be a Unix socket or use `sslmode=verify-full`;
   before anything else the sidecar logs in once with a random password and exits (code 78) if that works.
9. **Container hardening.** User 10001, read-only root filesystem, all capabilities dropped, `no-new-privileges`,
   process and memory limits, an init process, no ports, network `signal-egress` only (Internet, no route to `artex`),
   the database socket volume mounted read-only, the account data in a volume mounted by the sidecar alone.

## Alternatives rejected

- **Keep the bridge and the nginx proxy (ADR 0007).** Linking and group listing would stay on the host, or the proxy
  would have to pass the link endpoints, which would make account takeover reachable through the socket.
- **Link and group commands over the socket.** Contradicts the owner's decision, and keeps linking reachable by
  anything that reaches the socket.
- **A database outbox for sending.** A second queue next to the delivery records, finding text stored a second time
  in a table that the least-trusted component drains, a timeout that cannot be told from a send, and a polling loop
  for the synchronous channel test.
- **TCP between `artex` and the sidecar.** ADR 0007 options B and C: tools could reach it.
- **A new wire format for send.** No benefit, and it would discard tested delivery-shape code.
- **LISTEN/NOTIFY.** A second, listening connection with its own reconnect logic, a poll after reconnects anyway, and
  any role may `LISTEN` on any channel.
- **A process per call, or `daemon --socket`.** A JVM start per send, store locking, no continuous receive; or a
  second socket with its own lifetime.
- **Creating the role in the migration.** Roles are cluster-wide, need `CREATEROLE` and a password; a migration must
  behave the same everywhere, and a failed one blocks every start.
- **Reusing ARTEX's database login, peer authentication, an init script, a separate schema.** The sidecar parses
  hostile input; peer needs `pg_hba.conf` and `pg_ident.conf` edits; an init script runs on new volumes only; the
  reference catalog allows schema `public` only.
- **A dedicated NOLOGIN owner role for the view and the functions.** More provisioning branches and a second role in
  every dump (see the accepted risks).
- **No confirmation step; a server-rendered QR image.** Without the confirmation a link code scanned by the wrong
  phone would send at once; an `<img>` cannot carry the bearer header, so the browser renders the code.
- **GraalVM native build of signal-cli.** Needed a reflection fix for linking in the pinned release.
- **The sidecar inside the ARTEX image; signal-cli built from source.** A larger blast radius, and a build that
  cannot be checked against the upstream signature.
- **A transitional mode that keeps `ARTEX_SIGNAL_NUMBER`.** Not wanted by the owner: the variable is ignored with one
  log line.
- **`/tmp` with `exec` for the native libraries.** Not needed once the libraries are taken out of the jars.
- **An egress filter (proxy or shipped firewall rules).** A new component; documented as the operator's job instead.

## Consequences

- One container instead of two, one network instead of two, and the account is managed in the UI. Nothing of the
  bridge deployment is carried over: after the upgrade Signal sends nothing until an account is linked and
  confirmed, and deliveries in between fail visibly and can be retried.
- New settings: `ARTEX_SIGNAL_DB_PASSWORD` (required with the override), `ARTEX_SIGNAL_DB_ROLE`,
  `ARTEX_SIGNAL_DB_AUTH_ASSERTED`, `ARTEX_SIGNAL_IMAGE`; the sidecar's own `ARTEX_SIGNAL_PG_DSN`,
  `ARTEX_SIGNAL_SOCKET`, `ARTEX_SIGNAL_DATA`. Removed: `ARTEX_SIGNAL_NUMBER`, `SIGNAL_CLI_IMAGE`,
  `SIGNAL_PROXY_IMAGE`, `signal-proxy.conf`.
- The database gains a role, one view and four functions. Migration `5004` is additive: the previous release runs
  on the migrated database with `ARTEX_SCHEMA_ALLOW_NEWER=5004`, and a guarded drop recipe exists. A dump carries
  grants to the role but not the role, so the role is created before a restore into a fresh cluster. A
  custom-format restore without it fails on commands that hold a function's `REVOKE ... FROM PUBLIC` together with
  its `GRANT`, which leaves those SECURITY DEFINER functions executable by `PUBLIC`; provisioning does not revoke
  `PUBLIC` again, so the operator runs the documented `REVOKE` statements after such a restore.
- A new function in the schema is executable by `PUBLIC` unless its migration revokes that; a catalog test pins the
  list.
- "Service not connected" and "database unavailable" are retried deliveries; "not linked", "not confirmed" and an
  error state are not. The sidecar answers `503` for network and I/O failures, so those are retried too.
- The image health check fails when signal-cli has been down for more than 120 seconds, but Docker does not restart
  an unhealthy container: the operator's monitoring has to act on it.
- C12 is re-run every 15 seconds while it is not verified (for about an hour), because `artex` no longer waits for
  the Signal container at start.
- The sidecar image has to follow signal-cli releases, because Signal retires old clients. Its first image scan
  showed 0 critical and 9 high findings (8 in Debian `libexpat` and `libuuid` without a fix yet, 1 in `wire-runtime`
  bundled with signal-cli 0.14.10), accepted and tracked.

### Accepted residual risks

- The three SECURITY DEFINER functions run as the schema owner, a superuser in the shipped Compose file; a defect in
  them would run with those rights. A dedicated owner role was not adopted.
- `ARTEX_SIGNAL_DB_PASSWORD` is in `.env` and visible in `docker inspect`, like `POSTGRES_PASSWORD`.
- A link code can remain in old row versions, the write-ahead log, dumps and PostgreSQL statement logs taken while a
  link is pending, and the SCRAM verifier can appear in statement logs. A code is useless after 2 minutes, and a
  stolen one attaches the sidecar to the thief's account, not the operator's.
- The sidecar's outbound network is unrestricted: it can reach the LAN and a cloud metadata address. ARTEX shows no
  warning for this; blocking it is the operator's firewall job.
- A compromised sidecar can make ARTEX hang at start or on four kinds of writes by holding ARTEX's advisory locks,
  until its container is stopped (runbook in the example deployment README).
- Unlink deletes local data only. Until the device is removed on the phone its keys stay valid, and any copy of the
  data volume taken before is a live credential.
- ARTEX's own database connection does not set `require_auth=scram-sha-256` by default, because volumes that still
  trust local connections would stop connecting. The protection against a planted database socket is the read-only
  `pgsock` mount in the sidecar; the README describes how to add the option after the switch to passwords.
- A linked device can read the account's messages; a dedicated account is recommended and acknowledged at link
  time.

## Rollback

The previous release: its images, its `docker-compose.signal.yml` and `signal-proxy.conf`, and
`ARTEX_SCHEMA_ALLOW_NEWER=5004`. The optional database clean-up is the guarded recipe in
[../schema-upgrade.md](../schema-upgrade.md) ("Migration 5004"). Steps: the example deployment README, "Rollback of
the Signal service".

## Outcome to check

The server logs `[signal] database role ready`; a link reaches "linked" and is confirmed; Signal deliveries reach
`sent`; no C12 other than a `not verified: socket absent` that clears within a minute of the start; neither log
contains a link code, a full phone number or a group id.
