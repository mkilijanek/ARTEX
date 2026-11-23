# syntax=docker/dockerfile:1
#
# Multi-stage build: `docker compose up -d --build` (or `docker build .`) produces
# the complete image from source. No prebuilt binary in the build context is needed,
# so a fresh `git clone` builds — dist/ is git-ignored and is no longer required.
#
#   frontend (web) -> Go binary with embedded UI (build) -> runtime image
#
# The Go binary is produced by build.sh with the same flags the release workflow's
# binaries job uses, so the image binary and the published release binary come from
# one source.

# ---------------------------------------------------------------- frontend ---
# Pinned to the Node 22 line the CI/release workflows use. On the build platform
# only; the frontend static export is architecture-independent.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build:static

# ------------------------------------------------------------- Go binary ---
# Pinned to the Go 1.26 line (matches go.mod and the release binaries job). The
# binary is CGO-free, so this stage runs on the build platform and cross-compiles
# to $TARGETARCH without QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/norma/ third_party/norma/
RUN go mod download
COPY . .
# Reuse the frontend built above; build.sh skips its own frontend step.
COPY --from=web /src/web/out ./server/webui/dist
ARG ARTEX_VERSION=dev
# The binary must stay static (no dynamic loader at run time); a future change to
# build.sh must not enable cgo silently. The check step below enforces the result.
ENV CGO_ENABLED=0
ARG TARGETOS
ARG TARGETARCH
# Same flags as the release binaries job (ARTEX_COMPRESS=0, -trimpath,
# -X main.version), driven through build.sh so there is one build path.
RUN ARTEX_SKIP_FRONTEND=1 \
    ARTEX_SKIP_NPM_CI=1 \
    ARTEX_COMPRESS=0 \
    ARTEX_OUTPUT=/out/artex \
    ARTEX_BUILD_VERSION="${ARTEX_VERSION}" \
    bash ./build.sh --target "${TARGETOS:-linux}/${TARGETARCH:-amd64}"
# Fail the build if the binary is not static: no INTERP program header, no NEEDED entry.
# The readelf output is captured first so that a missing readelf fails the step instead of
# passing an empty pipeline.
RUN set -eu; \
    ph="$(readelf -lW /out/artex)"; \
    dyn="$(readelf -dW /out/artex)"; \
    printf '%s\n' "$ph" | grep -q 'Program Headers'; \
    if printf '%s\n' "$ph" | grep -Eq '^[[:space:]]+INTERP[[:space:]]'; then \
      echo "/out/artex has an INTERP program header: it is not static" >&2; exit 1; fi; \
    if printf '%s\n' "$dyn" | grep -q '(NEEDED)'; then \
      echo "/out/artex has NEEDED entries: it is not static" >&2; exit 1; fi; \
    echo "/out/artex is static"

# ---------------------------------------------------------------- runtime ---
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a set of recon staples (add/remove as needed).
# Node 22.x comes from NodeSource: bookworm's apt nodejs is 18, Playwright requires >=20,
# and 20 is end-of-life. 22 matches the CI and release workflows.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Web-recon toolbox (drost-community take 1, #100): pinned release binaries installed
# into /usr/local/bin so hunt workers can run them directly through their Bash tool.
# Versions pinned per tool; upgrade deliberately. sqlmap is a python checkout of a
# pinned release tag (no release binaries) symlinked into PATH.
ARG NUCLEI_VERSION=v3.11.1
ARG HTTPX_VERSION=v1.12.0
ARG SUBFINDER_VERSION=v2.17.0
ARG KATANA_VERSION=v1.8.0
ARG FFUF_VERSION=v2.3.0
ARG DALFOX_VERSION=v3.2.4
# vulnx is ProjectDiscovery's vulnerability-intelligence CLI, the successor of cvemap
# (the cvemap repository was renamed; the binary is `vulnx`).
ARG VULNX_VERSION=v2.0.2
ARG SQLMAP_VERSION=1.10.11
RUN set -eux; \
    base="https://github.com"; \
    pd_fetch() { repo="$1"; ver="$2"; name="$3"; nov="${ver#v}"; \
      curl -fsSL "$base/$repo/releases/download/$ver/${name}_${nov}_linux_${TARGETARCH}.zip" -o /tmp/t.zip \
      && unzip -o -q /tmp/t.zip -d /tmp/td \
      && find /tmp/td -type f -name "$name" -exec install -m 0755 {} /usr/local/bin/ \; \
      && rm -rf /tmp/t.zip /tmp/td; }; \
    case "$TARGETARCH" in amd64) FFUF_ARCH=amd64; DALFOX_ARCH=x86_64;; arm64) FFUF_ARCH=arm64; DALFOX_ARCH=aarch64;; esac; \
    pd_fetch projectdiscovery/nuclei "$NUCLEI_VERSION" nuclei; \
    pd_fetch projectdiscovery/httpx "$HTTPX_VERSION" httpx; \
    pd_fetch projectdiscovery/subfinder "$SUBFINDER_VERSION" subfinder; \
    pd_fetch projectdiscovery/katana "$KATANA_VERSION" katana; \
    pd_fetch projectdiscovery/vulnx "$VULNX_VERSION" vulnx; \
    tar_fetch() { url="$1"; name="$2"; \
      curl -fsSL "$url" -o /tmp/t.tgz \
      && mkdir -p /tmp/td && tar -xzf /tmp/t.tgz -C /tmp/td \
      && find /tmp/td -type f -name "$name" -exec install -m 0755 {} /usr/local/bin/ \; \
      && rm -rf /tmp/t.tgz /tmp/td; }; \
    tar_fetch "$base/ffuf/ffuf/releases/download/$FFUF_VERSION/ffuf_${FFUF_VERSION#v}_linux_${FFUF_ARCH}.tar.gz" ffuf; \
    tar_fetch "$base/hahwul/dalfox/releases/download/$DALFOX_VERSION/dalfox-${DALFOX_VERSION}-linux-${DALFOX_ARCH}-musl.tar.gz" dalfox; \
    git clone --depth 1 --branch "$SQLMAP_VERSION" https://github.com/sqlmapproject/sqlmap /opt/sqlmap; \
    ln -s /opt/sqlmap/sqlmap.py /usr/local/bin/sqlmap; \
    nuclei -version >/dev/null; httpx -version; subfinder -version; katana -version; \
    vulnx --help >/dev/null; \
    ffuf -V >/dev/null; dalfox --version >/dev/null 2>&1 || true; sqlmap --version >/dev/null

# Tool user and reserved ids, run-time directories and the ping capability.
# 30002 is the account model-driven processes run as (home /home/artex-tool, no login
# shell); 30001, 30003 and 30004 are reserved and own nothing. The directories under /run
# are root-only except /run/artex-tmp (search only). /bin/ping loses its file capability:
# tools hold no capabilities, and ping falls back to ICMP datagram sockets.
RUN set -eux; \
    groupadd -g 30001 artex; groupadd -g 30002 artex-tool; \
    groupadd -g 30003 artex-sandbox; groupadd -g 30004 artex-mcp; \
    useradd -u 30001 -g 30001 -M -d /nonexistent -s /usr/sbin/nologin artex; \
    useradd -u 30002 -g 30002 -M -d /home/artex-tool -s /usr/sbin/nologin artex-tool; \
    useradd -u 30003 -g 30003 -M -d /nonexistent -s /usr/sbin/nologin artex-sandbox; \
    useradd -u 30004 -g 30004 -M -d /nonexistent -s /usr/sbin/nologin artex-mcp; \
    install -d -o 30002 -g 30002 -m 0700 /home/artex-tool; \
    install -d -o root -g root -m 0700 /run/artex-private /run/artex-private/vars; \
    install -d -o root -g root -m 0711 /run/artex-tmp; \
    if [ -n "$(getcap /bin/ping)" ]; then setcap -r /bin/ping; fi; \
    [ -z "$(getcap /bin/ping)" ]

# bbot recursive recon orchestrator (#117): blacklanternsecurity/bbot, AGPL-3.0
# (same license as ARTEX; standalone CLI dependency, not linked code). Pinned
# version, own venv to isolate from the image python. Module dependencies are
# preinstalled (--install-all-deps) so scans never need pip at runtime, which
# would fail under a program's egress allowlist.
ARG BBOT_VERSION=3.0.2
# bbot keeps its dependencies under $HOME/.bbot, so the install runs with HOME=/opt/bbot-home:
# a shared, root-owned tree that the tool user can read (chmod in the same RUN, so no layer
# copies the files again). The tool home gets a link per tool (bbot needs the directories
# themselves to be writable, so they are real directories) and a copy of bbot's setup state;
# scans, cache and config stay in the tool home. Scans run with --no-deps (the bbot-recon
# skill does too): the dependency state is keyed by host name and cannot match at run time.
RUN set -eux; \
    export HOME=/opt/bbot-home; \
    mkdir -p /opt/bbot-home; \
    python -m venv /opt/bbot; \
    /opt/bbot/bin/pip install --no-cache-dir "bbot==${BBOT_VERSION}"; \
    ln -s /opt/bbot/bin/bbot /usr/local/bin/bbot; \
    bbot -y --install-all-deps; \
    bbot --version >/dev/null; \
    bbot --dry-run -t example.com -m wafw00f,paramminer_getparams,lightfuzz,sslcert,gowitness >/dev/null 2>&1 || true; \
    bbot --dry-run -t example.com -m wafw00f,paramminer_getparams,lightfuzz,sslcert,gowitness --no-color >/tmp/bbot-dry.log 2>&1 || true; \
    ! grep -qE "Installing|ERRR|CRITICAL" /tmp/bbot-dry.log || { cat /tmp/bbot-dry.log; exit 1; }; \
    chmod -R a+rX /opt/bbot-home; \
    th=/home/artex-tool; bh=/opt/bbot-home/.bbot; \
    mkdir -p "$th/.bbot" "$th/.bbot/tools" "$th/.bbot/lib" "$th/.bbot/cache" "$th/.bbot/cache/depsinstaller"; \
    for e in "$bh"/tools/*; do ln -s "$e" "$th/.bbot/tools/$(basename "$e")"; done; \
    install -m 0644 "$bh/cache/depsinstaller/setup_status.json" "$th/.bbot/cache/depsinstaller/setup_status.json"; \
    chown -R -h 30002:30002 "$th"; \
    ( cd /; setpriv --reuid=30002 --regid=30002 --clear-groups --inh-caps=-all --ambient-caps=-all \
        --bounding-set=-all --no-new-privs env -i HOME="$th" PATH="$PATH" \
        bbot --dry-run --no-deps -y -t example.com -m wafw00f,paramminer_getparams,lightfuzz,sslcert,gowitness --no-color \
        >/tmp/bbot-tool-dry.log 2>&1 ) || { cat /tmp/bbot-tool-dry.log; exit 1; }; \
    ! grep -qE "Installing|ERRR|CRIT" /tmp/bbot-tool-dry.log || { cat /tmp/bbot-tool-dry.log; exit 1; }; \
    rm -rf "$th/.bbot/scans" "$th/.bbot/logs" "$th/.bbot/temp" "$th/.config" /tmp/bbot-tool-dry.log


# Preinstall Playwright MCP and CLI (globally) so no npx network download is needed at runtime.
# @playwright/mcp: browser MCP runs directly via `npx @playwright/mcp` (installed globally, no -y/@latest needed).
# All three packages are pinned to exact versions; upgrade them together and deliberately.
# @playwright/cli: provides playwright-cli, verified with --help right after install.
# Then install playwright (browser management) and preinstall chromium plus its system
# dependencies via --with-deps, so MCP/CLI work on first start inside the container
# without downloading browsers over the network.
ARG PLAYWRIGHT_MCP_VERSION=0.0.86
ARG PLAYWRIGHT_CLI_VERSION=0.1.25
ARG PLAYWRIGHT_VERSION=1.65.1
# Browsers go to a shared, root-owned path that the tool user can read (not under /root).
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
RUN npm install -g "@playwright/mcp@${PLAYWRIGHT_MCP_VERSION}" "@playwright/cli@${PLAYWRIGHT_CLI_VERSION}" "playwright@${PLAYWRIGHT_VERSION}" \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && chmod -R a+rX /opt/ms-playwright \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Binary compiled from source in the build stage above.
COPY --from=build /out/artex /app/artex
# Daemon startup script: relaunches the process after a crash and forwards SIGTERM
# to artex — docker stop only signals PID 1; without forwarding, artex never receives it,
# cannot shut down gracefully, and is hard-killed by SIGKILL after 10 seconds.
# root-owned like the binary (COPY keeps the builder's uid unless told otherwise)
COPY --chown=0:0 --chmod=0755 start.sh /app/start.sh
# Shell shim, root-owned 0755 (COPY keeps the builder's uid unless told otherwise); only the server's PATH points at it (see the file).
COPY --chown=0:0 --chmod=0755 docker/shims/bash /usr/local/libexec/artex/shims/bash
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# persistence point for data/ (SQLite + jwt.key)
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
