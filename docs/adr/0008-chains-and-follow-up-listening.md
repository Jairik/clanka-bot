# Chains and follow-up listening share the existing request pipeline

The middleware plans conjunctions before running any Tool. Each clause must
independently pass the existing semantic confidence, extraction and Permission
checks (or match an exact Control phrase). Only command-leading suffixes are
split: artist names, playlist names, durations and labels retain their `and`.
Uncertain plans go intact to the Agent path. Explicit Agent prefixes and pending
Confirmations keep their precedence. A chain stops on a Tool or playback error;
each successful step drains its reply audio before the next Tool runs. Dashboard
text chat executes the same ordered Tools without Pi audio.

`conversation.chains` defaults on. `conversation.follow_up_seconds` defaults to
5, accepts 0–15, and 0 disables it. Settings use the Dashboard's existing live
HeroUI rows. Follow-up capture starts only after reply playback drains, plays the
wake Cue, rejects buffered TTS/Cue frames and allows a 300 ms acoustic tail.
It times out if speech has not started. Music holding Focus suppresses the
window, including paused music. A bounded Pulse probe also suppresses it for receiver
streams started outside the Assistant, or when that probe fails: host AEC has not been calibrated for this mode.

Explicit `when the music stops, X`, `after the timer, X` and
`after this reply, X` requests create Deferred actions. The action must resolve
to safe non-confirm Tools at registration and goes through ordinary routing
and current Permission checks again when dispatched. The queue is in memory,
bounded to eight entries, and expires entries after one hour; restart drops it.
Completion consumes entries before dispatch, so events never execute one twice.
Timer completion means the Ring has ended, including stop or max-ring; it does
not mean a pending timer was deleted. Playback stop uses Music Focus release;
natural music endings are observed through the existing Spotify client every
two seconds. Reply completion uses Reply Focus release. A single dispatcher
executes triggered requests sequentially. Shutdown never starts timer actions.

The authenticated Dashboard Settings page lists pending entries and their
expiry and offers Cancel. `cancel queued actions` cancels every pending entry
by voice or chat. Consumed entries have begun dispatch and are no longer pending;
normal stop/Barge-in controls apply to their replies. No new leaf Tool is
introduced: chain planning, listening and completion dispatch belong to the
middleware, and Tools never call Tools.
