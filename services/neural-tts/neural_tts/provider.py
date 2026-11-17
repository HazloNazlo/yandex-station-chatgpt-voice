"""Unofficial Microsoft Edge provider, not an OpenAI/ChatGPT voice."""

from collections.abc import AsyncGenerator
from contextlib import aclosing
from dataclasses import dataclass

import edge_tts

from .errors import ProviderError


@dataclass(frozen=True)
class EdgeProvider:
    connect_timeout: int = 10
    receive_timeout: int = 10

    async def stream(self, text: str, voice: str) -> AsyncGenerator[bytes, None]:
        try:
            communication = edge_tts.Communicate(
                text,
                voice,
                connect_timeout=self.connect_timeout,
                receive_timeout=self.receive_timeout,
            )
            async with aclosing(communication.stream()) as chunks:
                async for chunk in chunks:
                    if chunk["type"] == "audio":
                        data = chunk["data"]
                        if not isinstance(data, bytes) or not data:
                            raise ProviderError()
                        yield data
        except ProviderError:
            raise
        except Exception:
            # URLs, text, and proxy/credential details can appear in upstream exceptions.
            raise ProviderError() from None
