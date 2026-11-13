# Equalizer setup and live checks

On a Debian/Raspberry Pi OS host, install the `caps` package (and
`pulseaudio-utils` for operator checks) using the host package manager.
The Pulse server must support `module-ladspa-sink`; PipeWire-Pulse implements
this module too. The assistant and Soloist images need no additional packages:
LADSPA is loaded by the host daemon, not by either container.

Keep the configured playback sink and Soloist's `ASSISTANT_PULSE_SINK` on the
original `aec_music_sink` when AEC is in use. Leave native Soloist PipeWire
routing disabled. The controller inserts EQ ahead of that sink automatically.
Open Settings → Equalizer. Select a preset or release a band slider to save.
Custom survives selecting another preset. Positive boosts can clip at high volume; lower the speaker volume before
boosting bands.

Owner checks on the Pi (not performed by the implementation agent):

- With music already playing, switch Flat/Bass Boost/Vocal Clarity/Treble Boost;
  confirm active music changes without receiver or Assistant restart.
- Adjust all ten bands with pointer and keyboard; select Flat then Custom and
  confirm Custom returns. Reload the page, restart the assistant container,
  then restart the host and confirm the active profile and Custom survive.
- Play a TTS reply, wake Cue, Timer and Alarm; confirm all use EQ and that
  volume, mute, ducking and stop still work.
- Inspect Pulse sinks and sink inputs: only one managed EQ should remain;
  its output must enter `aec_music_sink`, whose output enters the real speaker.
  Test wake detection and Barge-in over music to validate the AEC reference.
- Stop/start host audio and reconnect Bluetooth; confirm recovery or a visible
  Equalizer Health error with audible flat playback.
- In a maintenance window, make CAPS unavailable and change preset: confirm
  audible flat playback, a Health failure and recovery after restoring CAPS.
  Avoid removing a library currently in use by the audio server.
- Check the mobile Settings layout and keyboard slider labels/values.

## Isolated native validation

The Go package includes `TestNativePulse`, skipped during normal tests. Compile
the package test binary with CGO disabled, then run it inside a disposable
container with PulseAudio, pulseaudio-utils and CAPS installed. Start a private
Pulse daemon there and set `CLANKA_ISOLATED_PULSE_TEST=1` only for that test.
It creates a null master and verifies filter loading, preset replacement,
Assistant restart recovery, output routing and cleanup. Never enable this
integration test against the owner's real Pulse session.
