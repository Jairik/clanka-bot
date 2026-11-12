"""Wyoming TTS server around Pocket TTS.

Protocol only: the model lives in engine.py so the wire behaviour can be tested
with a fake engine. Audio is streamed chunk by chunk as the model produces it,
so playback starts before the whole reply is synthesised.
"""

from __future__ import annotations

import asyncio
import logging
import os
import threading
import time
from typing import Optional

from wyoming.audio import AudioChunk, AudioStart, AudioStop
from wyoming.event import Event, async_read_event, async_write_event
from wyoming.info import Attribution, Describe, Info, TtsProgram, TtsVoice
from wyoming.tts import (
    Synthesize,
    SynthesizeChunk,
    SynthesizeStart,
    SynthesizeStop,
    SynthesizeStopped,
)

from engine import MAX_RATE, MIN_RATE, PocketTtsEngine
from pcm import to_pcm16

_LOGGER = logging.getLogger("tts")

_SENTENCE_END = ".?!"
_WIDTH = 2
_CHANNELS = 1


class TtsServer:
    """Serves one Wyoming connection at a time per client, one synthesis at a time."""

    def __init__(self, engine, default_voice: str = "alba") -> None:
        self.engine = engine
        self.default_voice = default_voice
        # Pocket TTS is not thread-safe, so generations are serialised.
        self.lock = asyncio.Lock()

    async def handle_connection(self, reader, writer) -> None:
        session = _Session(self, writer)
        try:
            while True:
                event = await async_read_event(reader)
                if event is None:
                    break
                if not await session.handle(event):
                    break
        except (ConnectionError, asyncio.IncompleteReadError):
            pass
        finally:
            writer.close()
            try:
                await writer.wait_closed()
            except (ConnectionError, RuntimeError):
                pass

    def info_event(self) -> Event:
        attribution = Attribution(
            name="Kyutai", url="https://github.com/kyutai-labs/pocket-tts"
        )
        language = getattr(self.engine, "language", "en")
        voices = [
            TtsVoice(
                name=name,
                attribution=attribution,
                installed=True,
                description=None,
                version=None,
                languages=[language],
            )
            for name in getattr(self.engine, "voices", [])
        ]
        program = TtsProgram(
            name="pocket-tts",
            attribution=attribution,
            installed=True,
            description="Pocket TTS (Kyutai 100M) running on CPU",
            version=getattr(self.engine, "version", None),
            voices=voices,
            supports_synthesize_streaming=True,
        )
        return Info(tts=[program]).event()

    def voice_info_event(self) -> Event:
        """The revision and built-in voices the assistant's library indexes."""
        return Event(
            type="voice-info-result",
            data={
                "revision": getattr(self.engine, "revision", "unknown"),
                "voices": list(getattr(self.engine, "voices", [])),
                "dir": str(getattr(self.engine, "voice_dir", "")),
            },
        )


class _Session:
    """Per-connection state, including the streaming-text buffer."""

    def __init__(self, server: TtsServer, writer) -> None:
        self.server = server
        self.writer = writer
        self.streaming = False
        self.voice: Optional[str] = None
        self.rate = 1.0
        self.text = ""
        self.sent_audio = False

    async def handle(self, event: Event) -> bool:
        if Describe.is_type(event.type):
            await self.write(self.server.info_event())
            return True

        # Voice-library protocol. These events are ours, not part of upstream
        # Wyoming; the assistant calls them before and during a library scan.
        if event.type == "voice-info":
            await self.write(self.server.voice_info_event())
            return True

        if event.type == "voice-validate":
            # cache=False is used by uploads: the state is validated from a
            # temporary path and must not stay in the process cache after the
            # file is renamed.
            cache = event.data.get("cache", True) is not False
            await self.validate_voice(str(event.data.get("path", "")), cache)
            return True

        if event.type == "voice-warm":
            await self.warm_voice(str(event.data.get("voice", "")))
            return True

        if event.type == "voice-forget":
            await self.forget_voice(str(event.data.get("voice", "")))
            return True

        if Synthesize.is_type(event.type):
            request = Synthesize.from_event(event)
            try:
                rate = request_rate(event)
            except ValueError as exc:
                await self.error(str(exc))
                return True
            await self.synthesize(request.text, voice_name(request.voice), rate)
            return True

        if SynthesizeStart.is_type(event.type):
            request = SynthesizeStart.from_event(event)
            try:
                self.rate = request_rate(event)
            except ValueError as exc:
                await self.error(str(exc))
                return True
            self.streaming = True
            self.voice = voice_name(request.voice)
            self.text = ""
            self.sent_audio = False
            return True

        if SynthesizeChunk.is_type(event.type):
            if not self.streaming:
                return True
            chunk = SynthesizeChunk.from_event(event)
            self.text += chunk.text
            # Flush complete sentences now; the rest waits for more text.
            complete, self.text = split_complete_sentences(self.text)
            for segment in complete:
                await self.synthesize(segment, self.voice, self.rate)
            return True

        if SynthesizeStop.is_type(event.type):
            if self.streaming:
                if self.text.strip():
                    await self.synthesize(self.text, self.voice, self.rate)
                if not self.sent_audio:
                    # The protocol expects final audio before synthesize-stopped.
                    await self.write_start()
                    await self.write(AudioStop().event())
                await self.write(SynthesizeStopped().event())
                self.streaming, self.text, self.sent_audio = False, "", False
            return True

        # Unknown events are ignored, as the protocol expects.
        return True

    async def synthesize(self, text: str, voice: Optional[str], rate: float = 1.0) -> None:
        text = text.strip()
        if not text:
            await self.error("text is required")
            return

        async with self.server.lock:
            try:
                state = self.server.engine.state_for(voice or self.server.default_voice)
            except Exception as exc:  # unknown voice or unreadable prompt
                await self.error(str(exc))
                return

            sample_rate = int(self.server.engine.sample_rate)
            await self.write_start()
            queue: asyncio.Queue = asyncio.Queue()
            loop = asyncio.get_running_loop()
            stop = threading.Event()

            def worker() -> None:
                def publish(item) -> None:
                    try:
                        asyncio.run_coroutine_threadsafe(queue.put(item), loop).result()
                    except RuntimeError:
                        pass  # the event loop is gone; nothing left to send

                try:
                    for chunk in self.server.engine.synthesize(state, text, stop=stop, rate=rate):
                        if stop.is_set():
                            return
                        publish(to_pcm16(chunk))
                except Exception as exc:
                    publish(exc)
                finally:
                    publish(None)

            # Generation blocks the CPU, so it runs off the event loop.
            threading.Thread(target=worker, daemon=True).start()

            failure: Optional[Exception] = None
            while True:
                item = await queue.get()
                if item is None:
                    break
                if isinstance(item, Exception):
                    failure = item
                    break
                if not item:
                    continue
                try:
                    await self.write(
                        AudioChunk(rate=sample_rate, width=_WIDTH, channels=_CHANNELS, audio=item).event()
                    )
                except (ConnectionError, RuntimeError):
                    stop.set()
                    return

            if failure is not None:
                _LOGGER.warning("synthesis failed: %s", failure)
                await self.error(str(failure))
                return
            await self.write(AudioStop().event())
            self.sent_audio = True

    async def validate_voice(self, path: str, cache: bool = True) -> None:
        """Load a copied state and run a real model smoke test.

        The model is not thread-safe, so this shares the synthesis lock. The
        response always carries ok/error; a bad file must not kill the server.
        """
        async with self.server.lock:
            try:
                revision = await asyncio.to_thread(
                    self.server.engine.validate_voice, path, cache
                )
                payload = {"ok": True, "revision": revision, "error": ""}
            except Exception as exc:
                payload = {"ok": False, "revision": "", "error": str(exc)}
        await self.write(Event(type="voice-validated", data=payload))

    async def forget_voice(self, voice: str) -> None:
        """Drop one cached state before a replacement is validated."""
        async with self.server.lock:
            try:
                await asyncio.to_thread(self.server.engine.forget_voice, voice)
                payload = {"ok": True, "error": ""}
            except Exception as exc:
                payload = {"ok": False, "error": str(exc)}
        await self.write(Event(type="voice-forgotten", data=payload))

    async def warm_voice(self, voice: str) -> None:
        async with self.server.lock:
            try:
                await asyncio.to_thread(self.server.engine.warm_voice, voice)
                payload = {"ok": True, "error": ""}
            except Exception as exc:
                payload = {"ok": False, "error": str(exc)}
        await self.write(Event(type="voice-warmed", data=payload))

    async def write_start(self) -> None:
        rate = int(self.server.engine.sample_rate)
        await self.write(AudioStart(rate=rate, width=_WIDTH, channels=_CHANNELS).event())

    async def write(self, event: Event) -> None:
        await async_write_event(event, self.writer)

    async def error(self, text: str) -> None:
        await self.write(
            Event(type="error", data={"text": text, "code": "synthesis-failed"})
        )


def split_complete_sentences(text: str) -> tuple[list[str], str]:
    """Split off complete sentences, returning (segments, remainder)."""
    segments: list[str] = []
    start = 0
    for index, char in enumerate(text):
        if char in _SENTENCE_END:
            segment = text[start : index + 1].strip()
            if segment:
                segments.append(segment)
            start = index + 1
    return segments, text[start:]


def voice_name(voice) -> Optional[str]:
    return voice.name if voice is not None else None


def request_rate(event: Event) -> float:
    """Read the optional speech-rate multiplier, failing closed on nonsense."""
    raw = event.data.get("rate", 1.0)
    try:
        rate = float(raw)
    except (TypeError, ValueError):
        raise ValueError("rate must be a number")
    if not MIN_RATE <= rate <= MAX_RATE:
        raise ValueError(f"rate must be between {MIN_RATE} and {MAX_RATE}")
    return rate


async def serve(engine, host: str, port: int) -> None:
    tts_server = TtsServer(engine, default_voice=getattr(engine, "default_voice", "alba"))
    listener = await asyncio.start_server(tts_server.handle_connection, host, port)
    addresses = ", ".join(str(sock.getsockname()) for sock in listener.sockets)
    _LOGGER.info("listening on %s", addresses)
    async with listener:
        await listener.serve_forever()


def main() -> None:
    logging.basicConfig(
        level=os.environ.get("TTS_LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    host = os.environ.get("TTS_HOST", "0.0.0.0")
    port = int(os.environ.get("TTS_PORT", "10200"))
    voice_list = os.environ.get("TTS_VOICES", os.environ.get("TTS_VOICE", "alba"))
    voices = [name.strip() for name in voice_list.split(",") if name.strip()] or ["alba"]
    default_voice = os.environ.get("TTS_VOICE", voices[0])

    engine = PocketTtsEngine(
        language=os.environ.get("TTS_LANGUAGE") or None,
        voices=voices,
        voice_dir=os.environ.get("TTS_VOICE_DIR") or None,
    )
    started = time.monotonic()
    engine.load()  # warm the model before the health check can pass
    engine.default_voice = default_voice
    _LOGGER.info(
        "model warm in %.1fs (sample rate %s Hz, voices %s)",
        time.monotonic() - started,
        engine.sample_rate,
        ", ".join(voices),
    )

    try:
        asyncio.run(serve(engine, host, port))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
