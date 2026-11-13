# Runtime image for Spotify's Soloist Connect receiver. The proprietary
# archive is NOT baked into this image: the operator downloads it once with
# scripts/soloist-setup.sh and Compose mounts the binary read-only at
# /opt/soloist. The image only carries the shared libraries Soloist links at
# runtime, which keeps the official binary out of anything distributable.
FROM debian:trixie-slim

# Soloist needs a newer glibc than the Pi host provides, so it runs here.
# libpulse0 is the default output path (PULSE_SERVER, like the Assistant);
# libpipewire-0.3-0 supports the optional -d native node routing.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        libatomic1 \
        libpipewire-0.3-0 \
        libpulse0 \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

# Bind-mounted at deploy time: ./data, ./cache and ./status are host
# directories, so no entrypoint chown or root stage is needed.
RUN mkdir -p /data /cache /status
COPY entrypoint.sh heartbeat.sh healthcheck.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/entrypoint.sh \
    /usr/local/bin/heartbeat.sh \
    /usr/local/bin/healthcheck.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
