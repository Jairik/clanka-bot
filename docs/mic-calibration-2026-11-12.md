# Dev-box C270 calibration (#36)

On 2026-11-12, calibrated the physical source
`alsa_input.usb-046d_0825_EC865510-02.mono-fallback` on `archlinux`.
The source was initially at 41%, rather than the original issue's 100%.

The shell needed `PULSE_SERVER=unix:/run/user/1000/pulse/native` to reach the
existing PipeWire PulseAudio server. No audio service restart was needed.

| Measurement | Before | After |
| --- | --- | --- |
| Source volume | 41% / -23.12 dB | **32% / -29.69 dB** |
| Idle RMS | -40.9 dBFS | **-47.4 dBFS** |
| Frame p50 / p95 / peak | -40.9 / -39.6 / -38.0 dBFS | -47.4 / -46.5 / -45.7 dBFS |
| Frames above -38 dBFS | 0 / 125 | **0 / 188** |

Samples are 32 ms mono PCM16 frames at 16 kHz. Recording windows were requested
as 5 s and 8 s; the table reports the actual captured frame counts after capture
startup. Changed only the host source volume; the product threshold remains
**-38 dBFS** and the silence window remains **500 ms**.

```sh
# Recheck the calibrated source through the existing PulseAudio socket.
PULSE_SERVER=unix:/run/user/1000/pulse/native \
  scripts/mic-noise-check.sh alsa_input.usb-046d_0825_EC865510-02.mono-fallback 8
```

## Human-spoken verification

The user said “Okay Nabu” and asked the time, then confirmed hearing a reply.
The existing `clanka-bot-deploy-assistant-1` container recorded:

- `2026-11-12T21:31:24.629890774Z`: `voice: wake`, model `okay_nabu`.
- Request `be6bf918bb52fc11d78d5523046620e6`: `voice: endpoint`, `speech=true`,
  `silence_ms=512`, **`endpointing_ms=501`**.
- Transcript: `What time is it? What time is it?` (the phrase was captured twice).
- Final request: `source=voice`, `outcome=ok`; the user heard the reply.

This validates the physical mic correction and the silence endpoint rather than
the 15 s cap. The deployed assistant is a newer build than this checkout
(54 Tools at startup); this was a host calibration check, not a deployment of
the weather changes. The repeated transcript took the Agent path and had
`response_ms=4631`, so this does not establish Fast path response latency or
complete the full #29 human acceptance run. Gain was still 32% after the turn;
persistence across reboot has not been tested.
