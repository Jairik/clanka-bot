# Global EQ precedes the echo-cancel reference

Issue #143 uses one stereo CAPS Eq10X2 LADSPA filter, hosted by the existing
PulseAudio or PipeWire-Pulse server. The ten octave bands span 31 Hz–16 kHz
with integer gains from -12 to +12 dB. Flat, Bass Boost, Vocal Clarity and
Treble Boost are built in; Custom is saved separately from the active profile.
Both profiles live in the assistant's SQLite settings store (ADR-0003).

The supported graph is all playback → EQ → `aec_music_sink` → real output.
`playback.sink` / `ASSISTANT_PULSE_SINK` must identify the original AEC sink
when echo cancellation is enabled. EQ must not go after the AEC sink: that
would change the speaker signal after the reference was taken. With no AEC,
the configured sink (or the original server default) is the EQ master.
The microphone source and AEC module are never rebuilt or modified.

New Assistant speech, Cue and Ring streams resolve the active EQ destination
when opening. EQ becomes the server default for Soloist and other clients.
A 500 ms reconciliation also moves active streams, including Soloist streams
explicitly pinned to the configured master. Such pinned new streams can play
flat for up to one reconciliation interval. Native PipeWire playback, remote
Spotify devices and clients explicitly targeting another physical sink bypass
this Pulse graph and are outside the supported routing setup.

LADSPA control values are module arguments. Live changes load the alternate
`clanka_eq_a` / `clanka_eq_b` sink, move active streams, then unload the old
module. This may cause a brief discontinuity; it does not restart the Assistant, AEC or receiver.
Filter outputs are excluded from moves so the graph cannot feed itself.
A failed filter load restores the original default and unloads the previous
filter; playback uses the original flat path and Health reports the failure.
The worker retries, restores saved settings after host audio restarts, and
recovers its previous master from its module arguments after Assistant restarts.

The host audio server loads plugins in its own filesystem. Installing CAPS
inside the assistant or Soloist Dockerfile would have no effect. Install the
host package as described in [audio setup](../audio/equalizer.md); containers
already carry the required `pactl` client. No audio server is added to Compose.

References: [CAPS ports](https://quitte.de/dsp/caps.html#Eq10X2),
[PulseAudio LADSPA module](https://www.freedesktop.org/wiki/Software/PulseAudio/Documentation/User/Modules/#module-ladspa-sink).
