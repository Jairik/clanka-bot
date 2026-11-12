# The Go middleware owns all audio I/O; model services never touch audio devices

Only the Go `assistant` process opens the mic and the speaker (one named Pulse source and one named sink, set in config). It streams PCM to the wake word, STT and TTS services over Wyoming, and plays the PCM that TTS sends back. Those containers do computation only and never mount the audio socket, even though openWakeWord and Pocket TTS can each open a device themselves. We chose this so one process always knows when the Assistant is speaking. That lets it mute or keep listening during playback and support Barge-in (stop playback when the wake word fires) without signals between processes. It also keeps the audio path in our own code, so replies can later go to a Satellite instead of the local speaker. Any model service can be swapped for another Wyoming server without touching audio setup.

## Considered Options

- **A separate capture service fanning mic audio to wake word and STT:** rejected. It's one more process, and the "who hears what, when" logic would sit away from the rest of the conversation flow.
- **TTS plays audio itself:** rejected. Go would then need a signal to know when playback starts and stops, and Satellite routing would have to live inside the TTS wrapper.

## Consequences

- Echo cancellation is not done in Go. Go assumes its source is already cleaned, either by the mic's hardware or by PipeWire's `echo-cancel` module on the host. Where that happens is decided in the Hardware plan.
- Only the `assistant` container mounts the PulseAudio socket. This supersedes the compose sketch in the Docker emulation research, where `wakeword` and `tts` mounted it.
- [ADR-0004](0004-spotify-plays-through-the-pi.md) adds one deliberate second Pulse client: the `librespot` music container. It is not a model service and it never opens the mic; it exists so music can be ducked locally. The ML containers (`wakeword`, `stt`, `tts`, `embed`, `web-search`) still never touch an audio device.
