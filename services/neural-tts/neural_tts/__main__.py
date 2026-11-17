"""Run with python -m neural_tts; all settings and credentials come from env."""

import argparse
import logging
import shutil

from aiohttp import web

from .app import create_app
from .config import Settings


def main() -> None:
    parser = argparse.ArgumentParser(
        description=(
            "Authenticated, bounded Edge neural TTS proxy (unofficial, no SLA; not ChatGPT Vale)."
        )
    )
    parser.parse_args()
    try:
        settings = Settings.from_env()
    except ValueError as error:
        parser.error(str(error))
    if shutil.which(settings.ffmpeg_binary) is None:
        parser.error("ffmpeg is unavailable; install a patched ffmpeg and set NEURAL_TTS_FFMPEG")
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    logging.getLogger(__name__).info(
        "neural_tts listening host=%s port=%d", settings.host, settings.port
    )
    web.run_app(
        create_app(settings),
        host=settings.host,
        port=settings.port,
        access_log=None,
        print=None,
        handler_cancellation=True,
        shutdown_timeout=settings.terminate_timeout + 2 * settings.kill_timeout + 5,
    )


if __name__ == "__main__":
    main()
