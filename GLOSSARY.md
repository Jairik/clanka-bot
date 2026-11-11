# Assistant

A self-hosted personal voice assistant centred on a Raspberry Pi 5: it hears requests, resolves them, and answers by voice or on screen. (Working name; the product may be renamed later.)

## Language

### Requests

**Tool**:
A single named operation the Assistant can perform, such as `calendar.get_events`, available to both the Fast path and the Agent path. Tools are leaf operations: a Tool never calls another Tool.
_Avoid_: Skill, command, action, plugin

**Permission level**:
How much a Tool may change the world: `read` (no side effects), `act` (harmless or easily undone side effects), or `confirm` (hard to undo or outward-facing, needs a Confirmation).
_Avoid_: Scope, access level, risk tier

**Local-only Tool**:
A Tool whose results must never leave the Pi, so the Agent path can neither see nor call it.
_Avoid_: Private tool, offline tool, sensitive tool

**Confirmation**:
The user's spoken yes that a `confirm` Tool call needs before it runs. It lasts one turn, and anything other than a clear yes drops the call.
_Avoid_: Approval, consent, verification

**Fast path**:
Handling a request by matching it straight to a Tool on the device, without the general-purpose agent. The match may be by meaning, not only exact wording; a Control phrase is also part of the Fast path.
_Avoid_: Deterministic route, local intent

**Control phrase**:
A short fixed utterance, such as "stop" or "louder", that maps directly to a Tool call without matching by meaning.
_Avoid_: Hotword, command word

**Agent path**:
Handling a request by handing it to the general-purpose agent, which may call Tools as part of its answer.
_Avoid_: LLM fallback, Hermes route

**Agent request**:
An utterance that starts with a fixed prefix such as "ask the agent", which sends the request straight to the Agent path.
_Avoid_: Escape phrase, override

**Barge-in**:
Interrupting the Assistant mid-reply by saying the wake word, which abandons the current reply and starts a new request.
_Avoid_: Interrupt, cut-off

**Response latency**:
The time from the user's last spoken sound to the first sound of the Assistant's actual reply. Chimes and fillers played while waiting don't count as the reply.
_Avoid_: Lag, delay, turnaround

**Filler**:
A short phrase, such as "one moment", spoken while the user waits: when a reply is slow to start, or as soon as a request goes to the Agent path, so the user knows it is leaving the device. At most one per request.
_Avoid_: Holding message, stall phrase

**Unavailable message**:
A configurable phrase spoken when the Agent path can't reach its model, so the user knows the request wasn't handled. The Fast path keeps working.
_Avoid_: Error message, brain-offline line

### Devices

**Satellite**:
Any device other than the Pi that a user speaks, types to, or listens through: the phone web app, a desktop client, or a future wireless microphone.
_Avoid_: Client, endpoint, remote

### Outputs

**Briefing**:
A spoken or displayed summary composed from the results of several Tools, such as the morning schedule plus important email.
_Avoid_: Digest, report, summary

**Notification**:
An alert the Assistant pushes unprompted to a Satellite or display, subject to cooldowns.
_Avoid_: Alert, push, ping
