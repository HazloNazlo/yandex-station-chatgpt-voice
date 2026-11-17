"""Version-one transport framing around headerless interleaved PCM payloads."""

import struct

SAMPLE_RATE = 48000
CHANNELS = 2
SAMPLE_BITS = 16
PCM_FRAME_BYTES = CHANNELS * SAMPLE_BITS // 8
BYTES_PER_SECOND = SAMPLE_RATE * PCM_FRAME_BYTES
MAX_PACKET_BYTES = 3840  # 20 ms of PCM; length prefixes do not count toward this bound.
CONTENT_TYPE = "application/vnd.station.pcm-stream; version=1"
COMPLETION_PACKET = struct.pack("<I", 0)
AUDIO_HEADERS = {
    "Content-Type": CONTENT_TYPE,
    "X-Audio-Format": "pcm_s16le",
    "X-Audio-Sample-Rate": str(SAMPLE_RATE),
    "X-Audio-Channels": str(CHANNELS),
    "X-Audio-Sample-Bits": str(SAMPLE_BITS),
    "X-Audio-Framing": "length-prefix-le32",
    "X-Audio-Max-Frame-Bytes": str(MAX_PACKET_BYTES),
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    "X-Accel-Buffering": "no",
}
VOICES = frozenset({"ru-RU-SvetlanaNeural", "ru-RU-DmitryNeural"})
DEFAULT_VOICE = "ru-RU-SvetlanaNeural"
