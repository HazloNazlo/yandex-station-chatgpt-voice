from __future__ import annotations

import asyncio

import pytest
from conftest import AUTH, PCM, TEST_TOKEN, ProviderProbe
from test_decoder import FakeProcess, install_process, settings

from neural_tts.app import SERVICE_KEY
from neural_tts.decoder import FfmpegDecoder


async def test_failed_reaping_fails_closed_without_starting_more_processes(
    client_factory, monkeypatch, caplog
) -> None:
    process = FakeProcess(auto_exit=False, terminate_exits=False)
    calls = install_process(monkeypatch, process)

    def unkillable() -> None:
        process.kill_calls += 1

    monkeypatch.setattr(process, "kill", unkillable)
    provider = ProviderProbe(after=asyncio.Event())
    decoder = FfmpegDecoder(settings())
    client = await client_factory(
        provider=provider.stream, decoder=decoder.stream, first_audio_timeout=0.01
    )
    response = await client.post("/v1/speech", json={"text": "hello"}, headers=AUTH)
    assert response.status == 503
    assert (await response.json())["error"]["code"] == "cleanup_failure"
    assert "X-Audio-Format" not in response.headers
    assert provider.closed.is_set()
    assert process.terminate_calls == 1
    assert process.kill_calls == 1
    assert not process.reaped.is_set()
    assert client.server.app[SERVICE_KEY].active_requests == 0
    assert "ffmpeg_cleanup_failed code=reap_timeout" in caplog.text
    following = await client.post("/v1/speech", json={"text": "do not spawn"}, headers=AUTH)
    assert following.status == 503
    assert (await following.json())["error"]["code"] == "cleanup_failure"
    assert len(calls) == 1
    assert provider.calls == [("hello", "ru-RU-SvetlanaNeural")]
    # This is only a fake OS process; release its simulated wait after the assertions.
    process.exit(-9)
    await process.wait()


async def test_spawn_cancellation_drains_stderr_before_reaping(monkeypatch) -> None:
    started = asyncio.Event()
    release_spawn = asyncio.Event()
    drained = asyncio.Event()
    diagnostics = 1024 * 1024

    def stderr_read(total: int) -> None:
        if total >= diagnostics:
            drained.set()

    process = FakeProcess(stderr=(b"e" * diagnostics,), auto_exit=False, stderr_read=stderr_read)
    original_wait = process.wait

    async def wait() -> int:
        await drained.wait()
        return await original_wait()

    async def spawn(*_args, **_kwargs):
        started.set()
        await release_spawn.wait()
        return process

    monkeypatch.setattr(process, "wait", wait)
    monkeypatch.setattr(asyncio, "create_subprocess_exec", spawn)

    async def source():
        yield b"unneeded MP3"

    pending = asyncio.create_task(anext(FfmpegDecoder(settings()).stream(source())))
    await started.wait()
    pending.cancel()
    await asyncio.sleep(0)
    release_spawn.set()
    with pytest.raises(asyncio.CancelledError):
        await pending
    assert process.stderr.bytes_read == diagnostics
    assert process.reaped.is_set()
    assert process.stdin.closed
    assert not [task for task in asyncio.all_tasks() if task.get_name().startswith("neural-tts-")]


async def test_child_does_not_inherit_tokens_or_report_file_logging(monkeypatch) -> None:
    process = FakeProcess(stdout=(PCM,))
    calls = install_process(monkeypatch, process)
    monkeypatch.setenv("PATH", "/runtime-path-for-fake-process")
    monkeypatch.setenv("NEURAL_TTS_TOKEN", TEST_TOKEN)
    monkeypatch.setenv("OPENAI_API_KEY", "test-only-not-an-api-key")
    monkeypatch.setenv("FFREPORT", "file=must-not-write.log:level=48")

    async def source():
        yield b"MP3"

    result = [chunk async for chunk in FfmpegDecoder(settings()).stream(source())]
    assert result == [PCM]
    child_env = calls[0][1]["env"]
    assert child_env["PATH"] == "/runtime-path-for-fake-process"
    assert "NEURAL_TTS_TOKEN" not in child_env
    assert "OPENAI_API_KEY" not in child_env
    assert "FFREPORT" not in child_env
    assert process.reaped.is_set()
