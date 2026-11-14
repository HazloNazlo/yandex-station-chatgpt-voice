# Browser-session tools: audit and implementation plan

**Status:** read-only audit of `main` at `c45c25868d0b464275b123fc414bcc15d72010fd` (14 November 2026). This document deliberately does not switch the project to an OpenAI developer API.

## Executive summary

The current voice transport is ChatGPT's private web Voice endpoint, `POST https://chatgpt.com/realtime/wm`, with a ChatGPT web-session token and WebRTC. It is not the public Realtime API.

There are two separate problems:

1. The code contains a partial Realtime-API-style function-calling implementation, but the current `/wm` session does not advertise those tools. The code history shows that the `session.update` and system-role priming attempt was removed because it was incompatible with `wingman`.
2. The working fallback is not model tool calling. It is a local transcript-triggered phrase router followed by HTTP requests and a `relay_message` back into the same voice conversation. That router is intentionally narrow, the search implementation is weak, and there is no URL-opening tool.

**Recommendation:** keep the browser-session WebRTC transport, do not re-add the removed Realtime `session.update`, and build a clearly separated local tool gateway. The gateway should recognise requests from completed user transcripts, execute safe web/weather/page tools, and return structured evidence through `relay_message`. That is the practical way to add custom tools without an OpenAI developer API key on the direct `/wm` path.

This gives the voice assistant useful tool-like behavior, but it is important to describe it honestly: on direct `/wm`, custom tools are executed by the Station-side client, not chosen through arbitrary model-emitted function calls.

## What the code does today

### Voice session setup

- `internal/voice/protocol.go:encodeSession` sends `voice_mode: "wingman"`, a backend model, `conversation_mode: primary_assistant`, and `client_tools: []`.
- `internal/voice/call.go:runCall` establishes WebRTC and the negotiated DataChannel, then starts streaming audio. It does not send the tools message.
- `internal/voice/tools.go:getSessionUpdateMessage` still constructs a `session.update` with `search_web`, `set_volume`, `set_voice`, and `hang_up`. The function currently has no caller.
- `getSystemInstructionMessage` is also currently unused.
- `parseToolCalls` expects `response.function_call_arguments.done`, `response.output_item.done`, and `response.done`; the handler then sends `conversation.item.create/function_call_output` and `response.create`.

The mismatch is important: those event names and the `session.update` tool schema are the public Realtime function-calling shape. The active `/realtime/wm` route is a separate private ChatGPT web transport. The third-party experimental SDK documentation for this private route says that `client_tools` must remain empty and that `/wm` only accepts the reserved device-tool IDs `timer`, `stopwatch`, `precise_location`, and `led`; arbitrary application schemas are rejected. It also distinguishes this direct voice-only route from the desktop app-server mediation path, whose handoff events can reach server-owned tools but requires a separate Node/Bun + `codex` process and is not emitted on the direct `/wm` DataChannel. This is reverse-engineered, unofficial documentation, not an OpenAI protocol guarantee:
- https://loginwithchatgpt.dev/docs/guides/realtime-voice
- https://loginwithchatgpt.dev/docs/reference/core

### Why internet access feels unreliable

The active route is in `internal/voice/call.go:peer.handleMessage`:

1. It collects text from `chat_message_delta` events until the user message is marked finished.
2. It runs `parseHangupCommand`, `parseVoiceCommand`, and `parseVolumeCommand`.
3. It calls `detectSearchQuery`. Only if this function returns true does it start the local search goroutine.
4. The goroutine calls `searchWeb`, flushes local playback, and injects the result as another user-role `relay_message`.

This is not a general-purpose agent loop. It is a list of phrase patterns. The current tests in `tools_internal_test.go:TestDetectSearchQuery` explicitly require several factual questions (for example, “кто такой Илон Маск” and “расскажи историю породы русский спаниель”) **not** to invoke search. A request such as “посмотри в интернете …” works because it matches an explicit marker; a natural request that does not match a marker will stay inside the voice model.

The current internet layer has further limitations:

- `searchWeb` checks a few hard-coded categories, then queries Wikipedia first and DuckDuckGo HTML second.
- `fetchDuckDuckGo` extracts title/snippet fragments with regular expressions. It does not return a useful structured source list with canonical URLs, timestamps, and domains.
- There is no `open_url` / page-reading implementation.
- `fetchWeather` parses only the first word after a location preposition. Multi-word city names are truncated. It primarily returns a compact current-weather string from wttr.in, not a structured forecast tool.
- The transcript-triggered path gives the search request a four-second deadline. This is tight for a sequence of external requests and HTML parsing.
- Some fetchers do not validate HTTP status or bound the entire response body.
- `sharedHTTPClient` sets `InsecureSkipVerify: true`. This disables TLS certificate verification for the search/weather/currency HTTP client. It was added as an Android 6 compatibility workaround, but it is not safe as a general network policy and should be replaced with a verifiable CA strategy.

## What not to do

Do not simply restore the removed block that sends `getSessionUpdateMessage()` after the DataChannel opens. The commit that removed it is:

- https://github.com/HazloNazlo/yandex-station-chatgpt-voice/commit/e8632ab79772c645800c9a495d5eec317a166efa

That commit explicitly describes the `session.update` and system-role priming as `wingman`-incompatible. Re-adding it without a captured successful `/wm` exchange would risk breaking session startup and would still rely on Realtime API event names that are not demonstrated on this transport.

Do not make the model say that it called a tool unless the client actually executed that tool and returned evidence. A `relay_message` is a user-role text injection, not a function call or a tool result.

## Recommended target design (no OpenAI developer API)

Keep Station Mini 1 as the lightweight voice endpoint. Add an internal tool gateway between transcript completion and `relay_message`:

```text
Yandex Station Mini 1
  wakeword / Alice-priority gate
  microphone + Opus / WebRTC / speaker
  ChatGPT /realtime/wm session (web-account session)
             |
             | chat_message_delta transcript
             v
  Transcript assembler + turn ID
             |
             v
  Local intent router / tool dispatcher
      |         |          |          |
      |         |          |          +-- set_volume / set_voice
      |         |          +------------- get_weather
      |         +------------------------ open_url
      +---------------------------------- search_web
             |
             v
  Structured, bounded tool result
             |
             | relay_message (source links + facts)
             v
  Same ChatGPT voice conversation speaks the answer
```

The tool gateway can be a Go package in the existing daemon. If external HTTP/DNS constraints on the Station are too severe, the same package contract can later be moved behind a small VPS HTTP service; that is an infrastructure change, not a dependency on the OpenAI API.

### Tool contracts

Start with a small set of tools that are deterministic and testable:

| Tool | Input | Behavior |
|---|---|---|
| `search_web` | query, optional freshness window | Return 3–6 results with title, URL, domain, snippet, and date when available |
| `open_url` | absolute URL | Fetch a public HTTP(S) page and return cleaned readable text, page title, final URL, and metadata |
| `get_weather` | city or coordinates, optional date range | Geocode the full location and return current conditions / forecast with units and timezone |
| `get_currency_rates` | currency symbols | Return rates with provider and timestamp |
| `get_crypto_price` | supported symbol | Return price, quote currency, source, and timestamp |
| `set_volume` | volume or delta | Retain the existing Station-specific implementation |
| `set_voice` | voice | Retain the existing voice preference implementation |

Use one dispatcher that accepts a typed intent, validates arguments, executes the handler with a context deadline, and returns a typed result. Do not mix transcript parsing, network code, and response formatting inside `peer.handleMessage`.

### Search provider

Preferred direction: configurable SearXNG, with the current provider kept only as an explicit fallback during migration.

- Use a self-hosted SearXNG instance (for example on the user's own VPS) instead of depending on one public instance.
- SearXNG exposes a JSON search interface at `/search?q=...&format=json`; the JSON format must be enabled in the instance configuration. Public instances may disable it.
- Keep the base URL in local configuration, not in source code. Make health-check/status output show whether the configured search backend is reachable, without printing credentials.
- Normalize results into one internal structure. Deduplicate by canonical URL and retain title, URL, snippet, engine/provider, and publication date when available.
- If SearXNG is unavailable, use a clearly reported fallback. Do not return “internet works” when no source was actually obtained.

References:
- https://docs.searxng.org/dev/search_api
- https://docs.searxng.org/admin/settings/settings_search.html

### Weather provider

Replace the single-word wttr.in parser with geocoding plus a structured forecast provider. Open-Meteo exposes current and multi-day forecast data and geocoding in JSON, without requiring an API key for its free tier. The free service is limited to non-commercial use (up to 10,000 calls/day) and requires CC BY 4.0 attribution; commercial use requires a separate arrangement. This is a good fit only if the project's use complies with those terms.

References:
- https://open-meteo.com/
- https://open-meteo.com/en/docs
- https://open-meteo.com/en/terms

### URL opening and page safety

`open_url` should be a fetch-and-extract tool, not an unrestricted proxy:

- Accept only absolute `http` and `https` URLs.
- Reject loopback, private, link-local, multicast, reserved, and cloud-metadata IP ranges after DNS resolution.
- Re-check each redirect target and cap redirect count.
- Reject embedded credentials and unsupported ports/schemes.
- Apply connection, header, total-request, and body-size limits; accept HTML/text only.
- Extract readable text and canonical links; drop scripts, styles, forms, and hidden content.
- Never execute JavaScript, download arbitrary files, or invoke shell commands from model/user text.
- Treat retrieved page content as untrusted data, not instructions to the agent.

## Intent routing: first release

Because direct `/wm` does not provide arbitrary client function schemas, the first release should use a high-precision local router rather than pretend to implement native model function calling.

1. Separate transcript assembly from intent classification. Preserve the completed utterance exactly and attach a turn ID.
2. Add intents for explicit web research, URL opening/page reading, weather/forecast, date-sensitive questions, currency, and crypto.
3. Recognize natural language variants, not only the exact phrase “в интернете”. Examples: “что сейчас происходит…”, “найди последние новости…”, “открой сайт…”, “прочитай эту страницу…”, “какая погода завтра в …”.
4. Do not send greetings, small talk, or stable arithmetic to search.
5. When a phrase is ambiguous (for example, “погода” without a city), ask for the missing city instead of querying an arbitrary location.
6. When a tool starts, stop/flush current generated speech using a supported control action if possible; do not allow a stale answer to overlap the tool result.
7. Relay a compact evidence packet into the existing voice conversation. It must say which sources were used, include URLs, and distinguish “no results” from “network error”.
8. Preserve cancellation: a new user turn, Alice priority, or call end cancels the outstanding tool request and prevents late results from being relayed.

This is still heuristic routing. If true semantic tool selection by the live voice runtime is a hard requirement, evaluate the desktop-style Codex App Server bridge described in detail in [DESKTOP_APP_SERVER_BRIDGE.md](DESKTOP_APP_SERVER_BRIDGE.md). It is a separate transport mode, not an extension to direct /wm: a Node/Bun VPS starts a Codex app-server process, negotiates the realtime voice session, and receives native handoff events on a separate server-side channel. Custom tools are declared and executed by the backend.

Important authentication distinction: the desktop app-server bridge is separate from the existing browser-cookie session.json. The inspected third-party SDK mirrors the public Codex CLI OAuth client and private Codex backend protocol; it does not use the official SIWC third-party plan-sharing flow. OpenAI's current plan page lists Free Codex access specifically for the desktop app, while Plus explicitly includes CLI access. The App Server is a Codex CLI component, so Free support for this bridge is not documented. The alternative SIWC plan-sharing flow for third-party open-source apps currently lists eligible Plus/Pro users, not Free. Do not assume either path works on Free or try to bypass plan restrictions; first run the official Codex CLI gate on the user's Mac. Detailed conditions and exact test commands are in [DESKTOP_APP_SERVER_BRIDGE.md](DESKTOP_APP_SERVER_BRIDGE.md).

The direct /wm path itself does not expose arbitrary custom function schemas. Do not restore the old Realtime API-style session.update block to this path. Keep the local tool gateway as the reliable first implementation while the app-server alternative is validated with a harmless end-to-end proof of concept. References: https://loginwithchatgpt.dev/docs/guides/realtime-voice, https://learn.chatgpt.com/docs/app-server, and https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server.

## Security and reliability changes required

- Remove `InsecureSkipVerify: true`. Do not weaken TLS globally to work around the Station's certificate store. Use a current CA bundle or a narrowly scoped, verifiable trust configuration.
- Bound every HTTP response body and check status codes before parsing.
- Use explicit per-tool deadlines and one overall turn deadline.
- Sanitize logs: never print access tokens, cookies, authorization headers, or whole page bodies.
- Avoid storing transcripts unless the user explicitly enables diagnostic logging; diagnostic fixtures must be redacted.
- Add SSRF protection to `open_url`, including redirect and DNS-rebinding checks.
- Treat search results and page contents as untrusted prompt data.
- Never let remote content cause volume changes, process execution, or other side effects.

## Test plan and acceptance criteria

Use mocked HTTP transports and captured, sanitized DataChannel envelopes. No live session credentials belong in tests.

- A natural weather request with a multi-word city returns the correct geocoded location and a current/forecast result.
- “Открой example.org и расскажи, что там” fetches the public page and returns its title, URL, and readable text.
- A general current-events/news request invokes search without requiring the literal phrase “в интернете”.
- A greeting, “расскажи анекдот”, and simple arithmetic do not invoke web tools.
- Search results contain canonical URLs; empty result sets and network failures are distinct.
- Invalid schemes, localhost/private destinations, redirect-to-private URLs, oversized responses, and slow servers are rejected.
- A new user turn, call cancellation, or native Alice priority prevents an old tool result from being injected into the conversation.
- Existing wakeword behavior, native Alice priority, volume, voice switching, and ARMv7/glibc compatibility remain intact.
- Tests verify the actual result structure and safety behavior, not just that an HTTP request was attempted.

## Implementation order

1. Add event fixtures and test the current `chat_message_delta` transcript assembly against real sanitized captures.
2. Extract the local router/dispatcher and add `open_url` and structured tool result types.
3. Replace the DDG-only HTML scraping path with configurable SearXNG + URL/source normalization.
4. Replace the weather parser/provider with geocoding + Open-Meteo.
5. Fix TLS verification and all response bounds.
6. Expand phrase/intention coverage with negative tests and cancellation tests.
7. Only after this is reliable, prototype whether a separate browser-session text agent can delegate semantic tool choice without a developer API. Keep it behind a feature flag and do not let it block the direct voice call.

**Bottom line:** the immediate blocker is not just a bad search engine. The project currently has no supported custom tool-registration path on its chosen private `/wm` transport, and its fallback router is deliberately narrow. Stabilize that fallback as a real local tool gateway first; do not revive the incompatible Realtime API message format.
