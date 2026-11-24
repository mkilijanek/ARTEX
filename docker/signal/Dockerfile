# syntax=docker/dockerfile:1
#
# artex-signal: the Signal sidecar. A static Go supervisor that starts signal-cli
# (JVM build) directly, with a fixed command line and no shell.
#
#   go build -> signal-cli fetch/verify -> runtime image   (last stage, the default)
#                                       -> smoke image     (--target smoke)
#
# Every base image is pinned by digest. signal-cli is pinned by version and sha256;
# the build fails on a hash mismatch. The two native libraries are taken from the
# verified jars, checked again by hash, and installed in a root-owned read-only
# directory, so nothing is unpacked into /tmp at run time (/tmp may be noexec).
#
#   docker build -f docker/signal/Dockerfile -t artex-signal .
#   docker build -f docker/signal/Dockerfile --target smoke -t artex-signal-smoke .

# ----------------------------------------------------------------- build ---
FROM golang:1.26-bookworm@sha256:4ebd099cae89439fff17af404a9c724079d439b4d92178819318735312ee9eb3 AS build
ARG ARTEX_VERSION=dev
WORKDIR /src
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly \
    GOTOOLCHAIN=local
COPY . .
RUN mkdir -p /out \
 && go build -trimpath -ldflags "-s -w -buildid= -X main.version=${ARTEX_VERSION}" \
      -o /out/artex-signal ./cmd/artex-signal \
 && go build -trimpath -ldflags "-s -w -buildid=" \
      -o /out/fakecli ./signalsidecar/testdata/fakecli
# The binary must stay static: no interpreter, no NEEDED entry.
RUN set -eu; \
    hdr="$(readelf -lW /out/artex-signal)"; \
    dyn="$(readelf -dW /out/artex-signal 2>&1 || true)"; \
    if printf '%s\n' "$hdr" | grep -q INTERP; then echo "artex-signal is dynamically linked" >&2; exit 1; fi; \
    if printf '%s\n' "$dyn" | grep -q NEEDED; then echo "artex-signal has shared library dependencies" >&2; exit 1; fi

# ------------------------------------------------------------- signal-cli ---
FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e AS signal-cli
ARG SIGNAL_CLI_VERSION=0.14.10
ARG SIGNAL_CLI_SHA256=706a57d9faae7525cf732936efdabf210146e3c83f9e8ae7e4e62f5946121c33
ARG LIBSIGNAL_JNI_SHA256=dfd7bd7e2658950f74b476547f46a64ebcbbcf024e83e1e0ffa99329bdd818b0
ARG SQLITE_JNI_SHA256=9149f234a03da04caba72ce534dfa0f0e5b999ae8d03d630034be65f24c7b44c
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl unzip zip binutils \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /work
RUN set -eu; \
    curl -fsSL --proto '=https' --tlsv1.2 -o signal-cli.tar.gz \
      "https://github.com/AsamK/signal-cli/releases/download/v${SIGNAL_CLI_VERSION}/signal-cli-${SIGNAL_CLI_VERSION}.tar.gz"; \
    echo "${SIGNAL_CLI_SHA256}  signal-cli.tar.gz" | sha256sum -c -; \
    mkdir -p /opt/signal-cli/lib /opt/signal-cli/native /work/unpack; \
    tar -xzf signal-cli.tar.gz -C /work/unpack; \
    cp /work/unpack/signal-cli-${SIGNAL_CLI_VERSION}/lib/*.jar /opt/signal-cli/lib/
# libsignal: take the library from the verified jar, check it, remove it from the jar,
# strip debug info, install under the name the JVM looks for.
RUN set -eu; \
    cd /opt/signal-cli/lib; \
    jar="$(ls libsignal-client-*.jar)"; \
    [ "$(printf '%s\n' "$jar" | wc -l)" = 1 ]; \
    unzip -o -j "$jar" libsignal_jni_amd64.so -d /work/libsignal; \
    echo "${LIBSIGNAL_JNI_SHA256}  /work/libsignal/libsignal_jni_amd64.so" | sha256sum -c -; \
    zip -d "$jar" '*signal_jni*.so' '*signal_jni*.dylib' '*signal_jni*.dll'; \
    strip --strip-debug -o /opt/signal-cli/native/libsignal_jni.so /work/libsignal/libsignal_jni_amd64.so
# sqlite-jdbc: Linux x86_64 library only, checked, removed from the jar.
RUN set -eu; \
    cd /opt/signal-cli/lib; \
    jar="$(ls sqlite-jdbc-*.jar)"; \
    [ "$(printf '%s\n' "$jar" | wc -l)" = 1 ]; \
    unzip -o -j "$jar" org/sqlite/native/Linux/x86_64/libsqlitejdbc.so -d /work/sqlite; \
    echo "${SQLITE_JNI_SHA256}  /work/sqlite/libsqlitejdbc.so" | sha256sum -c -; \
    cp /work/sqlite/libsqlitejdbc.so /opt/signal-cli/native/libsqlitejdbc.so; \
    zip -d "$jar" '*.so' '*.dylib' '*.dll' '*.jnilib'
# Fail the build if any jar still holds a native library or a testing library exists.
RUN set -eu; \
    for j in /opt/signal-cli/lib/*.jar; do \
      if unzip -Z1 "$j" | grep -Ei '\.(so|dylib|dll|jnilib)(\.[0-9]+)*$'; then \
        echo "native library left in $j" >&2; exit 1; \
      fi; \
    done; \
    if find /opt /work -iname '*signal_jni_testing*' | grep .; then \
      echo "testing library present" >&2; exit 1; \
    fi; \
    test -s /opt/signal-cli/native/libsignal_jni.so; \
    test -s /opt/signal-cli/native/libsqlitejdbc.so; \
    chown -R 0:0 /opt/signal-cli; \
    find /opt/signal-cli -type d -exec chmod 0555 {} +; \
    find /opt/signal-cli -type f -exec chmod 0444 {} +

# ----------------------------------------------------------------- smoke ---
# The runtime image plus the scripted stand-in for signal-cli, for CI and the compose
# smoke test. The sidecar starts /usr/bin/java with a fixed command line, so the
# stand-in is installed at that path. There is no signal-cli and there are no native
# libraries in this image.
FROM gcr.io/distroless/java25-debian13@sha256:d1b024b25b9dd0a363c14e2927e8059c94bff102f2e2a645eb849ea36766292a AS smoke
COPY --from=build --chown=0:0 --chmod=0555 /out/artex-signal /usr/local/bin/artex-signal
COPY --from=build --chown=0:0 --chmod=0555 /out/fakecli /usr/bin/java
ENV ARTEX_SIGNAL_SOCKET=/sock/api.sock \
    ARTEX_SIGNAL_DATA=/data
USER 10001:10001
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/artex-signal", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/artex-signal"]

# --------------------------------------------------------------- runtime ---
FROM gcr.io/distroless/java25-debian13@sha256:d1b024b25b9dd0a363c14e2927e8059c94bff102f2e2a645eb849ea36766292a AS runtime
ARG ARTEX_VERSION=dev
ARG SIGNAL_CLI_VERSION=0.14.10
LABEL org.opencontainers.image.title="artex-signal" \
      org.opencontainers.image.description="Signal sidecar for ARTEX: supervisor plus signal-cli" \
      org.opencontainers.image.source="https://github.com/mkilijanek/ARTEX" \
      org.opencontainers.image.licenses="AGPL-3.0" \
      org.opencontainers.image.version="${ARTEX_VERSION}" \
      org.opencontainers.image.base.name="gcr.io/distroless/java25-debian13" \
      io.artex.signal-cli.version="${SIGNAL_CLI_VERSION}"
# The tree keeps the modes set in the signal-cli stage: directories 0555, files 0444.
COPY --from=signal-cli --chown=0:0 /opt/signal-cli /opt/signal-cli
COPY --from=build --chown=0:0 --chmod=0555 /out/artex-signal /usr/local/bin/artex-signal
# The java binary (/usr/bin/java), the native directory and the lib directory are
# fixed places that the binary compiles in; only the socket and data paths are settings.
ENV ARTEX_SIGNAL_SOCKET=/sock/api.sock \
    ARTEX_SIGNAL_DATA=/data
USER 10001:10001
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/artex-signal", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/artex-signal"]
