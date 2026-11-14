# Dashboard speech recognition: measurements and decision

Issue #104 asked whether the Dashboard's mic should stay on the Pi's STT
service or move to browser-side recognition. **Decision: keep the server path
as the only path.** The measurements below are from the desktop deployment; the
browser-side accuracy comparison was not run because the server path already
meets the latency target and the browser path would add a cloud dependency and
a second transcription path for no demonstrated gain.

## What was measured

Device: Arch Linux desktop, 12 logical cores, the full Compose stack running
locally (assistant, STT, TTS, embed). Client: `curl` from the same host, so
upload is loopback and effectively zero. Audio was synthesized with the
Dashboard's own wording preview (Pocket TTS, `alba`) and resampled to 16 kHz
mono 16-bit PCM, the exact format the browser uploads. Each utterance was
posted to `POST /api/ui/ask/audio` three times after a warm-up.

Stages come from the activity record the request returns (`stt_finalize`,
`routing`, `tool_execute`). "Upload + answer" is total minus `stt_finalize`; it
includes the loopback upload, routing and the reply (for the longer clips the
Agent path contributes most of it).

| Utterance | Audio | Total (median) | `stt_finalize` (median) | Upload + answer |
| --- | ---: | ---: | ---: | ---: |
| "What time is it right now, please?" | 2.5 s | 596 ms | 559 ms | ~37 ms |
| same phrase, 3× | 6.6 s | 3.58 s | 3.10 s | ~0.48 s |
| same phrase, 6× | 13.1 s | 5.41 s | 4.80 s | ~0.61 s |

An earlier 2.5 s run measured 0.62–0.88 s total with 0.60–0.87 s STT; the
short clip is consistently under one second. Transcripts were exact on the
short clip; the repeated long clips were transcribed as repeated phrases and
correctly answered ("It's 10:23 AM.").

Transcription dominates at every length and grows with the clip. The Pi's STT
is slower than this desktop; that is the real risk, not the upload. On a phone
over the tailnet the upload is no longer loopback, but a 2 s 16 kHz PCM clip is
64 KB, which is small next to the STT cost even on a slow link.

## The browser alternative

- **Web Speech API** is only present in some browsers, is a cloud service on
  Chromium (audio leaves the device to Google), and gives no control over
  vocabulary or the one-recording-at-a-time rule. It also disagrees with the
  Pi's STT settings, so "Voice" would mean two different things depending on
  the browser.
- **whisper.cpp WASM** keeps audio local but ships a model to every client,
  adds a large download and a second model to keep in sync, and runs on a
  phone CPU that is also power-constrained. A small model would have to be
  compared on the owner's voice before it could be trusted for a Tool call.
- Either path would need its own accuracy evaluation and a fallback rule. The
  measured server path is already sub-second for a normal request on the
  desktop; there is no user-visible problem for it to solve today.

## Decision

1. **Keep the server STT path as the default and the only path.** The browser
   keeps recording 16 kHz PCM, uploading one bounded clip, and showing the
   transcript. One recording at a time and no stored audio are unchanged.
2. **Do not add Web Speech or whisper.cpp WASM** without a measured Pi-side
   problem. The existing 150 ms semantic budget and the 20 s STT cap are the
   places a slow path would show up first.
3. **Revisit if** a phone over the tailnet shows a total request above roughly
   two seconds for a short utterance, or the Pi's STT becomes the latency
   bottleneck after other work. The first step then is a Pi measurement with
   the same table, not a code change.

## Reproduce

```sh
# Synthesize one utterance with the active voice.
curl -s -H "Tailscale-User-Login: $ASSISTANT_DASHBOARD_OWNER" \
  -H "Origin: http://127.0.0.1:8082" \
  "http://127.0.0.1:8082/settings/wording/preview?text=what%20time%20is%20it" \
  -o utterance.wav
ffmpeg -y -i utterance.wav -ar 16000 -ac 1 -f s16le utterance.pcm

curl -s -o response.json -w '%{time_total}\n' \
  -H "Tailscale-User-Login: $ASSISTANT_DASHBOARD_OWNER" \
  -H "Origin: http://127.0.0.1:8082" \
  -H 'Content-Type: audio/l16; rate=16000; channels=1; endianness=little-endian' \
  --data-binary @utterance.pcm http://127.0.0.1:8082/api/ui/ask/audio
```

The response's `Just.Stages` has the per-stage timings; the audio body is not
stored.
