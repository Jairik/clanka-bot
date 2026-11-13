# Local wrapper around the upstream openWakeWord Wyoming service.
#
# The upstream image already carries the built-in wake word models, so there is
# nothing to download here. Wrapping it lets Compose tag the image
# clanka/wakeword:${GIT_SHA} and add our own health check. The base digest is
# the multi-arch manifest list, so the same file builds on amd64 and the Pi.
FROM rhasspy/wyoming-openwakeword:latest@sha256:52cb1168731a1849fc28cf339c935fde58746bbabc94226668a40ef6ddf5d42b

# Fail the build if the pinned upstream image stops shipping the wake word
# models; nothing is fetched here, the weights are already in the base layer.
RUN ls /usr/src/.venv/lib/python3.11/site-packages/pyopen_wakeword/models/*.tflite >/dev/null

# The locally trained detectors under models/, including "clanker" and, when
# they exist, the wake-free volume_up/volume_down models (#81). The glob keeps
# the build valid while a command model is absent: Compose passes the directory,
# and a name that is not present is simply never detected. Compose passes
# --custom-model-dir /usr/src/custom_models. This layer comes before the app
# code so editing the health check does not rebuild it.
COPY models/*.tflite /usr/src/custom_models/

# Per-model thresholds. The upstream handler applies one global --threshold to
# every model; the Assistant sends a threshold map with each detect request so
# clanker, okay_nabu and #81's command models can use different values. The
# patcher asserts every anchor, so a bumped base image fails the build instead
# of silently losing the feature.
COPY patch_per_model_thresholds.py /usr/src/patch_per_model_thresholds.py
RUN /usr/src/.venv/bin/python3 /usr/src/patch_per_model_thresholds.py

# Uses the interpreter in the upstream venv, which already has wyoming installed.
COPY healthcheck.py /usr/src/healthcheck.py
