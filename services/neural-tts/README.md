# Neural TTS proxy

Python 3.11+ host-side speech synthesis for the Station consumer. The server uses
Microsoft's Russian neural voices through `edge-tts` and decodes its MP3 stream with
ffmpeg, entirely through asynchronous in-memory pipes. Audio is signed 16-bit
little-endian PCM at 48000 Hz, stereo, carried in the version-one framing below.

**Edge TTS is unofficial, has no availability SLA, and can change or rate-limit
without notice. These are Microsoft Svetlana/Dmitry voices, not ChatGPT Vale or any
other OpenAI voice.** This service never selects or impersonates a ChatGPT voice.
Text leaves your machine and is sent to Microsoft's Edge speech service. The proxy
never retries a failed utterance or changes voices after a partial stream.

## Run locally

Install a supported Python 3.11+ and a patched ffmpeg on the host. ffmpeg must be on
PATH, or set `NEURAL_TTS_FFMPEG` to its executable. From the repository root:

```sh
cd services/neural-tts
python3.11 -m venv .venv
.venv/bin/python -m pip install --no-cache-dir -r requirements.txt
export NEURAL_TTS_TOKEN="$(.venv/bin/python -c 'import secrets; print(secrets.token_urlsafe(32))')"
.venv/bin/python -m neural_tts
```

The default address is `127.0.0.1:8090`. Provision the same random token to the
Station consumer through its secret configuration; do not print, commit, put in a
URL, or include it in shell history. The example generates an ephemeral development
token without displaying it. For persistent use, inject a secret from your service
manager or secret store. Missing, blank, short, non-ASCII, or whitespace-containing
tokens prevent startup. Tokens must contain 32-256 visible ASCII characters.

On Windows use `py -3.11 -m venv .venv` and `.venv\Scripts\python.exe` instead of the
Unix virtual-environment executable. Set environment variables through PowerShell
or the service manager. No credentials are needed to install dependencies or run
the tests.

For a Station on another device, put a TLS-terminating reverse proxy in front of
this loopback service and configure the consumer with its **HTTPS** endpoint. Plain
HTTP exposes the bearer token and transcript outside a trusted loopback connection.
If binding to a LAN IP is necessary, explicitly set `NEURAL_TTS_HOST` and restrict
access with a firewall. The proxy does not implement TLS or CORS. Keep authorization
headers out of reverse-proxy logs and disable response buffering for low latency.
Preserve the content type and all `X-Audio-*` headers.

## Wire contract

Request:

```http
POST /v1/speech HTTP/1.1
Authorization: Bearer <shared-random-token>
Content-Type: application/json

{"text":"Привет!","voice":"ru-RU-SvetlanaNeural"}
```

- `text` must be a nonblank string, at most 4096 Unicode code points and 16384 UTF-8
  bytes by default. Limits include surrounding whitespace; text is not truncated.
- `voice` may be omitted, selecting `ru-RU-SvetlanaNeural`. The only other allowed
  voice is `ru-RU-DmitryNeural`. Values are case-sensitive; aliases such as `Vale`
  are not accepted.
- Unknown fields, duplicate JSON keys, non-finite JSON numbers, invalid UTF-8 or
  Unicode, and unsupported control characters are rejected. Plain text is passed
  to `edge-tts`, which escapes SSML. User-supplied SSML is not supported.
- The JSON body is limited to 32768 bytes by default, including when transferred
  chunked. Request compression is rejected before reading or decompressing it.
- The bearer scheme is case-insensitive. Tokens are compared using constant-time
  `hmac.compare_digest` on fixed-length SHA-256 digests. Duplicate Authorization
  headers are rejected. HTTP optional surrounding whitespace is normalized by
  aiohttp's HTTP parser.

Success headers (additional standard HTTP headers are allowed):

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.station.pcm-stream; version=1
X-Audio-Format: pcm_s16le
X-Audio-Sample-Rate: 48000
X-Audio-Channels: 2
X-Audio-Sample-Bits: 16
X-Audio-Framing: length-prefix-le32
X-Audio-Max-Frame-Bytes: 3840
Cache-Control: no-store
X-Content-Type-Options: nosniff
X-Accel-Buffering: no
X-Request-ID: <server-generated-id>
```

Headers are prepared **only after the first nonempty valid whole-frame PCM chunk**.
The response is streamed, not buffered to its full duration. There is no HTTP
response compression and no content length. HTTP chunk boundaries do not have to
match audio packet boundaries.

### PCM packet framing

The HTTP body is a concatenation of:

1. A **four-byte unsigned little-endian length** `N`.
2. Exactly `N` bytes of headerless PCM.
3. More packets, followed by a mandatory **four-byte zero length** success marker.

For an audio packet, `4 <= N <= 3840` and `N % 4 == 0`. Each four-byte PCM frame
contains signed 16-bit little-endian left and right samples, interleaved. A full
3840-byte packet carries 20 ms. ffmpeg output is reassembled across partial reads
and split into packets at whole stereo-frame boundaries. Length prefixes are
transport metadata, not audio; the Station consumer must remove them before playback.

A zero length is not an empty audio packet. It is written **only after** the
provider reaches successful EOF, ffmpeg finishes with exit code zero, all input and
output bounds pass, all PCM is whole-frame, and the subprocess and iterators close
successfully. A successful stream contains at least one audio packet.

The client must require valid headers, valid packet lengths, complete payloads, the
zero marker, and then clean EOF with no trailing data. Missing markers, premature
EOF (even on a four-byte PCM boundary), and any read error mean a failed turn.
Cancel/flush any remaining playback according to the Station turn gate; **do not
retry or speak the text again after partial output**.

This envelope intentionally replaces an unframed `application/octet-stream` body.
It detects aligned truncation even when an HTTP/2 reverse proxy converts a broken
upstream chunked response into a clean HTTP EOF. The PCM inside each envelope has
no WAV/RIFF, MP3, or other media header. Do not feed the envelope directly to an
ordinary WAV/MP3 player.

### Errors and cancellation

Before success headers, failures use `application/json`:

```json
{"error":{"code":"provider_failure","message":"Speech provider failed."}}
```

| Status | Codes and meaning |
| --- | --- |
| 400 | `invalid_request`: malformed JSON, invalid fields, forbidden voice or text |
| 401 | `unauthorized`: missing/invalid bearer token, including duplicate headers |
| 413 | `body_too_large` or `text_too_long`: request bound exceeded |
| 415 | `unsupported_media_type`: wrong content type, charset or content encoding |
| 429 | `busy`: all synthesis slots occupied; no queue; `Retry-After: 1` |
| 500 | `internal_error`: unexpected internal failure, without exception details |
| 502 | `provider_failure`, `decoder_failure`, `no_audio` or `audio_limit_exceeded` |
| 503 | `shutting_down` or `cleanup_failure`: no work accepted; restart after cleanup failure |
| 504 | `deadline_exceeded`: body, first-audio, idle, write or total deadline |

After success headers, a provider/decoder failure or expired deadline cannot change
HTTP status. The server aborts the connection **without the zero success marker**;
it never appends JSON or an error message to audio. A client disconnect cancels the
handler, upstream stream, and decoder even while waiting for the first audio.

## Resource and lifetime bounds

The admission check and slot acquisition have no intervening await. The service
rejects excess work immediately, before reading its body or creating a provider or
ffmpeg process. A slot covers body reading, synthesis, decoding, response writes,
and cleanup. There is no synthesis queue and no audio queue.

The MP3 feeder, PCM reader, and stderr drainer run concurrently. Pipe reads/writes
are bounded (16384 bytes for audio, 4096 bytes for stderr); at most 8192 bytes of
stderr are retained and its contents are never logged. Awaited stdin draining and
HTTP writes propagate backpressure. The MP3 input format and pipe-only ffmpeg
protocol allowlist prevent using the provider stream as a file/network playlist.
ffmpeg receives fixed arguments, not a shell command, text, or client-selected path.
The forced MP3 input uses `-probesize 512` to limit startup stream analysis while
retaining the analyzed audio. It still needs enough initial codec/packet data;
there is no promise of output after an arbitrary few bytes. No `nobuffer` or
input-direct-I/O flags discard or bypass the initial audio.
Its environment contains only runtime executable/library paths and OS temporary
paths, not bearer/provider credentials, proxies, or `FFREPORT`. Report-file logging
is therefore disabled, too.

On success, failure, cancellation, early iterator close, or shutdown the service
closes stdin, cancels and joins the feeder, drains stdout during shutdown, sends
terminate if necessary, then kill after its grace period, and waits to reap ffmpeg.
Process creation is owned through cancellation so a process created concurrently
with a disconnect cannot escape cleanup. A second cancellation cannot interrupt
owned cleanup. Pipe tasks are joined; audio and transcripts are never written to
disk. OS-level inability to reap a killed process, or failure to join a pipe task,
produces a safe error log and `cleanup_failure`; the service refuses further work
until restarted rather than exceeding its concurrency cap or claiming success.

Application logs contain only a generated request ID, outcome code, decoded byte
count and elapsed time. The CLI disables aiohttp access logging. Token, request
text, upstream exception details, peer addresses, and ffmpeg stderr are not logged.
When embedding the application in another aiohttp runner, preserve
`handler_cancellation=True`, `auto_decompress=False`, and disable its access logger.

## Environment configuration

All values are optional except `NEURAL_TTS_TOKEN`. Numeric limits must be positive;
seconds must also be finite. Invalid configuration fails startup without echoing
its value. Smaller limits are useful for testing and restrictive deployments.

| Variable | Default |
| --- | --- |
| `NEURAL_TTS_TOKEN` | Required, 32-256 visible ASCII characters |
| `NEURAL_TTS_HOST` | `127.0.0.1` (bind IP literal; `::1` also supported) |
| `NEURAL_TTS_PORT` | `8090` |
| `NEURAL_TTS_FFMPEG` | `ffmpeg` |
| `NEURAL_TTS_MAX_CONCURRENCY` | `2` |
| `NEURAL_TTS_MAX_BODY_BYTES` | `32768` |
| `NEURAL_TTS_MAX_TEXT_CHARS` | `4096` |
| `NEURAL_TTS_MAX_TEXT_BYTES` | `16384` |
| `NEURAL_TTS_MAX_ENCODED_BYTES` | `2097152` |
| `NEURAL_TTS_MAX_AUDIO_SECONDS` | `120` (23040000 decoded PCM bytes) |
| `NEURAL_TTS_BODY_TIMEOUT_SECONDS` | `5` |
| `NEURAL_TTS_FIRST_AUDIO_TIMEOUT_SECONDS` | `8` |
| `NEURAL_TTS_IDLE_TIMEOUT_SECONDS` | `10` |
| `NEURAL_TTS_WRITE_TIMEOUT_SECONDS` | `10` |
| `NEURAL_TTS_TOTAL_TIMEOUT_SECONDS` | `30` |
| `NEURAL_TTS_TERMINATE_TIMEOUT_SECONDS` | `1` |
| `NEURAL_TTS_KILL_TIMEOUT_SECONDS` | `2` |

The total deadline includes reading the body, network synthesis, decoding and
client writes; the first-audio deadline starts after request validation. A deadline
may be followed by the finite process-cleanup window (normally at most five
seconds with defaults). An excessive synthesized audio duration is a failure, not
silent truncation. The service currently includes only the Edge provider; it does
not require Azure or OpenAI credentials.

## Tests and checks

From this directory, install the pinned development dependencies inside the local
virtual environment, then run:

```sh
.venv/bin/python -m pip install --no-cache-dir -r requirements-test.txt
PYTHONDONTWRITEBYTECODE=1 .venv/bin/python -m pytest -q
.venv/bin/python -m ruff check neural_tts tests
.venv/bin/python -m ruff format --check neural_tts tests
.venv/bin/python -m pip check
```

Tests use fake providers and decoders, loopback aiohttp clients, and fake subprocess
pipes. They cover authentication, JSON/voice/body/text bounds, early streaming,
packet splitting and completion, concurrency without queues, disconnects, body/
first-audio/idle/total deadlines, safe logs, cancellation during creation and
cleanup, stderr backpressure, invalid decode output, terminate/kill/reap, and
shutdown. Optional real-ffmpeg tests generate synthetic MP3 in memory, check stereo
samples and their 440 Hz frequency, and hold remaining input and EOF after a
1536-byte prefix. The streaming regression compares useful early PCM and the full
output/sample count against an independent default-ffmpeg decode. These tests
skip only if ffmpeg is unavailable. No test contacts Edge or uses speech credentials.

## Dependencies and license notes

Runtime direct pins are in [requirements.txt](requirements.txt); test/tool pins are
in [requirements-test.txt](requirements-test.txt). Stable versions were checked
against PyPI on 2026-11-17. Refresh compatible pins and run security checks before
redistributing or deploying; a current version is not a guarantee of availability
or the absence of vulnerabilities.

- [aiohttp 3.14.5](https://pypi.org/project/aiohttp/3.14.5/) supports Python 3.10+;
  its license expression is Apache-2.0 AND MIT (including its llhttp component).
- [edge-tts 7.3.1](https://pypi.org/project/edge-tts/7.3.1/) supports Python 3.7+
  and aiohttp 3.x; its code is LGPLv3 with an MIT-licensed subtitle-helper exception.
  Keep its upstream license and notices when redistributing the separate package.
  Its library license does not grant an SLA or blanket permission to use Microsoft's service.
- [pytest 9.2.0](https://pypi.org/project/pytest/9.2.0/) and
  [Ruff 0.17.5](https://pypi.org/project/ruff/0.17.5/) are MIT;
  [pytest-asyncio 1.5.0](https://pypi.org/project/pytest-asyncio/1.5.0/) is Apache-2.0.
- ffmpeg is an external executable, not bundled here. Its license depends on build
  options and linked codecs (LGPL/GPL and possibly non-redistributable builds).
  Check your distribution against [ffmpeg's legal guidance](https://ffmpeg.org/legal.html).

Retain the notices for transitive dependencies shipped by your installer as well.
No new license is assigned to the surrounding project's source by these notes.
