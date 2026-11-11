# Local wrapper around the upstream openWakeWord Wyoming service.
#
# The upstream image already carries the wake word models, so there is nothing
# to download here. Wrapping it lets Compose tag the image
# clanka/wakeword:${GIT_SHA} and add our own health check. The base digest is
# the multi-arch manifest list, so the same file builds on amd64 and the Pi.
FROM rhasspy/wyoming-openwakeword:latest@sha256:52cb1168731a1849fc28cf339c935fde58746bbabc94226668a40ef6ddf5d42b

# Uses the interpreter in the upstream venv, which already has wyoming installed.
COPY healthcheck.py /usr/src/healthcheck.py
