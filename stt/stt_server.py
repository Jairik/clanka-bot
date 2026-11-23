"""Wyoming STT server around Moonshine Voice.

Protocol only: the model lives in engine.py so the wire behaviour can be tested
with a fake engine. Audio arrives as Wyoming audio-chunk events and the Go
middleware does the endpointing, so the server transcribes until audio-stop.

One transcription runs at a time, because Moonshine streams share a
transcriber. A second request waits SESSION_WAIT_SECONDS for the model and is
then refused with a `busy` error rather than a client-side timeout, and a
session that falls silent for SESSION_TIMEOUT_SECONDS gives the model back
(#196).
"""

from __future__ import annotations

import asyncio
import logging
import os
import sys
import time
from typing import Optional

from wyoming.asr import Transcribe, TranscriptChunk, TranscriptStop
from wyoming.audio import AudioChunk, AudioStart, AudioStop
from wyoming.event import Event, async_read_event, async_write_event
from wyoming.info import AsrModel, AsrProgram, Attribution, Describe, Info

from correct import (
    VocabularyCorrector,
    correction_enabled,
    keyterm_bias_enabled,
    load_keyterms,
)
from moonshine_engine import MoonshineEngine
from pcm16 import to_floats

_LOGGER = logging.getLogger("stt")

_ATTRIBUTION = Attribution(
    name="Moonshine AI", url="https://github.com/moonshine-ai/moonshine"
)

# How long a transcription waits for the model another session holds before it
# is refused. Well under the Go client's stt.timeout (20 s), so a refusal is a
# clear answer instead of a read timeout.
SESSION_WAIT_SECONDS = float(os.environ.get("STT_SESSION_WAIT", "5"))
# No audio for this long ends a session and gives the model back. This is the
# server-side bound; the client's own utterance cap is not trusted.
SESSION_TIMEOUT_SECONDS = float(os.environ.get("STT_SESSION_TIMEOUT", "30"))

BUSY_TEXT = "speech recognition is busy"
IDLE_TEXT = "speech recognition timed out waiting for audio"


def _deadline(seconds: float):
    """A timeout for one await, disabled when the configured bound is <= 0."""
    return asyncio.timeout(seconds if seconds > 0 else None)


class SttServer:
    """Serves transcription requests; one active stream at a time."""

    def __init__(self, engine, session_wait=None, session_timeout=None, corrector=None) -> None:
        self.engine = engine
        # Moonshine streams share one transcriber; serialise them.
        self.lock = asyncio.Lock()
        self.session_wait = SESSION_WAIT_SECONDS if session_wait is None else session_wait
        self.session_timeout = (
            SESSION_TIMEOUT_SECONDS if session_timeout is None else session_timeout
        )
        # None means leave the model text alone (tests, or STT_CORRECT=0).
        self.corrector = corrector

    async def handle_connection(self, reader, writer) -> None:
        session = _Session(self, writer)
        try:
            while True:
                try:
                    # A session that holds the model and falls silent is ended
                    # by the idle bound; a connection that never asked for
                    # anything, such as a half-open one, is just reaped (#196).
                    async with _deadline(self.session_timeout):
                        event = await async_read_event(reader)
                except TimeoutError:
                    await session.expire()
                    break
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

    def corrected_transcript(self, raw: str, language: str | None) -> dict:
        """Build the Wyoming transcript payload, keeping the raw model text.

        Routing sees the corrected string. A rewrite stores raw_text on the
        event so diagnosis can recover the model output without logging it.
        STT_CORRECT=0, or a missing corrector, leaves the model text as-is.
        """
        text = raw
        if self.corrector is not None:
            result = self.corrector.apply(raw)
            text = result.text
            if result.replacements:
                _LOGGER.info(
                    "applied %d vocabulary corrections",
                    len(result.replacements),
                )
        payload = {"text": text}
        if language:
            payload["language"] = language
        if text != raw:
            payload["raw_text"] = raw
        return payload


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
        # Idle clock: reset when a session starts and on every audio event, so
        # the bound covers a client that stops mid-session without closing.
        self.last_audio = time.monotonic()

    async def handle(self, event: Event) -> bool:
        if Describe.is_type(event.type):
            await self.write(self.server.info_event())
            return True

        if Transcribe.is_type(event.type):
            request = Transcribe.from_event(event)
            await self.close()
            # The session clock starts here: the wait for the model is bounded
            # separately, and audio is due once the model is ours.
            self.last_audio = time.monotonic()
            if not await self.acquire():
                return True
            try:
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
            self.last_audio = time.monotonic()
            return True

        if AudioChunk.is_type(event.type):
            self.last_audio = time.monotonic()
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
                raw = await asyncio.to_thread(self.stream.finish)
                payload = self.server.corrected_transcript(raw, self.language)
                await self.write(Event(type="transcript", data=payload))
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

    async def acquire(self) -> bool:
        """Take the model, or refuse the session as busy within the wait budget."""
        started = time.monotonic()
        acquired = False
        try:
            async with _deadline(self.server.session_wait):
                await self.server.lock.acquire()
                acquired = True
        except TimeoutError:
            # asyncio.timeout can fire after the acquire returned; releasing
            # here keeps one bad race from leaking the model for good.
            if acquired:
                self.server.lock.release()
                acquired = False
        if acquired:
            self.holds_lock = True
            return True
        _LOGGER.warning(
            "refused a transcription as busy after %.1fs; another session holds the model",
            time.monotonic() - started,
        )
        await self.error(BUSY_TEXT, code="busy")
        return False

    async def expire(self) -> None:
        """Reap a connection that went silent, releasing the model if held."""
        silent = time.monotonic() - self.last_audio
        if not self.holds_lock:
            # Nothing was waiting on this connection; closing it is enough.
            _LOGGER.debug("closed an idle connection after %.0fs", silent)
            return
        _LOGGER.warning(
            "ended an idle transcription after %.0fs without audio; released the model",
            silent,
        )
        await self.error(IDLE_TEXT, code="idle-timeout")

    async def error(self, text: str, code: str = "transcription-failed") -> None:
        await self.write(Event(type="error", data={"text": text, "code": code}))

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


async def serve(engine, host: str, port: int, corrector=None) -> None:
    stt_server = SttServer(engine, corrector=corrector)
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
    # Both gates are off unless the env is an explicit on-value. Measured
    # evidence from the replay corpus must land before either is enabled.
    keyterms = load_keyterms() if keyterm_bias_enabled() else []

    engine = MoonshineEngine(model=model, language=language, keyterms=keyterms)
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

    corrector = VocabularyCorrector.from_path() if correction_enabled() else None
    if corrector is not None:
        _LOGGER.info("vocabulary correction on (%d terms)", len(corrector.terms))
    else:
        _LOGGER.info("vocabulary correction off")
    if keyterms:
        _LOGGER.info("keyterm bias: %s", ", ".join(keyterms))

    try:
        asyncio.run(serve(engine, host, port, corrector=corrector))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
