# Timer and Alarm completion audio

On Scheduled alerts, schedule a Timer (seconds) or Alarm (local HH:MM), or
edit a pending alert's Completion audio. Saving an empty value restores its
standard alert. Supported values:

- A song name, searched on Spotify.
- A Spotify track, album or playlist URL (or `spotify:` URI).
- `playlist: Running`, resolved against the existing cached playlist library
  through the shared Name matcher. `my running playlist` works too.
- An HTTPS YouTube video URL (`youtube.com/watch?v=…` or `youtu.be/…`).

Playlists shuffle. Audio replaces TTS and the normal Cue, holds Ring Focus,
and stops on stop, cancel, snooze, shutdown or the ten-minute limit. If media
cannot load, the normal spoken alert is used. Spotify starts are checked after
two seconds because Web API acceptance alone does not prove receiver playback.
The account-side failure tracked in #71 can still prevent live Spotify audio.

Say “set a timer for 15 minutes and play Here Comes the Sun” or “set an alarm
for 7 and play my running playlist”. The Tool inputs accept `audio`; the Agent
path handles compound wording, and timer/alarm extraction also preserves the
completion clause without treating digits in song titles as a duration.

The assistant runtime image includes yt-dlp, ffmpeg and pacat. YouTube audio
streams through child-process pipes to the configured local Pulse sink; every
child is cancelled and reaped before Ring releases the speaker.
