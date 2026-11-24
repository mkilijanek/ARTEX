# syntax=docker/dockerfile:1@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
#
# Tools base image: everything ARTEX needs at run time except ARTEX itself (apt packages,
# Node, the recon binaries, the tool user and run directories, Playwright, bbot with its
# dependencies). Build context: this directory.
#
#   docker build -f docker/base/Dockerfile --target slim -t artex-base:slim docker/base
#
# Targets:
#   slim  the default. No Java, jadx or C toolchain: bbot's installer pulls them in and the
#         build removes them again, and writes the list of removed packages to
#         /usr/share/artex/slim-removed-packages.txt.
#   full  slim plus exactly those packages and jadx, as one more layer (every slim layer is a
#         layer of full).
#
# The Python base is pinned by digest. Debian packages are brought up to date with
# `apt-get upgrade`, so a rebuild of unchanged inputs picks up security fixes; what changed is
# visible with inventory.sh. check-tools.sh runs at the end of the build, as root and as the tool
# user through the same privilege drop the server uses.

# ------------------------------------------------------------------- core ---
FROM python:3.12-slim-bookworm@sha256:935c3c5b12357a7e10caa9c1e6ca5692344d97cf229832ef584be2cec9fc03df AS core
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a set of recon staples (add/remove as needed).
# Node 22.x comes from NodeSource: bookworm's apt nodejs is 18, Playwright requires >=20,
# and 20 is end-of-life. 22 matches the CI and release workflows.
# NodeSource's headers (/usr/include/node, 59 MB) are not needed: nothing here builds a native addon.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap procps psmisc \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /usr/include/node /var/lib/apt/lists/*
# Web-recon toolbox (drost-community take 1, #100): pinned release binaries installed
# into /usr/local/bin so hunt workers can run them directly through their Bash tool.
# Versions pinned per tool; upgrade deliberately. sqlmap is a python checkout of a
# pinned release tag (no release binaries) symlinked into PATH; its .git is dropped.
# Every release asset is verified against a pinned sha256 before it is unpacked. Sources:
# nuclei, httpx, subfinder, katana, vulnx: the <tool>_<version>_checksums.txt of the GitHub release
# (the file for katana is katana-<version>-checksums.txt); ffuf: ffuf_<version>_checksums.txt of the
# release; dalfox: the <asset>.sha256 file of the release (its checksum.txt covers only the source
# archives). Each value equals the sha256 digest GitHub reports for the asset. A version bump
# must replace both hashes of the tool.
ARG NUCLEI_VERSION=v3.11.1
ARG NUCLEI_SHA256_AMD64=ea63d4ae232808cd7c6bc00d0142428e231fab59dae01042246097d195835ab6
ARG NUCLEI_SHA256_ARM64=8044e3d9768ba0a744b2872c1a87e813006f013da97ca9f50f7661a4203bec07
ARG HTTPX_VERSION=v1.12.0
ARG HTTPX_SHA256_AMD64=9d8439e8b6c9aa7d1e2314817a392e00d5178da3af5652f7475f88868f418f76
ARG HTTPX_SHA256_ARM64=fd7b123c1dfbc3d69f19f524e4eebcd6ec06b9a6cbd56813c76f11645197331e
ARG SUBFINDER_VERSION=v2.17.0
ARG SUBFINDER_SHA256_AMD64=a39c3e5c7a26ef3c430915c4d6cbf7716a00f26769a75e9405aba796142eba37
ARG SUBFINDER_SHA256_ARM64=24913d49dddc1d3a5e99a4a7580da5be980959b9415b96f52c4f7f4124392190
ARG KATANA_VERSION=v1.8.0
ARG KATANA_SHA256_AMD64=cb027ae99d0757061b70c07a15c7a80f62c5a535ab52c4a329f80c4b0a7309d2
ARG KATANA_SHA256_ARM64=80e269210727a855511ffac8dfb01297b2344742acec71a35e8ba4a67a90f0f2
ARG FFUF_VERSION=v2.3.0
ARG FFUF_SHA256_AMD64=b2a3c725fcb9da175159682f54d6e9149f2905b00d84d155e7efc5d599975ceb
ARG FFUF_SHA256_ARM64=e2d712615d53da797ee929006e41f2df5a7960a5353133db9c150ddaa22ae4e8
ARG DALFOX_VERSION=v3.2.4
ARG DALFOX_SHA256_AMD64=c11cfd2f8261973f0a2a2a1eba5a147ce77712a29acfa6343f9847fb8bb7eca2
ARG DALFOX_SHA256_ARM64=fd56b0b7da2d06868cd73b504f41d21314629feffcd5bdc97718db1c9dd2fa70
# vulnx is ProjectDiscovery's vulnerability-intelligence CLI, the successor of cvemap
# (the cvemap repository was renamed; the binary is `vulnx`).
ARG VULNX_VERSION=v2.0.2
ARG VULNX_SHA256_AMD64=387abd1c42b0357e1d4257b54528fc38e1d0357b12f05ab12117a4c628a5aaf8
ARG VULNX_SHA256_ARM64=13c5888dde027b84bd3b6cf69c36c87e1e7aad7e7159bdeeb576f4962dcc1d06
ARG SQLMAP_VERSION=1.10.11
RUN set -eux; \
    base="https://github.com"; \
    case "$TARGETARCH" in amd64) FFUF_ARCH=amd64; DALFOX_ARCH=x86_64;; arm64) FFUF_ARCH=arm64; DALFOX_ARCH=aarch64;; *) echo "unsupported TARGETARCH $TARGETARCH" >&2; exit 1;; esac; \
    pick() { if [ "$TARGETARCH" = amd64 ]; then echo "$1"; else echo "$2"; fi; }; \
    pd_fetch() { repo="$1"; ver="$2"; name="$3"; sha="$(pick "$4" "$5")"; nov="${ver#v}"; \
      curl -fsSL "$base/$repo/releases/download/$ver/${name}_${nov}_linux_${TARGETARCH}.zip" -o /tmp/t.zip \
      && echo "$sha  /tmp/t.zip" | sha256sum -c - \
      && unzip -o -q /tmp/t.zip -d /tmp/td \
      && find /tmp/td -type f -name "$name" -exec install -m 0755 {} /usr/local/bin/ \; \
      && rm -rf /tmp/t.zip /tmp/td; }; \
    pd_fetch projectdiscovery/nuclei "$NUCLEI_VERSION" nuclei "$NUCLEI_SHA256_AMD64" "$NUCLEI_SHA256_ARM64"; \
    pd_fetch projectdiscovery/httpx "$HTTPX_VERSION" httpx "$HTTPX_SHA256_AMD64" "$HTTPX_SHA256_ARM64"; \
    pd_fetch projectdiscovery/subfinder "$SUBFINDER_VERSION" subfinder "$SUBFINDER_SHA256_AMD64" "$SUBFINDER_SHA256_ARM64"; \
    pd_fetch projectdiscovery/katana "$KATANA_VERSION" katana "$KATANA_SHA256_AMD64" "$KATANA_SHA256_ARM64"; \
    pd_fetch projectdiscovery/vulnx "$VULNX_VERSION" vulnx "$VULNX_SHA256_AMD64" "$VULNX_SHA256_ARM64"; \
    tar_fetch() { url="$1"; name="$2"; sha="$(pick "$3" "$4")"; \
      curl -fsSL "$url" -o /tmp/t.tgz \
      && echo "$sha  /tmp/t.tgz" | sha256sum -c - \
      && mkdir -p /tmp/td && tar -xzf /tmp/t.tgz -C /tmp/td \
      && find /tmp/td -type f -name "$name" -exec install -m 0755 {} /usr/local/bin/ \; \
      && rm -rf /tmp/t.tgz /tmp/td; }; \
    tar_fetch "$base/ffuf/ffuf/releases/download/$FFUF_VERSION/ffuf_${FFUF_VERSION#v}_linux_${FFUF_ARCH}.tar.gz" ffuf "$FFUF_SHA256_AMD64" "$FFUF_SHA256_ARM64"; \
    tar_fetch "$base/hahwul/dalfox/releases/download/$DALFOX_VERSION/dalfox-${DALFOX_VERSION}-linux-${DALFOX_ARCH}-musl.tar.gz" dalfox "$DALFOX_SHA256_AMD64" "$DALFOX_SHA256_ARM64"; \
    git clone --depth 1 --branch "$SQLMAP_VERSION" https://github.com/sqlmapproject/sqlmap /opt/sqlmap; \
    rm -rf /opt/sqlmap/.git; \
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

# Preinstall Playwright MCP and CLI (globally) so no npx network download is needed at runtime.
# @playwright/mcp: browser MCP runs directly via `npx @playwright/mcp` (installed globally, no -y/@latest needed).
# All three packages are pinned to exact versions; upgrade them together and deliberately.
# @playwright/cli: provides playwright-cli, verified with --help right after install.
# Then install playwright (browser management) and preinstall chromium plus its system
# dependencies via --with-deps, so MCP/CLI work on first start inside the container
# without downloading browsers over the network.
# This step comes before bbot: bbot's own Chromium download is skipped when its tools
# directory already has a chrome-linux, and the next step makes that a link to this chromium.
ARG PLAYWRIGHT_MCP_VERSION=0.0.86
ARG PLAYWRIGHT_CLI_VERSION=0.1.25
ARG PLAYWRIGHT_VERSION=1.65.1
# Browsers go to a shared, root-owned path that the tool user can read (not under /root).
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
RUN npm install -g "@playwright/mcp@${PLAYWRIGHT_MCP_VERSION}" "@playwright/cli@${PLAYWRIGHT_CLI_VERSION}" "playwright@${PLAYWRIGHT_VERSION}" \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && chmod -R a+rX /opt/ms-playwright \
    && npm cache clean --force \
    && rm -rf /root/.npm /root/.cache /tmp/* /var/lib/apt/lists/*

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
#
# One RUN, because what bbot's installer leaves behind only shrinks the image when it is removed
# in the layer that creates it:
#  - chrome-linux is linked to the Playwright chromium before the install, so bbot does not
#    download its own browser (700 MB). The installer then runs `chmod 4755` on the
#    chrome_sandbox it finds through that link; the setuid bit is removed again.
#  - the legba binary is moved out of bbot's temp directory (cleaned below) into the tools
#    directory, and linked into /usr/local/bin: the legba module runs it as plain `legba`.
#  - the libraries the tools load stay: every library of every ELF file left under the tool trees
#    is marked manual, plus libpcap0.8 (masscan opens it with dlopen at scan time, so ldd does not
#    list it) and libssl3. Then Java and the C toolchain bbot installed are purged with their
#    now unused dependencies, and the removed package names are written to
#    /usr/share/artex/slim-removed-packages.txt (the full variant installs exactly those).
#  - jadx is removed (it needs Java); nuclei is a link to the pinned /usr/local/bin/nuclei
#    after a check that bbot's copy is the same file.
#  - bbot looks for its own build tools (gcc, make, libssl-dev, ...) at every start, even with
#    --no-deps, and tries to install them when they are missing, which fails for the tool user.
#    It skips that when a marker file named by a hash of its core dependency list is in its cache
#    directory. The installer wrote that marker in the shared tree; a copy goes to the tool home.
#  - caches, bbot's download and build leftovers, node's headers and npm, the tests of the bbot
#    venv, its pip, and the apt lists are removed.
RUN --mount=type=bind,source=check-tools.sh,target=/mnt/check-tools.sh \
    set -eux; \
    export HOME=/opt/bbot-home; \
    bh=/opt/bbot-home/.bbot; th=/home/artex-tool; \
    mkdir -p "$bh/tools"; \
    set -- /opt/ms-playwright/chromium-*/chrome-linux64; \
    [ "$#" -eq 1 ]; \
    [ -x "$1/chrome" ]; \
    ln -s "$1" "$bh/tools/chrome-linux"; \
    python -m venv /opt/bbot; \
    /opt/bbot/bin/pip install --no-cache-dir "bbot==${BBOT_VERSION}"; \
    ln -s /opt/bbot/bin/bbot /usr/local/bin/bbot; \
    bbot -y --install-all-deps; \
    bbot --version >/dev/null; \
    [ -L "$bh/tools/chrome-linux" ]; \
    find /opt/ms-playwright -type f -perm /6000 -exec chmod ug-s {} +; \
    mv "$bh"/temp/legba-*/legba "$bh/tools/legba"; \
    rm -rf "$bh/temp/legba-"*; \
    ln -s "$bh/tools/legba" /usr/local/bin/legba; \
    legba --version; \
    cmp "$bh/tools/nuclei" /usr/local/bin/nuclei; \
    ln -sf /usr/local/bin/nuclei "$bh/tools/nuclei"; \
    rm -rf "$bh/tools/jadx"; \
    rm -rf "$bh/tools/node/include" "$bh/tools/node/lib" "$bh/tools/node/share" "$bh/temp/node-v"*.tar.xz \
      "$bh/temp/masscan" "$bh/temp/massdns" "$bh/temp/medusa/gitrepo" "$bh/temp/medusa/workdir"; \
    ls -la "$bh/temp" "$bh/tools"; \
    bash /mnt/check-tools.sh scan-libs >/tmp/scan-libs.out 2>/tmp/scan-libs.err; \
    cat /tmp/scan-libs.err; \
    if grep -E '^(MISSING|UNPARSED)' /tmp/scan-libs.out; then exit 1; fi; \
    keep="libpcap0.8 libssl3"; \
    for lib in $(sed -n 's/^LIB //p' /tmp/scan-libs.out | sort -u); do \
      case "$lib" in /lib/*|/usr/lib/*|/lib64/*) ;; *) continue ;; esac; \
      pkg=$(dpkg -S "$lib" 2>/dev/null || dpkg -S "$(readlink -f "$lib")" 2>/dev/null || dpkg -S "/usr$lib" 2>/dev/null) \
        || { echo "no package owns $lib"; exit 1; }; \
      keep="$keep $(printf '%s\n' "$pkg" | sed 's/: .*//; s/:amd64//g; s/:arm64//g; s/, / /g')"; \
    done; \
    keep=$(printf '%s\n' $keep | sort -u); \
    purge=; \
    for p in default-jre default-jre-headless openjdk-17-jre openjdk-17-jre-headless java-common \
             gcc make autoconf automake libtool libssl-dev libpcap0.8-dev manpages-dev python3-apt; do \
      if dpkg -s "$p" >/dev/null 2>&1; then purge="$purge $p"; fi; \
    done; \
    mkdir -p /usr/share/artex; \
    apt-get -s purge --auto-remove $purge | sed -n 's/^Remv \([^ ]*\) .*/\1/p' | sort -u >/tmp/would-remove-unmarked; \
    apt-mark showmanual | sort >/tmp/manual-before; \
    apt-mark manual $keep >/dev/null; \
    apt-mark showmanual | sort >/tmp/manual-after; \
    comm -13 /tmp/manual-before /tmp/manual-after >/usr/share/artex/slim-marked-manual.txt; \
    apt-get -s purge --auto-remove $purge | sed -n 's/^Remv \([^ ]*\) .*/\1/p' | sort -u >/tmp/would-remove; \
    comm -23 /tmp/would-remove-unmarked /tmp/would-remove >/usr/share/artex/slim-kept-packages.txt; \
    dpkg-query -W -f='${Package}\n' | sort >/tmp/pkgs-before-purge; \
    apt-get purge -y --auto-remove $purge; \
    dpkg-query -W -f='${Package}\n' | sort >/tmp/pkgs-after-purge; \
    comm -23 /tmp/pkgs-before-purge /tmp/pkgs-after-purge >/usr/share/artex/slim-removed-packages.txt; \
    [ -s /usr/share/artex/slim-removed-packages.txt ]; \
    echo "newly marked manual:"; cat /usr/share/artex/slim-marked-manual.txt; \
    echo "kept with apt-mark manual instead of being removed:"; cat /usr/share/artex/slim-kept-packages.txt; \
    echo "removed:"; cat /usr/share/artex/slim-removed-packages.txt; \
    if command -v java || command -v gcc || command -v make; then exit 1; fi; \
    rm -rf /opt/bbot-home/.cache /opt/bbot-home/.npm /opt/bbot-home/.ansible \
      /var/lib/apt/lists/* /var/cache/debconf/*-old /tmp/*; \
    find /opt/bbot -type d \( -name tests -o -name test \) -prune -exec rm -rf {} +; \
    rm -rf /opt/bbot/lib/python3.12/site-packages/pip /opt/bbot/lib/python3.12/site-packages/pip-* \
      /opt/bbot/bin/pip /opt/bbot/bin/pip3 /opt/bbot/bin/pip3.12; \
    bbot --version >/dev/null; \
    bbot --dry-run -t example.com -m wafw00f,paramminer_getparams,lightfuzz,sslcert,gowitness >/dev/null 2>&1 || true; \
    bbot --dry-run -t example.com -m wafw00f,paramminer_getparams,lightfuzz,sslcert,gowitness --no-color >/tmp/bbot-dry.log 2>&1 || true; \
    ! grep -qE "Installing|ERRR|CRITICAL" /tmp/bbot-dry.log || { cat /tmp/bbot-dry.log; exit 1; }; \
    rm -rf "$bh/scans" "$bh/logs"; \
    chmod -R a+rX /opt/bbot-home; \
    mkdir -p "$th/.bbot" "$th/.bbot/tools" "$th/.bbot/lib" "$th/.bbot/cache" "$th/.bbot/cache/depsinstaller"; \
    for e in "$bh"/tools/*; do ln -s "$e" "$th/.bbot/tools/$(basename "$e")"; done; \
    install -m 0644 "$bh/cache/depsinstaller/setup_status.json" "$th/.bbot/cache/depsinstaller/setup_status.json"; \
    find "$bh/cache" -maxdepth 1 -type f -name '[-0-9]*' -exec install -m 0644 {} "$th/.bbot/cache/" \; ; \
    [ -n "$(find "$th/.bbot/cache" -maxdepth 1 -type f -name '[-0-9]*')" ]; \
    chown -R -h 30002:30002 "$th"; \
    ( cd /; setpriv --reuid=30002 --regid=30002 --clear-groups --inh-caps=-all --ambient-caps=-all \
        --bounding-set=-all --no-new-privs env -i HOME="$th" PATH="$PATH" \
        bbot --dry-run --no-deps -y -t example.com -m wafw00f,paramminer_getparams,lightfuzz,sslcert,gowitness --no-color \
        >/tmp/bbot-tool-dry.log 2>&1 ) || { cat /tmp/bbot-tool-dry.log; exit 1; }; \
    ! grep -qE "Installing|ERRR|CRIT" /tmp/bbot-tool-dry.log || { cat /tmp/bbot-tool-dry.log; exit 1; }; \
    rm -rf "$th/.bbot/scans" "$th/.bbot/logs" "$th/.bbot/temp" "$th/.config" /tmp/*

# ------------------------------------------------------------------- slim ---
FROM core AS slim
# The tool matrix, as root and as the tool user. It must leave nothing behind in the image: the
# home of the tool user is listed before, and whatever the tools create there on first use
# (caches, configuration, scan output) is removed afterwards. The /tmp of the build is cleared too.
RUN --mount=type=bind,source=check-tools.sh,target=/mnt/check-tools.sh \
    --mount=type=bind,source=setuid-expected.txt,target=/mnt/setuid-expected.txt \
    set -eux; \
    th=/home/artex-tool; \
    find "$th" | sort >/tmp/home.before; \
    CHECK_BUILD_SANDBOX=1 bash /mnt/check-tools.sh slim; \
    find "$th" | sort >/tmp/home.after; \
    comm -13 /tmp/home.before /tmp/home.after | sort -r | while IFS= read -r f; do echo "removing $f"; rm -rf "$f"; done; \
    rm -rf /tmp/* /var/tmp/*
ENV ARTEX_VARIANT=slim

# ------------------------------------------------------------------- full ---
FROM slim AS full
# Java, the C toolchain and the -dev packages come back as the exact list the slim build removed
# (bbot's installer chose them), without recommends. jadx is put where bbot's jadx module puts it
# and looks for it ({tools}/jadx/bin/jadx); bbot downloads it without a checksum, so the release
# zip is pinned here. The tool home gets the link for it, owned by the tool user like the others.
# bbot's core-dependency marker in the tool home stays valid: it only says that the check has
# been done, and gcc and make are present again.
ARG JADX_VERSION=1.5.0
ARG JADX_SHA256=c5a713fa4800cbb9e6df85ced1bef95ba329040c95cb87d54465f108483e4ef9
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends $(cat /usr/share/artex/slim-removed-packages.txt); \
    curl -fsSL -o /tmp/jadx.zip "https://github.com/skylot/jadx/releases/download/v${JADX_VERSION}/jadx-${JADX_VERSION}.zip"; \
    echo "${JADX_SHA256}  /tmp/jadx.zip" | sha256sum -c -; \
    jd=/opt/bbot-home/.bbot/tools/jadx; \
    mkdir -p "$jd"; \
    unzip -q /tmp/jadx.zip -d "$jd" bin/jadx "lib/jadx-${JADX_VERSION}-all.jar"; \
    chmod -R a+rX "$jd"; \
    ln -s "$jd" /home/artex-tool/.bbot/tools/jadx; \
    chown -h 30002:30002 /home/artex-tool/.bbot/tools/jadx; \
    rm -rf /var/lib/apt/lists/* /var/cache/debconf/*-old /tmp/*
# The tool matrix of the full variant, including the setuid list: the files must be the ones the
# slim variant has, so a package that brings a setuid file fails the build.
RUN --mount=type=bind,source=check-tools.sh,target=/mnt/check-tools.sh \
    --mount=type=bind,source=setuid-expected.txt,target=/mnt/setuid-expected.txt \
    set -eux; \
    th=/home/artex-tool; \
    find "$th" | sort >/tmp/home.before; \
    CHECK_BUILD_SANDBOX=1 bash /mnt/check-tools.sh full; \
    find "$th" | sort >/tmp/home.after; \
    comm -13 /tmp/home.before /tmp/home.after | sort -r | while IFS= read -r f; do echo "removing $f"; rm -rf "$f"; done; \
    rm -rf /tmp/* /var/tmp/*
ENV ARTEX_VARIANT=full
