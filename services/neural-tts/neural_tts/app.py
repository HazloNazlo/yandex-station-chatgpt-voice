"""Authenticated streaming endpoint with fail-fast admission and finite work."""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
import logging
import math
import struct
import uuid
from collections.abc import AsyncGenerator, Callable
from contextlib import aclosing
from dataclasses import dataclass
from http import HTTPStatus

from aiohttp import web

from .config import Settings
from .decoder import FfmpegDecoder
from .errors import AudioLimitError, CleanupError, DecoderError, SpeechError
from .protocol import (
    AUDIO_HEADERS,
    BYTES_PER_SECOND,
    COMPLETION_PACKET,
    DEFAULT_VOICE,
    MAX_PACKET_BYTES,
    PCM_FRAME_BYTES,
    VOICES,
)
from .provider import EdgeProvider

LOG = logging.getLogger(__name__)
Provider = Callable[[str, str], AsyncGenerator[bytes, None]]
Decoder = Callable[[AsyncGenerator[bytes, None]], AsyncGenerator[bytes, None]]
XML_FIRST_PRINTABLE = 0x20


@dataclass
class StreamState:
    request_id: str
    response: web.StreamResponse | None = None
    audio_bytes: int = 0


def _unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate JSON key")
        result[key] = value
    return result


def _reject_constant(_value: str) -> object:
    raise ValueError("Non-finite JSON constant")


def error_response(error: SpeechError) -> web.Response:
    headers = {"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"}
    if error.status == HTTPStatus.UNAUTHORIZED:
        headers["WWW-Authenticate"] = 'Bearer realm="neural-tts"'
    if error.status == HTTPStatus.TOO_MANY_REQUESTS:
        headers["Retry-After"] = "1"
    response = web.json_response(
        {"error": {"code": error.code, "message": error.message}},
        status=error.status,
        headers=headers,
    )
    # Never drain a rejected, oversized, or untrusted body to reuse its connection.
    response.force_close()
    return response


class SpeechService:
    def __init__(self, settings: Settings, provider: Provider, decoder: Decoder) -> None:
        self.settings = settings
        self.provider = provider
        self.decoder = decoder
        self._token_digest = hashlib.sha256(settings.token.encode("ascii")).digest()
        self._active: set[asyncio.Task[object]] = set()
        self._closing = False
        self._unhealthy = False

    @property
    def active_requests(self) -> int:
        return len(self._active)

    def _authorized(self, request: web.Request) -> bool:
        headers = request.headers.getall("Authorization", [])
        header = headers[0] if len(headers) == 1 else ""
        scheme, separator, candidate = header.partition(" ")
        digest = hashlib.sha256(candidate.encode("utf-8", "surrogatepass")).digest()
        matches = hmac.compare_digest(digest, self._token_digest)
        return matches and len(headers) == 1 and bool(separator) and scheme.lower() == "bearer"

    def _admission_error(self, request: web.Request) -> SpeechError | None:
        if not self._authorized(request):
            return SpeechError(
                HTTPStatus.UNAUTHORIZED, "unauthorized", "Bearer authentication required."
            )
        if self._unhealthy:
            return CleanupError()
        if self._closing:
            return SpeechError(
                HTTPStatus.SERVICE_UNAVAILABLE, "shutting_down", "Service is shutting down."
            )
        if self.active_requests >= self.settings.max_concurrency:
            return SpeechError(
                HTTPStatus.TOO_MANY_REQUESTS, "busy", "Synthesis capacity is occupied."
            )
        return None

    async def speech(self, request: web.Request) -> web.StreamResponse:
        if error := self._admission_error(request):
            return error_response(error)
        # No await between checking and taking a slot: asyncio admission is atomic, with no queue.
        task = asyncio.current_task()
        if task is None:
            raise RuntimeError("Speech handler requires an asyncio task")
        self._active.add(task)
        loop = asyncio.get_running_loop()
        started = loop.time()
        state = StreamState(request_id=uuid.uuid4().hex)
        outcome = "internal_error"
        try:
            async with asyncio.timeout(self.settings.total_timeout):
                async with asyncio.timeout(self.settings.body_timeout):
                    text, voice = await self._input(request)
                response = await self._stream(request, text, voice, state)
                outcome = "ok"
                return response
        except TimeoutError:
            outcome = "deadline_exceeded"
            return self._failure(
                request,
                state.response,
                SpeechError(HTTPStatus.GATEWAY_TIMEOUT, outcome, "Synthesis deadline exceeded."),
            )
        except SpeechError as error:
            outcome = error.code
            if isinstance(error, CleanupError):
                self._unhealthy = True
            return self._failure(request, state.response, error)
        except asyncio.CancelledError:
            outcome = "cancelled"
            self._abort(request, state.response)
            raise
        except (ConnectionResetError, BrokenPipeError):
            outcome = "disconnected"
            self._abort(request, state.response)
            raise
        except Exception:
            outcome = "internal_error"
            return self._failure(
                request,
                state.response,
                SpeechError(HTTPStatus.INTERNAL_SERVER_ERROR, outcome, "Speech synthesis failed."),
            )
        finally:
            self._active.discard(task)
            # No request text, headers, token, peer IP, provider errors, or ffmpeg stderr in logs.
            LOG.info(
                "speech request=%s outcome=%s audio_bytes=%d elapsed_ms=%d",
                state.request_id,
                outcome,
                state.audio_bytes,
                round((loop.time() - started) * 1000),
            )

    async def _stream(
        self, request: web.Request, text: str, voice: str, state: StreamState
    ) -> web.StreamResponse:
        loop = asyncio.get_running_loop()
        next_deadline = loop.time() + self.settings.first_audio_timeout
        async with (
            aclosing(self.provider(text, voice)) as source,
            aclosing(self.decoder(source)) as pcm,
        ):
            while True:
                async with asyncio.timeout_at(next_deadline):
                    try:
                        chunk = await anext(pcm)
                    except StopAsyncIteration:
                        break
                if not isinstance(chunk, bytes) or len(chunk) % PCM_FRAME_BYTES:
                    raise DecoderError()
                if not chunk:
                    await asyncio.sleep(0)
                    continue
                state.audio_bytes += len(chunk)
                if state.audio_bytes > self.settings.max_audio_seconds * BYTES_PER_SECOND:
                    raise AudioLimitError()
                if state.response is None:
                    state.response = web.StreamResponse(
                        headers={**AUDIO_HEADERS, "X-Request-ID": state.request_id}
                    )
                    async with asyncio.timeout(self.settings.write_timeout):
                        await state.response.prepare(request)
                for offset in range(0, len(chunk), MAX_PACKET_BYTES):
                    payload = chunk[offset : offset + MAX_PACKET_BYTES]
                    packet = struct.pack("<I", len(payload)) + payload
                    async with asyncio.timeout(self.settings.write_timeout):
                        await state.response.write(packet)
                next_deadline = loop.time() + self.settings.idle_timeout
        if state.response is None:
            raise SpeechError(
                HTTPStatus.BAD_GATEWAY, "no_audio", "Provider produced no valid audio."
            )
        # Closing both iterators (including ffmpeg exit/reap) precedes the success marker.
        async with asyncio.timeout(self.settings.write_timeout):
            await state.response.write(COMPLETION_PACKET)
            await state.response.write_eof()
        return state.response

    async def _input(self, request: web.Request) -> tuple[str, str]:
        charset = request.charset.lower() if request.charset is not None else None
        if request.content_type != "application/json" or charset not in (None, "utf-8", "utf8"):
            raise SpeechError(
                HTTPStatus.UNSUPPORTED_MEDIA_TYPE,
                "unsupported_media_type",
                "Send application/json encoded as UTF-8.",
            )
        if request.headers.get("Content-Encoding", "identity").lower() != "identity":
            raise SpeechError(
                HTTPStatus.UNSUPPORTED_MEDIA_TYPE,
                "unsupported_media_type",
                "Compressed request bodies are not supported.",
            )
        maximum = self.settings.max_body_bytes
        if request.content_length is not None and request.content_length > maximum:
            raise SpeechError(
                HTTPStatus.REQUEST_ENTITY_TOO_LARGE, "body_too_large", "JSON body is too large."
            )
        body = bytearray()
        async for chunk in request.content.iter_chunked(min(maximum + 1, 4096)):
            if len(body) + len(chunk) > maximum:
                raise SpeechError(
                    HTTPStatus.REQUEST_ENTITY_TOO_LARGE, "body_too_large", "JSON body is too large."
                )
            body.extend(chunk)
        try:
            data = json.loads(
                body.decode("utf-8"),
                object_pairs_hook=_unique_object,
                parse_constant=_reject_constant,
            )
        except (ValueError, RecursionError):
            raise SpeechError(
                HTTPStatus.BAD_REQUEST, "invalid_request", "Send a valid JSON object."
            ) from None
        if not isinstance(data, dict) or set(data) - {"text", "voice"}:
            raise SpeechError(
                HTTPStatus.BAD_REQUEST,
                "invalid_request",
                "Only text and voice fields are supported.",
            )
        text = data.get("text")
        voice = data.get("voice", DEFAULT_VOICE)
        if not isinstance(text, str) or not text.strip():
            raise SpeechError(
                HTTPStatus.BAD_REQUEST, "invalid_request", "text must be a nonblank string."
            )
        if not isinstance(voice, str) or voice not in VOICES:
            raise SpeechError(HTTPStatus.BAD_REQUEST, "invalid_request", "voice is not allowed.")
        try:
            encoded = text.encode("utf-8")
        except UnicodeError:
            raise SpeechError(
                HTTPStatus.BAD_REQUEST, "invalid_request", "text must be valid Unicode."
            ) from None
        if len(text) > self.settings.max_text_chars or len(encoded) > self.settings.max_text_bytes:
            raise SpeechError(
                HTTPStatus.REQUEST_ENTITY_TOO_LARGE, "text_too_long", "text exceeds its limit."
            )
        if any(
            (ord(char) < XML_FIRST_PRINTABLE and char not in "\t\r\n")
            or ord(char) in (0xFFFE, 0xFFFF)
            for char in text
        ):
            raise SpeechError(
                HTTPStatus.BAD_REQUEST,
                "invalid_request",
                "text contains unsupported control characters.",
            )
        return text, voice

    @classmethod
    def _failure(
        cls, request: web.Request, response: web.StreamResponse | None, error: SpeechError
    ) -> web.StreamResponse:
        if response is not None and response.prepared:
            cls._abort(request, response)
            return response
        return error_response(error)

    @staticmethod
    def _abort(request: web.Request, response: web.StreamResponse | None) -> None:
        if response is not None:
            response.force_close()
        transport = request.transport
        if transport is not None:
            transport.abort()

    async def shutdown(self, _app: web.Application) -> None:
        self._closing = True
        handlers = tuple(self._active)
        for task in handlers:
            task.cancel()
        if handlers:
            await asyncio.gather(*handlers, return_exceptions=True)


SERVICE_KEY = web.AppKey("neural_tts_service", SpeechService)


def create_app(
    settings: Settings, *, provider: Provider | None = None, decoder: Decoder | None = None
) -> web.Application:
    upstream = EdgeProvider(
        connect_timeout=max(1, math.ceil(min(10, settings.first_audio_timeout))),
        receive_timeout=max(1, math.ceil(min(60, settings.idle_timeout))),
    )
    service = SpeechService(
        settings,
        upstream.stream if provider is None else provider,
        FfmpegDecoder(settings).stream if decoder is None else decoder,
    )
    app = web.Application(
        client_max_size=settings.max_body_bytes,
        handler_args={"handler_cancellation": True, "auto_decompress": False},
    )
    app[SERVICE_KEY] = service
    app.router.add_post("/v1/speech", service.speech)
    app.on_shutdown.append(service.shutdown)
    return app
