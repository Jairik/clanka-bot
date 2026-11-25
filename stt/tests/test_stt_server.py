import asyncio
import sys
import threading
import time
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import stt_server  # noqa: E402
from correct import VocabularyCorrector  # noqa: E402
from parakeet_engine import ParakeetStream, _extract_text, resolve_model_name  # noqa: E402
from vad import SileroVadSession  # noqa: E402
from wyoming.asr import Transcribe, Transcript, TranscriptChunk, TranscriptStop  # noqa: E402
from wyoming.audio import AudioChunk, AudioStart, AudioStop  # noqa: E402
from wyoming.client import AsyncTcpClient  # noqa: E402
from wyoming.event import Event  # noqa: E402
from wyoming.info import AsrProgram, Describe, Info  # noqa: E402


class FakeStream:
    """Stands in for a Parakeet stream: records audio, returns canned text."""

    def __init__(self, text="what time is it", partial="", fail=None):
        self.text = text
        self.partial_text = partial
        self.fail = fail
        self.audio = []
        self.rates = []
        self.closed = False

    @property
    def partial(self):
        return self.partial_text

    def add_audio(self, samples, rate):
        if self.fail == "add":
            raise RuntimeError("model exploded mid-stream")
        self.audio.append(list(samples))
        self.rates.append(rate)

    def finish(self):
        if self.fail == "finish":
            raise RuntimeError("model exploded at the end")
        return self.text

    def close(self):
        self.closed = True


class FakeEngine:
    language = "en"
    version = "test"
    sample_rate = 16000
    supports_streaming = False

    def __init__(self, model="parakeet-tdt-0.6b-v2", stream=None, fail=None, factory=None):
        self.model = model
        self.stream = stream or FakeStream()
        self.fail = fail
        # A factory makes one stream per session, so a concurrency test can tell
        # the sessions apart; otherwise every session shares one stream.
        self.factory = factory
        self.requested = []
        self.streams = []

    def create_stream(self, model=None):
        if self.fail == "create":
            raise RuntimeError("model is not baked into the image")
        self.requested.append(model)
        stream = self.factory() if self.factory is not None else self.stream
        self.streams.append(stream)
        return stream


class FakeVadSession:
    def __init__(self, scores, fail=False):
        self._scores = list(scores)
        self._fail = fail
        self.calls = []

    def score(self, samples, sample_rate=16000):
        if self._fail:
            raise RuntimeError("vad exploded")
        self.calls.append((list(samples), sample_rate))
        if self._scores:
            return self._scores.pop(0)
        return 0.0


class FakeVadEngine:
    def __init__(self, scores=(0.92, 0.05), fail=False):
        self.scores = list(scores)
        self.fail = fail
        self.sessions = []

    def create_session(self, sample_rate=16000):
        session = FakeVadSession(self.scores, fail=self.fail)
        self.sessions.append((sample_rate, session))
        return session


async def with_server(engine, body, **server_options):
    stt_server_instance = stt_server.SttServer(engine, **server_options)
    listener = await asyncio.start_server(stt_server_instance.handle_connection, "127.0.0.1", 0)
    port = listener.sockets[0].getsockname()[1]
    try:
        await body(port)
    finally:
        listener.close()
        await listener.wait_closed()


def run(engine, body, **server_options):
    asyncio.run(with_server(engine, body, **server_options))


async def connect(port):
    return AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2)


def test_describe_advertises_parakeet():
    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Describe().event())
            event = await client.read_event()
            assert event is not None
            assert Info.is_type(event.type)
            info = Info.from_event(event)
            assert len(info.asr) == 1
            program = info.asr[0]
            assert isinstance(program, AsrProgram)
            assert program.name == "parakeet"
            assert program.installed is True
            assert program.supports_transcript_streaming is False
            assert [model.name for model in program.models] == ["parakeet-tdt-0.6b-v2"]
            assert program.models[0].languages == ["en"]
            assert program.attribution.url.startswith("https://")

    run(FakeEngine(), body)


def test_transcribe_returns_final_text_and_optional_partials():
    stream = FakeStream(text="what time is it", partial="what time")
    engine = FakeEngine(stream=stream)

    async def body(port):
        async with await connect(port) as client:
            await client.write_event(
                Transcribe(name="parakeet-tdt-0.6b-v2", language="en").event()
            )
            await client.write_event(
                AudioStart(rate=16000, width=2, channels=1).event()
            )
            # 0.5 and -0.5 in signed 16-bit little-endian.
            pcm = b"\x00\x40\x00\xc0"
            await client.write_event(
                AudioChunk(rate=16000, width=2, channels=1, audio=pcm).event()
            )
            await client.write_event(AudioStop().event())

            events = {}
            while True:
                event = await client.read_event()
                assert event is not None
                events[event.type] = event
                if TranscriptStop.is_type(event.type):
                    break

            assert engine.requested == ["parakeet-tdt-0.6b-v2"]
            assert events["transcript-chunk"].data["text"] == "what time"
            assert events["transcript"].data["text"] == "what time is it"
            assert stream.rates == [16000]

    run(engine, body)
    assert stream.audio == [[0.5, -0.5]]
    assert stream.closed is True


def test_empty_transcript_is_still_a_complete_response():
    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Transcribe().event())
            await client.write_event(AudioStart(rate=16000, width=2, channels=1).event())
            await client.write_event(AudioStop().event())
            event = await client.read_event()
            assert event is not None and Transcript.is_type(event.type)
            assert Transcript.from_event(event).text == ""
            stop = await client.read_event()
            assert stop is not None and TranscriptStop.is_type(stop.type)

    run(FakeEngine(stream=FakeStream(text="")), body)


def test_missing_model_becomes_an_error_event():
    engine = FakeEngine(fail="create")

    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Transcribe(name="parakeet-tdt-0.6b-v3").event())
            event = await client.read_event()
            assert event is not None and event.type == "error"
            assert "not baked" in event.data["text"]
            assert event.data["code"] == "transcription-failed"

    run(engine, body)


def test_model_failure_mid_stream_becomes_an_error_event():
    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Transcribe().event())
            await client.write_event(AudioStart(rate=16000, width=2, channels=1).event())
            await client.write_event(
                AudioChunk(rate=16000, width=2, channels=1, audio=b"\x01\x00").event()
            )
            event = await client.read_event()
            assert event is not None and event.type == "error"
            assert "mid-stream" in event.data["text"]

    run(FakeEngine(stream=FakeStream(fail="add")), body)


def test_model_failure_at_the_end_becomes_an_error_event():
    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Transcribe().event())
            await client.write_event(AudioStart(rate=16000, width=2, channels=1).event())
            await client.write_event(AudioStop().event())
            event = await client.read_event()
            assert event is not None and event.type == "error"
            assert "at the end" in event.data["text"]

    run(FakeEngine(stream=FakeStream(fail="finish")), body)


def test_pcm_conversion_scales_signed_16_bit():
    from pcm16 import to_floats

    assert to_floats(b"\x00\x40\x00\xc0") == [0.5, -0.5]
    assert to_floats(b"") == []


def test_parakeet_stream_buffers_and_decodes_once():
    seen = []

    class FakeRecognizer:
        def recognize(self, audio, sample_rate=16000):
            seen.append((audio.copy(), sample_rate))
            return "  hello world.  "

    stream = ParakeetStream(FakeRecognizer(), threading.Lock())
    stream.add_audio([0.5, -0.5], 16000)
    stream.add_audio([0.25], 16000)
    assert stream.partial == ""
    assert seen == []
    assert stream.finish() == "hello world."
    assert len(seen) == 1
    assert seen[0][1] == 16000
    assert np.allclose(seen[0][0], np.array([0.5, -0.5, 0.25], dtype=np.float32))
    # Empty stream returns "" without invoking recognizer.
    assert stream.finish() == ""
    assert len(seen) == 1


def test_parakeet_model_aliases_and_text_extraction():
    assert resolve_model_name("nemo-parakeet-tdt-0.6b-v2") == "parakeet-tdt-0.6b-v2"
    assert resolve_model_name("parakeet-tdt-0.6b-v2") == "parakeet-tdt-0.6b-v2"
    assert _extract_text(" hello ") == "hello"
    assert _extract_text([" hello ", "world"]) == "hello world"
    assert _extract_text(None) == ""


def test_silero_vad_session_maintains_context_and_state():
    seen = []

    class FakeOrtSession:
        def run(self, outputs, feeds):
            assert outputs == ["output", "stateN"]
            assert feeds["input"].shape == (1, 576)
            assert feeds["state"].shape == (2, 1, 128)
            assert list(feeds["sr"]) == [16000]
            seen.append((feeds["input"].copy(), feeds["state"].copy()))
            next_state = feeds["state"] + 1.0
            prob = np.array([[0.8 if len(seen) == 1 else 0.1]], dtype=np.float32)
            return prob, next_state

    vad = SileroVadSession(FakeOrtSession(), threading.Lock(), sample_rate=16000)
    first = np.linspace(-0.5, 0.5, 512, dtype=np.float32)
    second = np.zeros(512, dtype=np.float32)

    assert abs(vad.score(first, 16000) - 0.8) < 1e-5
    assert abs(vad.score(second, 16000) - 0.1) < 1e-5
    # First frame has zero-padded context; second frame carries tail of first.
    assert np.allclose(seen[0][0][0, :64], 0.0)
    assert np.allclose(seen[1][0][0, :64], first[-64:])
    # RNN state propagated across frames.
    assert np.allclose(seen[1][1], 1.0)


# --- Silero VAD wire protocol ----------------------------------------------

FMT = {"rate": 16000, "width": 2, "channels": 1}
PCM = b"\x00\x40\x00\xc0"


def test_vad_scores_chunks_without_contending_with_asr_lock():
    vad_engine = FakeVadEngine(scores=(0.88, 0.12))
    engine = FakeEngine(stream=FakeStream(text="turn on the lights"))

    async def body(port):
        # Hold the ASR lock with an open transcription session.
        async with await connect(port) as asr_client:
            await asr_client.write_event(Transcribe().event())
            await asr_client.write_event(AudioStart(**FMT).event())

            # A VAD session on a second connection still succeeds immediately.
            async with await connect(port) as vad_client:
                await vad_client.write_event(Event(type="vad-start", data=FMT))
                ready = await vad_client.read_event()
                assert ready is not None and ready.type == "vad-ready"

                await vad_client.write_event(AudioChunk(**FMT, audio=PCM).event())
                first = await vad_client.read_event()
                assert first is not None and first.type == "vad-score"
                assert first.data["probability"] == 0.88

                await vad_client.write_event(AudioChunk(**FMT, audio=PCM).event())
                second = await vad_client.read_event()
                assert second is not None and second.type == "vad-score"
                assert second.data["probability"] == 0.12

                await vad_client.write_event(AudioStop().event())

            await asr_client.write_event(AudioChunk(**FMT, audio=PCM).event())
            await asr_client.write_event(AudioStop().event())
            ev = await asr_client.read_event()
            assert ev is not None and ev.type == "transcript"
            assert ev.data["text"] == "turn on the lights"

    run(engine, body, vad_engine=vad_engine)


def test_vad_unavailable_returns_structured_error():
    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Event(type="vad-start", data=FMT))
            event = await client.read_event()
            assert event is not None and event.type == "error"
            assert event.data["code"] == "vad-unavailable"

    run(FakeEngine(), body, vad_engine=None)


# --- One model, one session (#196) -----------------------------------------
#
# The voice loop and the Dashboard mic reach the same service over separate
# connections, so a second transcription while one is in flight is normal. It
# must be refused as busy inside the wait budget, and a session that stops
# sending audio must give the model back instead of holding it until restart.


async def full_session(port):
    """Run one complete transcription on its own connection."""
    async with await connect(port) as client:
        await client.write_event(Transcribe().event())
        await client.write_event(AudioStart(**FMT).event())
        await client.write_event(AudioChunk(**FMT, audio=PCM).event())
        await client.write_event(AudioStop().event())
        seen = {}
        while True:
            event = await client.read_event()
            assert event is not None, "service closed before transcript-stop"
            seen[event.type] = event
            if TranscriptStop.is_type(event.type):
                return seen


def test_a_second_transcription_is_refused_as_busy_while_the_model_is_held():
    stream = FakeStream(text="what time is it", partial="what time")
    engine = FakeEngine(stream=stream)
    wait = 0.15

    async def body(port):
        async with await connect(port) as held:
            await held.write_event(Transcribe().event())
            await held.write_event(AudioStart(**FMT).event())

            async with await connect(port) as second:
                await second.write_event(Transcribe().event())
                started = time.monotonic()
                event = await second.read_event()
                elapsed = time.monotonic() - started
                assert event is not None
                assert event.type == "error", event.type
                assert event.data["code"] == "busy"
                assert "busy" in event.data["text"]
                # Refused after the wait budget, not when the client gave up.
                assert elapsed >= wait * 0.5, elapsed
                assert elapsed < 2, elapsed
                # A refused stream stays readable for the events already sent.
                await second.write_event(AudioStop().event())

            # The session that holds the model is untouched by the refusal.
            await held.write_event(AudioChunk(**FMT, audio=PCM).event())
            await held.write_event(AudioStop().event())
            seen = {}
            while True:
                event = await held.read_event()
                assert event is not None, "holder lost its response"
                seen[event.type] = event
                if TranscriptStop.is_type(event.type):
                    break
            assert "error" not in seen
            assert seen["transcript-chunk"].data["text"] == "what time"
            assert seen["transcript"].data["text"] == "what time is it"

    run(engine, body, session_wait=wait, session_timeout=5)
    assert stream.audio == [[0.5, -0.5]]
    assert stream.rates == [16000]
    assert stream.closed is True


def test_concurrent_waiters_are_all_refused_and_the_model_stays_serialised():
    engine = FakeEngine(factory=FakeStream)
    wait = 0.15

    async def body(port):
        async with await connect(port) as held:
            await held.write_event(Transcribe().event())

            async def refused():
                async with await connect(port) as client:
                    await client.write_event(Transcribe().event())
                    event = await client.read_event()
                    assert event is not None and event.type == "error"
                    return event.data["code"], event.data["text"]

            # Three callers at once: the Dashboard mic plus traffic that
            # arrived while the voice loop was still endpointing.
            refusals = await asyncio.gather(refused(), refused(), refused())
            assert [code for code, _ in refusals] == ["busy"] * 3
            assert all("busy" in text for _, text in refusals)

            await held.write_event(AudioStop().event())
            while True:
                event = await held.read_event()
                assert event is not None and event.type != "error"
                if TranscriptStop.is_type(event.type):
                    break

        # A later transcription succeeds, so no waiter leaked the lock.
        seen = await full_session(port)
        assert seen["transcript"].data["text"] == "what time is it"

    run(engine, body, session_wait=wait, session_timeout=5)
    assert len(engine.streams) == 2  # the held session, then the later one


def test_an_idle_session_times_out_and_releases_the_model():
    engine = FakeEngine(factory=FakeStream)
    idle = 0.2

    async def body(port):
        async with await connect(port) as silent:
            await silent.write_event(Transcribe().event())
            await silent.write_event(AudioStart(**FMT).event())
            started = time.monotonic()
            event = await silent.read_event()
            elapsed = time.monotonic() - started
            assert event is not None, "an idle session must be told, not dropped"
            assert event.type == "error", event.type
            assert event.data["code"] == "idle-timeout"
            assert "timed out" in event.data["text"]
            assert elapsed >= idle * 0.5, elapsed
            assert elapsed < 2, elapsed
            # The service closes the connection rather than leaving it waiting.
            assert await silent.read_event() is None

        # The model is free again for the next turn.
        seen = await full_session(port)
        assert seen["transcript"].data["text"] == "what time is it"

    run(engine, body, session_wait=1, session_timeout=idle)
    assert engine.streams[0].closed is True


def test_audio_keeps_a_session_alive_past_the_idle_timeout():
    engine = FakeEngine(stream=FakeStream(text="what time is it", partial="what time"))
    idle = 0.3

    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Transcribe().event())
            await client.write_event(AudioStart(**FMT).event())
            # 0.8 s of streaming, well past the idle bound: a speaking turn is
            # not a stalled one.
            for _ in range(8):
                await client.write_event(AudioChunk(**FMT, audio=PCM).event())
                await asyncio.sleep(0.1)
            await client.write_event(AudioStop().event())
            seen = {}
            while True:
                event = await client.read_event()
                assert event is not None
                seen[event.type] = event
                if TranscriptStop.is_type(event.type):
                    break
            assert "error" not in seen
            assert seen["transcript"].data["text"] == "what time is it"

    run(engine, body, session_wait=1, session_timeout=idle)


def test_vocabulary_correction_preserves_raw_text_on_the_transcript():
    stream = FakeStream(text="play music on spot ify", partial="play music")
    engine = FakeEngine(stream=stream)

    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Transcribe().event())
            await client.write_event(AudioStart(**FMT).event())
            await client.write_event(AudioChunk(**FMT, audio=PCM).event())
            await client.write_event(AudioStop().event())
            seen = {}
            while True:
                event = await client.read_event()
                assert event is not None
                seen[event.type] = event
                if TranscriptStop.is_type(event.type):
                    break
            assert seen["transcript"].data["text"] == "play music on Spotify"
            assert seen["transcript"].data["raw_text"] == "play music on spot ify"

    run(engine, body, corrector=VocabularyCorrector.from_path())


def test_correction_off_leaves_the_model_text_alone():
    stream = FakeStream(text="play music on spot ify", partial="play music")

    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Transcribe().event())
            await client.write_event(AudioStart(**FMT).event())
            await client.write_event(AudioStop().event())
            event = await client.read_event()
            assert event is not None and Transcript.is_type(event.type)
            assert event.data["text"] == "play music on spot ify"
            assert "raw_text" not in event.data

    run(FakeEngine(stream=stream), body)


def test_a_connection_that_never_starts_a_session_is_reaped():
    idle = 0.2

    async def body(port):
        async with await connect(port) as client:
            await client.write_event(Describe().event())
            assert await client.read_event() is not None
            started = time.monotonic()
            assert await client.read_event() is None, "half-open connection kept open"
            assert time.monotonic() - started >= idle * 0.5

    run(FakeEngine(), body, session_timeout=idle)
