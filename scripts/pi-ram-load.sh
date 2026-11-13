#!/usr/bin/env bash
# Scripted load for the #69/#74 RAM measurements, run on the Pi.
#
# One turn is: wake-word detection over Wyoming -> speech-to-text over Wyoming
# -> one typed Dashboard request -> TTS synthesis over Wyoming. Fifteen turns
# by default, mixing Fast-path and Agent-path questions.
#
# AUDIO SAFETY: this script never plays anything. It refuses to run unless the
# named Echo sink is muted, the TTS step counts PCM bytes and throws them away,
# and the Dashboard typed path answers with outcome `no_audio` by design (it
# has no speaker attached). Nothing here calls PulseAudio except the mute
# assertion.
#
# It also starts scripts/pi-ram-sample.sh at a 2 s interval and stops it when
# the turns finish, so `--out` holds the sampled load window.
#
# Usage, on the Pi, with the stack deployed:
#   scripts/pi-ram-load.sh                 # 15 turns
#   TURNS=1 scripts/pi-ram-load.sh         # one warm-up/soak turn
#
# Results land in $OUT_DIR (default /tmp/pi-ram-<timestamp>): load.jsonl
# (memory samples), load.log (one JSON line per step) and start/end epochs.

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=${REPO:-$(cd "$HERE/.." && pwd)}
cd "$REPO"

SHA=${GIT_SHA:-$(git rev-parse --short HEAD)}
PROJECT=${PROJECT:-clanka-bot}
NETWORK=${NETWORK:-${PROJECT}_default}
# The sink whose mute guards the run: the current default (the Echo Dot on the
# Pi) unless SINK names another.
SINK=${SINK:-$(pactl get-default-sink 2>/dev/null || true)}
if [ -z "$SINK" ]; then
  echo "refusing to run: no default sink; set SINK to the speaker's pactl name" >&2
  exit 1
fi
TURNS=${TURNS:-15}
SAMPLE_INTERVAL=${SAMPLE_INTERVAL:-2}
CLIENT=${CLIENT:-clanka-ram-load-client}
STT_IMAGE=${STT_IMAGE:-clanka/stt:$SHA}

OUT_DIR=${OUT_DIR:-/tmp/pi-ram-$(date +%Y%m%d-%H%M%S)}
mkdir -p "$OUT_DIR"
SAMPLES="$OUT_DIR/load.jsonl"
LOG="$OUT_DIR/load.log"
FIXTURE="$OUT_DIR/stt-fixture.pcm"

export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}

# --- audio interlock -------------------------------------------------------
if ! command -v pactl >/dev/null 2>&1; then
  echo "refusing to run: pactl is not available, so the mute state is unknown" >&2
  exit 1
fi
mute=$(pactl get-sink-mute "$SINK" 2>/dev/null | awk '{print $2}')
if [ "$mute" != "yes" ]; then
  echo "refusing to run: $SINK is not muted (state: ${mute:-unknown})" >&2
  exit 1
fi
echo "audio: $SINK is muted; no step below plays audio" | tee "$LOG"

# --- load client container -------------------------------------------------
cp "$HERE/pi-ram-load-client.py" "$OUT_DIR/client.py"

cleanup() {
  if [ -n "${SAMPLER:-}" ]; then kill "$SAMPLER" >/dev/null 2>&1 || true; fi
  docker rm -f "$CLIENT" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker rm -f "$CLIENT" >/dev/null 2>&1 || true
docker run -d --rm --name "$CLIENT" --network "$NETWORK" \
  --label clanka.ram.exclude=1 \
  -v "$REPO/wakeword/tests/fixtures:/fixtures:ro" \
  -v "$OUT_DIR:/load" \
  "$STT_IMAGE" sleep infinity >/dev/null
echo "client: $CLIENT on $NETWORK ($STT_IMAGE)"

# A known-good STT input made from the owner's TTS voice, once, before the
# measured window: the repo has wake-word fixtures but no speech-to-text ones.
if [ ! -s "$FIXTURE" ]; then
  docker exec "$CLIENT" python /load/client.py fixture "what time is it?" /load/stt-fixture.pcm
fi

# --- turns -----------------------------------------------------------------
QUESTIONS=(
  "what time is it"
  "what's the weather in Berlin"
  "explain what a heat pump does in two sentences"
  "what's the date today"
  "what is 15 percent of 240"
  "tell me a short fact about the moon"
  "what time is it in Tokyo"
  "who wrote Pride and Prejudice"
  "what's the weather in Berlin right now"
  "explain photosynthesis in two sentences"
  "what day of the week is it"
  "what is 12 times 12"
  "summarize what a heat pump does in one sentence"
  "how many days until Christmas"
  "tell me one sentence about the ocean"
)

# .env values may be quoted; strip both quote styles without a shell quoting
# puzzle. The owner login is an identity, not a secret: it is in deploy.md.
OWNER=${ASSISTANT_DASHBOARD_OWNER:-}
if [ -z "$OWNER" ]; then
  OWNER=$(grep -E '^ASSISTANT_DASHBOARD_OWNER=' .env | cut -d= -f2- | tr -d "\"'" || true)
fi
if [ -z "$OWNER" ]; then
  echo "refusing to run: ASSISTANT_DASHBOARD_OWNER is empty, the Dashboard is off" >&2
  exit 1
fi

# One typed request through the Dashboard listener on host loopback. The
# identity header is exactly what `tailscale serve` injects; the loopback
# source and Host checks still apply, and the same-origin marker is the one a
# browser fetch sends. The reply text comes back in the Activity view so the
# TTS step can speak the real answer instead of a canned line.
ask() {
  local text=$1
  curl -sS --max-time 180 \
    -H "Tailscale-User-Login: $OWNER" \
    -H "Sec-Fetch-Site: same-origin" \
    --data-urlencode "text=$text" \
    http://127.0.0.1:8082/activity/ask |
    python3 -c '
import json, sys
view = json.load(sys.stdin)
just = view.get("Just") or {}
reply = (just.get("Reply") or "").strip()
print(json.dumps({"step": "ask", "path": just.get("Path"), "tool": just.get("Tool"),
                  "outcome": just.get("Outcome"), "reply": reply[:200]}), file=sys.stderr)
print(reply)
'
}

date +%s >"$OUT_DIR/start.epoch"
sudo -n "$HERE/pi-ram-sample.sh" --interval "$SAMPLE_INTERVAL" \
  --duration 3600 --project "$PROJECT" --out "$SAMPLES" >/dev/null &
SAMPLER=$!
sleep 2  # one baseline sample before the first turn

positive=($(ls wakeword/tests/fixtures/positive/*.wav))
for i in $(seq 1 "$TURNS"); do
  text=${QUESTIONS[$(((i - 1) % ${#QUESTIONS[@]}))]}
  wav=${positive[$(((i - 1) % ${#positive[@]}))]}
  echo "turn $i/$TURNS: $text"
  docker exec "$CLIENT" python /load/client.py wake "/fixtures/positive/$(basename "$wav")" 2>&1 | tee -a "$LOG" || true
  docker exec "$CLIENT" python /load/client.py stt /load/stt-fixture.pcm 2>&1 | tee -a "$LOG" || true
  reply=$(ask "$text" 2>>"$LOG")
  echo "ask reply: ${reply:0:120}" >>"$LOG"
  docker exec "$CLIENT" python /load/client.py tts "${reply:-The answer is ready.}" 2>&1 | tee -a "$LOG" || true
done
date +%s >"$OUT_DIR/end.epoch"

kill "$SAMPLER" >/dev/null 2>&1 || true
SAMPLER=""

echo "samples: $SAMPLES"
echo "log:     $LOG"
wc -l <"$SAMPLES" | xargs printf 'samples collected: %s\n'

mute=$(pactl get-sink-mute "$SINK" 2>/dev/null | awk '{print $2}')
echo "audio: $SINK mute is now ${mute:-unknown}"
