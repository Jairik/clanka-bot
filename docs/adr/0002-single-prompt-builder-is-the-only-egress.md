# A single prompt builder is the only way personal data leaves the Pi; ZDR is required in code; no redaction layer

Everything sent to the Agent path's model goes through one prompt builder. It builds the request from labelled parts: system prompt, history turns, and Tool results. It drops any part that came from a Local-only Tool. Every Tool must say whether it is local-only when it is registered; there is no default. Local-only Tools are never offered to the agent or served over MCP. Every provider request sets OpenRouter's `provider.zdr: true` and `provider.data_collection: "deny"` in code, not in config, and the account also enforces "ZDR endpoints only". If no host qualifies, the request fails and the Unavailable message plays, with no failover. We chose this so the privacy line sits in one place you can audit, where an omission means "not sent" rather than "leaked".

## Considered Options

- **A redaction layer that scans outgoing text (regex or entity detection):** rejected. It's brittle and gives false confidence, because private details survive in paraphrase. The utterance alone, such as "read me the email from my doctor", can reveal them.
- **ZDR as a config flag:** rejected. One edited setting would quietly send personal data to hosts that keep it. The request flag can only tighten what the account enforces, never loosen it.
- **Loading schedule and email into the system prompt ahead of time:** rejected in favour of need-to-know. Personal data leaves only through Tool calls the agent makes during a request the user made.

## Consequences

- The policy applies to the data, not to a provider. Hermes on OpenCode Go must reuse the same filtering, and must not expose Local-only Tools over MCP.
- Local-only Tool turns stay in the history and routing log on the Pi, but are left out of history sent off the device.
- Detail: [Privacy line for the Agent path](https://github.com/Jairik/clanka-bot/issues/11).
