# OpenCode Go as the Agent path provider

Research for [#6](https://github.com/Jairik/clanka-bot/issues/6). Researched 2026-11-11. Docs source last changed 2026-11-10 (commit `23eab68a3c` of `services/site/src/docs/content/console/go.mdx` in `anomalyco/opencode`). Terms of Service effective 2026-11-08.

No credentials were used. Live probes hit only the public model list and unauthenticated error responses.

## Question

What does OpenCode Go offer as the Agent path's model provider (`opencode-go/` models)? Which models, what API shape (endpoint, auth), tool calling and streaming, rate limits, quota and cost, latency to first token, and any terms that restrict use from a self-hosted app?

## Answer

- **It is a plain HTTP API that the Assistant can call directly.** The base URL is `https://opencode.ai/inference/go`, and auth is `Authorization: Bearer <Console API key>`. Each model is served in one native format: OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, or Gemini. The docs say Console translates every text model into the other formats too. The DeepSeek models use **OpenAI-compatible Chat Completions** at `https://opencode.ai/inference/go/openai/v1/chat/completions`. [1][2][3]
- **The `opencode-go/` prefix is only an OpenCode config convention.** Over the wire the model ID has no prefix, for example `"model": "deepseek-v4-pro"`. [1][4]
- **Every request must carry an `x-opencode-session` header.** A request without it gets HTTP 400 `MissingSessionID` (verified live, 2026-11-11). The docs also ask each client to send its own User-Agent (for example `clanka-bot/0.1`) and "typical coding agent traffic." [1][2][probe]
- **Tool calling is supported on all 30 Go models.** The live `/v1/models` metadata reports `capabilities.tools.supported: true` for each one. For the DeepSeek models it lists tool_choice `auto|none|required|tool`, structured output, and reasoning returned in `reasoning_content`. [5]
- **Streaming is supported.** The inference docs describe streamed responses and where cost is reported in a stream, for example the final usage chunk when Chat Completions uses `stream_options.include_usage`. [3]
- **Quota is measured in dollars, not requests.** Go costs $10/mo, Go Plus $40/mo, and Go Teams $20 per member per month. Each model has a monthly dollar allowance. Within it, a rolling 5-hour window allows 20% and a week allows 50%. There is no documented RPM or concurrency limit. Optional "Extra Usage" falls back to a pay-as-you-go balance once the limits are hit. [1]
- **Terms: one soft blocker and one clause to watch.**
  - The Go page says Go is "designed for OpenCode and other coding agents that produce similar types of requests," and that traffic "is monitored for abuse."
  - A voice assistant is not a coding agent. Nothing explicitly forbids it, but it falls outside the stated intent, so there is account risk.
  - The general ToS bans "any processes that run or are activated while you are not logged into the Services" and programmatic extraction of Output.
  - That is generic boilerplate, but unattended Briefings and Notifications calls are the use it could be read against. [1][6]
- **Free models are probably unavailable to the Assistant.** "Free-tier chat models are only available from within OpenCode; other clients receive HTTP 403, even with an API key." [3][7]

**Bottom line:** OpenCode Go works technically as an OpenAI-compatible, tool-calling, streaming provider. The Assistant would call DeepSeek V4.1 Flash or V4 Pro directly with a Bearer key, an `x-opencode-session` header, and its own User-Agent. The main risk is policy, since Go is positioned for coding agents. Another option is to run the Agent path through a validated client such as Hermes Agent (v0.21.6 or later), which sends the session header itself. [1][7]

## API shape

| Item | Value |
|---|---|
| Base URL (Go) | `https://opencode.ai/inference/go` (pay-as-you-go Console is `https://opencode.ai/inference`) [2] |
| Chat Completions | `/openai/v1/chat/completions` |
| Responses | `/openai/v1/responses` |
| Anthropic Messages | `/anthropic/v1/messages` |
| Gemini | `/google/v1beta/models/<model>:generateContent` (or `:streamGenerateContent`) |
| Model list | `GET https://opencode.ai/inference/go/v1/models` (public, no auth) [1][5] |
| Auth | `Authorization: Bearer <token>`, using a personal API key or a service account key [3]. Note that Go Teams does not cover service accounts. [1] |
| Required header | `x-opencode-session: ses_...`, stable for each conversation. It is used for routing and prompt caching, and requests without it return 400. [2][probe] |
| Requested header | Its own User-Agent, not a generic SDK name [1] |
| Auth error | Missing key returns `{"error":{"type":"authentication_error","message":"OpenCode Go requires an API key"}}` [probe] |
| Cost reporting | Each response's `usage` has `cost`, `opencode.billing_source` (`go`/`go-plus`/...), `opencode.list_cost`, and `opencode.allowance`, which is how many dollars of the plan's monthly limit the request used. [3] |
| AI SDK package for DeepSeek | `@ai-sdk/openai-compatible`; models.dev registers provider `opencode-go` with `api: https://opencode.ai/inference/go/openai/v1` and env `OPENCODE_API_KEY` [4] |

Example request (shape only, from [3] with the Go base URL and session header):

```bash
curl -X POST https://opencode.ai/inference/go/openai/v1/chat/completions \
  -H "Authorization: Bearer $OPENCODE_API_KEY" \
  -H "x-opencode-session: ses_<stable-per-conversation-id>" \
  -H "User-Agent: clanka-bot/0.1" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[...]}'
```

## Models (Go plan, as of 2026-11-11)

All 30 models report tool support in the live list [5]. Prices are per 1M tokens (input/output). The "Go monthly limit" column is the $10 plan's allowance for that model; Go Plus is higher (for example DeepSeek V4.1 Flash $200 and V4 Pro $60) [1].

| Model ID | API | In / Out $ | Go monthly limit | Est. requests per 5h on Go [1] | Notes |
|---|---|---|---|---|---|
| `deepseek-v4.1-flash` | chat-completions | 0.15 / 0.60 (peak 2x) | $100 | 43,330 | 1M ctx, image input, reasoning effort low/high/max. Requires the **Global** region setting. |
| `deepseek-v4-pro` | chat-completions | 0.66 / 1.98 (peak 2x) | $15 | 1,050 | 1M ctx, text only, reasoning effort high/max. Requires the **Global** region setting. |
| `glm-5.3-flash` | chat-completions | 0.15 / 0.50 | $60 | 6,320 | |
| `glm-5.3`, `glm-5.2` | chat-completions | 1.40 / 4.40 | $15 / $60 | | |
| `kimi-k3.1-pro`, `kimi-k3`, `kimi-k2.7-code` | chat-completions | 2/10, 3/15, 0.95/4 | $15 / $15 / $60 | Kimi K3: 110 | |
| `mimo-v2.6-flash`, `mimo-v2.6-pro`, `mimo-v2.5`, `mimo-v2.5-pro` | chat-completions | 0.14/0.28 to 0.435/0.87 | $60 / $15 / $60 / $15 | MiMo-V2.6-Flash: 30,100 | |
| `minimax-m3.1-flash` | chat-completions | 0.15 / 0.60 | $20 | | |
| `minimax-m3` | messages | 0.30 / 1.20 | $60 | | |
| `qwen3.8-max`, `qwen3.8-flash`, `qwen3.7-plus` | messages | 2/6, 0.15/0.47, 0.40/1.60 | $15 / $30 / $60 | Qwen3.8 Flash: 5,400 | |
| `claude-haiku-5-5` | messages | | $15 | | 30-day retention |
| `grok-4.7`, `gpt-6-luna`, `gpt-5.6-luna` | responses | | $15 each | | 30-day retention |
| `muse-spark-1.3-contributor`, `muse-spark-1.2-contributor` | responses | 0.10 / 0.20 | $60 | | **Trains on your prompts.** Off by default. |
| `gemini-4-argon` | generate-content | 2 / 10 (50% off until 2026-11-30) | $30 | | |
| `longcat-2.0` | chat-completions | 0.30 / 1.20 | $60 | | |
| `hy4`, `hy4-preview`, `hy3` | chat-completions | | $60 / $30 / $60 | | |
| `step-5-preview` | chat-completions | 1.00 / 2.70 | $15 | | |
| `beam-free`, `iceman-contributor-free`, `longcat-2.5-free` | chat-completions | free | unlimited, limited time | | Probably 403 outside OpenCode (see uncertainties) |

DeepSeek peak hours are 01:00–04:00 and 06:00–10:00 UTC, Monday to Friday, and prices double during them [1][5]. For a US/Eastern user, 06:00–10:00 UTC is 01:00–05:00 or 02:00–06:00 local time, so peak pricing rarely affects interactive use.

**Practical sizing.** DeepSeek V4.1 Flash on the $10 plan allows about 43k requests per 5 hours by OpenCode's estimate for typical coding-agent requests. A voice assistant's Agent path will not get near that. DeepSeek V4 Pro allows about 1k requests per 5 hours, which is still ample.

## Privacy

- Default workspace regions are Europe and United States. DeepSeek models only become available after an owner or admin selects **Global** under Settings › Privacy › Regions. [1]
- DeepSeek models: not used for training, 0-day retention, under a ZDR agreement "renewed monthly" and stated as valid through 2026-11-30. [1]
- OpenAI, xAI, and Anthropic models: 30-day retention. Muse Spark Contributor models train on request data. [1]

## Terms relevant to a self-hosted app

- Go page, "Where can I use it?": "OpenCode Go is designed for OpenCode and other coding agents that produce similar types of requests. Traffic is monitored for abuse that degrades the experience for other users." It lists validated third-party clients: Hermes, Claude Code, Codex, ZCode, Pi, jcode, and Kilo Code CLI. [1]
- ToS prohibited-use list: "automatically or programmatically extracts data or Output", "any processes that run or are activated while you are not logged into the Services", and "uses Output to develop artificial intelligence models that compete with the Services". It also says "You will only use the Services for your own internal use", which a single-user Assistant satisfies. OpenCode may suspend accounts "for any reason in our discretion." [6]
- The ToS has no API-specific carve-out. However, OpenCode's own docs give direct API usage, curl examples, and AI SDK packages "for applications calling Go directly," so programmatic API use is clearly intended. [1][3]

## Latency to first token

**Not published.** No official TTFT or latency figure exists. The Go page only says it aims for "reliable, low latency access" [1]. Measuring it needs a key, which was out of scope here; see the uncertainties below.

## Open uncertainties

1. **Is a voice assistant acceptable traffic?** The docs do not ban it, but they position Go for coding agents and monitor for abuse. To reduce risk: send a clear User-Agent and per-conversation session IDs, keep volume low, or ask OpenCode support. Routing through Hermes (a validated client) stays closer to the stated intent.
2. **TTFT from the Pi** is unmeasured. Run a short probe with a real key, checking DeepSeek V4.1 Flash in particular and comparing peak with off-peak hours.
3. **Go's free models from non-OpenCode clients.** The inference docs and the Hermes page say free-tier chat models return 403 outside OpenCode. It is not stated whether Go's "Free" models count as "free-tier." Assume they are unavailable.
4. **Per-minute and concurrency limits** are undocumented. Only dollar allowances (5h/week/month) are published.
5. **Volatility.** "The list of models may change," and limits "may change as we learn from early usage" [1]. The DeepSeek ZDR terms renew monthly. Pin the model ID in config and re-check this page before building.
6. Whether a session ID must be a `ses_`-prefixed value or can be any stable string. The docs show the format `ses_...`, but nothing says another format is rejected.

## Sources

1. OpenCode Go docs: https://opencode.ai/docs/go/ (source: https://github.com/anomalyco/opencode/blob/dev/services/site/src/docs/content/console/go.mdx, last commit 2026-11-10). Accessed 2026-11-11.
2. Harnesses, Base URLs, and session header: https://opencode.ai/docs/console/harnesses (source `services/site/src/docs/content/console/harnesses/index.mdx`). Accessed 2026-11-11.
3. Inference API (auth, endpoints, streaming cost fields, free-tier 403): source `services/site/src/docs/content/console/api/inference.mdx` in https://github.com/anomalyco/opencode. Accessed 2026-11-11.
4. models.dev registry (`opencode-go` provider entry): https://models.dev/api.json. Accessed 2026-11-11.
5. Live Go model list: https://opencode.ai/inference/go/v1/models (unauthenticated GET, 2026-11-11, 30 models).
6. OpenCode Terms of Service: https://opencode.ai/legal/terms-of-service (effective 2026-11-08). Accessed 2026-11-11.
7. Hermes harness page: source `services/site/src/docs/content/console/harnesses/hermes.mdx` (Hermes v0.21.6+ sends the session header; provider `opencode-go`, env `OPENCODE_GO_API_KEY`).
8. OpenCode Console/Zen models page: https://opencode.ai/docs/zen/. Accessed 2026-11-11.

[probe]: Unauthenticated POSTs to `https://opencode.ai/inference/go/openai/v1/chat/completions` on 2026-11-11. With no session header the response was 400 `MissingSessionID`. With a session header but no key it was `authentication_error`.
