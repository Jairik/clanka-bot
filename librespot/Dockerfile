# librespot v0.8.0, built from a pinned commit. The builder runs on the build
# host and cross-compiles for arm64, so `docker buildx build --platform
# linux/arm64` does not need QEMU for the Rust compile.
#
# Features:
#   pulseaudio-backend      - output goes to the host Pulse sink, not ALSA
#   with-avahi              - advertise through a container-local Avahi daemon.
#                             libmdns aborts on the first interface whose
#                             multicast join fails (ENOBUFS on hosts with many
#                             Docker bridges); Avahi tolerates that and can be
#                             restricted to the LAN interface.
#   rustls-tls-webpki-roots - no OpenSSL cross-compile and a self-contained
#                             trust store, as upstream recommends for containers
#
# The runtime image carries the binary, libpulse0, and a tiny D-Bus + Avahi
# responder. Credentials live only in the librespot-cache volume.
FROM --platform=$BUILDPLATFORM rust:1.85-bookworm AS build

ARG TARGETARCH
ARG LIBRESPOT_VERSION=v0.8.0
ARG LIBRESPOT_COMMIT=d36f9f1907e8cc9d68a93f8ebc6b627b1bf7267d

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gcc-aarch64-linux-gnu \
        libc6-dev-arm64-cross \
        libpulse-dev \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

# arm64 development headers for the cross build. The amd64 headers above serve
# the native build.
RUN dpkg --add-architecture arm64 \
    && apt-get update \
    && apt-get install -y --no-install-recommends libpulse-dev:arm64 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
# The commit URL is immutable even if the tag is moved.
RUN curl -fsSL "https://codeload.github.com/librespot-org/librespot/tar.gz/${LIBRESPOT_COMMIT}" \
      | tar -xz --strip-components=1

RUN rustup target add aarch64-unknown-linux-gnu

# The source tarball is not a git worktree, so tell vergen the pinned commit
# and date for a useful `librespot --version` string instead of its default.
ENV VERGEN_GIT_SHA=${LIBRESPOT_COMMIT} \
    VERGEN_GIT_COMMIT_DATE=2025-11-10

ENV PKG_CONFIG_ALLOW_CROSS=1 \
    PKG_CONFIG_PATH_aarch64-unknown-linux-gnu=/usr/lib/aarch64-linux-gnu/pkgconfig \
    CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER=aarch64-linux-gnu-gcc \
    CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_RUSTFLAGS="-L native=/usr/lib/aarch64-linux-gnu"

RUN set -eux; \
    mkdir -p /out; \
    if [ "$TARGETARCH" = "arm64" ]; then \
        cargo build --release --locked --target aarch64-unknown-linux-gnu \
            --no-default-features \
            --features "pulseaudio-backend with-avahi rustls-tls-webpki-roots"; \
        install -m 0755 target/aarch64-unknown-linux-gnu/release/librespot /out/librespot; \
    else \
        cargo build --release --locked \
            --no-default-features \
            --features "pulseaudio-backend with-avahi rustls-tls-webpki-roots"; \
        install -m 0755 target/release/librespot /out/librespot; \
    fi

FROM debian:trixie-slim

# Redeclare the pins for the image labels; an ARG is scoped to its stage.
ARG LIBRESPOT_VERSION=v0.8.0
ARG LIBRESPOT_COMMIT=d36f9f1907e8cc9d68a93f8ebc6b627b1bf7267d

LABEL org.opencontainers.image.title="clanka librespot" \
      org.opencontainers.image.description="Spotify Connect receiver pinned to librespot ${LIBRESPOT_VERSION}" \
      org.opencontainers.image.version="${LIBRESPOT_VERSION}" \
      org.opencontainers.image.revision="${LIBRESPOT_COMMIT}" \
      org.opencontainers.image.source="https://github.com/librespot-org/librespot"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        avahi-daemon \
        ca-certificates \
        dbus \
        iproute2 \
        libpulse0 \
        tzdata \
        util-linux \
    && rm -rf /var/lib/apt/lists/*

# Both directories are named volumes. The entrypoint fixes their ownership to
# the host UID and drops root before librespot runs. The default UID also gets
# a passwd entry here because the system D-Bus EXTERNAL auth rejects a UID that
# is not in the container's passwd database; the entrypoint adds one when
# ASSISTANT_UID differs.
RUN groupadd -g 1000 librespot \
    && useradd -u 1000 -g 1000 -M -d /tmp -s /usr/sbin/nologin librespot \
    && mkdir -p /cache /status
COPY --from=build /out/librespot /usr/local/bin/librespot
COPY entrypoint.sh event.sh healthcheck.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/entrypoint.sh /usr/local/bin/event.sh /usr/local/bin/healthcheck.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
