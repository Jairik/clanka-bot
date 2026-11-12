# Audio test fixtures

`tone.mp3` is a 0.6 s, 440 Hz, 32 kbit/s stereo MP3 used by the pure-Go MP3
decode and upload tests.

- Generated on 2026-11-12 with:
  `ffmpeg -f lavfi -i "sine=frequency=440:duration=0.6" -ar 44100 -ac 2 -b:a 32k -map_metadata -1 -fflags +bitexact -flags:a +bitexact tone.mp3`
  (ffmpeg n9.0.2). ffmpeg is a fixture generator only; the assistant decodes
  MP3 at runtime with `github.com/hajimehoshi/go-mp3` v0.3.4, Apache-2.0.
- SHA-256: `05fb5db459cf0f959307b77f89c5136247983d835be96a9f311f0bb20154ea7a`
- The source is a synthesised sine wave created by the command above, so no
  third-party recording, performance or metadata is included. The fixture is
  dedicated to the public domain (CC0 1.0).

WAV fixtures are synthesised in the tests themselves; no WAV file is committed.
