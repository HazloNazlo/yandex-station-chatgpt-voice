# Desktop-style app-server bridge for GPT Live

**Status:** architecture study and proof-of-concept proposal for this repository. Target hardware: Yandex Station Mini 1. The objective is model-selected tools during natural, interruptible GPT Live voice, without an OpenAI Developer API key.

## Executive summary

The desktop-style app-server bridge is the strongest candidate for getting genuine model-selected tools while preserving a live voice experience.

It is **not** a way to add arbitrary tools to the Station project's existing direct ChatGPT web Voice endpoint, /realtime/wm. That route is voice-first and does not expose arbitrary application tool schemas. The bridge starts a separate Codex App Server session, negotiates a realtime voice session through it, and uses a separate server-side event channel for native handoffs and tool execution.

Authentication also differs from the current Station browser-cookie login:

- Current project: ChatGPT website session cookies are imported to the Station and refreshed to obtain a short-lived web Voice token for direct /realtime/wm.
- App-server bridge: a ChatGPT-plan OAuth token is used by Codex App Server. A device-code / Sign in with ChatGPT flow can obtain it. The same ChatGPT account can be used, but the existing session.json browser cookie is not itself the Codex app-server credential.

This can avoid a Developer API key and API-project pay-as-you-go billing. OpenAI documents Codex App Server using an OAuth access token authorized by the user's ChatGPT plan. The underlying Codex provider sends inference requests to the Responses endpoint using that plan token; that is not the same thing as a developer API key. Work and Codex usage is subject to the user's ChatGPT plan allowances and credits:
- https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server
- https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan
- https://help.openai.com/en/articles/20001275-chatgpt-work-and-codex

**Important maturity boundary:** Codex App Server itself is an OpenAI-documented, open-source JSON-RPC server. The specific realtime voice extension and event names used by the bridge (--enable realtime_conversation, thread/realtime/start, handoff_request, etc.) are experimental/private and are not documented as a stable public voice API. The third-party SDK is a useful implementation reference, not an OpenAI-supported integration contract.

## The three layers

### 1. GPT Live voice surface

The Station and the remote voice service exchange audio over WebRTC. The live voice surface is responsible for listening, speaking, natural turn-taking, and interruption.

The bridge does not need to inspect partial transcripts and guess that a user said "search the web". Instead, when the native voice runtime decides that a task should be delegated, it emits a native handoff item to the app-server side.

### 2. Codex App Server execution side

The backend starts a codex app-server child process and drives it through JSON-RPC 2.0 over newline-delimited JSON on stdio. Codex app-server has a documented thread/turn/item model, initialization handshake, notifications, tool approvals, and dynamic tool APIs:
- https://learn.chatgpt.com/docs/app-server

The third-party realtime bridge uses an additional experimental feature gate and realtime methods. In the inspected implementation it:

1. Starts codex --enable realtime_conversation app-server --stdio.
2. Performs initialize, then sends initialized.
3. Creates an ephemeral thread with a selected execution model, dynamic tool schemas, read-only sandbox, and no generic command approval prompts.
4. Calls thread/realtime/start with the Station's WebRTC SDP offer, voice, and audio output modality.
5. Receives a thread/realtime/sdp notification and returns the SDP answer to the Station.
6. Listens for thread/realtime/itemAdded; when the item is handoff_request, it reports the handoff to the application.
7. Handles item/tool/call server requests, validates the requested tool name against the configured allowlist, invokes the application callback, and returns a dynamic-tool result.
8. Uses a reserved speak_to_user helper to send concise results into the Live audio session via thread/realtime/appendSpeech.

The source-level reference is:
- https://github.com/opencoredev/login-with-chatgpt/blob/main/packages/server/src/realtime-app-server.ts

The documented Codex app-server transport is JSON-RPC 2.0; stdio uses JSONL. The app-server documentation describes the initialization sequence and dynamic tool calls, but it does not currently document the realtime-specific methods or handoff item as a stable public protocol:
- https://learn.chatgpt.com/docs/app-server

### 3. Application tool gateway

Tool schemas and implementations belong to the application's server, not to the browser and not to the Station's untrusted audio transcript.

Suggested initial server tools:

| Tool | Suggested implementation | Access |
|---|---|---|
| web_search | SearXNG JSON API or another configured provider | Read-only |
| open_url | Safe HTTP fetch + HTML-to-text extraction | Read-only |
| get_weather | Geocoding + Open-Meteo or another provider | Read-only |
| get_currency_rates | Configured rates provider | Read-only |
| get_crypto_price | Configured market-data provider | Read-only |
| set_station_volume | Authenticated RPC to the Station's Go process | Device mutation |
| set_station_voice | Authenticated RPC to the Station's Go process | Device mutation |

The model can choose among these tools based on the task. The application's executeTool callback still validates every argument, applies timeouts, and controls the real side effect. A model-generated schema is not an authorization boundary.


## Message and media flow

The key architectural separation is:

- WebRTC carries the actual two-way audio.
- The app-server's JSON-RPC child process handles voice delegation and tool lifecycle.
- The Node/Bun HTTP server exposes a user-scoped signaling endpoint and a separate event stream to the Station client.
- Tool implementations run on the VPS (or another trusted backend), not in the Mini 1 process.

### Expected Station integration

The existing Go Station client already creates a Pion WebRTC offer and processes the SDP answer. The first integration spike should test whether it can keep that media path while replacing direct signaling:

1. Mini 1 creates the normal local WebRTC SDP offer using the existing Pion peer.
2. Instead of posting multipart SDP + session to https://chatgpt.com/realtime/wm, the Go client posts the SDP and selected voice/model to an authenticated VPS endpoint such as POST /station/realtime/app-server.
3. The VPS starts the Codex app-server session, submits the offer to thread/realtime/start, and returns the answer SDP plus an opaque session ID.
4. Mini 1 applies the SDP answer and streams audio as it does today. The VPS is the signaling/control host; it should not proxy the RTP audio unless testing proves this is required.
5. In parallel, the Go client consumes a bounded, authenticated NDJSON event stream for session lifecycle and tool status. Tool execution itself remains on the VPS.
6. When a tool finishes, the bridge speaks the result in the same Live conversation. No synthetic user-role relay_message is needed for the primary tool result path.
7. If native Alice becomes active, the Station immediately closes its media session and calls the VPS teardown endpoint. The VPS cancels outstanding work and kills the child process.

The third-party SDK's browser helper connectChatGPTRealtimeAppServer() is designed for a browser microphone and an application UI. The Station project should **not** try to run that browser helper on ARMv7. Instead, reuse the server-side session primitive / HTTP route design and adapt the existing Go Pion client. Whether the same SDP offer and data-channel settings work unchanged with this server path must be proven in a small PoC.

### Suggested HTTP surface

The SDK's built-in authenticated routes are:

| Method | Route | Purpose |
|---|---|---|
| POST | /realtime/app-server | Start a Live + app-server session and exchange SDP |
| GET | /realtime/app-server/:id/events | Stream NDJSON lifecycle / tool events |
| POST | /realtime/app-server/:id/confirm | Resolve a pending application confirmation |
| DELETE | /realtime/app-server/:id | Stop the Live session and child process |

For the Station, put a narrow device-authenticated facade in front of these routes. The Station should authenticate as a paired device; the backend maps that device to the ChatGPT account and session. Do not copy the browser's HttpOnly login cookie or any ChatGPT refresh token onto the Station just to call these endpoints.

The bridge emits events such as session.started, session.closed, handoff, tool.running, tool.completed, tool.pending_confirmation, tool.failed, and error. The bridge stream is separate from the native DataChannel event stream.

## Authentication and billing: critical distinction

The current Station project uses a browser-session cookie family and a short-lived token minted for the ChatGPT website Voice client. That credential is for the direct web Voice route.

The app-server path uses Codex/ChatGPT-plan OAuth credentials. The referenced SDK's sign-in route is a device-code flow: the user authorizes a ChatGPT account on OpenAI's device verification page, and the backend stores the resulting OAuth session. The child process is then given a Codex-compatible auth file. Its inspected source writes:
- auth_mode: chatgpt
- a ChatGPT-plan access_token
- account_id
- optional id_token and refresh_token
- OPENAI_API_KEY: null

The source creates a private temporary HOME directory and writes auth.json with owner-only permissions. This is another credential family, not an import of the Station's existing __Secure-next-auth.session-token cookie.

Official OpenAI documentation describes Codex app-server using an OAuth token authorized for ChatGPT plan usage. It states that the access token is sent to the Responses endpoint; token refresh is the application's responsibility. The ChatGPT account's Codex/Work allowances and credits apply. Do not describe this as "no network API at all"; accurately describe it as **no Developer API key / no API-project billing, using ChatGPT-plan OAuth**.

References:
- https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server
- https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan
- https://help.openai.com/en/articles/20001275-chatgpt-work-and-codex
- https://github.com/opencoredev/login-with-chatgpt/blob/main/docs/content/docs/concepts/how-it-works.mdx

The selected execution model must be passed in the session request. Do not assume the same model slug as the current direct /wm voice backend will be accepted by Codex. A model catalog can include slugs unavailable to a particular account; a successful test inference is the entitlement check. The third-party bridge guide explicitly warns that delegated tool turns can fail with misleading entitlement errors if a default model is silently substituted.


## Tool execution and confirmations

The inspected SDK uses server-owned function schemas:

- tools: name, description, and JSON input schema.
- executeTool(context): receives callId, name, and parsed arguments.
- confirmTool(context): optional handler called only after an authenticated confirmation route is used.

Important behavior: when a tool returns pendingConfirmation, the bridge immediately returns a structured "pending" result to the model. This avoids leaving the JSON-RPC request open and freezing the realtime voice surface. The actual consequential operation waits for explicit app confirmation.

For this Station project:

- Searches, page reads, weather, and rates can be read-only and normally need no confirmation.
- Device changes and other mutations should have explicit policy and idempotency.
- A voice utterance such as "yes, send it" must not by itself approve a consequential action. The reference bridge expects an application-side confirmation. Mini 1 has no rich display; a future implementation would need a safe physical confirmation gesture or a companion phone/web UI.
- Do not expose a generic shell tool, arbitrary browser automation, or unrestricted URL fetcher.
- Keep volume and voice operations local to the Station. A VPS callback cannot run the Station's local shell scripts directly; it needs an authenticated, narrow device RPC if those controls are exposed through the app-server tool list.

## Operational requirements

The app-server bridge is a long-running service, not a good fit for request-isolated serverless or edge functions:

- Node.js or Bun backend.
- codex executable available on the VPS PATH and compatible with the realtime extension.
- Process supervision for the Node service and all Codex child processes.
- Persistent encrypted token/session storage and a stable server secret. Do not use an in-memory session store in production.
- One active Live/app-server session per account in the reference implementation; default idle/session TTL is 30 minutes.
- Sticky routing if more than one backend instance is deployed, because the child-process/session registry is process-local.
- TLS for all Station-to-VPS control requests and a per-Station credential with rotation/revocation.
- Rate limits and quotas per account/device.
- Bounded event queues and HTTP request sizes.
- Explicit close on Station disconnect, Alice takeover, timeout, sign-out, or server shutdown.

The Codex app-server's ordinary documented stdio mode is JSONL over stdin/stdout. The reference bridge also starts it with the experimental realtime_conversation feature. The latter is the compatibility risk: it can change independently of the documented JSON-RPC surface.

## Fit for the existing Mini 1 project

| Concern | Direct /wm + local router | Desktop-style app-server bridge |
|---|---|---|
| Voice/audio client | Existing Go/Pion client | Reuse Go/Pion is plausible, but SDP handoff needs a PoC |
| ChatGPT login | Existing browser web-session cookie | Separate ChatGPT-plan/Codex OAuth device login |
| Tool selection | Local phrase rules | Native model handoff to server execution |
| Tool schemas | Not arbitrary on direct /wm | App-owned dynamic schemas on server |
| Tool execution | Mostly on Station | Mostly on VPS |
| Internet search | Local DDG/Wikipedia implementation | Any server-defined search provider |
| Open URL / weather | Need to build local handlers | Build tools on VPS, model can select them |
| Runtime | Go daemon on Station | Go daemon + long-lived Node/Bun + Codex child |
| Main risk | Heuristic coverage and brittle scraping | Experimental realtime extension + OAuth/session operations |

### Recommended split of responsibilities

- Station Mini 1: wakeword detection, native Alice priority, microphone, Opus/WebRTC, speaker, playback interruption, local volume, local voice preference, and device lifecycle.
- VPS: device authentication, ChatGPT-plan OAuth session, Codex app-server child lifecycle, tool schemas, web search, URL extraction, weather, tool result normalization, confirmation state, rate limiting, and logs.
- WebRTC media should stay peer-to-service. The VPS should handle signaling and tool control, not transcode or proxy audio, unless the PoC demonstrates that direct media cannot be established.

## Proof-of-concept plan

Do not rewrite the Go client first. Validate the unknown private realtime extension in isolation.

### PoC 1 — SDK/browser

1. Deploy a small long-lived Bun/Node service on a VPS.
2. Install the MIT-licensed @opencoredev/loginwithchatgpt-core and @opencoredev/loginwithchatgpt-server packages.
3. Sign in with ChatGPT using the device-code OAuth flow (same account as the Station's web session).
4. Register only one harmless diagnostic_ping dynamic tool with a strict schema.
5. Connect the reference browser helper connectChatGPTRealtimeAppServer().
6. Ask for a task that should trigger a handoff; verify the handoff event, tool callback, tool result, and spoken response.
7. Confirm that closing the call kills the Codex child and removes its temporary home.

Pass condition: the signed-in account can complete one native realtime handoff and hear the tool result, without an OpenAI Developer API key.

### PoC 2 — Native Station transport

1. Keep the current Pion peer, Opus tracks, wakeword handling, and Alice gate unchanged.
2. Add a feature-flagged signaling adapter that posts the Pion offer to a device-authenticated VPS route and receives the SDP answer.
3. Subscribe to the NDJSON bridge event stream using the returned opaque session ID.
4. Verify full-duplex audio and interruption on the actual Mini 1.
5. Verify that a tool handoff does not stop or duplicate the voice media stream.
6. Verify Alice takeover tears down both the Pion peer and server-side Codex process.

Pass condition: Mini 1 can use the new bridge while the current /wm path remains available as a rollback mode.

### PoC 3 — real read-only tools

Add in this order:
1. web_search(query, freshness?)
2. open_url(url)
3. get_weather(location, date_range?)

Use mocked tests first, then live provider tests. Return source URLs and provider timestamps so the voice model can state where facts came from.

### Go/no-go criteria

Proceed beyond PoC only when all of these are true:

- No Developer API key is needed; ChatGPT-plan OAuth authentication succeeds on the intended account.
- The Codex execution model is available under that account, and usage is visible under its ChatGPT-plan allowance.
- A native handoff occurs without transcript heuristics.
- The same WebRTC audio path remains interruptible.
- The Station can reliably start, stop, and cancel the remote session.
- Alice retains priority and late tool results are discarded.
- Tools are allowlisted and server-owned.
- The SDK's experimental realtime event shapes remain reproducible across repeated runs and a pinned Codex version.

## Current recommendation

Treat this as a **separate, feature-flagged transport option**, not a change to the existing /wm implementation and not an assumption that ChatGPT web cookies can authorize Codex.

It is more capable than the local phrase router and closely matches the desired "model decides when to use tools" experience. It also has higher operational and compatibility cost: a Node/Bun VPS, a separate ChatGPT-plan OAuth login, Codex child-process supervision, and reliance on an experimental realtime extension.

For the first milestone, prove one harmless native handoff with diagnostic_ping. If that works on the account and model you plan to use, continue to web search, URL reading, and weather. If it fails due to entitlement or protocol changes, keep the local tool gateway plan as the stable fallback.

## References

### OpenAI documentation

- Codex app-server JSON-RPC protocol: https://learn.chatgpt.com/docs/app-server
- Sign in with ChatGPT: Codex app-server with ChatGPT-plan OAuth: https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server
- Using Codex with your ChatGPT plan: https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan
- ChatGPT Work and Codex usage: https://help.openai.com/en/articles/20001275-chatgpt-work-and-codex

### Experimental implementation reference (not an OpenAI API contract)

- SDK repository (MIT): https://github.com/opencoredev/login-with-chatgpt
- Experimental realtime voice guide: https://loginwithchatgpt.dev/docs/guides/realtime-voice
- Server handler options: https://loginwithchatgpt.dev/docs/reference/server
- HTTP routes: https://loginwithchatgpt.dev/docs/reference/routes
- Implementation of the app-server bridge: https://github.com/opencoredev/login-with-chatgpt/blob/main/packages/server/src/realtime-app-server.ts
