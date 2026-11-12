# Spotify plays on the Pi through librespot into the Assistant's own sink

Music plays through a librespot (Spotify Connect) service on the Pi, whose output goes to the same Pulse sink as the Assistant's speech. The Spotify Web API is used only for control (play, search, queue, now playing). We chose this so that ducking for a reply, a Ring or the wake word, and the Focus rules for "pause"/"next", are local volume changes the Assistant makes instantly. They aren't Web API calls that can lag or fail. It also gives echo cancellation on the host a reference signal for the music, which a separate speaker can't provide.

## Considered Options

- **The Echo Dot as its own Spotify Connect device:** rejected. Every duck becomes a Web API volume call, the Assistant can't hear what's playing, and wake-word detection over the music has no reference to cancel against.
- **Any device the user picks:** deferred, not rejected. "Play on my desktop" can be added later as a playback transfer; when music plays elsewhere, it simply has no Focus on the Pi.

## Consequences

- One more always-on container (librespot) in the Pi's RAM budget.
- Requires Spotify Premium for both librespot and Web API playback control (confirmed).
