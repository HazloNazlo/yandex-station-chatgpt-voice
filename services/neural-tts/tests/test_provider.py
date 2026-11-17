from __future__ import annotations

import asyncio

import pytest

from neural_tts.errors import ProviderError
from neural_tts.provider import EdgeProvider


async def test_edge_adapter_yields_only_audio_and_uses_socket_deadlines(monkeypatch) -> None:
    construction = []
    closed = asyncio.Event()

    class Communication:
        def __init__(self, *args, **kwargs):
            construction.append((args, kwargs))

        async def stream(self):
            try:
                yield {"type": "SentenceBoundary", "text": "private metadata"}
                yield {"type": "audio", "data": b"first-mp3-fragment"}
                yield {"type": "audio", "data": b"last-mp3-fragment"}
            finally:
                closed.set()

    monkeypatch.setattr("neural_tts.provider.edge_tts.Communicate", Communication)
    provider = EdgeProvider(connect_timeout=3, receive_timeout=7)
    received = [chunk async for chunk in provider.stream("plain <text>", "ru-RU-DmitryNeural")]
    assert received == [b"first-mp3-fragment", b"last-mp3-fragment"]
    assert construction == [
        (("plain <text>", "ru-RU-DmitryNeural"), {"connect_timeout": 3, "receive_timeout": 7})
    ]
    assert closed.is_set()


async def test_edge_adapter_aclose_closes_inner_stream(monkeypatch) -> None:
    closed = asyncio.Event()

    class Communication:
        def __init__(self, *_args, **_kwargs):
            pass

        async def stream(self):
            try:
                yield {"type": "audio", "data": b"some-mp3"}
                await asyncio.Event().wait()
            finally:
                closed.set()

    monkeypatch.setattr("neural_tts.provider.edge_tts.Communicate", Communication)
    stream = EdgeProvider().stream("hello", "ru-RU-SvetlanaNeural")
    assert await anext(stream) == b"some-mp3"
    await stream.aclose()
    assert closed.is_set()


async def test_edge_adapter_cancellation_is_propagated_and_closes_upstream(monkeypatch) -> None:
    started = asyncio.Event()
    closed = asyncio.Event()

    class Communication:
        def __init__(self, *_args, **_kwargs):
            pass

        async def stream(self):
            try:
                started.set()
                await asyncio.Event().wait()
                yield {"type": "audio", "data": b"unreachable"}
            finally:
                closed.set()

    monkeypatch.setattr("neural_tts.provider.edge_tts.Communicate", Communication)
    pending = asyncio.create_task(anext(EdgeProvider().stream("hello", "ru-RU-SvetlanaNeural")))
    await started.wait()
    pending.cancel()
    with pytest.raises(asyncio.CancelledError):
        await pending
    assert closed.is_set()


@pytest.mark.parametrize("data", [b"", "not-bytes", None])
async def test_edge_adapter_rejects_bad_audio(monkeypatch, data) -> None:
    class Communication:
        def __init__(self, *_args, **_kwargs):
            pass

        async def stream(self):
            yield {"type": "audio", "data": data}

    monkeypatch.setattr("neural_tts.provider.edge_tts.Communicate", Communication)
    with pytest.raises(ProviderError, match="Speech provider failed"):
        await anext(EdgeProvider().stream("hello", "ru-RU-SvetlanaNeural"))


async def test_upstream_exception_details_are_not_public(monkeypatch) -> None:
    def communication(*_args, **_kwargs):
        raise RuntimeError("private URL containing a provider secret")

    monkeypatch.setattr("neural_tts.provider.edge_tts.Communicate", communication)
    with pytest.raises(ProviderError) as failure:
        await anext(EdgeProvider().stream("hello", "ru-RU-SvetlanaNeural"))
    assert str(failure.value) == "Speech provider failed."
    assert failure.value.__suppress_context__ is True
