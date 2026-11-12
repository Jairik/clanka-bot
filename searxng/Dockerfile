# Upstream SearXNG pinned for the assistant stack. Wrapping it in a local
# image gives the service a clanka/web-search:<sha> tag, so scripts/deploy.sh
# can roll back to the matching build; the version itself is the official
# 2026.11.12 release image (amd64, arm64 and arm/v7).
FROM docker.io/searxng/searxng:2026.11.12-de722ab1b
