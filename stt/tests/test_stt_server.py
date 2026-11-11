import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import stt_server  # noqa: E402
from wyoming.asr import Transcribe, Transcript, TranscriptChunk, TranscriptStop  # noqa: E402
from wyoming.audio import AudioChunk, AudioStart, AudioStop  # noqa: E402
from wyoming.client import AsyncTcpClient  # noqa: E402
from wyoming.info import AsrProgram, Describe, Info  # noqa: E402


class FakeStream:
    """Stands in for a Moonshine stream: records audio, returns canned text."""

    def __init__(self, text="what time is it", partial="what time", fail=None):
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

    def __init__(self, model="small-streaming", stream=None, fail=None):
        self.model = model
        self.stream = stream or FakeStream()
        self.fail = fail
        self.requested = []

    def create_stream(self, model=None):
        if self.fail == "create":
            raise RuntimeError("model is not baked into the image")
        self.requested.append(model)
        return self.stream


async def with_server(engine, body):
    stt_server_instance = stt_server.SttServer(engine)
    listener = await asyncio.start_server(stt_server_instance.handle_connection, "127.0.0.1", 0)
    port = listener.sockets[0].getsockname()[1]
    try:
        await body(port)
    finally:
        listener.close()
        await listener.wait_closed()


def run(engine, body):
    asyncio.run(with_server(engine, body))


async def connect(port):
    return AsyncTcpClient("127.0.0.1", port, connect_timeout=2, read_timeout=2)


def test_describe_advertises_moonshine():
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
            assert program.name == "moonshine"
            assert program.installed is True
            assert program.supports_transcript_streaming is True
            assert [model.name for model in program.models] == ["small-streaming"]
            assert program.models[0].languages == ["en"]
            assert program.attribution.url.startswith("https://")

    run(FakeEngine(), body)


def test_transcribe_streams_partials_and_returns_the_final_text():
    stream = FakeStream(text="what time is it", partial="what time")
    engine = FakeEngine(stream=stream)

    async def body(port):
        async with await connect(port) as client:
            await client.write_event(
                Transcribe(name="tiny-streaming", language="en").event()
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

            assert engine.requested == ["tiny-streaming"]
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
            await client.write_event(Transcribe(name="medium-streaming").event())
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


def test_transcript_text_joins_completed_lines():
    from moonshine_engine import transcript_text

    class Line:
        def __init__(self, text):
            self.text = text

    class TranscriptObject:
        lines = [Line("what time"), Line("is it ")]

    assert transcript_text(TranscriptObject()) == "what time is it"
    assert transcript_text(None) == ""
