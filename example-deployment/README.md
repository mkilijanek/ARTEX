# ARTEX example deployment (published image)

A Docker Compose deployment that runs ARTEX from the published image
`ghcr.io/mkilijanek/artex`, pinned to one version. Nothing is built and no repository checkout
is needed on the target host: copy this directory, fill in `.env`, start.

It keeps the security layout of the repository's `docker-compose.yml`: PostgreSQL has no network
and does not listen on TCP (a Unix socket in a shared volume only), nothing but the web UI and the
capture proxy is published, both on `127.0.0.1` by default, and no capabilities are added or dropped
(tool isolation needs Docker's default set; see [../docs/tool-isolation.md](../docs/tool-isolation.md)).
Do not add `cap_drop: ALL` or `privileged`.

Requirements: Docker with the Compose plugin (v2.24 or newer, the overrides use `!override`), a
`linux/amd64` host. The release publishes `linux/amd64` images only; there is no native arm64 image.

**Versions older than 0.5.18m use a different compose layout (TCP PostgreSQL, different volumes)
and must not be used with this file.** Upgrade such an installation to 0.5.18m first by following
[../README.md](../README.md#updating-and-upgrading) and [../docs/schema-upgrade.md](../docs/schema-upgrade.md).

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
  `docker buildx imagetools inspect ghcr.io/mkilijanek/artex:v0.5.18m`. A digest cannot be moved,
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

Validate any combination without starting anything:

```bash
docker compose -f docker-compose.yml -f docker-compose.lan.yml config
```

## Upgrading

1. Read the release notes and [../CHANGELOG.md](../CHANGELOG.md) for the target version.
2. Back up first (see Backups).
3. Change `ARTEX_VERSION` in `.env` (with the digest override, also `ARTEX_IMAGE_DIGEST`).
4. `docker compose pull && docker compose up -d`

Migrations run automatically when the new version starts (`[migrate]` lines in
`docker compose logs artex`). Details and failure modes: [../docs/schema-upgrade.md](../docs/schema-upgrade.md).

## Rollback

Set the previous version in `.env` and run `docker compose pull && docker compose up -d`. If the
newer version applied a schema migration, the older binary refuses to start with
`unknown migration applied`. To keep the service up while you prepare the right release or restore
the pre-upgrade dump, set `ARTEX_SCHEMA_ALLOW_NEWER` in `.env` to the four-digit version of the
newest migration the newer release added, for example `5003` when rolling back from 0.5.18m to
0.5.17m. This is a stopgap with real risks and it expires when a still newer migration exists;
restoring the pre-upgrade backup is the safer path. Read
[../docs/schema-upgrade.md](../docs/schema-upgrade.md) ("The override `ARTEX_SCHEMA_ALLOW_NEWER`")
first, and remove the setting afterwards. Note that going below 0.5.18m means a different compose layout (see above).

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
secrets. Repository-level details: [../README.md](../README.md#backups).

## Stop and remove

```bash
docker compose down        # keeps the volumes (database, data)
docker compose down -v     # also DELETES pgdata, pgsock and artexdata: all data is lost
```
