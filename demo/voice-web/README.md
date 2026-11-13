# Cloned-voice web demo

A local page for trying the Assistant's cloned Pocket TTS voices: type a line,
pick a voice, hear it. It uses the same engine as the `tts` service — Pocket TTS
(Kyutai 100M) on CPU — but loads all six `.safetensors` clones instead of the one
voice the deployed service ships with.

## Run it

```sh
python3 demo/voice-web/server.py --open
```

Then type at <http://127.0.0.1:8090/>. The first start loads the model
(~10 s, printed progress); after that each line is generated at roughly
3–4x real time on CPU.

No Python packages are needed — only the standard library. It expects the local
voice bundle from `VOICE-CLONING.md`:

| What | Default |
|---|---|
| Voices (`<name>.safetensors`, `<name>_demo.wav`) | `~/.local/share/clanka-bot/voices` |
| `pocket-tts` binary | `~/.local/share/clanka-bot/bin/pocket-tts` |

Override with `--voices-dir` / `--pocket-tts` (or `VOICE_DEMO_VOICES_DIR` /
`VOICE_DEMO_POCKET_TTS`). Voices are discovered from the directory, so a
re-cloned voice shows up on the next start with no code change.

## How it works

```
browser ──> server.py ──> pocket-tts serve   (localhost:8019)
               │                 │
               └── serves each voice's .safetensors over loopback
```

`server.py` starts `pocket-tts serve` as a child process and exposes the local
`.safetensors` files to it at `/internal/voices/<name>.safetensors`, because the
built-in server only accepts `http://`, `https://` or `hf://` voice URLs — not
local paths. The page talks to a small JSON API:

- `GET /api/voices` — the discovered voices, with demo-clip durations
- `POST /api/speak` `{"text": "...", "voice": "cartman"}` — returns `audio/wav`
- `GET /voices/<name>_demo.wav` — the preview clips

Useful flags: `--port` (demo, default 8090), `--model-port` (private Pocket TTS
server, default 8019), `--host`, `--open`, and `--model-url` to reuse an
already-running `pocket-tts serve` instead of starting one.

## Privacy

The voices are clones of other people's voices for private use. They live
outside the repo and are never committed, baked into an image, or uploaded.
This demo binds to loopback by default and only serves the `.safetensors` files
to loopback clients; keep it that way if you pass `--host`.
