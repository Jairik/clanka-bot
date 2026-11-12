import asyncio
import sys
import threading
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import server  # noqa: E402
from wyoming.audio import AudioChunk, AudioStart, AudioStop  # noqa: E402
from wyoming.client import AsyncTcpClient  # noqa: E402
from wyoming.event import Event  # noqa: E402
from wyoming.info import Describe, Info  # noqa: E402
from wyoming.tts import (  # noqa: E402
    Synthesize,
    SynthesizeChunk,
    SynthesizeStart,
    SynthesizeStop,
    SynthesizeStopped,
    SynthesizeVoice,
)


class FakeEngine:
    """Stands in for Pocket TTS: yields canned PCM16 chunks."""

    language = "en"
    version = "test"
    revision = "test-revision-1"
    sample_rate = 24000
    voices = ["alba"]
    voice_dir = "/voices"

    def __init__(self, chunks=(b"\x01\x02",)):
        self.chunks = chunks
        self.texts = []
        self.voices_used = []
        self.rates = []
        self.validated = []
        self.warmed = []

    def state_for(self, voice):
        self.voices_used.append(voice)
        return voice

    def synthesize(self, state, text, stop=None, rate=1.0):
        self.texts.append(text)
        self.rates.append(rate)
        for chunk in self.chunks:
            if callable(chunk):
                yield from chunk()
            else:
                yield chunk

    def validate_voice(self, path):
        if not path.endswith(".safetensors"):
            raise ValueError("only .safetensors voice states are supported")
        if "broken" in path:
            raise RuntimeError("model could not load the state")
        self.validated.append(path)
        return self.revision

    def warm_voice(self, voice):
        self.warmed.append(voice)
        return voice


class GatedEngine(FakeEngine):
    """Yields the first chunk, then blocks until the test releases the gate."""

    def __init__(self, gate):
        super().__init__()
        self.gate = gate

    def synthesize(self, state, text, stop=None, rate=1.0):
        self.texts.append(text)
        self.rates.append(rate)
        yield b"\x01\x02"
        assert self.gate.wait(2.0), "client did not receive the first chunk in time"
        yield b"\x03\x04"


async def with_server(engine, body):
    tts_server = server.TtsServer(engine)
    listener = await asyncio.start_server(tts_server.handle_connection, "127.0.0.1", 0)
    port = listener.sockets[0].getsockname()[1]
    try:
        await body(port)
    finally:
        listener.close()
        await listener.wait_closed()


def run(engine, body):
    asyncio.run(with_server(engine, body))


def test_describe_advertises_a_warm_pocket_tts_program():
    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            await client.write_event(Describe().event())
            event = await client.read_event()
            assert event is not None
            assert Info.is_type(event.type)
            info = Info.from_event(event)
            assert len(info.tts) == 1
            program = info.tts[0]
            assert program.name == "pocket-tts"
            assert program.installed is True
            assert program.supports_synthesize_streaming is True
            assert [voice.name for voice in program.voices] == ["alba"]
            assert program.voices[0].languages == ["en"]
            assert program.attribution.name == "Kyutai"
            assert program.attribution.url.startswith("https://")

    run(FakeEngine(), body)


def test_synthesize_streams_audio_before_synthesis_finishes():
    gate = threading.Event()
    engine = GatedEngine(gate)

    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            await client.write_event(
                Synthesize(text="Hello there.", voice=SynthesizeVoice(name="alba")).event()
            )

            start = await client.read_event()
            assert start is not None and AudioStart.is_type(start.type)
            start_audio = AudioStart.from_event(start)
            assert (start_audio.rate, start_audio.width, start_audio.channels) == (24000, 2, 1)

            # This read only returns if the first chunk was forwarded immediately.
            first = await client.read_event()
            assert first is not None and AudioChunk.is_type(first.type)
            assert AudioChunk.from_event(first).audio == b"\x01\x02"

            gate.set()

            second = await client.read_event()
            assert second is not None and AudioChunk.is_type(second.type)
            assert AudioChunk.from_event(second).audio == b"\x03\x04"
            stop = await client.read_event()
            assert stop is not None and AudioStop.is_type(stop.type)

    run(engine, body)
    assert engine.texts == ["Hello there."]
    assert engine.voices_used == ["alba"]


def test_streaming_synthesize_flushes_sentences_and_finishes_with_stopped():
    engine = FakeEngine((b"\x09",))
    types = []

    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            await client.write_event(SynthesizeStart(voice=SynthesizeVoice(name="alba")).event())
            await client.write_event(SynthesizeChunk(text="One.").event())
            await client.write_event(SynthesizeChunk(text="Two").event())
            await client.write_event(SynthesizeStop().event())

            while True:
                event = await client.read_event()
                assert event is not None
                types.append(event.type)
                if SynthesizeStopped.is_type(event.type):
                    break

    run(engine, body)
    assert types == [
        "audio-start",
        "audio-chunk",
        "audio-stop",
        "audio-start",
        "audio-chunk",
        "audio-stop",
        "synthesize-stopped",
    ]
    assert engine.texts == ["One.", "Two"]


def test_engine_failure_becomes_an_error_event():
    class FailingEngine(FakeEngine):
        def synthesize(self, state, text, stop=None, rate=1.0):
            raise RuntimeError("model exploded")
            yield b""  # pragma: no cover - keeps this a generator

    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            await client.write_event(Synthesize(text="Hello.").event())
            while True:
                event = await client.read_event()
                assert event is not None
                if event.type == "error":
                    assert "model exploded" in event.data["text"]
                    return

    run(FailingEngine(), body)


def test_pcm_conversion_scales_floats_to_signed_16_bit():
    from pcm import to_pcm16

    assert to_pcm16([0.0, 0.5, -0.5]) == b"\x00\x00\xff\x3f\x01\xc0"
    assert to_pcm16([2.0, -2.0]) == b"\xff\x7f\x01\x80"
    assert to_pcm16(b"\x01\x02") == b"\x01\x02"


def test_pcm_conversion_accepts_numpy_arrays():
    import numpy as np

    from pcm import to_pcm16

    assert to_pcm16(np.array([0.0, 0.5], dtype="float32")) == b"\x00\x00\xff\x3f"


def test_voice_info_reports_the_revision_and_builtins():
    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            from wyoming.event import Event

            await client.write_event(Event(type="voice-info"))
            event = await client.read_event()
            assert event is not None
            assert event.type == "voice-info-result"
            assert event.data["revision"] == "test-revision-1"
            assert event.data["voices"] == ["alba"]
            assert event.data["dir"] == "/voices"

    run(FakeEngine(), body)


def test_voice_validate_reports_success_and_failure_without_dropping_the_connection():
    engine = FakeEngine()

    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            from wyoming.event import Event

            await client.write_event(
                Event(type="voice-validate", data={"path": "/voices/jarvis.safetensors"})
            )
            event = await client.read_event()
            assert event is not None
            assert event.type == "voice-validated"
            assert event.data["ok"] is True
            assert event.data["revision"] == "test-revision-1"

            await client.write_event(
                Event(type="voice-validate", data={"path": "/voices/broken.safetensors"})
            )
            event = await client.read_event()
            assert event is not None
            assert event.type == "voice-validated"
            assert event.data["ok"] is False
            assert "model could not load" in event.data["error"]

    run(engine, body)
    assert engine.validated == ["/voices/jarvis.safetensors"]


def test_voice_warm_loads_the_selected_state():
    engine = FakeEngine()

    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            from wyoming.event import Event

            await client.write_event(Event(type="voice-warm", data={"voice": "/voices/jarvis.safetensors"}))
            event = await client.read_event()
            assert event is not None
            assert event.type == "voice-warmed"
            assert event.data["ok"] is True

    run(engine, body)
    assert engine.warmed == ["/voices/jarvis.safetensors"]


def test_rate_is_passed_to_the_synthesizer():
    engine = FakeEngine()

    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            await client.write_event(
                Event(
                    type="synthesize",
                    data={"text": "Faster now.", "voice": {"name": "alba"}, "rate": 2.0},
                )
            )
            while True:
                event = await client.read_event()
                assert event is not None
                if AudioStop.is_type(event.type):
                    break

    run(engine, body)
    assert engine.rates == [2.0]


def test_invalid_rate_is_an_error_event():
    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            await client.write_event(
                Event(type="synthesize", data={"text": "Too fast.", "rate": 9.0})
            )
            event = await client.read_event()
            assert event is not None
            assert event.type == "error"
            assert "rate must be between" in event.data["text"]

    run(FakeEngine(), body)


def test_streaming_synthesis_keeps_its_start_rate():
    engine = FakeEngine((b"\x09",))

    async def body(port):
        async with AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2) as client:
            await client.write_event(
                Event(type="synthesize-start", data={"voice": {"name": "alba"}, "rate": 0.5})
            )
            await client.write_event(SynthesizeChunk(text="One.").event())
            await client.write_event(SynthesizeStop().event())
            while True:
                event = await client.read_event()
                assert event is not None
                if SynthesizeStopped.is_type(event.type):
                    break

    run(engine, body)
    assert engine.rates == [0.5]


def test_resampler_changes_length_and_spans_chunks():
    import numpy as np

    from engine import StreamingResampler

    tone = np.sin(np.linspace(0, 2 * np.pi, 1000)).astype("float32")
    faster = StreamingResampler(2.0)
    # Two chunks must join without a length discontinuity.
    out = np.concatenate([faster.process(tone[:400]), faster.process(tone[400:])])
    assert 480 <= len(out) <= 510

    slower = StreamingResampler(0.5)
    out = np.concatenate([slower.process(tone[:400]), slower.process(tone[400:])])
    assert 1980 <= len(out) <= 2010
    assert np.max(np.abs(out)) <= 1.0
