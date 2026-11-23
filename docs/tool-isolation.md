# Tool isolation

In the Docker image, every process that an agent starts (Bash, background tasks, `Monitor`, interactive shells, the
file tools, MCP stdio servers, custom command and script tools) runs as a separate unprivileged user, `artex-tool`
(uid/gid 30002). The server itself stays root. The server checks at every start that this works and reports the
result as the **tool isolation status** in its log, in each session's activity trace and as a banner in the web UI.

This page is the operator's reference: what the feature does, what each status and check means and what to do about
it, and what to set up when you do not use the shipped `docker-compose.yml`. The design is in
[adr/0006-agent-tools-run-as-a-separate-user.md](adr/0006-agent-tools-run-as-a-separate-user.md).

## Contents

- [What it does](#what-it-does)
- [The status](#the-status)
- [Checks and what to do](#checks-and-what-to-do)
- [Log lines](#log-lines)
- [Turning it off](#turning-it-off)
- [Custom Compose files](#custom-compose-files)
- [Running outside the official image](#running-outside-the-official-image)
- [Behind a reverse proxy](#behind-a-reverse-proxy)
- [Workspaces](#workspaces)
- [Limits](#limits)
- [Rollback](#rollback)

## What it does

### Users

| uid/gid | name | use |
|---|---|---|
| 30001 | `artex` | reserved, owns nothing |
| 30002 | `artex-tool` | every agent tool process; home `/home/artex-tool`, no login shell |
| 30003 | `artex-sandbox` | reserved, owns nothing |
| 30004 | `artex-mcp` | reserved, owns nothing |

On the host, workspaces of tasks and chat sessions are owned by uid 30002. Edit them on the host with `sudo`, or
through the web UI's workspace browser.

### How a tool process starts

The image puts a root-owned shim directory (`/usr/local/libexec/artex/shims`) first in the server's `PATH`. A shell
the SDK starts goes through these stages, each replacing the previous one:

1. `artex __astool` (root): builds a clean environment from a fixed allow-list, sets the process limit, and passes
   per-call variables in a root-only file instead of on the command line;
2. `setpriv`: changes to uid/gid 30002, clears supplementary groups, empties every capability set and sets
   `no_new_privs`;
3. `artex __toolenv` (uid 30002): applies the per-call variables;
4. the program.

The file tools (`Read`, `Write`, `Edit`, `MultiEdit`, `LS`, `Glob`, `Grep`) run in a short-lived `artex __toolfs`
process started the same way. A file-tool call that the helper cannot complete is a tool error; it never falls back
to running inside the server. The helper has 120 seconds and 16 MiB of output per call; `Read`, `Edit` and
`MultiEdit` refuse devices and files over 256 MiB.

The `__` subcommands are internal stages of the `artex` binary. Do not call them yourself, except
`__migrate-workspaces` (see [Workspaces](#workspaces)).

### What tools can and cannot reach

| Item | With isolation on |
|---|---|
| The server's signing key, configuration file, process environment, the `traffic` directory, the variables directory | not readable by tools (check C3) |
| Database | the bundled PostgreSQL has no network and is reached only through a Unix socket under `/run/artex-private` (root 0700), which tools cannot traverse (check C6). The database password is not readable by tools |
| Targets and the recording proxy | reachable, as before |
| Global egress proxy (traffic capture off) | tools get a local forwarder, `http://127.0.0.1:8789`, in `HTTP_PROXY`, `HTTPS_PROXY` and `ALL_PROXY` (both cases). The forwarder adds the proxy's credentials itself, so tools see neither the proxy address nor its credentials |
| `ARTEX_USER_AGENT` | passed when the server has set it (only while the User-Agent switch resolves a value for API Recon); not a secret |
| Proxy variables that contain `@` in the server's environment | dropped before a tool starts |
| Workspaces (`<data>/tasks/<id>`, `<data>/sessions/<id>`) | owned by uid 30002, mode 0700 |
| Other top-level directories of the data directory | `traffic`, `transcripts`, `evidence`, `archives`: root 0700. `spill`, `drafts`, `noa`, `tasks`, `sessions`: root 0711 (search only) |
| `/tmp`, `/dev/shm`, `/home/artex-tool` | writable |
| Capabilities, raw sockets | none: `nmap -sS`, `-sU` and `-O` and packet capture do not work in this release; `nmap -sT` and `ping` (ICMP datagram sockets) do |

**Egress forwarder.** It listens on `127.0.0.1:8789` inside the container (loopback only, not published by Compose).
Change the address with the server flag `-egress <host:port>` (the host must be a loopback address). `-egress ""`
turns it off: with a global egress proxy set, tools are still pointed at `http://127.0.0.1:8789`, so their egress
through the proxy fails; the log says `[egress] forwarder disabled (-egress is empty): tools have no route through a
global egress proxy`, and the status does not change (no E1). It is a plain forward proxy (HTTP requests with an absolute URL, and `CONNECT`), with no TLS
interception and no recording, and it reads the global proxy setting on every connection, so a change applies without
a restart. It supports `http://`, `https://` and `socks5://` upstream proxies. **It never connects directly:** while
no global egress proxy is configured it answers `502` (`egress forwarder: no egress proxy is configured`) and logs
that at most once a minute. With traffic capture on, tools use the recording proxy as before. WebFetch and WebSearch
run inside the server and keep using the global proxy directly. The browser MCP server gets the forwarder address too.

**CA certificate for tools.** With traffic capture on, `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`,
`GIT_SSL_CAINFO` and `NODE_EXTRA_CA_CERTS` point at `<data>/public/traffic-ca-cert.pem`, a copy the server writes at
start (root, 0644).

**Process limit.** Tool processes have `RLIMIT_NPROC` 4096 (soft and hard). The kernel counts every process of uid
30002 together, so all running tools share these 4096. The shipped Compose file also sets `pids_limit: 8192` for the
whole `artex` container.

**When tool processes are ended.** At server start, at a graceful stop (the server waits up to 5 seconds for running
sessions first), and whenever no agent session holds a tool process (the idle sweep). MCP stdio servers are started
per run, so the idle sweep does not need them to reconnect.

**MCP stdio servers** run as uid 30002 in `/home/artex-tool/mcp/<name>`. For a `stdio` row, environment names that
are not plain variable names, or that start with `LD_`, `GCONV_`, `GLIBC_`, `MALLOC_`, `PYTHON`, `PERL5`, `RUBY` or
`GIT_`, or that are `NODE_OPTIONS`, `BASH_ENV`, `ENV`, `PATH`, `HOME`, `IFS`, `SHELL` or `TMPDIR`, are refused when
saved (`denied variable name <KEY>`, HTTP 400 from the API) and when spawned (log
`[mcp] refused server <name>: denied variable name <KEY>`). `http` and `sse` rows use the same field for request
headers and are not affected.

**Platform tools need a human.** When an agent calls `create_skill`, `update_skill_file`, `create_custom_tool`,
`update_custom_tool`, `create_mcp` or `update_mcp`, the call waits on the **Approval Records** page for an operator.
The AI judge and the intercept rules are never consulted for them; a rejection, a timeout or an error writes nothing.
The record shows the content to be written. For MCP rows, environment values are shown except secrets: a value is
masked when a whole word of its key (split at `_`, `-`, `.` and at a lower-to-upper case change) is `TOKEN`, `SECRET`,
`PASSWORD`, `PASSWD`, `PWD`, `APIKEY`, `CREDENTIAL`, `CREDENTIALS`, `COOKIE`, `BEARER`, `DSN`, `PRIVATE` or
`AUTHORIZATION`, or when `KEY` is the last word or follows `API`, `ACCESS`, `SECRET`, `PRIVATE`, `SIGNING`,
`ENCRYPTION`, `CLIENT` or `AUTH`. In URLs (values and the row's `url`), a userinfo password and query parameters whose
name contains `token`, `key`, `secret`, `sig`, `signature`, `password`, `auth` or `code` are masked.

**Skills** are loaded from a snapshot that the server copies from the skills directory without following links:
`/var/lib/artex/skills/<generation>` in the image (root 0755), `<data>/.skills-snapshot` for binary installs. Symbolic
links, devices, FIFOs and sockets in the skills tree are skipped and logged (`[skills] refused non-regular entry
<path>`). If the skills cannot be loaded for an agent that has skills, that agent's run gets no MCP tools, and status
entry S13 is raised.

**Script tools** write their script to `<data>/spill/scripts/` (removed after the run) and run the interpreter as the
tool user in the workspace. **`ask_operator`** accepts an image only as a regular file with one name, at most 10 MB,
inside the task's working directory or the browser MCP server's directory `/home/artex-tool/mcp/browser`.

**Spill files** (long tool output) are under `<data>/spill/<kind>/<session>`; directories untouched for 7 days are
removed at the next server start.

## The status

| Status | Meaning | Tools run as |
|---|---|---|
| `enforced` | isolation works and every check passed. The shipped image with its default settings | uid 30002 |
| `degraded` | isolation works, but a check failed: something tools should not reach is reachable, a probe could not run, the host lacks link protection, or a server part failed. The reason names the check | uid 30002 |
| `unavailable` | the platform cannot change users (P1-P4 below): binary installs, `cap_drop: ALL`, no `setpriv` | **the server's own user, with its access** |
| `disabled` | `ARTEX_TOOL_ISOLATION=off` | **the server's own user, with its access** |

Only the platform preconditions P1-P5 switch isolation off. A check that runs a tool process (C1-C11) can only move the
status from `enforced` to `degraded`.

**Sessions run in every status.** The status itself refuses nothing; it is reported instead (a session start still fails, with `[workspace] session start refused: ...`, when its workspace cannot be created or handed to the tool user, for example with a reduced capability set or a link where a workspace directory belongs):

- **Server log:** `[isolation] status=<status> tool_uid=<30002|none>` at every start. For any status other than
  `enforced`, a block of `[isolation] WARNING` lines follows; it is repeated every 6 hours.
- **Every session start:** `[isolation] WARNING session <kind>/<id>: <reason>` in the log and an error entry in the
  session's activity trace (task and chat views).
- **Web UI:** a banner above every page.
  - `unavailable` and `disabled`: red, cannot be dismissed;
  - `degraded`: red, can be collapsed for the browser session;
  - `enforced` with warnings (C7, C8 or C9 whose probe ran and connected): a yellow notice that can be dismissed;
  - the page reads the status every 60 seconds and when the window gets focus. After 2 failed reads in a row it shows
    "Tool isolation status is unknown".
- **API:** `GET /api/stats` (authenticated) returns `tool_isolation: {status, reason, warnings}`. `GET /api/health`
  does not carry the status.

The self-check runs once, at start. After fixing the cause of a C1-C9, C11, S13 or E1 entry, **restart the server**
(`docker compose restart artex`). Only C10 clears by itself.

## Checks and what to do

### Platform preconditions (read by the server; no tool process involved)

| Id | Fails when | Status | What to do |
|---|---|---|---|
| P1 | the server does not run as root | `unavailable` | run the official image as root (its default). A container started with `--user` or Kubernetes `runAsNonRoot` with a non-zero user exits at start, as in earlier releases |
| P2 | `CAP_SETUID`, `CAP_SETGID` or `CAP_SETPCAP` is missing from the server's effective set | `unavailable` | keep Docker's default capability set (remove `cap_drop: ALL`). Adding back only these three is **not enough**: P2 then passes, but the server also changes the owner and mode of each new workspace (`CAP_CHOWN`, then `CAP_FOWNER` for the mode of a directory it no longer owns) and must enter the tool user's 0700 directories (`CAP_DAC_OVERRIDE`); without them every session start fails with `[workspace] session start refused: ...` |
| P3 | `/usr/bin/setpriv` is missing, not a root-owned regular file, writable by group or others, or not executable | `unavailable` | use the official image (`setpriv` comes from util-linux) |
| P4 | `/app/artex` is not a static executable, or the shim directory or `bash` shim is not root-owned and closed to others | `unavailable` | build the image from the shipped `Dockerfile`; do not mount anything over `/app/artex` or `/usr/local/libexec/artex` |
| P5 | `ARTEX_TOOL_ISOLATION=off` is set | `disabled` | remove the override |

### Self-check (a shell started exactly as a session starts one, run as the tool user)

| Id | Fails when | Status | What to do |
|---|---|---|---|
| C1 | the tool shell is not the dropped one: wrong uid, gid or groups, a capability left, `no_new_privs` not set, an environment name outside the allow-list, or the shell could not run | `degraded` | read the detail in the log. Usually an image that differs from the shipped `Dockerfile`; rebuild it |
| C2 | the tool uid is not 30002, or equals the server's uid | `degraded` | as C1 |
| C3 | tools can read one of: the environment of process 1 or of the server, the signing key, the configuration file, the `traffic` directory, the variables directory, or a top-level entry of the data directory that is a symbolic link | `degraded` | check the `[isolation] permissions:` lines in the log. Remove symbolic links at the top of the data directory; do not mount the key or configuration file with modes the server cannot close |
| C4 | a fresh tool-owned directory under `<data>/tasks` cannot be created or written | `degraded` | free disk space; make sure the data volume is writable and accepts `chown` (some Docker Desktop and rootless setups do not; not tested) |
| C5 | the skills directory is writable by the tool user | `degraded` | on the host, make `./skills` not writable for uid 30002 (for example owned by your user, mode 0755) |
| C6 | the database socket accepts a connection from the tool user, or the probe could not run | `degraded` | mount the socket volume only under `/run/artex-private` (the image makes it root 0700), as the shipped Compose file does |
| C7 | the database's TCP endpoint accepts a connection from tools (custom Compose files, external PostgreSQL) | warning; `degraded` if the probe could not run | switch to the socket layout of the shipped Compose file, or restrict the database with `pg_hba.conf` and a firewall. Tools still cannot read the password |
| C8 | `/var/run/docker.sock` exists in the container and accepts a connection | warning; `degraded` if the probe could not run | remove the Docker socket mount |
| C9 | the cloud metadata address `169.254.169.254:80` is reachable | warning; `degraded` if the probe could not run | block that address for containers in the host firewall |
| C10 | tool processes are still running after the kill at start, or a cleanup of tool processes failed | `degraded`; cleared automatically once none are left | the server kills again every few seconds while no session needs tools. If it persists, restart the container |
| C11 | `fs.protected_hardlinks` or `fs.protected_symlinks` is not `1` on the host, or cannot be read | `degraded` | on the host: `sudo sysctl -w fs.protected_hardlinks=1 fs.protected_symlinks=1`, and persist both in `/etc/sysctl.d/`. Most distributions already set them |

C7-C9 are warnings when their probe ran and connected; the status stays `enforced` and the UI shows the yellow notice.
When a probe cannot run (`python3`, `timeout` or `bash` missing) or gives no answer, the check degrades: it proved
nothing.

### Status entries raised after start

| Id | Raised when | What to do |
|---|---|---|
| S13 | the skill snapshot cannot be set up at start (`skills will not load (...)`), or a run cannot load the skills of an agent that has skills (`skills cannot be loaded (...): agents with skills get no MCP tools`) | check the `[skills]` lines in the log, the free space and that `/var/lib/artex/skills` is writable by root; restart |
| E1 | the egress forwarder cannot listen at start (`the egress forwarder is not running (...)`) or stops (`the egress forwarder stopped (...)`); tools then cannot use the global egress proxy | check the `[egress]` lines; free the port or fix `-egress`; restart |

Both make the status `degraded` and stay until the server restarts.

## Log lines

| Line | Meaning |
|---|---|
| `[isolation] status=enforced tool_uid=30002` | the start-up result |
| `[isolation] cleanup: no tool process left` / `cleanup: N tool processes survived the kill` | the kill at start |
| `[isolation] warning <id>: <detail>` | with status `enforced`: a check that only warns (C7, C8 or C9 whose probe ran and connected) |
| `[isolation] WARNING ...` | the warning block; repeated every 6 hours while the status is not `enforced` |
| `[isolation] WARNING session <kind>/<id>: ...` | a session started while the status is not `enforced` |
| `[isolation] N leftover tool processes; killing again every few seconds` / `leftover tool processes are gone` | C10 |
| `[isolation] idle sweep failed: ...; trying again every few seconds` | a failed cleanup (at most once a minute; C10) |
| `[isolation] idle sweep skipped: N leases held for > 1 h` | sessions have held tool processes for more than an hour |
| `[isolation] permissions: ...` | an entry the permission step could not close (see C3) |
| `[isolation] stop: no tool process left` | graceful stop |
| `[toolfs] <tool>: <reason>` | a file tool failed in its helper |
| `[mcp] refused server <name>: denied variable name <KEY>` | an MCP stdio row was not started |
| `[skills] refused non-regular entry <path>` | a link or special file in the skills tree was skipped |
| `[egress] forwarder on 127.0.0.1:8789: ...` | the forwarder is listening |
| `[egress] refused a request: no egress proxy is configured; ...` | a tool used the forwarder while no global proxy is set |
| `[workspace] session start refused: ...` | a session's workspace could not be created or handed to the tool user; that session does not start |
| `[workspace] ...` | ownership hand-over of a workspace (see [Workspaces](#workspaces)) |
| `[auth] ignored a legacy key file in the data directory` | a `jwt.key` left in the data directory by an older release was renamed `jwt.key.ignored-<unix>` and is not used |

## Turning it off

```bash
# .env
ARTEX_TOOL_ISOLATION=off
```

Then `docker compose up -d`. Only `off` changes anything (an empty value means unset). It is read from the
environment only; there is no setting in the UI. Tools then run as the server's user, and every start, session and
page shows the warnings. The other changes of this release (socket-only database, workspace API, archives, login
throttling, platform-tool approval, MCP name checks) stay.

## Custom Compose files

If you run your own Compose file, carry over these parts of the shipped `docker-compose.yml`:

```yaml
services:
  postgres:
    network_mode: "none"
    command: ["postgres", "-c", "listen_addresses="]
    environment:
      POSTGRES_INITDB_ARGS: "--auth-local=scram-sha-256 --auth-host=scram-sha-256"   # new volumes only
    volumes:
      - pgdata:/var/lib/postgresql/data
      - pgsock:/var/run/postgresql:z
  artex:
    init: true
    pids_limit: 8192
    environment:
      ARTEX_PG_DSN: postgres://${POSTGRES_USER:-artex}:${POSTGRES_PASSWORD}@/${POSTGRES_DB:-artex}?host=/run/artex-private/postgres&sslmode=disable
    volumes:
      - pgsock:/run/artex-private/postgres:z
volumes:
  pgdata:
  pgsock:
```

- **Capabilities:** keep Docker's default set. With `cap_drop: ALL` the status becomes `unavailable` (P2); adding back only `SETUID`, `SETGID` and `SETPCAP` makes session starts fail (see P2).
- **User:** run the `artex` service as root (the default). Do not set `user:`.
- **`init: true`:** Docker's init as PID 1 collects ended tool processes. Without it, `start.sh` as PID 1 does the
  same while it waits for the server.
- **`pids_limit: 8192`:** an upper bound for the whole container; tool processes are held at 4096 on their own.
- **`:z`** on both socket mounts matters on hosts with SELinux enforcing; elsewhere it does nothing.
- **Database over TCP** (an external PostgreSQL, or a Compose file that keeps the network): it keeps working; the
  self-check warns (C7). Tools cannot read the password.
- **The skills mount** (`./skills:/app/skills`) must not be writable by uid 30002 (C5).
- Do not mount the Docker socket (C8).

## Running outside the official image

- **Binary installs** (`./start.sh`, `start.bat`, `install.sh` option 2, `update.sh` option 2): the server runs as
  your login user and cannot change users, so the status is `unavailable`. Agent tools run as your login user and can
  read what it can read, including this server's configuration, its database and your home directory. `install.sh`
  and `update.sh` print this. Use the Docker image when you need tools separated from the server.
- **Kubernetes or `docker run`:** run the container as root with Docker's default capabilities, and mount the
  database socket under `/run/artex-private/postgres`. Without an init process, `start.sh` as PID 1 collects ended
  processes. `runAsNonRoot` with a non-zero user exits at start, as in earlier releases.
- **Other OS images or a modified `Dockerfile`:** the image must keep the users 30001-30004, `/home/artex-tool`
  (30002, 0700), `/run/artex-private` and `/run/artex-private/vars` (root 0700), `/run/artex-tmp` (root 0711), the
  shim directory, a static `/app/artex` and `/usr/bin/setpriv`. P3, P4 and C1 report what is missing.
- **arm64, Docker Desktop and rootless Docker or Podman** have not been tested.

## Behind a reverse proxy

The login throttle keys on the address the connection comes from and never reads `X-Forwarded-For`. Behind a reverse
proxy, or when clients reach the server through the Docker gateway, **all clients share one source**: repeated wrong
passwords from any of them lock all of them out for a while, and concurrent logins without a device cookie whose password comparisons
overlap beyond the source's remaining failures (5 when none failed) can briefly get `429`. Login and change-password
bodies are limited to 64 KiB (`413`) and must arrive within 10 seconds (`400`, connection closed); neither counts as a
failure. A browser that logged in successfully before carries the
`artex_device` cookie and is counted on its own. Details: the README, "Login throttling".

## Workspaces

Workspaces are created owned by uid 30002, mode 0700. Content written by an older release (owned by root) is handed
to uid 30002 at the first session start of that workspace:

- nothing is followed: links are changed as links, and a file with more than one name (a hard link) is left alone and
  logged (`[workspace] ...: left alone, it has N names: ...`);
- set-id bits are cleared by the kernel;
- each session start hands over at most 200,000 entries or works 10 seconds; the rest is done at the next session
  start (`[workspace] ...: stopped at the limit ...`);
- a root-only stamp, `<data>/.isolation/workspaces`, lets later walks skip unchanged subtrees. It is safe to delete:
  the next walk is a full one.

To finish a large workspace in one go, with the server stopped:

```bash
docker compose stop artex
docker compose run --rm --no-deps --entrypoint /app/artex artex __migrate-workspaces            # every task and chat session
docker compose run --rm --no-deps --entrypoint /app/artex artex __migrate-workspaces --task 42  # one task
docker compose start artex
```

Options: `--data DIR` (default: `data` next to the binary, `/app/data` in the image) and `--task ID` (a number; a
non-numeric id exits `1`; an id without a workspace prints `0 workspaces done` and exits `0`). It prints
`[migrate-workspaces] N workspaces done, ...`. Exit codes: `0` done; `1` failed, a workspace was refused or not
finished, or not run as root; `2` usage; `3` a server is running against the data directory (it holds
`<data>/.isolation/server.lock`).

**Workspace API** (`/api/workspace/*`, the UI's file browser). Every path is opened one component at a time without
following links:

- a symbolic link, or a path through one, answers `400 invalid path`; deleting a link removes the link itself;
- a file with more than one name answers `400 invalid path` for read, download and write: such a file cannot be viewed
  or overwritten through the API;
- a FIFO, device or socket answers `400` for list, read, write, mkdir and upload, `404` for download, and can be
  deleted;
- a path that changes while it is being opened answers `400`; a write to a file that is replaced by a FIFO at that
  moment fails with `500` instead of waiting.

**Task archives** skip symbolic links and files with more than one name (both counted in the manifest's
`skipped_entries`). If an entry changes while it is packed, the archive job fails with
`entry changed during packaging`; archive the task again once it is idle.

## Limits

- **One tool user for all tasks.** Tools of different tasks and chats run as the same uid. While one session is
  running, a tool can read the workspaces of other tasks and the command lines and environment of other tool
  processes, including values the operator gave to MCP servers and custom tools. A process left running in the
  background keeps running until the next point where all tool processes are ended (no session holding tool
  processes, a graceful stop, or a start).
- **Values you give tools are readable by tools.** MCP environment values and custom tool parameters are meant for
  tools and are not hidden from them.
- **No raw sockets in this release.** `nmap -sS`, `-sU`, `-O` and packet capture need capabilities that tools do not
  get. A separate tool runner that restores them is planned for the next release.
- **Process limit.** All tools together can run at most 4096 processes; a tool that forks without bound slows other
  tools until it is ended. The server and its kill helper stay usable.
- **File tools are slower.** Each file-tool call starts a helper process: a `Read` took about 160 ms against about
  55 ms before (median of 100 calls on one host).
- **Sparse files grow on restore.** A sparse file in a workspace is restored from a task archive at its full size.
- **`Monitor` commands** do not use the recording proxy or the global egress proxy (as before): they connect from the
  host's own address, unrecorded. Use Bash for target traffic.
- **Tool interception rules for Bash** do not see custom command and script tools.
- **Hosts without link protection** (C11) run `degraded`.
- **Binary installs** run tools with the server's access (`unavailable`).

## Rollback

To go back to 0.5.17m:

1. Restore its Compose file (`git checkout v0.5.17m -- docker-compose.yml`, or your own copy) and its image
   (`git checkout v0.5.17m`, then `docker compose up -d --build`).
2. **Set `ARTEX_SCHEMA_ALLOW_NEWER=5003`** in `.env`. This release also applies database migration `5003`; without
   the override 0.5.17m refuses to start (`[migrate] refused: unknown migration applied ...`). See
   [schema-upgrade.md](schema-upgrade.md), "Migration 5003".
3. Data: nothing to do. Files owned by uid 30002 stay readable to the older release; `<data>/public`,
   `<data>/spill`, `<data>/.isolation` and `jwt.key.ignored-*` are ignored by it.

After the rollback, agent tools run as root again, the database uses the older TCP layout, and the throttling,
approval and workspace rules of this release are gone.

A partial rollback keeps this release and only turns the user change off: `ARTEX_TOOL_ISOLATION=off`
([Turning it off](#turning-it-off)).
