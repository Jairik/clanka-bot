# Agent runtime: Hermes Agent vs a Go tool-calling loop

Research for [#5](https://github.com/Jairik/clanka-bot/issues/5). Researched 2026-11-11.
Versions checked: Hermes Agent v0.23.1 (released 2026-11-10, `main` at `5df966a`), openai-go v3.98.0, MCP Go SDK v1.8.0, MCP spec 2026-07-28.

## Question

Should the Agent path run on Hermes Agent or on a thin tool-calling loop written in Go? Compare:

- what Hermes Agent is and how it runs (process, language, dependencies)
- its memory and CPU cost on a Pi 5
- how it would call the Assistant's Tools (MCP, HTTP or something else)
- whether it works with an OpenAI-compatible provider such as OpenCode Go
- which built-in features (memory, persona, web search) it would save us from writing

## Recommendation

**Write a thin tool-calling loop in Go inside the middleware.** Use `openai-go` pointed at OpenCode Go's Chat Completions endpoint, and dispatch tool calls straight into the middleware's own Tool registry. Write memory, persona and web search ourselves as small Tools and prompt fragments.

Keep Hermes as a fallback, not a dependency. Design the Tool registry so it can also be served over MCP with the official Go SDK. Hermes, or any other agent, could then be run later as a sidecar that consumes our Tools over MCP and is driven through its OpenAI-compatible API server. That move would not require rewriting the Tools.

Why:

1. **Hermes is a large Python agent platform, not a library.** It needs Python 3.14 plus Node.js, pnpm, ripgrep and FFmpeg. Its arm64 Docker image is about 1.1 GB compressed. It brings its own gateway, session store, cron scheduler, TUI, desktop app, skills hub and 40+ server-side tools, including terminal and file access. Most of that duplicates or competes with what the Go middleware is supposed to own: routing, sessions, notifications and scheduling.
2. **Our Tools would have to cross a process boundary.** Hermes's tools are server-side. Its API server does not run tools that the client defines, so the middleware's Tools would have to be exposed to it as an MCP server, over stdio or HTTP. In-process Go calls are simpler, and the Fast path already needs the same registry in Go.
3. **The features we'd save are small to write ourselves.** Hermes memory is two bounded Markdown files (2,200 and 1,375 characters) injected into the system prompt, plus a `memory` tool. Persona is a `SOUL.md` placed first in the system prompt. Web search is a `web_search` tool with pluggable backends. Each is a day or less in Go. The parts of Hermes that are genuinely big (self-improving skills, Honcho user modelling, subagents, Kanban, messaging gateways) are not things the prototype needs.
4. **Both options can use OpenCode Go.** Hermes has a built-in `opencode-go` provider. A Go loop can use `openai-go` with `option.WithBaseURL` against OpenCode Go's OpenAI-compatible endpoint. The provider is not a deciding factor.

## Comparison

| | Hermes Agent | Go tool-calling loop |
|---|---|---|
| What it is | "The self-improving AI agent" by Nous Research. MIT licence, Python. CLI/TUI, desktop app, messaging gateway and API server, all driving one `AIAgent` core. | About 200–400 lines in the middleware: a messages slice, then a loop of `chat.completions` → tool calls → results until a final answer or the max number of turns. |
| Process / language | A separate Python process (`hermes gateway`). Python `>=3.11,<3.15`, and only 3.14 is actively supported. The installer also pulls in Node.js, npm, pnpm, ripgrep and FFmpeg. | Runs inside the existing Go binary. No extra runtime. |
| Dependencies | Exact-pinned core (openai, httpx, pydantic, rich, prompt_toolkit, croniter, cryptography, jinja2 and more), with extras for each provider. | `github.com/openai/openai-go/v3`, plus optionally `github.com/modelcontextprotocol/go-sdk`. |
| Pi 5 (arm64) support | Officially Tier 1 on Linux aarch64 and in Docker on aarch64. The arm64 image is about 1,107 MB compressed (`stable`/`latest`, 2026-11-10). No published RAM or CPU figure. The docs only say a session with many tool calls can pin "tens of MB" of transcript, and that there is a `memory_high_mb` eviction budget. | A few MB of extra RSS in a process that is already running. Model inference is remote, so CPU cost is mostly JSON and HTTP. |
| Calling Assistant Tools | Through MCP servers (`mcp_servers` in `config.yaml`, stdio or HTTP/SSE, discovered automatically at startup). Native Hermes tools would mean writing Python plugins. | A direct function call into the Go Tool registry. The same registry serves the Fast path. |
| How the middleware drives it | OpenAI-compatible HTTP + SSE API server on `127.0.0.1:8642` (`/v1/chat/completions`, `/v1/responses`, `/v1/runs`, sessions through `X-Hermes-Session-Key`). `API_SERVER_KEY` is required because the API exposes the full toolset, terminal included. ACP and TUI-gateway JSON-RPC are also available. | Not needed. It is a function call. |
| OpenCode Go | Built in: provider `opencode-go`, `OPENCODE_GO_API_KEY`. Hermes sends the `x-opencode-session` header and routes each model to the right wire format (Chat, Responses or Anthropic). | `openai-go` with `option.WithBaseURL("https://opencode.ai/inference/go/openai/v1")`. We'd need to send `x-opencode-session` ourselves. Limit ourselves to Chat-Completions-format models (DeepSeek, GLM, Kimi, MiMo and others), or translate formats. |
| Memory | Built in: `MEMORY.md` (2,200 characters) and `USER.md` (1,375 characters), frozen into the system prompt at session start and edited by the `memory` tool. FTS5 session search, optional Honcho. | Write it: a `remember`/`forget` Tool backed by SQLite (planned anyway), injected into the system prompt. |
| Persona | Built in: `SOUL.md` is slot #1 of the system prompt, plus `/personality` overlays. | Write it: a persona file prepended to the system prompt. |
| Web search | Built in: `web_search`/`web_extract`. Backends include Firecrawl (the default), SearXNG (self-hosted), DDGS (no key), Brave, Tavily, Exa and others. | Write it: one Tool calling SearXNG, Brave or Tavily over HTTP. |
| Other things it gives us | Cron scheduler, subagents, skills that improve themselves, context compression, fallback providers, voice mode, wake word, a Spotify integration. | None, but the middleware already plans to own scheduling (Briefings), Notifications and Spotify. |
| Risks | A very fast-moving codebase (multiple releases a week, a large surface area). Behaviour depends on its own prompt and toolset defaults. Terminal and file tools need to be locked down. Dual ownership of sessions and memory with the middleware. | We own context management, retries and compression. We may reimplement features piecemeal over time. |

## Notes on OpenCode Go

- OpenCode's docs list these endpoints: `https://opencode.ai/inference/go/openai/v1/chat/completions`, `.../openai/v1/responses`, `.../anthropic/v1/messages` and `https://opencode.ai/inference/go/v1/models`. Each model has a native format, and the docs say Console translates between formats.
- The docs say Go works outside OpenCode, naming Claude Code, Codex, Hermes and Pi. They ask clients to send their own user agent and a stable `x-opencode-session` header for each conversation.
- They describe the service as "designed for OpenCode and similar coding-agent traffic", and abusive traffic is monitored. Low-volume use as a personal voice assistant looks fine, but check the terms before relying on it.
- Hermes docs say its `opencode-go` provider pins the relay `opencode.ai/zen/go/v1`, which is a different path from OpenCode's current docs. See Open uncertainties.

## Sketch of the Go loop (for scale)

```go
client := openai.NewClient(option.WithBaseURL(base), option.WithAPIKey(key),
    option.WithHeader("x-opencode-session", sessionID))
for turn := 0; turn < maxTurns; turn++ {
    resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
        Model: model, Messages: msgs, Tools: registry.OpenAITools()})
    // if no tool calls: return resp.Choices[0].Message.Content
    // else: append assistant msg; for each call, msgs = append(msgs, openai.ToolMessage(registry.Call(ctx, call), call.ID))
}
```

## Sources

- Hermes Agent repo, README and `pyproject.toml` (v0.23.1, `main` @ `5df966a`, 2026-11-11): https://github.com/NousResearch/hermes-agent
- Hermes docs, Programmatic Integration (ACP, TUI gateway, API server): https://github.com/NousResearch/hermes-agent/blob/main/website/docs/developer-guide/programmatic-integration.md
- Hermes docs, API Server: https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server (source: `website/docs/user-guide/features/api-server.md`)
- Hermes docs, MCP: https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp
- Hermes docs, Persistent Memory: https://hermes-agent.nousresearch.com/docs/user-guide/features/memory
- Hermes docs, Personality & SOUL.md: https://hermes-agent.nousresearch.com/docs/user-guide/features/personality
- Hermes docs, Web Search: https://hermes-agent.nousresearch.com/docs/user-guide/features/web-search
- Hermes docs, Providers (OpenCode Go / Zen): https://hermes-agent.nousresearch.com/docs/integrations/providers
- Hermes docs, Platform support (aarch64 Tier 1): https://github.com/NousResearch/hermes-agent/blob/main/website/docs/getting-started/platform-support.md
- Hermes docs, Gateway session lifecycle (memory-pressure eviction): https://github.com/NousResearch/hermes-agent/blob/main/website/docs/developer-guide/gateway-session-lifecycle.md
- Docker Hub `nousresearch/hermes-agent` tags (image sizes, 2026-11-10): https://hub.docker.com/r/nousresearch/hermes-agent/tags
- OpenCode Go docs (endpoints, models, formats, usage terms): https://opencode.ai/docs/go/
- openai-go (official Go SDK, v3.98.0, 2026-11-09): https://github.com/openai/openai-go
- MCP Go SDK (official, v1.8.0, 2026-09-14; supports spec 2026-07-28 back to 2024-11-05): https://github.com/modelcontextprotocol/go-sdk
- MCP specification (latest 2026-07-28): https://modelcontextprotocol.io/specification

## Open uncertainties

- **Hermes RAM and CPU on a Pi 5 were not measured.** No first-party figure exists. The 1.1 GB image and the Python + Node stack suggest a few hundred MB of RSS at idle, but that is an inference. Measure with `docker stats` in the local prototype if Hermes is ever reconsidered.
- **OpenCode Go base URL drift.** Hermes uses `opencode.ai/zen/go/v1`, while OpenCode's docs now list `opencode.ai/inference/go/openai/v1`. Confirm which one is live when building, and prefer the path in OpenCode's docs.
- **Which OpenCode Go model, and its tool-calling quality.** The Go loop is simplest with a Chat-Completions-format model (for example `deepseek-v4.1-flash` or `glm-5.3-flash`). Tool-calling reliability and latency per model were not tested. That belongs to the model-choice ticket.
- **OpenCode Go acceptable use.** The service is framed for coding-agent traffic. A voice assistant is low volume, but this has not been confirmed with OpenCode.
- **Hermes churn.** Release cadence is high (v0.23.1 shipped one day before this research), so any feature claims here may change quickly.
