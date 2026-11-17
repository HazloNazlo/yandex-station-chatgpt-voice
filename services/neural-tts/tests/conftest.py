from __future__ import annotations

import asyncio
from collections.abc import AsyncGenerator, Awaitable, Callable
from dataclasses import replace

import pytest
from aiohttp.test_utils import TestClient, TestServer

from neural_tts.app import Decoder, Provider, create_app
from neural_tts.config import Settings

TEST_TOKEN = "test-only-not-a-credential-0000000000"
AUTH = {"Authorization": f"Bearer {TEST_TOKEN}"}
PCM = b"\x01\x00\xfe\xff"


class ProviderProbe:
    def __init__(
        self,
        chunks: tuple[bytes, ...] = (PCM,),
        *,
        before: asyncio.Event | None = None,
        after: asyncio.Event | None = None,
        error: Exception | None = None,
    ) -> None:
        self.chunks = chunks
        self.before = before
        self.after = after
        self.error = error
        self.started = asyncio.Event()
        self.closed = asyncio.Event()
        self.calls: list[tuple[str, str]] = []

    async def stream(self, text: str, voice: str) -> AsyncGenerator[bytes, None]:
        self.calls.append((text, voice))
        self.started.set()
        try:
            if self.before is not None:
                await self.before.wait()
            for chunk in self.chunks:
                yield chunk
            if self.after is not None:
                await self.after.wait()
            if self.error is not None:
                raise self.error
        finally:
            self.closed.set()


class DecoderProbe:
    def __init__(self) -> None:
        self.started = asyncio.Event()
        self.closed = asyncio.Event()

    async def stream(self, source: AsyncGenerator[bytes, None]) -> AsyncGenerator[bytes, None]:
        self.started.set()
        try:
            async for chunk in source:
                yield chunk
        finally:
            self.closed.set()


@pytest.fixture
async def client_factory() -> AsyncGenerator[Callable[..., Awaitable[TestClient]], None]:
    clients: list[TestClient] = []

    async def make(
        *,
        provider: Provider | None = None,
        decoder: Decoder | None = None,
        settings: Settings | None = None,
        **changes: object,
    ) -> TestClient:
        base = Settings(token=TEST_TOKEN) if settings is None else settings
        configured = replace(base, **changes) if changes else base
        app = create_app(
            configured,
            provider=ProviderProbe().stream if provider is None else provider,
            decoder=DecoderProbe().stream if decoder is None else decoder,
        )
        client = TestClient(TestServer(app))
        await client.start_server()
        clients.append(client)
        return client

    yield make
    for client in reversed(clients):
        await client.close()


@pytest.fixture(autouse=True)
def isolated_credentials(monkeypatch: pytest.MonkeyPatch) -> None:
    for variable in (
        "NEURAL_TTS_TOKEN",
        "AZURE_SPEECH_KEY",
        "AZURE_SPEECH_REGION",
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "FFREPORT",
    ):
        monkeypatch.delenv(variable, raising=False)
