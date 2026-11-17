"""Environment-only server configuration; never put secrets in reprs or errors."""

from __future__ import annotations

import ipaddress
import math
import os
from collections.abc import Mapping
from dataclasses import dataclass, field

TOKEN_MIN_CHARS = 32
TOKEN_MAX_CHARS = 256
ASCII_FIRST_VISIBLE = 33
ASCII_LAST_VISIBLE = 126
TCP_MAX_PORT = 65535


@dataclass(frozen=True)
class Settings:
    token: str = field(repr=False)
    host: str = "127.0.0.1"
    port: int = 8090
    ffmpeg_binary: str = "ffmpeg"
    max_concurrency: int = 2
    max_body_bytes: int = 32768
    max_text_chars: int = 4096
    max_text_bytes: int = 16384
    max_encoded_bytes: int = 2097152
    max_audio_seconds: float = 120.0
    body_timeout: float = 5.0
    first_audio_timeout: float = 8.0
    idle_timeout: float = 10.0
    write_timeout: float = 10.0
    total_timeout: float = 30.0
    terminate_timeout: float = 1.0
    kill_timeout: float = 2.0

    def __post_init__(self) -> None:
        if (
            not isinstance(self.token, str)
            or not TOKEN_MIN_CHARS <= len(self.token) <= TOKEN_MAX_CHARS
        ):
            raise ValueError("NEURAL_TTS_TOKEN must contain 32-256 printable ASCII characters")
        if any(not ASCII_FIRST_VISIBLE <= ord(char) <= ASCII_LAST_VISIBLE for char in self.token):
            raise ValueError("NEURAL_TTS_TOKEN must contain printable ASCII without whitespace")
        try:
            ipaddress.ip_address(self.host)
        except ValueError:
            raise ValueError("NEURAL_TTS_HOST must be a bind IP address") from None
        if (
            not isinstance(self.port, int)
            or isinstance(self.port, bool)
            or not 1 <= self.port <= TCP_MAX_PORT
        ):
            raise ValueError("NEURAL_TTS_PORT must be an integer between 1 and 65535")
        if not self.ffmpeg_binary or "\x00" in self.ffmpeg_binary:
            raise ValueError("NEURAL_TTS_FFMPEG must name an executable")
        for name, variable in _INTEGER_ENV.items():
            value = getattr(self, name)
            if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
                raise ValueError(f"{variable} must be a positive integer")
        for name, variable in _SECONDS_ENV.items():
            value = getattr(self, name)
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise ValueError(f"{variable} must be a finite positive number")
            if not math.isfinite(value) or value <= 0:
                raise ValueError(f"{variable} must be a finite positive number")

    @classmethod
    def from_env(cls, environ: Mapping[str, str] | None = None) -> Settings:
        env = os.environ if environ is None else environ
        values: dict[str, object] = {"token": env.get("NEURAL_TTS_TOKEN", "")}
        for name, variable in _STRING_ENV.items():
            if variable in env:
                values[name] = env[variable]
        for name, variable in {"port": "NEURAL_TTS_PORT", **_INTEGER_ENV}.items():
            if variable in env:
                try:
                    values[name] = int(env[variable])
                except ValueError:
                    raise ValueError(f"{variable} must be a positive integer") from None
        for name, variable in _SECONDS_ENV.items():
            if variable in env:
                try:
                    values[name] = float(env[variable])
                except ValueError:
                    raise ValueError(f"{variable} must be a finite positive number") from None
        return cls(**values)  # type: ignore[arg-type]  # Values are parsed and validated above.


_STRING_ENV = {"host": "NEURAL_TTS_HOST", "ffmpeg_binary": "NEURAL_TTS_FFMPEG"}
_INTEGER_ENV = {
    "max_concurrency": "NEURAL_TTS_MAX_CONCURRENCY",
    "max_body_bytes": "NEURAL_TTS_MAX_BODY_BYTES",
    "max_text_chars": "NEURAL_TTS_MAX_TEXT_CHARS",
    "max_text_bytes": "NEURAL_TTS_MAX_TEXT_BYTES",
    "max_encoded_bytes": "NEURAL_TTS_MAX_ENCODED_BYTES",
}
_SECONDS_ENV = {
    "max_audio_seconds": "NEURAL_TTS_MAX_AUDIO_SECONDS",
    "body_timeout": "NEURAL_TTS_BODY_TIMEOUT_SECONDS",
    "first_audio_timeout": "NEURAL_TTS_FIRST_AUDIO_TIMEOUT_SECONDS",
    "idle_timeout": "NEURAL_TTS_IDLE_TIMEOUT_SECONDS",
    "write_timeout": "NEURAL_TTS_WRITE_TIMEOUT_SECONDS",
    "total_timeout": "NEURAL_TTS_TOTAL_TIMEOUT_SECONDS",
    "terminate_timeout": "NEURAL_TTS_TERMINATE_TIMEOUT_SECONDS",
    "kill_timeout": "NEURAL_TTS_KILL_TIMEOUT_SECONDS",
}
