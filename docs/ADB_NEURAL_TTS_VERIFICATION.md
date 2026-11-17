# ADB verification for the neural TTS fallback

This is a procedure to run on your own Station after reviewing the [architecture and limitations](<SEAMLESS_VOICE_REVIEW.md>). No ADB, ARM hardware, account, or live Edge test was executed during implementation. Do not treat the host test results as device certification.

## 1. Identify the device before changing anything

Use one explicitly selected ADB serial. These examples are host shell commands; replace placeholders with the actual device, not an assumed model:

```sh
adb devices -l
export ADB_SERIAL='<exact serial shown by adb devices>'
adb -s "${ADB_SERIAL:?}" shell id
adb -s "${ADB_SERIAL:?}" shell uname -a
adb -s "${ADB_SERIAL:?}" shell 'cat /proc/cpuinfo; cat /proc/meminfo'
adb -s "${ADB_SERIAL:?}" shell 'date -u; command -v getconf; command -v getprop'
adb -s "${ADB_SERIAL:?}" shell 'getconf GNU_LIBC_VERSION'
adb -s "${ADB_SERIAL:?}" shell 'getprop ro.product.model; getprop ro.board.platform; getprop ro.product.cpu.abi'
```

Some firmware has no `getprop` or `getconf`; an unavailable command is not a compatibility pass. Record the CPU/ABI, kernel, libc/loader, memory, and firmware from available sources. The request says Allwinner A33/kernel 3.10, while the baseline README identified Amlogic A113X/kernel 4.9.68. Resolve that difference. If libc is Bionic rather than GNU glibc, do not assume this `arm-linux-gnueabihf.2.23` build will load.

Verify root (`uid=0`) and the native firmware interfaces used by the existing client:

```sh
adb -s "${ADB_SERIAL:?}" shell 'ls -ld /data /system/vendor/quasar /system/vendor/quasar/libs'
adb -s "${ADB_SERIAL:?}" shell 'ls -l /system/vendor/quasar/set_volume.sh'
adb -s "${ADB_SERIAL:?}" shell 'command -v ss; command -v netstat'
# Run whichever is present. Capture is expected on native loopback port 9850.
adb -s "${ADB_SERIAL:?}" shell 'ss -ltn'
```

Check the native state/spotter ABI as required by the existing Station client. Do not open a second capture reader while the running client owns processed audio. Do not stop Quasar, disable VQE/AEC, or run a root/flashing tool intended for another board. Preserve Alice and its wake-word priority.

## 2. Prepare a clean compatible source checkout

Use a fresh checkout or a copy with no unrelated work. The patch base is exactly:

```text
f230f1cdb9173414fbe39c60fe35b94e5f040c1d
```

The current source requires Go **1.27.4** and host Opus/pkg-config/C compiler for host tests, Python **3.10+** for the existing Station helper, and uv/Zig for the existing ARM build. The proxy separately requires Python **3.11+** and ffmpeg. Do not force Go 1.23 or silently rewrite the module version.

The ARM build rejects any whitespace in the checkout path. Choose a path without spaces; do not bypass its Opus/libtool guard. From a fresh checkout, apply the extracted package's patch:

```sh
# BUNDLE is the directory extracted from the supplied ZIP, containing changes.patch.
export BUNDLE='<absolute path to extracted station-neural-tts directory>'
git status --short
git rev-parse HEAD
# Ensure HEAD is the recorded base and there is no conflicting work before applying.
git apply --check "${BUNDLE:?}/changes.patch"
git apply "${BUNDLE:?}/changes.patch"
```

Complete replacement files are under the package's `replacement/` directory; using the patch is preferable because it includes coordinated changes and tests. Do not replace only the speech function. No commit/staging is needed to test the worktree.

Use workspace-local Go caches if normal cache directories are not writable:

```sh
export GOMODCACHE="$PWD/.cache/go-mod"
export GOCACHE="$PWD/.cache/go-build"
export GOPATH="$PWD/.cache/go-path"
export GOTOOLCHAIN=auto
go version
go test -mod=readonly -tags=nolibopusfile -race -count=1 -timeout=90s ./...
go vet -mod=readonly -tags=nolibopusfile ./...
```

The three opt-in external tool tests are separate from deterministic unit tests:

```sh
VOICE_LIVE_TOOLS_TESTS=1 go test -mod=readonly -tags=nolibopusfile \
  -run 'TestSearchWebLive|TestOpenURL_Public|TestWeather$' -v ./internal/voice
```

That live run can fail because Binance/CBR/Open-Meteo/DDG is blocked, changed, or rate-limited. Investigate the actual provider response rather than restoring success-shaped fallback text.

Prepare the pinned Zig tool used by the inspected [build script](<../tools/build-arm.sh>), then build:

```sh
uv tool install ziglang==0.17.1
uv tool run --offline --from ziglang==0.17.1 python-zig version
make build
file build/yandex-station-chatgpt-voice
# With host binutils/readelf available:
readelf -h build/yandex-station-chatgpt-voice
readelf -A build/yandex-station-chatgpt-voice
readelf -l build/yandex-station-chatgpt-voice
readelf --version-info build/yandex-station-chatgpt-voice
```

Confirm Linux ELF32/ARM EABI, the required hard-float ABI, loader, and symbol versions against the actual firmware. The configured glibc 2.23/Cortex-A7 target is not a kernel/firmware ABI test. Do not use a pre-existing bundled binary expecting the new source changes. Full repository `make check` additionally requires its existing developer tools; it was not run in this host environment.

## 3. Start and verify the proxy on the home server/VPS

Follow the [complete proxy README](<../services/neural-tts/README.md>). From the service directory:

```sh
python3.11 -m venv .venv
.venv/bin/python -m pip install --no-cache-dir -r requirements.txt
.venv/bin/python -m pip install --no-cache-dir -r requirements-test.txt
PYTHONDONTWRITEBYTECODE=1 .venv/bin/python -m pytest -q
.venv/bin/python -m ruff check neural_tts tests
.venv/bin/python -m ruff format --check neural_tts tests
.venv/bin/python -m pip check

# Ephemeral development token: generated but not printed or put in a URL.
export NEURAL_TTS_TOKEN="$(.venv/bin/python -c 'import secrets; print(secrets.token_urlsafe(32))')"
export NEURAL_TTS_MAX_AUDIO_SECONDS=45
.venv/bin/python -m neural_tts
```

Keep the token in a secret store/service-manager environment for persistent operation. Its required length is 32-256 visible ASCII characters. Default bind is `127.0.0.1:8090`; never publicly expose that plaintext backend. It has two synthesis slots, no wait queue, first-PCM deadline 8 seconds, total deadline 30 seconds, and bounded cleanup. Errors before headers are safe JSON; failures after headers have no success marker.

Put a TLS frontend in front of the loopback service. For example, a Caddy route can stream the response without batching:

```caddyfile
speech.example.org {
    @speech {
        path /v1/speech
        method POST
    }
    handle @speech {
        reverse_proxy 127.0.0.1:8090 {
            flush_interval -1
        }
    }
    handle {
        respond "Not found" 404
    }
}
```

This is configuration to adapt to your host, not a server started by this task. Use a valid certificate/name, preserve Authorization and all `X-Audio-*` response headers, disable response buffering/compression, and do not log tokens or answer text. Restrict access at the firewall/frontend too; the application admission cap does not limit every TCP connection at the reverse proxy. For stronger operational isolation, run as an unprivileged service with a cgroup/process/memory limit and control-group shutdown so a broken OS subprocess cannot outlive service restart.

Validate the HTTPS chain and correct device clock/CA store. Never use `curl -k`, `InsecureSkipVerify`, or an HTTP LAN URL as a fix. HTTP is allowed by the Go consumer only for a literal loopback address through a trusted tunnel. ADB-over-Wi-Fi is not an encrypted production tunnel; an ADB reverse experiment belongs only on an isolated development network.

Before a real provider trial, the proxy tests and real-ffmpeg synthetic tone test should pass. A separate, authorized short Edge synthesis must then verify actual voice quality, first PCM, and a successful zero marker plus clean EOF. The offline tone test does **not** prove neural synthesis or Edge availability.

## 4. Back up and install using the existing management tools

From the repository root, use the [existing helper](<../tools/station.py>) through the [Make targets](<../Makefile>), not a second unmanaged speaker process. Set the device IPv4 address explicitly:

```sh
export STATION_IP='<actual Station IPv4 address>'
make STATION_IP="${STATION_IP:?}" connect
# Make selects the network endpoint. Use that same endpoint for subsequent raw ADB commands.
export ADB_SERIAL="${STATION_IP:?}:5555"
adb -s "${ADB_SERIAL:?}" shell id
adb -s "${ADB_SERIAL:?}" shell uname -a
# Confirm this is the same hardware verified in step 1 before any stop/install.
make STATION_IP="${STATION_IP:?}" status
make STATION_IP="${STATION_IP:?}" stop
make STATION_IP="${STATION_IP:?}" install
```

`install` builds/verifies the binary, stops the verified old client, retains its previous-client backup, and leaves the new client **stopped**. Inspect that outcome before activation. It preserves the ChatGPT session; do not copy or print it. If credentials truly need renewal, use `make ... login` with your own browser/account rather than extracting them into a command line.

Verify executable loading without starting capture:

```sh
adb -s "${ADB_SERIAL:?}" shell '/data/yandex-station-chatgpt-voice/client --help'
```

A missing loader, illegal instruction, unresolved GLIBC symbol, or native-library ABI error is a stop condition. Do not proceed to microphone/playback by assuming the configured target proves compatibility.

## 5. Provision the independent device token privately

On a trusted host holding the same proxy token, run this from the **repository root** to create the device config without putting the token in shell history. Keep the proxy running in its service/terminal; supply its existing token in this provisioning terminal through the secret store, rather than generating a second token. This example refuses to overwrite an existing host file:

```sh
# Use the real HTTPS frontend. This is a separate device credential, not OpenAI auth.
export NEURAL_TTS_URL='https://speech.example.org/v1/speech'
python3 - <<'PY'
import json, os
config = {
    'url': os.environ['NEURAL_TTS_URL'],
    'token': os.environ['NEURAL_TTS_TOKEN'],
    'voice': 'ru-RU-SvetlanaNeural',
    'barge_in_rms': 300,
    'experimental_wm_stop_speaking': False,
    'log_transcripts': False,
}
fd = os.open('neural-tts.json', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w', encoding='utf-8') as stream:
    json.dump(config, stream, ensure_ascii=False)
PY
```

The root-level host config is ignored by the supplied repository changes. Keep private backups outside the delivered package. If the Station already has a config, inspect/back it up privately first; do not overwrite a symlink or an unreviewed path. Confirm the application directory resolves to the expected absolute location before any rename:

```sh
adb -s "${ADB_SERIAL:?}" shell 'readlink -f /data/yandex-station-chatgpt-voice'
# Expect exactly /data/yandex-station-chatgpt-voice, and stop on any unexpected result.
adb -s "${ADB_SERIAL:?}" shell 'test -d /data/yandex-station-chatgpt-voice && test ! -L /data/yandex-station-chatgpt-voice'
adb -s "${ADB_SERIAL:?}" shell 'test ! -e /data/yandex-station-chatgpt-voice/neural-tts.json.new && test ! -L /data/yandex-station-chatgpt-voice/neural-tts.json.new'
adb -s "${ADB_SERIAL:?}" push neural-tts.json /data/yandex-station-chatgpt-voice/neural-tts.json.new
adb -s "${ADB_SERIAL:?}" shell 'chown 0:0 /data/yandex-station-chatgpt-voice/neural-tts.json.new && chmod 600 /data/yandex-station-chatgpt-voice/neural-tts.json.new'
adb -s "${ADB_SERIAL:?}" shell 'readlink -f /data/yandex-station-chatgpt-voice/neural-tts.json.new'
```

Check that the source resolves exactly to the named `.new` file and the destination is absent or the intended regular private config, never a symlink. After making any needed private backup and reviewing both paths:

```sh
adb -s "${ADB_SERIAL:?}" shell 'test ! -L /data/yandex-station-chatgpt-voice/neural-tts.json && mv /data/yandex-station-chatgpt-voice/neural-tts.json.new /data/yandex-station-chatgpt-voice/neural-tts.json'
# Print metadata only, not contents.
adb -s "${ADB_SERIAL:?}" shell 'ls -l /data/yandex-station-chatgpt-voice/neural-tts.json'
```

For the normal root-owned client, mode must be `600` and owner root. A non-root installation must match that client UID. Configuration is read for each call; valid settings apply on the next call. Missing config means no local spoken tool answers. Invalid config fails explicitly. Native ChatGPT voice selection remains independent from the neural provider voice:

```sh
make STATION_IP="${STATION_IP:?}" VOICE=vale start
make STATION_IP="${STATION_IP:?}" status
make STATION_IP="${STATION_IP:?}" logs
```

If the client is already running, use the existing `restart` target to apply startup values. Keep the experimental `/wm` stop flag off for the first hardware tests.

## 6. Run the behavioral matrix

Use authored test phrases, not private conversation recordings. Keep default transcript logging off. The client should report neural speech enabled, playback holds, local tool starts/completion, and local speech drain/byte/timing metrics without answer text or credentials.

| Test | Required observation |
| --- | --- |
| Ordinary question after `Яндекс` | Selected native voice; no lost initial syllable; no permanently held answer |
| USD/EUR/CNY reference rate | Neural voice; source date and reference-rate qualification; not a bank buy/sell quote |
| BTC/ETH | Exact asset, Binance, USDT units, receipt time; outage is not a made-up price |
| Weather now in a named city | Correct city/source/time and units; missing data is not zero degrees |
| Weather tomorrow/day after | Date matches that city's forecast; provider failure never becomes today's wttr weather |
| Fresh news | Search snippets and verification warning; no timeless Wikipedia substitute claimed as today's news |
| Interrupt during retrieval | Old request cancels; old answer does not later become audible |
| Interrupt during neural speech | Old HTTP stream and remaining callback PCM cancel; the next input owns playback |
| Wait after a local answer | The old autonomous `/wm` answer never resumes merely because TTS ended |
| Trigger Alice during retrieval or playback | ChatGPT/local output stops and native Alice owns playback; no late local speech/cue competes |
| Break proxy connection before first PCM | Bounded failure, no speech retry, no remote refusal unmute |
| Break proxy mid-stream or omit success marker | Partial failure is detected, remaining local audio is retired, no repeated utterance/second voice |
| Reject token / wrong audio headers | Safe explicit error, no token/body logged and no incompatible PCM played |
| Rapid successive turns / farewell interrupted by new speech | Stale completion cannot close or unmute the new turn |
| Silence, whisper, nearby noise, assistant playback at several levels | Calibrate RMS gate; identify false triggers/missed speech rather than claiming guaranteed VAD |
| Close while remote audio is held | Inaudible pending media is dropped, not reported as a native drain fault |

Test acoustic leakage with an external recorder rather than another native microphone client. The fixed RMS threshold is a deployment starting point, not proof of speech detection. Lowering it improves sensitivity but can increase false holds; raising it can miss quiet input. The 60-second missing-transcript watchdog deliberately fails closed instead of timer-unmuting. If a strict no-old-session-audio guarantee is required, validate server turn acknowledgement or use a reconnect boundary; local generation alone cannot identify all late untagged RTP.

Only after baseline tests pass should you separately enable and test the observed `stop_speaking` action. Record acceptance/state/audio behavior and cancellation semantics, not just a successful DataChannel write. Do not interpret an SDK constructor as backend acknowledgement.

## 7. Measure latency and resource use

Collect at least 20 ordinary and 20 local-tool trials on the intended network, plus interruptions at multiple speech positions. Define:

- `t0`: actual end of user utterance, from external acoustic measurement.
- `t1`: completed transcript/classification.
- `t2`: completed tool fetch.
- `t3`: first PCM queued by the Go consumer.
- `t4`: first audible sound at the speaker.

The client `first_pcm_ms` metric measures HTTP-request start to **first queued PCM**, not `t0`, actual DAC onset, or total assistant latency. Measure `t4 - t0`, fetch time, first-synthesis time, and queue/device tail independently. Report median/P95/max, source/error rates, and network conditions. Set your acceptance targets before the test; useful initial targets might be P95 acoustic interruption tail below 100 ms and P95 end-of-utterance-to-local-speech below 1.5 s. These are **targets, not measured results or promises**. Final-transcript lookup and a second synthesis service add unavoidable work relative to a native handoff path.

Use the verified client PID from `make status` to inspect CPU/RSS/high-water RSS/threads. On firmware supporting these tools:

```sh
export CLIENT_PID='<verified numeric active client PID>'
case "$CLIENT_PID" in ''|*[!0-9]*) echo 'Use a verified numeric PID' >&2; exit 1;; esac
adb -s "${ADB_SERIAL:?}" shell "grep -E 'VmRSS|VmHWM|Threads' /proc/$CLIENT_PID/status"
adb -s "${ADB_SERIAL:?}" shell 'top -b -n 1'
adb -s "${ADB_SERIAL:?}" shell 'cat /proc/meminfo'
```

Compare idle, native voice, local streaming, and interruption; include native Alice/OS memory. No ARM RSS/CPU number is claimed here. Watch for growing goroutines/threads/RSS, queue overflow, underruns, reconnect storms, and failed cleanup. PCM adds about 1.536 Mbit/s of network payload; neural inference/ffmpeg stays off the Station. Existing short cue `aplay` processes may still appear, but there must be no long-lived separate TTS playback process on the Station.

On the proxy, confirm no more than two owned ffmpeg syntheses, immediate busy rejection, prompt cancellation and process reaping, and no audio/transcript files. A rare `cleanup_failure` rejects further work until restart; it is not successful synthesis. Keep any soak short enough for the account's actual voice allowance, and do not bypass limits.

## 8. Roll back safely

Stop the verified client and use the existing binary backup:

```sh
make STATION_IP="${STATION_IP:?}" stop
make STATION_IP="${STATION_IP:?}" rollback
make STATION_IP="${STATION_IP:?}" status
```

`rollback` restores the previous client and leaves it stopped without changing ChatGPT credentials. Restore any previous private neural configuration separately after verifying its resolved absolute source/destination and permissions. If there was no original config, retain the new file privately under a non-active backup name instead of blindly deleting secrets. Then deliberately start the chosen version:

```sh
make STATION_IP="${STATION_IP:?}" start
```

Do not remove credential files, flash firmware, disable native Alice services, or publish private config/logs during rollback. Enable autostart only after the hardware/acoustic/soak gates pass. The fallback remains a different-voice solution; promote a native Codex bridge only after all entitlement/handoff/media gates in the [review](<SEAMLESS_VOICE_REVIEW.md>) pass.
