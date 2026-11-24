# Tools base image

`docker/base/` builds the image that holds everything ARTEX needs at run time except ARTEX itself: apt
packages, Node, the recon binaries, the tool user and run directories, Playwright, and bbot with its
dependencies. The release `Dockerfile` starts from it by digest and adds the binary, the skills and the
start script.

| File | Purpose |
|---|---|
| `Dockerfile` | targets `slim` and `full` (`full` is `slim` plus one layer: Java, jadx, the C toolchain) |
| `check-tools.sh` | the tool matrix; run at the end of the build and again in a container |
| `inventory.sh` | prints what is installed and the hashes of the tool binaries |
| `setuid-expected.txt` | the setuid/setgid files the image may contain |
| `../base-inventory/` | the inventory of the pinned base, one file per variant |
| `../../.github/workflows/image.yml` | builds, checks and publishes the base |

## Build locally

The build context is this directory. Plan for **30 GB or more** of free disk (build cache, both
variants, and the layers of the Python base) and for a build of tens of minutes; the first build
downloads a lot.

```bash
docker build -f docker/base/Dockerfile --target slim -t artex-base:slim docker/base
docker build -f docker/base/Dockerfile --target full -t artex-base:full docker/base
```

Build `full` first or second, as you like: it contains every layer of `slim`, so the second build reuses
the cache of the first. To build as the workflow does (layers with timestamps rewritten to the commit
time, so that two builds of the same inputs have the same layers):

```bash
docker buildx build -f docker/base/Dockerfile --target full -t artex-base:full \
  --build-arg SOURCE_DATE_EPOCH="$(git log -1 --format=%ct -- docker/base)" \
  --output type=docker,rewrite-timestamp=true docker/base
```

`rewrite-timestamp` needs a buildx builder with the `docker-container` driver (`docker buildx create
--driver docker-container --use`), not the plain `docker` driver.

## Check and inventory in a container

The build runs `check-tools.sh` too, but a build sandbox has no `net.ipv4.ping_group_range`, so the
`ping` row is skipped there. Run the check in a container as well; it must end with `0 skipped`:

```bash
docker run --rm -v "$PWD/docker/base:/mnt/base:ro" --entrypoint bash artex-base:slim \
  /mnt/base/check-tools.sh slim
docker run --rm -v "$PWD/docker/base:/mnt/base:ro" --entrypoint bash artex-base:full \
  /mnt/base/check-tools.sh full
```

On a host with SELinux enforcing, a bind mount of the checkout is denied. Do not disable the
confinement. Copy the scripts into a stopped container instead, and start it:

```bash
for v in slim full; do
  ctr=$(docker create --entrypoint bash "artex-base:$v" /mnt/base/check-tools.sh "$v")
  docker cp docker/base/. "$ctr":/mnt/base
  docker start -a "$ctr"
  docker rm "$ctr"
done
```

(A `:z` relabel of a temporary copy of `docker/base` also works; never relabel the checkout itself.)
The last line of a good run is `check-tools: N passed, 0 failed, 0 skipped (variant slim)`.

The inventory is read from inside the image; compare it with the committed one:

```bash
docker run --rm -i --entrypoint sh artex-base:slim -s < docker/base/inventory.sh > /tmp/inventory-slim.txt
diff -u docker/base-inventory/slim.txt /tmp/inventory-slim.txt
```

## Publishing

The workflow `image.yml` builds and checks both variants on GitHub-hosted runners.

| Event | Result |
|---|---|
| pull request that changes `docker/base/**` or the workflow | build and check, **never pushes** |
| push to `main` with those paths | build, check, then publish |
| monthly schedule (1st, 05:00 UTC) | the same, to pick up Debian security updates |
| manual run (`workflow_dispatch`, input `push`) | build and check from any branch; **publishes only with `push: true` on `main`**, any other branch fails before it builds |

Publishing is the job `base-publish`. It runs only on `main`, only for non-PR events, and waits for the
owner's approval in the `base-publish` environment. It is the only job with permission to write packages
and attestations. It pushes both variants to `ghcr.io/mkilijanek/artex-base`, attaches an SBOM and a
provenance attestation to each, and writes both digests into the run summary.

The publish job rebuilds the images (cache hits) and compares their layers and configuration with the
images the build job checked; if they differ (for example the cache was evicted and an unpinned input
moved), it stops before pushing. Start the run again.

### Tags

`<variant>-<YYYYMMDD>-<short sha>`, for example `slim-20261124-6d843da` and `full-20261124-6d843da`.

- The date is the build date in UTC, the sha is the commit the run built. The two variants of one run
  share both.
- Tags are **write-once, enforced by the workflow**: before pushing either variant, the job checks that
  both tags are free and fails otherwise. GHCR itself does not prevent an overwrite, so this is not
  "immutable".
- There is no moving tag (`slim`, `latest`). Consumers pin by digest; the tag is only a label.
- If one variant fails to push after the other succeeded, the run fails. The orphan tag is harmless
  (nothing pins it); the next run has a new tag.

## Bump the pin in the release Dockerfile

A change to the tools takes two pull requests.

1. A pull request that changes `docker/base/**`. Its run builds and checks without pushing. After it
   merges, the push run on `main` waits for approval, then publishes.
2. A pull request that moves the two `FROM` lines of the release `Dockerfile` that name
   `ghcr.io/mkilijanek/artex-base` to the new `tag@sha256:<digest>` (both digests are in the publish
   run's summary). The same pull request must:
   - commit the new inventories from the run's `base-evidence` artifact to `docker/base-inventory/slim.txt`
     and `docker/base-inventory/full.txt`, and
   - carry the inventory diff (the run summary shows it against the currently pinned inventory) in the
     description.

Before merging, check the attestation of each digest. The release workflow does the same before it builds:

```bash
gh attestation verify oci://ghcr.io/mkilijanek/artex-base@<digest> \
  --repo mkilijanek/ARTEX \
  --signer-workflow mkilijanek/ARTEX/.github/workflows/image.yml \
  --source-ref refs/heads/main --deny-self-hosted-runners
```

`docker/base-inventory/` is outside `docker/base/**`, so committing an inventory does not start a base
rebuild.

### A bbot bump

bbot looks for its own build tools (gcc, make, libssl-dev, ...) at every start and tries to install them
when they are missing, which fails for the tool user. It skips that when a marker file, named by a hash of
its core dependency list, is in its cache directory; the build copies that marker (and the dependency
state file) from the shared tree to the tool home. A new bbot version can change the dependency list and
so the marker name, or the way it is checked. After changing `BBOT_VERSION`, re-verify the marker:

- the build still finds the marker files it copies (the `RUN` fails if none is found), and
- the `bbot --dry-run --no-deps` rows of `check-tools.sh` pass as the tool user, with no `Installing`,
  `ERRR` or `CRIT` line, in `slim` **and** in `full` (the C toolchain is back in `full`, the marker
  stays valid there).

## Inputs that are not pinned

Even with unchanged `docker/base/**` a rebuild can differ: `bbot --install-all-deps` (its own downloads
and apt choices), the NodeSource setup script and the Node 22.x minor, Debian packages (updated on purpose
by `apt-get upgrade`), and Playwright's `--with-deps` packages. The recon binaries, bbot, Playwright and
sqlmap are version-pinned but not hash-pinned. The inventory diff is where to review them.
