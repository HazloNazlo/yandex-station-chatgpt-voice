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
