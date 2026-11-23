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
| `docker-compose.signal.yml` | Signal notifications: a signal-cli REST bridge and an allowlisting nginx proxy, reachable by ARTEX only through a Unix socket (see [Signal notifications](#signal-notifications-optional)) |
| `signal-proxy.conf` | the nginx configuration of that proxy (used by `docker-compose.signal.yml`) |

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
- **signal**: needs `ARTEX_SIGNAL_NUMBER` in `.env` (Compose refuses to start without it) and a Signal account on the
  bridge. Adds the services `signal-cli` and `signal-proxy`, the networks `signal-internal` and `signal-egress` and
  the volumes `signal-data` and `signalsock`; `artex` then waits at start until the bridge is healthy (tens of
  seconds). Setup, limits and runbooks: [Signal notifications](#signal-notifications-optional).

Validate any combination without starting anything:

```bash
docker compose -f docker-compose.yml -f docker-compose.lan.yml config
```

## Signal notifications (optional)

ARTEX can send finding notifications to one Signal group or to Signal phone numbers through a self-hosted
[signal-cli REST bridge](https://github.com/bbernhard/signal-cli-rest-api). `docker-compose.signal.yml` runs it.
It needs ARTEX 0.5.19m or later: set `ARTEX_VERSION=v0.5.19m` (or newer) in `.env`, since older images have no
Signal channel.
The repository's root `docker-compose.yml` has no Signal support; use this directory.

The Signal channel has been verified against the bridge API without a Signal account. Delivery from a real account
and the 2000-byte message limit have not been verified yet. The bridge's own endpoints used in this section through
`docker compose exec signal-cli curl ...` (register, verify, PIN, QR link, group list, rate-limit challenge, device
list and unlink) were checked against the bridge's source, not run with a real account; the bridge's API
documentation is authoritative for the version you run.

### Layout and why

The bridge has no authentication, and its API does far more than send: it can link a new device to the Signal
account (which then reads all messages), register or unregister the number, and manage groups. Agent tools run in
the `artex` container's network namespace (as uid 30002, see
[../docs/tool-isolation.md](../docs/tool-isolation.md)), so anything `artex` can reach over the network, tools can
reach too. The override therefore gives `artex` **no network path to the bridge**:

| Part | What it is |
|---|---|
| `signal-cli` | the bridge (`bbernhard/signal-cli-rest-api`, `MODE=json-rpc`, port 8080, never published). Networks `signal-internal` and `signal-egress` |
| `signal-proxy` | nginx with `signal-proxy.conf`, read-only root filesystem. Listens only on the socket `api.sock` in the volume `signalsock`; network `signal-internal` |
| `signal-internal` | `internal: true` (no route out); only the proxy and the bridge |
| `signal-egress` | Internet access for the bridge alone (Signal's servers) |
| `signalsock` | the socket volume, mounted in `artex` at `/run/artex-private/signal`. `/run/artex-private` is root 0700, so tools cannot traverse it (check C12); the proxy also sets the socket to root 0600 |
| `signal-data` | the bridge's account keys (`/home/.local/share/signal-cli`), private SELinux label `:Z` |

`artex` is on neither network: it cannot resolve `signal-cli` and cannot route to its address. The override sets
`ARTEX_SIGNAL_API=unix:/run/artex-private/signal/api.sock` and passes `ARTEX_SIGNAL_NUMBER` from `.env`.

The proxy forwards exactly two requests: `POST /v2/send` (body at most 64 KiB, larger gets `413`) and
`GET /v1/about`. Everything else, including device linking, registration and the group list, gets `403` from nginx
and never reaches the bridge. The query string is dropped. nginx decodes and normalises the path first, so an encoded
form such as `/v1/devices/X%2f..%2f..%2f..%2fv2%2fsend` is just a send to `/v2/send`. `HEAD /v1/about` passes the
proxy and gets `404` from the bridge. Account management is therefore done only from the host, with
`docker compose exec signal-cli ...`.

**Limits of this protection.**

- Users on the Docker host can reach the bridge's container address (Docker routes container addresses from the
  host). This is accepted: access to the host already means access to Docker. Restrict it with a host firewall if
  that matters to you.
- The separation relies on Docker isolating user-defined bridge networks from each other. Tested with Docker
  (iptables and firewalld backend). Docker with `"iptables": false`, and Podman, do not enforce it, and the layout then
  loses its protection. Run the check below after any change of the container engine or firewall.

Check the layout from inside `artex` as the tool user (both must fail):

```bash
# 1. the socket: expect PermissionError
docker compose exec -u 30002:30002 artex python3 -I -S -c \
  'import socket; socket.socket(socket.AF_UNIX).connect("/run/artex-private/signal/api.sock")'
# 2. TCP to every address of the bridge: expect "not reachable"
for ip in $(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$(docker compose ps -q signal-cli)"); do
  docker compose exec -u 30002:30002 artex python3 -I -S -c '
import socket, sys
try:
    socket.create_connection((sys.argv[1], 8080), timeout=3)
    print(sys.argv[1], "REACHABLE: the layout is broken")
except OSError as e:
    print(sys.argv[1], "not reachable:", e)' "$ip"
done
```

Also check that `docker compose logs artex | grep "\[isolation\] status="` still says `enforced` and that no C12
appears.

### Set up

The commands below assume the override is in the file set: put
`COMPOSE_FILE=docker-compose.yml:docker-compose.signal.yml` in `.env` (see
[Choosing and combining overrides](#choosing-and-combining-overrides)). Replace `+15555550100` with your sender number.

1. **Sender number.** A dedicated number for ARTEX is recommended. Set it in `.env` in E.164 form:
   `ARTEX_SIGNAL_NUMBER=+15555550100`.
2. **Start:** `docker compose pull && docker compose up -d`. The bridge starts without an account; `artex` waits
   until the bridge is healthy (its JVM takes tens of seconds).
3. **Account**, one of:
   - **Register the dedicated number** on the bridge. Registration usually needs a captcha token from Signal
     (`signalcaptcha://...`; see the bridge's documentation). Then verify with the code Signal sends to that number,
     and set a registration-lock PIN:

     ```bash
     docker compose exec -T signal-cli curl -fsS -X POST -H 'Content-Type: application/json' \
       -d '{"captcha":"signalcaptcha://<token>"}' 'http://127.0.0.1:8080/v1/register/+15555550100'
     docker compose exec -T signal-cli curl -fsS -X POST \
       'http://127.0.0.1:8080/v1/register/+15555550100/verify/<code>'
     docker compose exec -T signal-cli curl -fsS -X POST -H 'Content-Type: application/json' \
       -d '{"pin":"<pin>"}' 'http://127.0.0.1:8080/v1/accounts/+15555550100/pin'
     ```

     Keep the PIN with the backups: without it, re-registering the number can be locked.
   - **Link the bridge as a secondary device** of an existing Signal account. Messages then come from that account.
     Write the QR code to a file and scan it in the Signal app (Settings, Linked devices):

     ```bash
     docker compose exec -T signal-cli curl -fsS 'http://127.0.0.1:8080/v1/qrcodelink?device_name=artex' > qr.png
     ```

     The primary phone must stay in use, or Signal eventually unlinks the device.

   Then restart the bridge so it loads the account: `docker compose restart signal-cli`. These endpoints were checked
   against the bridge's source, not run with a real account; the bridge's API documentation is authoritative for the
   version you run.
4. **Channel in ARTEX** (System, Notifications): kind "Signal". **A group is recommended** and preselected: members
   are managed in Signal, and ARTEX stores no personal phone numbers.
   - **Group.** Create the group in Signal and add the sender number to it. Get the group id (this group-list request
     is for the operator only and is deliberately not reachable through the proxy):

     ```bash
     docker compose exec -T signal-cli curl -fsS 'http://127.0.0.1:8080/v1/groups/+15555550100'
     ```

     Take the `id` field of your group (it starts with `group.`), choose "Recipient type" "Group (recommended)" and
     enter that id as the one recipient.
   - **Phone numbers.** Choose "Phone numbers" and enter one or more numbers in E.164 form (`+15555550101`).
5. Press **Send test message**. A failure shows the error text (see the table below).

### Channel rules

- Config keys: `recipients` (list) and `recipient_type` (`group` or `numbers`). Without `recipient_type` (API clients),
  the list itself decides. There is no sender, URL or secret in the channel: the sender is `ARTEX_SIGNAL_NUMBER` and
  the endpoint is `ARTEX_SIGNAL_API`, both server environment, read once at start (change them, then restart `artex`).
- A group is exactly one `group.<id>`; numbers are one or more E.164 numbers. Numbers and a group cannot be mixed in
  one channel, and a channel takes one group only: use one channel per group. Duplicates are removed when sending.
  Signal usernames and UUIDs are not supported.
- Default rate: 20 messages per minute.
- Messages are plain text, at most 2000 bytes each. A digest that does not fit sends the remaining findings in the
  next message; one over-long finding is cut. No attachments and no link previews.
- Delivery is at least once: when the bridge has already passed a message to Signal but ARTEX times out (25 seconds)
  before the answer, ARTEX sends it again and recipients see it twice.
- Phone numbers and group ids are masked in ARTEX's errors, delivery history and logs (`…0102`, `group.…abcd`). The
  bridge's own log (`docker compose logs signal-cli`) shows them.

### Errors

| The error contains (delivery history, test result, `[notify]` log) | Meaning | Retried |
|---|---|---|
| `Signal bridge socket unreachable (is docker-compose.signal.yml up?)` | the proxy or bridge is not running, or the override is not in the file set | yes |
| `Signal rate limit: ...` | Signal asks for a rate-limit challenge; see below | yes |
| `Signal sender is not configured: set ARTEX_SIGNAL_NUMBER` | `ARTEX_SIGNAL_NUMBER` is unset (also shown when both variables are unset) | no |
| `Signal sender is not configured: ARTEX_SIGNAL_NUMBER must be an E.164 number ...` / `Signal bridge is not configured: ...` | an invalid value; at start the log has `[notify] signal: <VARIABLE> is invalid ...` (a number shown masked) | no |
| `peer rejected the request (HTTP 400): <bridge reason>` | the bridge refused the send (unregistered sender, unknown group, expired client, ...) | no |
| `delivered to K of N; failed: …0102 (<reason>)` / `delivered to the group, but failed for M member(s): ...` | a partial delivery; sending again would repeat it to those who got it | no |
| `Signal bridge returned an unexpected success response (HTTP <code>)` | the bridge answered 2xx without confirming a send | no |

The bridge answers `400` for transient failures as well, so those are not retried automatically: retry them from
the delivery records on the Notifications page once the cause is gone.

**Rate-limit challenge.** ARTEX keeps the challenge token out of its errors. To get it, send one message from the
host and read the `429` answer; then solve a captcha (see the bridge's documentation) and submit both:

```bash
docker compose exec -T signal-cli curl -sS -X POST -H 'Content-Type: application/json' \
  -d '{"number":"+15555550100","recipients":["+15555550100"],"message":"challenge"}' 'http://127.0.0.1:8080/v2/send'
docker compose exec -T signal-cli curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"challenge_token":"<token>","captcha":"signalcaptcha://<captcha>"}' \
  'http://127.0.0.1:8080/v1/accounts/+15555550100/rate-limit-challenge'
```

### Upgrading the bridge

Both images are pinned by tag and digest (`SIGNAL_CLI_IMAGE`, `SIGNAL_PROXY_IMAGE`; defaults in
`docker-compose.signal.yml`, commented in `example.env`). Signal stops accepting old client versions after some
months, and sends then fail with `HTTP 400`, so update the bridge regularly:

1. Read the bridge's release notes. Get the new digest:
   `docker buildx imagetools inspect bbernhard/signal-cli-rest-api:<tag>`.
2. Set `SIGNAL_CLI_IMAGE=bbernhard/signal-cli-rest-api:<tag>@sha256:<digest>` in `.env`.
3. `docker compose pull && docker compose up -d`, then press **Send test message** on a Signal channel.

`docker compose` may print `mount of type volume should not define bind option` for the `:z`/`:Z` labels on named
volumes (the base file's `pgsock` too). It is harmless.

### Backup of the account

`signal-data` holds the account keys: whoever has a copy can send, and read new messages, as that number. Back it up
and keep the copy as private as `.env`. The volume carries a private SELinux label, so the throwaway container runs
without label confinement (`--security-opt label=disable`), as root, and does not relabel the volume. The volume name
is `<project>_signal-data` (`docker volume ls | grep signal-data`):

```bash
docker run --rm --security-opt label=disable -v example-deployment_signal-data:/d:ro -v "$PWD":/backup alpine \
  tar czf /backup/signal-data-$(date +%Y%m%d-%H%M%S).tar.gz -C /d .
```

Restore with the bridge stopped (`docker compose stop signal-proxy signal-cli`), into the same volume, then start it
again (`docker compose up -d`):

```bash
docker run --rm --security-opt label=disable -v example-deployment_signal-data:/d -v "$PWD":/backup:ro alpine \
  tar xzf /backup/signal-data-<timestamp>.tar.gz -C /d
```

### Rollback of the override

Run without it and remove its two containers. Keep `signal-data` (the account); `signalsock` can go. If `.env` sets
`COMPOSE_FILE`, take `docker-compose.signal.yml` out of it. Keep `ARTEX_SIGNAL_NUMBER` in `.env` until the second
command has run (Compose refuses the override file without it).

```bash
docker compose -f docker-compose.yml up -d
docker compose -f docker-compose.yml -f docker-compose.signal.yml rm -sf signal-cli signal-proxy
```

ARTEX keeps running. Signal channels stay, and their deliveries fail visibly (`Signal sender is not configured: set
ARTEX_SIGNAL_NUMBER`, because the base file does not pass the variable) instead of vanishing; disable them. An ARTEX version without Signal
support treats them as an unknown kind and marks their deliveries failed; disable or delete Signal channels before
going back to it.

### Incident: isolation check C12 appeared

Symptom: the isolation status is `degraded` with C12 in the log (`[isolation] WARNING ...`), in `/api/stats` and in
the UI banner. `not verified: socket absent` right after a start with the proxy down is expected and clears after the
next send or **Send test message**. Any other C12 detail means the tool user may reach the bridge, which allows a
takeover of the Signal account:

1. **Cut the path now:** `docker compose stop signal-proxy`. The socket is the only way to the bridge; Signal
   deliveries fail (retried) until it is back.
2. **Find the cause:** the `signalsock` volume must be mounted at `/run/artex-private/signal` only, and
   `/run/artex-private` must be root `0700`
   (`docker compose exec artex stat -c '%U %a %n' /run/artex-private /run/artex-private/signal`). Look for local edits
   of the Compose files that add `artex` to a `signal-*` network, publish a port or move the mount.
3. **Check the account for unknown linked devices**, and remove every one you did not link:

   ```bash
   docker compose exec -T signal-cli curl -fsS 'http://127.0.0.1:8080/v1/devices/+15555550100'
   docker compose exec -T signal-cli curl -fsS -X DELETE 'http://127.0.0.1:8080/v1/devices/+15555550100/<deviceId>'
   ```

   With a linked (secondary-device) setup, also check Settings, Linked devices on the primary phone.
4. **Restore:** fix the files, `docker compose up -d`, press **Send test message** on a Signal channel, and confirm
   that `/api/stats` and the log show no C12 before you rely on the channel again.

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
`ARTEX_SCHEMA_ALLOW_NEWER=5003`), use that release's own compose file from the repository, not this one.

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
secrets. Repository-level details: [../README.md](../README.md#backups). With the Signal override, also back up
the account volume `signal-data` ([Backup of the account](#backup-of-the-account)).

## Stop and remove

```bash
docker compose down        # keeps the volumes (database, data)
docker compose down -v     # also DELETES pgdata, pgsock and artexdata: all data is lost
```

With `docker-compose.signal.yml` in the file set, `down -v` also deletes `signal-data`, the Signal account: the number
must then be registered or linked again (and a registration lock needs its PIN).
