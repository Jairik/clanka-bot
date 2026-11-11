"""Wyoming STT server around Moonshine Voice.

Protocol only: the model lives in engine.py so the wire behaviour can be tested
with a fake engine. Audio arrives as Wyoming audio-chunk events and the Go
middleware does the endpointing, so the server transcribes until audio-stop.
"""

from __future__ import annotations

import asyncio
import logging
import os
import sys
import time
from pathlib import Path
from typing import Optional

from wyoming.asr import Transcribe, Transcript, TranscriptChunk, TranscriptStop
from wyoming.audio import AudioChunk, AudioStart, AudioStop
from wyoming.event import Event, async_read_event, async_write_event
from wyoming.info import AsrModel, AsrProgram, Attribution, Describe, Info

from moonshine_engine import MoonshineEngine
from pcm16 import to_floats

_LOGGER = logging.getLogger("stt")

_ATTRIBUTION = Attribution(
    name="Moonshine AI", url="https://github.com/moonshine-ai/moonshine"
)


class SttServer:
    """Serves transcription requests; one active stream at a time."""

    def __init__(self, engine) -> None:
        self.engine = engine
        # Moonshine streams share one transcriber; serialise them.
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
            await session.close()
            writer.close()
            try:
                await writer.wait_closed()
            except (ConnectionError, RuntimeError):
                pass

    def info_event(self) -> Event:
        languages = [getattr(self.engine, "language", "en")]
        model = AsrModel(
            name=self.engine.model,
            attribution=_ATTRIBUTION,
            installed=True,
            description="Moonshine Voice streaming speech-to-text",
            version=getattr(self.engine, "version", None),
            languages=languages,
        )
        program = AsrProgram(
            name="moonshine",
            attribution=_ATTRIBUTION,
            installed=True,
            description="Moonshine Voice on CPU",
            version=getattr(self.engine, "version", None),
            models=[model],
            supports_transcript_streaming=True,
            # The Go middleware owns endpointing; this service has no VAD gate.
            requires_external_vad=True,
        )
        return Info(asr=[program]).event()


class _Session:
    """Per-connection state. Holds the server lock for one transcription."""

    def __init__(self, server: SttServer, writer) -> None:
        self.server = server
        self.writer = writer
        self.stream = None
        self.rate = 16000
        self.language: Optional[str] = None
        self.sent_partial = ""
        self.holds_lock = False

    async def handle(self, event: Event) -> bool:
        if Describe.is_type(event.type):
            await self.write(self.server.info_event())
            return True

        if Transcribe.is_type(event.type):
            request = Transcribe.from_event(event)
            await self.close()
            try:
                await self.server.lock.acquire()
                self.holds_lock = True
                self.language = request.language or getattr(self.server.engine, "language", "en")
                self.stream = await asyncio.to_thread(
                    self.server.engine.create_stream, request.name
                )
            except Exception as exc:  # unknown model, model load failure
                await self.error(str(exc))
                await self.release()
            return True

        if AudioStart.is_type(event.type):
            start = AudioStart.from_event(event)
            self.rate = start.rate
            return True

        if AudioChunk.is_type(event.type):
            if self.stream is None:
                return True
            chunk = AudioChunk.from_event(event)
            try:
                await asyncio.to_thread(
                    self.stream.add_audio, to_floats(chunk.audio), chunk.rate
                )
            except Exception as exc:
                await self.error(str(exc))
                await self.close()
                return True
            partial = self.stream.partial
            if partial and partial != self.sent_partial:
                self.sent_partial = partial
                await self.write(TranscriptChunk(text=partial).event())
            return True

        if AudioStop.is_type(event.type):
            if self.stream is None:
                return True
            try:
                text = await asyncio.to_thread(self.stream.finish)
                await self.write(Transcript(text=text, language=self.language).event())
                await self.write(TranscriptStop().event())
            except Exception as exc:
                await self.error(str(exc))
            finally:
                stream, self.stream = self.stream, None
                if stream is not None:
                    try:
                        await asyncio.to_thread(stream.close)
                    except Exception:
                        pass
                await self.release()
            return True

        # Unknown events are ignored, as the protocol expects.
        return True

    async def write(self, event: Event) -> None:
        await async_write_event(event, self.writer)

    async def error(self, text: str) -> None:
        await self.write(
            Event(type="error", data={"text": text, "code": "transcription-failed"})
        )

    async def release(self) -> None:
        if self.holds_lock:
            self.holds_lock = False
            self.server.lock.release()

    async def close(self) -> None:
        if self.stream is not None:
            stream = self.stream
            self.stream = None
            try:
                await asyncio.to_thread(stream.close)
            except Exception:
                pass
        await self.release()


async def serve(engine, host: str, port: int) -> None:
    stt_server = SttServer(engine)
    listener = await asyncio.start_server(stt_server.handle_connection, host, port)
    addresses = ", ".join(str(sock.getsockname()) for sock in listener.sockets)
    _LOGGER.info("listening on %s", addresses)
    async with listener:
        await listener.serve_forever()


def main() -> None:
    logging.basicConfig(
        level=os.environ.get("STT_LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    host = os.environ.get("STT_HOST", "0.0.0.0")
    port = int(os.environ.get("STT_PORT", "10300"))
    model = os.environ.get("STT_MODEL", "small-streaming")
    language = os.environ.get("STT_LANGUAGE", "en")

    engine = MoonshineEngine(model=model, language=language)
    started = time.monotonic()
    try:
        engine.load()
    except Exception as exc:
        print(f"stt: cannot load model {model!r}: {exc}", file=sys.stderr)
        raise
    _LOGGER.info(
        "model %s warm in %.1fs (%s, %d Hz)",
        model,
        time.monotonic() - started,
        language,
        getattr(engine, "sample_rate", 16000),
    )

    try:
        asyncio.run(serve(engine, host, port))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
