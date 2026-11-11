# Build the assistant on the target architecture, then ship a small runtime image.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/assistant ./cmd/assistant

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 assistant
COPY --from=build /out/assistant /usr/local/bin/assistant
COPY config.yaml /app/config.yaml
WORKDIR /app
USER assistant
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/assistant"]
