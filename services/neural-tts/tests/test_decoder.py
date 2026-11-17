from __future__ import annotations

import asyncio
import math
import os
import shutil
import struct
from collections.abc import AsyncGenerator
from contextlib import aclosing

import pytest
from conftest import PCM, TEST_TOKEN

from neural_tts.config import Settings
from neural_tts.decoder import (
    FFMPEG_ENV_KEYS,
    PIPE_CHUNK_BYTES,
    STDERR_TAIL_BYTES,
    FfmpegDecoder,
    StderrTail,
)
from neural_tts.errors import AudioLimitError, DecoderError, ProviderError


class FakeReader:
    def __init__(self, chunks: tuple[bytes, ...] = (), *, on_read=None) -> None:
        self.queue: asyncio.Queue[bytes] = asyncio.Queue()
        self.pending = b""
        self.eof = False
        self.bytes_read = 0
        self.on_read = on_read
        for chunk in chunks:
            self.push(chunk)

    def push(self, chunk: bytes) -> None:
        self.queue.put_nowait(chunk)

    async def read(self, maximum: int) -> bytes:
        if not self.pending and not self.eof:
            self.pending = await self.queue.get()
            self.eof = not self.pending
        chunk, self.pending = self.pending[:maximum], self.pending[maximum:]
        self.bytes_read += len(chunk)
        if self.on_read is not None:
            self.on_read(self.bytes_read)
        return chunk


class FakeWriter:
    def __init__(self, process, *, drain_gate: asyncio.Event | None = None, broken=False) -> None:
        self.process = process
        self.drain_gate = drain_gate
        self.broken = broken
        self.chunks: list[bytes] = []
        self.closed = False
        self.written = asyncio.Event()

    def write(self, chunk: bytes) -> None:
        self.chunks.append(chunk)
        self.written.set()

    async def drain(self) -> None:
        if self.broken:
            raise BrokenPipeError("ffmpeg stopped")
        if self.drain_gate is not None:
            await self.drain_gate.wait()

    def close(self) -> None:
        if not self.closed:
            self.closed = True
            if self.process.auto_exit:
                self.process.exit(self.process.exit_code)


class FakeProcess:
    # Independent pipe, exit and backpressure controls model the subprocess boundary.
    def __init__(  # noqa: PLR0913
        self,
        stdout: tuple[bytes, ...] = (),
        stderr: tuple[bytes, ...] = (),
        *,
        auto_exit=True,
        exit_code=0,
        terminate_exits=True,
        drain_gate=None,
        broken=False,
        stdout_read=None,
        stderr_read=None,
    ) -> None:
        self.auto_exit = auto_exit
        self.exit_code = exit_code
        self.terminate_exits = terminate_exits
        self.returncode = None
        self.stdout = FakeReader(stdout, on_read=stdout_read)
        self.stderr = FakeReader(stderr, on_read=stderr_read)
        self.stdin = FakeWriter(self, drain_gate=drain_gate, broken=broken)
        self.exited = asyncio.Event()
        self.terminated = asyncio.Event()
        self.reaped = asyncio.Event()
        self.terminate_calls = 0
        self.kill_calls = 0
        self.wait_calls = 0

    def exit(self, code: int) -> None:
        if self.returncode is None:
            self.returncode = code
            self.stdout.push(b"")
            self.stderr.push(b"")
            self.exited.set()

    def terminate(self) -> None:
        self.terminate_calls += 1
        self.terminated.set()
        if self.terminate_exits:
            self.exit(-15)

    def kill(self) -> None:
        self.kill_calls += 1
        self.exit(-9)

    async def wait(self) -> int:
        self.wait_calls += 1
        await self.exited.wait()
        self.reaped.set()
        return self.returncode


def install_process(monkeypatch, process: FakeProcess):
    calls = []

    async def spawn(*args, **kwargs):
        calls.append((args, kwargs))
        return process

    monkeypatch.setattr(asyncio, "create_subprocess_exec", spawn)
    return calls


async def source_chunks(*chunks: bytes) -> AsyncGenerator[bytes, None]:
    for chunk in chunks:
        yield chunk


def settings(**changes):
    return Settings(token=TEST_TOKEN, terminate_timeout=0.01, kill_timeout=0.1, **changes)


async def test_decoder_reassembles_whole_stereo_frames_and_reaps_success(monkeypatch) -> None:
    process = FakeProcess(stdout=(b"\x01", b"\x00\xfe", b"\xff\xff", b"\x7f\x00\x80"))
    calls = install_process(monkeypatch, process)
    decoder = FfmpegDecoder(settings())
    pcm = [chunk async for chunk in decoder.stream(source_chunks(b"mp3-one", b"mp3-two"))]
    assert pcm == [b"\x01\x00\xfe\xff", b"\xff\x7f\x00\x80"]
    assert process.stdin.chunks == [b"mp3-one", b"mp3-two"]
    assert process.stdin.closed
    assert process.reaped.is_set()
    assert process.terminate_calls == 0
    assert process.kill_calls == 0
    arguments, options = calls[0]
    assert arguments[arguments.index("-i") + 1] == "pipe:0"
    assert arguments[arguments.index("-c:a") + 1] == "pcm_s16le"
    assert arguments[arguments.index("-ar") + 1] == "48000"
    assert arguments[arguments.index("-ac") + 1] == "2"
    assert arguments[-3:] == ("-f", "s16le", "pipe:1")
    assert arguments[arguments.index("-protocol_whitelist") + 1] == "pipe"
    assert options["stdin"] == asyncio.subprocess.PIPE
    assert options["stdout"] == asyncio.subprocess.PIPE
    assert options["stderr"] == asyncio.subprocess.PIPE
    assert options["limit"] == PIPE_CHUNK_BYTES


async def test_stdout_and_stdin_progress_concurrently(monkeypatch) -> None:
    output_read = asyncio.Event()
    process = FakeProcess(
        stdout=(PCM,), drain_gate=output_read, stdout_read=lambda _total: output_read.set()
    )
    install_process(monkeypatch, process)
    decoder = FfmpegDecoder(settings())
    async with asyncio.timeout(1):
        pcm = [chunk async for chunk in decoder.stream(source_chunks(b"mp3"))]
    assert pcm == [PCM]
    assert process.reaped.is_set()


async def test_large_upstream_chunks_use_bounded_stdin_writes(monkeypatch) -> None:
    process = FakeProcess(stdout=(PCM,))
    install_process(monkeypatch, process)
    encoded = b"m" * (3 * PIPE_CHUNK_BYTES + 4)
    decoder = FfmpegDecoder(settings())
    pcm = [chunk async for chunk in decoder.stream(source_chunks(encoded))]
    assert pcm == [PCM]
    assert [len(chunk) for chunk in process.stdin.chunks] == [16384, 16384, 16384, 4]
    assert b"".join(process.stdin.chunks) == encoded


async def test_stderr_tail_has_a_fixed_memory_bound() -> None:
    reader = FakeReader((b"diagnostic" * 100000, b"last diagnostic", b""))
    tail = StderrTail(bytearray())
    await tail.drain(reader)
    assert reader.bytes_read == 1000015
    assert len(tail.data) == STDERR_TAIL_BYTES
    assert tail.data[-15:] == b"last diagnostic"


async def test_stderr_is_drained_while_feeder_and_stdout_are_active(monkeypatch) -> None:
    diagnostics_drained = asyncio.Event()
    count = 1024 * 1024

    def read(total):
        if total >= count:
            diagnostics_drained.set()

    process = FakeProcess(
        stdout=(PCM,), stderr=(b"e" * count,), drain_gate=diagnostics_drained, stderr_read=read
    )
    install_process(monkeypatch, process)
    decoder = FfmpegDecoder(settings())
    async with asyncio.timeout(1):
        pcm = [chunk async for chunk in decoder.stream(source_chunks(b"mp3"))]
    assert pcm == [PCM]
    assert process.stderr.bytes_read == count
    assert process.reaped.is_set()


async def test_decoded_audio_arrives_before_upstream_completion(monkeypatch) -> None:
    gate = asyncio.Event()
    source_paused = asyncio.Event()
    source_closed = asyncio.Event()
    process = FakeProcess(stdout=(PCM,))
    install_process(monkeypatch, process)

    async def source():
        try:
            yield b"first-mp3"
            source_paused.set()
            await gate.wait()
            yield b"last-mp3"
        finally:
            source_closed.set()

    decoder = FfmpegDecoder(settings())
    async with aclosing(decoder.stream(source())) as pcm:
        assert await anext(pcm) == PCM
        await asyncio.wait_for(source_paused.wait(), 1)
        assert not source_closed.is_set()
        gate.set()
        assert [chunk async for chunk in pcm] == []
    assert process.stdin.chunks == [b"first-mp3", b"last-mp3"]
    assert source_closed.is_set()
    assert process.reaped.is_set()


@pytest.mark.parametrize("output", [(b"\x01",), (PCM, b"\x01\x02\x03")])
async def test_incomplete_pcm_frame_is_an_error(monkeypatch, output) -> None:
    process = FakeProcess(stdout=output)
    install_process(monkeypatch, process)
    decoder = FfmpegDecoder(settings())
    with pytest.raises(DecoderError):
        _ = [chunk async for chunk in decoder.stream(source_chunks(b"mp3"))]
    assert process.reaped.is_set()


async def test_nonzero_ffmpeg_exit_is_an_error_and_is_reaped(monkeypatch) -> None:
    process = FakeProcess(stdout=(PCM,), stderr=(b"private ffmpeg details",), exit_code=7)
    install_process(monkeypatch, process)
    decoder = FfmpegDecoder(settings())
    with pytest.raises(DecoderError):
        _ = [chunk async for chunk in decoder.stream(source_chunks(b"mp3"))]
    assert process.returncode == 7
    assert process.reaped.is_set()


async def test_provider_failure_is_not_hidden_by_ffmpeg_exit(monkeypatch) -> None:
    process = FakeProcess()
    install_process(monkeypatch, process)
    source_closed = asyncio.Event()

    async def source():
        try:
            raise ProviderError()
            yield b"unreachable"
        finally:
            source_closed.set()

    decoder = FfmpegDecoder(settings())
    with pytest.raises(ProviderError):
        await anext(decoder.stream(source()))
    assert source_closed.is_set()
    assert process.reaped.is_set()


async def test_encoded_input_budget_cancels_and_reaps(monkeypatch) -> None:
    process = FakeProcess()
    install_process(monkeypatch, process)
    decoder = FfmpegDecoder(settings(max_encoded_bytes=4))
    with pytest.raises(AudioLimitError):
        await anext(decoder.stream(source_chunks(b"too much MP3")))
    assert process.stdin.chunks == []
    assert process.reaped.is_set()


async def test_broken_stdin_is_a_decoder_failure(monkeypatch) -> None:
    process = FakeProcess(broken=True)
    install_process(monkeypatch, process)
    decoder = FfmpegDecoder(settings())
    with pytest.raises(DecoderError):
        await anext(decoder.stream(source_chunks(b"mp3")))
    assert process.reaped.is_set()


async def test_missing_executable_is_a_safe_decoder_error(monkeypatch) -> None:
    async def spawn(*_args, **_kwargs):
        raise FileNotFoundError("private executable path")

    monkeypatch.setattr(asyncio, "create_subprocess_exec", spawn)
    decoder = FfmpegDecoder(settings())
    with pytest.raises(DecoderError) as failure:
        await anext(decoder.stream(source_chunks(b"mp3")))
    assert str(failure.value) == "Audio decoding failed."


async def cancellation_source(started: asyncio.Event, closed: asyncio.Event):
    try:
        yield b"mp3"
        started.set()
        await asyncio.Event().wait()
    finally:
        closed.set()


async def test_cancellation_terminates_kills_reaps_and_closes_provider(monkeypatch) -> None:
    process = FakeProcess(auto_exit=False, terminate_exits=False)
    install_process(monkeypatch, process)
    source_started = asyncio.Event()
    source_closed = asyncio.Event()
    decoder = FfmpegDecoder(settings())
    pending = asyncio.create_task(
        anext(decoder.stream(cancellation_source(source_started, source_closed)))
    )
    await asyncio.wait_for(source_started.wait(), 1)
    pending.cancel()
    with pytest.raises(asyncio.CancelledError):
        await pending
    assert source_closed.is_set()
    assert process.stdin.closed
    assert process.terminate_calls == 1
    assert process.kill_calls == 1
    assert process.reaped.is_set()
    assert not [task for task in asyncio.all_tasks() if task.get_name().startswith("neural-tts-")]


async def test_deadline_also_reaps_stubborn_ffmpeg(monkeypatch) -> None:
    process = FakeProcess(auto_exit=False, terminate_exits=False)
    install_process(monkeypatch, process)
    source_started = asyncio.Event()
    source_closed = asyncio.Event()
    decoder = FfmpegDecoder(settings())
    with pytest.raises(TimeoutError):
        async with asyncio.timeout(0.03):
            await anext(decoder.stream(cancellation_source(source_started, source_closed)))
    assert source_closed.is_set()
    assert process.terminate_calls == 1
    assert process.kill_calls == 1
    assert process.reaped.is_set()


async def test_second_cancellation_cannot_interrupt_process_reaping(monkeypatch) -> None:
    process = FakeProcess(auto_exit=False, terminate_exits=False)
    install_process(monkeypatch, process)
    source_started = asyncio.Event()
    source_closed = asyncio.Event()
    decoder = FfmpegDecoder(settings())
    pending = asyncio.create_task(
        anext(decoder.stream(cancellation_source(source_started, source_closed)))
    )
    await source_started.wait()
    pending.cancel()
    await process.terminated.wait()
    pending.cancel()
    with pytest.raises(asyncio.CancelledError):
        await pending
    assert process.kill_calls == 1
    assert process.reaped.is_set()
    assert source_closed.is_set()


async def test_cancel_during_spawn_still_acquires_and_reaps_process(monkeypatch) -> None:
    spawn_started = asyncio.Event()
    release_spawn = asyncio.Event()
    process = FakeProcess(auto_exit=False)

    async def spawn(*_args, **_kwargs):
        spawn_started.set()
        await release_spawn.wait()
        return process

    monkeypatch.setattr(asyncio, "create_subprocess_exec", spawn)
    decoder = FfmpegDecoder(settings())
    pending = asyncio.create_task(anext(decoder.stream(source_chunks(b"mp3"))))
    await spawn_started.wait()
    pending.cancel()
    await asyncio.sleep(0)
    assert not pending.done()
    release_spawn.set()
    with pytest.raises(asyncio.CancelledError):
        await pending
    assert process.terminate_calls == 1
    assert process.reaped.is_set()
    assert process.stdin.closed


async def test_aclose_after_first_pcm_reaps_decoder(monkeypatch) -> None:
    process = FakeProcess(stdout=(PCM,), auto_exit=False)
    install_process(monkeypatch, process)
    source_started = asyncio.Event()
    source_closed = asyncio.Event()
    decoder = FfmpegDecoder(settings())
    stream = decoder.stream(cancellation_source(source_started, source_closed))
    assert await anext(stream) == PCM
    await source_started.wait()
    await stream.aclose()
    assert source_closed.is_set()
    assert process.terminate_calls == 1
    assert process.reaped.is_set()


async def _run_real_ffmpeg(arguments: tuple[str, ...], data: bytes | None = None) -> bytes:
    process = await asyncio.create_subprocess_exec(
        "ffmpeg",
        "-hide_banner",
        "-loglevel",
        "error",
        "-nostdin",
        *arguments,
        stdin=asyncio.subprocess.PIPE if data is not None else asyncio.subprocess.DEVNULL,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        env={key: value for key, value in os.environ.items() if key.lower() in FFMPEG_ENV_KEYS},
    )
    try:
        async with asyncio.timeout(5):
            output, diagnostics = await process.communicate(data)
        assert process.returncode == 0, diagnostics.decode("utf-8", "replace")
        return output
    finally:
        if process.returncode is None:
            process.kill()
        await process.wait()


async def _synthetic_mp3(duration: float) -> bytes:
    return await _run_real_ffmpeg(
        (
            "-f",
            "lavfi",
            "-i",
            f"sine=frequency=440:sample_rate=24000:duration={duration}",
            "-ac",
            "1",
            "-b:a",
            "48k",
            "-f",
            "mp3",
            "pipe:1",
        )
    )


@pytest.mark.skipif(
    shutil.which("ffmpeg") is None, reason="Optional real decoder smoke requires ffmpeg"
)
async def test_real_ffmpeg_decodes_in_memory_mono_mp3_to_stereo_s16le_48000() -> None:
    mp3 = await _synthetic_mp3(0.25)

    async def provider():
        for offset in range(0, len(mp3), 97):
            yield mp3[offset : offset + 97]

    decoder = FfmpegDecoder(settings())
    async with asyncio.timeout(5):
        decoded = b"".join([chunk async for chunk in decoder.stream(provider())])
    assert 48000 <= len(decoded) <= 65000
    assert len(decoded) % 4 == 0
    samples = struct.unpack(f"<{len(decoded) // 2}h", decoded)
    left, right = samples[0::2], samples[1::2]
    assert left == right
    assert 1000 < max(left) < 5000

    def power(frequency: int) -> float:
        real = sum(
            value * math.cos(2 * math.pi * frequency * i / 48000) for i, value in enumerate(left)
        )
        imag = sum(
            value * math.sin(2 * math.pi * frequency * i / 48000) for i, value in enumerate(left)
        )
        return real * real + imag * imag

    assert power(440) > 100 * max(power(220), power(880))


async def _read_useful_early_pcm(stream: AsyncGenerator[bytes, None]) -> bytes:
    chunks = []
    received = 0
    # Include useful signal beyond MP3 encoder delay, not only a silent first frame.
    while received < 16384:
        chunk = await anext(stream)
        chunks.append(chunk)
        received += len(chunk)
    return b"".join(chunks)


@pytest.mark.skipif(
    shutil.which("ffmpeg") is None, reason="Optional real streaming regression requires ffmpeg"
)
@pytest.mark.parametrize("duration", [0.25, 1.0])
async def test_real_ffmpeg_streams_prefix_before_eof_without_losing_audio(duration: float) -> None:
    mp3 = await _synthetic_mp3(duration)
    # Independent default-FFmpeg decode, not production arguments or a mock decoder.
    reference = await _run_real_ffmpeg(
        (
            "-threads",
            "1",
            "-f",
            "mp3",
            "-i",
            "pipe:0",
            "-map",
            "0:a:0",
            "-filter_threads",
            "1",
            "-threads",
            "1",
            "-c:a",
            "pcm_s16le",
            "-ar",
            "48000",
            "-ac",
            "2",
            "-f",
            "s16le",
            "pipe:1",
        ),
        mp3,
    )
    reference_frames = len(reference) // 4
    assert int(duration * 48000) <= reference_frames <= int((duration + 0.1) * 48000)
    assert len(reference) % 4 == 0
    prefix_bytes = 1536
    assert len(mp3) > prefix_bytes
    prefix_fed = asyncio.Event()
    release = asyncio.Event()
    provider_closed = asyncio.Event()
    provided_bytes = 0

    async def provider():
        nonlocal provided_bytes
        try:
            provided_bytes = prefix_bytes
            yield mp3[:prefix_bytes]
            prefix_fed.set()
            await release.wait()
            provided_bytes = len(mp3)
            yield mp3[prefix_bytes:]
        finally:
            provider_closed.set()

    stream = FfmpegDecoder(settings()).stream(provider())
    early = asyncio.create_task(_read_useful_early_pcm(stream))
    try:
        await asyncio.wait_for(prefix_fed.wait(), 2)
        done, _ = await asyncio.wait({early}, timeout=2)
        assert done, "Decoder buffered the short MP3 until further input/EOF"
        leading = early.result()
        assert not release.is_set()
        assert not provider_closed.is_set()
        assert provided_bytes == prefix_bytes
        assert len(leading) % 4 == 0
        assert leading == reference[: len(leading)]
        samples = struct.unpack(f"<{len(leading) // 2}h", leading)
        assert max(abs(value) for value in samples) > 1000
        release.set()
        async with asyncio.timeout(5):
            decoded = leading + b"".join([chunk async for chunk in stream])
        assert provider_closed.is_set()
        assert len(decoded) // 4 == reference_frames
        assert decoded == reference
    finally:
        release.set()
        if not early.done():
            early.cancel()
        await asyncio.gather(early, return_exceptions=True)
        await stream.aclose()
