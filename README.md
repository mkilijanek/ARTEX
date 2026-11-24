<div align="center">

# ARTEX

AI autonomous penetration testing system (Go backend + Next.js frontend)


 Fork of [ARTEX by Autumn-27](https://github.com/Autumn-27) · English translation of the Chinese source · maintained publicly under AGPL-3.0

</div>

---

## Screenshots


| Dashboard (overview / token usage / activity stream) | Task list |
| :---: | :---: |
| ![Dashboard](screenshots/dashboard.png) | ![Tasks](screenshots/tasks.png) |

| Task · execution (sessions / tool calls) | Exploration graph |
| :---: | :---: |
| ![Execution](screenshots/sessions.png) | ![Exploration graph](screenshots/graph.png) |

| Findings | Assets |
| :---: | :---: |
| ![Findings](screenshots/findings.png) | ![Assets](screenshots/assets.png) |

| Asset coverage graph (force-directed layout · tested highlights · node collapse/expand) |
| :---: |
| ![Asset coverage graph](screenshots/assets_test.png) |

| Traffic recording | Human-in-the-loop chat |
| :---: | :---: |
| ![Traffic](screenshots/traffic.png) | ![Chat](screenshots/chat.png) |

| Agent management | LLM configuration |
| :---: | :---: |
| ![Agent](screenshots/agents.png) | ![LLM](screenshots/llm.png) |

| Interception approvals | Backend logs |
| :---: | :---: |
| ![Interception](screenshots/intercept.png) | ![Logs](screenshots/logs.png) |


---

## Approval record details

The global "Approval Records", the in-task "Interception Approvals", and the approval cards in conversations all support expanding to view details. The display structure references the
[AegisHook approval details component](https://github.com/RuoJi6/AegisHook/blob/main/web/src/components/CallDetail.vue), reusing ARTEX's components and theme:


## Traffic capture browser extension

The bundled browser extension (`extension/`) turns your own browsing into agent-visible evidence: log into an authenticated target once (captchas, email OTP and 2FA included), and the captured requests/responses land in ARTEX's traffic store like proxy-recorded traffic, ready to search and bind to findings:

- Load the unpacked extension from `extension/` (Chrome/Edge, Developer Mode → Load unpacked);
- In the popup, set your ARTEX URL and token, then press **Start capture** on the tab you are browsing;
- Use the site normally; text requests/responses are batched to `POST /api/traffic/ingest` (authenticated). Binary bodies (images, fonts) are skipped, bodies are truncated at 256 KiB, batches are capped at 50 entries;
- Press **Stop** when done. Everything stays in your local traffic store.

## Asset sync (ScopeSentry)

Sync asset data directly from [ScopeSentry](https://github.com/Autumn-27/ScopeSentry), avoiding duplicate collection:

- On the "**Asset Sync**" page, enter the ScopeSentry address and API Key to connect the data source;
- Choose the targets and asset types to sync by **project** or **task** (domains / subdomains / IPs / ports / sites / endpoints…);
- One-click import, merged by company asset scope, straight into ARTEX's asset graph for agents to explore.

---

## Installation

> Requires a **PostgreSQL** database; exploration requires an **LLM** (`ANTHROPIC_API_KEY` or `OPENAI_API_KEY`, which can also be configured in the UI).

### Option 1: one-click install script (recommended)

```bash
git clone https://github.com/BaaRai/ARTEX.git
cd ARTEX
./install.sh
```

The script will: detect / auto-install Docker → then let you choose **① all-Docker** or **② local build and run**:

- **① All-Docker**: enter a Postgres password (press Enter for a random one) → auto-writes `.env` → `docker compose up -d --build` (the image is built from this checkout).
- **② Local run**: choose a database (connect to an existing one / start one with Docker) → generates `config.json` → `go` builds the embedded single binary → starts.

Once installed, open **http://localhost:8787** (on first entry, `/setup` sets the admin password).
`/setup` asks for a one-time setup token: while no password is set, the backend prints it at startup in its log (`docker compose logs artex | grep "Setup token"`, or the terminal running `./start.sh`).

The web UI is published on `127.0.0.1` by default. To reach it from another machine, or from Windows when ARTEX runs inside WSL, set `ARTEX_BIND_ADDR=0.0.0.0` in `.env` (the setup token still protects the first-run claim), or keep loopback and use a reverse proxy or an SSH tunnel.

### Option 2: Docker Compose (manual)

```bash
git clone https://github.com/BaaRai/ARTEX.git
cd ARTEX
cp .env.example .env          # set POSTGRES_PASSWORD, optionally ANTHROPIC_API_KEY
# build the single binary first (frontend, then go build -tags embedui), see the Dockerfile header
docker compose up -d --build  # builds artex from this checkout + starts postgres
# → http://localhost:8787
```

> The image is built locally. There is no published image to pull: the previous
> default pointed at a third-party namespace that no longer hosts this project.

The image already includes common tools (ripgrep/curl/vim/npm/nmap…); `./skills` and `./data` are persisted via bind mounts.

In the image, agent tools (Bash, file tools, MCP stdio servers, custom tools) run as a separate unprivileged user
(`artex-tool`, uid 30002), not as the server's user, and the bundled PostgreSQL is reached only through a Unix socket.
The server checks this at every start and shows the result in its log and as a banner in the UI. Statuses, checks,
custom Compose files and limits: [docs/tool-isolation.md](docs/tool-isolation.md).

For remote MCP, you can choose `http` (Streamable HTTP) or `sse` (legacy SSE) in system settings.
Legacy SSE servers usually establish the event stream with `GET /sse`, then receive JSON-RPC requests via the
`/message?sessionId=...` returned by the server; when configuring, set the URL to `/sse` and the request header as
`Authorization=Bearer <token>`.

**Published image, no checkout:** [example-deployment/](example-deployment/) is a Docker Compose deployment that runs the pinned release image from GHCR, with optional overrides (digest pin, host data and skills directories, LAN publishing, resource limits).

### Option 3: download prebuilt binaries (Releases)

Download the zip for your platform from [Releases](https://github.com/BaaRai/ARTEX/releases); after extracting you get `artex` + `start.sh` (`start.bat` on Windows) + `skills/` + `config.example.json`:

```bash
cp config.example.json config.json   # fill in the database connection
./start.sh                           # → http://localhost:8787
```

> Please start with `start.sh` / `start.bat` instead of running `./artex` directly. It is a daemon script: if the process crashes, it restarts it after a backoff.
> To keep it running in the background: `nohup ./start.sh >artex.log 2>&1 &`.

> A binary install (options 3 and 4, and `install.sh` option 2) runs agent tools as your login user: they can read
> what that user can read, including this server's configuration, its database and your home directory. The tool
> isolation status is `unavailable` and the server warns at every start, every session and in the UI. Use the Docker
> image to run tools as a separate user ([docs/tool-isolation.md](docs/tool-isolation.md)).

### Option 4: build the single binary from source

```bash
# 1) frontend static export
cd web && npm ci && npm run build:static && cd ..
# 2) copy into the embed directory
cp -r web/out server/webui/dist
# 3) build (only -tags embedui embeds the frontend)
CGO_ENABLED=0 go build -tags embedui -o artex ./cmd/artex
./start.sh
```

### Option 5: build cross-platform release archives

`build.sh` first builds and embeds the frontend, then strips debug info via the Go linker, and compresses the release files into zips. Release mode generates zips for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 by default:

```bash
./build.sh --release
# output: dist/artex-0.3.3-*.zip
```

UPX self-extracting binaries may be incompatible with some Linux kernels, virtualization environments, or security policies, so they are disabled by default. Use `ARTEX_TARGETS` to customize the targets; when you have confirmed the target runtime is compatible, explicitly pass `--upx` to further shrink the binary:

```bash
ARTEX_TARGETS=linux/amd64,windows/amd64 ./build.sh --release
./build.sh --target linux/amd64 --upx
```

---

## Backups

`scripts/backup.sh` dumps the Postgres database (and optionally the data
directory) with timestamped output and rotation:

```bash
scripts/backup.sh --with-data          # db dump + data/ archive, keep 14
scripts/backup.sh --keep 7             # db only, keep 7
```

Restore only **full** dumps, into a freshly created database: a partial or data-only restore, or an edit of the
migration ledger table `schema_migrations`, is unsupported (see [docs/schema-upgrade.md](docs/schema-upgrade.md),
"Restores"). Restore a dump into a running stack (with the variables of `.env` in your shell; the database has no
network, so this goes through `docker compose exec`, and a database volume created with the current
`docker-compose.yml` requires the password):

```bash
gunzip -c backups/artex-db-<timestamp>.sql.gz | PGPASSWORD="$POSTGRES_PASSWORD" \
  docker compose exec -T -e PGPASSWORD postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"
```

Daily cron example (03:20, operator crontab):

```
20 3 * * * cd /path/to/ARTEX && scripts/backup.sh --with-data >> backups/backup.log 2>&1
```

## Updating and upgrading

> Upgrading only swaps the program and never touches data: the Postgres volume `pgdata`, `./data` (SQLite, workspaces, etc.), and `./skills` are all preserved. **No manual database migration is needed**: on every start `artex` re-runs the frozen `schema.sql` (with `ADD COLUMN` / `CREATE INDEX IF NOT EXISTS`) and then applies the numbered migrations in `db/migrations/` that are not yet recorded in the table `schema_migrations`, i.e. "restart equals migrate". **Back up the database (`scripts/backup.sh`) and `./data` before upgrading**, stop older instances, and do not run `pg_dump` during the upgrade: migration statements wait at most 60 seconds for a lock and then fail. The first start of a release with migrations writes the table `schema_migrations` (four baseline rows, one more for each later migration) and applies the migrations after them: `0004` builds the index `idx_llm_usage_ts` on `llm_usage` (writes to that table wait for the build, which runs before the server listens; for a large table pre-build it, see the page below), and `5002` adds the table `tool_rag_observations`, and `5003` adds the four detection tables (storage only: no API or UI yet; archiving a task that has detection runs is refused); going back to 0.5.14m or older needs nothing (they ignore the tables), going back to 0.5.15m needs `ARTEX_SCHEMA_ALLOW_NEWER=5003` (the override covers `0004`, `5002` and `5003`; use `5002` on a database that never ran `5003`) or the guarded recipes, and a binary that has the ledger but does not ship `5003` (0.5.17m) refuses the ledger entry `5003` unless it is started with `ARTEX_SCHEMA_ALLOW_NEWER=5003` (0.5.16m, which does not ship `5002` either, needs the same value). The start refuses a database it does not understand (for example one that a newer release migrated) before it writes anything; the message names the fix, and `ARTEX_SCHEMA_ALLOW_NEWER` is a capped override for keeping the service up. Details, refusals, rollback and what is not covered: [docs/schema-upgrade.md](docs/schema-upgrade.md).

### Upgrading from a clone made before 2026-10-23

The repository history was rewritten that day to drop a personal email address from the commit metadata. A clone made before then cannot fast-forward, and `./update.sh` now stops instead of silently rebuilding stale code. Recover once, then upgrade normally:

```bash
git stash                 # only if you have local changes
git fetch origin
git reset --hard origin/main
git stash pop             # only if you stashed
```

Your data is not affected: `./data`, the `pgdata` volume and `./skills` are not part of the git history.

### Option 1: one-click update script

```bash
cd ARTEX
./update.sh
```

The script first optionally `git pull`s the latest code, then lets you choose **① Docker update** or **② local build update** (matching `install.sh`):

- **① Docker**: rebuilds the image from this checkout → `docker compose build artex` → `docker compose up -d` (re-creates every service whose configuration changed, including `postgres`; restarting migrates the schema automatically). Afterwards it warns if the database volume still trusts local connections without a password, and says when that check was skipped because `postgres` is not running.
- **② Local**: rebuild the frontend static assets → recompile `./artex` (restart the process for it to take effect). It prints that a binary install runs agent tools as your login user.

### Option 2: Docker Compose (manual)

```bash
cd ARTEX
git pull                       # update compose / scripts
docker compose build artex     # rebuild the image from this checkout
docker compose up -d           # recreate the changed services → schema migrates automatically
docker image prune -f          # clean up old images (optional)
```

Bring up **all** services, not only `artex`: a release can change the `postgres` service too (the release after
0.5.17m removes its network), and `up -d artex` alone leaves the old database container running. Upgrade notes of
each release are in [CHANGELOG.md](CHANGELOG.md).

### Option 3: prebuilt binaries (Releases)

Download the new version's zip from [Releases](https://github.com/BaaRai/ARTEX/releases), stop the old process, then overwrite `artex` and `skills/` (keep your `config.json` and `data/`), and restart:

```bash
cp -r <extracted dir>/skills ./ && cp <extracted dir>/artex ./
./start.sh
```

### Option 4: build from source

```bash
git pull
cd web && npm ci && npm run build:static && cd ..
cp -r web/out server/webui/dist
CGO_ENABLED=0 go build -tags embedui -o artex ./cmd/artex
# restart ./start.sh
```

---

## Configuration

**Database** (`config.json`, or override with the `ARTEX_PG_DSN` environment variable):

```json
{
  "database": {
    "host": "127.0.0.1", "port": 5432,
    "user": "artex", "password": "yourpass",
    "dbname": "artex", "sslmode": "disable"
  }
}
```

**LLM**: `export ANTHROPIC_API_KEY=sk-...` (or `OPENAI_API_KEY`); you can also fill it in on the UI's "LLM Configuration" page — which can add a local Ollama server as a keyless provider (format "Ollama", base URL default `http://127.0.0.1:11434/v1`, models loaded from the server).
Optional: `ARTEX_LLM_PROVIDER` (anthropic | openai | openai-responses | ollama) / `ARTEX_LLM_MODEL` / `ARTEX_LLM_BASE_URL` / `ARTEX_LLM_PROXY`.

**Concurrency**: the number of work agents per task is configured in "System Settings" (default 3).

**Common flags**: `./start.sh -addr :8787 -proxy :8788` (`-addr` frontend+API, `-proxy` traffic recording proxy, `-egress` the local forwarder agent tools use to reach the global egress proxy, default `127.0.0.1:8789`, loopback only; empty disables it, and tools then have no route through a global egress proxy). The startup script passes arguments through to `artex` verbatim.

**Tool isolation**: `ARTEX_TOOL_ISOLATION=off` (environment only) makes agent tools run as the server's user, with warnings. See [docs/tool-isolation.md](docs/tool-isolation.md).

### Reverse proxy deployment (HTTPS / only exposing 443)

The frontend and API/SSE are served by the same backend port (default `:8787`), and the realtime activity stream uses the **same-origin** address by default, so **there is no need to configure `NEXT_PUBLIC_SSE_BASE`**: just expose 443 publicly and keep 8787 on the internal network.

SSE is a long-lived connection with continuous pushes, so the reverse proxy **must disable buffering**, otherwise the browser connects but never receives events (the activity stream spins forever). Nginx example:

```nginx
server {
    listen 443 ssl;
    server_name your.domain.com;
    # ssl_certificate / ssl_certificate_key ...

    location / {
        proxy_pass http://127.0.0.1:8787;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;

        # SSE essentials: buffering off, long timeouts, HTTP/1.1
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
    }
}
```

> Only when SSE needs a different origin from the page (e.g. a dedicated subdomain) should you set `NEXT_PUBLIC_SSE_BASE` **at build time** (this variable is baked into the static bundle during `next build`; setting it at container runtime has no effect).

Behind a reverse proxy every client reaches ARTEX from the proxy's address, so they all share one login throttle
(next section).

### Login throttling

`POST /api/auth/login` and `POST /api/auth/change-password` count failed password checks:

- **Per source.** The source is the address the connection comes from; `X-Forwarded-For` and other headers are never
  read. Loopback and every address of the server's own interfaces count as **one** source. IPv6 sources are counted
  per `/64`. Every other address is its own source.
- **Rule.** After 5 failures the source gets `429 Too Many Requests` with `Retry-After`, also for a correct password.
  The lock starts at 1 second and doubles with each further failure up to 15 minutes. A successful check resets the
  count; a count with no failure for 30 minutes is forgotten.
- **Order of a request.** A locked source (or device) gets `429` before its body is read. The body must be at most
  64 KiB (else `413 Request body too large`) and arrive within 10 seconds (else `400` and the connection is closed);
  neither counts as a failure, and neither does a malformed body.
- **Parallel attempts.** A check is counted from the start of the password comparison. A source (or device) with k
  failures left before its lock runs at most k comparisons at the same time; further parallel attempts reaching the
  comparison get `429` with `Retry-After` at once. After a lock ends, one comparison at a time is admitted.
- **Device cookie.** A successful login sets the cookie `artex_device` (HttpOnly, SameSite=Strict, Secure when ARTEX itself serves TLS (behind a
  TLS-terminating reverse proxy it is not marked Secure),
  90 days, a new value at every login). A request with a valid cookie is counted against that device alone, never
  against its source: a browser that logged in before can still log in while its source is locked, and its own wrong
  passwords answer `401` until the device's own count locks it.
- **No global lockout.** Failures from one source never lock out another source.
- State is in memory (a restart clears it), bounded to 10,000 sources. When a lock starts the log shows
  `[auth] source <address> is locked out of password checks for <duration> after repeated failures` (or
  `a device at source <address> ...`).

Behind a reverse proxy, or through the Docker gateway, all clients share one source, so wrong passwords from any of
them lock all cookie-less clients out for a while, and concurrent logins without a device cookie whose password comparisons
overlap beyond the source's remaining failures (5 when none failed) can briefly get `429` even with correct passwords. Log in from a browser that has the device cookie, or wait.

**Outbound User-Agent** (System settings, "Advanced settings"): the switch "Hide ARTEX user agent" is on by default. While it is on, ARTEX adds no identifying User-Agent of its own to LLM, enrichment, MCP, notify and custom-tool requests (Go's default applies). Two exceptions remain: WebFetch and WebSearch still send the Norma library's own User-Agent (`norma/0.4` and a Firefox string) until Norma gets a parameter (they are shown greyed out), and the API Recon scripts keep their built-in strings (`Mozilla/5.0 (spa-api-recon)` and `Mozilla/5.0 (spa-api-recon spider)`), which name the skill. When the switch is off, the checked components (LLM, Enrichment, MCP, Notify, Tools, API Recon) send `Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 ARTEX/<version> (+<component>)`, or the optional custom value (up to 256 printable characters of the 8-bit set, Latin-1, U+0020-U+007E and U+00A1-U+00FF without U+00AD, stored trimmed, as UTF-8, and sent as Latin-1 bytes, so "café" goes out as `63 61 66 E9`; some servers and WAFs refuse non-ASCII header bytes, so plain ASCII is the safest choice; it applies to every checked component; a profile's own User-Agent uses the same rule). A User-Agent set on an LLM profile, MCP server, notify channel or custom tool always wins. The settings page shows what each component sends now. Changes apply to new requests without a restart (an open MCP SSE stream keeps its value until it reconnects). If a stored value is unreadable or fails these rules at start, the server logs `[useragent] settings not applied` and runs as hidden. The settings keys are `user_agent_hidden`, `user_agent_custom` and `user_agent_components` on `/api/settings`.

`ARTEX_USER_AGENT` is managed by the server: the API Recon scripts read it, and any value an operator exports for the server is overwritten, or removed while no value applies, at start and on every change of these settings. It is not a secret. It is on the tool environment allow-list and also reaches other child processes of the server, such as MCP stdio servers and custom command tools.

### Signal notifications

Notification channels (System, Notifications) include **Signal**. ARTEX sends through its own Signal service, `artex-signal`: a separate container that runs signal-cli as a **linked device** of one Signal account, the way Signal Desktop is linked to a phone. A channel sends to **one Signal group** (recommended and preselected: members are managed in Signal, and ARTEX stores no personal phone numbers) or to one or more phone numbers in E.164 form, never both. Messages are plain text of at most 2000 bytes; a digest that does not fit continues in the next message. The channel holds only `recipients` and `recipient_type`; the sender is the account linked in the UI.

**The repository's root `docker-compose.yml` does not support Signal.** Use [example-deployment/](example-deployment/README.md#signal-notifications-optional) with its override `docker-compose.signal.yml`. Set up:

1. **Override.** Add `docker-compose.signal.yml` to the file set, set `ARTEX_SIGNAL_DB_PASSWORD` in `.env` (a long random value of letters and digits) and start. ARTEX creates the service's database role at start and logs `[signal] database role ready`. Details, and what to do when that line does not appear: the example deployment README.
2. **Link.** System, Notifications, card "Signal account", **Link account**: enter a device name (it is shown on the phone), your current password, and tick the acknowledgement. Scan the QR code with Signal on the phone (Settings, Linked devices, Link a new device) within 2 minutes.
3. **Confirm.** Type the last 6 digits of the phone number of the account you linked. Nothing is sent until you confirm, so a code scanned by the wrong phone cannot send. Wrong digits count like wrong passwords, and the third wrong attempt starts a wait of 10 seconds that doubles with each further wrong attempt, up to 15 minutes.
4. **Group.** In a Signal channel, choose "Group (recommended)" and pick the group from the list (**Refresh** reloads it from the account), or enter its `group.<id>` with "Enter id manually". The linked account must be a member of the group.
5. Press **Send test message**.

**Use a dedicated Signal account.** A linked device can read every message of the account; the link dialog asks you to acknowledge that. The service drops what it receives (no attachments, stories, avatars or stickers are downloaded) and stores and forwards nothing of it.

**Unlink** (card, **Unlink**, with your password) deletes the account data in the service only. signal-cli cannot remove a linked device from the account, so **also remove the device under Linked devices on the phone**. Until you do, the device's keys stay valid, and any copy of the service's data volume can still read and send as that account.

Server environment, read once at start: `ARTEX_SIGNAL_API` (the service socket, only `unix:/absolute/path`; the override sets it), `ARTEX_SIGNAL_DB_PASSWORD` (the password of the service's database role), and optionally `ARTEX_SIGNAL_DB_ROLE` (role name, default `artex_signal`) and `ARTEX_SIGNAL_DB_AUTH_ASSERTED=1` (external PostgreSQL only; see the example deployment README). `ARTEX_SIGNAL_NUMBER` is no longer used: when it is still set, the log shows `[notify] signal: ARTEX_SIGNAL_NUMBER is no longer used; link the account under System, Notifications`. An invalid `ARTEX_SIGNAL_API` logs `[notify] signal: ARTEX_SIGNAL_API is invalid ...` at start. Deliveries fail visibly instead of being dropped: while the service is not connected they are retried; while no account is linked or confirmed they are marked failed and can be retried from the delivery records. Requests to the service carry the User-Agent of the `Notify` component.

Agent tools cannot reach the service: `artex` shares no network with it, and the socket sits under `/run/artex-private`, which isolation check C12 guards ([docs/tool-isolation.md](docs/tool-isolation.md)). Design: [docs/adr/0011-signal-sidecar-with-database-config.md](docs/adr/0011-signal-sidecar-with-database-config.md). The service has been tested with a simulated signal-cli, and the production image has been started with the real signal-cli but without an account; delivery from a real account and the 2000-byte limit are not verified yet.

---



## Development

### Manual vulnerability retesting

The "Retest" tab on the task details page lets you page through the task's vulnerabilities, review past conclusions and evidence, and manually start a retest. After starting, the current tab keeps its place and shows a spinner with "Retesting"; once the fix is confirmed, the vulnerability status is updated accordingly.

Click "Retest" in the actions area of any row in the vulnerability list, or click "Start Retest" in the "Vulnerability Retest" area of the vulnerability details; fill in the optional fixed version, test conditions, or limits, and the system creates an independent retest agent session while keeping the current page. The flat list, group-by-task, and asset views all support this entry point; while a retest is running, a spinner and "Retesting" are shown: click to enter the corresponding session to watch, and it reverts to "Retest" when finished. Retesting does not require restarting the original scan task; conclusions are "Still reproducible", "Fixed", or "Cannot confirm", and each conclusion, evidence, and session link is saved in the vulnerability details.

On first start, the new backend pre-creates an editable "Vulnerability Retest" (`retester`) agent; you can configure its prompt, LLM, run budget, and tools in Agent management. It uses its bound LLM by default, or the global active configuration when unbound. When a retest session completes successfully with a "Fixed" conclusion, the system automatically sets the vulnerability's handling status to "Fixed"; running, failed, stopped, or other conclusions keep the original status. Original evidence and reports are always preserved. You can also manually select "Fixed" from the status dropdown. While the same vulnerability is being retested, the existing session is reused; after a stop, failure, or service restart, you can start it again.

### Finding ticket ID

Each vulnerability can carry an optional "Ticket ID" that points to an external ticketing system (one line of text, up to 128 characters, no format rule). Click the field in the Status card of the vulnerability details to edit it: Enter or leaving the field saves, Esc cancels, and an empty value clears it. The value is shown in the vulnerability list, included in JSON, CSV and Markdown exports and in task archives, and is never sent to agents or models or included in outgoing notifications. The API field is `ticket_id` on `PATCH /api/exploration/findings/{id}`.

### Finding comments

The "Comments" panel in the vulnerability details lets analysts add plain-text, append-only notes. Comments are attributed to the signed-in operator account, are kept across retests and status changes, travel with task archives, and are never sent to agents or included in exports. See [docs/finding-comments.md](docs/finding-comments.md) for the API, limits and rollback notes.

In this version, history is viewed via vulnerability details and sessions; it is not yet included in vulnerability report exports or task archive packages, and is not automatically linked to traffic captures. Demo mode only generates clearly labeled simulated records and never requests real targets.

### Running and testing locally

```bash
./dev.sh    # backend(:8787) + traffic proxy(:8788) + frontend next dev(:5173) → http://localhost:5173
```

- Backend: `go run ./cmd/artex` (without `-tags embedui`, the frontend is not embedded)
- Frontend: `cd web && npm run dev` (`/api` is reverse-proxied to the backend, with hot reload)
- Tests: `go test ./...`. Database tests take the database **only** from the `ARTEX_PG_DSN` environment variable (`postgres://USER:PASS@HOST:5432/DB?sslmode=disable`, a throw-away PostgreSQL 16 server) and never read `config.json`, so a test run cannot touch your own database; without the variable they skip, and the schema upgrade tests fail. Schema upgrade checks: `scripts/upgrade-gate.sh`, see [docs/schema-upgrade.md](docs/schema-upgrade.md).
- Mock preview (no backend): `cd web && NEXT_PUBLIC_MOCK=1 npm run dev`

---

## System architecture

ARTEX is an **LLM multi-agent-driven autonomous penetration testing system**: a Go monolithic backend (with embedded Next.js frontend) + PostgreSQL; agent capabilities are provided by the [`norma`](https://github.com/Autumn-27/norma) SDK (`agentcore` / `tool` / `permission` / `harness` / `memory` / `transcript`). At its core is a **dual-graph architecture**, plus two autonomy mechanisms built around it: **process-level information exchange between workers** and the **planner's multi-round shared todolist that stabilizes attack chains**.

### Overall layering

```mermaid
flowchart TB
  subgraph FE["Frontend Next.js (go:embed embedded single binary)"]
    UI["Dashboard · Tasks · Assets · Coverage graph · Traffic · Workspace · System settings"]
  end
  subgraph SRV["server (Go net/http)"]
    API["REST /api/*  JWT auth  SSE"]
    ENG["engine scheduling loop"]
    MGR["Manager  task/engine/store lifecycle"]
  end
  subgraph AG["agent (norma SDK)"]
    GO["goals  goal decomposition + scope extraction"]
    PL["planner  the planner (sole intent generator)"]
    WK["worker  executor ×N"]
    MA["mainagent  human-in-the-loop"]
  end
  subgraph DB["PostgreSQL"]
    AGRAPH["Asset graph  assets / companies / task_scope"]
    EGRAPH["Exploration graph  exploration_nodes / anchors / activity"]
  end
  subgraph SUB["Supporting subsystems"]
    PROXY["Traffic recording proxy  MITM + CA tracing"]
    GUARD["guard / intercept  tool approval gate"]
    ENR["enrich  DNS / HTTP async enrichment"]
    EXT["MCP · skills · memory · report"]
  end

  UI -->|HTTP| API
  API --> MGR --> ENG
  ENG --> PL
  ENG --> WK
  API --> MA
  API --> GO
  PL --> DB
  WK --> DB
  MA --> DB
  GO --> DB
  WK -->|"Bash / HTTP fully traced"| PROXY
  WK --> GUARD
  WK --> ENR
  PL -.-> EXT
  WK -.-> EXT
  MA -.-> EXT
```

| Layer | Responsibility |
| --- | --- |
| **Frontend** | Next.js static export, embedded into the single binary via `go:embed`; visualizes tasks/assets/exploration graph/coverage graph, human-in-the-loop chat |
| **server** | `net/http` routing + JWT auth + SSE; `Manager` owns the lifecycle of tasks, engines, and DB stores |
| **engine** | One `plannerLoop` + N worker goroutines per task; intent claiming, timeout/pause/drain |
| **agent** | goals / planner / worker / mainagent; `ToolSet` exposes the dual graphs as LLM tools |
| **db** | Postgres persistence of the dual graphs (pgx); the schema is embedded via `go:embed` and idempotently applied on every start |
| **Supporting** | Recording MITM proxy, approval gate, async enrichment, MCP/skills/memory/report |

### Dual-graph architecture: exploration graph + asset graph

The system splits "**what the goals are**" and "**how far testing has progressed**" into two mutually independent graphs linked by anchors:

- **Asset Graph (globally shared)**: a single source of truth for assets shared across tasks. Nodes are `root_domain / subdomain / ip / service / app / endpoint`, owned by companies; the parent-child relationships domain→subdomain→service→endpoint and the dedup keys are all computed by the program: agents only submit raw information.
- **Exploration Graph (per-task)**: the "thinking and advancing" process of one task. Nodes are `goal` / `intent` / `fact` / `finding (vulnerability)` / `hint`, connected by edges like `spawns / derived_from / yields / proves` into a **lineage chain**, answering "which directions derive from which facts, and what they produced".
- **The two graphs are linked by anchors**: `exploration_anchors(node_id, asset_id)` anchors intents/facts/vulnerabilities to concrete assets: so you can see which assets an exploration direction hits, and conversely look up from a given asset which intents tested it in this task and which facts were derived. This also powers **asset test coverage** and the **asset coverage graph** (in-scope assets + tested highlights).

```mermaid
flowchart LR
  subgraph EG["Exploration graph (per-task · advance chain)"]
    direction TB
    G["goal"]
    I1["intent A"]
    F1["fact"]
    I2["intent B"]
    FD["finding (vulnerability)"]
    G -->|spawns| I1
    I1 -->|yields| F1
    F1 -->|derived_from| I2
    I2 -->|proves| FD
  end
  subgraph AG["Asset graph (globally shared · source of truth)"]
    direction TB
    RD["root_domain"]
    SD["subdomain"]
    SV["service"]
    EP["endpoint"]
    RD --> SD --> SV --> EP
  end
  I1 -. anchor .-> SD
  F1 -. anchor .-> SV
  I2 -. anchor .-> EP
  FD -. anchor .-> EP
```

> Division of labor: the **planner** reads the exploration graph's situation, judges goals, and dispatches **intents** into the frontier only when there are uncovered new directions; a **worker** claims **one intent**, executes with real tools, writes new assets/facts/vulnerabilities back into both graphs, then stops. The asset graph is shared facts; the exploration graph is the per-task advance chain.

### Engine and intent lifecycle (the closed loop of one exploration)

The engine is an **event-driven** closed loop: any graph change wakes the planner, the planner dispatches intents, workers claim and execute intents and write back, and the write-back triggers the next round, until the goal is proven (`prove_goal`).

```mermaid
sequenceDiagram
  autonumber
  participant EV as graph-change debounce
  participant P as planner
  participant FR as frontier intent queue
  participant W as worker
  participant PX as recording proxy
  participant DB as dual graphs + activity

  EV-->>P: wake up
  P->>DB: read situation (graph_overview prefetch + coverage/scope)
  P->>FR: dispatch 0..N intents (with asset_ids)
  Note over P,FR: most wake-ups dispatch 0 (no new direction means done)
  W->>FR: claimNext claims one intent
  W->>DB: fetch the raw assets of the intent's asset_ids as initial information
  W->>PX: real tool execution (Kali / Bash / HTTP)
  PX-->>W: response (fully traced + CA verified)
  W->>DB: write back fact / asset / finding + per-step activity
  DB-->>EV: graph change
  EV-->>P: wake again (closed loop)
```

### Process-level information exchange between workers

During a deep exploration, many valuable observations (an error, a response snippet, a hidden parameter) appear in a worker's **execution process** but may never be written as formal facts. To avoid duplicate work and let workers on the chain stand on each other's shoulders, workers can **search processes across works**:

- `search_all_worker_traces(q)`: keyword-search the **execution processes of other works in this task** (automatically excluding the steps of your own intent); hits carry `intent_id`;
- `list_worker_traces` / `get_worker_trace(intent_id, step_ids=[…])`: first see which works have run, then fetch the full content of specific steps of a given work for detailed exchange.

This way, even before a corresponding fact exists on the exploration graph, later workers can reuse observations from others' processes: **information flows between workers at "execution process" granularity**, while the boundary stays the same (each worker still only works on the intent it claimed).

```mermaid
flowchart LR
  WA["worker A (intent #12)"] -->|"per-step activity"| ACT[("exploration graph · activity process store")]
  WB["worker B (intent #34)"] -->|"per-step activity"| ACT
  WC["worker C (intent #56)"] ==>|"1) search_all_worker_traces(q)"| ACT
  ACT ==>|"2) hits on A/B steps (self excluded)"| WC
  WC ==>|"3) get_worker_trace(id, step_ids)"| ACT
  ACT ==>|"4) returns full process content"| WC
```

### The planner's multi-round shared todolist → stable attack chains

Real attack chains are usually **multi-step sequences with dependencies** (e.g.: find an injection point → obtain credentials → move laterally → escalate privileges); dispatching them all in parallel at once would only cause chaos. The planner therefore keeps a **planning todolist that is retained per task and shared across wake-ups**:

- The planner is event-driven, woken on every graph change, but **each wake-up is a brand-new session**; the shared todolist lets it **record a serial exploitation chain once**, then **dispatch intents step by step by dependency** over subsequent rounds, instead of pre-expanding the whole chain in a single round;
- Each round only dispatches intents for next steps whose "prerequisite steps are complete and the facts they depend on exist", and updates the list as progress happens (marking steps satisfied by facts as done).

```mermaid
flowchart TB
  subgraph TODO["Shared todolist (retained per task · persists across wake-ups)"]
    direction LR
    T1["1 injection point  [done]"]
    T2["2 get credentials  [in progress]"]
    T3["3 lateral movement  [pending prerequisite]"]
    T4["4 privilege escalation  [pending prerequisite]"]
    T1 -.prerequisite met.-> T2 -.-> T3 -.-> T4
  end
  R1["round 1 wake-up  dispatch intent ①"] --> T1
  R2["round 2 (① produced a fact)  dispatch intent ②"] --> T2
  R3["round 3 (② produced a fact)  dispatch intent ③"] --> T3
```

Thus, even in an "event-driven + stateless session" environment, the attack chain still **advances steadily, without repetition or reordering**: this is the key to ARTEX autonomously completing multi-step exploitation chains.

---

## References

https://github.com/oritera/Cairn


## License and disclaimer

### Open source license

This project is licensed under the **GNU Affero General Public License v3.0 (AGPL-3.0)**; see the [LICENSE](LICENSE) file in the repository root for the full terms.

This means anyone may freely use, modify, and distribute this project, but **derivative works must also be open-sourced under AGPL-3.0**; in particular, **if you modify this project and provide it to users over a network (e.g. deployed as an online service), you must also make the corresponding complete source code available to those users**.

> ⚠️ **Important**: the open source license itself does not restrict how the software is used. The "Usage restrictions" and "Disclaimer" below are additional terms and a solemn statement from the maintainers to users, so please abide by them.

**ARTEX is an offensive-security tool. Use it only against systems you own, or that you are explicitly authorized to test (your own lab, your organization's assets, or a bug bounty / vulnerability disclosure program's targets within its published scope and rules of engagement). Any use without authorization is a violation of these terms.**

### Permitted use

- **Read, learn, and study this project's source code**;
- Run it against **systems you own** or in **isolated lab environments**;
- Run it against **authorized bug bounty / vulnerability disclosure program targets**, within their published scope and rules of engagement;
- Academic research and code review.

### Prohibited actions

- **It is strictly forbidden to scan, probe, exploit, or attack any website, online service, or networked system without the explicit authorization of its owner**;
- It is strictly forbidden to test outside a program's published scope, or in violation of its rules of engagement;
- It is strictly forbidden to use this tool for illegal intrusion, data theft, extortion, denial of service, or any destructive or criminal activity;
- It is strictly forbidden to use this tool for anything that violates the laws and regulations of your country/region.

### Compliance responsibility

Users must comply with all laws and regulations of their country/region regarding cybersecurity, data protection, and computer crime. **All legal liability and consequences arising from the use of this tool are borne by the user.**

### Disclaimer

This project is provided "as is", without any express or implied warranty. The authors and contributors are not liable for any direct or indirect loss, data loss, system damage, or legal disputes caused by the use of this tool (whether used properly or not). **Downloading, installing, or using this project means you have read, understood, and agreed to all of the above terms.**

ARTEX_UNTRUSTED_BOUNDARY defaults to enabled. Set ARTEX_UNTRUSTED_BOUNDARY=0 to disable SDK tool-output framing; summary trust rules still apply.

ARTEX_DATA_ENVELOPE defaults to enabled. It frames agent- and target-derived text in the planner input, the worker launch and the digest compressor request as `[[ARTEX_DATA nonce=… field=…]]` blocks, and ends the planner input with a goal anchor. Set ARTEX_DATA_ENVELOPE=0 (or false/off/no) to disable both; the code-owned untrusted-content and data rules in the prompts still apply. This marks data for the model and does not prevent prompt injection; see `SECURITY.md`.

ARTEX_TOOL_RAG defaults to observe (unset, empty or `observe`). For every planner round and worker run, ARTEX ranks the bound tool list with BM25 and records, next to the tools the model actually called, what a filter would have kept. The tool list the model receives is never changed and nothing is blocked; there is no filter mode in this release. Rows hold counts and tool names only (no goal, query, description or result) and go with their task. Set ARTEX_TOOL_RAG=off (or 0, false, no) to switch it off; `1`, `true`, `on`, `yes` and `filter` still only observe and log one warning. ARTEX_TOOL_RAG_TOPK (default 12) sets the k the rows compare against. `docker-compose.yml` passes both variables from `.env`; an empty value means the default. Details, failure behaviour, the check after 14 days and rollback: [docs/tool-rag-observation.md](docs/tool-rag-observation.md).
