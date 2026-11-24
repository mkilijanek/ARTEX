# ARTEX example deployment (published image)

A Docker Compose deployment that runs ARTEX from the published image
`ghcr.io/mkilijanek/artex`, pinned to one version. Nothing is built and no repository checkout
is needed on the target host: copy this directory, fill in `.env`, start.

It keeps the security layout of the repository's `docker-compose.yml`: PostgreSQL has no network
and does not listen on TCP (a Unix socket in a shared volume only), nothing but the web UI and the
capture proxy is published, both on `127.0.0.1` by default, and no capabilities are added or dropped
(tool isolation needs Docker's default set; see [../docs/tool-isolation.md](../docs/tool-isolation.md)).
Do not add `cap_drop: ALL` or `privileged`.

Requirements: Docker with the Compose plugin (a recent version: the overrides use `!override`, added in v2.24; tested with v5.8.0), a
`linux/amd64` host. The release publishes `linux/amd64` images only; there is no native arm64 image.

**Versions older than 0.5.18m use a different compose layout (TCP PostgreSQL, different volumes)
and must not be used with this file.** Upgrade such an installation to 0.5.18m first by following
[../README.md](../README.md#updating-and-upgrading) and [../docs/schema-upgrade.md](../docs/schema-upgrade.md).
When you upgrade from 0.5.17m or earlier, rotate the database password and the model provider API
keys afterwards and follow the other post-upgrade actions in
[GHSA-x67q-r3xq-3ghq](https://github.com/mkilijanek/ARTEX/security/advisories/GHSA-x67q-r3xq-3ghq). A fresh install needs none of this.

## Quick start

```bash
cp example.env .env
$EDITOR .env          # set POSTGRES_PASSWORD (a long random value), ARTEX_VERSION, API keys if you want them
docker compose pull
docker compose up -d
docker compose ps     # wait until both services are up and postgres is healthy
```

Open <http://127.0.0.1:8787>. While no admin password is set, the server prints a one-time setup
token at startup; `/setup` asks for it:

```bash
docker compose logs artex | grep "Setup token"
```

Then set the admin password on `/setup`. (If the line is missing, a password already exists in the
data volume.) LLM keys can be set in `.env` or in the UI.

Check tool isolation after the first start. The status should read `enforced`:

```bash
docker compose logs artex | grep "\[isolation\] status="
```

The web UI shows a banner on every page when the status is anything else. See
[../docs/tool-isolation.md](../docs/tool-isolation.md) for the statuses and what each cause means.
Also expect `[migrate]` lines: the schema migrates at start.

## Files

| File | Purpose |
|---|---|
| `docker-compose.yml` | the base deployment: postgres, artex, named volumes `pgdata`, `pgsock`, `artexdata` |
| `example.env` | template for `.env` (versions, database, ARTEX_* settings) |
| `docker-compose.digest.yml` | pin the artex image by digest instead of by tag |
| `docker-compose.data-bind.yml` | store data in a host directory instead of the volume `artexdata` |
| `docker-compose.skills.yml` | use a host skills directory instead of the built-in skills |
| `docker-compose.lan.yml` | publish the UI beyond localhost (read the warning) |
| `docker-compose.resources.yml` | example CPU and memory limits |
| `docker-compose.signal.yml` | Signal notifications: the `artex-signal` service, reachable by ARTEX only through a Unix socket (see [Signal notifications](#signal-notifications-optional)) |

## Choosing and combining overrides

Every override is optional and is added with another `-f`, always after the base file:

```bash
docker compose -f docker-compose.yml -f docker-compose.digest.yml -f docker-compose.resources.yml up -d
```

To avoid typing it each time, set `COMPOSE_FILE` in `.env` (separator `:` on Linux and macOS):
`COMPOSE_FILE=docker-compose.yml:docker-compose.digest.yml`. Use the same set of files for every
later command (`pull`, `logs`, `exec`, `down`).

- **digest**: needs `ARTEX_IMAGE_DIGEST=sha256:...` in `.env`. Get it from the release workflow
  summary (`Digest: sha256:...`) or with
  `docker buildx imagetools inspect ghcr.io/mkilijanek/artex:v0.6.0m`. A digest cannot be moved,
  a tag can. `ARTEX_VERSION` is still required and should match.
- **data-bind**: `ARTEX_DATA_DIR` (default `./data`). The image runs the server as root and
  creates and owns its subdirectories itself, so no `chown` on the host is needed; keep the
  directory private (it holds `jwt.key`). It does not copy existing data: stop the stack and copy
  the volume content first if you switch.
- **skills**: `ARTEX_SKILLS_DIR` (default `./skills`). The mount hides the built-in skills, so
  seed the directory first (the command is in the file's header). It is not mounted read-only
  because the UI and platform tools create and edit skills there. The directory must not be
  writable by uid 30002 (own it by your user, mode 0755), or the isolation status becomes
  `degraded`. Skills are snapshotted at start: restart after host edits.
- **lan**: publishes `8787` on `0.0.0.0` (or `ARTEX_BIND_ADDR` if you set it), over plain HTTP.
  Use only on a trusted network or put a TLS reverse proxy in front
  ([../README.md](../README.md#reverse-proxy-deployment-https--only-exposing-443)). Port `8788`
  stays on `127.0.0.1`.
- **resources**: example limits (artex 2 CPUs / 4 GiB, postgres 1 CPU / 1 GiB). Tune them.
- **signal**: needs `ARTEX_SIGNAL_DB_PASSWORD` in `.env` (Compose refuses to start without it). Adds the service
  `artex-signal`, the network `signal-egress` and the volumes `artex-signal-data` and `artex-signal-sock`, and gives
  `artex` the socket and the password of the service's database role. `artex` does not wait for the service. The
  Signal account is linked from the UI. Setup, limits and runbooks: [Signal notifications](#signal-notifications-optional).

Validate any combination without starting anything:

```bash
docker compose -f docker-compose.yml -f docker-compose.lan.yml config
```

## Signal notifications (optional)

ARTEX can send finding notifications to one Signal group or to Signal phone numbers through its own Signal service,
`artex-signal`. It runs signal-cli as a **linked device** of one Signal account (as Signal Desktop is linked to a
phone); you link it from the UI by scanning a QR code. `docker-compose.signal.yml` runs it from the image
`ghcr.io/mkilijanek/artex-signal`, which each release publishes with the same tag as `ghcr.io/mkilijanek/artex`. Both
images use `ARTEX_VERSION`, so it needs a release that publishes the `artex-signal` image (the first one after
0.6.0m). The repository's root `docker-compose.yml` has no Signal support; use this directory.

The service has been tested with a simulated signal-cli, and the production image has been started with the real
signal-cli, with `/tmp` mounted `noexec`, but without a Signal account. Delivery from a real account and the 2000-byte
message limit have not been verified yet.

**Use a dedicated Signal account.** A linked device can read every message of the account. The service drops what it
receives (it downloads no attachments, stories, avatars or stickers) and stores and forwards none of it, but whoever
takes over the service or its data volume can read the account's new messages.

### Layout and why

Agent tools run in the `artex` container's network namespace (as uid 30002, see
[../docs/tool-isolation.md](../docs/tool-isolation.md)), so anything `artex` can reach over the network, tools can
reach too. The override therefore gives `artex` **no network path to the service**, and nothing in the service can
link a device or change the account through the socket:

| Part | What it is |
|---|---|
| `artex-signal` | a Go supervisor that starts signal-cli (JVM) with a fixed command line and no shell. User `10001:10001`, read-only root filesystem, `/tmp` in memory with `noexec,nosuid`, all capabilities dropped, `no-new-privileges`, `pids_limit: 256`, `mem_limit: 768m`, `init: true`, no ports. Network `signal-egress` only |
| `signal-egress` | outbound Internet access for the service alone (Signal's servers). `artex` is not on it |
| `artex-signal-sock` | the socket volume: `/sock` in the service, `/run/artex-private/signal` in `artex`. The service creates `api.sock` with mode 0600 in its own 0700 directory; `/run/artex-private` is root 0700, so tools cannot traverse it (check C12). SELinux label `:z` |
| `artex-signal-data` | the account keys and the signal-cli state (`/data`), mounted in the service only. Private SELinux label `:Z` |
| `pgsock` | the database socket volume, mounted **read-only** (`:ro,z`): the service can connect, but cannot create or replace a file there |

The socket answers exactly two requests: `POST /v2/send` (body at most 64 KiB) and `GET /v1/about`. Every other path
gets `404` (a wrong method on those two paths gets `405`). Linking, unlinking, the confirmation and the group list go
through the database instead: ARTEX writes what the operator asked for into a table, and the service reads it through
**one view** and reports its state through **three functions**, as its own database role. That role can do nothing
else in ARTEX's schema: it cannot read any table, create objects or create temporary tables, and has at most 5
connections. It keeps what PostgreSQL gives every role: it can connect to other databases of the cluster that allow
`CONNECT` to `PUBLIC`, read the system catalogs (names and function sources, not table rows) and take advisory locks
(see [Incident: ARTEX hangs at start or on writes](#incident-artex-hangs-at-start-or-on-writes)).

signal-cli in the service only ever gets these calls: `version`, `listAccounts`, `startLink`, `finishLink`,
`listGroups` and `send`. It cannot register, unregister or delete the account, or remove a device.

**Environment of the service** (the override sets it; you do not need to change it):

| Variable | Meaning |
|---|---|
| `ARTEX_SIGNAL_PG_DSN` | required: the connection string of the service's role. A Unix socket, or TCP with `sslmode=verify-full`. The override builds it from `ARTEX_SIGNAL_DB_ROLE`, `ARTEX_SIGNAL_DB_PASSWORD` and `POSTGRES_DB` |
| `ARTEX_SIGNAL_SOCKET` | the socket path, default `/sock/api.sock` (set in the image) |
| `ARTEX_SIGNAL_DATA` | the signal-cli data directory, default `/data` (set in the image) |

The service reads these once and removes them from its environment, and makes itself non-dumpable, so signal-cli
(the same user) cannot read the password from `/proc`. signal-cli gets only `HOME`, `PATH`, `LANG` and `TMPDIR`.

**The service refuses to run (exit code 78)** when `ARTEX_SIGNAL_PG_DSN` is missing or cannot be parsed, when it names
a TCP host without `sslmode=verify-full`, or when the database accepts a login of the role **with a wrong password**
(the service tries one random password before anything else). Docker then restarts it again and again;
`docker compose logs artex-signal` shows the reason. Until the database can be reached, it waits and logs
`[signal] the database password check cannot be tested yet: ...`. After that it waits, without starting signal-cli or
opening the socket, until ARTEX has confirmed that the database checks passwords (see
[Database role](#database-role)).

**Limits of this protection.**

- The separation relies on Docker isolating user-defined networks from each other. Tested with Docker (iptables and
  firewalld backend). Docker with `"iptables": false`, and Podman, do not enforce it. Run the check below after any
  change of the container engine or firewall.
- **Outbound traffic of the service is not restricted.** Besides Signal's servers it can reach your LAN, a cloud
  metadata address (`169.254.169.254`) and ARTEX's port if that is published on a non-loopback address. ARTEX does
  not warn about this. If it matters, block those destinations for the subnet of `signal-egress` in the host firewall
  (for Docker, in the `DOCKER-USER` chain). The subnet:
  `docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' example-deployment_signal-egress`.

Check the layout (both must hold):

```bash
# 1. the socket, from inside artex as the tool user: expect PermissionError
docker compose exec -u 30002:30002 artex python3 -I -S -c \
  'import socket; socket.socket(socket.AF_UNIX).connect("/run/artex-private/signal/api.sock")'
# 2. the networks of artex: expect no name ending in _signal-egress
docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$(docker compose ps -q artex)"
```

Also check that `docker compose logs artex | grep "\[isolation\] status="` still says `enforced` and that no C12
appears.

### Set up

The commands below assume the override is in the file set: put
`COMPOSE_FILE=docker-compose.yml:docker-compose.signal.yml` in `.env` (see
[Choosing and combining overrides](#choosing-and-combining-overrides)).

1. **Password of the service's database role.** Set `ARTEX_SIGNAL_DB_PASSWORD` in `.env` to a long random value of
   letters and digits, for example the output of `openssl rand -hex 24` (the value goes into a connection string
   without quoting). Compose refuses to start the override without it.
2. **Start:** `docker compose pull && docker compose up -d`. `artex` does not wait for the service.
3. **Check the role:** `docker compose logs artex | grep "\[signal\]"` must show `[signal] database role ready`. If it
   shows anything else, see [Database role](#database-role). `docker compose ps` shows `artex-signal` as `healthy`
   once its socket answers, normally within a minute.
4. **Link** (System, Notifications, card "Signal account", **Link account**): enter a device name (letters, digits,
   space, dot, underscore or hyphen, up to 50; it is shown on the phone), your current ARTEX password, and tick the
   acknowledgement that the device can read all messages. Scan the QR code with Signal on the phone of the account
   (Settings, Linked devices, Link a new device). The code is valid for 2 minutes; **Cancel** ends it early. One link
   can be pending at a time.
5. **Confirm:** type the last 6 digits of the phone number of the account you meant to link. ARTEX never shows those
   digits, so a code scanned by the wrong phone cannot be confirmed by mistake, and **nothing is sent until you
   confirm** (both ARTEX and the service refuse). Wrong digits count like wrong passwords, and the third wrong attempt
   starts a wait of 10 seconds that doubles with each further wrong attempt, up to 15 minutes.
6. **Channel** (System, Notifications): kind "Signal". **A group is recommended** and preselected: members are managed
   in Signal, and ARTEX stores no personal phone numbers.
   - **Group.** Add the linked account to the group in Signal. In the channel, choose "Group (recommended)" and pick
     the group from the list; **Refresh** reloads the list from the account. The masked id next to each name tells
     groups with the same name apart. "Enter id manually" takes a `group.<id>` instead.
   - **Phone numbers.** Choose "Phone numbers" and enter one or more numbers in E.164 form (`+15555550101`).
7. Press **Send test message**. A failure shows the error text (see [Errors](#errors)).

**Unlink** (card, **Unlink**, with your password) deletes the account data inside the service and starts signal-cli
without an account. **It does not remove the device from the account**: signal-cli gives a linked device no way to do
that. Remove the device (the name you chose) under Linked devices in Signal on the phone. Until you do, its keys stay
valid on Signal's servers, and any copy of `artex-signal-data` taken before the unlink can still read and send as that
account. When the card shows an error state ("Unlink and reset"), unlinking is also the way out: link again
afterwards.

### Database role

At every start, when `ARTEX_SIGNAL_DB_PASSWORD` is set, ARTEX creates or checks the service's role
(`ARTEX_SIGNAL_DB_ROLE`, default `artex_signal`; it must not be the `POSTGRES_USER` role). It:

- refuses, and changes nothing, when `pg_hba.conf` lets the role in **without a password** (method `trust`), when
  `PUBLIC` may create objects in schema `public`, or when a role of that name exists that ARTEX did not create (it
  marks its own role with the comment `artex signal sidecar`) or that has a forbidden attribute (`SUPERUSER`,
  `CREATEDB`, `CREATEROLE`, `REPLICATION`, `BYPASSRLS`), a membership in another role or owned objects;
- creates the role when it is absent (`LOGIN NOINHERIT CONNECTION LIMIT 5`, nothing else, a SCRAM-SHA-256 password
  verifier computed by ARTEX, the marker comment, and the settings `idle_in_transaction_session_timeout = '10s'` and
  `search_path = public`), or changes only what differs on a role it made;
- checks the password by logging in as the role and sets it only when that login fails (so a changed
  `ARTEX_SIGNAL_DB_PASSWORD` applies at the next start of `artex`; restart `artex-signal` afterwards);
- grants the view and the three functions, and takes `TEMPORARY` on the ARTEX database away from `PUBLIC` once. It
  records that, so the rollback recipe gives it back only when ARTEX took it.

It never fails the start. The outcome is one log line and a line on the card:

| Log line (`docker compose logs artex`) | What to do |
|---|---|
| `[signal] database role ready` | nothing |
| `[signal] database role: pg_hba.conf lets this role in without a password (method trust); switch the rules to scram-sha-256, then restart` | the database volume was created before 0.5.18m and still trusts local connections. Follow the two steps "To require the password on an existing volume" in `example.env`, then `docker compose restart artex artex-signal` |
| `[signal] database role: PUBLIC may create objects in schema public; run REVOKE CREATE ON SCHEMA public FROM PUBLIC, then restart` | run that statement as `POSTGRES_USER` in the ARTEX database, then restart `artex` |
| `[signal] database role: a role of that name exists that this server did not create; it is left unchanged` | pick another `ARTEX_SIGNAL_DB_ROLE`, or look at that role and drop it yourself |
| `[signal] database role: the role has ...; an operator must look at it, nothing was changed` | remove the named attribute, membership or owned objects, then restart `artex` |
| `[signal] database role missing and cannot be created by this database user; run once as a user who may: ...` | an external PostgreSQL; see below |
| `[signal] database role not checked: no password is configured and the role does not exist` | `ARTEX_SIGNAL_DB_PASSWORD` does not reach `artex`; the override is not in the file set |

`ARTEX_SIGNAL_DB_ROLE`: the shipped override uses it only in the service's connection string and does not pass it to
`artex`, which then provisions the default name. Keep the default, or also set the variable for `artex` in a local
override file.

**External PostgreSQL.** The service's connection string must be a Unix socket or use `sslmode=verify-full`.

- When ARTEX's database user may not create roles, run the role statements once as a user who may, with the
  **same password** as `ARTEX_SIGNAL_DB_PASSWORD`, then restart `artex`. ARTEX finds its marker, checks the login and
  grants the rest itself:

  Replace `artex_signal` when `ARTEX_SIGNAL_DB_ROLE` names another role:

  ```sql
  CREATE ROLE artex_signal LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 5 PASSWORD '<password>';
  COMMENT ON ROLE artex_signal IS 'artex signal sidecar';
  ALTER ROLE artex_signal SET idle_in_transaction_session_timeout = '10s';
  ALTER ROLE artex_signal SET search_path = public;
  ```

- When the card then reports `role can create temporary tables`, ARTEX's user could not take `TEMPORARY` away from
  `PUBLIC`. As the owner of the ARTEX database, check whether `PUBLIC` holds it, and only if this returns `t`, run the
  transaction below (replace `artex` with the database name):

  ```sql
  SELECT d.datacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(d.datacl) a WHERE a.grantee = 0 AND a.privilege_type = 'TEMPORARY')
    FROM pg_database d WHERE d.datname = current_database();
  BEGIN;
  REVOKE TEMPORARY ON DATABASE artex FROM PUBLIC;
  UPDATE signal_sidecar_config SET public_temp_revoked = true, updated_at = now() WHERE id;
  COMMIT;
  ```

- ARTEX reads the authentication rules from `pg_hba_file_rules`, which by default only superusers may read. When it
  cannot, the card shows "Database logins: could not be checked", and the service waits without starting signal-cli.
  Once you have made sure that the rules require a password for the role, set `ARTEX_SIGNAL_DB_AUTH_ASSERTED=1` in
  the environment of `artex`. The service still refuses to start when a wrong password logs in.
- On a cluster shared with other databases, run `REVOKE CONNECT ON DATABASE <other> FROM PUBLIC` for each of them,
  or use the `pg_hba.conf` lines below.

**Optional: admit the role only to the ARTEX database.** The two lines below, placed first in `pg_hba.conf` (the
first matching line wins), let the role log in to the ARTEX database with a password and refuse it everywhere else.
For the bundled PostgreSQL, run this once, after the rules require passwords:

```bash
docker compose exec -T -u postgres postgres sh -c 'f="$PGDATA/pg_hba.conf"; { printf "local %s artex_signal scram-sha-256\nlocal all artex_signal reject\n" "$POSTGRES_DB"; cat "$f"; } > /tmp/pg_hba.new && cat /tmp/pg_hba.new > "$f" && rm /tmp/pg_hba.new && pg_ctl reload -D "$PGDATA"'
```

It prints `server signaled`. To undo, delete the two lines from the top of the file and run `pg_ctl reload` the same
way.

**Optional: require SCRAM on ARTEX's own connection.** Once the database volume requires passwords (the steps in
`example.env`), you can add `require_auth=scram-sha-256` to `ARTEX_PG_DSN`. ARTEX then refuses a server that asks for
a cleartext or MD5 password or lets it in without one, which protects its password against a fake database socket. It
is not the default because on a volume that still trusts local connections ARTEX would not connect at all. Put this
in a file of your own, for example `docker-compose.require-auth.yml`, add it to the file set after the other files,
and check that `artex` still starts:

```yaml
services:
  artex:
    environment:
      ARTEX_PG_DSN: postgres://${POSTGRES_USER:-artex}:${POSTGRES_PASSWORD}@/${POSTGRES_DB:-artex}?host=/run/artex-private/postgres&sslmode=disable&require_auth=scram-sha-256
```

### Health and restarts

- The supervisor restarts signal-cli by itself, with a backoff, when it exits, and also after three calls in a row
  have timed out.
- The image's health check asks the service on its socket (`GET /v1/about`, no database), every 30 seconds, after a
  start period of 60 seconds, 3 retries. It fails while the socket does not exist yet (the service is waiting for the
  database, see above) and when signal-cli has been down or silent for more than 120 seconds. In a test, the container
  turned `unhealthy` about 3 minutes after signal-cli stopped.
- **Docker does not restart an `unhealthy` container.** `restart: unless-stopped` acts only when the process exits.
  Something has to act on `unhealthy`: your monitoring, or for example a cron job that runs this every few minutes:

  ```bash
  cd /path/to/example-deployment && [ "$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose ps -q artex-signal)")" = unhealthy ] && docker compose restart artex-signal
  ```

- The card shows "Service: not connected" when the service has not reported for 30 seconds. Deliveries made then
  fail with `Signal service is not connected` and are retried.

### Channel rules

- Config keys: `recipients` (list) and `recipient_type` (`group` or `numbers`). Without `recipient_type` (API clients),
  the list itself decides. There is no sender, URL or secret in the channel: the sender is the linked and confirmed
  account, and the endpoint is `ARTEX_SIGNAL_API`, server environment read once at start.
- A group is exactly one `group.<id>`; numbers are one or more E.164 numbers. Numbers and a group cannot be mixed in
  one channel, and a channel takes one group only: use one channel per group. Duplicates are removed when sending.
  Signal usernames and UUIDs are not supported.
- Default rate: 20 messages per minute.
- Messages are plain text, at most 2000 bytes each. A digest that does not fit sends the remaining findings in the
  next message; one over-long finding is cut. No attachments and no link previews.
- Delivery is at least once: when the service has already passed a message to Signal but ARTEX times out (25 seconds)
  before the answer, ARTEX sends it again and recipients see it twice.
- Phone numbers and group ids are masked in ARTEX's errors, delivery history and logs (`…0102`, `group.…abcd`). The
  service's log (`docker compose logs artex-signal`) holds no message text, link code, group name, full number or
  group id either.

### Errors

| The error contains (delivery history, test result, `[notify]` log) | Meaning | Retried |
|---|---|---|
| `Signal service socket unreachable (is docker-compose.signal.yml up?)` | the service is not running or has not opened its socket yet, or the override is not in the file set | yes |
| `Signal service is not connected` | the service has not reported to the database for 30 seconds | yes |
| `Signal state cannot be read: database unavailable` | ARTEX could not read the Signal state | yes |
| `peer server error (HTTP 503): ...` | the service runs, but signal-cli is not running, its view of the configuration is stale, or Signal's servers could not be reached | yes |
| `Signal rate limit: ...` | Signal rate-limits the account; wait. ARTEX cannot answer a rate-limit challenge | yes |
| `Signal account is not linked: link one under System, Notifications` | no account is linked | no |
| `confirm the linked Signal account first` | an account is linked but not confirmed | no |
| `Signal service is in an error state: unlink and link the account again` | more than one account or an account without a number in the data volume, or the device was removed on the phone | no |
| `Signal service is not configured: ...` | `ARTEX_SIGNAL_API` is unset or invalid; at start the log has `[notify] signal: ARTEX_SIGNAL_API is invalid ...` | no |
| `Signal service socket path is a symlink; refusing to connect` / `... is not a socket; refusing to connect` | something other than the service's socket is at the path: check the volume mounts | yes |
| `peer rejected the request (HTTP 400): <reason>` | the service refused the send (a recipient Signal does not know, an unknown group, a changed safety number, ...) | no |
| `delivered to K of N; failed: …0102 (<reason>)` / `delivered to the group, but failed for M member(s): ...` | a partial delivery; sending again would repeat it to those who got it | no |
| `Signal service returned an unexpected success response (HTTP <code>)` | the service answered 2xx without confirming a send | no |

Failures that are not retried automatically can be retried from the delivery records on the Notifications page once
the cause is gone.

### Upgrading the service

The image carries one pinned signal-cli version (0.14.10 in the first release), checked by its SHA-256 at build
time. Signal stops accepting old client versions after some months, and sends then fail, so keep ARTEX up to
date; the service follows `ARTEX_VERSION`. The signal-cli version of an image:
`docker image inspect -f '{{ index .Config.Labels "io.artex.signal-cli.version" }}' <image>`.

To pin the service by digest, set `ARTEX_SIGNAL_IMAGE=ghcr.io/mkilijanek/artex-signal:<tag>@sha256:<digest>` in
`.env` (get the digest with `docker buildx imagetools inspect ghcr.io/mkilijanek/artex-signal:<tag>`), and change it
together with `ARTEX_VERSION`.

`docker compose` may print `mount of type volume should not define bind option` for the `:z`/`:Z` labels on named
volumes (the base file's `pgsock` too). It is harmless.

### Backup of the account

`artex-signal-data` holds the linked device's keys: **whoever has a copy can send, and read new messages, as the
linked account until the device is removed on the phone.** You do not need a backup: when the volume is lost, the
service starts without an account; link again and remove the old device on the phone. If you keep a backup anyway,
keep it as private as `.env`. The volume carries a private SELinux label, so the throwaway container runs without label
confinement (`--security-opt label=disable`), as root, and does not relabel the volume. The volume name is
`<project>_artex-signal-data` (`docker volume ls | grep artex-signal-data`):

```bash
docker run --rm --security-opt label=disable -v example-deployment_artex-signal-data:/d:ro -v "$PWD":/backup alpine \
  tar czf /backup/artex-signal-data-$(date +%Y%m%d-%H%M%S).tar.gz -C /d .
```

Restore with the service stopped (`docker compose stop artex-signal`), into the same volume, then start it again
(`docker compose up -d`). The archive keeps the owner (10001) and the modes:

```bash
docker run --rm --security-opt label=disable -v example-deployment_artex-signal-data:/d -v "$PWD":/backup:ro alpine \
  tar xzf /backup/artex-signal-data-<timestamp>.tar.gz -C /d
```

**Database dumps** (see [Backups](#backups)) carry the grants to the service's role, but not the role itself (roles
belong to the cluster). Restored into a fresh database volume, the four `GRANT ... TO artex_signal` lines fail with
`ERROR:  role "artex_signal" does not exist` and the rest of the restore goes on. That is harmless: at its next start
with `ARTEX_SIGNAL_DB_PASSWORD` set, ARTEX creates the role and grants again. To restore without those errors, create
the role first, with the same password:

```bash
set -a; . ./.env; set +a
PGPASSWORD="$POSTGRES_PASSWORD" docker compose exec -T -e PGPASSWORD -e ARTEX_SIGNAL_DB_PASSWORD postgres \
  sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1' <<'SQL'
\getenv pw ARTEX_SIGNAL_DB_PASSWORD
CREATE ROLE artex_signal LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 5 PASSWORD :'pw';
COMMENT ON ROLE artex_signal IS 'artex signal sidecar';
ALTER ROLE artex_signal SET idle_in_transaction_session_timeout = '10s';
ALTER ROLE artex_signal SET search_path = public;
SQL
```

**Do not restore with `--no-acl`** (`pg_restore`), and do not dump with `--no-acl` or `-x`: that also drops the
`REVOKE ... FROM PUBLIC` lines, and every role of the cluster could then run the service's functions. If it happened,
run as `POSTGRES_USER` in the ARTEX database:

```sql
REVOKE ALL ON FUNCTION public.signal_sidecar_heartbeat(text, bigint, text, text, text, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.signal_sidecar_set_link(bigint, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.signal_sidecar_set_groups(bigint, jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.signal_sidecar_grant(regrole) FROM PUBLIC;
```

### Upgrade from the signal-cli REST API override

Releases 0.5.19m to 0.6.0m shipped a different `docker-compose.signal.yml`: a third-party signal-cli REST API container,
an nginx proxy configured by `signal-proxy.conf`, and the sender number in `ARTEX_SIGNAL_NUMBER`. Nothing of it is
carried over: the new service is linked again from the UI, and its old data volume is not used. Signal deliveries
made between the upgrade and the confirmation of the new link fail visibly and can be retried from the delivery
records.

1. Back up first (see [Backups](#backups)). Keep the old volume `<project>_signal-data` untouched until the new set-up
   works: it is the way back.
2. In `.env`: set `ARTEX_VERSION` to the new release, set `ARTEX_SIGNAL_DB_PASSWORD`, and remove
   `ARTEX_SIGNAL_NUMBER`, `SIGNAL_CLI_IMAGE` and `SIGNAL_PROXY_IMAGE`.
3. Replace `docker-compose.signal.yml` with the one of the new release and delete `signal-proxy.conf`.
4. `docker compose pull && docker compose up -d --remove-orphans`. `--remove-orphans` removes the old `signal-cli` and
   `signal-proxy` containers.
5. Continue with [Set up](#set-up) from step 3: check the role, link, confirm, and pick the group in each Signal
   channel. A stored `group.<id>` stays valid (a group's id does not depend on the device that sends), as long as the
   newly linked account is a member of the group. The service can only be **linked** to an account that has a phone
   (or other primary device); it cannot register a number. A number that was registered on the old container itself
   needs a primary device before it can be linked.
6. Press **Send test message**. When it works, remove the old device on the phone (if the old container was a linked
   device), and delete the old volumes once you are sure:
   `docker volume rm example-deployment_signal-data example-deployment_signalsock`.

### Rollback of the Signal service

**Back to the previous release** (with the signal-cli REST API override):

1. Take `docker-compose.signal.yml` and `signal-proxy.conf` from that release (the `example-deployment` directory of
   its tag), and put back its `.env` values: `ARTEX_VERSION`, `ARTEX_SIGNAL_NUMBER` and, if you had them,
   `SIGNAL_CLI_IMAGE` and `SIGNAL_PROXY_IMAGE`.
2. Set `ARTEX_SCHEMA_ALLOW_NEWER=5004` in `.env`: the older binary refuses the database otherwise, because migration
   `5004` added the Signal tables. Read the risks in
   [../docs/schema-upgrade.md](../docs/schema-upgrade.md) ("The override `ARTEX_SCHEMA_ALLOW_NEWER`").
3. `docker compose pull && docker compose up -d --remove-orphans`. This removes the `artex-signal` container; the old
   containers start with the untouched `signal-data` volume. (If you deleted it, register or link the number again as
   that release's README describes.)
4. Optional, to remove the Signal tables, functions and role from the database: stop `artex`, take a dump, and run the
   drop recipe `db/testdata/upgrade/rollback-drop-5004.sql` from the source tree of the release you are leaving, as
   described in [../docs/schema-upgrade.md](../docs/schema-upgrade.md) ("Migration 5004"):

   ```bash
   docker compose stop artex
   set -a; . ./.env; set +a
   PGPASSWORD="$POSTGRES_PASSWORD" docker compose exec -T -e PGPASSWORD postgres \
     psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 < rollback-drop-5004.sql
   ```

   Then remove `ARTEX_SCHEMA_ALLOW_NEWER` from `.env` and `docker compose up -d`.
5. Remove the device you linked (the name you chose) under Linked devices on the phone, then delete the new volumes
   once you are sure: `docker volume rm example-deployment_artex-signal-data example-deployment_artex-signal-sock`.

**Without Signal, on this release:** take `docker-compose.signal.yml` out of `COMPOSE_FILE` and run
`docker compose -f docker-compose.yml up -d --remove-orphans`, which removes the `artex-signal` container and restarts
`artex` without the socket. Signal channels stay, and their deliveries fail visibly
(`Signal service is not configured: set ARTEX_SIGNAL_API (unix:/absolute/path)`) instead of vanishing; disable them.
Keep `artex-signal-data` if you want to come back with the same link; otherwise remove the device on the phone and
delete the volume. An ARTEX version without Signal support treats Signal channels as an unknown kind and marks their
deliveries failed; disable or delete them before going back to it.

### Incident: isolation check C12 appeared

Symptom: the isolation status is `degraded` with C12 in the log (`[isolation] WARNING ...`), in `/api/stats` and in
the UI banner. `not verified: socket absent` after a start, while the service has not opened its socket yet, is
expected: ARTEX checks again every 15 seconds for about an hour, after the first successful send and on every
**Send test message**, and the entry clears once the socket is there. Any other C12 detail means the tool user may reach the socket, and with it
send messages as the linked account:

1. **Cut the path now:** `docker compose stop artex-signal`. Signal deliveries fail (retried) until it is back.
2. **Find the cause:** the `artex-signal-sock` volume must be mounted in `artex` at `/run/artex-private/signal` only,
   and `/run/artex-private` must be root `0700`
   (`docker compose exec artex stat -c '%U %a %n' /run/artex-private /run/artex-private/signal`). Look for local edits
   of the Compose files that add `artex` to `signal-egress`, publish a port or move the mount.
3. **Check the delivery records and the account:** look for messages you did not send, and check Settings, Linked
   devices on the phone for devices you did not link.
4. **Restore:** fix the files, `docker compose up -d`, press **Send test message** on a Signal channel, and confirm
   that `/api/stats` and the log show no C12 before you rely on the channel again.

### Incident: ARTEX hangs at start or on writes

Every database role can take PostgreSQL advisory locks, and ARTEX uses fixed lock keys, for example while it starts.
A compromised service could hold them, and ARTEX then waits: at start, on company-scope changes, on finding evidence
writes and on retest scheduling. Statements on the Signal tables give up after 3 seconds with a lock timeout error
instead. If ARTEX hangs in one of those places with the Signal override in use, run `docker compose stop artex-signal`:
its database sessions end, the locks are released, and ARTEX goes on. Then look at `docker compose logs artex-signal`
before you start it again.

## Upgrading

1. Read the release notes and [../CHANGELOG.md](../CHANGELOG.md) for the target version.
2. Back up first (see Backups).
3. Change `ARTEX_VERSION` in `.env` (with the digest override, also `ARTEX_IMAGE_DIGEST`).
4. `docker compose pull && docker compose up -d`

Migrations run automatically when the new version starts (`[migrate]` lines in
`docker compose logs artex`). Details and failure modes: [../docs/schema-upgrade.md](../docs/schema-upgrade.md).

## Rollback

To roll back to an older release that still uses this layout (0.5.18m or later), set that version in `.env` and run `docker compose pull && docker compose up -d`. If the
newer version applied a schema migration, the older binary refuses to start with
`unknown migration applied`. To keep the service up while you prepare the right release or restore
the pre-upgrade dump, set `ARTEX_SCHEMA_ALLOW_NEWER` in `.env` to the four-digit version of the
newest migration the newer release added, for example `5003` when rolling back from 0.5.18m to
0.5.17m. This is a stopgap with real risks and it expires when a still newer migration exists;
restoring the pre-upgrade backup is the safer path. Read
[../docs/schema-upgrade.md](../docs/schema-upgrade.md) ("The override `ARTEX_SCHEMA_ALLOW_NEWER`")
first, and remove the setting afterwards. To go below 0.5.18m (for example to 0.5.17m, which needs
`ARTEX_SCHEMA_ALLOW_NEWER=5003`), use that release's own compose file from the repository, not this one. Going back
from the first release with the Signal service to 0.6.0m needs `ARTEX_SCHEMA_ALLOW_NEWER=5004`; with the Signal
override, follow [Rollback of the Signal service](#rollback-of-the-signal-service).

## Backups

Database dump (the database has no network, so this goes through `docker compose exec`; the
variables come from your `.env`):

```bash
set -a; . ./.env; set +a
PGPASSWORD="$POSTGRES_PASSWORD" docker compose exec -T -e PGPASSWORD postgres \
  pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" | gzip > "artex-db-$(date +%Y%m%d-%H%M%S).sql.gz"
```

Restore only full dumps into a freshly created database; see "Restores" in
[../docs/schema-upgrade.md](../docs/schema-upgrade.md). Restore command for a running stack:

```bash
gunzip -c artex-db-<timestamp>.sql.gz | PGPASSWORD="$POSTGRES_PASSWORD" \
  docker compose exec -T -e PGPASSWORD postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"
```

Data directory (`jwt.key`, workspaces, evidence, transcripts): with the default named volume,
archive it through a throwaway container (the volume is named `<project>_artexdata`; the project
is the directory name, `example-deployment`, unless you set `-p`/`COMPOSE_PROJECT_NAME`):

```bash
docker run --rm -v example-deployment_artexdata:/data:ro -v "$PWD":/backup alpine \
  tar czf /backup/artex-data-$(date +%Y%m%d-%H%M%S).tar.gz -C /data .
```

With `docker-compose.data-bind.yml` the data is the host directory `ARTEX_DATA_DIR`; stop the stack
(or accept a slightly inconsistent copy) and archive it as root. Keep backups private: they contain
secrets. Repository-level details: [../README.md](../README.md#backups). With the Signal override, read
[Backup of the account](#backup-of-the-account): the volume `artex-signal-data` is a live credential, and a database
dump needs care when it is restored into a fresh volume.

## Stop and remove

```bash
docker compose down        # keeps the volumes (database, data)
docker compose down -v     # also DELETES pgdata, pgsock and artexdata: all data is lost
```

With `docker-compose.signal.yml` in the file set, `down -v` also deletes `artex-signal-data`, the linked device's
keys: link again afterwards, and remove the old device under Linked devices on the phone.
