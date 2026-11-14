# Spotify plays on the Pi through a Connect receiver into the Assistant's own sink

Music plays through a Spotify Connect receiver on the Pi (Spotify's official Soloist binary), whose output goes to the same Pulse sink as the Assistant's speech. The Spotify Web API is used only for control (play, search, queue, now playing). We chose this so that ducking for a reply, a Ring or the wake word, and the Focus rules for "pause"/"next", are local volume changes the Assistant makes instantly. They aren't Web API calls that can lag or fail. It also gives echo cancellation on the host a reference signal for the music, which a separate speaker can't provide.

## Considered Options

- **The Echo Dot as its own Spotify Connect device:** rejected. Every duck becomes a Web API volume call, the Assistant can't hear what's playing, and wake-word detection over the music has no reference to cancel against.
- **Any device the user picks:** deferred, not rejected. "Play on my desktop" can be added later as a playback transfer; when music plays elsewhere, it simply has no Focus on the Pi.

## Consequences

- One more always-on container (`soloist`) in the Pi's RAM budget.
- Requires Spotify Premium for both the receiver and Web API playback control (confirmed).
- Soloist is proprietary. The operator downloads its official archive on the deployment host; the repository and images carry no binary or API key (see `soloist/README.md`).
- An official build expires. Health reports the remaining days, and an expired build is a failure until the operator runs `make soloist-update`.

## Implementation note

The original implementation used librespot (a from-source open-source receiver). Issue #71's live compatibility work replaced it with Spotify's Soloist because the account's audio keys were refused by librespot (#71). The optional native PipeWire route (`-d`) bypasses the Pulse socket, so the supported Compose service leaves it off and lets Soloist use the Pulse default sink, the same sink the Assistant uses; `pactl` ducking then matches its `spotify` application name. `SPOTIFY_PIPEWIRE_DEVICE` remains available for explicit native routing, where ducking would need a PipeWire-level controller.

The `soloist` Compose service uses host networking because Docker's bridge network hides mDNS multicast; Soloist advertises the device itself, so no container Avahi or D-Bus mount is needed. Only the metadata-only status directory is shared with the Assistant; the paired data and API key are not.
