"""Safe, stable public errors. Upstream text and exception details stay private."""

from http import HTTPStatus


class SpeechError(Exception):
    def __init__(self, status: HTTPStatus, code: str, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message


class ProviderError(SpeechError):
    def __init__(self) -> None:
        super().__init__(HTTPStatus.BAD_GATEWAY, "provider_failure", "Speech provider failed.")


class DecoderError(SpeechError):
    def __init__(self) -> None:
        super().__init__(HTTPStatus.BAD_GATEWAY, "decoder_failure", "Audio decoding failed.")


class CleanupError(SpeechError):
    def __init__(self) -> None:
        super().__init__(
            HTTPStatus.SERVICE_UNAVAILABLE,
            "cleanup_failure",
            "Decoder cleanup failed; service requires restart.",
        )


class AudioLimitError(SpeechError):
    def __init__(self) -> None:
        super().__init__(
            HTTPStatus.BAD_GATEWAY, "audio_limit_exceeded", "Audio exceeded its limit."
        )
