#!/usr/bin/env bash
# Acceptance seam: the real image, the real Wyoming service, a synthesized
# "clanker" that must fire and near misses that must not.
#
# Runs its own throwaway container (no Compose project, no ports published, no
# volumes beyond this directory read-only) and removes it on exit. Fixtures are
# held-out Pocket TTS voices, so this does not measure the owner's room.
#
# Usage: wakeword/tests/run_detection_test.sh   (THRESHOLD=0.5 by default)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
IMAGE=${IMAGE:-clanka/wakeword:test}
THRESHOLD=${THRESHOLD:-0.5}

docker build -q -t "$IMAGE" "$HERE/.." >/dev/null

cid=$(docker run -d --rm \
  -v "$HERE:/test:ro" \
  "$IMAGE" \
  --uri tcp://0.0.0.0:10400 \
  --custom-model-dir /usr/src/custom_models \
  --threshold "$THRESHOLD")
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT

ready=""
for _ in $(seq 1 30); do
  if docker exec "$cid" /usr/src/.venv/bin/python3 /usr/src/healthcheck.py >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [[ -z "$ready" ]]; then
  echo "wakeword service never became healthy; last logs:" >&2
  docker logs "$cid" >&2 || true
  exit 1
fi

# The custom model must be served alongside the upstream built-ins, so
# WAKEWORD_MODEL can still name okay_nabu or another upstream detector.
docker exec "$cid" /usr/src/.venv/bin/python3 /test/wyoming_client.py \
  --addr tcp://127.0.0.1:10400 --expect-models clanker,okay_nabu

docker exec "$cid" /usr/src/.venv/bin/python3 /test/wyoming_client.py \
  --addr tcp://127.0.0.1:10400 --model clanker \
  --positive /test/fixtures/positive --negative /test/fixtures/negative
