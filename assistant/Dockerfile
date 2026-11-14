# Build the Dashboard's React app, then the assistant on the target
# architecture, then ship a small runtime image.

# The web bundle is plain JS/CSS, so it builds on the host platform even for
# an arm64 image; nothing in it depends on the target architecture.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
# Vite writes to ../internal/dashboard/ui/dist, which the Go build embeds.
RUN mkdir -p ../internal/dashboard/ui && pnpm build

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/dashboard/ui/dist ./internal/dashboard/ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/assistant ./cmd/assistant

FROM alpine:3.22
# pacat from pulseaudio-utils streams raw PCM to the configured Pulse sink.
RUN apk add --no-cache ca-certificates tzdata pulseaudio-utils yt-dlp ffmpeg python3 py3-pip && adduser -D -u 10001 assistant
RUN python3 -m venv /opt/ble && /opt/ble/bin/pip install --no-cache-dir bleak==1.1.1
ENV PATH="/opt/ble/bin:$PATH"
COPY --from=build /out/assistant /usr/local/bin/assistant
# The runtime user must read config even when the host checkout uses umask 077.
COPY --chmod=0644 config.yaml /app/config.yaml
# The named data, voices and wake-sample volumes are seeded from these
# directories. Compose runs the container as the host UID for the Pulse socket,
# so all three must be writable by it; the SQLite file is created 0600 by the
# process and takes are 0600.
RUN chmod 0755 /app && mkdir -p /app/data/voices /app/data/wake-samples && chmod 0777 /app/data /app/data/voices /app/data/wake-samples
WORKDIR /app
USER assistant
# Catch unreadable config or a non-traversable parent before publishing an image.
RUN test -x /app && test -r /app/config.yaml
EXPOSE 8080 8081 8082
ENTRYPOINT ["/usr/local/bin/assistant"]
