# Phrase book: short deterministic replies

The Fast path answers a Tool call with a fixed sentence rendered in Go. Many of
those sentences are only an acknowledgement ("Timer set for ten minutes.",
"Okay, switching to the GLaDOS persona."). The phrase book replaces the
acknowledgement with the shortest useful line and lets each Persona say it in
character.

## Two layers

1. **Shipped default.** `assistant/internal/phrasebook` holds one terse phrase
   per overridable Tool (`timer.set` → "Set.", `spotify.play` → "Playing.").
   This layer applies even when no Persona is selected, so the assistant is
   brief out of the box.
2. **Persona override.** A Persona may replace any of those phrases. The
   Phrase book page edits them, and "Generate from persona" asks the one Agent
   provider for a complete in-character set. Selecting a Persona applies its
   overrides to the next reply; no restart is needed.

An override is stored in the `persona_phrases` table (migration 13) keyed by
Persona and Tool. The registry asks the phrase book through
`Registry.SetSpeaker`; the Tool's own `Speak` renderer still runs first, and
the hook only replaces the text for a Tool that is on the overridable list.

## What is overridable

Only acknowledgements. Content-bearing replies stay exactly as the Tool
rendered them, because shortening them would delete the answer:

- overridable: `timer.set`, `alarm.*`, `lights.*`, `audio.*`, `quiet.*`,
  `voice.set/rate`, `reminder.set/cancel`, `notify.phone`, `persona.switch`,
  all acting `spotify.*` Tools, `note.remember/forget`, `list.add/remove`.
- never overridable: `time.*`, `date.*`, `weather.current`, `timer.remaining`,
  `timer.list`, `alarm.list`, `reminder.list`, `quiet` reads, `voice.list`,
  `voice.current`, `assistant.repeat`, `assistant.help`, `system.status`,
  `calc.evaluate`, `units.convert`, `random.*`, `persona.list`,
  `persona.current`, `spotify.now_playing`, `note.recall`, `list.read`, and
  `github.issue_create` (it speaks the created issue).

A new Tool that only acknowledges an action should add one entry to
`defaults` in `internal/phrasebook`. The slot list the Dashboard and the
generator use is derived from the registry, so no second list to update.

## Generating a set

"Generate from persona" sends the Persona's name and personality prompt plus
every overridable Tool's description and shipped default to the configured
Agent model. The prompt asks for 1–4 words per Tool, in character, with no
invented facts. The response is validated: unknown Tools are dropped, empty or
over-long values are dropped, and only then does one transaction replace the
Persona's existing overrides. The new set is live immediately; the page is where
the owner reviews it, and each row can be edited or reset afterwards.

Generation uses `Agent.Complete`, a non-streaming call with the same provider,
key, model and forced ZDR / data-collection-denial fields as a normal turn. It
does not read or write conversational history and adds no Tools. A missing
provider key is a clear page error, not a retry loop.

Replacing a set **overwrites manual edits** for that Persona. The page says so
before the button runs. Reset one row to restore the shipped default; "Clear
all overrides" removes the Persona's whole set.

## Loading the starter Personas

`personas/starter.json` holds the owner's starter set (Scuzz, Sneer, Bark,
Biff, Blank), each with all 35 phrase keys. Load it into a running Dashboard:

```sh
ASSISTANT_DASHBOARD_OWNER=you@example.com \
  python3 scripts/seed-personas.py --base http://127.0.0.1:8082 \
  --file personas/starter.json
```

The script uses the Dashboard's own authenticated form API; it never opens the
database and never prints a secret. It updates an existing Persona instead of
failing on a duplicate name, writes every phrase, and can activate one with
`--activate Scuzz`. Use `--dry-run` first.

## Limits

- Max 80 characters per phrase, one line. The reply is spoken, not a
  paragraph.
- Only successful Fast path replies are replaced. Error text, confirmation
  questions, Focus-intent replies and Agent prose are untouched.
- The hook applies to `assistant.repeat` history because history records the
  spoken text, not the Tool's original sentence.
- A phrase book is local data. It is never included in an Agent prompt; the
  generator sends the Persona prompt and Tool descriptions, not other phrases
  or history.
