from __future__ import annotations

import math
import sys

import pytest
from conftest import TEST_TOKEN

from neural_tts import __main__
from neural_tts.app import SERVICE_KEY
from neural_tts.config import Settings


def test_environment_token_is_required_and_never_defaulted(monkeypatch) -> None:
    monkeypatch.setenv("NEURAL_TTS_TOKEN", "ambient-secret-not-to-use-000000000")
    with pytest.raises(ValueError, match="NEURAL_TTS_TOKEN"):
        Settings.from_env({})


def test_defaults_bind_only_loopback_and_hide_token() -> None:
    settings = Settings.from_env({"NEURAL_TTS_TOKEN": TEST_TOKEN})
    assert settings.host == "127.0.0.1"
    assert settings.port == 8090
    assert settings.max_concurrency == 2
    assert settings.first_audio_timeout == 8
    assert settings.total_timeout == 30
    assert TEST_TOKEN not in repr(settings)


@pytest.mark.parametrize("token", ["", "short", "x" * 257, "x" * 31 + " ", "x" * 31 + "я"])
def test_rejects_bad_tokens_without_echoing_them(token) -> None:
    with pytest.raises(ValueError) as failure:
        Settings(token=token)
    assert str(failure.value).startswith("NEURAL_TTS_TOKEN must")
    if token:
        assert token not in str(failure.value)


@pytest.mark.parametrize(
    "changes",
    [
        {"host": "localhost"},
        {"host": "0.0.0.0\nunsafe"},
        {"port": 0},
        {"port": 65536},
        {"port": True},
        {"max_concurrency": 0},
        {"max_body_bytes": -1},
        {"max_text_chars": 0},
        {"max_text_bytes": False},
        {"max_encoded_bytes": -1},
        {"first_audio_timeout": 0},
        {"total_timeout": math.inf},
        {"idle_timeout": math.nan},
        {"write_timeout": -1},
        {"max_audio_seconds": 0},
        {"kill_timeout": False},
        {"ffmpeg_binary": ""},
    ],
)
def test_invalid_resource_configuration_fails_startup(changes) -> None:
    with pytest.raises(ValueError):
        Settings(token=TEST_TOKEN, **changes)


def test_environment_limits_are_parsed() -> None:
    settings = Settings.from_env(
        {
            "NEURAL_TTS_TOKEN": TEST_TOKEN,
            "NEURAL_TTS_HOST": "::1",
            "NEURAL_TTS_PORT": "9099",
            "NEURAL_TTS_MAX_CONCURRENCY": "3",
            "NEURAL_TTS_MAX_TEXT_BYTES": "1024",
            "NEURAL_TTS_FIRST_AUDIO_TIMEOUT_SECONDS": "1.5",
            "NEURAL_TTS_TOTAL_TIMEOUT_SECONDS": "30",
        }
    )
    assert settings.host == "::1"
    assert settings.port == 9099
    assert settings.max_concurrency == 3
    assert settings.max_text_bytes == 1024
    assert settings.first_audio_timeout == 1.5
    assert settings.total_timeout == 30


@pytest.mark.parametrize(
    "variable",
    ["NEURAL_TTS_PORT", "NEURAL_TTS_MAX_CONCURRENCY", "NEURAL_TTS_TOTAL_TIMEOUT_SECONDS"],
)
def test_invalid_environment_numbers_are_safe_errors(variable) -> None:
    private_value = "not-a-number-or-safe-log-value"
    with pytest.raises(ValueError) as failure:
        Settings.from_env({"NEURAL_TTS_TOKEN": TEST_TOKEN, variable: private_value})
    assert variable in str(failure.value)
    assert private_value not in str(failure.value)


def test_cli_keeps_handler_cancellation_and_disables_access_logs(monkeypatch) -> None:
    recorded = []
    monkeypatch.setenv("NEURAL_TTS_TOKEN", TEST_TOKEN)
    monkeypatch.setattr(sys, "argv", ["neural_tts"])
    monkeypatch.setattr(__main__.shutil, "which", lambda _executable: "/fake/ffmpeg")
    monkeypatch.setattr(
        __main__.web, "run_app", lambda app, **kwargs: recorded.append((app, kwargs))
    )
    __main__.main()
    app, options = recorded[0]
    assert app[SERVICE_KEY].settings.host == "127.0.0.1"
    assert options["host"] == "127.0.0.1"
    assert options["port"] == 8090
    assert options["handler_cancellation"] is True
    assert options["access_log"] is None
    assert options["print"] is None
