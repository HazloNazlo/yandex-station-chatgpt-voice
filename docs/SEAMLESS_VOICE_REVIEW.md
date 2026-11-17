# Station Mini voice architecture and implementation review

Evidence checked on 2026-11-17, Europe/Moscow. Code base: the fork's `main` commit `f230f1cdb9173414fbe39c60fe35b94e5f040c1d`; protocol audit documents: `02a1258`. This review separates inspected client code, reported account experiments, host tests, and untested hardware/provider behavior.

## Recommendation

Ship the implemented **hybrid `/wm` plus streaming neural TTS fallback** only after the hardware gates in the [ADB procedure](<ADB_NEURAL_TTS_VERIFICATION.md>) pass. It fixes the playback ownership and buffering defects without depending on prompt injection or an assumed public Realtime API. It is not an identical-voice solution: ordinary conversation uses your selected ChatGPT voice; local tool answers use Microsoft Svetlana or Dmitry.

For **the same native ChatGPT voice with model-selected tools**, investigate the Codex app-server realtime/handoff route first. Its inspected bridge has a real tool RPC channel and native `appendSpeech`, but your account's realtime entitlement, handoff behavior, Pion SDP, and Station playback still need end-to-end proof. Do not call that route production-ready because a non-voice Free-account diagnostic passed.

There is no established implementation that simultaneously guarantees all of these on this hardware and account: private `/wm`, no developer API billing, arbitrary tools, exactly the selected native voice, zero refusal leakage, and seamless cancellation. A prompt cannot supply missing authenticated protocol capabilities.

## Architecture comparison

| Route | Voice used for retrieved facts | Tool control | Account/payment boundary | Decision |
| --- | --- | --- | --- | --- |
| Direct `/wm` plus `relay_message` | Potentially the selected ChatGPT voice | Text/context injection; no verified application tool schema | Personal web session; private endpoint and voice quota | Experiment only; neither faithful recitation nor interruption is guaranteed |
| Direct `/wm` plus this neural fallback | Svetlana/Dmitry for local answers; selected ChatGPT voice for ordinary turns | Station classifies the transcript, retrieves facts, and owns playback | Existing web session plus unofficial Edge service; no OpenAI developer key | Implemented and host-tested; validate on the actual Station; accept voice switching |
| Codex app-server realtime plus native handoff | Native voice selected for that realtime session, if entitled and working | `item/tool/call`, handoff controller, `thread/realtime/appendSpeech` | Codex/ChatGPT account, model availability and experimental voice rollout; not inferred from browser cookie possession | Strongest same-voice candidate, gated by the tests below |
| Documented Sign in with ChatGPT Responses plus local TTS | Your chosen external TTS, not native Vale | Documented text/tool orchestration for eligible applications/plans | Current open-source token-sharing guidance covers eligible Plus/Pro Responses requests; it does not establish private realtime access | A supported text-orchestration alternative, not a native-voice substitute; not implemented here |
| Public Realtime API plus registered tools | A public Realtime API voice, not necessarily a ChatGPT web voice | Documented tools and cancellation controls | Developer API authentication and usage billing | Best protocol control, but outside the requested no-developer-key constraint |

Edge has no availability SLA. Engineering safeguards in this proxy do not turn its upstream into a production-contract service. A supported commercial neural provider or a locally hosted model can reuse the PCM contract, but no such second provider is included or benchmarked in this delivery.

## 1. What `/wm` session fields are supported?

No exhaustive, versioned OpenAI schema for `POST https://chatgpt.com/realtime/wm?dcid=0` was found. Do not equate an SDK allowlist, a request builder, or HTTP 200 with backend support or proof that an individual option took effect.

The repository's [session encoder](<../internal/voice/protocol.go>) emits these field groups:

| Group | Fields emitted by this client |
| --- | --- |
| Voice and session identity | `voice_mode`, `voice`, `voice_session_id`, `voice_status_request_id` |
| Conversation/streaming | `conversation_mode`, `enable_message_streaming`, `client_tools: []`, `history_and_training_disabled` |
| Language and model selectors | `language_code`, `model_slug`, `model_slug_advanced`, `requested_default_model`, and `backend_model` (default `gpt-6-thinking`) |
| Backend tuning | `backend_reasoning_effort: "instant"`; `backend_thinking_effort` defaults to `extended` and is omitted when configured exactly `disabled` |
| Locale | `timezone`, `timezone_offset_min`, `deprecate_celsius` |

The independent SDK also exposes conversation/parent IDs and its own restricted session-option contract. Its current source explicitly reserves `/wm` client tools for first-party device functions; it reports reserved IDs such as `timer`, `stopwatch`, `precise_location`, and `led`, not arbitrary weather/news/application function schemas.

**Custom system/developer instructions are not established for direct `/wm`.** Neither this client's builder nor the inspected SDK gives evidence for an `instructions`, `system_prompt`, or public `session.update.tools` mechanism on this path. Adding an unknown field may be ignored or rejected. A successful handshake alone is not a system-instruction test. The app-server `prompt` field discussed later belongs to a different initialization/control protocol.

Keep known-working initialization intact. If testing an additional field with your own account, vary one field per fresh call, record only sanitized request shape and outcomes, include a control call, and check a behavioral consequence. Do not log cookies, bearer tokens, raw SDP/ICE addresses, or full transcripts. These experiments cannot prove an exhaustive supported schema.

References: [pinned repository](https://github.com/HazloNazlo/yandex-station-chatgpt-voice/tree/f230f1c), [web-tools audit](https://raw.githubusercontent.com/HazloNazlo/yandex-station-chatgpt-voice/02a1258/docs/WEB_TOOLS_BROWSER_SESSION.md), [pinned independent SDK](https://github.com/opencoredev/login-with-chatgpt/blob/a1330697f35a1c6429009953acc8ec3e2f9f1f1c/packages/core/src/realtime.ts).

## 2. What actually interrupts `/wm` speech?

The inspected SDK constructs an `action_request` for `stop_speaking`, then wraps it in the private transceiver envelope. The implemented [encoder](<../internal/voice/protocol.go>) produces this exact two-layer JSON:

```json
{"type":"data_message","data":"{\"type\":\"action_request\",\"payload\":{\"action\":\"stop_speaking\"}}"}
```

This is better evidence than assuming public Realtime `response.cancel`. It is **not** a live acceptance test on your Station/account, a cancellation acknowledgement, or proof that the server's model turn/context was truncated to heard audio. The SDK also names listening actions; those names do not establish an application-controlled defer-response mode.

The local configuration therefore keeps `experimental_wm_stop_speaking` **false by default**. Enabling it sends the observed shape, with stale-work checks, but local playback correctness does not depend on an acknowledgement. The patch never sends guessed public `response.cancel`, `conversation.item.truncate`, or `session.update` frames to `/wm`.

Three separate layers matter:

1. **Server production:** only a validated server action/turn protocol can stop or defer generation. Muting the local speaker does not stop account quota consumption.
2. **Queued/callback PCM:** the patch invalidates the old generation, including a partly consumed callback block. Clearing only a channel cannot do this.
3. **Already submitted device samples:** bytes already in ALSA/hardware cannot be unheard. A callback acknowledgement is not a measured DAC timestamp. Measure the acoustic tail on the Station.

Early gating runs on the processed microphone frame before Opus encoding/upstream send. It is integer RMS with a 320 ms quiet reset, not a full speech recognizer. This reduces the causal race in which the backend speaks before the final transcript arrives. It does not guarantee zero initial leakage for whispering, missed detection, noise, or untagged late remote audio.

**Important ceiling:** remote RTP has no verified user-turn ID. The receiver can reject PCM/RTP already buffered under an old local generation. It cannot perfectly identify old server-produced packets arriving after a new generation begins. Natural server barge-in may resolve that in practice; it is not a proof. For a hard session boundary, close/recreate the peer connection, or validate a real server cancellation/acknowledgement contract. Reconnecting trades conversation continuity and latency for attribution safety. This patch does not silently reconnect every tool turn.

References: [SDK actions and envelope](https://github.com/opencoredev/login-with-chatgpt/blob/a1330697f35a1c6429009953acc8ec3e2f9f1f1c/packages/core/src/realtime.ts), [independent realtime guide](https://loginwithchatgpt.dev/docs/guides/realtime-voice).

## 3. Can relay roles or text markers guarantee a fact-based spoken answer?

No. The inspected relay constructor uses a **user** message. A made-up `system`/`tool` role or text such as `[System Update]` and `[Tool Result]` is not evidence of authenticated tool-message support. An accepted relay is not proof of faithful audio recitation. A prompt can improve the probability of using supplied data, but cannot create a missing tool channel, cancel the already-started reply, or guarantee behavior.

A truthful experimental user relay could say:

```text
The Station program has already retrieved these data; you do not need to browse.
Read the supplied facts in Russian. Source: ...; source date/time: ... .
Data: ... . Say when a requested fact is missing. Do not claim that you visited the site.
```

Do not treat this as a production guarantee or feed untrusted page instructions into a privileged role. The patch removes the relay/priming path entirely for local answers: retrieved text goes to neural synthesis, not back into the autonomous `/wm` answer. Completion of local synthesis leaves that remote turn **blocked**, rather than timer-unmuting its refusal.

The client also no longer interprets guessed public Realtime function-call events as its application tool bridge. Typed rates/weather failures return a failure, not stale encyclopedia text presented as current data.

## 4. Can Codex app-server supply a native tool/voice bridge?

The inspected third-party bridge uses a substantially different flow:

```text
Station Pion audio <--------------------------> OpenAI realtime voice
       |                                                   ^
       | SDP/control over authenticated private bridge     | native appendSpeech
       v                                                   |
Home/VPS bridge <------ JSON-RPC stdio ------> Codex app-server
       |                                   handoff -> execution model
       +---- bounded typed weather/rates/search tools <---- item/tool/call
```

Run app-server and execution-model orchestration on a home server/VPS, not the 256 MB Station. A separate execution model is part of this bridge; "no second model" marketing should not be interpreted literally.

### Inspected initialization and event flow

The following shapes are from inspected source, not a claim that they have run successfully on your account:

1. Initialize with an honest `clientInfo` and `capabilities.experimentalApi: true`; then send `initialized`.
2. `thread/start` creates an ephemeral, read-only thread with `approvalPolicy: "never"`, `threadSource: "realtime_voice"`, execution instructions, permitted `dynamicTools`, and a `speak_to_user` helper. Select an execution model actually available to the account; do not copy a retired default model from an SDK.
3. Start realtime:

```json
{
  "method":"thread/realtime/start",
  "params":{
    "threadId":"<thread-id>",
    "outputModality":"audio",
    "clientManagedHandoffs":false,
    "flushTranscriptTailOnSessionEnd":true,
    "codexResponsesAsItems":false,
    "includeStartupContext":false,
    "prompt":"<realtime instructions for this app-server session>",
    "transport":{"type":"webrtc","sdp":"<Station offer>"},
    "version":"v3",
    "voice":"juniper"
  }
}
```

4. The answer SDP arrives through a separate `thread/realtime/sdp` notification. Apply that SDP to the existing Station peer connection; starting another browser audio session is not equivalent.
5. The inspected bridge observes `thread/realtime/itemAdded` items of type `handoff_request`. With `clientManagedHandoffs: false`, do not start a duplicate execution turn merely because that observation arrived.
6. Dynamic execution tools arrive as **server-originated JSON-RPC requests** `item/tool/call`. Validate thread/turn/call ID, registered tool name, argument schema, cancellation, and bounds. Reply to the RPC request ID using `contentItems` and `success`, not a fake WebRTC `relay_message` role.
7. The helper requests native voice speech with this control-plane call:

```json
{"method":"thread/realtime/appendSpeech","params":{"threadId":"<thread-id>","text":"<bounded verified facts>"}}
```

This is **text for the native realtime speech path**, not client PCM or a Microsoft TTS request. Keep WebRTC media direct where validated. Gate local playback with the existing Station epoch and cancel bridge tools when Alice takes priority.

The SDK mirrors first-party authentication/client behavior and includes an unsupported synthetic attestation-error mechanism. This delivery does **not** copy that mechanism or pretend to be an official desktop client. Use a legitimate supported login and verify the actual installed app-server schema. Browser-session cookies, Codex login, and registered Sign in with ChatGPT tokens are distinct credential/control contracts.

### Free versus Plus

The owner's [Free-account report](https://raw.githubusercontent.com/HazloNazlo/yandex-station-chatgpt-voice/02a1258/docs/FREE_PLAN_BRIDGE_TEST.md) records CLI 0.169.0 producing `CODEX_FREE_OK` and a model-selected `diagnostic_ping` dynamic tool. That is evidence for **non-voice app-server execution on that account/version**. It does not prove realtime audio, native handoff, or native speech.

Current [official help](https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan) includes Free/Go in Codex availability, while the [pricing page](https://learn.chatgpt.com/docs/pricing.md) frames Free/Go as desktop GPT-6 Luna access subject to rollout. Do not turn either statement into a blanket guarantee of experimental app-server voice on every Free account. Plus/Pro inclusion also does not establish third-party private-voice entitlement or an attestation exception. [Official Work/Voice guidance](https://help.openai.com/en/articles/20001275-chatgpt-work-and-codex) shows that native product experiences can use their selected tools; it does not add those tools to this direct `/wm` client.

The documented [open-source Sign in with ChatGPT token-sharing route](https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server.md) and [quickstart](https://developers.openai.com/siwc/quickstart.md) are a separate supported surface. Eligible Responses access is not proof of `/wm` or app-server realtime access.

### Required promotion gates

- **4A, audio entitlement:** legitimate login, exact CLI/schema recorded, realtime session starts and produces nonempty correctly decoded audio; do not count JSON success alone.
- **4B, native handoff:** a spoken user request causes one handoff and one execution/tool path, without a duplicate `turn/start` or client workaround.
- **4C, grounded native speech:** retrieve a fixture containing a random, non-model-known fact; return it through `item/tool/call`; hear the same selected native voice speak it through `appendSpeech`/the native controller. Check errors and missing-data answers too.
- **4D, Station media:** the actual Pion offer/SDP/ICE connects, first syllables survive, playback format is correct, interruption and Alice priority work, and stale results cannot reopen audio. The SDK's browser SDP conventions are not automatically compatible with this audio-only offer.
- **4E, soak:** measure account/model/voice limits, reconnects, tool cancellation, CPU/RSS, network failures and multiple-turn latency before deployment.

References: [bridge audit](https://raw.githubusercontent.com/HazloNazlo/yandex-station-chatgpt-voice/02a1258/docs/DESKTOP_APP_SERVER_BRIDGE.md), [inspected SDK bridge](https://raw.githubusercontent.com/opencoredev/login-with-chatgpt/main/packages/server/src/realtime-app-server.ts), [official app-server documentation](https://learn.chatgpt.com/docs/app-server.md), [CLI 0.169.0 protocol declarations](https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/app-server-protocol/src/protocol/common.rs), [current realtime protocol types](https://github.com/openai/codex/blob/main/codex-rs/app-server-protocol/src/protocol/v2/realtime.rs).

## 5. Implemented Go streaming neural fallback

### Exact module responsibilities

| File | Change |
| --- | --- |
| [audio.go](<../internal/voice/audio.go>) | One generation-tagged callback queue for remote and local PCM; invalidate partial current audio; bounded transcript hold; lossless local backpressure and callback drain acknowledgement |
| [turns.go](<../internal/voice/turns.go>) | User-turn cancellation/ownership, processed-PCM RMS gate, atomic stale-turn checks and worker registration ordered against peer teardown |
| [speech.go](<../internal/voice/speech.go>) | Private local configuration, HTTPS bearer request, strict framed-PCM consumer, bounds/cancellation, no retry and no unmute on EOF |
| [dispatch.go](<../internal/voice/dispatch.go>) | Transcript-driven intent dispatch, typed local retrieval, truthful failure speech, ordinary-turn remote release |
| [protocol.go](<../internal/voice/protocol.go>) | Existing session initialization; observed optional stop encoder; bounded transcript assembly and completed-ID deduplication |
| [call.go](<../internal/voice/call.go>) | Attach audio/coordinator before negotiation, close coordinator before teardown wait; optional config validation; remove old relay/public-function-call handling |
| [tools.go](<../internal/voice/tools.go>) | Remove Google MP3/aplay speech and priming; check typed response status/JSON/units/date/arrays; fail honestly; bound weather cache |
| [proxy](<../services/neural-tts/README.md>) | Complete Python 3.11+ aiohttp/Edge/ffmpeg streaming service and lifecycle tests |

The supplied patch applies all these changes together. Copying only the old speech function is not sufficient: completion, callback current-buffer ownership, RTP generation, and peer teardown must agree.

### Actual audio formats and playback ownership

This inspected code captures **16 kHz mono s16, 320 samples per 20 ms frame** from native processed audio. Its Opus RTP clock is **48 kHz**; that clock is not the capture sample rate. Playback is **48 kHz stereo signed16le**, 960 sample frames/20 ms. Preserve native VQE/AEC and unity software gain; measure level differences rather than restoring an unverified +5 dB boost.

```text
new detected input
    cancel old retrieval/HTTP/local writer
    bump generation -> pending; discard old callback PCM
    hold bounded remote audio until completed transcript
        ordinary turn -> allow retained remote prefix
        tool turn     -> discard held remote; claim local
            retrieve -> proxy MP3 decode/resample -> bounded PCM queue
            successful marker + clean EOF + callback drain
            retire local -> blocked (NOT remote)
        next genuine input -> new generation and classification
Alice epoch revocation -> cancel all work; callback emits silence
```

Queue limits are explicit: normal remote admission **12 blocks/240 ms**, pending hold **50 full blocks/up to 1 second**, local queue **8 full blocks/up to 160 ms**. Releasing a longer held prefix temporarily permits bounded continuation within the same 50-block capacity; the normal remote limit returns when that backlog drains. This avoids dropping new words merely because classification released more than 12 held blocks. A held-answer overflow fails closed rather than silently dropping its first syllables. No completed transcript within 60 seconds fails the call; a timer never reopens an unclassified answer. Shorter packets mean shorter actual queued durations.

The bounded transcript assembler handles the repository's inspected add/append/replace/status shapes, mixed string/text parts, and assistant-start completion fallback. It is not advertised as a complete protocol for arbitrarily interleaved message IDs or future `/wm` event versions. Unsupported ordering/paths require sanitized captures and new contract fixtures, not guessed tool execution.

### Why synthesis belongs off-device

Edge emits MP3. The proxy concurrently streams MP3 into ffmpeg stdin, drains stdout/stderr, resamples to 48 kHz stereo s16le, and sends bounded PCM packets. The Station does not run Python, a neural model, MP3 decoding/resampling, or a separate speech `aplay` process. Existing short wake/end cue subprocesses are unchanged; the no-aplay statement applies to **local spoken answers**.

PCM uses 192,000 bytes/s, approximately **1.536 Mbit/s** before protocol overhead. This is a deliberate home-network bandwidth tradeoff for a simple, already-supported malgo callback format. Proxy-side Opus could reduce bandwidth, but would need another validated packet/decoder contract and is not included. No claim is made that Silero can run in realtime on this ARM CPU/256 MB device; benchmark it on a server if choosing a local upstream.

### Wire contract and local configuration

```http
POST /v1/speech
Authorization: Bearer <independent-device-token>
Content-Type: application/json

{"text":"По данным источника ...","voice":"ru-RU-SvetlanaNeural"}
```

```http
Content-Type: application/vnd.station.pcm-stream; version=1
X-Audio-Format: pcm_s16le
X-Audio-Sample-Rate: 48000
X-Audio-Channels: 2
X-Audio-Sample-Bits: 16
X-Audio-Framing: length-prefix-le32
X-Audio-Max-Frame-Bytes: 3840
```

The body repeats `uint32_le N` plus exactly `N` PCM bytes: `4 <= N <= 3840`, divisible by 4. A **mandatory zero-length success marker** follows only successful provider completion, successful decode, ffmpeg exit zero, and cleanup. The Go consumer also requires clean HTTP EOF with no extra bytes. Aligned EOF without the marker is a failure, including reverse-proxy-normalized truncation. Never feed the framing prefix to the audio device or retry a partial utterance.

Install the following private, client-owned mode-600 file on the Station:

```json
{
  "url":"https://speech.example.org/v1/speech",
  "token":"<replace-with-independent-random-device-token>",
  "voice":"ru-RU-SvetlanaNeural",
  "barge_in_rms":300,
  "experimental_wm_stop_speaking":false,
  "log_transcripts":false
}
```

```text
/data/yandex-station-chatgpt-voice/neural-tts.json
```

Missing configuration explicitly disables local spoken tool answers; ordinary `/wm` voice still works. Invalid configuration is a configuration error. There is no old Google fallback. Only Svetlana/Dmitry are accepted. HTTPS is required except literal-loopback HTTP through a trusted tunnel; LAN HTTP, URL credentials/query/fragment, and redirects are rejected. This device token is **not** the ChatGPT cookie/OAuth token. The proxy receives local answer text, never the ChatGPT credential or microphone stream.

The Go consumer bounds text to 2000 runes with a spoken truncation notice, total request/playout to 45 seconds, and PCM to 8,640,000 bytes. Proxy defaults are two concurrent requests with no admission queue, first valid PCM within 8 seconds, total work within 30 seconds, plus bounded process cleanup. For this Station deployment, set `NEURAL_TTS_MAX_AUDIO_SECONDS=45`; the generic proxy's independent default is 120. Long responses can fail a deadline: neither text length nor a network success implies the whole utterance was spoken.

## Retrieval and accuracy limits

- Currency comes from the CBR XML-daily **mirror**, not a direct guaranteed official live endpoint. The answer speaks publication date and reference RUB-per-unit USD/EUR/CNY, normalizes nominal, and distinguishes bank buy/sell rates. It is not a stock-quote implementation.
- BTC/ETH quotes validate the exact Binance pair and positive finite price. The output says **USDT**, not guaranteed US dollars, and labels receipt time in Moscow; exchange availability/rate limiting still apply.
- Open-Meteo validates coordinates, presence of current values, timestamps, WMO codes, physics bounds, and daily array/null bounds. Zero degrees is valid; missing temperature is not zero. Supported local forecast offsets are today/current, tomorrow, and day after tomorrow. Explicit unsupported historical/week/month requests fail; this keyword router is not general temporal/geographic reasoning.
- The current-weather-only wttr fallback cannot answer tomorrow/day-after requests. It labels receipt time. Weather cache has at most 64 entries, 15-minute reuse, and retains the source/receipt timestamp; cached data is not claimed freshly observed.
- News/general search returns bounded source snippets. For freshness/news requests it does not fall back to Wikipedia. Snippets are explicitly **not independently fact/date verified**. The patch does not turn them into researched multi-source news synthesis.
- Page reading retains SSRF restrictions. Provider outages never prove that the Internet works or that a fact is current. A successful search result is external data, not an instruction to execute.

## Verification and release boundary

Executed on the combined host worktree, not a staged snapshot:

- Go 1.27.4, host darwin/arm64: complete default hermetic `go test -tags=nolibopusfile -race`, `go vet`, and host executable build/CLI help.
- `govulncheck` reported zero reachable or imported-package vulnerabilities. Its module-only advisory [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) concerns unmaintained `golang.org/x/crypto/openpgp`, which this application does not import; this is not a blanket security certification.
- New Go tests cover partly consumed callback invalidation, held prefix release, hold overflow, no post-TTS unmute, local backpressure cancellation, drain callback timing/lease loss, Alice revocation, stale hangup rejection, worker teardown admission, transcript patch/dedup/bounds, framed split reads/truncation/PCM cap, loopback streaming HTTP cancellation, private configuration, and typed data validation.
- Python 3.12.14: **114 proxy tests passed**, Ruff lint/format passed, dependency compatibility and CLI help passed. Real ffmpeg checked in-memory mono MP3 to stereo s16le/48 kHz and signal content. The added streaming regression withholds remaining MP3/EOF, requires useful non-silent PCM first, and checks the prefix, complete output, and sample count against an independent default-ffmpeg reference.
- A direct FFmpeg 9.0.2 experiment exposed startup buffering with the default probing. The corrected input `-probesize 512` produced first PCM about **27-28 ms after decoder start with a preloaded 1,536-byte MP3 prefix**, before remaining input/EOF, without losing analyzed audio. This is an offline decoder measurement, **not** live Edge latency, end-of-utterance latency, or DAC onset; no `nobuffer`/direct-I/O shortcut is used.
- Three pre-existing external-provider tests now require `VOICE_LIVE_TOOLS_TESTS=1`; default tests use deterministic provider fixtures. They were not counted as live-provider successes.

**Not executed:** ARM cross-build, native ALSA/VQE/spotter runtime, ADB deployment, real Edge synthesis/voice quality, live `/wm` action acceptance, account realtime/handoff entitlement, or same-voice Codex/Station integration. No hardware or account credentials were available. This environment has no ADB/Zig/uv/readelf helpers, and the existing ARM build deliberately rejects the current whitespace-containing checkout path.

The actual repository requires **Go 1.27.4**, not the requested general Go 1.23+ assumption. An ARM target configured for glibc 2.23 does not prove compatibility with a different CPU, kernel, native firmware ABI, or audio stack. The baseline README's Amlogic/kernel 4.9.68 identification conflicts with the stated Allwinner A33/kernel 3.10 target; the ADB procedure starts by resolving that discrepancy. Do not flash/root/deploy using the wrong board instructions.

For reproduction, build/application steps, acoustics, measured latency/resource gates and rollback, use the [ADB procedure](<ADB_NEURAL_TTS_VERIFICATION.md>) and [proxy README](<../services/neural-tts/README.md>). Host tests are necessary but do not certify the desired seamless user experience.

Distribution note: the baseline README advertises MIT, but this inspected checkout contains no root license file. Confirm upstream licensing before redistribution. No proprietary firmware library, vendored dependency, credential, or unvalidated ARM binary is included in the delivery.
