"""Concurrent bounded pipe decoding with cancellation-safe process ownership."""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
from collections.abc import AsyncGenerator
from dataclasses import dataclass
from typing import TypeVar

from .config import Settings
from .errors import AudioLimitError, CleanupError, DecoderError, ProviderError, SpeechError
from .protocol import CHANNELS, PCM_FRAME_BYTES, SAMPLE_RATE

LOG = logging.getLogger(__name__)
PIPE_CHUNK_BYTES = 16384
STDERR_CHUNK_BYTES = 4096
STDERR_TAIL_BYTES = 8192
T = TypeVar("T")
FFMPEG_ENV_KEYS = frozenset(
    {
        "path",
        "systemroot",
        "windir",
        "ld_library_path",
        "dyld_library_path",
        "dyld_fallback_library_path",
        "tmp",
        "temp",
        "tmpdir",
    }
)
FFMPEG_ARGS = (
    "-hide_banner",
    "-loglevel",
    "error",
    "-nostats",
    "-nostdin",
    "-protocol_whitelist",
    "pipe",
    "-threads",
    "1",
    "-f",
    "mp3",
    # Bound initial analysis for the known format without discarding buffered audio.
    "-probesize",
    "512",
    "-i",
    "pipe:0",
    "-map",
    "0:a:0",
    "-vn",
    "-sn",
    "-dn",
    "-filter_threads",
    "1",
    "-threads",
    "1",
    "-c:a",
    "pcm_s16le",
    "-ar",
    str(SAMPLE_RATE),
    "-ac",
    str(CHANNELS),
    "-f",
    "s16le",
    "pipe:1",
)


async def finish_owned_task(task: asyncio.Task[T]) -> T:
    """Always join this owned task, even if a second disconnect/shutdown cancels us."""
    cancelled = False
    while not task.done():
        try:
            await asyncio.shield(task)
        except asyncio.CancelledError:
            cancelled = True
    result = task.result()
    if cancelled:
        raise asyncio.CancelledError
    return result


@dataclass
class StderrTail:
    """Drain arbitrary diagnostics with fixed retention and without logging their contents."""

    data: bytearray

    async def drain(self, stream: asyncio.StreamReader) -> None:
        while chunk := await stream.read(STDERR_CHUNK_BYTES):
            self.data.extend(chunk)
            if len(self.data) > STDERR_TAIL_BYTES:
                del self.data[:-STDERR_TAIL_BYTES]


class FfmpegDecoder:
    def __init__(self, settings: Settings) -> None:
        self.settings = settings

    async def _spawn(self) -> asyncio.subprocess.Process:
        spawn = asyncio.create_task(
            asyncio.create_subprocess_exec(
                self.settings.ffmpeg_binary,
                *FFMPEG_ARGS,
                stdin=asyncio.subprocess.PIPE,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                limit=PIPE_CHUNK_BYTES,
                env={
                    key: value
                    for key, value in os.environ.items()
                    if key.lower() in FFMPEG_ENV_KEYS
                },
            ),
            name="neural-tts-spawn",
        )
        cancelled = False
        # Shield creation so a child born during cancellation is still acquired and reaped.
        while not spawn.done():
            try:
                await asyncio.shield(spawn)
            except asyncio.CancelledError:
                cancelled = True
            except Exception:
                break
        try:
            process = spawn.result()
        except Exception:
            if cancelled:
                raise asyncio.CancelledError from None
            raise DecoderError() from None
        if cancelled:
            await finish_owned_task(
                asyncio.create_task(self._cleanup(process, None, None), name="neural-tts-reap")
            )
            raise asyncio.CancelledError
        return process

    async def stream(self, source: AsyncGenerator[bytes, None]) -> AsyncGenerator[bytes, None]:
        process = await self._spawn()
        feeder: asyncio.Task[None] | None = None
        stderr: asyncio.Task[None] | None = None
        try:
            if process.stdin is None or process.stdout is None or process.stderr is None:
                raise DecoderError()
            feeder = asyncio.create_task(self._feed(process, source), name="neural-tts-feed")
            tail = StderrTail(bytearray())
            stderr = asyncio.create_task(tail.drain(process.stderr), name="neural-tts-stderr")
            carry = b""
            while chunk := await self._read_stdout(process.stdout, feeder, stderr):
                if process.returncode not in (None, 0):
                    raise DecoderError()
                data = carry + chunk
                complete = len(data) - len(data) % PCM_FRAME_BYTES
                carry = data[complete:]
                if complete:
                    yield data[:complete]
            if feeder.done():
                feeder.result()
            if await process.wait() != 0:
                raise DecoderError()
            await feeder
            await stderr
            if carry:
                raise DecoderError()
        finally:
            cleanup = asyncio.create_task(
                self._cleanup(process, feeder, stderr), name="neural-tts-reap"
            )
            await finish_owned_task(cleanup)

    async def _feed(
        self, process: asyncio.subprocess.Process, source: AsyncGenerator[bytes, None]
    ) -> None:
        writer = process.stdin
        if writer is None:
            raise DecoderError()
        total = 0
        try:
            async with contextlib.aclosing(source):
                async for chunk in source:
                    if not isinstance(chunk, bytes):
                        raise ProviderError()
                    total += len(chunk)
                    if total > self.settings.max_encoded_bytes:
                        raise AudioLimitError()
                    # Chunk large upstream messages too; asyncio's stdin high-water mark is bounded.
                    for offset in range(0, len(chunk), PIPE_CHUNK_BYTES):
                        writer.write(chunk[offset : offset + PIPE_CHUNK_BYTES])
                        await writer.drain()
        except (BrokenPipeError, ConnectionResetError):
            raise DecoderError() from None
        except SpeechError:
            raise
        except Exception:
            raise ProviderError() from None
        finally:
            with contextlib.suppress(OSError):
                writer.close()

    @staticmethod
    async def _read_stdout(
        stream: asyncio.StreamReader, feeder: asyncio.Task[None], stderr: asyncio.Task[None]
    ) -> bytes:
        read = asyncio.create_task(stream.read(PIPE_CHUNK_BYTES), name="neural-tts-stdout")
        watched: set[asyncio.Task[object]] = {read, feeder, stderr}
        try:
            while True:
                await asyncio.wait(watched, return_when=asyncio.FIRST_COMPLETED)
                # Never send bytes once an already-known producer failure is observable.
                if feeder.done():
                    feeder.result()
                    watched.discard(feeder)
                if stderr.done():
                    try:
                        stderr.result()
                    except Exception:
                        raise DecoderError() from None
                    watched.discard(stderr)
                if read.done():
                    try:
                        return read.result()
                    except OSError:
                        raise DecoderError() from None
        finally:
            if not read.done():
                read.cancel()
            await asyncio.gather(read, return_exceptions=True)

    async def _cleanup(
        self,
        process: asyncio.subprocess.Process,
        feeder: asyncio.Task[None] | None,
        stderr: asyncio.Task[None] | None,
    ) -> None:
        if feeder is not None:
            feeder.cancel()
        if process.stdin is not None:
            with contextlib.suppress(OSError):
                process.stdin.close()
        if stderr is None and process.stderr is not None:
            # Spawn cancellation can reach cleanup before the normal drainer was started.
            tail = StderrTail(bytearray())
            stderr = asyncio.create_task(tail.drain(process.stderr), name="neural-tts-stderr")
        # asyncio Process.wait can otherwise deadlock on a full, paused stdout pipe.
        stdout = (
            asyncio.create_task(self._discard(process.stdout), name="neural-tts-discard")
            if process.stdout is not None
            else None
        )
        try:
            if process.returncode is None:
                with contextlib.suppress(ProcessLookupError):
                    process.terminate()
                try:
                    async with asyncio.timeout(self.settings.terminate_timeout):
                        await process.wait()
                except TimeoutError:
                    if process.returncode is None:
                        with contextlib.suppress(ProcessLookupError):
                            process.kill()
                    async with asyncio.timeout(self.settings.kill_timeout):
                        await process.wait()
            else:
                async with asyncio.timeout(self.settings.kill_timeout):
                    await process.wait()
        except TimeoutError:
            # An unkillable OS process is not recoverable in Python. Do not hide a cleanup failure.
            LOG.error("ffmpeg_cleanup_failed code=reap_timeout")
            raise CleanupError() from None
        finally:
            tasks = [task for task in (feeder, stderr, stdout) if task is not None]
            for task in tasks:
                task.cancel()
            if tasks:
                try:
                    async with asyncio.timeout(self.settings.kill_timeout):
                        await asyncio.gather(*tasks, return_exceptions=True)
                except TimeoutError:
                    LOG.error("ffmpeg_cleanup_failed code=pipe_join_timeout")
                    raise CleanupError() from None

    @staticmethod
    async def _discard(stream: asyncio.StreamReader) -> None:
        while await stream.read(PIPE_CHUNK_BYTES):
            pass
