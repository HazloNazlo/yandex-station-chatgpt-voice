from __future__ import annotations

import asyncio
import contextlib
import hmac
import logging
import struct

import aiohttp
import pytest
from aiohttp import web
from conftest import AUTH, PCM, TEST_TOKEN, DecoderProbe, ProviderProbe
from multidict import CIMultiDict

from neural_tts.app import SERVICE_KEY
from neural_tts.errors import DecoderError, ProviderError


@pytest.mark.parametrize(
    "authorization", [None, "", "Bearer wrong", f"Basic {TEST_TOKEN}", f"Bearer  {TEST_TOKEN}"]
)
async def test_authentication_rejects_before_provider_work(client_factory, authorization) -> None:
    provider = ProviderProbe()
    decoder = DecoderProbe()
    client = await client_factory(provider=provider.stream, decoder=decoder.stream)
    headers = {} if authorization is None else {"Authorization": authorization}
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=headers)
    assert response.status == 401
    assert response.headers["WWW-Authenticate"] == 'Bearer realm="neural-tts"'
    assert (await response.json())["error"]["code"] == "unauthorized"
    assert provider.calls == []
    assert not decoder.started.is_set()


async def test_duplicate_authorization_is_rejected(client_factory) -> None:
    provider = ProviderProbe()
    client = await client_factory(provider=provider.stream)
    headers = CIMultiDict([("Authorization", AUTH["Authorization"])] * 2)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=headers)
    assert response.status == 401
    assert provider.calls == []


async def test_token_comparison_uses_fixed_length_constant_time_digest(
    client_factory, monkeypatch
) -> None:
    original = hmac.compare_digest
    comparisons = []

    def compare(left, right):
        comparisons.append((len(left), len(right)))
        return original(left, right)

    monkeypatch.setattr(hmac, "compare_digest", compare)
    client = await client_factory()
    for token in ("bad", TEST_TOKEN):
        response = await client.post(
            "/v1/speech", json={"text": "hello"}, headers={"Authorization": f"bearer {token}"}
        )
        assert response.status == (200 if token == TEST_TOKEN else 401)
        await response.read()
    assert comparisons == [(32, 32), (32, 32)]


@pytest.mark.parametrize(
    "payload",
    [
        {},
        {"text": ""},
        {"text": " \n\t"},
        {"text": None},
        {"text": 42},
        {"text": "hello", "voice": None},
        {"text": "hello", "voice": "Vale"},
        {"text": "hello", "voice": "en-US-AriaNeural"},
        {"text": "hello", "extra": "not supported"},
        {"text": "\ud800"},
        {"text": "bad\u0000text"},
        {"text": "bad\ufffetext"},
        ["hello"],
    ],
)
async def test_json_voice_and_unicode_bounds(client_factory, payload) -> None:
    provider = ProviderProbe()
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json=payload, headers=AUTH)
    assert response.status == 400
    assert (await response.json())["error"]["code"] == "invalid_request"
    assert "X-Audio-Format" not in response.headers
    assert provider.calls == []


@pytest.mark.parametrize(
    "body", [b"not-json", b'{"text":"a","text":"b"}', b'{"text":NaN}', b"\xff"]
)
async def test_malformed_and_ambiguous_json(client_factory, body) -> None:
    client = await client_factory()
    response = await client.post(
        "/v1/speech", data=body, headers={**AUTH, "Content-Type": "application/json"}
    )
    assert response.status == 400
    assert (await response.json())["error"]["code"] == "invalid_request"


@pytest.mark.parametrize(
    "headers",
    [
        {"Content-Type": "text/plain"},
        {"Content-Encoding": "gzip"},
        {"Content-Type": "application/json; charset=latin1"},
    ],
)
async def test_media_type_and_compression_rejected(client_factory, headers) -> None:
    provider = ProviderProbe()
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers={**AUTH, **headers})
    assert response.status == 415
    assert provider.calls == []


@pytest.mark.parametrize("voice", ["ru-RU-SvetlanaNeural", "ru-RU-DmitryNeural"])
async def test_explicit_allowlisted_voice_is_forwarded(client_factory, voice) -> None:
    provider = ProviderProbe()
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json={"text": "hello", "voice": voice}, headers=AUTH)
    assert response.status == 200
    await response.read()
    assert provider.calls == [("hello", voice)]


async def test_character_and_utf8_byte_limits_are_independent(client_factory) -> None:
    provider = ProviderProbe()
    client = await client_factory(provider=provider.stream, max_text_chars=4, max_text_bytes=6)
    for text, status in (("abcd", 200), ("abcde", 413), ("я" * 3, 200), ("я" * 4, 413)):
        response = await client.post("/v1/speech", json={"text": text}, headers=AUTH)
        assert response.status == status
        await response.read()
    assert provider.calls == [("abcd", "ru-RU-SvetlanaNeural"), ("яяя", "ru-RU-SvetlanaNeural")]


async def test_declared_and_chunked_body_limits(client_factory) -> None:
    provider = ProviderProbe()
    client = await client_factory(provider=provider.stream, max_body_bytes=12)
    headers = {**AUTH, "Content-Type": "application/json"}
    accepted = await client.post("/v1/speech", data=b'{"text":"a"}', headers=headers)
    assert accepted.status == 200
    await accepted.read()
    declared = await client.post("/v1/speech", data=b'{"text":"aa"}', headers=headers)
    assert declared.status == 413

    async def chunks():
        yield b'{"text":"'
        yield b'aa"}'

    chunked = await client.post("/v1/speech", data=chunks(), headers=headers)
    assert chunked.status == 413
    assert provider.calls == [("a", "ru-RU-SvetlanaNeural")]


async def test_success_headers_and_exact_pcm_envelope(client_factory) -> None:
    provider = ProviderProbe(chunks=(b"", PCM, b"\xff\x7f\x00\x80"))
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert response.status == 200
    assert response.headers["Content-Type"] == "application/vnd.station.pcm-stream; version=1"
    assert response.headers["X-Audio-Format"] == "pcm_s16le"
    assert response.headers["X-Audio-Sample-Rate"] == "48000"
    assert response.headers["X-Audio-Channels"] == "2"
    assert response.headers["X-Audio-Sample-Bits"] == "16"
    assert response.headers["X-Audio-Framing"] == "length-prefix-le32"
    assert response.headers["X-Audio-Max-Frame-Bytes"] == "3840"
    assert response.headers["Cache-Control"] == "no-store"
    assert "Content-Encoding" not in response.headers
    assert await response.read() == (
        b"\x04\x00\x00\x00\x01\x00\xfe\xff\x04\x00\x00\x00\xff\x7f\x00\x80\x00\x00\x00\x00"
    )
    assert provider.closed.is_set()


async def test_large_decoder_chunks_are_split_to_twenty_ms_packets(client_factory) -> None:
    provider = ProviderProbe(chunks=(PCM * 2000,))
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    body = await response.read()
    assert response.status == 200
    assert body[:4] == b"\x00\x0f\x00\x00"
    assert body[3844:3848] == b"\x00\x0f\x00\x00"
    assert body[7688:7692] == b"\x40\x01\x00\x00"
    assert body[4:3844] + body[3848:7688] + body[7692:8012] == PCM * 2000
    assert body[8012:] == b"\x00\x00\x00\x00"


async def test_no_headers_until_first_valid_audio(client_factory) -> None:
    gate = asyncio.Event()
    provider = ProviderProbe(before=gate)
    client = await client_factory(provider=provider.stream)
    pending = asyncio.create_task(client.post("/v1/speech", json={"text": "hello"}, headers=AUTH))
    await asyncio.wait_for(provider.started.wait(), 1)
    await asyncio.sleep(0.01)
    assert not pending.done()
    gate.set()
    response = await pending
    assert response.status == 200
    assert await response.read() == b"\x04\x00\x00\x00" + PCM + b"\x00\x00\x00\x00"


async def test_first_packet_streams_before_provider_finishes(client_factory) -> None:
    gate = asyncio.Event()
    provider = ProviderProbe(after=gate)
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert await response.content.readexactly(8) == b"\x04\x00\x00\x00" + PCM
    assert not provider.closed.is_set()
    gate.set()
    assert await response.read() == b"\x00\x00\x00\x00"
    assert provider.closed.is_set()


@pytest.mark.parametrize("chunks,code", [((), "no_audio"), ((b"\x01\x00\x00",), "decoder_failure")])
async def test_empty_or_invalid_audio_never_gets_success_headers(
    client_factory, chunks, code
) -> None:
    client = await client_factory(provider=ProviderProbe(chunks=chunks).stream)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert response.status == 502
    assert "X-Audio-Format" not in response.headers
    assert (await response.json())["error"]["code"] == code


async def test_provider_failure_before_audio_is_json_502(client_factory) -> None:
    provider = ProviderProbe(chunks=(), error=ProviderError())
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert response.status == 502
    assert (await response.json())["error"]["code"] == "provider_failure"
    assert provider.closed.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0


async def test_provider_failure_after_audio_has_no_completion_packet(client_factory) -> None:
    gate = asyncio.Event()
    provider = ProviderProbe(after=gate, error=ProviderError())
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert await response.content.readexactly(8) == b"\x04\x00\x00\x00" + PCM
    gate.set()
    with pytest.raises(aiohttp.ClientPayloadError):
        await response.content.readexactly(4)
    assert provider.closed.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0


async def test_busy_is_immediate_and_does_not_queue(client_factory) -> None:
    gate = asyncio.Event()
    provider = ProviderProbe(before=gate)
    client = await client_factory(provider=provider.stream, max_concurrency=1)
    first = asyncio.create_task(client.post("/v1/speech", json={"text": "first"}, headers=AUTH))
    await asyncio.wait_for(provider.started.wait(), 1)
    busy = await client.post("/v1/speech", json={"text": "queued?"}, headers=AUTH)
    assert busy.status == 429
    assert busy.headers["Retry-After"] == "1"
    assert provider.calls == [("first", "ru-RU-SvetlanaNeural")]
    gate.set()
    assert (await first).status == 200
    await provider.closed.wait()
    next_response = await client.post("/v1/speech", json={"text": "next"}, headers=AUTH)
    assert next_response.status == 200
    await next_response.read()
    assert provider.calls == [("first", "ru-RU-SvetlanaNeural"), ("next", "ru-RU-SvetlanaNeural")]


async def test_disconnect_before_audio_cancels_both_iterators(client_factory) -> None:
    provider = ProviderProbe(before=asyncio.Event())
    decoder = DecoderProbe()
    client = await client_factory(provider=provider.stream, decoder=decoder.stream)
    pending = asyncio.create_task(client.post("/v1/speech", json={"text": "hello"}, headers=AUTH))
    await asyncio.wait_for(provider.started.wait(), 1)
    pending.cancel()
    with pytest.raises(asyncio.CancelledError):
        await pending
    await asyncio.wait_for(provider.closed.wait(), 1)
    assert decoder.closed.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0


async def test_disconnect_after_audio_cancels_both_iterators(client_factory) -> None:
    provider = ProviderProbe(after=asyncio.Event())
    decoder = DecoderProbe()
    client = await client_factory(provider=provider.stream, decoder=decoder.stream)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    await response.content.readexactly(8)
    response.close()
    await asyncio.wait_for(provider.closed.wait(), 1)
    assert decoder.closed.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0


async def test_first_audio_deadline_returns_504_and_releases_slot(client_factory) -> None:
    provider = ProviderProbe(before=asyncio.Event())
    decoder = DecoderProbe()
    client = await client_factory(
        provider=provider.stream, decoder=decoder.stream, first_audio_timeout=0.04
    )
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert response.status == 504
    assert "X-Audio-Format" not in response.headers
    assert (await response.json())["error"]["code"] == "deadline_exceeded"
    assert provider.closed.is_set()
    assert decoder.closed.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0


async def test_idle_deadline_aborts_without_success_marker(client_factory) -> None:
    provider = ProviderProbe(after=asyncio.Event())
    client = await client_factory(provider=provider.stream, idle_timeout=0.04)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    await response.content.readexactly(8)
    with pytest.raises(aiohttp.ClientPayloadError):
        await response.content.readexactly(4)
    assert provider.closed.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0


async def test_total_deadline_cancels_periodic_stream(client_factory, caplog) -> None:
    closed = asyncio.Event()

    async def periodic(_text, _voice):
        try:
            while True:
                yield PCM
                await asyncio.sleep(0.005)
        finally:
            closed.set()

    caplog.set_level(logging.INFO)
    client = await client_factory(provider=periodic, idle_timeout=1, total_timeout=0.04)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    await response.content.readexactly(8)
    with pytest.raises(aiohttp.ClientPayloadError):
        while True:
            length = struct.unpack("<I", await response.content.readexactly(4))[0]
            assert length == 4  # A success marker would incorrectly satisfy a truncated turn.
            await response.content.readexactly(length)
    assert closed.is_set()
    assert "outcome=deadline_exceeded" in caplog.text


async def test_body_read_deadline_is_finite(client_factory) -> None:
    body_closed = asyncio.Event()

    async def slow_body():
        try:
            yield b'{"text":'
            await asyncio.Event().wait()
        finally:
            body_closed.set()

    provider = ProviderProbe()
    client = await client_factory(provider=provider.stream, body_timeout=0.04)
    response = await client.post(
        "/v1/speech", data=slow_body(), headers={**AUTH, "Content-Type": "application/json"}
    )
    assert response.status == 504
    assert (await response.json())["error"]["code"] == "deadline_exceeded"
    await asyncio.wait_for(body_closed.wait(), 1)
    assert provider.calls == []


async def test_pcm_output_budget_checked_before_headers(client_factory) -> None:
    client = await client_factory(max_audio_seconds=0.00001)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert response.status == 502
    assert "X-Audio-Format" not in response.headers
    assert (await response.json())["error"]["code"] == "audio_limit_exceeded"


async def test_safe_logs_and_errors_never_include_request_or_exception_secrets(
    client_factory, caplog
) -> None:
    private_text = "private transcript must not be logged"
    upstream_detail = f"upstream URL key={TEST_TOKEN} text={private_text}"
    provider = ProviderProbe(chunks=(), error=RuntimeError(upstream_detail))
    caplog.set_level(logging.INFO)
    client = await client_factory(provider=provider.stream)
    response = await client.post("/v1/speech", json={"text": private_text}, headers=AUTH)
    assert response.status == 500
    public = await response.text()
    assert "outcome=internal_error" in caplog.text
    for secret in (private_text, TEST_TOKEN, upstream_detail):
        assert secret not in public
        assert secret not in caplog.text


async def test_shutdown_cancels_active_request_and_releases_resources(client_factory) -> None:
    provider = ProviderProbe(before=asyncio.Event())
    decoder = DecoderProbe()
    client = await client_factory(provider=provider.stream, decoder=decoder.stream)
    pending = asyncio.create_task(client.post("/v1/speech", json={"text": "hello"}, headers=AUTH))
    await asyncio.wait_for(provider.started.wait(), 1)
    await client.server.app[SERVICE_KEY].shutdown(client.server.app)
    with contextlib.suppress(aiohttp.ServerDisconnectedError, asyncio.CancelledError):
        await pending
    assert provider.closed.is_set()
    assert decoder.closed.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0
    closing = await client.post("/v1/speech", json={"text": "next"}, headers=AUTH)
    assert closing.status == 503


async def test_utf8_charset_case_and_http_optional_whitespace(client_factory) -> None:
    client = await client_factory()
    response = await client.post(
        "/v1/speech",
        data=b'{"text":"hello"}',
        headers={
            "Authorization": f"Bearer {TEST_TOKEN} \t",
            "Content-Type": "application/json; charset=UTF-8",
        },
    )
    assert response.status == 200
    assert await response.read() == b"\x04\x00\x00\x00" + PCM + b"\x00\x00\x00\x00"


async def test_write_deadline_cancels_iterators_without_completion(
    client_factory, monkeypatch
) -> None:
    original = web.StreamResponse.write

    async def stalled(response, data):
        if "X-Audio-Format" in response.headers:
            await asyncio.Event().wait()
        await original(response, data)

    monkeypatch.setattr(web.StreamResponse, "write", stalled)
    provider = ProviderProbe()
    decoder = DecoderProbe()
    client = await client_factory(
        provider=provider.stream, decoder=decoder.stream, write_timeout=0.03
    )
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert response.status == 200
    with pytest.raises(aiohttp.ClientPayloadError):
        await response.read()
    assert provider.closed.is_set()
    assert decoder.closed.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0


async def test_prepare_deadline_is_json_without_success_headers(
    client_factory, monkeypatch
) -> None:
    original = web.StreamResponse.prepare

    async def stalled(response, request):
        if "X-Audio-Format" in response.headers:
            await asyncio.Event().wait()
        return await original(response, request)

    monkeypatch.setattr(web.StreamResponse, "prepare", stalled)
    provider = ProviderProbe()
    decoder = DecoderProbe()
    client = await client_factory(
        provider=provider.stream, decoder=decoder.stream, write_timeout=0.03
    )
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert response.status == 504
    assert "X-Audio-Format" not in response.headers
    assert (await response.json())["error"]["code"] == "deadline_exceeded"
    assert provider.closed.is_set()
    assert decoder.closed.is_set()


async def test_output_budget_after_first_packet_has_no_success_marker(client_factory) -> None:
    gate = asyncio.Event()
    closed = asyncio.Event()

    async def provider(_text, _voice):
        try:
            yield PCM
            await gate.wait()
            yield PCM
        finally:
            closed.set()

    client = await client_factory(provider=provider, max_audio_seconds=0.000025)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert await response.content.readexactly(8) == b"\x04\x00\x00\x00" + PCM
    gate.set()
    with pytest.raises(aiohttp.ClientPayloadError):
        await response.content.readexactly(4)
    assert closed.is_set()


async def test_late_decoder_failure_is_not_a_successful_aligned_eof(client_factory) -> None:
    gate = asyncio.Event()
    closed = asyncio.Event()

    async def decoder(source):
        try:
            async for chunk in source:
                yield chunk
            await gate.wait()
            raise DecoderError()
        finally:
            closed.set()

    client = await client_factory(decoder=decoder)
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert await response.content.readexactly(8) == b"\x04\x00\x00\x00" + PCM
    gate.set()
    with pytest.raises(aiohttp.ClientPayloadError):
        await response.content.readexactly(4)
    assert closed.is_set()
