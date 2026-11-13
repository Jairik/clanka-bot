#!/usr/bin/env bash
# Acceptance seam: the real image, the real Wyoming service, a synthesized
# "clanker" that must fire and near misses that must not.
#
# Also proves the two-model setup and the per-model thresholds the Assistant
# sends with every detect request:
#   - clanker and okay_nabu load together in one handler;
#   - a threshold map for another model does not change clanker;
#   - a raised clanker threshold suppresses its detection;
#   - a lowered clanker threshold overrides the service's global --threshold.
#
# Runs its own throwaway containers (no Compose project, no ports published, no
# volumes beyond this directory read-only) and removes them on exit. Fixtures
# are held-out Pocket TTS voices, so this does not measure the owner's room.
#
# Usage: wakeword/tests/run_detection_test.sh   (THRESHOLD=0.5 by default)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
IMAGE=${IMAGE:-clanka/wakeword:test}
THRESHOLD=${THRESHOLD:-0.5}
cid=""

docker build -q -t "$IMAGE" "$HERE/.." >/dev/null

start_service() {
  local threshold=$1
  docker run -d --rm \
    -v "$HERE:/test:ro" \
    "$IMAGE" \
    --uri tcp://0.0.0.0:10400 \
    --custom-model-dir /usr/src/custom_models \
    --threshold "$threshold"
}

cleanup() {
  if [[ -n "$cid" ]]; then
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

wait_ready() {
  local id=$1
  for _ in $(seq 1 30); do
    if docker exec "$id" /usr/src/.venv/bin/python3 /usr/src/healthcheck.py >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "wakeword service never became healthy; last logs:" >&2
  docker logs "$id" >&2 || true
  return 1
}

py() {
  docker exec "$cid" /usr/src/.venv/bin/python3 /test/wyoming_client.py --addr tcp://127.0.0.1:10400 "$@"
}

cid=$(start_service "$THRESHOLD")
wait_ready "$cid"

# The custom model must be served alongside the upstream built-ins, so
# WAKEWORD_MODELS can name okay_nabu or another upstream detector too.
py --expect-models clanker,okay_nabu

py --model clanker \
  --positive /test/fixtures/positive \
  --negative /test/fixtures/negative

# Two models in one detect stream: both load, and clanker still fires.
py --model clanker,okay_nabu --positive /test/fixtures/positive

# A threshold for another model must not move clanker's.
py --model clanker --positive /test/fixtures/positive \
  --thresholds okay_nabu=1.0

# A raised per-model threshold suppresses clanker even though the service
# global threshold is lower.
py --model clanker --positive /test/fixtures/positive \
  --thresholds clanker=1.0 --expect miss

# A lowered per-model threshold overrides the global one. This needs a service
# whose global threshold would block the detection, so restart it at 1.0.
docker rm -f "$cid" >/dev/null
cid=$(start_service 1.0)
wait_ready "$cid"
py --model clanker --positive /test/fixtures/positive \
  --thresholds clanker=0.0 --expect hit
